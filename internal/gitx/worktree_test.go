package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
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
// branch of another repo on the host (whose objects it lists as
// alternates) would have atl's git on the host show that repo's history
// and files as the task's (atl diff, atl show: what the assistant reads).
func TestReplaceRefsAreNotRead(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	wt, base := f.addWorktree(t, ctx, "x")
	secret := filepath.Join(t.TempDir(), "secret")
	git(t, f.root, "init", "-q", "-b", "main", secret)
	commitFile(t, secret, "creds", "hunter2\n", "the user's private work")
	writeFile(t, filepath.Join(f.repo, ".git", "objects", "info", "alternates"), filepath.Join(secret, ".git", "objects")+"\n")
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
// trap (as "a") and an index.lock and a directory where the index goes
// (the add fails); at the first deletion in the trap it
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
					os.MkdirAll(filepath.Join(admin, name, "index", "x"), 0o755) // the index cannot be written
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

// onRegister runs f with the path of the first directory made in admin
// (the registration of a worktree being added), as a neighbouring task
// watching the shared git dir would. stop ends the watch.
func onRegister(t *testing.T, admin string, f func(dir string)) (stop func()) {
	t.Helper()
	must(t, os.MkdirAll(admin, 0o755))
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unix.InotifyAddWatch(fd, admin, unix.IN_CREATE); err != nil {
		t.Fatal(err)
	}
	done, ended := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		defer unix.Close(fd)
		buf := make([]byte, 64<<10)
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
				if ev.Mask&unix.IN_ISDIR != 0 {
					f(filepath.Join(admin, name))
					return
				}
			}
		}
	}()
	return func() { close(done); <-ended }
}

// TestAddWorktreeRunsNoFilterSetMeanwhile: the checks of the repo's config
// come before the checkout. A neighbouring task that names a filter for
// every file (info/attributes) and defines it once the new worktree's
// registration appears must not have it run where atl runs.
func TestAddWorktreeRunsNoFilterSetMeanwhile(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	base, err := RevParse(ctx, f.repo, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	writeFile(t, filepath.Join(f.repo, ".git", "info", "attributes"), "* filter=pwn\n")
	git(t, f.repo, "config", "extensions.worktreeConfig", "true")
	stop := onRegister(t, filepath.Join(f.repo, ".git", "worktrees"), func(dir string) {
		cfg, err := os.OpenFile(filepath.Join(f.repo, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
		if err == nil {
			fmt.Fprintf(cfg, "[filter \"pwn\"]\n\tsmudge = touch %s; cat\n\tprocess = touch %s\n", marker, marker)
			cfg.Close()
		}
		os.WriteFile(filepath.Join(dir, "config.worktree"), []byte("[filter \"pwn\"]\n\tsmudge = touch "+marker+"; cat\n"), 0o644)
	})
	wt := filepath.Join(f.root, "wt-x")
	err = AddWorktree(ctx, f.repo, wt, "atl/x", base, to(f.origin))
	stop()
	if _, serr := os.Stat(marker); serr == nil {
		t.Fatalf("the filter the task set ran (add: %v)", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(wt, "README.md")); err != nil || string(b) != "hello\n" {
		t.Errorf("README.md = %q, %v", b, err)
	}
}

// TestAddWorktreeIntoAnEmptyDir: an add into an empty directory that
// fails leaves the directory, empty, as git does.
func TestAddWorktreeIntoAnEmptyDir(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	base, err := RevParse(ctx, f.repo, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(f.root, "wt-empty")
	must(t, os.Mkdir(wt, 0o755))
	stop := onRegister(t, filepath.Join(f.repo, ".git", "worktrees"), func(dir string) {
		os.MkdirAll(filepath.Join(dir, "index", "x"), 0o755) // the index cannot be written
	})
	err = AddWorktree(ctx, f.repo, wt, "atl/x", base, to(f.origin))
	stop()
	if err == nil {
		t.Fatal("the add succeeded")
	}
	entries, rerr := os.ReadDir(wt)
	if rerr != nil || len(entries) != 0 {
		t.Errorf("after the failed add: %v, %v; want the dir, empty", entries, rerr)
	}
	// And an add that succeeds afterwards.
	must(t, os.RemoveAll(filepath.Join(f.repo, ".git", "worktrees")))
	git(t, f.repo, "branch", "-D", "atl/x")
	if err := AddWorktree(ctx, f.repo, wt, "atl/x", base, to(f.origin)); err != nil {
		t.Fatal(err)
	}
}

// TestAddBranchWorktreeRefusesABranchInUse: as git does, a branch that
// another worktree is rebasing or bisecting is refused too.
func TestAddBranchWorktreeRefusesABranchInUse(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	git(t, f.repo, "branch", "atl/r")
	git(t, f.repo, "branch", "atl/b")
	git(t, f.repo, "branch", "atl/free")
	other, _ := f.addWorktree(t, ctx, "other")
	otherGit := git(t, other, "rev-parse", "--absolute-git-dir")
	writeFile(t, filepath.Join(f.repo, ".git", "rebase-merge", "head-name"), "refs/heads/atl/r\n")
	writeFile(t, filepath.Join(otherGit, "BISECT_START"), "atl/b\n")
	for _, b := range []string{"atl/r", "atl/b", "atl/other", "main"} {
		if err := AddBranchWorktree(ctx, f.repo, filepath.Join(f.root, "wt-"+filepath.Base(b)), b, to(f.origin)); err == nil {
			t.Errorf("%s, in use: added", b)
		}
	}
	if err := AddBranchWorktree(ctx, f.repo, filepath.Join(f.root, "wt-free"), "atl/free", to(f.origin)); err != nil {
		t.Errorf("a free branch: %v", err)
	}
}

// TestWorktreesInAReftableRepo: a clone whose refs are in reftable (git
// 3's default) gets working worktrees, and its branches push.
func TestWorktreesInAReftableRepo(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := exec.Command("git", "init", "-q", "--ref-format=reftable", t.TempDir()).CombinedOutput(); err != nil {
		t.Skip("git without reftable")
	}
	repo := filepath.Join(f.root, "rt")
	git(t, f.root, "clone", "-q", "--ref-format=reftable", f.origin, repo)
	base, err := RevParse(ctx, repo, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(f.root, "wt-rt")
	if err := AddWorktree(ctx, repo, wt, "atl/rt", base, to(f.origin)); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(wt, "README.md")); err != nil || string(b) != "hello\n" {
		t.Fatalf("README.md = %q, %v", b, err)
	}
	if br := git(t, wt, "branch", "--show-current"); br != "atl/rt" {
		t.Errorf("branch %q", br)
	}
	if st := git(t, wt, "status", "--porcelain"); st != "" {
		t.Errorf("status:\n%s", st)
	}
	commitFile(t, wt, "w.txt", "w\n", "work")
	if err := PushBranch(ctx, repo, to(f.origin), "atl/rt", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, want := git(t, f.origin, "rev-parse", "atl/rt"), git(t, wt, "rev-parse", "HEAD"); got != want {
		t.Errorf("origin atl/rt = %s, want %s", got, want)
	}
	if err := RemoveWorktree(ctx, repo, wt, false); err != nil {
		t.Fatal(err)
	}
	if err := AddBranchWorktree(ctx, repo, wt, "atl/rt", to(f.origin)); err != nil {
		t.Fatal(err)
	}
	if st := git(t, wt, "status", "--porcelain"); st != "" {
		t.Errorf("status after re-add:\n%s", st)
	}
}
