package model

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// MaxNotesLen caps a task's notes text, in bytes (64 KiB).
const MaxNotesLen = 64 << 10

var (
	// ErrNotesTooLarge is returned (wrapped) when a write would make a
	// task's notes longer than MaxNotesLen.
	ErrNotesTooLarge = errors.New("notes too large")
	// ErrNotesConflict is returned (as a *NotesConflictError) when a task's
	// notes changed since the text an edit started from.
	ErrNotesConflict = errors.New("notes changed meanwhile")
)

// NotesConflictError is a failed compare-and-set of a task's notes: they
// are no longer the text the edit started from. Current is what they are
// now. errors.Is(err, ErrNotesConflict) matches it.
type NotesConflictError struct {
	Current string
}

func (e *NotesConflictError) Error() string {
	return "notes changed since editing started (e.g. an agent appended a note)"
}

// Is makes errors.Is(err, ErrNotesConflict) true.
func (e *NotesConflictError) Is(target error) bool { return target == ErrNotesConflict }

// NotesTooLarge returns the error for notes of n bytes (> MaxNotesLen).
func NotesTooLarge(n int) error {
	return fmt.Errorf("%w: %d bytes, the limit is %d (64 KiB); shorten them (TUI m, or atl notes <task> --set)",
		ErrNotesTooLarge, n, MaxNotesLen)
}

// SanitizeNotes returns notes text in its stored form: invalid UTF-8
// becomes U+FFFD, \r\n and lone \r become \n, every other control
// character except \n and \t is dropped, and trailing whitespace at the
// end of the text is trimmed (leading indentation and blank lines inside
// are kept).
func SanitizeNotes(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, s)
	return strings.TrimRightFunc(s, unicode.IsSpace)
}

// NoteLine formats an appended note: "- YYYY-MM-DD [author] body", with
// the date of day in local time. body is used as given (see
// SanitizeNotes); newlines inside it are kept.
func NoteLine(day time.Time, author, body string) string {
	return "- " + day.Local().Format("2006-01-02") + " [" + author + "] " + body
}
