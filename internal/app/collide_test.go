package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/store"
	"github.com/andreujuanc/atelier/internal/workspace"
)

// twoWorkspaces makes e's App workspace "alpha" under a fresh workspaces root
// ($ATL_WORKSPACES; "alpha" is a symlink to e.root there, so only resolving it
// tells it is this one) and creates workspace "beta" next to it, whose
// read-write store it returns.
func twoWorkspaces(t *testing.T, e *taskEnv) *store.Store {
	t.Helper()
	root := t.TempDir()
	t.Setenv("ATL_WORKSPACES", root)
	if err := os.WriteFile(filepath.Join(e.root, workspace.File), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(e.root, filepath.Join(root, "alpha")); err != nil {
		t.Fatal(err)
	}
	e.app.Paths.Workspace = e.root
	e.app.Workspace = &workspace.Applied{Workspace: &workspace.Workspace{Name: "alpha", Dir: e.root}}
	beta := filepath.Join(root, "beta")
	if err := os.MkdirAll(beta, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beta, workspace.File), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(beta, "data", "atelier.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func mkOtherTask(t *testing.T, st *store.Store, slug string, status model.Status, off int) *model.Task {
	t.Helper()
	task := &model.Task{Slug: slug, Title: slug, Status: status, Branch: "atl/" + slug, PortOffset: off}
	if err := st.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	return task
}

// TestNewTaskAvoidsOtherWorkspaces: a new task's port offset is free of the
// open tasks of every other workspace under the root, and the ones of this
// workspace are not taken for another's. Unreadable databases are skipped
// with a warning; a workspace without one, silently.
func TestNewTaskAvoidsOtherWorkspaces(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	beta := twoWorkspaces(t, e)
	mkOtherTask(t, beta, "one", model.StatusActive, 0)
	mkOtherTask(t, beta, "two", model.StatusReview, 200)
	mkOtherTask(t, beta, "gone", model.StatusDone, 100) // closed: its offset is free
	root := os.Getenv("ATL_WORKSPACES")
	// Not a workspace and no database: nothing to avoid, nothing to say.
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A database that is not one: skipped with a warning.
	bad := filepath.Join(root, "bad", "data", "atelier.db")
	if err := os.MkdirAll(filepath.Dir(bad), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("not sqlite, not at all, not even close"), 0o644); err != nil {
		t.Fatal(err)
	}

	newTask := func(title string) *model.Task {
		t.Helper()
		task, err := e.app.NewTask(ctx, NewTaskOpts{Title: title, NoFetch: true, Repos: []RepoArg{{"api", model.ModeRO}}})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	if task := newTask("First"); task.PortOffset != 100 {
		t.Fatalf("offset %d, want 100", task.PortOffset)
	}
	out := e.app.Out.(*bytes.Buffer).String()
	if !strings.Contains(out, "warning: workspace bad: ") || !strings.Contains(out, "not avoided") || strings.Contains(out, "empty") {
		t.Fatalf("out = %q", out)
	}
	if task := newTask("Second"); task.PortOffset != 300 {
		t.Fatalf("offset %d, want 300", task.PortOffset)
	}
	// Seen from beta, alpha's open tasks (100, 300) are the other workspace's.
	os.Remove(bad)
	betaApp := &App{Paths: e.app.Paths, Store: beta, Out: &bytes.Buffer{}}
	betaApp.Paths.Workspace = filepath.Join(root, "beta")
	betaApp.Paths.DB = filepath.Join(root, "beta", "data", "atelier.db")
	others := betaApp.otherWorkspaceTasks(ctx)
	got := map[int]string{}
	for _, o := range others.Tasks {
		got[o.PortOffset] = o.Workspace + "/" + o.Slug
	}
	if len(others.Warnings) != 0 || len(got) != 2 || got[100] != "alpha/first" || got[300] != "alpha/second" {
		t.Fatalf("from beta: %+v", others)
	}
	if off, err := beta.NextPortOffset(ctx, 100, 300); err != nil || off != 400 {
		t.Fatalf("beta's next offset %d, %v", off, err)
	}
}

// TestDoctorPortOffsetClash: doctor warns about an open task of this
// workspace with the same offset as an open task of another, naming both,
// and does not take this workspace for another one.
func TestDoctorPortOffsetClash(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	beta := twoWorkspaces(t, e)
	check := func() Check {
		t.Helper()
		for _, c := range e.app.Doctor(ctx) {
			if c.Name == "port offsets" {
				return c
			}
		}
		t.Fatal("no port offsets check")
		return Check{}
	}
	mkOtherTask(t, e.app.Store, "fix", model.StatusActive, 100)
	mkOtherTask(t, e.app.Store, "old", model.StatusDone, 200)
	mkOtherTask(t, beta, "other", model.StatusActive, 0)
	mkOtherTask(t, beta, "closed", model.StatusCancelled, 100)
	if c := check(); !c.OK || c.Warn || !strings.Contains(c.Detail, "1 other workspaces read") {
		t.Fatalf("no clash: %+v", c)
	}
	mkOtherTask(t, beta, "login", model.StatusBacklog, 100)
	mkOtherTask(t, beta, "older", model.StatusActive, 200) // alpha's task with 200 is closed
	c := check()
	if !c.OK || !c.Warn || !strings.Contains(c.Detail, "alpha/fix and beta/login (+100)") ||
		strings.Contains(c.Detail, "older") || strings.Contains(c.Detail, "alpha/fix and alpha/") ||
		!strings.Contains(c.Detail, "close one") {
		t.Fatalf("clash: %+v", c)
	}
}
