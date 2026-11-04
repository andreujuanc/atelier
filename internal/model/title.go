package model

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Title validation errors.
var (
	ErrTitleEmpty   = errors.New("title is empty")
	ErrTitleControl = errors.New("title contains control characters")
	ErrTitleUTF8    = errors.New("title is not valid UTF-8")
)

// NormalizeTitle validates a new task title for a rename and returns it in
// the stored form: one line, trimmed, with runs of spaces collapsed to one.
// Control characters (newlines and tabs included) and invalid UTF-8 are
// rejected, as is a title that is empty after trimming.
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
	return t, nil
}
