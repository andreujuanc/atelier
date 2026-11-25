package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

type fakeUpdater struct {
	available  string
	checkErr   error
	installErr error
	checks     int
	installs   int
}

func (f *fakeUpdater) Available(context.Context) (string, error) {
	f.checks++
	return f.available, f.checkErr
}

func (f *fakeUpdater) Install(context.Context) (string, error) {
	f.installs++
	if f.installErr != nil {
		return "", f.installErr
	}
	return f.available, nil
}

// newUpdateTest is newTest with up as the model's Updater.
func newUpdateTest(t *testing.T, up Updater) (*Model, *[]tea.Msg) {
	t.Helper()
	fb := &fakeBackend{tasks: sampleTasks()}
	m := New(context.Background(), fb)
	m.updater = up
	var scheduled []tea.Msg
	m.after = func(_ time.Duration, msg tea.Msg) tea.Cmd {
		scheduled = append(scheduled, msg)
		return nil
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	drain(m, m.Init())
	return m, &scheduled
}

func headerOf(m *Model) string {
	return strings.SplitN(plainView(m), "\n", 2)[0]
}

func TestUpdateOffered(t *testing.T) {
	up := &fakeUpdater{available: "v0.2.0"}
	m, scheduled := newUpdateTest(t, up)
	if h := headerOf(m); !strings.Contains(h, "↑ atl v0.2.0 U update") {
		t.Errorf("header %q does not offer the update", h)
	}
	var ticks int
	for _, msg := range *scheduled {
		if _, ok := msg.(updateTickMsg); ok {
			ticks++
		}
	}
	if ticks != 1 {
		t.Errorf("%d update ticks armed, want 1", ticks)
	}
	// The tick checks again; the check arms the next tick.
	drain(m, func() tea.Msg { return updateTickMsg{} })
	if up.checks != 2 {
		t.Errorf("%d checks, want 2", up.checks)
	}
}

func TestUpdateNotOffered(t *testing.T) {
	for _, up := range []*fakeUpdater{{}, {available: "v0.2.0", checkErr: errors.New("offline")}} {
		m, _ := newUpdateTest(t, up)
		if h := headerOf(m); strings.Contains(h, "update") {
			t.Errorf("header %q offers an update", h)
		}
		key(m, "U")
		if m.status != "atl is up to date" || up.installs != 0 {
			t.Errorf("U: status %q, %d installs", m.status, up.installs)
		}
	}
}

func TestUpdateWithoutUpdater(t *testing.T) {
	m, _ := newTest(t, &fakeBackend{tasks: sampleTasks()})
	key(m, "U")
	if !strings.Contains(m.status, "atl update") {
		t.Errorf("status %q does not point at atl update", m.status)
	}
}

func TestUpdateRestarts(t *testing.T) {
	up := &fakeUpdater{available: "v0.2.0"}
	m, _ := newUpdateTest(t, up)
	key(m, "U")
	if up.installs != 1 {
		t.Fatalf("%d installs, want 1", up.installs)
	}
	if !m.RestartRequested() {
		t.Errorf("not restarting after the update (status %q)", m.status)
	}
}

// TestUpdateRestartWaits: an update that ends while a form is open, or an
// operation runs, restarts only once the TUI is back in normal mode with
// nothing running.
func TestUpdateRestartWaits(t *testing.T) {
	up := &fakeUpdater{available: "v0.2.0"}
	m, _ := newUpdateTest(t, up)
	cmd := press(m, "U")
	if !m.updating || !strings.Contains(m.statusLine(120), "update atl") {
		t.Fatalf("U: updating %v, status line %q", m.updating, m.statusLine(120))
	}
	key(m, "n") // the new-task form
	if m.mode != modeForm {
		t.Fatalf("mode %v, want the form", m.mode)
	}
	drain(m, cmd)
	if m.RestartRequested() {
		t.Fatal("restarted over the open form")
	}
	if h := headerOf(m); !strings.Contains(h, "atl v0.2.0, restarting") {
		t.Errorf("header %q", h)
	}
	m.busy[1] = "open alpha"
	key(m, "esc")
	if m.mode != modeNormal {
		t.Fatalf("mode %v after esc, want normal", m.mode)
	}
	if m.RestartRequested() {
		t.Fatal("restarted while an operation runs")
	}
	delete(m.busy, 1)
	m.Update(opMsg{id: 1, op: "open"})
	if !m.RestartRequested() {
		t.Error("no restart once nothing runs")
	}
}

func TestUpdateFails(t *testing.T) {
	up := &fakeUpdater{available: "v0.2.0", installErr: errors.New("its build attestation does not check out\ndetails")}
	m, _ := newUpdateTest(t, up)
	key(m, "U")
	if m.RestartRequested() || !m.statusErr || !strings.Contains(m.status, "update: its build attestation does not check out") {
		t.Errorf("restart %v, status %q (err %v)", m.RestartRequested(), m.status, m.statusErr)
	}
	if h := headerOf(m); !strings.Contains(h, "U update") {
		t.Errorf("header %q no longer offers the update to retry", h)
	}
}
