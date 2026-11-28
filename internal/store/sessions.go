package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
)

const sessionCols = `id, task_id, claude_session_id, tmux_target, name, state, created_at, last_event_at, transcript_path, background, launch_seq`

func scanSession(sc scanner) (*model.Session, error) {
	var ss model.Session
	var state string
	var created, last int64
	if err := sc.Scan(&ss.ID, &ss.TaskID, &ss.ClaudeSessionID, &ss.TmuxTarget, &ss.Name,
		&state, &created, &last, &ss.TranscriptPath, &ss.Background, &ss.LaunchSeq); err != nil {
		return nil, err
	}
	ss.State = model.SessionState(state)
	ss.CreatedAt = msTime(created)
	ss.LastEventAt = msTime(last)
	return &ss, nil
}

func (s *Store) querySessions(ctx context.Context, query string, args ...any) ([]model.Session, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Session
	for rows.Next() {
		ss, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ss)
	}
	return out, rows.Err()
}

func (s *Store) getSession(ctx context.Context, desc, where string, args ...any) (*model.Session, error) {
	ss, err := scanSession(s.db.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("session %s: %w", desc, ErrNotFound)
	}
	return ss, err
}

// CreateSession inserts sess, setting sess.ID and filling zero
// CreatedAt/LastEventAt with now.
func (s *Store) CreateSession(ctx context.Context, sess *model.Session) error {
	return insertSession(ctx, s.db, sess)
}

// CreateNamedSession is CreateSession for a session atl is about to launch
// a window for: in the same transaction the row is named name(id) (and
// sess.Name set), so that it is known as atl's launch from the start (see
// ListUnrecordedLaunches).
func (s *Store) CreateNamedSession(ctx context.Context, sess *model.Session, name func(id int64) string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cp := *sess
	if err := insertSession(ctx, tx, &cp); err != nil {
		return err
	}
	cp.Name = name(cp.ID)
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET name = ? WHERE id = ?`, cp.Name, cp.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	*sess = cp
	return nil
}

func insertSession(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, sess *model.Session) error {
	if !sess.State.Valid() {
		return fmt.Errorf("store: invalid session state %q", sess.State)
	}
	created, last := normTime(sess.CreatedAt), normTime(sess.LastEventAt)
	res, err := db.ExecContext(ctx, `INSERT INTO sessions
		(task_id, claude_session_id, tmux_target, name, state, created_at, last_event_at, transcript_path)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.TaskID, sess.ClaudeSessionID, sess.TmuxTarget, sess.Name, string(sess.State),
		toMS(created), toMS(last), sess.TranscriptPath)
	if err != nil {
		return fmt.Errorf("store: insert session for task %d: %w", sess.TaskID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	sess.ID, sess.CreatedAt, sess.LastEventAt = id, created, last
	return nil
}

// GetSession returns the session with the given id.
func (s *Store) GetSession(ctx context.Context, id int64) (*model.Session, error) {
	return s.getSession(ctx, fmt.Sprint(id), "id = ?", id)
}

// GetSessionByClaudeID returns the most recent (highest id) session with the
// given Claude session id. An empty cid never matches.
func (s *Store) GetSessionByClaudeID(ctx context.Context, cid string) (*model.Session, error) {
	if cid == "" {
		return nil, fmt.Errorf("session with empty claude id: %w", ErrNotFound)
	}
	return s.getSession(ctx, fmt.Sprintf("claude id %q", cid),
		"claude_session_id = ? ORDER BY id DESC LIMIT 1", cid)
}

// GetSessionByClaudeIDInTask is GetSessionByClaudeID among task taskID's
// sessions only: rows of other tasks never match.
func (s *Store) GetSessionByClaudeIDInTask(ctx context.Context, cid string, taskID int64) (*model.Session, error) {
	if cid == "" {
		return nil, fmt.Errorf("session with empty claude id: %w", ErrNotFound)
	}
	return s.getSession(ctx, fmt.Sprintf("claude id %q in task %d", cid, taskID),
		"claude_session_id = ? AND task_id = ? ORDER BY id DESC LIMIT 1", cid, taskID)
}

// ClaudeIDInOtherTask reports whether a session of a task other than taskID
// has Claude session id cid ("" never matches).
func (s *Store) ClaudeIDInOtherTask(ctx context.Context, cid string, taskID int64) (bool, error) {
	if cid == "" {
		return false, nil
	}
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE claude_session_id = ? AND task_id != ? LIMIT 1`,
		cid, taskID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ListSessions returns a task's sessions ordered by created_at asc.
func (s *Store) ListSessions(ctx context.Context, taskID int64) ([]model.Session, error) {
	return s.querySessions(ctx, `SELECT `+sessionCols+` FROM sessions WHERE task_id = ?
		ORDER BY created_at, id`, taskID)
}

// ListSessionsByTask returns the sessions of the tasks taskIDs by task id,
// each task's ordered as ListSessions orders them: one query for what would
// be one ListSessions per task.
func (s *Store) ListSessionsByTask(ctx context.Context, taskIDs []int64) (map[int64][]model.Session, error) {
	ids, err := json.Marshal(taskIDs)
	if err != nil {
		return nil, err
	}
	ss, err := s.querySessions(ctx, `SELECT `+sessionCols+` FROM sessions
		WHERE task_id IN (SELECT value FROM json_each(?)) ORDER BY created_at, id`, string(ids))
	if err != nil {
		return nil, err
	}
	out := map[int64][]model.Session{}
	for _, sess := range ss {
		out[sess.TaskID] = append(out[sess.TaskID], sess)
	}
	return out, nil
}

// ListSessionsByState returns sessions in any of the given states (all
// sessions if none given), ordered by created_at asc.
func (s *Store) ListSessionsByState(ctx context.Context, states ...model.SessionState) ([]model.Session, error) {
	where := "1"
	var args []any
	if len(states) > 0 {
		for _, st := range states {
			if !st.Valid() {
				return nil, fmt.Errorf("store: invalid session state %q", st)
			}
			args = append(args, string(st))
		}
		where = "state IN (" + placeholders(len(states)) + ")"
	}
	return s.querySessions(ctx, `SELECT `+sessionCols+` FROM sessions WHERE `+where+
		` ORDER BY created_at, id`, args...)
}

// UpdateSession writes sess's mutable fields (claude_session_id, tmux_target,
// name, state, last_event_at). A zero LastEventAt is replaced with now.
func (s *Store) UpdateSession(ctx context.Context, sess *model.Session) error {
	if !sess.State.Valid() {
		return fmt.Errorf("store: invalid session state %q", sess.State)
	}
	last := normTime(sess.LastEventAt)
	if err := s.execOne(ctx, "session", sess.ID, `UPDATE sessions SET claude_session_id = ?, tmux_target = ?,
		name = ?, state = ?, last_event_at = ?, background = '' WHERE id = ?`,
		sess.ClaudeSessionID, sess.TmuxTarget, sess.Name, string(sess.State), toMS(last), sess.ID); err != nil {
		return err
	}
	sess.LastEventAt = last
	return nil
}

// Every state change clears a session's background (see
// model.Session.Background), except SetIdle, which sets it.

// SetSessionState sets a session's state and last_event_at (now if at is zero).
func (s *Store) SetSessionState(ctx context.Context, id int64, st model.SessionState, at time.Time) error {
	if !st.Valid() {
		return fmt.Errorf("store: invalid session state %q", st)
	}
	return s.execOne(ctx, "session", id, `UPDATE sessions SET state = ?, last_event_at = ?, background = '' WHERE id = ?`,
		string(st), toMS(normTime(at)), id)
}

// SetIdle sets a session idle (last_event_at = at, now if zero) with the
// background work its Claude still has in flight, one per line ("": none).
func (s *Store) SetIdle(ctx context.Context, id int64, background string, at time.Time) error {
	return s.execOne(ctx, "session", id, `UPDATE sessions SET state = 'idle', last_event_at = ?, background = ? WHERE id = ?`,
		toMS(normTime(at)), background, id)
}

// Launch identifies one launch of a live session's window, as observed by
// ListLaunchedLive: the recorded tmux target plus the session's launch
// sequence number (bumped by ClaimStopped and SetSessionLaunch). Window ids
// are reused after a tmux server restart, so the target alone does not
// identify a launch.
type Launch struct {
	ID     int64
	Target string
	Seq    int64
}

// ListLaunchedLive returns every live session (running, waiting, idle) that
// has a recorded tmux target, with the launch it was observed at.
func (s *Store) ListLaunchedLive(ctx context.Context) ([]Launch, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, tmux_target, launch_seq FROM sessions
		WHERE state IN ('running', 'waiting', 'idle') AND tmux_target != '' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Launch
	for rows.Next() {
		var l Launch
		if err := rows.Scan(&l.ID, &l.Target, &l.Seq); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// MarkStoppedIfLaunch sets the session to stopped (last_event_at = at, now if
// zero) only if it is still live and still at the launch l (same non-empty
// tmux target and launch sequence number). A session relaunched since l was
// observed — even into a window that reuses the old window id — or ended
// meanwhile is left alone. It reports whether the row changed.
func (s *Store) MarkStoppedIfLaunch(ctx context.Context, l Launch, at time.Time) (bool, error) {
	return s.changed(ctx, `UPDATE sessions SET state = 'stopped', last_event_at = ?, background = ''
		WHERE id = ? AND state IN ('running', 'waiting', 'idle') AND tmux_target != ''
		AND tmux_target = ? AND launch_seq = ?`, toMS(normTime(at)), l.ID, l.Target, l.Seq)
}

// Unrecorded is a live session whose window atl launched, or was about to,
// without recording it (see ListUnrecordedLaunches).
type Unrecorded struct {
	ID, TaskID int64
	Slug       string // its task's
	Name       string // its window name ("" before a resume of a row the hooks made)
	Seq        int64  // its launch sequence number
}

// ListUnrecordedLaunches returns the live sessions (running, waiting, idle)
// that atl made or claimed for a launch (they have a window name, or a
// launch sequence number past 0) and whose window it has not recorded (an
// empty tmux target): a launch under way, or one whose atl died before it
// recorded the window. A row the hooks made for a Claude atl did not launch
// has neither.
func (s *Store) ListUnrecordedLaunches(ctx context.Context) ([]Unrecorded, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.id, s.task_id, t.slug, s.name, s.launch_seq
		FROM sessions s JOIN tasks t ON t.id = s.task_id
		WHERE s.state IN ('running', 'waiting', 'idle') AND s.tmux_target = ''
		AND (s.name != '' OR s.launch_seq > 0) ORDER BY s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Unrecorded
	for rows.Next() {
		var u Unrecorded
		if err := rows.Scan(&u.ID, &u.TaskID, &u.Slug, &u.Name, &u.Seq); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// MarkStoppedIfUnrecorded sets the session to stopped (last_event_at = at,
// now if zero) only if it is still live, still has no tmux target and is
// still at launch sequence u.Seq: a launch recorded since u was observed is
// left alone. It reports whether the row changed.
func (s *Store) MarkStoppedIfUnrecorded(ctx context.Context, u Unrecorded, at time.Time) (bool, error) {
	return s.changed(ctx, `UPDATE sessions SET state = 'stopped', last_event_at = ?, background = ''
		WHERE id = ? AND state IN ('running', 'waiting', 'idle') AND tmux_target = '' AND launch_seq = ?`,
		toMS(normTime(at)), u.ID, u.Seq)
}

// ClaimStopped atomically moves a stopped session to idle (last_event_at =
// at, now if zero), clears its tmux_target and bumps its launch sequence,
// reporting whether this caller won the claim. Only the winner may launch a
// window for the session. The target is cleared because the old window is
// gone and a live row with an empty target is one Recover leaves alone while
// its window is launched; the launcher records the new window with
// SetSessionLaunch. It is ClaimFrom(ctx, id, StateStopped, at).
func (s *Store) ClaimStopped(ctx context.Context, id int64, at time.Time) (bool, error) {
	return s.ClaimFrom(ctx, id, model.StateStopped, at)
}

// ClaimFrom is the resume claim for a session the caller saw in state from,
// which must be stopped or ended (a past session): like ClaimStopped, it
// atomically moves the session to idle, clears its tmux_target and bumps its
// launch sequence, but only if the session is still in state from (the
// caller checks it has a Claude session id; hooks never clear one). It reports whether this caller won; only the
// winner may launch `claude --resume` for it.
func (s *Store) ClaimFrom(ctx context.Context, id int64, from model.SessionState, at time.Time) (bool, error) {
	if from != model.StateStopped && from != model.StateEnded {
		return false, fmt.Errorf("store: cannot claim a session for resume from state %q", from)
	}
	return s.changed(ctx, `UPDATE sessions SET state = 'idle', tmux_target = '', launch_seq = launch_seq + 1,
		last_event_at = ?, background = '' WHERE id = ? AND state = ?`, toMS(normTime(at)), id, string(from))
}

// SetSessionLaunch records the tmux window and name of a launched session and
// bumps its launch sequence, leaving every other column (state,
// claude_session_id, ...) untouched.
func (s *Store) SetSessionLaunch(ctx context.Context, id int64, tmuxTarget, name string) error {
	return s.execOne(ctx, "session", id, `UPDATE sessions SET tmux_target = ?, name = ?,
		launch_seq = launch_seq + 1 WHERE id = ?`, tmuxTarget, name, id)
}

// AdoptLaunch records, for a session the caller has just claimed for a
// resume (ClaimFrom), the tmux window and name of the Claude still running
// from its last launch, which it adopts rather than launching one: the
// claim's bump of the launch sequence is undone, back to that launch's
// record, so that Claude's $ATL_LAUNCH stays current (see EndLive).
func (s *Store) AdoptLaunch(ctx context.Context, id int64, tmuxTarget, name string) error {
	return s.execOne(ctx, "session", id, `UPDATE sessions SET tmux_target = ?, name = ?,
		launch_seq = launch_seq - 1 WHERE id = ?`, tmuxTarget, name, id)
}

// changed runs a conditional UPDATE and reports whether it changed a row.
func (s *Store) changed(ctx context.Context, query string, args ...any) (bool, error) {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SetClaudeSession records the Claude session id reported by a SessionStart
// hook and sets state to idle and last_event_at (now if at is zero), leaving
// every other column untouched. It works from any state (resume of a stopped
// or ended session).
func (s *Store) SetClaudeSession(ctx context.Context, id int64, claudeSessionID string, at time.Time) error {
	return s.execOne(ctx, "session", id, `UPDATE sessions SET claude_session_id = ?, state = 'idle',
		last_event_at = ?, background = '' WHERE id = ?`, claudeSessionID, toMS(normTime(at)), id)
}

// SetClaudeSessionID records a new Claude session id (e.g. after
// auto-compaction) and sets last_event_at (now if at is zero), leaving state
// and every other column untouched.
func (s *Store) SetClaudeSessionID(ctx context.Context, id int64, claudeSessionID string, at time.Time) error {
	return s.execOne(ctx, "session", id, `UPDATE sessions SET claude_session_id = ?, last_event_at = ? WHERE id = ?`,
		claudeSessionID, toMS(normTime(at)), id)
}

// EndLive sets a live session (running, waiting, idle) ended (last_event_at
// = at, now if zero), reporting whether it did. A session atl itself
// stopped or ended is left as it is: a SessionEnd from its Claude being
// killed then must not make it unresumable. launch, if not negative, is
// the LaunchSeq the ending Claude was started at: a session launched again
// since (claimed for a resume, which bumps it past launch+1) is left as it
// is too, as that is its old Claude ending late.
func (s *Store) EndLive(ctx context.Context, id, launch int64, at time.Time) (bool, error) {
	return s.changed(ctx, `UPDATE sessions SET state = 'ended', last_event_at = ?, background = ''
		WHERE id = ? AND state IN ('running', 'waiting', 'idle') AND (? < 0 OR launch_seq <= ? + 1)`,
		toMS(normTime(at)), id, launch, launch)
}

// SetStateIf moves a session from state from to state to (last_event_at =
// at, now if zero) only if it is currently in from, reporting whether the
// row changed. An unknown id is not an error.
func (s *Store) SetStateIf(ctx context.Context, id int64, from, to model.SessionState, at time.Time) (bool, error) {
	if !from.Valid() || !to.Valid() {
		return false, fmt.Errorf("store: invalid session state transition %q -> %q", from, to)
	}
	return s.changed(ctx, `UPDATE sessions SET state = ?, last_event_at = ?, background = '' WHERE id = ? AND state = ?`,
		string(to), toMS(normTime(at)), id, string(from))
}

// TouchSession sets a session's last_event_at (now if at is zero) only.
func (s *Store) TouchSession(ctx context.Context, id int64, at time.Time) error {
	return s.execOne(ctx, "session", id, `UPDATE sessions SET last_event_at = ? WHERE id = ?`,
		toMS(normTime(at)), id)
}

// SetTranscriptPath records the Claude Code transcript file of a session
// (reported by every SessionStart hook), leaving every other column
// untouched.
func (s *Store) SetTranscriptPath(ctx context.Context, id int64, path string) error {
	return s.execOne(ctx, "session", id, `UPDATE sessions SET transcript_path = ? WHERE id = ?`, path, id)
}

// SetTranscriptPathIfEmpty records a session's transcript file only if none
// is recorded yet (hooks other than SessionStart fill a gap but never
// overwrite). An unknown id is not an error.
func (s *Store) SetTranscriptPathIfEmpty(ctx context.Context, id int64, path string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET transcript_path = ?
		WHERE id = ? AND transcript_path = ''`, path, id)
	return err
}

// AddSessionTranscript records path as one of session id's transcript files
// (session_transcripts) without changing transcript_path: a path a hook
// reports that is not the latest. Nothing past maxSessionTranscripts, and
// an unknown id is not an error. Its first_seen is the row's last_event_at,
// as the v8 triggers'.
func (s *Store) AddSessionTranscript(ctx context.Context, id int64, path string) error {
	if path == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO session_transcripts (session_id, path, first_seen)
		SELECT id, ?, last_event_at FROM sessions WHERE id = ?
		AND (SELECT count(*) FROM session_transcripts WHERE session_id = ?) < ?`,
		path, id, id, maxSessionTranscripts)
	return err
}

// ListSessionTranscripts returns, by session id, every transcript file the
// sessions sessionIDs have had, oldest first (transcript_path among them).
func (s *Store) ListSessionTranscripts(ctx context.Context, sessionIDs []int64) (map[int64][]string, error) {
	ids, err := json.Marshal(sessionIDs)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, path FROM session_transcripts
		WHERE session_id IN (SELECT value FROM json_each(?)) ORDER BY first_seen, path`, string(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			return nil, err
		}
		out[id] = append(out[id], path)
	}
	return out, rows.Err()
}
