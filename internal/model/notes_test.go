package model

import (
	"errors"
	"testing"
	"time"
)

func TestSanitizeNotes(t *testing.T) {
	for in, want := range map[string]string{
		"":                                "",
		"a\r\nb\rc\n":                     "a\nb\nc",
		"\n  indented\n\ttab\x1b[31m\x07": "\n  indented\n\ttab[31m",
		"bad \xff utf8\u0085":             "bad � utf8",
		"keep\n\n\nblank lines \n\t\n ":   "keep\n\n\nblank lines",
	} {
		if got := SanitizeNotes(in); got != want {
			t.Errorf("SanitizeNotes(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNoteLine(t *testing.T) {
	day := time.Date(2026, 11, 4, 12, 0, 0, 0, time.Local)
	if got := NoteLine(day, AuthorAssistant, "body\nmore"); got != "- 2026-11-04 [assistant] body\nmore" {
		t.Fatalf("NoteLine = %q", got)
	}
}

func TestNotesErrors(t *testing.T) {
	var err error = &NotesConflictError{Current: "x"}
	if !errors.Is(err, ErrNotesConflict) || errors.Is(err, ErrNotesTooLarge) {
		t.Fatal("conflict error does not match ErrNotesConflict only")
	}
	if err := NotesTooLarge(70000); !errors.Is(err, ErrNotesTooLarge) {
		t.Fatal(err)
	}
}
