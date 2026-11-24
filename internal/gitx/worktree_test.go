package gitx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
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

// TestWorktreeOddPaths: a path is git's output as it is, whitespace and
// newlines at its end included (the clone's own path too).
func TestWorktreeOddPaths(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"trailing space ", "trailing newline\n", " leading space", "new\nline"} {
		f := newFixture(t)
		repo := filepath.Join(f.root, "clone "+name)
		git(t, f.root, "clone", "-q", f.origin, repo)
		f.repo = repo
		base, err := RevParse(ctx, repo, "origin/main")
		if err != nil {
			t.Fatal(err)
		}
		wt := filepath.Join(f.root, "wt "+name)
		if err := AddWorktree(ctx, repo, wt, "atl/x", base, to(f.origin)); err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		if !IsRepo(repo) || !IsRepo(wt) {
			t.Errorf("%q: IsRepo(clone) = %v, IsRepo(worktree) = %v", name, IsRepo(repo), IsRepo(wt))
		}
		if err := CheckWorktree(ctx, wt, repo); err != nil {
			t.Errorf("%q: %v", name, err)
		}
		if err := RemoveWorktree(ctx, repo, wt, false); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
}

// TestRemoveWorktreeFollowsNoSwappedDir: every task with a repo rw can write
// the clone's git dir, worktrees/ included. git deletes a directory by path,
// one lstat and then one opendir per entry, so a task that swaps a directory
// there for a symlink as soon as the deletion starts has git delete, on the
// host, what the symlink points to (by names it chose: a home directory's).
// RemoveWorktree must delete nothing outside the worktree and its own
// registration: not through a registration of its own the task rigged, and
// not through a stale one it planted (which `git worktree prune` would
// delete).
func TestRemoveWorktreeFollowsNoSwappedDir(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	victim := t.TempDir() // the user's files, which no task can reach
	var names []string
	for i := range 40 {
		name := fmt.Sprintf("t%02d", i)
		names = append(names, name)
		writeFile(t, filepath.Join(victim, name, "keep"), "mine\n")
	}
	intact := func(what string) {
		t.Helper()
		for _, name := range names {
			if _, err := os.Stat(filepath.Join(victim, name, "keep")); err != nil {
				t.Fatalf("%s: a file outside the repo was deleted: %v", what, err)
			}
		}
	}
	admin := filepath.Join(f.repo, ".git", "worktrees")

	// A stale registration (its worktree is gone), rigged.
	wt, _ := f.addWorktree(t, ctx, "x")
	planted := filepath.Join(admin, "planted")
	writeFile(t, filepath.Join(planted, "gitdir"), filepath.Join(f.root, "gone", ".git")+"\n")
	stop := swapOnDelete(t, filepath.Join(planted, "a"), victim, names)
	err := RemoveWorktree(ctx, f.repo, wt, false)
	stop()
	intact("a stale registration")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}

	// The worktree's own registration, rigged.
	wt, _ = f.addWorktree(t, ctx, "y")
	id := filepath.Base(wt)
	stop = swapOnDelete(t, filepath.Join(admin, id, "a"), victim, names)
	RemoveWorktree(ctx, f.repo, wt, true) // may fail on the swapped dir; nothing else may go
	stop()
	intact("the worktree's own registration")
}

// swapOnDelete fills dir with many files and with directories named names,
// then, at the first deletion in it, moves it aside and puts a symlink to
// target in its place, as a task would from its container. stop ends the
// watch.
func swapOnDelete(t *testing.T, dir, target string, names []string) (stop func()) {
	t.Helper()
	must(t, os.MkdirAll(dir, 0o755))
	for i := range 3000 {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("f%04d", i)), "")
	}
	for _, name := range names {
		writeFile(t, filepath.Join(dir, name, "keep"), "")
	}
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unix.InotifyAddWatch(fd, dir, unix.IN_DELETE); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		defer unix.Close(fd)
		buf := make([]byte, 4096)
		for {
			select {
			case <-done:
				return
			default:
			}
			if n, _ := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, 10); n <= 0 {
				continue
			}
			if n, _ := unix.Read(fd, buf); n > 0 {
				os.Rename(dir, dir+".aside")
				os.Symlink(target, dir)
				return
			}
		}
	}()
	return func() { close(done); <-ended }
}
