package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/store"
	"github.com/andreujuanc/atelier/internal/tmux"
)

// killPoint says where crashRun's child atl dies: when it runs Cmd (git or
// tmux) with arguments ("$*") matching the shell case pattern Args, the
// command's wrapper SIGKILLs it, before it runs the real command or, if
// After, after; with Nth, only at the Nth such call (the ones before run).
type killPoint struct {
	Cmd, Args string
	After     bool
	Nth       int
}

// newWindowCall matches the tmux call that makes a session's window.
const newWindowCall = `*" new-window "*|*" new-session "*`

// crashRun runs op (see TestCrashHelper) in a child process on a's paths,
// tmux socket and atl binary, and SIGKILLs that process at kill. It fails
// unless the child died of SIGKILL: the kill point was reached.
func crashRun(t *testing.T, a *App, kill killPoint, op ...string) {
	t.Helper()
	real, err := exec.LookPath(kill.Cmd)
	if err != nil {
		t.Skipf("%s not installed", kill.Cmd)
	}
	bin := t.TempDir()
	after := ""
	if kill.After {
		after = fmt.Sprintf("%q \"$@\"; ", real)
	}
	count := ""
	if kill.Nth > 1 {
		c := filepath.Join(bin, "count")
		count = fmt.Sprintf(`n=$(($(cat %[1]q 2>/dev/null || echo 0) + 1)); echo $n > %[1]q; [ $n -lt %[2]d ] && exec %[3]q "$@"; `, c, kill.Nth, real)
	}
	wrapper := fmt.Sprintf(`#!/bin/sh
case "$*" in
%s) %s%skill -9 $PPID; exit 1;;
esac
exec %q "$@"
`, kill.Args, count, after, real)
	if err := os.WriteFile(filepath.Join(bin, kill.Cmd), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"CRASHTEST_ROOT="+filepath.Dir(a.Paths.Repos),
		"CRASHTEST_SOCK="+a.Tmux.Socket,
		"CRASHTEST_ATLBIN="+a.AtlBin,
		"CRASHTEST_OP="+strings.Join(op, "\n"))
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("child %v was not killed at %+v: %v\n%s", op, kill, err, out)
	}
}

// TestCrashHelper is crashRun's child, not a test: it runs CRASHTEST_OP
// (newline-separated: an operation and its arguments) on the App crashRun
// describes, and should be killed on the way.
func TestCrashHelper(t *testing.T) {
	opv := os.Getenv("CRASHTEST_OP")
	if opv == "" {
		t.Skip("crashRun's child only")
	}
	root := os.Getenv("CRASHTEST_ROOT")
	paths := config.Paths{
		Repos:     filepath.Join(root, "repos"),
		Tasks:     filepath.Join(root, "tasks"),
		DB:        filepath.Join(root, "data", "atelier.db"),
		StateDir:  filepath.Join(root, "state"),
		ConfigDir: filepath.Join(root, "config"),
	}
	st, err := store.Open(paths.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := &App{Paths: paths, Store: st, Tmux: &tmux.Client{Socket: os.Getenv("CRASHTEST_SOCK")}, Out: os.Stderr,
		Now: time.Now, AtlBin: os.Getenv("CRASHTEST_ATLBIN"), CheckBin: anyBin}
	ctx := context.Background()
	op := strings.Split(opv, "\n")
	switch op[0] {
	case "new-session":
		_, err = a.NewSession(ctx, op[1])
	case "resume":
		_, err = a.Resume(ctx, op[1])
	case "new": // title repo...
		var repos []RepoArg
		for _, r := range op[2:] {
			repos = append(repos, RepoArg{Name: r, Mode: model.ModeRW})
		}
		_, err = a.NewTask(ctx, NewTaskOpts{Title: op[1], Repos: repos, NoFetch: true})
	case "add": // task repo
		_, err = a.AddRepo(ctx, op[1], RepoArg{Name: op[2], Mode: model.ModeRW}, true)
	case "done":
		_, err = a.Done(ctx, op[1], false)
	case "reopen":
		err = a.SetStatus(ctx, op[1], model.StatusActive)
	default:
		t.Fatalf("unknown op %q", op[0])
	}
	t.Fatalf("%v returned (%v) instead of being killed", op, err)
}

// TestRecoverAfterKilledResume: atl killed while it resumes a session (here
// at the tmux call that makes its window, before and after it) leaves the
// session claimed (idle, no window recorded). Recover must make it stopped
// again once no launch is under way, so that it can be resumed: otherwise it
// stays idle, which neither Resume nor the open picker ever takes.
func TestRecoverAfterKilledResume(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprintf("after=%v", after), func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			e.task(t, "t", model.StatusActive, true)
			s, err := e.app.NewSession(ctx, "t")
			if err != nil {
				t.Fatal(err)
			}
			if err := e.app.Store.SetClaudeSession(ctx, s.ID, "cid-1", time.Now()); err != nil {
				t.Fatal(err)
			}
			e.argv(t, s.ID)
			e.tmux(t, "kill-window", "-t", s.TmuxTarget)
			if n, err := e.app.Recover(ctx); err != nil || n != 1 {
				t.Fatalf("Recover = %d, %v", n, err)
			}

			crashRun(t, e.app, killPoint{Cmd: "tmux", Args: newWindowCall, After: after}, "resume", "t")
			if got := e.session(t, s.ID); got.State != model.StateIdle || got.TmuxTarget != "" {
				t.Fatalf("after the kill: %+v", got)
			}
			if after {
				// The window the killed atl made runs on: the session
				// is live, and stays so while it does.
				if _, err := e.app.Recover(ctx); err != nil {
					t.Fatal(err)
				}
				if got := e.session(t, s.ID); got.State != model.StateIdle {
					t.Fatalf("live window's session: %s", got.State)
				}
				e.tmux(t, "kill-window", "-t", "claude-"+strconv.FormatInt(s.ID, 10))
			}
			// A launch under way (the launch lock held) is left alone.
			unlock, err := e.app.launchLock(ctx, s.TaskID, syscall.LOCK_SH)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.app.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if got := e.session(t, s.ID); got.State != model.StateIdle {
				t.Fatalf("Recover during a launch: %s", got.State)
			}
			unlock()

			if n, err := e.app.Recover(ctx); err != nil || n != 1 {
				t.Fatalf("Recover = %d, %v", n, err)
			}
			if got := e.session(t, s.ID); got.State != model.StateStopped || got.ClaudeSessionID != "cid-1" {
				t.Fatalf("after Recover: %+v", got)
			}
			// The first launch's argv: e.argv would read it, or the resumed
			// stub's file while it is being written.
			os.Remove(filepath.Join(e.out, "argv-"+strconv.FormatInt(s.ID, 10)))
			if n, err := e.app.Resume(ctx, "t"); err != nil || n != 1 {
				t.Fatalf("Resume = %d, %v", n, err)
			}
			if argv := e.argv(t, s.ID); !strings.Contains(strings.Join(argv, " "), "--resume=cid-1") {
				t.Fatalf("argv %q", argv)
			}
		})
	}
}

// TestRecoverAfterKilledNewSession: atl killed while it starts a new session
// leaves a live row with no window recorded. Recover makes it stopped once no
// launch is under way and its window (if the killed atl made one) is gone;
// a row for a Claude atl did not launch (the hooks made it) is left alone.
func TestRecoverAfterKilledNewSession(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprintf("after=%v", after), func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			task := e.task(t, "t", model.StatusActive, true)
			foreign := &model.Session{TaskID: task.ID, ClaudeSessionID: "by-hand", State: model.StateIdle}
			if err := e.app.Store.CreateSession(ctx, foreign); err != nil {
				t.Fatal(err)
			}
			crashRun(t, e.app, killPoint{Cmd: "tmux", Args: newWindowCall, After: after}, "new-session", "t")
			sessions, err := e.app.Store.ListSessions(ctx, task.ID)
			if err != nil || len(sessions) != 2 {
				t.Fatalf("sessions %+v, %v", sessions, err)
			}
			s := sessions[1]
			if s.State != model.StateIdle || s.TmuxTarget != "" {
				t.Fatalf("after the kill: %+v", s)
			}
			if after {
				e.argv(t, s.ID)
				if _, err := e.app.Recover(ctx); err != nil {
					t.Fatal(err)
				}
				if got := e.session(t, s.ID); got.State != model.StateIdle {
					t.Fatalf("live window's session: %s", got.State)
				}
				e.tmux(t, "kill-window", "-t", "claude-"+strconv.FormatInt(s.ID, 10))
			}
			if n, err := e.app.Recover(ctx); err != nil || n != 1 {
				t.Fatalf("Recover = %d, %v", n, err)
			}
			if got := e.session(t, s.ID); got.State != model.StateStopped {
				t.Fatalf("after Recover: %+v", got)
			}
			if got := e.session(t, foreign.ID); got.State != model.StateIdle {
				t.Fatalf("hook row: %+v", got)
			}
		})
	}
}

// atl adds a worktree itself (gitx.AddWorktree). worktreeAddCall matches
// the git call in rw repo name's clone right before it makes anything of
// the worktree (the new branch, or the check that the branch is free);
// worktreeCheckoutCall the checkout that writes its files (after it, only
// the index's stat data and the registration's lock are left to write).
func worktreeAddCall(name string) string {
	return `*"/repos/` + name + ` "*" branch --quiet "*|*"/repos/` + name + ` worktree list "*`
}
func worktreeCheckoutCall(name string) string { return `*"/` + name + ` "*" checkout-index "*` }

// atl removes a worktree itself (gitx.RemoveWorktree), after git calls in
// it. worktreeCheckCall matches the check of rw repo name's worktree (the
// 2nd one of atl done is right before its removal), worktreeLastCall the
// call that ends its dirty check (the 3rd is the last before its removal),
// branchCheckCall the first of the branch clean-up in repo name (once
// every worktree is removed).
func worktreeCheckCall(name string) string {
	return `*"/` + name + ` rev-parse --path-format=absolute "*`
}
func worktreeLastCall(name string) string { return `*"/` + name + ` rev-parse --git-path "*` }
func branchCheckCall(name string) string  { return `*"/repos/` + name + ` show-ref "*` }

// assertClosedClean fails unless task slug is closed and nothing of it is
// left on disk: no task dir, no worktree or branch in the clones of repos.
func assertClosedClean(t *testing.T, e *taskEnv, slug string, repos ...string) {
	t.Helper()
	tk, err := e.app.Store.GetTaskBySlug(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Status.Open() {
		t.Errorf("task %s is %s", slug, tk.Status)
	}
	if exists(e.app.Paths.TaskDir(slug)) {
		t.Errorf("task dir of %s left", slug)
	}
	for _, r := range repos {
		if n := worktreeCount(t, e.canonical(r)); n != 1 {
			t.Errorf("%s has %d worktrees", r, n)
		}
		if branchExists(e.canonical(r), "atl/"+slug) {
			t.Errorf("branch atl/%s left in %s", slug, r)
		}
	}
}

// assertOpenWhole fails unless task slug is open with a worktree of each of
// repos on its branch, and its generated files.
func assertOpenWhole(t *testing.T, e *taskEnv, slug string, repos ...string) {
	t.Helper()
	tk, err := e.app.Store.GetTaskBySlug(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	if !tk.Status.Open() || len(tk.Repos) != len(repos) {
		t.Fatalf("task %s: %s, repos %+v", slug, tk.Status, tk.Repos)
	}
	for _, r := range repos {
		if b := git(t, e.app.Paths.WorktreePath(slug, r), "branch", "--show-current"); b != "atl/"+slug {
			t.Errorf("%s worktree on %q", r, b)
		}
	}
	for _, f := range []string{"CLAUDE.md", ".claude/settings.json"} {
		if !exists(filepath.Join(e.app.Paths.TaskDir(slug), f)) {
			t.Errorf("%s missing", f)
		}
	}
}

// TestKilledNewTask: atl new killed before or after it adds a worktree
// leaves the task, open, with what it had made. Cancelling it removes all
// of that, and the title can be used again.
func TestKilledNewTask(t *testing.T) {
	for _, kill := range []killPoint{
		{Cmd: "git", Args: worktreeAddCall("api")},
		{Cmd: "git", Args: worktreeAddCall("web")},
		{Cmd: "git", Args: worktreeCheckoutCall("web"), After: true},
	} {
		t.Run(fmt.Sprintf("%+v", kill), func(t *testing.T) {
			e := newTaskEnv(t)
			ctx := context.Background()
			e.addRepo(t, "api", "main")
			e.addRepo(t, "web", "main")
			crashRun(t, e.app, kill, "new", "x", "api", "web")
			if _, err := e.app.Cancel(ctx, "x", false); err != nil {
				t.Fatal(err)
			}
			assertClosedClean(t, e, "x", "api", "web")
			tk, err := e.app.NewTask(ctx, NewTaskOpts{Title: "x", NoFetch: true,
				Repos: []RepoArg{{"api", model.ModeRW}, {"web", model.ModeRW}}})
			if err != nil {
				t.Fatal(err)
			}
			assertOpenWhole(t, e, tk.Slug, "api", "web")
		})
	}
}

// TestKilledAddRepo: atl add killed before or after it adds the worktree,
// before it records the repo. Adding the repo again works (through AddRepo,
// or AddRepos as atl add and the TUI do): the worktree and branch the
// killed add left are removed first, as its rollback would have, unless
// they hold work, which is refused and kept.
func TestKilledAddRepo(t *testing.T) {
	for i, kill := range []killPoint{
		{Cmd: "git", Args: worktreeAddCall("web")},
		{Cmd: "git", Args: worktreeCheckoutCall("web"), After: true},
		{Cmd: "git", Args: worktreeCheckoutCall("web"), After: true},
	} {
		t.Run(fmt.Sprintf("%d %+v", i, kill), func(t *testing.T) {
			e := newTaskEnv(t)
			ctx := context.Background()
			e.addRepo(t, "api", "main")
			e.addRepo(t, "web", "main")
			if _, err := e.app.NewTask(ctx, NewTaskOpts{Title: "x", NoFetch: true, Repos: []RepoArg{{"api", model.ModeRW}}}); err != nil {
				t.Fatal(err)
			}
			crashRun(t, e.app, kill, "add", "x", "web")
			if i == 2 {
				// Work in what the killed add left: refused, kept.
				wt := e.app.Paths.WorktreePath("x", "web")
				commitIn(t, wt, "w.txt", "w\n")
				if _, err := e.app.AddRepo(ctx, "x", RepoArg{Name: "web", Mode: model.ModeRW}, true); err == nil ||
					!strings.Contains(err.Error(), "already exists") {
					t.Fatalf("add over work: %v", err)
				}
				if !exists(filepath.Join(wt, "w.txt")) || !branchExists(e.canonical("web"), "atl/x") {
					t.Fatal("work removed")
				}
				return
			}
			res, err := e.app.AddRepos(ctx, "x", []RepoArg{{Name: "web", Mode: model.ModeRW}}, AddOpts{NoFetch: true})
			if err != nil || len(res.Added) != 1 {
				t.Fatalf("add again: %+v, %v", res, err)
			}
			assertOpenWhole(t, e, "x", "api", "web")
			if _, err := e.app.Done(ctx, "x", false); err != nil {
				t.Fatal(err)
			}
			assertClosedClean(t, e, "x", "api", "web")
		})
	}
}

// TestKilledDone: atl done killed while it removes the worktrees leaves the
// task open with some of them gone. atl done again closes it.
func TestKilledDone(t *testing.T) {
	for _, c := range []struct {
		kill     killPoint
		api, web bool // the worktrees left
	}{
		{killPoint{Cmd: "git", Args: worktreeLastCall("api"), After: true, Nth: 3}, true, true},
		{killPoint{Cmd: "git", Args: worktreeCheckCall("web"), Nth: 2}, false, true},
		{killPoint{Cmd: "git", Args: branchCheckCall("api")}, false, false},
	} {
		kill := c.kill
		t.Run(fmt.Sprintf("%+v", kill), func(t *testing.T) {
			e := newTaskEnv(t)
			ctx := context.Background()
			e.addRepo(t, "api", "main")
			e.addRepo(t, "web", "main")
			if _, err := e.app.NewTask(ctx, NewTaskOpts{Title: "x", NoFetch: true,
				Repos: []RepoArg{{"api", model.ModeRW}, {"web", model.ModeRW}}}); err != nil {
				t.Fatal(err)
			}
			crashRun(t, e.app, kill, "done", "x")
			if api, web := exists(e.app.Paths.WorktreePath("x", "api")), exists(e.app.Paths.WorktreePath("x", "web")); api != c.api || web != c.web {
				t.Fatalf("killed: worktrees left api %v web %v, want %v %v", api, web, c.api, c.web)
			}
			if _, err := e.app.Done(ctx, "x", false); err != nil {
				t.Fatalf("done again: %v", err)
			}
			assertClosedClean(t, e, "x", "api", "web")
		})
	}
}

// TestKilledReopen: reopening killed while it adds the worktrees leaves the
// task closed with some of them there. Reopening again makes the rest.
func TestKilledReopen(t *testing.T) {
	for _, kill := range []killPoint{
		{Cmd: "git", Args: worktreeAddCall("api")},
		{Cmd: "git", Args: worktreeAddCall("web")},
		{Cmd: "git", Args: worktreeCheckoutCall("web"), After: true},
	} {
		t.Run(fmt.Sprintf("%+v", kill), func(t *testing.T) {
			e := newTaskEnv(t)
			ctx := context.Background()
			e.addRepo(t, "api", "main")
			e.addRepo(t, "web", "main")
			if _, err := e.app.NewTask(ctx, NewTaskOpts{Title: "x", NoFetch: true,
				Repos: []RepoArg{{"api", model.ModeRW}, {"web", model.ModeRW}}}); err != nil {
				t.Fatal(err)
			}
			commitIn(t, e.app.Paths.WorktreePath("x", "api"), "a.txt", "a\n") // kept branch in api
			if _, err := e.app.Done(ctx, "x", false); err != nil {
				t.Fatal(err)
			}
			crashRun(t, e.app, kill, "reopen", "x")
			if err := e.app.SetStatus(ctx, "x", model.StatusActive); err != nil {
				t.Fatalf("reopen again: %v", err)
			}
			assertOpenWhole(t, e, "x", "api", "web")
			if !exists(filepath.Join(e.app.Paths.WorktreePath("x", "api"), "a.txt")) {
				t.Error("api's commit lost")
			}
		})
	}
}

// TestDoneLockedWorktree: a git killed while it added a task's worktree
// leaves the worktree locked ("initializing"); done removes it, with its
// dir there or gone, and its branch, and the task reopens.
func TestDoneLockedWorktree(t *testing.T) {
	for _, gone := range []bool{false, true} {
		t.Run(fmt.Sprintf("gone=%v", gone), func(t *testing.T) {
			e := newTaskEnv(t)
			ctx := context.Background()
			e.addRepo(t, "api", "main")
			if _, err := e.app.NewTask(ctx, NewTaskOpts{Title: "x", NoFetch: true, Repos: []RepoArg{{"api", model.ModeRW}}}); err != nil {
				t.Fatal(err)
			}
			wt := e.app.Paths.WorktreePath("x", "api")
			git(t, e.canonical("api"), "worktree", "lock", "--reason", "initializing", wt)
			if gone {
				if err := os.RemoveAll(wt); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := e.app.Done(ctx, "x", false); err != nil {
				t.Fatal(err)
			}
			assertClosedClean(t, e, "x", "api")
			if err := e.app.SetStatus(ctx, "x", model.StatusActive); err != nil {
				t.Fatalf("reopen: %v", err)
			}
			assertOpenWhole(t, e, "x", "api")
		})
	}
}
