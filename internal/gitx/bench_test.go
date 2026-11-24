package gitx

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// benchWorktree returns a clean worktree of a repo (and the repo) with 200
// committed files, as the TUI's dirty check polls it.
func benchWorktree(b *testing.B) (wt, repo string) {
	b.Helper()
	root := b.TempDir()
	repo, wt = filepath.Join(root, "repo"), filepath.Join(root, "wt")
	gitB := func(dir string, args ...string) {
		b.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			b.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gitB(root, "init", "-q", "-b", "main", repo)
	for i := range 200 {
		p := filepath.Join(repo, fmt.Sprintf("d%d", i%10), fmt.Sprintf("f%d.go", i))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(fmt.Sprintf("package d\n\n// %d\n", i)), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	gitB(repo, "add", ".")
	gitB(repo, "commit", "-q", "-m", "files")
	gitB(repo, "worktree", "add", "-q", "-b", "atl/bench", wt)
	// Files older than the index, as in a worktree at rest: not racily clean,
	// so IsDirty decides on stat data alone.
	old := time.Now().Add(-time.Hour)
	if err := filepath.WalkDir(wt, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			err = os.Chtimes(p, old, old)
		}
		return err
	}); err != nil {
		b.Fatal(err)
	}
	gitB(wt, "update-index", "-q", "--refresh")
	return wt, repo
}

// BenchmarkDirtyPoll is what the TUI's dirty check runs per rw repo of an
// open task: CheckWorktree, then IsDirty, on a clean worktree.
func BenchmarkDirtyPoll(b *testing.B) {
	ctx := context.Background()
	wt, repo := benchWorktree(b)
	b.ResetTimer()
	for range b.N {
		if err := CheckWorktree(ctx, wt, repo); err != nil {
			b.Fatal(err)
		}
		if dirty, err := IsDirty(ctx, wt); err != nil || dirty {
			b.Fatalf("IsDirty = %v, %v", dirty, err)
		}
	}
}
