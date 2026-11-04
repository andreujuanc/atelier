package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRename(t *testing.T) {
	root := setup(t)
	if code, _, errs := atl(t, "new", "Fix", "login", "--repo", "api", "--no-fetch"); code != 0 {
		t.Fatalf("new: %d %s", code, errs)
	}

	// words are joined like `atl note`; the new title is printed
	code, out, errs := atl(t, "rename", "fix", "Fix", "the  login", "page ")
	if code != 0 || out != "Fix the login page\n" {
		t.Fatalf("rename: %d %q %s", code, out, errs)
	}
	code, out, _ = atl(t, "ls", "--json")
	var ls []map[string]any
	if err := json.Unmarshal([]byte(out), &ls); code != 0 || err != nil || len(ls) != 1 {
		t.Fatalf("ls: %d %v %s", code, err, out)
	}
	if ls[0]["title"] != "Fix the login page" || ls[0]["slug"] != "fix-login" || ls[0]["branch"] != "atl/fix-login" ||
		ls[0]["task_dir"] != filepath.Join(root, "tasks", "fix-login") {
		t.Fatalf("after rename: %v", ls[0])
	}
	if _, err := os.Stat(filepath.Join(root, "tasks", "fix-login", "api")); err != nil {
		t.Fatalf("worktree moved: %v", err)
	}

	for _, c := range []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"rename"}, 2, ""},
		{[]string{"rename", "fix-login"}, 2, ""},
		{[]string{"rename", "fix-login", "  ", " "}, 2, "title is empty"},
		{[]string{"rename", "fix-login", "a\nb"}, 2, "control characters"},
		{[]string{"rename", "fix-login", "esc\x1b[31m"}, 2, "control characters"},
		{[]string{"rename", "nope", "x"}, 1, ""},
		{[]string{"rename", "fix-login", strings.Repeat("x", 201)}, 2, "title is too long (the limit is 200 characters)"},
		{[]string{"new", strings.Repeat("y", 150), strings.Repeat("y", 60), "--repo", "api", "--no-fetch"}, 2, "title is too long (the limit is 200 characters)"},
	} {
		code, out, errs := atl(t, c.args...)
		if code != c.code || out != "" || !strings.Contains(errs, c.msg) {
			t.Errorf("atl %q: %d %q %q, want exit %d with %q", c.args, code, out, errs, c.code, c.msg)
		}
	}
	if _, out, _ := atl(t, "show", "fix-login"); !strings.Contains(out, "Fix the login page") {
		t.Fatalf("rejected rename changed the title:\n%s", out)
	}
	if _, out, _ := atl(t, "ls", "--json"); strings.Count(out, `"slug"`) != 1 {
		t.Fatalf("a too long title created a task:\n%s", out)
	}
	// exactly the cap is fine for both
	max := strings.Repeat("z", 200)
	if code, out, errs := atl(t, "rename", "fix-login", max); code != 0 || out != max+"\n" {
		t.Fatalf("rename to 200 chars: %d %s", code, errs)
	}
	if code, _, errs := atl(t, "rename", "fix-login", "Fix the login page"); code != 0 {
		t.Fatalf("rename back: %d %s", code, errs)
	}

	// any status: closed tasks can be renamed too
	if code, _, errs := atl(t, "cancel", "fix-login", "--force"); code != 0 {
		t.Fatalf("cancel: %d %s", code, errs)
	}
	if code, out, errs := atl(t, "rename", "fix-login", "Abandoned login fix"); code != 0 || out != "Abandoned login fix\n" {
		t.Fatalf("rename closed: %d %q %s", code, out, errs)
	}
	if _, err := os.Stat(filepath.Join(root, "tasks", "fix-login")); err == nil {
		t.Fatal("rename recreated the closed task's dir")
	}
}
