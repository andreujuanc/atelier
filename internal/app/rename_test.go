package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andreujuanc/atelier/internal/gen"
	"github.com/andreujuanc/atelier/internal/model"
)

func TestRename(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "Fix login", Repos: []RepoArg{{"api", model.ModeRW}}, NoFetch: true})
	if err != nil {
		t.Fatal(err)
	}
	dir := e.app.Paths.TaskDir(task.Slug)
	before, err := e.app.Store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}

	got, err := e.app.Rename(ctx, "fix", "  Fix the   login page  ")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Fix the login page" || got.Slug != "fix-login" || got.Branch != "atl/fix-login" ||
		got.PortOffset != before.PortOffset || got.Status != before.Status || got.Description != before.Description {
		t.Fatalf("renamed task = %+v", got)
	}
	if fmtRepos(got.Repos) != fmtRepos(before.Repos) {
		t.Fatalf("repos changed: %+v → %+v", before.Repos, got.Repos)
	}
	if _, err := e.app.Store.GetTaskBySlug(ctx, "fix-login"); err != nil {
		t.Fatalf("slug changed: %v", err)
	}
	// worktree, branch and task dir keep the original name
	if b := git(t, filepath.Join(dir, "api"), "symbolic-ref", "--short", "HEAD"); b != "atl/fix-login" {
		t.Fatalf("worktree branch %s", b)
	}
	entries, _ := os.ReadDir(e.app.Paths.Tasks)
	if len(entries) != 1 || entries[0].Name() != "fix-login" {
		t.Fatalf("task dirs = %v", entries)
	}
	// the generated CLAUDE.md follows the title
	md, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil || !strings.Contains(string(md), "# Fix the login page\n") || !strings.Contains(string(md), "Task: `fix-login`") {
		t.Fatalf("CLAUDE.md = %q, %v", md, err)
	}

	// validation: nothing stored
	for _, c := range []struct {
		title string
		err   error
	}{{"", model.ErrTitleEmpty}, {" \t ", model.ErrTitleControl}, {"   ", model.ErrTitleEmpty}, {"a\nb", model.ErrTitleControl}, {"x\x1b[2J", model.ErrTitleControl}} {
		if _, err := e.app.Rename(ctx, "fix-login", c.title); !errors.Is(err, c.err) {
			t.Errorf("Rename(%q) err = %v, want %v", c.title, err, c.err)
		}
	}
	if _, err := e.app.Rename(ctx, "missing", "x"); err == nil {
		t.Error("Rename of an unknown task succeeded")
	}
	if g, _ := e.app.Store.GetTask(ctx, task.ID); g.Title != "Fix the login page" {
		t.Fatalf("title after rejected renames = %q", g.Title)
	}

	// any status: a closed task is renamed, its removed CLAUDE.md is not recreated
	closed := &model.Task{Slug: "old", Title: "Old", Status: model.StatusDone, Branch: "atl/old"}
	if err := e.app.Store.CreateTask(ctx, closed); err != nil {
		t.Fatal(err)
	}
	if got, err := e.app.Rename(ctx, "old", "Old but renamed"); err != nil || got.Title != "Old but renamed" || got.Status != model.StatusDone {
		t.Fatalf("Rename closed = %+v, %v", got, err)
	}
	if exists(e.app.Paths.TaskDir("old")) {
		t.Fatal("rename created a closed task's dir")
	}
}

// TestRenameTitleCap checks that atl new and rename share the title cap.
func TestRenameTitleCap(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	long := strings.Repeat("x", model.MaxTitleLen+1)
	if _, err := e.app.NewTask(ctx, NewTaskOpts{Title: long, Repos: []RepoArg{{"api", model.ModeRW}}, NoFetch: true}); !errors.Is(err, model.ErrTitleTooLong) {
		t.Fatalf("NewTask with a %d-char title: %v", len(long), err)
	}
	if ts, _ := e.app.List(ctx); len(ts) != 0 {
		t.Fatalf("a too long title created a task: %+v", ts)
	}
	// the cap counts after collapsing spaces, in runes
	max := strings.Repeat("é", model.MaxTitleLen)
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "  " + max + "\n", Repos: []RepoArg{{"api", model.ModeRW}}, NoFetch: true})
	if err != nil || task.Title != max {
		t.Fatalf("NewTask at the cap: %v", err)
	}
	if _, err := e.app.Rename(ctx, task.Slug, long); !errors.Is(err, model.ErrTitleTooLong) {
		t.Fatalf("Rename too long: %v", err)
	}
	if got, err := e.app.Rename(ctx, task.Slug, max); err != nil || got.Title != max {
		t.Fatalf("Rename at the cap: %v", err)
	}
}

// TestRenameTaskDirGone checks that a rename racing done/cancel (which
// removes the task dir before it sets the status, without the task lock)
// never recreates the dir or its CLAUDE.md: it warns and the rename stands.
func TestRenameTaskDirGone(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "Fix login", Repos: []RepoArg{{"api", model.ModeRW}}, NoFetch: true})
	if err != nil {
		t.Fatal(err)
	}
	dir := e.app.Paths.TaskDir(task.Slug)
	if err := os.RemoveAll(dir); err != nil { // as finishTask does, status still open
		t.Fatal(err)
	}
	out := e.app.Out.(*bytes.Buffer)
	out.Reset()
	got, err := e.app.Rename(ctx, task.Slug, "Fix the login")
	if err != nil || got.Title != "Fix the login" {
		t.Fatalf("Rename = %+v, %v", got, err)
	}
	if exists(dir) {
		t.Fatal("rename recreated the removed task dir")
	}
	if w := out.String(); !strings.Contains(w, "warning: task fix-login renamed but CLAUDE.md not updated") || !strings.Contains(w, "is gone") {
		t.Fatalf("warning = %q", w)
	}

	// the gen helper itself never creates the dir
	if err := gen.RewriteClaudeMD(dir, *got, nil); err == nil || exists(dir) {
		t.Fatalf("RewriteClaudeMD on a missing dir: %v (exists %v)", err, exists(dir))
	}

	// the dir vanishes after the checks, right before the write
	task2, err := e.app.NewTask(ctx, NewTaskOpts{Title: "Other", Repos: []RepoArg{{"api", model.ModeRW}}, NoFetch: true})
	if err != nil {
		t.Fatal(err)
	}
	dir2 := e.app.Paths.TaskDir(task2.Slug)
	beforeClaudeMDRewrite = func(d string) {
		if err := os.RemoveAll(d); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeClaudeMDRewrite = nil })
	out.Reset()
	if _, err := e.app.Rename(ctx, task2.Slug, "Other renamed"); err != nil {
		t.Fatal(err)
	}
	if exists(dir2) {
		t.Fatal("rename recreated the task dir removed during the rewrite")
	}
	if w := out.String(); !strings.Contains(w, "warning: task other renamed but CLAUDE.md not updated") {
		t.Fatalf("warning = %q", w)
	}
}

func fmtRepos(rs []model.TaskRepo) string {
	var b strings.Builder
	for _, r := range rs {
		b.WriteString(r.Name + "|" + string(r.Mode) + "|" + r.BaseBranch + "|" + r.BaseCommit + ";")
	}
	return b.String()
}
