package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
)

func TestUpdateTaskTitle(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task := mkTask(t, s, "fix-login", model.StatusDone, 100,
		model.TaskRepo{Name: "api", Mode: model.ModeRW, BaseBranch: "main", BaseCommit: "abc"})
	other := mkTask(t, s, "other", model.StatusActive, 200)
	before, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(5 * time.Millisecond)
	if err := s.UpdateTaskTitle(ctx, task.ID, "Fix the login page"); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Title != "Fix the login page" || !after.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("after rename: %+v", after)
	}
	// only the title (and updated_at) changed
	want := *before
	want.Title, want.UpdatedAt = after.Title, after.UpdatedAt
	if !equalTask(want, *after) {
		t.Fatalf("rename changed more than the title:\nbefore %+v\nafter  %+v", before, after)
	}
	if o, _ := s.GetTask(ctx, other.ID); o.Title != "T other" {
		t.Fatalf("other task renamed: %+v", o)
	}
	if g, err := s.GetTaskBySlug(ctx, "fix-login"); err != nil || g.ID != task.ID {
		t.Fatalf("slug lookup after rename: %v %v", g, err)
	}

	for _, bad := range []string{"", "   ", " padded ", "two  spaces", "tab\there", "line\nbreak", "esc\x1b[31m", "bad\xffutf8"} {
		if err := s.UpdateTaskTitle(ctx, task.ID, bad); err == nil {
			t.Errorf("UpdateTaskTitle(%q) accepted", bad)
		}
	}
	if g, _ := s.GetTask(ctx, task.ID); g.Title != "Fix the login page" {
		t.Fatalf("rejected title stored: %q", g.Title)
	}
	if err := s.UpdateTaskTitle(ctx, 9999, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateTaskTitle(9999) err = %v", err)
	}
}

func equalTask(a, b model.Task) bool {
	if a.ID != b.ID || a.Slug != b.Slug || a.Title != b.Title || a.Description != b.Description ||
		a.Status != b.Status || a.Project != b.Project || a.Branch != b.Branch || a.PortOffset != b.PortOffset ||
		a.Base != b.Base || !a.CreatedAt.Equal(b.CreatedAt) || !a.UpdatedAt.Equal(b.UpdatedAt) || len(a.Repos) != len(b.Repos) {
		return false
	}
	for i := range a.Repos {
		if a.Repos[i] != b.Repos[i] {
			return false
		}
	}
	return true
}
