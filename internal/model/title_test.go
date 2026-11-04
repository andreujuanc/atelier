package model

import (
	"errors"
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
	} {
		got, err := NormalizeTitle(c.in)
		if got != c.want || !errors.Is(err, c.err) {
			t.Errorf("NormalizeTitle(%q) = %q, %v; want %q, %v", c.in, got, err, c.want, c.err)
		}
	}
}
