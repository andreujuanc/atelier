package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
)

// TestLaunchCAS checks ListLaunchedLive / MarkStoppedIfLaunch: a launch
// observed before a relaunch (even one reusing the window id) is stale.
func TestLaunchCAS(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task := mkTask(t, s, "t", model.StatusActive, 0)
	mk := func(st model.SessionState, target string) *model.Session {
		ss := &model.Session{TaskID: task.ID, State: st, TmuxTarget: target}
		if err := s.CreateSession(ctx, ss); err != nil {
			t.Fatal(err)
		}
		return ss
	}
	at := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	a := mk(model.StateIdle, "")
	if err := s.SetSessionLaunch(ctx, a.ID, "@1", "claude-a"); err != nil {
		t.Fatal(err)
	}
	mk(model.StateRunning, "") // launching / not launched by atl: not listed
	mk(model.StateEnded, "@2") // not live: not listed
	b := mk(model.StateWaiting, "@3")

	got, err := s.ListLaunchedLive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []Launch{{ID: a.ID, Target: "@1", Seq: 1}, {ID: b.ID, Target: "@3", Seq: 0}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListLaunchedLive = %+v, want %+v", got, want)
	}

	// Stop, claim and relaunch a into a window with the same id.
	if ok, err := s.MarkStoppedIfLaunch(ctx, got[0], at); !ok || err != nil {
		t.Fatalf("MarkStoppedIfLaunch = %v, %v", ok, err)
	}
	if ok, err := s.ClaimStopped(ctx, a.ID, at); !ok || err != nil {
		t.Fatalf("ClaimStopped = %v, %v", ok, err)
	}
	if err := s.SetSessionLaunch(ctx, a.ID, "@1", "claude-a"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.MarkStoppedIfLaunch(ctx, got[0], at); ok || err != nil {
		t.Fatalf("MarkStoppedIfLaunch(stale launch) = %v, %v; want false", ok, err)
	}
	if g, _ := s.GetSession(ctx, a.ID); g.State != model.StateIdle || g.TmuxTarget != "@1" {
		t.Fatalf("relaunched session = %+v", g)
	}
	// Ended meanwhile: left alone.
	if err := s.SetSessionState(ctx, b.ID, model.StateEnded, at); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.MarkStoppedIfLaunch(ctx, got[1], at); ok || err != nil {
		t.Fatalf("MarkStoppedIfLaunch(ended) = %v, %v; want false", ok, err)
	}
	if ok, err := s.MarkStoppedIfLaunch(ctx, Launch{ID: 9999, Target: "@1"}, at); ok || err != nil {
		t.Fatalf("MarkStoppedIfLaunch(unknown) = %v, %v", ok, err)
	}
}
