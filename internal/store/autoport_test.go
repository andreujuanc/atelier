package store

import (
	"context"
	"errors"
	"strings"
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
	if err := s.CreateTaskAutoPort(ctx, task, Avoid{}); err != nil {
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
	if err := s.CreateTaskAutoPort(ctx, bad, Avoid{}); err == nil || bad.ID != 0 {
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
			errs[i] = st.CreateTaskAutoPort(context.Background(), tasks[i], Avoid{})
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

// TestCreateTaskAutoPortAvoid: offsets reserved elsewhere (other
// workspaces) are skipped along with this database's open ones, and slugs
// SlugTaken reports get the next suffix, within the 40-char limit.
func TestCreateTaskAutoPortAvoid(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	mkTask(t, s, "fix", model.StatusActive, 100)
	outside := map[string]bool{"fix-2": true, "new": true}
	var asked []string
	avoid := Avoid{PortOffsets: []int{0, 200, 200, 500}, SlugTaken: func(slug string) (bool, error) {
		asked = append(asked, slug)
		return outside[slug], nil
	}}
	task := &model.Task{Slug: "fix", Title: "Fix", Status: model.StatusActive}
	if err := s.CreateTaskAutoPort(ctx, task, avoid); err != nil {
		t.Fatal(err)
	}
	if task.Slug != "fix-3" || task.Branch != "atl/fix-3" || task.PortOffset != 300 {
		t.Fatalf("got %+v", task)
	}
	// "fix" is taken in the database: SlugTaken is not asked about it.
	if strings.Join(asked, ",") != "fix-2,fix-3" {
		t.Fatalf("SlugTaken asked %v", asked)
	}
	task = &model.Task{Slug: "new", Title: "New", Status: model.StatusActive}
	if err := s.CreateTaskAutoPort(ctx, task, avoid); err != nil {
		t.Fatal(err)
	}
	if task.Slug != "new-2" || task.PortOffset != 400 {
		t.Fatalf("got %+v", task)
	}
	long := strings.Repeat("a", 39) + "b"
	outside[long] = true
	task = &model.Task{Slug: long, Title: "Long", Status: model.StatusActive}
	if err := s.CreateTaskAutoPort(ctx, task, avoid); err != nil {
		t.Fatal(err)
	}
	if task.Slug != strings.Repeat("a", 38)+"-2" || task.PortOffset != 600 {
		t.Fatalf("got %+v", task)
	}

	// A SlugTaken error aborts the insert.
	boom := errors.New("boom")
	bad := &model.Task{Slug: "other", Title: "Other", Status: model.StatusActive}
	err := s.CreateTaskAutoPort(ctx, bad, Avoid{SlugTaken: func(string) (bool, error) { return false, boom }})
	if !errors.Is(err, boom) || bad.ID != 0 {
		t.Fatalf("err = %v, task %+v", err, bad)
	}
	if _, err := s.GetTaskBySlug(ctx, "other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("inserted anyway: %v", err)
	}
}

func TestNextPortOffsetReserved(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	mkTask(t, s, "a", model.StatusActive, 100)
	mkTask(t, s, "b", model.StatusDone, 200)
	for _, tc := range []struct {
		reserved []int
		want     int
	}{
		{nil, 0},
		{[]int{0}, 200},
		{[]int{0, 200}, 300},
		{[]int{100, 300}, 0},
		{[]int{0, 250}, 200}, // not a multiple of 100: never a candidate anyway
	} {
		got, err := s.NextPortOffset(ctx, tc.reserved...)
		if err != nil || got != tc.want {
			t.Errorf("NextPortOffset(%v) = %d, %v; want %d", tc.reserved, got, err, tc.want)
		}
	}
}
