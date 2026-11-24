package gitx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/nofollow"
)

// TestIsDirtyCases: IsDirty agrees with `git status --porcelain` (run here
// with no filters configured, so it is a fair reference) on the cases it
// decides without reading through filters.
func TestIsDirtyCases(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		change func(t *testing.T, wt string)
		want   bool
	}{
		{"clean", func(t *testing.T, wt string) {}, false},
		{"touched, same content", func(t *testing.T, wt string) { touch(t, filepath.Join(wt, "README.md"), 10) }, false},
		{"rewritten, same content", func(t *testing.T, wt string) {
			p := filepath.Join(wt, "README.md")
			os.Remove(p)
			writeFile(t, p, "hello\n")
		}, false},
		{"same size, other content, same mtime", func(t *testing.T, wt string) {
			p := filepath.Join(wt, "README.md")
			fi := mustStat(t, p)
			writeFile(t, p, "HELLO\n")
			os.Chtimes(p, fi.ModTime(), fi.ModTime())
		}, true},
		{"other size", func(t *testing.T, wt string) { writeFile(t, filepath.Join(wt, "README.md"), "hello, world\n") }, true},
		{"deleted", func(t *testing.T, wt string) { os.Remove(filepath.Join(wt, "README.md")) }, true},
		{"replaced by a directory", func(t *testing.T, wt string) {
			os.Remove(filepath.Join(wt, "README.md"))
			writeFile(t, filepath.Join(wt, "README.md", "x"), "x")
		}, true},
		{"made executable", func(t *testing.T, wt string) { os.Chmod(filepath.Join(wt, "README.md"), 0o755) }, true},
		{"made executable, core.fileMode=false", func(t *testing.T, wt string) {
			git(t, wt, "config", "core.fileMode", "false")
			os.Chmod(filepath.Join(wt, "README.md"), 0o755)
		}, false},
		{"untracked", func(t *testing.T, wt string) { writeFile(t, filepath.Join(wt, "new"), "x") }, true},
		{"untracked, in a new directory", func(t *testing.T, wt string) { writeFile(t, filepath.Join(wt, "d", "e", "new"), "x") }, true},
		{"ignored", func(t *testing.T, wt string) {
			exclude := git(t, wt, "rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
			writeFile(t, exclude, "build/\n*.log\n")
			writeFile(t, filepath.Join(wt, "build", "out"), "x")
			writeFile(t, filepath.Join(wt, "x.log"), "x")
		}, false},
		{"a directory with only ignored files", func(t *testing.T, wt string) {
			exclude := git(t, wt, "rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
			writeFile(t, exclude, "*.log\n")
			writeFile(t, filepath.Join(wt, "d", "y.log"), "x")
		}, false},
		{"empty directory", func(t *testing.T, wt string) { os.MkdirAll(filepath.Join(wt, "empty"), 0o755) }, false},
		{"core.fileMode=no, executable", func(t *testing.T, wt string) {
			git(t, wt, "config", "core.fileMode", "no")
			os.Chmod(filepath.Join(wt, "README.md"), 0o755)
		}, false},
		{"staged", func(t *testing.T, wt string) {
			writeFile(t, filepath.Join(wt, "README.md"), "staged\n")
			git(t, wt, "add", "README.md")
		}, true},
		{"staged, then undone in the worktree", func(t *testing.T, wt string) {
			writeFile(t, filepath.Join(wt, "README.md"), "staged\n")
			git(t, wt, "add", "README.md")
			writeFile(t, filepath.Join(wt, "README.md"), "hello\n")
		}, true},
		{"intent to add", func(t *testing.T, wt string) {
			writeFile(t, filepath.Join(wt, "new"), "x")
			git(t, wt, "add", "-N", "new")
		}, true},
		{"assume-unchanged", func(t *testing.T, wt string) {
			git(t, wt, "update-index", "--assume-unchanged", "README.md")
			writeFile(t, filepath.Join(wt, "README.md"), "hidden\n")
		}, false},
		{"symlink retargeted", func(t *testing.T, wt string) {
			os.Symlink("README.md", filepath.Join(wt, "link"))
			git(t, wt, "add", "link")
			git(t, wt, "commit", "-q", "-m", "link")
			os.Remove(filepath.Join(wt, "link"))
			os.Symlink("elsewhere", filepath.Join(wt, "link"))
		}, true},
		{"symlink touched", func(t *testing.T, wt string) {
			os.Symlink("README.md", filepath.Join(wt, "link"))
			git(t, wt, "add", "link")
			git(t, wt, "commit", "-q", "-m", "link")
			os.Remove(filepath.Join(wt, "link"))
			os.Symlink("README.md", filepath.Join(wt, "link"))
		}, false},
		{"a commit, racily clean", func(t *testing.T, wt string) {
			writeFile(t, filepath.Join(wt, "c"), "c\n")
			git(t, wt, "add", "c")
			git(t, wt, "commit", "-q", "-m", "c")
		}, false},
		{"unmerged", func(t *testing.T, wt string) {
			git(t, wt, "checkout", "-q", "-b", "side")
			writeFile(t, filepath.Join(wt, "README.md"), "side\n")
			git(t, wt, "commit", "-q", "-am", "side")
			git(t, wt, "checkout", "-q", "-")
			writeFile(t, filepath.Join(wt, "README.md"), "here\n")
			git(t, wt, "commit", "-q", "-am", "here")
			cmd := exec.Command("git", "-C", wt, "merge", "-q", "side")
			cmd.Env = testEnv(t)
			cmd.Run() // conflicts
			writeFile(t, filepath.Join(wt, "README.md"), "here\n")
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			wt, _ := f.addWorktree(t, ctx, "d")
			// The index older than the files, so that only the cases
			// that make files racy are.
			idx := git(t, wt, "rev-parse", "--path-format=absolute", "--git-path", "index")
			touch(t, idx, 2)
			c.change(t, wt)
			got, err := IsDirty(ctx, wt)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("IsDirty = %v, want %v", got, c.want)
			}
			// git compares ctime to the second only: a rewrite in place
			// within the second, mtime put back, escapes its status.
			if c.name == "same size, other content, same mtime" {
				return
			}
			if status := git(t, wt, "status", "--porcelain", "--untracked-files=normal"); (status != "") != c.want {
				t.Errorf("git status disagrees with the case: %q", status)
			}
		})
	}
}

// TestIsDirtySubmodule: a submodule at another commit, or with changes of
// its own, makes the worktree dirty; one not checked out does not.
func TestIsDirtySubmodule(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	subsrc := filepath.Join(f.root, "subsrc")
	git(t, f.root, "init", "-q", "-b", "main", subsrc)
	commitFile(t, subsrc, "s.txt", "s\n", "s")
	git(t, f.repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", subsrc, "sub")
	git(t, f.repo, "commit", "-q", "-m", "sub")
	check := func(want bool) {
		t.Helper()
		if got, err := IsDirty(ctx, f.repo); err != nil || got != want {
			t.Fatalf("IsDirty = %v, %v; want %v", got, err, want)
		}
	}
	check(false)
	sub := filepath.Join(f.repo, "sub")
	writeFile(t, filepath.Join(sub, "s.txt"), "changed\n")
	check(true)
	git(t, sub, "checkout", "-q", "s.txt")
	check(false)
	commitFile(t, sub, "t.txt", "t\n", "t")
	check(true)
	git(t, f.repo, "add", "sub")
	git(t, f.repo, "commit", "-q", "-m", "sub moved")
	check(false)
	// Not checked out: an empty directory.
	git(t, f.repo, "submodule", "deinit", "-q", "-f", "sub")
	check(false)
}

// TestIsDirtySHA256: blobs are hashed in the repo's object format.
func TestIsDirtySHA256(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "--object-format=sha256", "-b", "main")
	commitFile(t, dir, "a", "a\n", "a")
	touch(t, filepath.Join(dir, "a"), 10)
	if got, err := IsDirty(ctx, dir); err != nil || got {
		t.Fatalf("IsDirty = %v, %v", got, err)
	}
	writeFile(t, filepath.Join(dir, "a"), "b\n")
	touch(t, filepath.Join(dir, "a"), 10)
	if got, err := IsDirty(ctx, dir); err != nil || !got {
		t.Fatalf("IsDirty after a change = %v, %v", got, err)
	}
}

func TestIsDirtyUnborn(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	if got, err := IsDirty(ctx, dir); err != nil || got {
		t.Fatalf("empty repo: IsDirty = %v, %v", got, err)
	}
	writeFile(t, filepath.Join(dir, "a"), "a\n")
	git(t, dir, "add", "a")
	if got, err := IsDirty(ctx, dir); err != nil || !got {
		t.Fatalf("staged in an unborn branch: IsDirty = %v, %v", got, err)
	}
	if _, err := IsDirty(ctx, filepath.Join(dir, "nope")); err == nil {
		t.Fatal("IsDirty of a missing dir: no error")
	}
}

// TestIsDirtyOpensNoFIFO: a tracked file the task swaps for a FIFO (or a
// symlink, or anything that is not a regular file) between IsDirty's look
// at it and the read of its content is a change; the read neither blocks
// nor follows anything.
func TestIsDirtyOpensNoFIFO(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "sub", "f"), "content\n")
	d, err := nofollow.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	fi, err := d.Lstat("sub/f") // what IsDirty saw: a regular file
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, "sub", "f"))
	if err := syscall.Mkfifo(filepath.Join(dir, "sub", "f"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := blobHash(d, "sub/f", fi, 40); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, nofollow.ErrNotRegular) {
			t.Errorf("hashing a FIFO: %v, want ErrNotRegular", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hashing a FIFO blocked")
	}
	// A directory on the way swapped for a symlink to one with the same file.
	other := t.TempDir()
	writeFile(t, filepath.Join(other, "f"), "content\n")
	os.RemoveAll(filepath.Join(dir, "sub"))
	if err := os.Symlink(other, filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	ix := &gitIndex{}
	e := &indexEntry{mode: modeRegular | 0o644, oid: "d95f3ad14dee633a758d2e331151e950dd13e4ed", path: "sub/f", size: 8}
	if changed, err := ix.changed(e, d, func() bool { return true }); err != nil || !changed {
		t.Errorf("a tracked file reached through a symlinked directory: changed = %v, %v; want a change", changed, err)
	}
}

// touch moves path's mtime by secs seconds.
func touch(t *testing.T, path string, secs int) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	when := fi.ModTime().Add(time.Duration(secs) * time.Second)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}
