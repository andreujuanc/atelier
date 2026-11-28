// Package store persists atelier's tasks and sessions in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite" // also registers the "sqlite" driver
	sqlite3 "modernc.org/sqlite/lib"
)

var (
	// ErrNotFound is returned (possibly wrapped) when a lookup matches nothing.
	ErrNotFound = errors.New("not found")
	// ErrAmbiguous is returned (possibly wrapped) when a reference matches more than one row.
	ErrAmbiguous = errors.New("ambiguous reference")
)

// Store is a handle on the atelier database. It is safe for concurrent use.
type Store struct {
	db *sql.DB
	// wal is the path of the database's -wal file (next to the database
	// file a symlinked path resolves to), or "" for a read-only store (see
	// Close).
	wal string
}

// migrations are applied in order; the database's PRAGMA user_version
// records how many have been applied. Never edit or reorder an entry,
// only append.
var migrations = []string{
	// v1
	`CREATE TABLE tasks (
  id INTEGER PRIMARY KEY,
  slug TEXT NOT NULL UNIQUE,
  title TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('backlog','active','review','done','cancelled')),
  project TEXT NOT NULL DEFAULT '',
  branch TEXT NOT NULL,
  port_offset INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE task_repos (
  task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  mode TEXT NOT NULL CHECK (mode IN ('rw','ro')),
  base_branch TEXT NOT NULL DEFAULT '',
  base_commit TEXT NOT NULL DEFAULT '',
  position INTEGER NOT NULL,
  PRIMARY KEY (task_id, name)
);
CREATE TABLE sessions (
  id INTEGER PRIMARY KEY,
  task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  claude_session_id TEXT NOT NULL DEFAULT '',
  tmux_target TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL CHECK (state IN ('running','waiting','idle','ended','stopped')),
  created_at INTEGER NOT NULL,
  last_event_at INTEGER NOT NULL
);
CREATE INDEX sessions_task ON sessions(task_id);
CREATE INDEX sessions_claude ON sessions(claude_session_id);`,
	// v2: launch_seq is bumped whenever a window is (about to be) launched
	// for the session, so Recover can compare-and-set against the launch it
	// observed even if tmux reuses a window id after a server restart.
	`ALTER TABLE sessions ADD COLUMN launch_seq INTEGER NOT NULL DEFAULT 0;`,
	// v3: the Claude Code transcript of a session (from hook input) and
	// free-form notes on a task (atelier assistant).
	`ALTER TABLE sessions ADD COLUMN transcript_path TEXT NOT NULL DEFAULT '';
CREATE TABLE task_notes (
  id INTEGER PRIMARY KEY,
  task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  body TEXT NOT NULL,
  author TEXT NOT NULL CHECK (author IN ('user','assistant')),
  created_at INTEGER NOT NULL
);
CREATE INDEX task_notes_task ON task_notes(task_id);`,
	// v4: the task's base branch choice ('' = auto, ':default' = origin's
	// default branch, else a branch name), used when repos are added later.
	// Existing tasks keep their old behaviour (origin's default branch); new
	// tasks get their base from the app ('' = auto).
	`ALTER TABLE tasks ADD COLUMN base TEXT NOT NULL DEFAULT '';
UPDATE tasks SET base=':default';`,
	// v5: one free-form notes text per task. The dated task_notes rows are
	// folded into it, oldest first, one "- YYYY-MM-DD [author] body" line
	// each (local date; newlines inside a body are kept). task_notes is
	// kept but no longer used. The UPDATE only fills empty notes, so
	// running it again changes nothing.
	`ALTER TABLE tasks ADD COLUMN notes TEXT NOT NULL DEFAULT '';
UPDATE tasks SET notes = (
  SELECT group_concat('- ' || strftime('%Y-%m-%d', n.created_at / 1000, 'unixepoch', 'localtime') ||
    ' [' || n.author || '] ' || n.body, char(10) ORDER BY n.created_at, n.id)
  FROM task_notes n WHERE n.task_id = tasks.id)
WHERE notes = '' AND EXISTS (SELECT 1 FROM task_notes n WHERE n.task_id = tasks.id);`,
	// v6: the background work (subagents, background shells, monitors, ...) a
	// session's Claude still had in flight when it last went idle, one line
	// each, as its Stop hook reported it ('': none, or not reported).
	`ALTER TABLE sessions ADD COLUMN background TEXT NOT NULL DEFAULT '';`,
	// v7: who first wrote each notes line (see Store.NotesBlame): the
	// sha256 of the line (lineHashV1), which copy of it, author and
	// time. The lines of the notes from before are recorded as unknown's
	// (migrationSteps).
	`CREATE TABLE notes_line_authors (
  task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  line_hash BLOB NOT NULL,
  copy INTEGER NOT NULL,
  author TEXT NOT NULL,
  at INTEGER NOT NULL,
  PRIMARY KEY (task_id, line_hash, copy)
);`,
	// v8: every transcript file a session has had (transcript_path keeps
	// only the latest: /clear, resume and compaction can start a new one),
	// at most maxSessionTranscripts per session. The triggers record each
	// path transcript_path is set to; AddSessionTranscript the others a
	// hook reports. The current paths are recorded now.
	`CREATE TABLE session_transcripts (
  session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  path TEXT NOT NULL,
  first_seen INTEGER NOT NULL,
  PRIMARY KEY (session_id, path)
);
CREATE TRIGGER session_transcripts_insert AFTER INSERT ON sessions WHEN NEW.transcript_path != ''
BEGIN
  INSERT OR IGNORE INTO session_transcripts (session_id, path, first_seen) VALUES (NEW.id, NEW.transcript_path, NEW.last_event_at);
END;
CREATE TRIGGER session_transcripts_update AFTER UPDATE OF transcript_path ON sessions WHEN NEW.transcript_path != ''
BEGIN
  INSERT OR IGNORE INTO session_transcripts (session_id, path, first_seen)
    SELECT NEW.id, NEW.transcript_path, NEW.last_event_at
    WHERE (SELECT count(*) FROM session_transcripts WHERE session_id = NEW.id) < 100;
END;
INSERT INTO session_transcripts (session_id, path, first_seen)
  SELECT id, transcript_path, last_event_at FROM sessions WHERE transcript_path != '';`,
	// v9: the last pre-clean of each task (atl learn): the extract's
	// input_hash, the key of what produced it (learn.CacheKey: input,
	// prompt version, model, effort), the occurrences kept (JSON), and
	// its cost. Only a call that succeeded is stored.
	`CREATE TABLE learn_digests (
  task_id INTEGER PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
  input_hash TEXT NOT NULL,
  cache_key TEXT NOT NULL,
  occurrences_json TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  cost_usd REAL NOT NULL DEFAULT 0
);`,
	// v10: learnings (atl learn's Analyse groups occurrences into them;
	// atl learnings reviews them) and their occurrences, each by its
	// stable key (learn.OccurrenceKey): a key that is here is not
	// analysed again. item_id is the item it quotes (learn.ItemID).
	// session is atelier's session id, 0 for a notes line; no foreign
	// key, so the evidence outlives a session row. at, decided_at: unix
	// ms (decided_at 0: not decided). learning_skips: the keys Analyse
	// left unassigned (tries: in how many runs); from 2 tries not sent
	// again while analyse_version is the current learn.AnalyseVersion.
	`CREATE TABLE learnings (
  id INTEGER PRIMARY KEY,
  text TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('candidate','proposed','accepted','rejected')),
  edited INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  decided_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE learning_occurrences (
  key TEXT PRIMARY KEY,
  learning_id INTEGER NOT NULL REFERENCES learnings(id) ON DELETE CASCADE,
  task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  item_id TEXT NOT NULL DEFAULT '',
  session INTEGER NOT NULL DEFAULT 0,
  at INTEGER NOT NULL,
  strength TEXT NOT NULL CHECK (strength IN ('strong','normal','mild')),
  polarity TEXT NOT NULL CHECK (polarity IN ('reinforce','contradict')),
  quote TEXT NOT NULL,
  text TEXT NOT NULL
);
CREATE INDEX learning_occurrences_learning ON learning_occurrences(learning_id);
CREATE INDEX learning_occurrences_task ON learning_occurrences(task_id);
CREATE TABLE learning_skips (
  key TEXT PRIMARY KEY,
  task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  analyse_version TEXT NOT NULL,
  tries INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX learning_skips_task ON learning_skips(task_id);`,
	// v11: the injection snapshot (one row): the "User preferences"
	// section of the start context, rendered from the accepted learnings
	// by every change of them (Store.Injection), so the hook only reads
	// it. No row: nothing to inject.
	`CREATE TABLE learn_injection (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  text TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);`,
}

// maxSessionTranscripts caps the session_transcripts rows of one session
// (the v8 trigger has it as a literal).
const maxSessionTranscripts = 100

// migrationSteps holds, by index in migrations, Go code to run right after
// that migration's SQL, in the same transaction: what SQL cannot do.
// Never edit an entry, only add.
var migrationSteps = map[int]func(context.Context, *sql.Tx) error{
	6: backfillNoteLinesV1, // v7
}

// DefaultBusyTimeout is how long Open waits on a locked database.
const DefaultBusyTimeout = 3 * time.Second

// Open opens (creating if needed) the database at path, creating its parent
// directory, and applies pending migrations. It is
// OpenWithTimeout(path, DefaultBusyTimeout).
func Open(path string) (*Store, error) { return OpenWithTimeout(path, DefaultBusyTimeout) }

// OpenWithTimeout is Open with an explicit busy timeout: how long a
// statement waits for a lock held by another connection before failing
// with SQLITE_BUSY.
//
// Pragmas are passed as DSN "_pragma" parameters so the driver applies them
// to every pooled connection, except journal_mode, which is persistent and
// set once (see setWAL). Transactions use BEGIN IMMEDIATE ("_txlock")
// so concurrent writers wait on busy_timeout instead of failing with
// SQLITE_BUSY on a read→write lock upgrade.
//
// Its connections do not checkpoint the WAL when they close; Close
// checkpoints it once it has grown past walTruncateSize instead (see
// noCheckpointOnClose).
func OpenWithTimeout(path string, busy time.Duration) (*Store, error) {
	if busy < 0 {
		busy = 0
	}
	if path == "" {
		return nil, errors.New("store: empty database path")
	}
	if strings.ContainsRune(path, '?') {
		return nil, fmt.Errorf("store: database path %q must not contain '?'", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("store: create db dir: %w", err)
	}
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busy.Milliseconds()))
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	db := sql.OpenDB(noCkptConnector{dsn: path + "?" + q.Encode(), drv: &sqlite.Driver{}})
	if err := setWAL(context.Background(), db, busy); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: open %s: set WAL mode: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate %s: %w", path, err)
	}
	// The database file as SQLite resolved it (following symlinks), next to
	// which it keeps the WAL.
	var file string
	if err := db.QueryRowContext(context.Background(), "SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&file); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	s.wal = file + "-wal"
	return s, nil
}

// setWAL puts the database in WAL mode, which is recorded in the file, so
// once per Open is enough. On a database not yet in WAL mode (a fresh one)
// the switch needs an exclusive lock, and while another connection opens
// it too SQLite fails it with SQLITE_BUSY at once instead of waiting on
// busy_timeout; setWAL retries it until busy has passed.
func setWAL(ctx context.Context, db *sql.DB, busy time.Duration) error {
	deadline := time.Now().Add(busy)
	for {
		_, err := db.ExecContext(ctx, "PRAGMA journal_mode = WAL")
		var se *sqlite.Error
		if err == nil || !errors.As(err, &se) || se.Code()&0xff != sqlite3.SQLITE_BUSY || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// ErrSchemaVersion is returned (wrapped) by OpenReadOnly when the
// database's user_version is not exactly the schema version of this binary
// (older, newer, or 0 for an empty or schema-less file).
var ErrSchemaVersion = errors.New("database schema version mismatch")

// OpenReadOnly opens the existing database at path for reading only, for
// callers that must never change it (`atl statusline`). Unlike Open it
// never creates the file or its directory, never migrates and never sets
// the journal mode: the connection is opened with SQLite's mode=ro and
// PRAGMA query_only=ON, and waits at most busy on a lock. A database whose
// user_version is not exactly len(migrations) is closed and reported as
// ErrSchemaVersion.
//
// The database file itself is never written. A read-only connection to a
// WAL database does create empty -wal and -shm files next to it when they
// are missing (SQLite needs the shared-memory index to read a WAL
// database), and cannot remove them on close. That is unavoidable without
// immutable=1 (unsafe while hooks write) or readonly_shm=1 (fails when no
// -shm exists), and harmless: read-write connections leave them in place
// too (see noCheckpointOnClose).
func OpenReadOnly(path string, busy time.Duration) (*Store, error) {
	if busy < 0 {
		busy = 0
	}
	if path == "" {
		return nil, errors.New("store: empty database path")
	}
	if strings.ContainsRune(path, '?') {
		return nil, fmt.Errorf("store: database path %q must not contain '?'", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	// A "file:" URI so SQLite honours mode=ro; the driver passes the whole
	// URI to sqlite3_open_v2 (SQLite ignores the driver's _pragma keys) and
	// applies the _pragma values on every pooled connection.
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busy.Milliseconds()))
	q.Add("_pragma", "query_only(ON)")
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String() + "?" + q.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	var v int
	if err := db.QueryRowContext(context.Background(), "PRAGMA user_version").Scan(&v); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: open %s read-only: %w", path, err)
	}
	if v != len(migrations) {
		db.Close()
		return nil, fmt.Errorf("store: %s: %w: %d, want %d", path, ErrSchemaVersion, v, len(migrations))
	}
	return &Store{db: db}, nil
}

// walTruncateSize is the -wal file size past which a read-write Store's
// Close checkpoints the WAL and truncates it.
const walTruncateSize = 1 << 20

// walCheckpointBusy bounds how long that checkpoint waits on another
// connection's lock or read transaction, well within a hook's latency
// budget.
const walCheckpointBusy = 20 * time.Millisecond

// Close closes the database. A read-write store first checkpoints the WAL
// into the database file and truncates it, if it has grown past
// walTruncateSize: its connections do not checkpoint on close (see
// noCheckpointOnClose), and when no other connection has the database
// open the next one to open it recovers the WAL by reading all of it and
// forgets what was already checkpointed, so SQLite's automatic checkpoint
// could never restart it and it would grow without bound. The checkpoint
// waits at most walCheckpointBusy for other connections; one that cannot
// finish (busy) is left to a later Close.
func (s *Store) Close() error {
	if s.wal != "" {
		s.truncateWAL(context.Background())
	}
	return s.db.Close()
}

// truncateWAL checkpoints the WAL and truncates it if the -wal file is
// larger than walTruncateSize, waiting at most walCheckpointBusy on locks.
// It is best effort: on any failure the WAL is left as it is.
func (s *Store) truncateWAL(ctx context.Context) {
	if fi, err := os.Stat(s.wal); err != nil || fi.Size() <= walTruncateSize {
		return
	}
	c, err := s.db.Conn(ctx)
	if err != nil {
		return
	}
	defer c.Close()
	// busy_timeout is per connection, and Close closes this one right after.
	if _, err := c.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout = %d", walCheckpointBusy.Milliseconds())); err != nil {
		return
	}
	_, _ = c.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
}

// migrate applies pending migrations. The common case (schema current) is
// a plain read of user_version with no write transaction, so concurrent
// opens (one per hook invocation) never contend for the write lock; only a
// needed migration takes BEGIN IMMEDIATE and re-checks the version inside
// it, since another process may have migrated in the meantime.
func (s *Store) migrate(ctx context.Context) error {
	v, err := userVersion(ctx, s.db)
	if err != nil || v == len(migrations) {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil) // BEGIN IMMEDIATE (_txlock)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if v, err = userVersion(ctx, tx); err != nil || v == len(migrations) {
		return err
	}
	for i := v; i < len(migrations); i++ {
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if step := migrationSteps[i]; step != nil {
			if err := step(ctx, tx); err != nil {
				return fmt.Errorf("migration %d: %w", i+1, err)
			}
		}
	}
	// PRAGMA does not accept bound parameters; the value is an int we control.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", len(migrations))); err != nil {
		return err
	}
	return tx.Commit()
}

// userVersion reads PRAGMA user_version, failing if the schema is newer
// than this binary supports.
func userVersion(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (int, error) {
	var v int
	if err := q.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, err
	}
	if v > len(migrations) {
		return 0, fmt.Errorf("database schema version %d is newer than this binary supports (%d)", v, len(migrations))
	}
	return v, nil
}

// now returns the current time truncated to millisecond precision in UTC,
// i.e. exactly what survives a round trip through the database.
func now() time.Time { return msTime(time.Now().UnixMilli()) }

func toMS(t time.Time) int64 { return t.UnixMilli() }

func msTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// normTime returns t normalised to ms/UTC, or now if t is zero.
func normTime(t time.Time) time.Time {
	if t.IsZero() {
		return now()
	}
	return msTime(t.UnixMilli())
}

type scanner interface {
	Scan(dest ...any) error
}

// placeholders returns "?,?,...,?" with n entries.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// execOne runs a statement on the row what id (an UPDATE or DELETE by id),
// failing with ErrNotFound when it matches no row.
func (s *Store) execOne(ctx context.Context, what string, id int64, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%s %d: %w", what, id, ErrNotFound)
	}
	return nil
}
