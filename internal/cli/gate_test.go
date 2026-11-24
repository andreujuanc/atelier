package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/andreujuanc/atelier/internal/config"
)

// TestGateArgs: atl gate refuses a bad command line before it opens
// anything.
func TestGateArgs(t *testing.T) {
	setup(t)
	for _, c := range []struct {
		args []string
		msg  string
	}{
		{[]string{"gate"}, "needs a task slug"},
		{[]string{"gate", "a/b", "--task-id", "1"}, "invalid task slug"},
		{[]string{"gate", "..", "--task-id", "1"}, "invalid task slug"},
		{[]string{"gate", "fix"}, "--task-id is required"},
		{[]string{"gate", "fix", "--task-id", "-1"}, "--task-id is required"},
		{[]string{"gate", "fix", "--assistant"}, "--assistant takes no slug"},
		{[]string{"gate", "--assistant", "--task-id", "1"}, "--assistant takes no slug"},
		{[]string{"gate", "--assistant", "--ro-mount", "/x"}, "--assistant takes no slug"},
		{[]string{"gate", "a", "b"}, "accepts between 0 and 1 arg"},
	} {
		if code, _, errs := atl(t, c.args...); code != exitUsage || !strings.Contains(errs, c.msg) {
			t.Errorf("%q: %d %q; want %d with %q", c.args, code, errs, exitUsage, c.msg)
		}
	}
	// A read-only mount the gate is given is checked again.
	if code, _, errs := atl(t, "gate", "fix", "--task-id", "1", "--ro-mount", filepath.Join(os.Getenv("ATL_REPOS"), "api")); code != exitErr ||
		!strings.Contains(errs, "--ro-mount") {
		t.Errorf("--ro-mount of a repo: %d %q", code, errs)
	}
	// A task that is not there: no gate.
	if code, _, errs := atl(t, "gate", "nope", "--task-id", "1"); code != exitErr || !strings.Contains(errs, "task dir") {
		t.Errorf("gate of a missing task: %d %q", code, errs)
	}
}

// TestReservedImages: a task gate reserves the images it started with, the
// workspace's sandbox.image and every project's image, read again on each
// call; a project file it cannot read makes it refuse.
func TestReservedImages(t *testing.T) {
	root := setup(t)
	ws := filepath.Join(root, "ws")
	if err := os.WriteFile(filepath.Join(ws, "atelier.toml"), []byte("[sandbox]\nimage = \"ws-img\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := config.Paths{Workspace: ws, ConfigDir: filepath.Join(root, "cfg")}
	reserved := reservedImages(p, "start-task", "start-box")
	got, err := reserved()
	if err != nil || !slices.Equal(got, []string{"start-task", "start-box", "ws-img"}) {
		t.Fatalf("reserved = %q, %v", got, err)
	}
	pf := p.ProjectFile("shop")
	if err := os.MkdirAll(filepath.Dir(pf), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pf, []byte("image = \"shop-img\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err = reserved(); err != nil || !slices.Contains(got, "shop-img") {
		t.Fatalf("reserved after a project = %q, %v", got, err)
	}
	if err := os.WriteFile(pf, []byte("image = [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err = reserved(); err == nil {
		t.Fatalf("reserved with a broken project file = %q, no error", got)
	}
}
