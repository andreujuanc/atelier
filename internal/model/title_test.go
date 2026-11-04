package model

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeTitle(t *testing.T) {
	for _, c := range []struct {
		in, want string
		err      error
	}{
		{"Fix login", "Fix login", nil},
		{"  Fix   the  login  ", "Fix the login", nil},
		{"Ünïcødé ✓ 日本", "Ünïcødé ✓ 日本", nil},
		{"", "", ErrTitleEmpty},
		{"   ", "", ErrTitleEmpty},
		{"a\tb", "", ErrTitleControl},
		{"a\nb", "", ErrTitleControl},
		{"\x1b[31mred", "", ErrTitleControl},
		{"del\x7f", "", ErrTitleControl},
		{"c1\u0085", "", ErrTitleControl},
		{"bad\xff", "", ErrTitleUTF8},
		{strings.Repeat("é", MaxTitleLen), strings.Repeat("é", MaxTitleLen), nil}, // runes, not bytes
		{"  " + strings.Repeat("a", MaxTitleLen) + "  ", strings.Repeat("a", MaxTitleLen), nil},
		{strings.Repeat("a", MaxTitleLen+1), "", ErrTitleTooLong},
		{strings.Repeat("ab ", 70), "", ErrTitleTooLong}, // 209 after trimming
	} {
		got, err := NormalizeTitle(c.in)
		if got != c.want || !errors.Is(err, c.err) {
			t.Errorf("NormalizeTitle(%q) = %q, %v; want %q, %v", c.in, got, err, c.want, c.err)
		}
	}
}

func TestCheckTitleLen(t *testing.T) {
	if err := CheckTitleLen(strings.Repeat("日", MaxTitleLen)); err != nil {
		t.Fatal(err)
	}
	err := CheckTitleLen(strings.Repeat("x", 511))
	if !errors.Is(err, ErrTitleTooLong) || !strings.Contains(err.Error(), "the limit is 200 characters") || !strings.Contains(err.Error(), "511") {
		t.Fatalf("err = %v", err)
	}
}
