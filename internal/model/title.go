package model

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Title validation errors.
var (
	ErrTitleEmpty   = errors.New("title is empty")
	ErrTitleControl = errors.New("title contains control characters")
	ErrTitleUTF8    = errors.New("title is not valid UTF-8")
	ErrTitleTooLong = fmt.Errorf("title is too long (the limit is %d characters)", MaxTitleLen)
)

// MaxTitleLen caps a task title, in runes, after trimming and collapsing
// spaces. `atl new`, `atl rename` and the TUI inputs all use it.
const MaxTitleLen = 200

// CheckTitleLen returns ErrTitleTooLong if title (in its stored form) is
// longer than MaxTitleLen runes.
func CheckTitleLen(title string) error {
	if n := utf8.RuneCountInString(title); n > MaxTitleLen {
		return fmt.Errorf("%w: it has %d", ErrTitleTooLong, n)
	}
	return nil
}

// NormalizeTitle validates a new task title for a rename and returns it in
// the stored form: one line, trimmed, with runs of spaces collapsed to one.
// Control characters (newlines and tabs included) and invalid UTF-8 are
// rejected, as is a title that is empty after trimming or longer than
// MaxTitleLen runes.
func NormalizeTitle(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", ErrTitleUTF8
	}
	if strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return "", ErrTitleControl
	}
	t := strings.Join(strings.Fields(s), " ")
	if t == "" {
		return "", ErrTitleEmpty
	}
	if err := CheckTitleLen(t); err != nil {
		return "", err
	}
	return t, nil
}
