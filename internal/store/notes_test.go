package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
)

// TestMigrationV3FromV2 builds a v2 database by hand (migrations 1-2 only),
// with rows in it, and checks that opening it applies v3: existing rows
// keep working with defaults, and notes can be added.
func TestMigrationV3FromV2(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "atelier.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range migrations[:2] {
		if _, err := db.Exec(m); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO tasks (id, slug, title, status, branch, port_offset, created_at, updated_at)
		VALUES (1, 'old', 'Old', 'active', 'atl/old', 0, 1000, 1000)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions (id, task_id, claude_session_id, state, created_at, last_event_at, launch_seq)
		VALUES (5, 1, 'cid', 'idle', 1000, 2000, 3)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != len(migrations) {
		t.Fatalf("user_version = %d (%v), want %d", v, err, len(migrations))
	}
	ss, err := s.GetSession(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if ss.TranscriptPath != "" || ss.ClaudeSessionID != "cid" || ss.LastEventAt.UnixMilli() != 2000 {
		t.Fatalf("migrated session = %+v", ss)
	}
	if err := s.SetTranscriptPathIfEmpty(ctx, 5, "/t/a.jsonl"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendNote(ctx, 1, model.AuthorUser, "hello"); err != nil {
		t.Fatal(err)
	}
	if text, err := s.GetNotes(ctx, 1); err != nil || !strings.HasSuffix(text, "[user] hello") {
		t.Fatalf("notes = %q, %v", text, err)
	}
	if ss, _ := s.GetSession(ctx, 5); ss.TranscriptPath != "/t/a.jsonl" {
		t.Fatalf("transcript = %q", ss.TranscriptPath)
	}
}

func TestTranscriptPath(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task := mkTask(t, s, "t", model.StatusActive, 0)
	ss := &model.Session{TaskID: task.ID, State: model.StateRunning, Name: "claude-1", ClaudeSessionID: "c1"}
	if err := s.CreateSession(ctx, ss); err != nil {
		t.Fatal(err)
	}
	get := func() *model.Session {
		t.Helper()
		g, err := s.GetSession(ctx, ss.ID)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	if get().TranscriptPath != "" {
		t.Fatal("default transcript path not empty")
	}
	if err := s.SetTranscriptPathIfEmpty(ctx, ss.ID, "/a"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTranscriptPathIfEmpty(ctx, ss.ID, "/b"); err != nil {
		t.Fatal(err)
	}
	if g := get(); g.TranscriptPath != "/a" {
		t.Fatalf("IfEmpty overwrote: %q", g.TranscriptPath)
	}
	if err := s.SetTranscriptPath(ctx, ss.ID, "/c"); err != nil {
		t.Fatal(err)
	}
	g := get()
	if g.TranscriptPath != "/c" || g.State != model.StateRunning || g.Name != "claude-1" || g.ClaudeSessionID != "c1" {
		t.Fatalf("after SetTranscriptPath: %+v", g)
	}
	if err := s.SetTranscriptPath(ctx, 999, "/x"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown id: %v", err)
	}
	if err := s.SetTranscriptPathIfEmpty(ctx, 999, "/x"); err != nil {
		t.Fatalf("IfEmpty unknown id: %v", err)
	}
	// CreateSession stores it; list scans include it.
	s2 := &model.Session{TaskID: task.ID, State: model.StateIdle, TranscriptPath: "/d"}
	if err := s.CreateSession(ctx, s2); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListSessions(ctx, task.ID)
	if err != nil || len(list) != 2 || list[0].TranscriptPath != "/c" || list[1].TranscriptPath != "/d" {
		t.Fatalf("ListSessions = %+v, %v", list, err)
	}
}

func TestNotes(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	a := mkTask(t, s, "a", model.StatusActive, 0)
	b := mkTask(t, s, "b", model.StatusDone, 100)
	today := time.Now().Format("2006-01-02")

	if text, err := s.GetNotes(ctx, a.ID); err != nil || text != "" {
		t.Fatalf("new task notes = %q, %v", text, err)
	}
	if _, err := s.GetNotes(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetNotes(unknown) = %v", err)
	}

	// Append: first line, then a newline separator; bodies sanitised and
	// trimmed; closed tasks too.
	line, err := s.AppendNote(ctx, a.ID, model.AuthorUser, "  first\x1b[31m note \n")
	if err != nil || line != "- "+today+" [user] first[31m note" {
		t.Fatalf("AppendNote = %q, %v", line, err)
	}
	if _, err := s.AppendNote(ctx, a.ID, model.AuthorAssistant, "two\r\nlines"); err != nil {
		t.Fatal(err)
	}
	want := "- " + today + " [user] first[31m note\n- " + today + " [assistant] two\nlines"
	if text, _ := s.GetNotes(ctx, a.ID); text != want {
		t.Fatalf("notes = %q, want %q", text, want)
	}
	if _, err := s.AppendNote(ctx, b.ID, model.AuthorUser, "closed"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []struct {
		id           int64
		author, body string
	}{{a.ID, model.AuthorUser, " \t\n"}, {a.ID, "bot", "x"}, {a.ID, "", "x"}, {999, model.AuthorUser, "x"}} {
		if _, err := s.AppendNote(ctx, bad.id, bad.author, bad.body); err == nil {
			t.Errorf("AppendNote(%+v) succeeded", bad)
		}
	}

	// SetNotes: compare-and-set against the text the edit started from.
	base, _ := s.GetNotes(ctx, a.ID)
	saved, err := s.SetNotes(ctx, a.ID, "mine\r\n\twith tab\x07\n\n", base)
	if err != nil || saved != "mine\n\twith tab" {
		t.Fatalf("SetNotes = %q, %v", saved, err)
	}
	if text, _ := s.GetNotes(ctx, a.ID); text != saved {
		t.Fatalf("stored %q", text)
	}
	// an agent appends meanwhile: a save from the old base conflicts and
	// changes nothing; the error carries the current text
	if _, err := s.AppendNote(ctx, a.ID, model.AuthorAssistant, "agent"); err != nil {
		t.Fatal(err)
	}
	cur, _ := s.GetNotes(ctx, a.ID)
	if cur != "mine\n\twith tab\n- "+today+" [assistant] agent" {
		t.Fatalf("after append: %q", cur)
	}
	_, err = s.SetNotes(ctx, a.ID, "stale edit", saved)
	var ce *model.NotesConflictError
	if !errors.Is(err, model.ErrNotesConflict) || !errors.As(err, &ce) || ce.Current != cur {
		t.Fatalf("stale SetNotes = %v", err)
	}
	if text, _ := s.GetNotes(ctx, a.ID); text != cur {
		t.Fatalf("conflict changed the notes: %q", text)
	}
	// overwrite: compare against the reported text
	if saved, err := s.SetNotes(ctx, a.ID, "stale edit", ce.Current); err != nil || saved != "stale edit" {
		t.Fatalf("overwrite = %q, %v", saved, err)
	}
	if _, err := s.SetNotes(ctx, 999, "x", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetNotes(unknown) = %v", err)
	}
	// ReplaceNotes ignores the current text; "" clears
	if saved, err := s.ReplaceNotes(ctx, a.ID, ""); err != nil || saved != "" {
		t.Fatalf("ReplaceNotes(\"\") = %q, %v", saved, err)
	}
	if text, _ := s.GetNotes(ctx, a.ID); text != "" {
		t.Fatalf("not cleared: %q", text)
	}
	// a trailing newline is not doubled by an append
	if _, err := s.db.Exec(`UPDATE tasks SET notes = 'x' || char(10) WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendNote(ctx, a.ID, model.AuthorUser, "y"); err != nil {
		t.Fatal(err)
	}
	if text, _ := s.GetNotes(ctx, a.ID); text != "x\n- "+today+" [user] y" {
		t.Fatalf("after newline-terminated notes: %q", text)
	}
	if text, _ := s.GetNotes(ctx, b.ID); text != "- "+today+" [user] closed" {
		t.Fatalf("closed task notes: %q", text)
	}
}

func TestNotesCap(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	a := mkTask(t, s, "a", model.StatusActive, 0)
	full := strings.Repeat("x", model.MaxNotesLen)
	if _, err := s.SetNotes(ctx, a.ID, full+"\n\n  ", ""); err != nil {
		t.Fatalf("SetNotes at the cap: %v", err) // trailing blanks don't count
	}
	if _, err := s.ReplaceNotes(ctx, a.ID, full+"y"); !errors.Is(err, model.ErrNotesTooLarge) ||
		!strings.Contains(err.Error(), "65537 bytes") || !strings.Contains(err.Error(), "64 KiB") {
		t.Fatalf("ReplaceNotes over the cap = %v", err)
	}
	if _, err := s.SetNotes(ctx, a.ID, full+"y", full); !errors.Is(err, model.ErrNotesTooLarge) {
		t.Fatalf("SetNotes over the cap = %v", err)
	}
	if _, err := s.AppendNote(ctx, a.ID, model.AuthorUser, "more"); !errors.Is(err, model.ErrNotesTooLarge) {
		t.Fatalf("AppendNote over the cap = %v", err)
	}
	if text, _ := s.GetNotes(ctx, a.ID); text != full {
		t.Fatalf("notes changed by refused writes (len %d)", len(text))
	}
	// an append that fits is fine
	if _, err := s.ReplaceNotes(ctx, a.ID, strings.Repeat("x", model.MaxNotesLen-100)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendNote(ctx, a.ID, model.AuthorUser, "fits"); err != nil {
		t.Fatalf("append within the cap: %v", err)
	}
}

// TestNotesConcurrentAppend checks that appends from several connections
// never lose a line (each is a read-modify-write in one IMMEDIATE tx).
func TestNotesConcurrentAppend(t *testing.T) {
	s, path := openTest(t)
	ctx := context.Background()
	a := mkTask(t, s, "a", model.StatusActive, 0)
	const n = 20
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			s2, err := Open(path)
			if err != nil {
				errs <- err
				return
			}
			defer s2.Close()
			_, err = s2.AppendNote(ctx, a.ID, model.AuthorAssistant, fmt.Sprintf("n%02d", i))
			errs <- err
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	text, _ := s.GetNotes(ctx, a.ID)
	if lines := strings.Split(text, "\n"); len(lines) != n {
		t.Fatalf("%d lines, want %d:\n%s", len(lines), n, text)
	}
}

// TestMigrationV5FromV4 builds a v4 database by hand with dated notes and
// checks that opening it folds them into tasks.notes (oldest first, local
// dates, newlines in a body kept), keeps task_notes, and that the
// migration is idempotent.
func TestMigrationV5FromV4(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "atelier.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range migrations[:4] {
		if _, err := db.Exec(m); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 4`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO tasks (id, slug, title, status, branch, port_offset, created_at, updated_at, base)
		 VALUES (1, 'one', 'One', 'active', 'atl/one', 0, 1000, 1000, ''),
		        (2, 'two', 'Two', 'done', 'atl/two', 100, 1000, 1000, ':default'),
		        (3, 'three', 'Three', 'backlog', 'atl/three', 200, 1000, 1000, '')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	d1 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.Local)
	d2 := time.Date(2026, 3, 5, 12, 0, 0, 0, time.Local)
	ins := func(id, task int64, body, author string, at time.Time) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO task_notes (id, task_id, body, author, created_at) VALUES (?, ?, ?, ?, ?)`,
			id, task, body, author, at.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	// inserted out of order: the migration orders by created_at, then id
	ins(10, 1, "second", "assistant", d2)
	ins(11, 1, "first\nwith a second line", "user", d1)
	ins(12, 1, "same day, later id", "user", d2)
	ins(13, 2, "closed task", "user", d1)
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != 5 || len(migrations) != 5 {
		t.Fatalf("user_version = %d (%v), migrations %d", v, err, len(migrations))
	}
	want := map[int64]string{
		1: "- 2026-03-01 [user] first\nwith a second line\n- 2026-03-05 [assistant] second\n- 2026-03-05 [user] same day, later id",
		2: "- 2026-03-01 [user] closed task",
		3: "",
	}
	check := func() {
		t.Helper()
		for id, w := range want {
			if got, err := s.GetNotes(ctx, id); err != nil || got != w {
				t.Errorf("task %d notes = %q, %v; want %q", id, got, err, w)
			}
		}
	}
	check()
	// task_notes is kept (not destructive)
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM task_notes`).Scan(&n); err != nil || n != 4 {
		t.Fatalf("task_notes rows = %d, %v", n, err)
	}
	// existing tasks keep working
	if tk, err := s.GetTask(ctx, 2); err != nil || tk.Base != ":default" || tk.Status != model.StatusDone {
		t.Fatalf("task 2 = %+v, %v", tk, err)
	}
	// idempotent: running the v5 UPDATE again (and reopening) changes
	// nothing, even after the notes were edited
	if _, err := s.SetNotes(ctx, 3, "edited", ""); err != nil {
		t.Fatal(err)
	}
	want[3] = "edited"
	update := migrations[4][strings.Index(migrations[4], "UPDATE"):]
	if _, err := s.db.Exec(update); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check()
}

// TestMigrationV4FromV3 builds a v3 database by hand, with a task in it,
// and checks that opening it applies v4: the old task gets base "" (auto)
// and new tasks round-trip their base.
func TestMigrationV4FromV3(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "atelier.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range migrations[:3] {
		if _, err := db.Exec(m); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO tasks (id, slug, title, status, branch, port_offset, created_at, updated_at)
		VALUES (1, 'old', 'Old', 'active', 'atl/old', 0, 1000, 1000)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO task_repos (task_id, name, mode, base_branch, base_commit, position)
		VALUES (1, 'api', 'rw', 'main', 'abc', 0)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != len(migrations) {
		t.Fatalf("user_version = %d (%v), want %d", v, err, len(migrations))
	}
	old, err := s.GetTask(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if old.Base != model.BaseDefault || len(old.Repos) != 1 || old.Repos[0].BaseBranch != "main" {
		t.Fatalf("migrated task = %+v", old)
	}
	nt := &model.Task{Slug: "prod", Title: "Prod", Status: model.StatusActive, Base: model.BaseDefault}
	if err := s.CreateTaskAutoPort(ctx, nt); err != nil {
		t.Fatal(err)
	}
	nt = &model.Task{Slug: "auto", Title: "Auto", Status: model.StatusActive, Base: model.BaseAuto}
	if err := s.CreateTaskAutoPort(ctx, nt); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bases := map[string]string{}
	for _, x := range list {
		bases[x.Slug] = x.Base
	}
	if bases["old"] != ":default" || bases["prod"] != ":default" || bases["auto"] != "" {
		t.Fatalf("bases = %v", bases)
	}
}
