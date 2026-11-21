package gitx

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestCheckWorktree: only a real directory that is a worktree of the given
// clone passes.
func TestCheckWorktree(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	wt, _ := f.addWorktree(t, ctx, "x")
	if err := CheckWorktree(ctx, wt, f.repo); err != nil {
		t.Fatalf("a worktree of the clone: %v", err)
	}
	other := newFixture(t)
	otherWT, _ := other.addWorktree(t, ctx, "y")

	path := filepath.Join(t.TempDir(), "api")
	if err := CheckWorktree(ctx, path, f.repo); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: %v, want ErrNotExist", err)
	}
	for name, setup := range map[string]func(){
		"a symlink to the worktree itself": func() { must(t, os.Symlink(wt, path)) },
		"a symlink to another clone":       func() { must(t, os.Symlink(other.repo, path)) },
		"another clone's worktree":         func() { must(t, os.Symlink(otherWT, path)) },
		"a .git file pointing elsewhere": func() {
			must(t, os.Mkdir(path, 0o755))
			writeFile(t, filepath.Join(path, ".git"), "gitdir: "+filepath.Join(other.repo, ".git")+"\n")
		},
		"a plain directory": func() { must(t, os.Mkdir(path, 0o755)) },
		"a subdirectory of the worktree": func() {
			must(t, os.Mkdir(filepath.Join(wt, "sub"), 0o755))
			path = filepath.Join(wt, "sub")
		},
	} {
		saved := path
		setup()
		if err := CheckWorktree(ctx, path, f.repo); err == nil {
			t.Errorf("%s: passed", name)
		}
		os.RemoveAll(path)
		path = saved
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
