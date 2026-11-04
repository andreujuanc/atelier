package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
)

func TestAddTaskRepo(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task := mkTask(t, s, "fix", model.StatusActive, 0,
		model.TaskRepo{Name: "zeta", Mode: model.ModeRW, BaseBranch: "main", BaseCommit: "abc"},
		model.TaskRepo{Name: "alpha", Mode: model.ModeRO})
	other := mkTask(t, s, "other", model.StatusActive, 100)
	before, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)

	add := model.TaskRepo{Name: "mid", Mode: model.ModeRW, BaseBranch: "trunk", BaseCommit: "def"}
	if err := s.AddTaskRepo(ctx, task.ID, add); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTaskRepo(ctx, task.ID, model.TaskRepo{Name: "docs", Mode: model.ModeRO}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []model.TaskRepo{before.Repos[0], before.Repos[1], add, {Name: "docs", Mode: model.ModeRO}}
	if !reflect.DeepEqual(got.Repos, want) {
		t.Fatalf("repos = %+v\nwant %+v", got.Repos, want)
	}
	if !got.UpdatedAt.After(before.UpdatedAt) {
		t.Error("updated_at not bumped")
	}

	// First repo of a task without repos gets position 0; repos of other
	// tasks are untouched.
	if err := s.AddTaskRepo(ctx, other.ID, model.TaskRepo{Name: "mid", Mode: model.ModeRO}); err != nil {
		t.Fatal(err)
	}
	var pos int
	if err := s.db.QueryRowContext(ctx, `SELECT position FROM task_repos WHERE task_id = ? AND name = 'mid'`, other.ID).Scan(&pos); err != nil || pos != 0 {
		t.Fatalf("position %d, %v", pos, err)
	}

	// Duplicate (either mode) is rejected and changes nothing.
	if err := s.AddTaskRepo(ctx, task.ID, model.TaskRepo{Name: "alpha", Mode: model.ModeRW}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate err = %v", err)
	}
	if g, _ := s.GetTask(ctx, task.ID); !reflect.DeepEqual(g.Repos, want) {
		t.Fatalf("duplicate changed repos: %+v", g.Repos)
	}

	if err := s.AddTaskRepo(ctx, 9999, model.TaskRepo{Name: "x", Mode: model.ModeRW}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown task err = %v", err)
	}
	if err := s.AddTaskRepo(ctx, task.ID, model.TaskRepo{Name: "", Mode: model.ModeRW}); err == nil {
		t.Error("empty name accepted")
	}
	if err := s.AddTaskRepo(ctx, task.ID, model.TaskRepo{Name: "y", Mode: "rx"}); err == nil {
		t.Error("invalid mode accepted")
	}
}

func TestAddTaskRepoClosedTask(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	for _, st := range []model.Status{model.StatusDone, model.StatusCancelled} {
		task := mkTask(t, s, "closed-"+string(st), model.StatusActive, 0)
		if err := s.SetTaskStatus(ctx, task.ID, st); err != nil {
			t.Fatal(err)
		}
		err := s.AddTaskRepo(ctx, task.ID, model.TaskRepo{Name: "x", Mode: model.ModeRO})
		if !errors.Is(err, ErrTaskClosed) || errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: err = %v, want ErrTaskClosed", st, err)
		}
		if g, _ := s.GetTask(ctx, task.ID); len(g.Repos) != 0 {
			t.Fatalf("%s: repo added to closed task: %+v", st, g.Repos)
		}
	}
	for _, st := range []model.Status{model.StatusBacklog, model.StatusReview} {
		task := mkTask(t, s, "open-"+string(st), st, 0)
		if err := s.AddTaskRepo(ctx, task.ID, model.TaskRepo{Name: "x", Mode: model.ModeRO}); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
	}
}
