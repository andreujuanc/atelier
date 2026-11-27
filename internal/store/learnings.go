package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Learning is a learning (atl learn's Analyse makes it, atl learnings
// reviews it). Status is one of learn.Statuses.
type Learning struct {
	ID        int64
	Text      string
	Status    string
	Edited    bool // the user reworded it (atl learnings edit)
	CreatedAt time.Time
	UpdatedAt time.Time
	DecidedAt time.Time // when it was accepted or rejected; zero if never
}

// LearningOccurrence is one occurrence of a learning: a place where the
// user stated it (learn.Occurrence), by its stable key
// (learn.OccurrenceKey).
type LearningOccurrence struct {
	Key        string
	LearningID int64
	TaskID     int64
	TaskSlug   string // read only (ListLearningOccurrences)
	ItemID     string // the item it quotes (learn.ItemID)
	Session    int64  // atelier's session id; 0 for a notes line
	At         time.Time
	Strength   string
	Polarity   string
	Quote      string
	Text       string
}

// ErrLearningStatus is returned (wrapped) when a learning's status does
// not allow a change (SetLearningStatus).
var ErrLearningStatus = errors.New("not allowed from its status")

const learningCols = `id, text, status, edited, created_at, updated_at, decided_at`

func scanLearning(sc interface{ Scan(...any) error }) (Learning, error) {
	var l Learning
	var created, updated, decided int64
	if err := sc.Scan(&l.ID, &l.Text, &l.Status, &l.Edited, &created, &updated, &decided); err != nil {
		return Learning{}, err
	}
	l.CreatedAt, l.UpdatedAt = msTime(created), msTime(updated)
	if decided != 0 {
		l.DecidedAt = msTime(decided)
	}
	return l, nil
}

// ListLearnings returns every learning, by id.
func (s *Store) ListLearnings(ctx context.Context) ([]Learning, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+learningCols+` FROM learnings ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Learning
	for rows.Next() {
		l, err := scanLearning(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// GetLearning returns learning id; ErrNotFound (wrapped) if there is none.
func (s *Store) GetLearning(ctx context.Context, id int64) (Learning, error) {
	l, err := scanLearning(s.db.QueryRowContext(ctx, `SELECT `+learningCols+` FROM learnings WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Learning{}, fmt.Errorf("learning %d: %w", id, ErrNotFound)
	}
	return l, err
}

// ListLearningOccurrences returns the occurrences of learning id, or of
// every learning when id is 0, with their task's slug, oldest first (by
// at, then key).
func (s *Store) ListLearningOccurrences(ctx context.Context, id int64) ([]LearningOccurrence, error) {
	q := `SELECT o.key, o.learning_id, o.task_id, t.slug, o.item_id, o.session, o.at, o.strength, o.polarity, o.quote, o.text
		FROM learning_occurrences o JOIN tasks t ON t.id = o.task_id`
	var args []any
	if id != 0 {
		q += ` WHERE o.learning_id = ?`
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY o.at, o.key`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LearningOccurrence
	for rows.Next() {
		var o LearningOccurrence
		var at int64
		if err := rows.Scan(&o.Key, &o.LearningID, &o.TaskID, &o.TaskSlug, &o.ItemID, &o.Session, &at, &o.Strength, &o.Polarity, &o.Quote, &o.Text); err != nil {
			return nil, err
		}
		o.At = msTime(at)
		out = append(out, o)
	}
	return out, rows.Err()
}

// LearningOccurrenceKeys returns which of keys are stored (assigned to a
// learning).
func (s *Store) LearningOccurrenceKeys(ctx context.Context, keys []string) (map[string]bool, error) {
	return s.keysIn(ctx, `SELECT key FROM learning_occurrences WHERE key IN `, keys)
}

// keysIn returns which of keys query (ending in "IN ") finds.
func (s *Store) keysIn(ctx context.Context, query string, keys []string) (map[string]bool, error) {
	out := map[string]bool{}
	for chunk := range slices.Chunk(keys, 500) {
		q := query + `(?` + strings.Repeat(",?", len(chunk)-1) + `)`
		args := make([]any, len(chunk))
		for i, k := range chunk {
			args[i] = k
		}
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				rows.Close()
				return nil, err
			}
			out[k] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Assignment is an occurrence to store with the learning it goes to:
// Occ.LearningID, or when that is 0 the new learning NewIndex (in
// AssignOccurrences' texts).
type Assignment struct {
	Occ      LearningOccurrence
	NewIndex int
}

// AssignOccurrences stores, in one transaction, the new learnings texts
// (as candidates; a text that some learning already has, by Unicode case
// folding, is that learning instead, whatever its status) and the
// occurrences of as with their learning; an occurrence whose key is
// stored already is left as it is. It returns the ids of the learnings
// it created.
func (s *Store) AssignOccurrences(ctx context.Context, texts []string, as []Assignment, at time.Time) (created []int64, err error) {
	if at.IsZero() {
		at = now()
	}
	tx, err := s.db.BeginTx(ctx, nil) // BEGIN IMMEDIATE (_txlock)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	known, err := learningTexts(ctx, tx)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(texts))
	for i, text := range texts {
		if id, ok := sameText(known, text, 0); ok {
			ids[i] = id
			continue
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO learnings (text, status, created_at, updated_at) VALUES (?, 'candidate', ?, ?)`,
			text, toMS(at), toMS(at))
		if err != nil {
			return nil, err
		}
		if ids[i], err = res.LastInsertId(); err != nil {
			return nil, err
		}
		known = append(known, idText{ids[i], text})
		created = append(created, ids[i])
	}
	for _, a := range as {
		o := a.Occ
		if o.LearningID == 0 {
			if a.NewIndex < 0 || a.NewIndex >= len(ids) {
				return nil, fmt.Errorf("occurrence %s: no new learning %d", o.Key, a.NewIndex)
			}
			o.LearningID = ids[a.NewIndex]
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO learning_occurrences
			(key, learning_id, task_id, item_id, session, at, strength, polarity, quote, text) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (key) DO NOTHING`,
			o.Key, o.LearningID, o.TaskID, o.ItemID, o.Session, toMS(o.At), o.Strength, o.Polarity, o.Quote, o.Text); err != nil {
			return nil, fmt.Errorf("occurrence of learning %d: %w", o.LearningID, err)
		}
	}
	return created, tx.Commit()
}

// SetLearningStatus moves learning id to status to, if its status is one
// of from: ErrNotFound if there is no such learning, ErrLearningStatus
// (with its status) if its status is not in from. decide sets its
// decided_at (an accept or reject by the user); an automatic transition
// leaves it.
func (s *Store) SetLearningStatus(ctx context.Context, id int64, from []string, to string, decide bool) error {
	t := toMS(now())
	q := `UPDATE learnings SET status = ?, updated_at = ?`
	args := []any{to, t}
	if decide {
		q += `, decided_at = ?`
		args = append(args, t)
	}
	q += ` WHERE id = ? AND status IN (?` + strings.Repeat(",?", len(from)-1) + `)`
	args = append(args, id)
	for _, f := range from {
		args = append(args, f)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		return err
	}
	l, err := s.GetLearning(ctx, id)
	if err != nil {
		return err
	}
	return fmt.Errorf("learning %d is %s: %w", id, l.Status, ErrLearningStatus)
}

// SetLearningText rewords learning id (the user's: it is marked edited).
// A text another learning has (by Unicode case folding) is
// ErrLearningExists.
func (s *Store) SetLearningText(ctx context.Context, id int64, text string) error {
	tx, err := s.db.BeginTx(ctx, nil) // BEGIN IMMEDIATE (_txlock)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	known, err := learningTexts(ctx, tx)
	if err != nil {
		return err
	}
	if other, ok := sameText(known, text, id); ok {
		return fmt.Errorf("learning %d: %w", other, ErrLearningExists)
	}
	res, err := tx.ExecContext(ctx, `UPDATE learnings SET text = ?, edited = 1, updated_at = ? WHERE id = ?`, text, toMS(now()), id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("learning %d: %w", id, ErrNotFound)
	}
	return tx.Commit()
}

// DeleteLearning deletes learning id and its occurrences, and returns how
// many occurrences went with it.
func (s *Store) DeleteLearning(ctx context.Context, id int64) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM learning_occurrences WHERE learning_id = ?`, id).Scan(&n); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM learnings WHERE id = ?`, id)
	if err != nil {
		return 0, err
	}
	if d, err := res.RowsAffected(); err != nil {
		return 0, err
	} else if d == 0 {
		return 0, fmt.Errorf("learning %d: %w", id, ErrNotFound)
	}
	return n, tx.Commit()
}

// DeleteEmptyCandidates deletes the candidates with no occurrence left
// (their tasks were deleted) that the user did not edit, and returns
// how many.
func (s *Store) DeleteEmptyCandidates(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM learnings WHERE status = 'candidate' AND edited = 0
		AND NOT EXISTS (SELECT 1 FROM learning_occurrences o WHERE o.learning_id = learnings.id)`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

type idText struct {
	id   int64
	text string
}

// learningTexts returns every learning's id and text, by id.
func learningTexts(ctx context.Context, tx *sql.Tx) ([]idText, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, text FROM learnings ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []idText
	for rows.Next() {
		var l idText
		if err := rows.Scan(&l.id, &l.text); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// sameText returns the first learning of known but except whose text is
// text by Unicode case folding.
func sameText(known []idText, text string, except int64) (int64, bool) {
	for _, l := range known {
		if l.id != except && strings.EqualFold(l.text, text) {
			return l.id, true
		}
	}
	return 0, false
}

// ErrLearningExists is returned (wrapped) when a learning would get the
// text of another (SetLearningText).
var ErrLearningExists = errors.New("another learning says that")

// LearningSkip is an occurrence Analyse left unassigned twice.
type LearningSkip struct {
	Key    string
	TaskID int64
}

// LearningSkipTries is in how many runs Analyse must leave a key out
// before it is skipped (LearningSkipKeys).
const LearningSkipTries = 2

// AddLearningSkips records that Analyse left skips out in one more run,
// under analyse version: a key's tries count up (from 1 again under
// another version). It returns how many are skipped now (reached
// LearningSkipTries).
func (s *Store) AddLearningSkips(ctx context.Context, skips []LearningSkip, version string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n := 0
	for _, sk := range skips {
		var tries int
		if err := tx.QueryRowContext(ctx, `INSERT INTO learning_skips (key, task_id, analyse_version, tries) VALUES (?, ?, ?, 1)
			ON CONFLICT (key) DO UPDATE SET
				tries = CASE WHEN analyse_version = excluded.analyse_version THEN tries + 1 ELSE 1 END,
				analyse_version = excluded.analyse_version
			RETURNING tries`, sk.Key, sk.TaskID, version).Scan(&tries); err != nil {
			return 0, err
		}
		if tries >= LearningSkipTries {
			n++
		}
	}
	return n, tx.Commit()
}

// LearningSkipKeys deletes the skips of other analyse versions than
// version and returns which of keys are skipped under it: left out in
// LearningSkipTries runs or more.
func (s *Store) LearningSkipKeys(ctx context.Context, keys []string, version string) (map[string]bool, error) {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM learning_skips WHERE analyse_version != ?`, version); err != nil {
		return nil, err
	}
	return s.keysIn(ctx, fmt.Sprintf(`SELECT key FROM learning_skips WHERE tries >= %d AND key IN `, LearningSkipTries), keys)
}

// ClearLearningSkips deletes every skip (atl learn --retry-skipped) and
// returns how many keys were skipped (LearningSkipTries reached).
func (s *Store) ClearLearningSkips(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM learning_skips WHERE tries >= ?`, LearningSkipTries).Scan(&n); err != nil {
		return 0, err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM learning_skips`)
	return n, err
}
