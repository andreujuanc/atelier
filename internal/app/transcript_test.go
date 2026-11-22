package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/model"
)

// TestTranscriptReadIsConfined: the gate recorded a transcript path inside
// the task's Claude dir, then the task changed what is there. atl show,
// atl transcript and the picker must not read another file through it, nor
// hang on a FIFO; a recorded path elsewhere is not read at all.
func TestTranscriptReadIsConfined(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	task := &model.Task{Slug: "boxed", Title: "Boxed", Status: model.StatusActive, Branch: "atl/boxed"}
	if err := e.app.Store.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	// The user's own transcript, which no task may read.
	userProj := filepath.Join(config.ClaudeConfigDir(), "projects", "-x")
	secret := filepath.Join(userProj, "user-session.jsonl")
	writeTestFile(t, secret, `{"type":"user","message":{"content":"SECRET-OF-THE-USER"}}`+"\n")
	outside := filepath.Join(e.root, "elsewhere.jsonl")
	writeTestFile(t, outside, `{"type":"user","message":{"content":"SECRET-OF-THE-USER elsewhere"}}`+"\n")

	proj := filepath.Join(e.app.Paths.ClaudeDir("boxed"), "projects", "p")
	path := filepath.Join(proj, "s.jsonl")
	writeTestFile(t, path, `{"type":"user","message":{"content":"the task's own prompt"}}`+"\n")
	s := &model.Session{TaskID: task.ID, State: model.StateIdle, TranscriptPath: path}
	if err := e.app.Store.CreateSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	if turns, err := e.app.Transcript(ctx, "boxed", s.ID, 0); err != nil || len(turns) != 1 || turns[0].Text != "the task's own prompt" {
		t.Fatalf("own transcript: %+v, %v", turns, err)
	}

	read := func(what string) {
		t.Helper()
		done := make(chan string, 1)
		go func() {
			var got []string
			if turns, err := e.app.Transcript(ctx, "boxed", s.ID, 0); err == nil {
				for _, tr := range turns {
					got = append(got, tr.Text)
				}
			}
			if d, err := e.app.Show(ctx, "boxed"); err == nil {
				got = append(got, d.SessionTitles[s.ID])
			}
			done <- strings.Join(got, " | ")
		}()
		select {
		case got := <-done:
			if strings.Contains(got, "SECRET") {
				t.Errorf("%s: read %q", what, got)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: reading the transcript blocked", what)
		}
	}

	os.Remove(path)
	if err := os.Symlink(secret, path); err != nil {
		t.Fatal(err)
	}
	read("the transcript swapped for a symlink to the user's")

	os.Remove(path)
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Fatal(err)
	}
	read("the transcript swapped for a FIFO")

	os.RemoveAll(proj)
	if err := os.Symlink(userProj, proj); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(userProj, "s.jsonl"), `{"type":"user","message":{"content":"SECRET-OF-THE-USER too"}}`+"\n")
	read("a directory on the way swapped for a symlink")

	if err := e.app.Store.SetTranscriptPath(ctx, s.ID, outside); err != nil {
		t.Fatal(err)
	}
	read("a recorded path outside the Claude dirs")
	if _, err := e.app.Transcript(ctx, "boxed", s.ID, 0); err == nil || !strings.Contains(err.Error(), "not read") {
		t.Errorf("a path outside the Claude dirs: %v", err)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
