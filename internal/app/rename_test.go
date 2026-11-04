package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func fmtRepos(rs []model.TaskRepo) string {
	var b strings.Builder
	for _, r := range rs {
		b.WriteString(r.Name + "|" + string(r.Mode) + "|" + r.BaseBranch + "|" + r.BaseCommit + ";")
	}
	return b.String()
}
