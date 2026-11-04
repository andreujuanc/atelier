package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
)

const sessionCols = `id, task_id, claude_session_id, tmux_target, name, state, created_at, last_event_at`

func scanSession(sc scanner) (*model.Session, error) {
	var ss model.Session
	var state string
	var created, last int64
	if err := sc.Scan(&ss.ID, &ss.TaskID, &ss.ClaudeSessionID, &ss.TmuxTarget, &ss.Name,
		&state, &created, &last); err != nil {
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
	if !sess.State.Valid() {
		return fmt.Errorf("store: invalid session state %q", sess.State)
	}
	created, last := normTime(sess.CreatedAt), normTime(sess.LastEventAt)
	res, err := s.db.ExecContext(ctx, `INSERT INTO sessions
		(task_id, claude_session_id, tmux_target, name, state, created_at, last_event_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sess.TaskID, sess.ClaudeSessionID, sess.TmuxTarget, sess.Name, string(sess.State),
		toMS(created), toMS(last))
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

// ListSessions returns a task's sessions ordered by created_at asc.
func (s *Store) ListSessions(ctx context.Context, taskID int64) ([]model.Session, error) {
	return s.querySessions(ctx, `SELECT `+sessionCols+` FROM sessions WHERE task_id = ?
		ORDER BY created_at, id`, taskID)
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
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET claude_session_id = ?, tmux_target = ?,
		name = ?, state = ?, last_event_at = ? WHERE id = ?`,
		sess.ClaudeSessionID, sess.TmuxTarget, sess.Name, string(sess.State), toMS(last), sess.ID)
	if err != nil {
		return err
	}
	if err := checkAffected(res, "session", sess.ID); err != nil {
		return err
	}
	sess.LastEventAt = last
	return nil
}

// SetSessionState sets a session's state and last_event_at (now if at is zero).
func (s *Store) SetSessionState(ctx context.Context, id int64, st model.SessionState, at time.Time) error {
	if !st.Valid() {
		return fmt.Errorf("store: invalid session state %q", st)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET state = ?, last_event_at = ? WHERE id = ?`,
		string(st), toMS(normTime(at)), id)
	if err != nil {
		return err
	}
	return checkAffected(res, "session", id)
}

// MarkStoppedIfLive sets the session to stopped (last_event_at = at, now if
// zero) only if it is currently live (running, waiting or idle), so it never
// overwrites a concurrent transition to ended. Rows with an empty tmux_target
// (hook-created rows for Claude processes atl did not launch, and sessions
// whose window is being launched — see ClaimStopped) are never changed, so a
// Recover working from a stale snapshot cannot stop a session mid-launch. It
// reports whether the row changed; an unknown id is not an error.
func (s *Store) MarkStoppedIfLive(ctx context.Context, id int64, at time.Time) (bool, error) {
	return s.changed(ctx, `UPDATE sessions SET state = 'stopped', last_event_at = ?
		WHERE id = ? AND state IN ('running', 'waiting', 'idle') AND tmux_target != ''`, toMS(normTime(at)), id)
}

// ClaimStopped atomically moves a stopped session to idle (last_event_at =
// at, now if zero) and clears its tmux_target, reporting whether this caller
// won the claim. Only the winner may launch a window for the session. The
// target is cleared because the old window is gone and a live row with an
// empty target is one Recover leaves alone while its window is launched; the
// launcher records the new window with SetSessionLaunch.
func (s *Store) ClaimStopped(ctx context.Context, id int64, at time.Time) (bool, error) {
	return s.changed(ctx, `UPDATE sessions SET state = 'idle', tmux_target = '', last_event_at = ?
		WHERE id = ? AND state = 'stopped'`, toMS(normTime(at)), id)
}

// SetSessionLaunch records the tmux window and name of a launched session,
// leaving every other column (state, claude_session_id, ...) untouched.
func (s *Store) SetSessionLaunch(ctx context.Context, id int64, tmuxTarget, name string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET tmux_target = ?, name = ? WHERE id = ?`,
		tmuxTarget, name, id)
	if err != nil {
		return err
	}
	return checkAffected(res, "session", id)
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
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET claude_session_id = ?, state = 'idle',
		last_event_at = ? WHERE id = ?`, claudeSessionID, toMS(normTime(at)), id)
	if err != nil {
		return err
	}
	return checkAffected(res, "session", id)
}

// SetClaudeSessionID records a new Claude session id (e.g. after
// auto-compaction) and sets last_event_at (now if at is zero), leaving state
// and every other column untouched.
func (s *Store) SetClaudeSessionID(ctx context.Context, id int64, claudeSessionID string, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET claude_session_id = ?, last_event_at = ? WHERE id = ?`,
		claudeSessionID, toMS(normTime(at)), id)
	if err != nil {
		return err
	}
	return checkAffected(res, "session", id)
}

// SetStateIf moves a session from state from to state to (last_event_at =
// at, now if zero) only if it is currently in from, reporting whether the
// row changed. An unknown id is not an error.
func (s *Store) SetStateIf(ctx context.Context, id int64, from, to model.SessionState, at time.Time) (bool, error) {
	if !from.Valid() || !to.Valid() {
		return false, fmt.Errorf("store: invalid session state transition %q -> %q", from, to)
	}
	return s.changed(ctx, `UPDATE sessions SET state = ?, last_event_at = ? WHERE id = ? AND state = ?`,
		string(to), toMS(normTime(at)), id, string(from))
}

// TouchSession sets a session's last_event_at (now if at is zero) only.
func (s *Store) TouchSession(ctx context.Context, id int64, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_event_at = ? WHERE id = ?`,
		toMS(normTime(at)), id)
	if err != nil {
		return err
	}
	return checkAffected(res, "session", id)
}
