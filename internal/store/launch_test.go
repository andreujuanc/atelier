package store

import (
	"context"
	"fmt"
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

// TestUnrecordedLaunches checks CreateNamedSession, ListUnrecordedLaunches
// and MarkStoppedIfUnrecorded: a live row atl made or claimed for a launch
// and has not recorded a window for is listed, a hook's row is not, and the
// compare-and-set misses a row recorded since.
func TestUnrecordedLaunches(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task := mkTask(t, s, "t", model.StatusActive, 0)
	named := &model.Session{TaskID: task.ID, State: model.StateIdle}
	if err := s.CreateNamedSession(ctx, named, func(id int64) string { return fmt.Sprintf("w-%d", id) }); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetSession(ctx, named.ID); named.Name != fmt.Sprintf("w-%d", named.ID) || got.Name != named.Name {
		t.Fatalf("named %+v, stored %+v", named, got)
	}
	hook := &model.Session{TaskID: task.ID, ClaudeSessionID: "c", State: model.StateIdle}
	if err := s.CreateSession(ctx, hook); err != nil {
		t.Fatal(err)
	}
	claimed := &model.Session{TaskID: task.ID, ClaudeSessionID: "d", State: model.StateStopped}
	if err := s.CreateSession(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimStopped(ctx, claimed.ID, time.Time{}); !ok || err != nil {
		t.Fatalf("ClaimStopped = %v, %v", ok, err)
	}
	got, err := s.ListUnrecordedLaunches(ctx)
	want := []Unrecorded{{ID: named.ID, TaskID: task.ID, Slug: "t", Name: named.Name}, {ID: claimed.ID, TaskID: task.ID, Slug: "t", Seq: 1}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ListUnrecordedLaunches = %+v, %v; want %+v", got, err, want)
	}
	if err := s.SetSessionLaunch(ctx, named.ID, "@1", named.Name); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.MarkStoppedIfUnrecorded(ctx, got[0], time.Time{}); ok || err != nil {
		t.Fatalf("MarkStoppedIfUnrecorded(recorded since) = %v, %v", ok, err)
	}
	if ok, err := s.MarkStoppedIfUnrecorded(ctx, got[1], time.Time{}); !ok || err != nil {
		t.Fatalf("MarkStoppedIfUnrecorded = %v, %v", ok, err)
	}
	if st, _ := s.GetSession(ctx, claimed.ID); st.State != model.StateStopped || st.ClaudeSessionID != "d" {
		t.Fatalf("claimed %+v", st)
	}
	if got, err := s.ListUnrecordedLaunches(ctx); err != nil || len(got) != 0 {
		t.Fatalf("ListUnrecordedLaunches = %+v, %v", got, err)
	}
}
