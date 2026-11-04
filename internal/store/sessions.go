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
