package store

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/andreujuanc/atelier/internal/model"
)

func TestCreateTaskAutoPort(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	mkTask(t, s, "fix", model.StatusActive, 0)
	mkTask(t, s, "old", model.StatusDone, 100) // closed: its offset is free

	task := &model.Task{Slug: "fix", Title: "Fix", Status: model.StatusBacklog,
		Repos: []model.TaskRepo{{Name: "api", Mode: model.ModeRW, BaseBranch: "main", BaseCommit: "abc"}}}
	if err := s.CreateTaskAutoPort(ctx, task); err != nil {
		t.Fatal(err)
	}
	if task.ID == 0 || task.Slug != "fix-2" || task.Branch != "atl/fix-2" || task.PortOffset != 100 || task.CreatedAt.IsZero() {
		t.Fatalf("got %+v", task)
	}
	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Slug != "fix-2" || got.PortOffset != 100 || len(got.Repos) != 1 || got.Repos[0].BaseCommit != "abc" {
		t.Fatalf("stored %+v", got)
	}

	// Validation failures leave the task untouched.
	bad := &model.Task{Slug: "x", Status: "nope"}
	if err := s.CreateTaskAutoPort(ctx, bad); err == nil || bad.ID != 0 {
		t.Fatalf("invalid status: err=%v task=%+v", err, bad)
	}
}

func TestCreateTaskAutoPortConcurrent(t *testing.T) {
	_, path := openTest(t)
	const n = 8
	var wg sync.WaitGroup
	tasks := make([]*model.Task, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, err := Open(path)
			if err != nil {
				errs[i] = err
				return
			}
			defer st.Close()
			tasks[i] = &model.Task{Slug: "same", Title: "Same", Status: model.StatusActive}
			errs[i] = st.CreateTaskAutoPort(context.Background(), tasks[i])
		}(i)
	}
	wg.Wait()
	slugs, ports := map[string]bool{}, map[int]bool{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if slugs[tasks[i].Slug] || ports[tasks[i].PortOffset] {
			t.Fatalf("duplicate slug/port: %+v", tasks[i])
		}
		slugs[tasks[i].Slug], ports[tasks[i].PortOffset] = true, true
	}
	for i := 0; i < n; i++ {
		if !ports[i*100] {
			t.Fatalf("ports not dense: %v", ports)
		}
	}
	if !slugs["same"] || !slugs["same-8"] {
		t.Fatalf("slugs: %v", slugs)
	}
}

func TestDeleteTask(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task := mkTask(t, s, "gone", model.StatusActive, 0, model.TaskRepo{Name: "api", Mode: model.ModeRW})
	sess := &model.Session{TaskID: task.ID, State: model.StateIdle}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTask(ctx, task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTask after delete: %v", err)
	}
	if _, err := s.GetSession(ctx, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session not cascaded: %v", err)
	}
	if err := s.DeleteTask(ctx, task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}
