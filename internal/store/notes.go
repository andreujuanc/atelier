package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/andreujuanc/atelier/internal/model"
)

// AddNote inserts n, setting n.ID and filling a zero CreatedAt with now.
// The body must be non-empty (after trimming) and the author valid.
func (s *Store) AddNote(ctx context.Context, n *model.Note) error {
	if strings.TrimSpace(n.Body) == "" {
		return fmt.Errorf("store: empty note")
	}
	if !model.ValidAuthor(n.Author) {
		return fmt.Errorf("store: invalid note author %q (want user or assistant)", n.Author)
	}
	created := normTime(n.CreatedAt)
	res, err := s.db.ExecContext(ctx, `INSERT INTO task_notes (task_id, body, author, created_at)
		VALUES (?, ?, ?, ?)`, n.TaskID, n.Body, n.Author, toMS(created))
	if err != nil {
		return fmt.Errorf("store: insert note for task %d: %w", n.TaskID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	n.ID, n.CreatedAt = id, created
	return nil
}

// ListNotes returns a task's notes, newest first (by created_at, then id);
// at most limit of them if limit > 0.
func (s *Store) ListNotes(ctx context.Context, taskID int64, limit int) ([]model.Note, error) {
	q := `SELECT id, task_id, body, author, created_at FROM task_notes WHERE task_id = ?
		ORDER BY created_at DESC, id DESC`
	args := []any{taskID}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Note
	for rows.Next() {
		var n model.Note
		var created int64
		if err := rows.Scan(&n.ID, &n.TaskID, &n.Body, &n.Author, &created); err != nil {
			return nil, err
		}
		n.CreatedAt = msTime(created)
		out = append(out, n)
	}
	return out, rows.Err()
}
