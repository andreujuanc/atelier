package store

import (
	"context"
	"database/sql"
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
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != 3 {
		t.Fatalf("user_version = %d (%v), want 3", v, err)
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
	n := &model.Note{TaskID: 1, Body: "hello", Author: model.AuthorUser}
	if err := s.AddNote(ctx, n); err != nil {
		t.Fatal(err)
	}
	if notes, err := s.ListNotes(ctx, 1, 0); err != nil || len(notes) != 1 || notes[0].Body != "hello" {
		t.Fatalf("notes = %+v, %v", notes, err)
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
	base := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		n := &model.Note{TaskID: a.ID, Body: fmt.Sprintf("n%d", i), Author: model.AuthorUser, CreatedAt: base.Add(time.Duration(i) * time.Minute)}
		if err := s.AddNote(ctx, n); err != nil {
			t.Fatal(err)
		}
		if n.ID == 0 || !n.CreatedAt.Equal(base.Add(time.Duration(i)*time.Minute)) {
			t.Fatalf("note = %+v", n)
		}
	}
	// same timestamp: higher id is newer
	same := &model.Note{TaskID: a.ID, Body: "n4", Author: model.AuthorAssistant, CreatedAt: base.Add(3 * time.Minute)}
	if err := s.AddNote(ctx, same); err != nil {
		t.Fatal(err)
	}
	nb := &model.Note{TaskID: b.ID, Body: "closed task note", Author: model.AuthorUser}
	if err := s.AddNote(ctx, nb); err != nil {
		t.Fatal(err)
	}
	if nb.CreatedAt.IsZero() {
		t.Fatal("zero CreatedAt not filled")
	}
	all, err := s.ListNotes(ctx, a.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var bodies []string
	for _, n := range all {
		bodies = append(bodies, n.Body)
	}
	if got := strings.Join(bodies, ","); got != "n4,n3,n2,n1,n0" {
		t.Fatalf("order = %s", got)
	}
	if all[0].Author != model.AuthorAssistant || all[0].TaskID != a.ID {
		t.Fatalf("first = %+v", all[0])
	}
	two, err := s.ListNotes(ctx, a.ID, 2)
	if err != nil || len(two) != 2 || two[0].Body != "n4" || two[1].Body != "n3" {
		t.Fatalf("limit 2 = %+v, %v", two, err)
	}
	if none, err := s.ListNotes(ctx, 999, 0); err != nil || len(none) != 0 {
		t.Fatalf("unknown task = %+v, %v", none, err)
	}
	for _, bad := range []*model.Note{
		{TaskID: a.ID, Body: "  ", Author: model.AuthorUser},
		{TaskID: a.ID, Body: "x", Author: "bot"},
		{TaskID: 999, Body: "x", Author: model.AuthorUser}, // FK
	} {
		if err := s.AddNote(ctx, bad); err == nil {
			t.Errorf("AddNote(%+v) succeeded", bad)
		}
	}
	// cascade on task delete
	if err := s.DeleteTask(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if left, err := s.ListNotes(ctx, a.ID, 0); err != nil || len(left) != 0 {
		t.Fatalf("notes after delete = %+v, %v", left, err)
	}
}
