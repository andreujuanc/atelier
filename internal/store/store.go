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

	_ "modernc.org/sqlite" // registers the "sqlite" driver
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
}

// Open opens (creating if needed) the database at path, creating its parent
// directory, and applies pending migrations.
//
// Pragmas are passed as DSN "_pragma" parameters so the driver applies them
// to every pooled connection. Transactions use BEGIN IMMEDIATE ("_txlock")
// so concurrent writers wait on busy_timeout instead of failing with
// SQLITE_BUSY on a read→write lock upgrade.
func Open(path string) (*Store, error) {
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
	q.Add("_pragma", "busy_timeout(3000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate %s: %w", path, err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var v int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d)", v, len(migrations))
	}
	if v == len(migrations) {
		return nil
	}
	for i := v; i < len(migrations); i++ {
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	// PRAGMA does not accept bound parameters; the value is an int we control.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", len(migrations))); err != nil {
		return err
	}
	return tx.Commit()
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

func checkAffected(res sql.Result, what string, id int64) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%s %d: %w", what, id, ErrNotFound)
	}
	return nil
}
