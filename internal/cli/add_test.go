package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReposAndAdd(t *testing.T) {
	root := setup(t)
	repos := filepath.Join(root, "repos")
	// A second repo (a clone of the same origin) and a plain dir.
	git(t, repos, "clone", "-q", filepath.Join(root, "origin.git"), "docs")
	if err := os.MkdirAll(filepath.Join(repos, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repos, "clone", "-q", filepath.Join(root, "origin.git"), "web")

	if code, out, errs := atl(t, "repos"); code != 0 || out != "api\ndocs\nweb\n" {
		t.Fatalf("repos: %d %q %s", code, out, errs)
	}
	if code, _, _ := atl(t, "repos", "extra"); code != 2 {
		t.Fatalf("repos extra arg: %d", code)
	}

	if code, _, errs := atl(t, "new", "Fix", "--repo", "api", "--no-fetch"); code != 0 {
		t.Fatalf("new: %d %s", code, errs)
	}
	dir := filepath.Join(root, "tasks", "fix")

	code, out, errs := atl(t, "add", "fix", "docs:ro")
	if code != 0 || out != "fix\tdocs\tro\t"+filepath.Join(repos, "docs")+"\n" {
		t.Fatalf("add ro: %d %q %s", code, out, errs)
	}
	if !strings.Contains(errs, "need a new session (s) to see docs") {
		t.Fatalf("add ro note: %q", errs)
	}
	code, out, errs = atl(t, "add", "fi", "web", "--no-fetch")
	if code != 0 || out != "fix\tweb\trw\t"+filepath.Join(dir, "web")+"\n" {
		t.Fatalf("add rw: %d %q %s", code, out, errs)
	}
	if strings.Contains(errs, "fetching") {
		t.Fatalf("--no-fetch fetched: %s", errs)
	}
	if _, err := os.Stat(filepath.Join(dir, "web", ".git")); err != nil {
		t.Fatalf("no worktree: %v", err)
	}
	md, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil || !strings.Contains(string(md), "**web**") || !strings.Contains(string(md), "**docs**") {
		t.Fatalf("CLAUDE.md: %v\n%s", err, md)
	}

	// Usage errors: exit 2.
	for _, a := range [][]string{
		{"add", "fix"},
		{"add", "fix", "web", "more"},
		{"add", "fix", "web:rx"},
		{"add", "fix", ":ro"},
		{"add", "fix", "web", "--bogus"},
	} {
		if code, _, errs := atl(t, a...); code != 2 {
			t.Errorf("%v: exit %d %s", a, code, errs)
		}
	}
	// Failures: exit 1.
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"add", "fix", "web"}, "already has repo web"},
		{[]string{"add", "fix", "notes:ro"}, "not a git repository"},
		{[]string{"add", "nope", "docs"}, "not found"},
	} {
		if code, _, errs := atl(t, c.args...); code != 1 || !strings.Contains(errs, c.want) {
			t.Errorf("%v: exit %d %s", c.args, code, errs)
		}
	}
	if code, _, errs := atl(t, "done", "fix"); code != 0 {
		t.Fatalf("done: %d %s", code, errs)
	}
	if code, _, errs := atl(t, "add", "fix", "docs"); code != 1 || !strings.Contains(errs, "is done") {
		t.Fatalf("add to done task: %d %s", code, errs)
	}
}
