package model

import (
	"strings"
	"testing"
)

func TestSlugify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Fix login bug", "fix-login-bug"},
		{"  --Hello,   World!!  ", "hello-world"},
		{"already-a-slug", "already-a-slug"},
		{"CamelCase123", "camelcase123"},
		{"", "task"},
		{"!!!", "task"},
		{"Café crème", "caf-cr-me"},
		{"日本語", "task"},
		{strings.Repeat("a", 50), strings.Repeat("a", 40)},
		{strings.Repeat("a", 39) + " b", strings.Repeat("a", 39)},
		{strings.Repeat("abc ", 15), strings.Repeat("abc-", 9) + "abc"},
	}
	for _, c := range cases {
		got := Slugify(c.in)
		if got != c.want {
			t.Errorf("Slugify(%q) = %q, want %q", c.in, got, c.want)
		}
		if len(got) > 40 || strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
			t.Errorf("Slugify(%q) = %q: invalid slug", c.in, got)
		}
	}
}

func TestParseStatus(t *testing.T) {
	for _, s := range []string{"backlog", "active", "review", "done", "cancelled"} {
		got, err := ParseStatus(s)
		if err != nil || string(got) != s {
			t.Errorf("ParseStatus(%q) = %q, %v", s, got, err)
		}
	}
	for _, s := range []string{"", "Active", "open", "canceled"} {
		if _, err := ParseStatus(s); err == nil {
			t.Errorf("ParseStatus(%q): expected error", s)
		}
	}
}

func TestParseMode(t *testing.T) {
	for _, s := range []string{"rw", "ro"} {
		got, err := ParseMode(s)
		if err != nil || string(got) != s {
			t.Errorf("ParseMode(%q) = %q, %v", s, got, err)
		}
	}
	for _, s := range []string{"", "RW", "wo"} {
		if _, err := ParseMode(s); err == nil {
			t.Errorf("ParseMode(%q): expected error", s)
		}
	}
}

func TestParseSessionState(t *testing.T) {
	for _, s := range AllSessionStates {
		if _, err := ParseSessionState(string(s)); err != nil {
			t.Errorf("ParseSessionState(%q): %v", s, err)
		}
	}
	if _, err := ParseSessionState("dead"); err == nil {
		t.Error("expected error")
	}
}

func TestStatusOpen(t *testing.T) {
	want := map[Status]bool{
		StatusBacklog: true, StatusActive: true, StatusReview: true,
		StatusDone: false, StatusCancelled: false, Status("bogus"): false,
	}
	for s, w := range want {
		if s.Open() != w {
			t.Errorf("%q.Open() = %v, want %v", s, !w, w)
		}
	}
	if len(AllStatuses) != 5 || AllStatuses[0] != StatusBacklog || AllStatuses[4] != StatusCancelled {
		t.Errorf("AllStatuses order wrong: %v", AllStatuses)
	}
}

func TestSessionStateLive(t *testing.T) {
	want := map[SessionState]bool{
		StateRunning: true, StateWaiting: true, StateIdle: true,
		StateEnded: false, StateStopped: false, SessionState(""): false,
	}
	for s, w := range want {
		if s.Live() != w {
			t.Errorf("%q.Live() = %v, want %v", s, !w, w)
		}
	}
}
