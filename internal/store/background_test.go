package store

import (
	"context"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
)

// TestSessionBackground: SetIdle records a session's background work and
// every other state change clears it.
func TestSessionBackground(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	task := &model.Task{Slug: "bg", Title: "Bg", Status: model.StatusActive, Branch: "atl/bg"}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	sess := &model.Session{TaskID: task.ID, ClaudeSessionID: "cid", TmuxTarget: "@1", State: model.StateRunning}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	at := time.UnixMilli(5000)
	bg := "subagent: Review\nshell: npm run dev"
	get := func() *model.Session {
		t.Helper()
		g, err := s.GetSession(ctx, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	idle := func() {
		t.Helper()
		if err := s.SetIdle(ctx, sess.ID, bg, at); err != nil {
			t.Fatal(err)
		}
		if g := get(); g.State != model.StateIdle || g.Background != bg || !g.LastEventAt.Equal(at) {
			t.Fatalf("SetIdle: %+v", g)
		}
	}
	for name, change := range map[string]func() error{
		"SetSessionState": func() error { return s.SetSessionState(ctx, sess.ID, model.StateRunning, at) },
		"SetStateIf": func() error {
			_, err := s.SetStateIf(ctx, sess.ID, model.StateIdle, model.StateStopped, at)
			return err
		},
		"MarkStoppedIfLaunch": func() error {
			l, err := s.ListLaunchedLive(ctx)
			if err != nil || len(l) != 1 {
				t.Fatalf("ListLaunchedLive = %v, %v", l, err)
			}
			_, err = s.MarkStoppedIfLaunch(ctx, l[0], at)
			return err
		},
		"SetClaudeSession": func() error { return s.SetClaudeSession(ctx, sess.ID, "cid", at) },
		"EndLive": func() error {
			_, err := s.EndLive(ctx, sess.ID, -1, at)
			return err
		},
		"UpdateSession": func() error {
			g := get()
			g.State = model.StateWaiting
			return s.UpdateSession(ctx, g)
		},
	} {
		idle()
		if err := change(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if g := get(); g.Background != "" {
			t.Errorf("%s kept the background: %q", name, g.Background)
		}
		if err := s.SetSessionLaunch(ctx, sess.ID, "@1", ""); err != nil { // live again, with a target
			t.Fatal(err)
		}
	}
	// A resume claim starts with none.
	idle()
	if err := s.SetSessionState(ctx, sess.ID, model.StateStopped, at); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimFrom(ctx, sess.ID, model.StateStopped, at); !ok || err != nil || get().Background != "" {
		t.Fatalf("ClaimFrom: %v %v %q", ok, err, get().Background)
	}
	// Compaction keeps it: the same process, its work goes on.
	idle()
	if err := s.SetClaudeSessionID(ctx, sess.ID, "cid2", at); err != nil || get().Background != bg {
		t.Fatalf("SetClaudeSessionID: %v %q", err, get().Background)
	}
}
