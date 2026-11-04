package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/andreujuanc/atelier/internal/model"
)

// GetNotes returns task id's notes text ("" if it has none).
func (s *Store) GetNotes(ctx context.Context, id int64) (string, error) {
	var text string
	err := s.db.QueryRowContext(ctx, `SELECT notes FROM tasks WHERE id = ?`, id).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("task %d: %w", id, ErrNotFound)
	}
	return text, err
}

// SetNotes replaces task id's notes with text if they are still base (the
// text the edit started from): an optimistic compare-and-set. Otherwise
// nothing changes and the error is a *model.NotesConflictError holding the
// current text (errors.Is(err, model.ErrNotesConflict)). text is stored in
// its model.SanitizeNotes form, which is returned; more than
// model.MaxNotesLen bytes is refused (model.ErrNotesTooLarge).
func (s *Store) SetNotes(ctx context.Context, id int64, text, base string) (string, error) {
	return s.writeNotes(ctx, id, text, &base)
}

// ReplaceNotes is SetNotes without the compare: text replaces whatever the
// notes are now ("" clears them).
func (s *Store) ReplaceNotes(ctx context.Context, id int64, text string) (string, error) {
	return s.writeNotes(ctx, id, text, nil)
}

func (s *Store) writeNotes(ctx context.Context, id int64, text string, base *string) (string, error) {
	text = model.SanitizeNotes(text)
	if len(text) > model.MaxNotesLen {
		return "", model.NotesTooLarge(len(text))
	}
	tx, err := s.db.BeginTx(ctx, nil) // BEGIN IMMEDIATE (_txlock)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var cur string
	if err := tx.QueryRowContext(ctx, `SELECT notes FROM tasks WHERE id = ?`, id).Scan(&cur); errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("task %d: %w", id, ErrNotFound)
	} else if err != nil {
		return "", err
	}
	if base != nil && cur != *base {
		return "", &model.NotesConflictError{Current: cur}
	}
	if cur == text {
		return text, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET notes = ? WHERE id = ?`, text, id); err != nil {
		return "", err
	}
	return text, tx.Commit()
}

// AppendNote atomically appends the line "- YYYY-MM-DD [author] body"
// (today, local time) to task id's notes, after a newline if they are not
// empty, and returns the line. The body is sanitised (model.SanitizeNotes)
// and trimmed, and must not be empty; author is model.AuthorUser or
// model.AuthorAssistant. An append that would make the notes longer than
// model.MaxNotesLen is refused (model.ErrNotesTooLarge).
func (s *Store) AppendNote(ctx context.Context, id int64, author, body string) (string, error) {
	body = strings.TrimSpace(model.SanitizeNotes(body))
	if body == "" {
		return "", fmt.Errorf("store: empty note")
	}
	if !model.ValidAuthor(author) {
		return "", fmt.Errorf("store: invalid note author %q (want user or assistant)", author)
	}
	line := model.NoteLine(now(), author, body)
	tx, err := s.db.BeginTx(ctx, nil) // BEGIN IMMEDIATE (_txlock)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var cur string
	if err := tx.QueryRowContext(ctx, `SELECT notes FROM tasks WHERE id = ?`, id).Scan(&cur); errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("task %d: %w", id, ErrNotFound)
	} else if err != nil {
		return "", err
	}
	next := line
	if cur != "" {
		sep := "\n"
		if strings.HasSuffix(cur, "\n") {
			sep = ""
		}
		next = cur + sep + line
	}
	if len(next) > model.MaxNotesLen {
		return "", model.NotesTooLarge(len(next))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET notes = ? WHERE id = ?`, next, id); err != nil {
		return "", err
	}
	return line, tx.Commit()
}
