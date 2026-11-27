package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/andreujuanc/atelier/internal/model"
)

// GetNotes returns task id's notes text ("" if it has none).
func (s *Store) GetNotes(ctx context.Context, id int64) (string, error) {
	return readNotes(ctx, s.db, id)
}

func readNotes(ctx context.Context, q querier, id int64) (string, error) {
	var text string
	err := q.QueryRowContext(ctx, `SELECT notes FROM tasks WHERE id = ?`, id).Scan(&text)
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
// model.MaxNotesLen bytes is refused (model.ErrNotesTooLarge). A write that
// changes the notes is recorded as author's (see NotesBlame).
func (s *Store) SetNotes(ctx context.Context, id int64, author, text, base string) (string, error) {
	return s.writeNotes(ctx, id, author, text, &base)
}

// ReplaceNotes is SetNotes without the compare: text replaces whatever the
// notes are now ("" clears them).
func (s *Store) ReplaceNotes(ctx context.Context, id int64, author, text string) (string, error) {
	return s.writeNotes(ctx, id, author, text, nil)
}

func (s *Store) writeNotes(ctx context.Context, id int64, author, text string, base *string) (string, error) {
	if !model.ValidAuthor(author) {
		return "", invalidAuthor(author)
	}
	text = model.SanitizeNotes(text)
	if len(text) > model.MaxNotesLen {
		return "", model.NotesTooLarge(len(text))
	}
	tx, err := s.db.BeginTx(ctx, nil) // BEGIN IMMEDIATE (_txlock)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	cur, err := readNotes(ctx, tx, id)
	if err != nil {
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
	if err := recordLineAuthors(ctx, tx, id, author, text, nil); err != nil {
		return "", err
	}
	return text, tx.Commit()
}

// AppendNote atomically appends the line "- YYYY-MM-DD [author] body"
// (today, local time) to task id's notes, after a newline if they are not
// empty, and returns the line. The body is sanitised (model.SanitizeNotes)
// and trimmed, and must not be empty; model.ValidAuthor must accept author;
// the append is recorded as theirs (see NotesBlame). An append that would
// make the notes longer than model.MaxNotesLen is refused
// (model.ErrNotesTooLarge).
func (s *Store) AppendNote(ctx context.Context, id int64, author, body string) (string, error) {
	body = strings.TrimSpace(model.SanitizeNotes(body))
	if body == "" {
		return "", fmt.Errorf("store: empty note")
	}
	if !model.ValidAuthor(author) {
		return "", invalidAuthor(author)
	}
	line := model.NoteLine(now(), author, body)
	tx, err := s.db.BeginTx(ctx, nil) // BEGIN IMMEDIATE (_txlock)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	cur, err := readNotes(ctx, tx, id)
	if err != nil {
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
	if err := recordLineAuthors(ctx, tx, id, author, next, &cur); err != nil {
		return "", err
	}
	return line, tx.Commit()
}

func invalidAuthor(a string) error {
	return fmt.Errorf("store: invalid note author %q (want user, assistant or agent)", a)
}

// maxLineAuthors bounds the notes_line_authors rows of one author in a
// task.
const maxLineAuthors = 10000

// recordLineAuthors records, in tx, author as the first writer of each line
// of text (by its canonLineV1 form; blank ones are skipped), copy by copy
// (the k-th copy of a line has its own row), unless one is recorded
// already. Rows are never changed or deleted (only with their task), so a
// line keeps its first writer however often it is removed and written
// again. For an append, prev is the text appended to: only the copies
// beyond those prev has are the author's (prev's are recorded already, or
// past a cap, and stay so); nil for a write of the whole text, which
// records every line.
//
// At maxLineAuthors rows an author records nothing more: its new lines
// blame as unknown. Such a line could come back in a whole-text write of
// the user's (a stale editor buffer, a restored copy) and must not become
// theirs: while any other author is at the cap, the user's whole-text
// writes record nothing (their new lines blame as unknown too), only
// their appends do.
func recordLineAuthors(ctx context.Context, tx *sql.Tx, id int64, author, text string, prev *string) error {
	rows, err := tx.QueryContext(ctx, `SELECT author, count(*) FROM notes_line_authors WHERE task_id = ? GROUP BY author`, id)
	if err != nil {
		return err
	}
	n, othersFull := 0, false
	for rows.Next() {
		var a string
		var c int
		if err := rows.Scan(&a, &c); err != nil {
			rows.Close()
			return err
		}
		if a == author {
			n = c
		} else if c >= maxLineAuthors {
			othersFull = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if n >= maxLineAuthors {
		return nil
	}
	if prev == nil && author == model.AuthorUser && othersFull {
		return nil
	}
	had := map[string]int{} // copies of each line in prev
	if prev != nil {
		for _, l := range notesLines(*prev) {
			had[canonLineV1(l)]++
		}
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO notes_line_authors (task_id, line_hash, copy, author, at)
		VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	at := toMS(now())
	copies := map[string]int{}
	for _, l := range notesLines(text) {
		c := canonLineV1(l)
		if c == "" {
			continue
		}
		if copies[c]++; copies[c] <= had[c] {
			continue
		}
		h := lineHashV1(c)
		res, err := stmt.ExecContext(ctx, id, h[:], copies[c], author, at)
		if err != nil {
			return err
		}
		added, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n += int(added); n >= maxLineAuthors {
			return nil
		}
	}
	return nil
}

// canonLineV1 is the form of a notes line whose sha256 notes_line_authors
// keeps (lineHashV1): invalid UTF-8, U+FFFD, control characters and format
// characters (Cf: BOM, zero-width, bidi marks) dropped (tabs first turned
// into spaces), runs of whitespace made one space, trimmed; "" is a blank
// line. It is what every writer leaves of a line: the TUI editor turns
// tabs into spaces and drops U+FFFD and control characters,
// model.SanitizeNotes turns invalid UTF-8 into U+FFFD. A line an external
// editor rewrites otherwise (NFC, wrapping, smart quotes) on a round trip
// of the user's becomes theirs: the user's review of every learning is the
// last defence.
//
// FROZEN: rows keep only hashes, so a change would orphan every row, and
// an orphaned agent line could then be claimed by a save of the user's. A
// new form needs a new table and a migration.
func canonLineV1(l string) string {
	l = strings.Map(func(r rune) rune {
		switch {
		case r == '\t':
			return ' '
		case r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, strings.ToValidUTF8(l, ""))
	return strings.Join(strings.Fields(l), " ")
}

// lineHashV1 is what notes_line_authors keeps of a line: the sha256 of its
// canonLineV1 form. FROZEN, like canonLineV1.
func lineHashV1(l string) [sha256.Size]byte { return sha256.Sum256([]byte(canonLineV1(l))) }

// backfillNoteLinesV1 is migration v7's step: it records the lines of every
// task's notes as model.AuthorUnknown's, those notes being written before
// atl recorded who wrote them. Text from before v7 may not be sanitised
// (v5 copied task_notes as they were), so both the lines of the raw text
// and those model.SanitizeNotes would split it into (at \r\n and lone \r
// too) are recorded: whatever the next write keeps is unknown's. FROZEN:
// it uses only V1 functions and its own copy of that split, never code
// that may change.
func backfillNoteLinesV1(ctx context.Context, tx *sql.Tx) error {
	const maxRows = 10000 // maxLineAuthors as of v7
	rows, err := tx.QueryContext(ctx, `SELECT id, notes FROM tasks WHERE notes != ''`)
	if err != nil {
		return err
	}
	notes := map[int64]string{}
	for rows.Next() {
		var id int64
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			rows.Close()
			return err
		}
		notes[id] = text
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO notes_line_authors (task_id, line_hash, copy, author, at)
		VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	at := time.Now().UnixMilli()
	for id, text := range notes {
		split := strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
		var order []string
		need := map[string]int{} // canonical line → copies to record
		for _, t := range []string{split, text} {
			n := map[string]int{}
			for _, l := range strings.Split(t, "\n") {
				c := canonLineV1(l)
				if c == "" {
					continue
				}
				if n[c]++; n[c] > need[c] {
					if need[c] == 0 {
						order = append(order, c)
					}
					need[c] = n[c]
				}
			}
		}
		added := 0
	lines:
		for _, c := range order {
			h := lineHashV1(c)
			for k := 1; k <= need[c]; k++ {
				if added >= maxRows {
					break lines
				}
				if _, err := stmt.ExecContext(ctx, id, h[:], k, model.AuthorUnknown, at); err != nil {
					return err
				}
				added++
			}
		}
	}
	return nil
}

// NotesBlame returns task id's notes, one entry per line ("" has none),
// each with who first wrote it, whatever its "[author]" tag says, and when
// (see recordLineAuthors). Lines are matched by their canonLineV1 form and
// by count, not position: the k-th copy of a line in the notes now is the
// k-th copy ever recorded. A line blank in that form is Blank, with no
// author. A line with nothing recorded is model.AuthorUnknown with a zero
// At (written past a cap). Lines of the notes from before migration v7 are
// unknown's, with the time of the migration.
func (s *Store) NotesBlame(ctx context.Context, id int64) ([]model.BlamedLine, error) {
	tx, err := s.readTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	cur, err := readNotes(ctx, tx, id)
	if err != nil || cur == "" {
		return nil, err
	}
	type key struct {
		hash [sha256.Size]byte
		copy int
	}
	type who struct {
		author string
		at     int64
	}
	recorded := map[key]who{}
	rows, err := tx.QueryContext(ctx, `SELECT line_hash, copy, author, at FROM notes_line_authors WHERE task_id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h []byte
		var k key
		var w who
		if err := rows.Scan(&h, &k.copy, &w.author, &w.at); err != nil {
			return nil, err
		}
		copy(k.hash[:], h)
		recorded[k] = w
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	lines := notesLines(cur)
	out := make([]model.BlamedLine, len(lines))
	copies := map[string]int{}
	for i, l := range lines {
		c := canonLineV1(l)
		if c == "" {
			out[i] = model.BlamedLine{Text: l, Blank: true}
			continue
		}
		copies[c]++
		out[i] = model.BlamedLine{Text: l, Author: model.AuthorUnknown}
		if w, ok := recorded[key{lineHashV1(c), copies[c]}]; ok {
			out[i].Author, out[i].At = w.author, msTime(w.at)
		}
	}
	return out, nil
}

// notesLines splits notes text into lines (a final newline ends the last
// one; "" has none).
func notesLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}
