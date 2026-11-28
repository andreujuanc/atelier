package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andreujuanc/atelier/internal/model"
)

func sessionTranscripts(t *testing.T, s *Store, id int64) string {
	t.Helper()
	m, err := s.ListSessionTranscripts(context.Background(), []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(m[id], ",")
}

func TestSessionTranscripts(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task := mkTask(t, s, "fix", model.StatusActive, 0)
	sess := &model.Session{TaskID: task.ID, State: model.StateIdle, TranscriptPath: "/c/a.jsonl"}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	none := &model.Session{TaskID: task.ID, State: model.StateIdle}
	if err := s.CreateSession(ctx, none); err != nil {
		t.Fatal(err)
	}
	if got := sessionTranscripts(t, s, sess.ID); got != "/c/a.jsonl" {
		t.Fatalf("after create: %q", got)
	}
	if got := sessionTranscripts(t, s, none.ID); got != "" {
		t.Fatalf("no transcript: %q", got)
	}
	steps := []func() error{
		func() error { return s.SetTranscriptPath(ctx, sess.ID, "/c/b.jsonl") },
		func() error { return s.SetTranscriptPathIfEmpty(ctx, sess.ID, "/c/x.jsonl") }, // not empty: nothing
		func() error { return s.SetTranscriptPath(ctx, sess.ID, "/c/a.jsonl") },        // known
		func() error { return s.AddSessionTranscript(ctx, sess.ID, "/c/c.jsonl") },
		func() error { return s.AddSessionTranscript(ctx, sess.ID, "") },
		func() error { return s.AddSessionTranscript(ctx, 999, "/c/d.jsonl") }, // unknown session
		func() error { return s.SetTranscriptPathIfEmpty(ctx, none.ID, "/c/n.jsonl") },
	}
	for i, f := range steps {
		if err := f(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	got := strings.Split(sessionTranscripts(t, s, sess.ID), ",")
	if len(got) != 3 || !strings.Contains(strings.Join(got, ","), "/c/a.jsonl") || !strings.Contains(strings.Join(got, ","), "/c/b.jsonl") ||
		!strings.Contains(strings.Join(got, ","), "/c/c.jsonl") {
		t.Errorf("paths: %q", got)
	}
	// Both ways in take first_seen from the row (one clock).
	var odd int
	if err := s.db.QueryRow(`SELECT count(*) FROM session_transcripts st JOIN sessions s ON s.id = st.session_id
		WHERE st.first_seen != s.last_event_at`).Scan(&odd); err != nil || odd != 0 {
		t.Errorf("%d rows with another first_seen (%v)", odd, err)
	}
	if got := sessionTranscripts(t, s, none.ID); got != "/c/n.jsonl" {
		t.Errorf("filled: %q", got)
	}

	// Capped per session, both ways in.
	for i := range maxSessionTranscripts + 10 {
		if err := s.SetTranscriptPath(ctx, sess.ID, fmt.Sprintf("/c/s%d.jsonl", i)); err != nil {
			t.Fatal(err)
		}
		if err := s.AddSessionTranscript(ctx, sess.ID, fmt.Sprintf("/c/h%d.jsonl", i)); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(strings.Split(sessionTranscripts(t, s, sess.ID), ",")); n != maxSessionTranscripts {
		t.Errorf("%d paths, cap %d", n, maxSessionTranscripts)
	}

	if err := s.DeleteTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM session_transcripts`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d rows after the task was deleted (%v)", n, err)
	}
}

// TestMigrationV8 records the transcript paths sessions had before it.
func TestMigrationV8(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atelier.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range migrations[:7] {
		if _, err := db.Exec(m); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	for _, q := range []string{
		`PRAGMA user_version = 7`,
		`INSERT INTO tasks (id, slug, title, status, branch, port_offset, created_at, updated_at) VALUES (1, 'one', 'One', 'active', 'atl/one', 0, 1000, 1000)`,
		`INSERT INTO sessions (id, task_id, state, created_at, last_event_at, transcript_path) VALUES (1, 1, 'idle', 1000, 2000, '/c/a.jsonl'), (2, 1, 'idle', 1000, 2000, '')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m, err := s.ListSessionTranscripts(context.Background(), []int64{1, 2})
	if err != nil || len(m) != 1 || strings.Join(m[1], ",") != "/c/a.jsonl" {
		t.Errorf("after v8: %v %v", m, err)
	}
}
