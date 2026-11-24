package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/sandbox"
)

// reopened checks that task slug is open as st and has what a new task has:
// its task dir with CLAUDE.md and the settings, and for rw repo api a
// worktree of the canonical clone on the task branch at head. It returns
// the task as stored.
func reopened(t *testing.T, e *taskEnv, slug string, st model.Status, head string) *model.Task {
	t.Helper()
	ctx := context.Background()
	task, err := e.app.Store.GetTaskBySlug(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != st {
		t.Fatalf("status %s, want %s", task.Status, st)
	}
	dir := e.app.Paths.TaskDir(slug)
	for _, f := range []string{"CLAUDE.md", filepath.Join(".claude", "settings.json")} {
		if !exists(filepath.Join(dir, f)) {
			t.Errorf("%s missing after reopen", f)
		}
	}
	wt := e.app.Paths.WorktreePath(slug, "api")
	if b := git(t, wt, "symbolic-ref", "--short", "HEAD"); b != "atl/"+slug {
		t.Fatalf("worktree on %s", b)
	}
	if got := git(t, wt, "rev-parse", "HEAD"); got != head {
		t.Fatalf("worktree HEAD %s, want %s", got, head)
	}
	v, err := e.app.Get(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}
	for _, rs := range v.RepoStatus {
		if rs.Err != "" || rs.Dirty {
			t.Errorf("repo %+v after reopen", rs)
		}
	}
	// Sessions can start in it again.
	if _, err := e.app.checkLaunchable(task); err != nil {
		t.Fatal(err)
	}
	return task
}

// done → active: the task dir, the worktree on the branch done kept (with
// its unpushed commit), the generated files and the project's setup are back.
func TestReopenKeptBranch(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "docs", "main")
	log := filepath.Join(e.root, "setup.log")
	pf := e.app.Paths.ProjectFile("shop")
	if err := os.MkdirAll(filepath.Dir(pf), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pf, []byte("setup = 'echo \"$ATL_TASK\" >> "+log+"'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "again", Project: "shop",
		Repos: []RepoArg{{"api", model.ModeRW}, {"docs", model.ModeRO}}})
	if err != nil {
		t.Fatal(err)
	}
	sha := e.commitIn(t, task.Slug, "api", "work")
	s := &model.Session{TaskID: task.ID, State: model.StateIdle}
	if err := e.app.Store.CreateSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.Done(ctx, task.Slug, false); err != nil {
		t.Fatal(err)
	}
	if exists(e.app.Paths.TaskDir(task.Slug)) {
		t.Fatal("done left the task dir")
	}
	// The bug: an open status on a closed task left it without a task dir.
	if err := e.app.SetStatus(ctx, task.Slug, model.StatusActive); err != nil {
		t.Fatal(err)
	}
	got := reopened(t, e, task.Slug, model.StatusActive, sha)
	if got.Repos[0].BaseCommit != task.Repos[0].BaseCommit || got.Repos[0].BaseBranch != "main" {
		t.Errorf("base changed: %+v, was %+v", got.Repos[0], task.Repos[0])
	}
	if exists(filepath.Join(e.app.Paths.TaskDir(task.Slug), "docs")) {
		t.Error("ro repo got a worktree")
	}
	md, _ := os.ReadFile(filepath.Join(e.app.Paths.TaskDir(task.Slug), "CLAUDE.md"))
	if !strings.Contains(string(md), e.canonical("docs")) {
		t.Errorf("CLAUDE.md does not list docs:\n%s", md)
	}
	if b, _ := os.ReadFile(log); string(b) != "again\nagain\n" {
		t.Errorf("setup log %q: setup did not run again", b)
	}
	if n := worktreeCount(t, e.canonical("api")); n != 2 {
		t.Errorf("%d worktrees of api", n)
	}
	// The old session is not resumed or touched.
	if old, _ := e.app.Store.GetSession(ctx, s.ID); old.State != model.StateEnded {
		t.Errorf("old session %s", old.State)
	}

	// And it closes again as usual.
	if _, err := e.app.Done(ctx, task.Slug, false); err != nil {
		t.Fatal(err)
	}
	git(t, e.canonical("api"), "rev-parse", "--verify", "refs/heads/atl/again")
}

// The local branch is gone, origin has it: the worktree comes back on a
// branch made from origin/<branch>, as fetched now.
func TestReopenFromOrigin(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "pushed", Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	e.commitIn(t, task.Slug, "api", "one")
	wt := e.app.Paths.WorktreePath(task.Slug, "api")
	git(t, wt, "push", "-q", e.origin("api"), "atl/pushed")
	git(t, e.canonical("api"), "fetch", "-q", "origin")
	// A later push that the canonical clone has not fetched yet.
	tip := e.commitIn(t, task.Slug, "api", "two")
	git(t, wt, "push", "-q", e.origin("api"), "atl/pushed")
	if _, err := e.app.Cancel(ctx, task.Slug, false); err != nil {
		t.Fatal(err)
	}
	git(t, e.canonical("api"), "branch", "-D", "atl/pushed")
	e.app.Out.(*bytes.Buffer).Reset()

	if err := e.app.SetStatus(ctx, task.Slug, model.StatusReview); err != nil {
		t.Fatal(err)
	}
	got := reopened(t, e, task.Slug, model.StatusReview, tip)
	if got.Repos[0].BaseCommit != task.Repos[0].BaseCommit {
		t.Errorf("base changed: %+v", got.Repos[0])
	}
	if out := e.app.Out.(*bytes.Buffer).String(); !strings.Contains(out, "made again from origin/atl/pushed") ||
		strings.Contains(out, "warning") {
		t.Errorf("output: %q", out)
	}
}

// Neither a local branch nor one on origin: the branch starts again from the
// task's base as of now, which is recorded, and a warning says the old
// commits are not on it.
func TestReopenNoBranch(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "lost", Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	old := e.commitIn(t, task.Slug, "api", "old")
	if _, err := e.app.Done(ctx, task.Slug, false); err != nil {
		t.Fatal(err)
	}
	git(t, e.canonical("api"), "branch", "-D", "atl/lost")
	tip := e.pushOrigin(t, "api", "newer")
	e.app.Out.(*bytes.Buffer).Reset()

	if err := e.app.SetStatus(ctx, task.Slug, model.StatusBacklog); err != nil {
		t.Fatal(err)
	}
	got := reopened(t, e, task.Slug, model.StatusBacklog, tip)
	if got.Repos[0].BaseCommit != tip || got.Repos[0].BaseBranch != "main" {
		t.Errorf("base not recorded: %+v", got.Repos[0])
	}
	if tip == old {
		t.Fatal("test setup")
	}
	if out := e.app.Out.(*bytes.Buffer).String(); !strings.Contains(out,
		"warning: repo api: branch atl/lost is gone, here and on origin; it starts again from origin/main, without the task's earlier commits") {
		t.Errorf("output: %q", out)
	}
}

// A reopen that fails part way undoes what it made and leaves the task
// closed: here the second repo's kept branch is checked out elsewhere.
func TestReopenFailureLeavesTaskClosed(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "web", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "half",
		Repos: []RepoArg{{"api", model.ModeRW}, {"web", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	kept := e.commitIn(t, task.Slug, "web", "w")
	if _, err := e.app.Done(ctx, task.Slug, false); err != nil {
		t.Fatal(err)
	}
	assertNoBranch(t, e.canonical("api"), "atl/half") // made anew by the reopen
	e.pushOrigin(t, "api", "newer")
	elsewhere := filepath.Join(e.root, "elsewhere")
	git(t, e.canonical("web"), "worktree", "add", "-q", elsewhere, "atl/half")

	err = e.app.SetStatus(ctx, task.Slug, model.StatusActive)
	if err == nil || !strings.Contains(err.Error(), "reopen task half: repo web:") || !strings.Contains(err.Error(), "it stays done") {
		t.Fatalf("err = %v", err)
	}
	got, err := e.app.Store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusDone || got.Repos[0].BaseCommit != task.Repos[0].BaseCommit {
		t.Fatalf("task changed: %+v", got)
	}
	if exists(e.app.Paths.TaskDir(task.Slug)) {
		t.Fatal("task dir left behind")
	}
	if n := worktreeCount(t, e.canonical("api")); n != 1 {
		t.Fatalf("api has %d worktrees", n)
	}
	assertNoBranch(t, e.canonical("api"), "atl/half")
	if got := git(t, e.canonical("web"), "rev-parse", "refs/heads/atl/half"); got != kept {
		t.Fatalf("web's kept branch at %s, want %s", got, kept)
	}

	// Once the branch is free the reopen works.
	git(t, e.canonical("web"), "worktree", "remove", elsewhere)
	if err := e.app.SetStatus(ctx, task.Slug, model.StatusActive); err != nil {
		t.Fatal(err)
	}
	if got := git(t, e.app.Paths.WorktreePath(task.Slug, "web"), "rev-parse", "HEAD"); got != kept {
		t.Fatalf("web HEAD %s, want %s", got, kept)
	}
}

// Closed ↔ closed and open → closed through SetStatus change the status only;
// a task closed that way keeps its worktree, which a reopen keeps.
func TestSetStatusPlainChanges(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "plain", Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.Done(ctx, task.Slug, false); err != nil {
		t.Fatal(err)
	}
	if err := e.app.SetStatus(ctx, task.Slug, model.StatusCancelled); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.app.Store.GetTask(ctx, task.ID); got.Status != model.StatusCancelled {
		t.Fatalf("status %s", got.Status)
	}
	if exists(e.app.Paths.TaskDir(task.Slug)) {
		t.Fatal("cancelled → done made the task dir")
	}
	if err := e.app.SetStatus(ctx, task.Slug, model.StatusDone); err != nil {
		t.Fatal(err)
	}
	if exists(e.app.Paths.TaskDir(task.Slug)) {
		t.Fatal("done → cancelled made the task dir")
	}

	other, err := e.app.NewTask(ctx, NewTaskOpts{Title: "flag", Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	head := e.commitIn(t, other.Slug, "api", "f")
	if err := e.app.SetStatus(ctx, other.Slug, model.StatusDone); err != nil {
		t.Fatal(err)
	}
	if !exists(e.app.Paths.WorktreePath(other.Slug, "api")) {
		t.Fatal("open → done removed the worktree")
	}
	if err := e.app.SetStatus(ctx, other.Slug, model.StatusActive); err != nil {
		t.Fatal(err)
	}
	reopened(t, e, other.Slug, model.StatusActive, head)
}

// Reopening is refused inside a task container, like closing.
func TestReopenInsideSandbox(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "boxed", Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.Done(ctx, task.Slug, false); err != nil {
		t.Fatal(err)
	}
	t.Setenv(sandbox.InEnv, "boxed-123456")
	if err := e.app.SetStatus(ctx, task.Slug, model.StatusActive); err == nil || !strings.Contains(err.Error(), "inside a task container") {
		t.Fatalf("err = %v", err)
	}
	t.Setenv(sandbox.InEnv, "")
	if got, _ := e.app.Store.GetTask(ctx, task.ID); got.Status != model.StatusDone || exists(e.app.Paths.TaskDir(task.Slug)) {
		t.Fatalf("task changed: %+v", got)
	}
}

// With the sandbox on: a container that closing the task could not remove
// (Docker did not answer) is removed by the reopen, and the next session
// gets a new container.
func TestReopenSandboxed(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	atl := filepath.Join(e.root, "atl-binary")
	if err := os.WriteFile(atl, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", testToken)
	fd := &fakeDocker{}
	e.app.AtlBin = atl
	e.app.Sandbox = &sandbox.Config{Image: "task-image"}
	e.app.Docker = fd.run
	e.app.StartGate = func(string, string, int64, string, string, []string) error { return nil }

	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "boxed", Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.ensureSandbox(ctx, task); err != nil {
		t.Fatal(err)
	}
	fd.mu.Lock()
	fd.down = true
	fd.mu.Unlock()
	if _, err := e.app.Done(ctx, task.Slug, false); err != nil {
		t.Fatal(err)
	}
	fd.mu.Lock()
	fd.down = false
	left := fd.label != ""
	fd.mu.Unlock()
	if !left {
		t.Fatal("test setup: the container is gone")
	}
	runs := len(fd.find("run"))

	if err := e.app.SetStatus(ctx, task.Slug, model.StatusActive); err != nil {
		t.Fatal(err)
	}
	if len(fd.find("rm")) == 0 {
		t.Fatalf("the old container was not removed: %q", fd.calls)
	}
	settings, err := os.ReadFile(filepath.Join(e.app.Paths.TaskDir(task.Slug), ".claude", "settings.json"))
	if err != nil || !strings.Contains(string(settings), "'"+sandbox.InBin+"' hook SessionStart") {
		t.Fatalf("settings do not call the container's atl: %v\n%s", err, settings)
	}
	if _, err := e.app.ensureSandbox(ctx, task); err != nil {
		t.Fatal(err)
	}
	if n := len(fd.find("run")); n != runs+1 {
		t.Fatalf("docker run calls %d, want a new container (%d)", n, runs+1)
	}
}

// A task closed with `atl status` keeps its worktree, registered in git;
// removed by hand afterwards, the reopen still makes it again (git would
// refuse a path it still has registered, "missing but already registered").
func TestReopenWorktreeRemovedByHand(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "by hand", Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	head := e.commitIn(t, task.Slug, "api", "work")
	if err := e.app.SetStatus(ctx, task.Slug, model.StatusDone); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(e.app.Paths.TaskDir(task.Slug)); err != nil {
		t.Fatal(err)
	}
	if err := e.app.SetStatus(ctx, task.Slug, model.StatusActive); err != nil {
		t.Fatal(err)
	}
	reopened(t, e, task.Slug, model.StatusActive, head)
}

// SetStatus decides under the per-task lock: a change made while done (or
// another operation) holds it waits, and then sees the task as that left
// it. Here the task was closed meanwhile, so active reopens it rather than
// marking a task without its worktrees active.
func TestSetStatusWaitsForLock(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "racing", NoFetch: true, Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	head := e.commitIn(t, task.Slug, "api", "work")
	unlock, err := e.app.lockTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.app.SetStatus(ctx, task.Slug, model.StatusReview) }()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("SetStatus did not wait for the lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	// What Done does under the lock.
	if err := e.app.finishTask(ctx, task, false, model.StatusDone); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	reopened(t, e, task.Slug, model.StatusReview, head)
}
