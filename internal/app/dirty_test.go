package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/andreujuanc/atelier/internal/model"
)

func TestDirtyTasks(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "web", "main")
	e.addRepo(t, "docs", "main")
	mk := func(title string, repos ...RepoArg) *model.Task {
		t.Helper()
		task, err := e.app.NewTask(ctx, NewTaskOpts{Title: title, Repos: repos})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	touch := func(slug, repo string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(e.app.Paths.WorktreePath(slug, repo), "dirty"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rm := func(slug, repo string) {
		t.Helper()
		if err := os.RemoveAll(e.app.Paths.WorktreePath(slug, repo)); err != nil {
			t.Fatal(err)
		}
	}
	dirty := mk("dirty", RepoArg{"api", model.ModeRW}, RepoArg{"docs", model.ModeRO})
	touch("dirty", "api")
	clean := mk("clean", RepoArg{"api", model.ModeRW}, RepoArg{"web", model.ModeRW})
	gone := mk("gone", RepoArg{"api", model.ModeRW})
	rm("gone", "api")
	mixed := mk("mixed", RepoArg{"api", model.ModeRW}, RepoArg{"web", model.ModeRW})
	rm("mixed", "api")
	touch("mixed", "web")
	closed := mk("closed", RepoArg{"api", model.ModeRW})
	touch("closed", "api")
	if err := e.app.SetStatus(ctx, "closed", model.StatusDone); err != nil {
		t.Fatal(err)
	}
	review := mk("review", RepoArg{"web", model.ModeRW})
	touch("review", "web")
	if err := e.app.SetStatus(ctx, "review", model.StatusReview); err != nil {
		t.Fatal(err)
	}
	// a dirty canonical clone of a ro repo does not make a task dirty
	if err := os.WriteFile(filepath.Join(e.canonical("docs"), "dirty"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	roOnly := mk("ro only", RepoArg{"docs", model.ModeRO})

	got, err := e.app.DirtyTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]bool{dirty.ID: true, mixed.ID: true, review.ID: true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for id := range want {
		if !got[id] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	for _, id := range []int64{clean.ID, gone.ID, closed.ID, roOnly.ID} {
		if got[id] {
			t.Fatalf("task %d reported dirty: %v", id, got)
		}
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := e.app.DirtyTasks(cctx); err == nil {
		t.Fatal("cancelled context: no error")
	}
}
