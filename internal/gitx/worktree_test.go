package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

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

// TestReplaceRefsAreNotRead: a task writes the clone's git dir, refs/replace
// too. A replace ref for the task's base commit that is a symlink to a
// branch of another repo on the host (whose objects it has) would have
// atl's git on the host show that repo's history and files as the task's
// (atl diff, atl show: what the assistant reads).
func TestReplaceRefsAreNotRead(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	wt, base := f.addWorktree(t, ctx, "x")
	secret := filepath.Join(t.TempDir(), "secret")
	git(t, f.root, "init", "-q", "-b", "main", secret)
	commitFile(t, secret, "creds", "hunter2\n", "the user's private work")
	// Its objects in the clone (alternates are refused: TestHostGitReadsOnlyTheRepo).
	git(t, f.repo, "fetch", "-q", secret, "main")
	commitFile(t, wt, "work.txt", "work\n", "the task's work")
	head := git(t, wt, "rev-parse", "HEAD")
	for _, id := range []string{base, head} {
		replace := filepath.Join(f.repo, ".git", "refs", "replace", id)
		must(t, os.MkdirAll(filepath.Dir(replace), 0o755))
		must(t, os.Symlink(filepath.Join(secret, ".git", "refs", "heads", "main"), replace))
	}
	var out bytes.Buffer
	if err := Diff(ctx, wt, base, false, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "hunter2") {
		t.Errorf("diff shows another repo's file:\n%s", out.String())
	}
	if _, subject, _, err := LastCommit(ctx, wt); err != nil || strings.Contains(subject, "private") {
		t.Errorf("last commit = %q, %v", subject, err)
	}
}

// TestAddWorktreeFailureFollowsNoSwappedDir: a task with the repo rw (it
// writes the clone's git dir) watches worktrees/ while atl adds a worktree
// for another task. In the registration dir that appears it makes the
// checkout fail (index.lock) and plants a directory that it swaps for a
// symlink to the user's files once the clean-up of the failed add deletes
// in it. The add fails, and deletes nothing outside the repo.
func TestAddWorktreeFailureFollowsNoSwappedDir(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	base, err := RevParse(ctx, f.repo, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	victim := t.TempDir()
	var names []string
	for i := range 40 {
		name := fmt.Sprintf("t%02d", i)
		names = append(names, name)
		writeFile(t, filepath.Join(victim, name, "keep"), "mine\n")
	}
	admin := filepath.Join(f.repo, ".git", "worktrees")
	must(t, os.MkdirAll(admin, 0o755))
	trap := filepath.Join(f.repo, ".git", "trap") // built ahead, moved in at once
	for i := range 3000 {
		writeFile(t, filepath.Join(trap, fmt.Sprintf("f%04d", i)), "")
	}
	for _, name := range names {
		writeFile(t, filepath.Join(trap, name, "keep"), "")
	}
	stop := plantOnAdd(t, admin, trap, victim)
	err = AddWorktree(ctx, f.repo, filepath.Join(f.root, "wt-new"), "atl/new", base, to(f.origin))
	stop()
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(victim, name, "keep")); err != nil {
			t.Fatalf("a file outside the repo was deleted: %v", err)
		}
	}
	if err == nil {
		t.Fatal("the add went through a failed checkout")
	}
}

// plantOnAdd watches admin; in the first directory made there it moves
// trap (as "a") and an index.lock; at the first deletion in the trap it
// swaps it for a symlink to target. stop ends the watch.
func plantOnAdd(t *testing.T, admin, trap, target string) (stop func()) {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	wAdmin, err := unix.InotifyAddWatch(fd, admin, unix.IN_CREATE)
	if err != nil {
		t.Fatal(err)
	}
	wTrap, err := unix.InotifyAddWatch(fd, trap, unix.IN_DELETE)
	if err != nil {
		t.Fatal(err)
	}
	done, ended := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		defer unix.Close(fd)
		buf := make([]byte, 64<<10)
		planted := ""
		for {
			select {
			case <-done:
				return
			default:
			}
			if n, _ := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, 10); n <= 0 {
				continue
			}
			n, _ := unix.Read(fd, buf)
			for off := 0; off+unix.SizeofInotifyEvent <= n; {
				ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
				name := strings.TrimRight(string(buf[off+unix.SizeofInotifyEvent:off+unix.SizeofInotifyEvent+int(ev.Len)]), "\x00")
				off += unix.SizeofInotifyEvent + int(ev.Len)
				switch {
				case int(ev.Wd) == wAdmin && ev.Mask&unix.IN_ISDIR != 0 && planted == "":
					planted = filepath.Join(admin, name, "a")
					os.Rename(trap, planted)
					os.WriteFile(filepath.Join(admin, name, "index.lock"), nil, 0o644)
				case int(ev.Wd) == wTrap && planted != "":
					os.Rename(planted, planted+".aside")
					os.Symlink(target, planted)
					return
				}
			}
		}
	}()
	return func() { close(done); <-ended }
}
