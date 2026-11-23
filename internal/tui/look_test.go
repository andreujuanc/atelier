package tui

import (
	"image/color"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/andreujuanc/atelier/internal/app"
	"github.com/andreujuanc/atelier/internal/model"
)

// TestHelpOverlay: ? opens the key reference over the list; the next key
// only closes it, whatever it is.
func TestHelpOverlay(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks()}
	m, _ := newTest(t, fb)
	if !strings.Contains(plainView(m), "? all keys") {
		t.Fatalf("status line does not say where the keys are:\n%s", plainView(m))
	}
	key(m, "?")
	if m.mode != modeHelp {
		t.Fatalf("? mode %v", m.mode)
	}
	v := checkView(t, m, 120, 40)
	for _, want := range []string{"Keys", "rename the selected task", "switch workspace", "any key closes"} {
		if !strings.Contains(v, want) {
			t.Errorf("key reference lacks %q:\n%s", want, v)
		}
	}
	calls := len(fb.calls)
	key(m, "d") // closes; never asks to close the task
	if m.mode != modeNormal || m.confirm != nil || len(fb.calls) != calls || len(m.busy) != 0 {
		t.Fatalf("d after ?: mode %v confirm %v calls %v", m.mode, m.confirm, fb.calls[calls:])
	}
	// Every size: no line wider than the screen, never more lines.
	key(m, "?")
	for _, sz := range [][2]int{{1, 1}, {10, 5}, {30, 10}, {60, 14}, {80, 24}, {200, 60}} {
		m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		checkView(t, m, sz[0], sz[1])
	}
}

// TestSpinner: the busy spinner ticks only while an operation runs.
func TestSpinner(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks()}
	m, sched := newTest(t, fb)
	spins := func() int {
		n := 0
		for _, msg := range *sched {
			if _, ok := msg.(spinTickMsg); ok {
				n++
			}
		}
		return n
	}
	if spins() != 0 || m.spinning {
		t.Fatalf("spinner armed while idle: %d", spins())
	}
	selectTask(t, m, "gamma")
	op := press(m, "s") // the op runs: gamma busy
	if spins() != 1 || !m.spinning {
		t.Fatalf("spinner not armed: %d", spins())
	}
	frame := m.spinner()
	if !strings.Contains(plainView(m), frame+" working…") {
		t.Fatalf("no spinner on the status line:\n%s", plainView(m))
	}
	*sched = nil
	m.Update(spinTickMsg{})
	if spins() != 1 || m.spinner() == frame {
		t.Fatalf("tick while busy: rescheduled %d, frame %q", spins(), m.spinner())
	}
	drain(m, op)
	*sched = nil
	m.Update(spinTickMsg{})
	if spins() != 0 || m.spinning {
		t.Fatalf("tick after the op: rescheduled %d spinning %v", spins(), m.spinning)
	}
}

// TestOKStatus: an operation that worked is marked with ✓, an error with ✗.
func TestOKStatus(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks()}
	m, _ := newTest(t, fb)
	selectTask(t, m, "gamma")
	key(m, "s")
	if !m.statusOK || !strings.Contains(plainView(m), "✓ started") {
		t.Fatalf("no ✓: %q ok %v\n%s", m.status, m.statusOK, plainView(m))
	}
	m.setErr(errNotFound)
	if m.statusOK || !strings.Contains(plainView(m), "✗ ") {
		t.Fatalf("error status: ok %v\n%s", m.statusOK, plainView(m))
	}
	m.setStatus("plain")
	if v := plainView(m); strings.Contains(v, "✓ plain") || strings.Contains(v, "✗ plain") {
		t.Fatalf("plain status marked:\n%s", v)
	}
}

var errNotFound = &notFoundErr{}

type notFoundErr struct{}

func (*notFoundErr) Error() string { return "not found" }

// TestTheme: the terminal's background picks the light or dark colours.
func TestTheme(t *testing.T) {
	t.Cleanup(func() { applyTheme(true) })
	fb := &fakeBackend{tasks: sampleTasks()}
	m, _ := newTest(t, fb)
	dark := keyStyle.GetForeground()
	m.Update(tea.BackgroundColorMsg{Color: color.White})
	if isDark || keyStyle.GetForeground() == dark {
		t.Fatal("a light background kept the dark colours")
	}
	m.Update(tea.BackgroundColorMsg{Color: color.Black})
	if !isDark || keyStyle.GetForeground() != dark {
		t.Fatal("a dark background did not bring the dark colours back")
	}
}

// TestTopBar: the workspace, the session counts of open tasks and the
// number of open tasks; what does not fit is dropped from the left.
func TestTopBar(t *testing.T) {
	fb := &fakeBackend{tasks: []app.TaskView{
		tv(1, "a", model.StatusActive, sess(1, model.StateWaiting), sess(2, model.StateRunning)),
		tv(2, "b", model.StatusReview, sess(3, model.StateRunning), sess(4, model.StateStopped)),
		tv(3, "c", model.StatusDone, sess(5, model.StateRunning)), // closed: not counted
	}}
	m, _ := newTest(t, fb)
	m.workspace = "work"
	top := strings.Split(plainView(m), "\n")[0]
	for _, want := range []string{" atelier  work", "● 1 waiting", "● 2 running", "○ 1 stopped", "2 open tasks"} {
		if !strings.Contains(top, want) {
			t.Errorf("top bar lacks %q: %q", want, top)
		}
	}
	if strings.Contains(top, "idle") {
		t.Errorf("zero count shown: %q", top)
	}
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	top = strings.Split(plainView(m), "\n")[0]
	if ansi.StringWidth(top) > 40 || !strings.Contains(top, "open tasks") || strings.Contains(top, "waiting") {
		t.Errorf("narrow top bar %q", top)
	}
}

// TestPasteRouting: a paste goes to the input that has the keyboard and
// never acts in normal mode or a dialog.
func TestPasteRouting(t *testing.T) {
	paste := func(m *Model, s string) {
		_, cmd := m.Update(tea.PasteMsg{Content: s})
		drain(m, cmd)
	}
	fb := &fakeBackend{tasks: sampleTasks(), repos: []string{"api", "docs", "web"}}
	m, _ := newTest(t, fb)

	key(m, "n")
	paste(m, "Fix\nthe login")
	if got := m.form.inputs[fieldTitle].Value(); got != "Fix the login" {
		t.Fatalf("form title %q", got)
	}
	key(m, "down")
	key(m, "down") // Repos: not a text field
	paste(m, "zzz")
	if m.form.inputs[fieldRepos].Value() != "" || m.mode != modeForm {
		t.Fatal("paste on Repos")
	}
	key(m, " ") // the picker
	paste(m, " we b")
	if m.mode != modePicker || m.picker.filter != "web" || m.picker.current() != "web" {
		t.Fatalf("picker filter %q current %q", m.picker.filter, m.picker.current())
	}
	key(m, "esc")
	key(m, "esc")
	key(m, "esc")
	if m.mode != modeNormal {
		t.Fatalf("mode %v", m.mode)
	}

	key(m, "/")
	paste(m, "gam")
	if m.filter != "gam" || strings.Join(rowSlugs(m), " ") != "#active gamma" {
		t.Fatalf("filter %q rows %v", m.filter, rowSlugs(m))
	}
	key(m, "esc")

	selectTask(t, m, "gamma")
	key(m, "t")
	m.setStatusErr("rename: the title cannot be empty (esc cancels)", true)
	paste(m, " again")
	if m.statusErr || m.renameInput.Value() != "Title of gamma again" {
		t.Fatalf("rename: err %v value %q", m.statusErr, m.renameInput.Value())
	}
	key(m, "esc")

	fb.notesText = map[string]string{"gamma": ""}
	key(m, "m")
	paste(m, "line 1\nline 2")
	if got := m.notes.ta.Value(); got != "line 1\nline 2" {
		t.Fatalf("notes %q", got)
	}
	key(m, "esc") // unsaved: asks
	paste(m, "y")
	if m.notes == nil || m.notes.ask != askDiscard || m.notes.ta.Value() != "line 1\nline 2" {
		t.Fatal("paste answered the question or reached the editor")
	}
	key(m, "y")

	key(m, "?")
	paste(m, "d")
	if m.mode != modeHelp || fb.count("Done") != 0 {
		t.Fatal("paste acted in the key reference")
	}
}

// TestWorkspacePaste: the selector's filter and name inputs take pastes.
func TestWorkspacePaste(t *testing.T) {
	f := &fakeWorkspaces{rows: sampleWorkspaces()}
	m := newWSTest(t, f, "")
	m.Update(tea.PasteMsg{Content: "be"})
	if m.filter != "" {
		t.Fatal("paste filtered in normal mode")
	}
	wsKey(m, "/")
	m.Update(tea.PasteMsg{Content: "be"})
	if m.filter != "be" || len(m.vis) != 1 {
		t.Fatalf("filter %q vis %v", m.filter, m.vis)
	}
	wsKey(m, "esc")
	wsKey(m, "n")
	m.Update(tea.PasteMsg{Content: "new-ws"})
	if m.nameInput.Value() != "new-ws" {
		t.Fatalf("name %q", m.nameInput.Value())
	}
}

// TestSessionAges: a task row shows the age of its last session activity
// when there is room; closed tasks do not.
func TestSessionAges(t *testing.T) {
	s := sess(1, model.StateRunning)
	s.LastEventAt = t0.Add(time.Hour - 5*time.Minute)
	fb := &fakeBackend{tasks: []app.TaskView{tv(1, "alpha", model.StatusActive, s), tv(2, "shut", model.StatusDone, s)}}
	m, _ := newTest(t, fb)
	if l := strings.TrimRight(listLine(t, m, "alpha"), " "); !strings.HasSuffix(l, "5m") {
		t.Errorf("alpha row %q", l)
	}
	if l := strings.TrimRight(listLine(t, m, "shut"), " "); strings.HasSuffix(l, "5m") {
		t.Errorf("closed row %q", l)
	}
}
