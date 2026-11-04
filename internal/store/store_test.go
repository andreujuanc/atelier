package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
)

func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nested", "dir", "atelier.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func mkTask(t *testing.T, s *Store, slug string, st model.Status, off int, repos ...model.TaskRepo) *model.Task {
	t.Helper()
	task := &model.Task{Slug: slug, Title: "T " + slug, Status: st, Branch: "atl/" + slug, PortOffset: off, Repos: repos}
	if err := s.CreateTask(context.Background(), task); err != nil {
		t.Fatalf("CreateTask(%s): %v", slug, err)
	}
	return task
}

func TestMigrationsIdempotent(t *testing.T) {
	s, path := openTest(t)
	ctx := context.Background()
	mkTask(t, s, "keep-me", model.StatusActive, 0)
	s.Close()

	for i := 0; i < 2; i++ {
		s2, err := Open(path)
		if err != nil {
			t.Fatalf("reopen %d: %v", i, err)
		}
		var v int
		if err := s2.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v != len(migrations) {
			t.Fatalf("user_version = %d, want %d", v, len(migrations))
		}
		if _, err := s2.GetTaskBySlug(ctx, "keep-me"); err != nil {
			t.Fatalf("data lost after reopen: %v", err)
		}
		s2.Close()
	}
}

func TestPragmas(t *testing.T) {
	s, _ := openTest(t)
	// Check on several pooled connections at once.
	ctx := context.Background()
	var conns []interface{ Close() error }
	for i := 0; i < 3; i++ {
		c, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		var jm string
		var fk, bt, sync int
		c.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&jm)
		c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk)
		c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&bt)
		c.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync)
		if jm != "wal" || fk != 1 || bt != 3000 || sync != 1 {
			t.Errorf("conn %d: journal_mode=%s foreign_keys=%d busy_timeout=%d synchronous=%d", i, jm, fk, bt, sync)
		}
	}
	for _, c := range conns {
		c.Close()
	}
}

func TestTaskCRUD(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	repos := []model.TaskRepo{
		{Name: "zeta", Mode: model.ModeRW, BaseBranch: "main", BaseCommit: "abc123"},
		{Name: "alpha", Mode: model.ModeRO},
		{Name: "mid", Mode: model.ModeRW, BaseBranch: "develop", BaseCommit: "def456"},
	}
	task := &model.Task{Slug: "fix-bug", Title: "Fix bug", Description: "desc", Status: model.StatusBacklog,
		Project: "acme", Branch: "atl/fix-bug", PortOffset: 200, Base: "release/1.2", Repos: repos}
	before := time.Now().Add(-time.Second)
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if task.ID == 0 || task.CreatedAt.Before(before) || task.UpdatedAt.IsZero() {
		t.Fatalf("ID/timestamps not set: %+v", task)
	}
	if task.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt not UTC")
	}

	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, task) {
		t.Fatalf("GetTask mismatch\n got %+v\nwant %+v", got, task)
	}
	got2, err := s.GetTaskBySlug(ctx, "fix-bug")
	if err != nil || !reflect.DeepEqual(got2, task) {
		t.Fatalf("GetTaskBySlug: %v %+v", err, got2)
	}

	// Explicit timestamps are preserved (at ms precision).
	ts := time.Date(2025, 1, 2, 3, 4, 5, 6_789_000, time.FixedZone("X", 3600))
	t2 := &model.Task{Slug: "old", Title: "Old", Status: model.StatusDone, Branch: "atl/old", CreatedAt: ts, UpdatedAt: ts}
	if err := s.CreateTask(ctx, t2); err != nil {
		t.Fatal(err)
	}
	g, _ := s.GetTask(ctx, t2.ID)
	if !g.CreatedAt.Equal(ts.Truncate(time.Millisecond)) || g.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt = %v, want %v UTC", g.CreatedAt, ts)
	}
	if len(g.Repos) != 0 {
		t.Errorf("expected no repos, got %v", g.Repos)
	}

	// Duplicate slug fails.
	if err := s.CreateTask(ctx, &model.Task{Slug: "fix-bug", Status: model.StatusBacklog}); err == nil {
		t.Error("duplicate slug: expected error")
	}

	// Not found.
	if _, err := s.GetTask(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetTask(9999) err = %v", err)
	}
	if _, err := s.GetTaskBySlug(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetTaskBySlug(nope) err = %v", err)
	}

	// SetTaskStatus bumps updated_at.
	time.Sleep(5 * time.Millisecond)
	if err := s.SetTaskStatus(ctx, task.ID, model.StatusActive); err != nil {
		t.Fatal(err)
	}
	g, _ = s.GetTask(ctx, task.ID)
	if g.Status != model.StatusActive || !g.UpdatedAt.After(task.UpdatedAt) {
		t.Errorf("SetTaskStatus: %+v", g)
	}
	if err := s.SetTaskStatus(ctx, 9999, model.StatusActive); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetTaskStatus(9999) err = %v", err)
	}

	time.Sleep(5 * time.Millisecond)
	if err := s.TouchTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	g2, _ := s.GetTask(ctx, task.ID)
	if !g2.UpdatedAt.After(g.UpdatedAt) {
		t.Errorf("TouchTask did not bump updated_at")
	}
	if err := s.TouchTask(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("TouchTask(9999) err = %v", err)
	}
}

func TestTaskValidation(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	bad := []*model.Task{
		{Slug: "a", Status: "open"},
		{Slug: "", Status: model.StatusActive},
		{Slug: "b", Status: model.StatusActive, Repos: []model.TaskRepo{{Name: "r", Mode: "wo"}}},
		{Slug: "c", Status: model.StatusActive, Repos: []model.TaskRepo{{Name: "", Mode: model.ModeRW}}},
	}
	for _, b := range bad {
		if err := s.CreateTask(ctx, b); err == nil {
			t.Errorf("CreateTask(%+v): expected error", b)
		}
	}
	task := mkTask(t, s, "ok", model.StatusActive, 0)
	if err := s.SetTaskStatus(ctx, task.ID, "bogus"); err == nil {
		t.Error("SetTaskStatus(bogus): expected error")
	}
	if _, err := s.ListTasks(ctx, "bogus"); err == nil {
		t.Error("ListTasks(bogus): expected error")
	}
	// Failed repo insert rolls back the task.
	err := s.CreateTask(ctx, &model.Task{Slug: "dup-repo", Status: model.StatusActive,
		Repos: []model.TaskRepo{{Name: "x", Mode: model.ModeRW}, {Name: "x", Mode: model.ModeRO}}})
	if err == nil {
		t.Fatal("duplicate repo: expected error")
	}
	if _, err := s.GetTaskBySlug(ctx, "dup-repo"); !errors.Is(err, ErrNotFound) {
		t.Errorf("task not rolled back: %v", err)
	}
	// SQL CHECK constraint backs up Go validation.
	if _, err := s.db.Exec(`UPDATE tasks SET status = 'bogus' WHERE id = ?`, task.ID); err == nil {
		t.Error("CHECK constraint on status not enforced")
	}
}

func TestListTasks(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	base := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	add := func(slug string, st model.Status, minutes int, repos ...model.TaskRepo) {
		ts := base.Add(time.Duration(minutes) * time.Minute)
		task := &model.Task{Slug: slug, Status: st, Branch: "atl/" + slug, CreatedAt: ts, UpdatedAt: ts, Repos: repos}
		if err := s.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	add("done-old", model.StatusDone, 1)
	add("active-old", model.StatusActive, 2, model.TaskRepo{Name: "b", Mode: model.ModeRW}, model.TaskRepo{Name: "a", Mode: model.ModeRO})
	add("backlog-1", model.StatusBacklog, 3)
	add("cancelled", model.StatusCancelled, 4)
	add("active-new", model.StatusActive, 5, model.TaskRepo{Name: "c", Mode: model.ModeRW})
	add("review", model.StatusReview, 6)
	add("done-new", model.StatusDone, 7)

	slugs := func(ts []model.Task) []string {
		var out []string
		for _, t := range ts {
			out = append(out, t.Slug)
		}
		return out
	}
	cases := []struct {
		statuses []model.Status
		want     []string
	}{
		{nil, []string{"backlog-1", "active-new", "active-old", "review", "done-new", "done-old", "cancelled"}},
		{[]model.Status{model.StatusDone, model.StatusActive}, []string{"active-new", "active-old", "done-new", "done-old"}},
		{[]model.Status{model.StatusReview}, []string{"review"}},
	}
	for _, c := range cases {
		got, err := s.ListTasks(ctx, c.statuses...)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(slugs(got), c.want) {
			t.Errorf("ListTasks(%v) = %v, want %v", c.statuses, slugs(got), c.want)
		}
		for _, task := range got {
			switch task.Slug {
			case "active-old":
				if len(task.Repos) != 2 || task.Repos[0].Name != "b" || task.Repos[1].Name != "a" || task.Repos[1].Mode != model.ModeRO {
					t.Errorf("active-old repos = %+v", task.Repos)
				}
			case "active-new":
				if len(task.Repos) != 1 || task.Repos[0].Name != "c" {
					t.Errorf("active-new repos = %+v", task.Repos)
				}
			default:
				if len(task.Repos) != 0 {
					t.Errorf("%s repos = %+v", task.Slug, task.Repos)
				}
			}
		}
	}
	empty, _ := openTest(t)
	got, err := empty.ListTasks(ctx)
	if err != nil || len(got) != 0 {
		t.Errorf("empty ListTasks = %v, %v", got, err)
	}
}

func TestResolveTask(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	a := mkTask(t, s, "fix-login", model.StatusActive, 0)
	b := mkTask(t, s, "fix-logout", model.StatusActive, 100)
	c := mkTask(t, s, "add-feature", model.StatusActive, 200)
	d := mkTask(t, s, "fix", model.StatusActive, 300)
	// Slug that looks like the id of another task: exact slug wins.
	e := mkTask(t, s, fmt.Sprint(a.ID), model.StatusActive, 400)
	f := mkTask(t, s, "2024-report", model.StatusActive, 500)

	cases := []struct {
		ref    string
		wantID int64
		err    error
	}{
		{"fix-login", a.ID, nil},
		{"fix", d.ID, nil}, // exact beats prefix
		{fmt.Sprint(a.ID), e.ID, nil},
		{fmt.Sprint(b.ID), b.ID, nil},
		{fmt.Sprint(c.ID), c.ID, nil},
		{"add", c.ID, nil},
		{"fix-log", 0, ErrAmbiguous},
		{"fix-logo", b.ID, nil},
		{"fix-", 0, ErrAmbiguous},
		{"fix-logou", b.ID, nil},
		{"2024-r", f.ID, nil},
		{"2024", f.ID, nil}, // all digits, no such id → prefix match
		{"zzz", 0, ErrNotFound},
		{"", 0, ErrNotFound},
		{"%", 0, ErrNotFound},
		{"99999999999999999999999", 0, ErrNotFound}, // overflows int64, no prefix match
		{"2024999999999999999999", 0, ErrNotFound},
	}
	for _, tc := range cases {
		got, err := s.ResolveTask(ctx, tc.ref)
		if tc.err != nil {
			if !errors.Is(err, tc.err) {
				t.Errorf("ResolveTask(%q) err = %v, want %v", tc.ref, err, tc.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("ResolveTask(%q): %v", tc.ref, err)
			continue
		}
		if got.ID != tc.wantID {
			t.Errorf("ResolveTask(%q) = %d (%s), want %d", tc.ref, got.ID, got.Slug, tc.wantID)
		}
	}
}

func TestUniqueSlug(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	check := func(base, want string) {
		t.Helper()
		got, err := s.UniqueSlug(ctx, base)
		if err != nil || got != want {
			t.Errorf("UniqueSlug(%q) = %q, %v; want %q", base, got, err, want)
		}
	}
	check("fix", "fix")
	mkTask(t, s, "fix", model.StatusDone, 0)
	check("fix", "fix-2")
	mkTask(t, s, "fix-2", model.StatusActive, 0)
	mkTask(t, s, "fix-4", model.StatusActive, 0)
	mkTask(t, s, "fixer", model.StatusActive, 0)
	check("fix", "fix-3")
	mkTask(t, s, "fix-3", model.StatusActive, 0)
	check("fix", "fix-5")
	check("fixer", "fixer-2")
	check("fi", "fi")

	// Suffixed slugs stay within 40 chars; trimmed base loses trailing "-".
	long := strings.Repeat("a", 37) + "-bc" // 40 chars
	mkTask(t, s, long, model.StatusActive, 0)
	check(long, strings.Repeat("a", 37)+"-2")
	mkTask(t, s, strings.Repeat("a", 37)+"-2", model.StatusActive, 0)
	check(long, strings.Repeat("a", 37)+"-3")
	for i := 3; i <= 9; i++ {
		mkTask(t, s, fmt.Sprintf("%s-%d", strings.Repeat("a", 37), i), model.StatusActive, 0)
	}
	check(long, strings.Repeat("a", 37)+"-10")
	check(strings.Repeat("z", 45), strings.Repeat("z", 40))
}

func TestNextPortOffset(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	next := func(want int) {
		t.Helper()
		got, err := s.NextPortOffset(ctx)
		if err != nil || got != want {
			t.Errorf("NextPortOffset = %d, %v; want %d", got, err, want)
		}
	}
	next(0)
	mkTask(t, s, "a", model.StatusActive, 0)
	next(100)
	mkTask(t, s, "b", model.StatusBacklog, 100)
	mkTask(t, s, "c", model.StatusReview, 300)
	next(200) // gap reused
	mkTask(t, s, "d", model.StatusDone, 200)
	mkTask(t, s, "e", model.StatusCancelled, 200)
	next(200) // done/cancelled don't hold offsets
	x := mkTask(t, s, "x", model.StatusActive, 200)
	next(400)
	if err := s.SetTaskStatus(ctx, x.ID, model.StatusDone); err != nil {
		t.Fatal(err)
	}
	next(200)
}

func TestSessions(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task := mkTask(t, s, "t1", model.StatusActive, 0)
	other := mkTask(t, s, "t2", model.StatusActive, 100, model.TaskRepo{Name: "r", Mode: model.ModeRW})

	base := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	s1 := &model.Session{TaskID: task.ID, Name: "claude-1", TmuxTarget: "@1", State: model.StateRunning, CreatedAt: base.Add(2 * time.Minute)}
	s2 := &model.Session{TaskID: task.ID, Name: "claude-2", State: model.StateIdle, CreatedAt: base.Add(time.Minute)}
	s3 := &model.Session{TaskID: other.ID, Name: "claude-1", State: model.StateEnded}
	for _, ss := range []*model.Session{s1, s2, s3} {
		if err := s.CreateSession(ctx, ss); err != nil {
			t.Fatal(err)
		}
		if ss.ID == 0 || ss.CreatedAt.IsZero() || ss.LastEventAt.IsZero() {
			t.Fatalf("ID/timestamps not set: %+v", ss)
		}
	}
	got, err := s.GetSession(ctx, s1.ID)
	if err != nil || !reflect.DeepEqual(got, s1) {
		t.Fatalf("GetSession = %+v, %v; want %+v", got, err, s1)
	}
	if _, err := s.GetSession(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSession(9999) err = %v", err)
	}

	list, err := s.ListSessions(ctx, task.ID)
	if err != nil || len(list) != 2 || list[0].ID != s2.ID || list[1].ID != s1.ID {
		t.Errorf("ListSessions = %+v, %v", list, err)
	}

	// Validation.
	if err := s.CreateSession(ctx, &model.Session{TaskID: task.ID, State: "dead"}); err == nil {
		t.Error("CreateSession(invalid state): expected error")
	}
	if err := s.CreateSession(ctx, &model.Session{TaskID: 9999, State: model.StateRunning}); err == nil {
		t.Error("CreateSession(unknown task): expected FK error")
	}

	// UpdateSession.
	s2.ClaudeSessionID = "cid-A"
	s2.TmuxTarget = "@7"
	s2.Name = "renamed"
	s2.State = model.StateWaiting
	s2.LastEventAt = base.Add(time.Hour)
	if err := s.UpdateSession(ctx, s2); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSession(ctx, s2.ID)
	if !reflect.DeepEqual(got, s2) {
		t.Errorf("after UpdateSession got %+v want %+v", got, s2)
	}
	if err := s.UpdateSession(ctx, &model.Session{ID: 9999, State: model.StateIdle}); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateSession(9999) err = %v", err)
	}
	if err := s.UpdateSession(ctx, &model.Session{ID: s2.ID, State: "x"}); err == nil {
		t.Error("UpdateSession(invalid state): expected error")
	}

	// GetSessionByClaudeID: most recent match wins; "" never matches.
	s4 := &model.Session{TaskID: other.ID, ClaudeSessionID: "cid-A", State: model.StateRunning}
	if err := s.CreateSession(ctx, s4); err != nil {
		t.Fatal(err)
	}
	g, err := s.GetSessionByClaudeID(ctx, "cid-A")
	if err != nil || g.ID != s4.ID {
		t.Errorf("GetSessionByClaudeID = %+v, %v; want id %d", g, err, s4.ID)
	}
	if _, err := s.GetSessionByClaudeID(ctx, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf(`GetSessionByClaudeID("") err = %v`, err)
	}
	if _, err := s.GetSessionByClaudeID(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf(`GetSessionByClaudeID("nope") err = %v`, err)
	}

	// SetSessionState.
	at := base.Add(2 * time.Hour)
	if err := s.SetSessionState(ctx, s1.ID, model.StateStopped, at); err != nil {
		t.Fatal(err)
	}
	g, _ = s.GetSession(ctx, s1.ID)
	if g.State != model.StateStopped || !g.LastEventAt.Equal(at) {
		t.Errorf("SetSessionState: %+v", g)
	}
	if err := s.SetSessionState(ctx, s1.ID, "x", at); err == nil {
		t.Error("SetSessionState(invalid): expected error")
	}
	if err := s.SetSessionState(ctx, 9999, model.StateIdle, at); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetSessionState(9999) err = %v", err)
	}

	// ListSessionsByState. States now: s1 stopped, s2 waiting, s3 ended, s4 running.
	ids := func(ss []model.Session) []int64 {
		var out []int64
		for _, x := range ss {
			out = append(out, x.ID)
		}
		return out
	}
	byState, err := s.ListSessionsByState(ctx, model.StateRunning, model.StateWaiting, model.StateIdle)
	if err != nil {
		t.Fatal(err)
	}
	var wantLive []int64
	for _, x := range []*model.Session{s2, s4} {
		wantLive = append(wantLive, x.ID)
	}
	if !reflect.DeepEqual(ids(byState), wantLive) {
		t.Errorf("ListSessionsByState(live) = %v, want %v", ids(byState), wantLive)
	}
	all, _ := s.ListSessionsByState(ctx)
	if len(all) != 4 {
		t.Errorf("ListSessionsByState() len = %d, want 4", len(all))
	}
	if _, err := s.ListSessionsByState(ctx, "x"); err == nil {
		t.Error("ListSessionsByState(invalid): expected error")
	}

	// Schema cascades task deletion to sessions and repos.
	if got, _ := s.GetTask(ctx, other.ID); len(got.Repos) != 1 {
		t.Fatalf("precondition: task repos = %+v", got.Repos)
	}
	if _, err := s.db.Exec(`DELETE FROM tasks WHERE id = ?`, other.ID); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.ListSessions(ctx, other.ID); len(l) != 0 {
		t.Errorf("sessions not cascaded: %+v", l)
	}
	var nRepos int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM task_repos WHERE task_id = ?`, other.ID).Scan(&nRepos); err != nil {
		t.Fatal(err)
	}
	if nRepos != 0 {
		t.Errorf("task_repos not cascaded: %d rows", nRepos)
	}
}

func TestConcurrentStores(t *testing.T) {
	_, path := openTest(t)
	ctx := context.Background()
	const writers, perWriter = 4, 25
	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter*2)
	for w := 0; w < writers; w++ {
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		wg.Add(1)
		go func(w int, st *Store) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				task := &model.Task{Slug: fmt.Sprintf("w%d-%d", w, i), Status: model.StatusActive,
					Repos: []model.TaskRepo{{Name: "r", Mode: model.ModeRW}}}
				if err := st.CreateTask(ctx, task); err != nil {
					errs <- err
					continue
				}
				sess := &model.Session{TaskID: task.ID, State: model.StateRunning}
				if err := st.CreateSession(ctx, sess); err != nil {
					errs <- err
				}
				if err := st.SetSessionState(ctx, sess.ID, model.StateIdle, time.Time{}); err != nil {
					errs <- err
				}
				if _, err := st.ListTasks(ctx); err != nil {
					errs <- err
				}
			}
		}(w, st)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	all, _ := s.ListTasks(ctx)
	if len(all) != writers*perWriter {
		t.Errorf("got %d tasks, want %d", len(all), writers*perWriter)
	}
}

func TestSessionTransitions(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task := mkTask(t, s, "t", model.StatusActive, 0)
	mk := func(st model.SessionState) *model.Session {
		ss := &model.Session{TaskID: task.ID, ClaudeSessionID: "cid", TmuxTarget: "@7", Name: "n", State: st}
		if err := s.CreateSession(ctx, ss); err != nil {
			t.Fatal(err)
		}
		return ss
	}
	at := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	get := func(id int64) *model.Session {
		g, err := s.GetSession(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}

	// ClaimStopped: only stopped sessions, and only once.
	for _, st := range model.AllSessionStates {
		ss := mk(st)
		ok, err := s.ClaimStopped(ctx, ss.ID, at)
		if err != nil {
			t.Fatal(err)
		}
		if want := st == model.StateStopped; ok != want {
			t.Errorf("ClaimStopped(%s) = %v", st, ok)
		}
		g := get(ss.ID)
		if st == model.StateStopped {
			if g.State != model.StateIdle || g.TmuxTarget != "" || !g.LastEventAt.Equal(at) || g.ClaudeSessionID != "cid" || g.Name != "n" {
				t.Errorf("after ClaimStopped: %+v", g)
			}
			if ok, err := s.ClaimStopped(ctx, ss.ID, at); ok || err != nil {
				t.Errorf("second ClaimStopped = %v, %v", ok, err)
			}
		} else if g.State != st || g.TmuxTarget != "@7" {
			t.Errorf("ClaimStopped touched %s session: %+v", st, g)
		}
	}
	if ok, err := s.ClaimStopped(ctx, 9999, at); ok || err != nil {
		t.Errorf("ClaimStopped(9999) = %v, %v", ok, err)
	}

	// ClaimFrom: only from the state the caller saw (stopped or ended), once,
	// bumping launch_seq like ClaimStopped.
	for _, st := range model.AllSessionStates {
		_, err := s.ClaimFrom(ctx, 1, st, at)
		if valid := st == model.StateStopped || st == model.StateEnded; valid != (err == nil) {
			t.Errorf("ClaimFrom(from %s) err = %v", st, err)
		}
	}
	ended := mk(model.StateEnded)
	if ok, err := s.ClaimFrom(ctx, ended.ID, model.StateStopped, at); ok || err != nil {
		t.Errorf("ClaimFrom(ended session, from stopped) = %v, %v", ok, err)
	}
	var seq0, seq1 int64
	if err := s.db.QueryRowContext(ctx, `SELECT launch_seq FROM sessions WHERE id = ?`, ended.ID).Scan(&seq0); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimFrom(ctx, ended.ID, model.StateEnded, at); !ok || err != nil {
		t.Errorf("ClaimFrom(ended) = %v, %v", ok, err)
	}
	if g := get(ended.ID); g.State != model.StateIdle || g.TmuxTarget != "" || g.ClaudeSessionID != "cid" || !g.LastEventAt.Equal(at) {
		t.Errorf("after ClaimFrom(ended): %+v", g)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT launch_seq FROM sessions WHERE id = ?`, ended.ID).Scan(&seq1); err != nil {
		t.Fatal(err)
	}
	if seq1 != seq0+1 {
		t.Errorf("launch_seq %d → %d, want +1", seq0, seq1)
	}
	if ok, err := s.ClaimFrom(ctx, ended.ID, model.StateEnded, at); ok || err != nil {
		t.Errorf("second ClaimFrom(ended) = %v, %v", ok, err)
	}

	// SetSessionLaunch: only target and name.
	ss := mk(model.StateRunning)
	before := get(ss.ID)
	if err := s.SetSessionLaunch(ctx, ss.ID, "@42", "claude-x"); err != nil {
		t.Fatal(err)
	}
	g := get(ss.ID)
	want := *before
	want.TmuxTarget, want.Name = "@42", "claude-x"
	if *g != want {
		t.Errorf("after SetSessionLaunch: %+v, want %+v", g, want)
	}
	if err := s.SetSessionLaunch(ctx, 9999, "@1", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetSessionLaunch(9999) err = %v", err)
	}
}

func TestHookSessionUpdates(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task := mkTask(t, s, "t", model.StatusActive, 0)
	at := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	get := func(id int64) model.Session {
		g, err := s.GetSession(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return *g
	}
	mk := func(st model.SessionState) model.Session {
		ss := &model.Session{TaskID: task.ID, ClaudeSessionID: "old", TmuxTarget: "@7", Name: "n", State: st}
		if err := s.CreateSession(ctx, ss); err != nil {
			t.Fatal(err)
		}
		return get(ss.ID)
	}

	// SetClaudeSession: from any state → idle, new claude id, last_event_at; nothing else.
	for _, st := range model.AllSessionStates {
		before := mk(st)
		if err := s.SetClaudeSession(ctx, before.ID, "new", at); err != nil {
			t.Fatal(err)
		}
		want := before
		want.ClaudeSessionID, want.State, want.LastEventAt = "new", model.StateIdle, at
		if g := get(before.ID); g != want {
			t.Errorf("SetClaudeSession from %s: %+v, want %+v", st, g, want)
		}
	}
	if err := s.SetClaudeSession(ctx, 9999, "x", at); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetClaudeSession(9999) err = %v", err)
	}

	// SetStateIf: only from the given state.
	for _, st := range model.AllSessionStates {
		before := mk(st)
		ok, err := s.SetStateIf(ctx, before.ID, model.StateWaiting, model.StateRunning, at)
		if err != nil {
			t.Fatal(err)
		}
		if want := st == model.StateWaiting; ok != want {
			t.Errorf("SetStateIf(waiting→running) from %s = %v", st, ok)
		}
		want := before
		if st == model.StateWaiting {
			want.State, want.LastEventAt = model.StateRunning, at
		}
		if g := get(before.ID); g != want {
			t.Errorf("SetStateIf from %s: %+v, want %+v", st, g, want)
		}
	}
	if ok, err := s.SetStateIf(ctx, 9999, model.StateWaiting, model.StateRunning, at); ok || err != nil {
		t.Errorf("SetStateIf(9999) = %v, %v", ok, err)
	}
	if _, err := s.SetStateIf(ctx, 1, "bogus", model.StateRunning, at); err == nil {
		t.Error("SetStateIf with invalid state: want error")
	}

	// TouchSession: last_event_at only.
	before := mk(model.StateRunning)
	if err := s.TouchSession(ctx, before.ID, at); err != nil {
		t.Fatal(err)
	}
	want := before
	want.LastEventAt = at
	if g := get(before.ID); g != want {
		t.Errorf("TouchSession: %+v, want %+v", g, want)
	}
	if err := s.TouchSession(ctx, 9999, at); !errors.Is(err, ErrNotFound) {
		t.Errorf("TouchSession(9999) err = %v", err)
	}
}

func TestSetClaudeSessionID(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task := mkTask(t, s, "t", model.StatusActive, 0)
	at := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	for _, st := range model.AllSessionStates {
		ss := &model.Session{TaskID: task.ID, ClaudeSessionID: "old", TmuxTarget: "@7", Name: "n", State: st}
		if err := s.CreateSession(ctx, ss); err != nil {
			t.Fatal(err)
		}
		before, _ := s.GetSession(ctx, ss.ID)
		if err := s.SetClaudeSessionID(ctx, ss.ID, "compacted", at); err != nil {
			t.Fatal(err)
		}
		want := *before
		want.ClaudeSessionID, want.LastEventAt = "compacted", at
		if g, _ := s.GetSession(ctx, ss.ID); *g != want {
			t.Errorf("SetClaudeSessionID from %s: %+v, want %+v", st, *g, want)
		}
	}
	if err := s.SetClaudeSessionID(ctx, 9999, "x", at); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetClaudeSessionID(9999) err = %v", err)
	}
}

// TestOpenCurrentSchemaNoWriteLock checks that opening an up-to-date
// database does not need the write lock, and that OpenWithTimeout sets the
// busy timeout.
func TestOpenCurrentSchemaNoWriteLock(t *testing.T) {
	s, path := openTest(t)
	ctx := context.Background()
	c, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer c.ExecContext(ctx, "ROLLBACK")

	start := time.Now()
	s2, err := OpenWithTimeout(path, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("open while another connection holds the write lock: %v", err)
	}
	defer s2.Close()
	if d := time.Since(start); d > 90*time.Millisecond {
		t.Errorf("open took %v; it should not wait on the write lock", d)
	}
	var bt int
	if err := s2.db.QueryRow("PRAGMA busy_timeout").Scan(&bt); err != nil || bt != 100 {
		t.Errorf("busy_timeout = %d, %v; want 100", bt, err)
	}
	if _, err := s2.GetTaskBySlug(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("read while locked: %v", err)
	}
}

func TestOpenConcurrentFreshDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Open(path)
			if err != nil {
				errs <- err
				return
			}
			s.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestOpenNewerSchema(t *testing.T) {
	s, path := openTest(t)
	if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", len(migrations)+1)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s2, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer than this binary") {
		if s2 != nil {
			s2.Close()
		}
		t.Errorf("Open newer schema: err = %v", err)
	}
}
