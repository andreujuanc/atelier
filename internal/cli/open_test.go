package cli

import (
	"strings"
	"testing"
)

func TestOpenFlags(t *testing.T) {
	setup(t)
	for _, c := range []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"open", "nope", "--session", "1", "--new"}, exitUsage, "mutually exclusive"},
		{[]string{"open", "nope", "--session", "0"}, exitUsage, "positive session id"},
		{[]string{"open", "nope", "--session", "x"}, exitUsage, "invalid argument"},
		{[]string{"open", "nope", "--session", "1"}, exitErr, "nope"},
		{[]string{"open", "nope", "--new"}, exitErr, "nope"},
		{[]string{"open", "nope"}, exitErr, "nope"},
	} {
		if code, _, errs := atl(t, c.args...); code != c.code || !strings.Contains(errs, c.msg) {
			t.Errorf("%v: exit %d %q, want %d %q", c.args, code, errs, c.code, c.msg)
		}
	}
	if code, out, _ := atl(t, "open", "--help"); code != 0 || !strings.Contains(out, "--session") || !strings.Contains(out, "--new") {
		t.Errorf("open --help: %d\n%s", code, out)
	}
}
