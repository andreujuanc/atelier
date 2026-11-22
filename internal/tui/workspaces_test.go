package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/andreujuanc/atelier/internal/app"
	"github.com/andreujuanc/atelier/internal/workspace"
)

// fakeWorkspaces backs a selector: Load returns rows, Create adds one (or
// adopts a directory) unless createErr.
type fakeWorkspaces struct {
	rows      []app.WorkspaceInfo
	loads     int
	created   []string
	createErr error
	notes     []string
}

func (f *fakeWorkspaces) selector(preselect string) WorkspaceSelector {
	return WorkspaceSelector{
		Root: "/home/u/atelier/workspaces", Home: "/home/u", Preselect: preselect,
		Load: func(context.Context) ([]app.WorkspaceInfo, error) {
			f.loads++
			return append([]app.WorkspaceInfo(nil), f.rows...), nil
		},
		Create: func(name string) ([]string, error) {
			f.created = append(f.created, name)
			if f.createErr != nil {
				return nil, f.createErr
			}
			for i := range f.rows {
				if f.rows[i].Name == name {
					f.rows[i].Workspace = true
					return f.notes, nil
				}
			}
			f.rows = append(f.rows, wsRow(name, true, 0, 0))
			return f.notes, nil
		},
	}
}

func wsRow(name string, ws bool, open, waiting int) app.WorkspaceInfo {
	return app.WorkspaceInfo{Entry: workspace.Entry{Name: name, Dir: "/home/u/atelier/workspaces/" + name, Workspace: ws}, OpenTasks: open, Waiting: waiting}
}

// drainWS runs cmd and feeds the resulting messages back into m; it
// reports whether one of them was tea.Quit.
func drainWS(m *WorkspacePicker, cmd tea.Cmd) bool {
	quit := false
	queue := []tea.Cmd{cmd}
	for n := 0; len(queue) > 0 && n < 100; n++ {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		switch msg := c().(type) {
		case nil:
		case tea.BatchMsg:
			queue = append(queue, msg...)
		case tea.QuitMsg:
			quit = true
		default:
			_, next := m.Update(msg)
			queue = append(queue, next)
		}
	}
	return quit
}

func newWSTest(t *testing.T, f *fakeWorkspaces, preselect string) *WorkspacePicker {
	t.Helper()
	m := NewWorkspacePicker(context.Background(), f.selector(preselect))
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	drainWS(m, m.Init())
	return m
}

// wsKey sends key k and reports whether the selector quit.
func wsKey(m *WorkspacePicker, k string) bool {
	_, cmd := m.Update(keyMsg(k))
	return drainWS(m, cmd)
}

func wsView(m *WorkspacePicker) string { return ansi.Strip(m.View()) }

func sampleWorkspaces() []app.WorkspaceInfo {
	broken := wsRow("broken", true, 0, 0)
	broken.Err = errors.New("file is not a database")
	return []app.WorkspaceInfo{
		broken,
		wsRow("beta", true, 2, 0),
		wsRow("alpha", true, 3, 1),
		wsRow("old", false, 0, 0),
	}
}

func TestWorkspacePickerView(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
	f := &fakeWorkspaces{rows: sampleWorkspaces()}
	m := newWSTest(t, f, "")
	v := wsView(m)
	for _, want := range []string{
		"atelier workspaces  ~/atelier/workspaces",
		"beta      2 open",
		"alpha      3 open  ● 1 waiting",
		"~/atelier/workspaces/alpha",
		"old     not a workspace (no atelier.toml)",
		"broken  database: file is not a database",
		"enter open  n new workspace  / filter",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	// Paths line up.
	col := -1
	for _, l := range strings.Split(v, "\n") {
		if j := strings.Index(l, "~/atelier/workspaces/"); j >= 0 {
			i := ansi.StringWidth(l[:j])
			if col >= 0 && i != col {
				t.Errorf("path column %d, want %d:\n%s", i, col, v)
			}
			col = i
		}
	}
	// The waiting count is drawn as the main TUI draws waiting sessions;
	// the non-workspace dimmed.
	raw := m.View()
	if !strings.Contains(raw, waitStyle.Render("  ● 1 waiting")) {
		t.Errorf("waiting not highlighted:\n%q", raw)
	}
	if !strings.Contains(raw, dimStyle.Render("old")) {
		t.Errorf("old not dimmed:\n%q", raw)
	}
	// No preselection: the first row.
	if r := m.selected(); r == nil || r.Name != "broken" {
		t.Fatalf("selected %v", r)
	}
	// Every size: no line wider than the screen, never more lines.
	for w := 0; w <= 60; w += 3 {
		for _, h := range []int{0, 1, 2, 4, 7} {
			m.Update(tea.WindowSizeMsg{Width: w, Height: h})
			v := m.View()
			if h > 0 && w > 0 && strings.Count(v, "\n")+1 > h {
				t.Fatalf("%dx%d: %d lines", w, h, strings.Count(v, "\n")+1)
			}
			for _, l := range strings.Split(v, "\n") {
				if ansi.StringWidth(l) > w {
					t.Fatalf("%dx%d: line too wide: %q", w, h, ansi.Strip(l))
				}
			}
		}
	}
}

func TestWorkspacePickerOpen(t *testing.T) {
	f := &fakeWorkspaces{rows: sampleWorkspaces()}
	m := newWSTest(t, f, "alpha")
	if r := m.selected(); r == nil || r.Name != "alpha" {
		t.Fatalf("preselected %v", r)
	}
	wsKey(m, "k")
	wsKey(m, "up")
	wsKey(m, "up") // stops at the top
	if r := m.selected(); r.Name != "broken" {
		t.Fatalf("after up: %s", r.Name)
	}
	wsKey(m, "jj") // one event, two moves
	if r := m.selected(); r.Name != "alpha" {
		t.Fatalf("after jj: %s", r.Name)
	}
	// The directory without atelier.toml: enter asks to adopt it; any
	// other key cancels, nothing is created.
	wsKey(m, "down")
	if wsKey(m, "enter") || m.mode != wsAdopt || m.Chosen() != "" {
		t.Fatalf("enter on old: mode %v chosen %q", m.mode, m.Chosen())
	}
	if v := wsView(m); !strings.Contains(v, "Adopt ~/atelier/workspaces/old (not a workspace: no atelier.toml)? y/n") || !strings.Contains(v, "`atl workspace new old` does") {
		t.Errorf("no adopt question:\n%s", v)
	}
	if wsKey(m, "n") || m.mode != wsNormal || len(f.created) != 0 {
		t.Fatalf("n did not cancel: mode %v, created %v", m.mode, f.created)
	}
	// A preselection that is gone: the first row.
	m = newWSTest(t, f, "gone")
	if r := m.selected(); r.Name != "broken" {
		t.Fatalf("preselected gone: %s", r.Name)
	}
	// enter on a workspace opens it (quits with its name).
	wsKey(m, "j")
	if !wsKey(m, "enter") || m.Chosen() != "beta" {
		t.Fatalf("enter: chosen %q", m.Chosen())
	}
	// q and ctrl+c quit without a choice.
	for _, k := range []string{"q", "ctrl+c"} {
		m = newWSTest(t, f, "")
		if !wsKey(m, k) || m.Chosen() != "" {
			t.Errorf("%s: chosen %q", k, m.Chosen())
		}
	}
}

func TestWorkspacePickerAdopt(t *testing.T) {
	f := &fakeWorkspaces{rows: sampleWorkspaces(), notes: []string{`tmux_socket = "default": …`}}
	m := newWSTest(t, f, "old")
	wsKey(m, "enter")
	if wsKey(m, "y") {
		t.Fatal("adopting quit")
	}
	if len(f.created) != 1 || f.created[0] != "old" || m.mode != wsNormal {
		t.Fatalf("created %v, mode %v", f.created, m.mode)
	}
	v := wsView(m)
	if !strings.Contains(v, "workspace old adopted — enter opens it; note: tmux_socket") || strings.Contains(v, "not a workspace") {
		t.Errorf("after adopt:\n%s", v)
	}
	if r := m.selected(); r.Name != "old" || !r.Workspace || f.loads != 2 {
		t.Fatalf("selected %+v, loads %d", r, f.loads)
	}
	if !wsKey(m, "enter") || m.Chosen() != "old" {
		t.Fatalf("open adopted: %q", m.Chosen())
	}
	// A failure is shown; back to the list.
	f = &fakeWorkspaces{rows: sampleWorkspaces(), createErr: errors.New("workspace path too long")}
	m = newWSTest(t, f, "old")
	wsKey(m, "enter")
	wsKey(m, "y")
	if m.mode != wsNormal || !strings.Contains(wsView(m), "workspace path too long") {
		t.Errorf("adopt error: mode %v\n%s", m.mode, wsView(m))
	}
}

func TestWorkspacePickerNew(t *testing.T) {
	f := &fakeWorkspaces{rows: sampleWorkspaces()}
	m := newWSTest(t, f, "")
	wsKey(m, "n")
	if m.mode != wsNew || !strings.Contains(wsView(m), "new workspace> ") {
		t.Fatalf("n: mode %v\n%s", m.mode, wsView(m))
	}
	// Letters typed go to the prompt, not to the list's keys.
	for _, c := range []struct{ typed, want string }{
		{"", "type the new workspace's name"},
		{"Bad", "invalid workspace name"},
		{"alpha", "workspace alpha already exists"},
	} {
		m.nameInput.SetValue(c.typed)
		if wsKey(m, "enter") || len(f.created) != 0 || m.mode != wsNew {
			t.Fatalf("%q: created %v, mode %v", c.typed, f.created, m.mode)
		}
		if v := wsView(m); !strings.Contains(v, c.want) {
			t.Errorf("%q: no %q:\n%s", c.typed, c.want, v)
		}
	}
	// Invalid while typing: said at once; an existing directory without
	// atelier.toml: it would be adopted.
	m.nameInput.SetValue("")
	typeWS(m, "X")
	if v := wsView(m); !strings.Contains(v, "invalid workspace name") {
		t.Errorf("typing X:\n%s", v)
	}
	m.nameInput.SetValue("")
	typeWS(m, "old")
	if v := wsView(m); !strings.Contains(v, "~/atelier/workspaces/old exists without atelier.toml: enter adopts it") {
		t.Errorf("typing old:\n%s", v)
	}
	// esc cancels.
	wsKey(m, "esc")
	if m.mode != wsNormal || len(f.created) != 0 {
		t.Fatalf("esc: mode %v", m.mode)
	}
	// A new one is created, listed and selected; q in the name is typed.
	wsKey(m, "n")
	typeWS(m, "aq-1")
	if wsKey(m, "enter") {
		t.Fatal("create quit")
	}
	if len(f.created) != 1 || f.created[0] != "aq-1" || m.mode != wsNormal {
		t.Fatalf("created %v, mode %v", f.created, m.mode)
	}
	if r := m.selected(); r == nil || r.Name != "aq-1" {
		t.Fatalf("selected %v", r)
	}
	if v := wsView(m); !strings.Contains(v, "workspace aq-1 created") || !strings.Contains(v, "aq-1    0 open") {
		t.Errorf("after create:\n%s", v)
	}
	// A failure keeps the prompt, with the error.
	f.createErr = errors.New("permission denied")
	wsKey(m, "n")
	typeWS(m, "zz")
	wsKey(m, "enter")
	if m.mode != wsNew || !strings.Contains(wsView(m), "permission denied") {
		t.Errorf("create error: mode %v\n%s", m.mode, wsView(m))
	}
}

func typeWS(m *WorkspacePicker, s string) {
	for _, r := range s {
		wsKey(m, string(r))
	}
}

func TestWorkspacePickerFilter(t *testing.T) {
	f := &fakeWorkspaces{rows: sampleWorkspaces()}
	m := newWSTest(t, f, "")
	wsKey(m, "/")
	typeWS(m, "e")
	if v := wsView(m); strings.Contains(v, "old ") || len(m.vis) != 3 || !strings.Contains(v, "beta ") || !strings.Contains(v, "alpha ") {
		t.Errorf("filter e:\n%s", v)
	}
	if r := m.selected(); r.Name != "broken" {
		t.Fatalf("filtered selection %s", r.Name)
	}
	wsKey(m, "down")
	wsKey(m, "down")
	wsKey(m, "enter") // apply
	if m.mode != wsNormal || m.filter != "e" || m.selected().Name != "alpha" {
		t.Fatalf("apply: mode %v filter %q sel %s", m.mode, m.filter, m.selected().Name)
	}
	if !strings.Contains(wsView(m), "filter: /e") {
		t.Errorf("no filter hint:\n%s", wsView(m))
	}
	wsKey(m, "esc") // clears, the selection stays
	if m.filter != "" || len(m.vis) != 4 || m.selected().Name != "alpha" {
		t.Fatalf("esc: filter %q, %d rows, sel %s", m.filter, len(m.vis), m.selected().Name)
	}
	wsKey(m, "/")
	typeWS(m, "zzz")
	if v := wsView(m); !strings.Contains(v, "no workspaces match /zzz") {
		t.Errorf("no match:\n%s", v)
	}
	wsKey(m, "enter")
	if wsKey(m, "enter") || m.Chosen() != "" {
		t.Fatal("enter with nothing shown opened something")
	}
}

func TestWorkspacePickerEmpty(t *testing.T) {
	f := &fakeWorkspaces{}
	m := NewWorkspacePicker(context.Background(), f.selector(""))
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	if v := wsView(m); !strings.Contains(v, "loading…") {
		t.Errorf("before load:\n%s", v)
	}
	drainWS(m, m.Init())
	if v := wsView(m); !strings.Contains(v, "no workspaces in ~/atelier/workspaces yet — press n to create one") {
		t.Errorf("empty:\n%s", v)
	}
	if wsKey(m, "enter") || m.Chosen() != "" {
		t.Fatal("enter on nothing")
	}
	wsKey(m, "n")
	typeWS(m, "first")
	wsKey(m, "enter")
	if r := m.selected(); r == nil || r.Name != "first" {
		t.Fatalf("selected %v", r)
	}
	// A list that cannot be read is said.
	m = NewWorkspacePicker(context.Background(), WorkspaceSelector{Root: "/r", Load: func(context.Context) ([]app.WorkspaceInfo, error) {
		return nil, fmt.Errorf("open /r: permission denied")
	}})
	drainWS(m, m.Init())
	if v := wsView(m); !strings.Contains(v, "cannot list the workspaces in /r: open /r: permission denied") {
		t.Errorf("load error:\n%s", v)
	}
}

// TestSwitchWorkspaceKey: w leaves the task TUI for the workspace
// selector like q quits (once no operation is in flight); q or ctrl+c
// meanwhile make it a plain quit. The workspace's name is on the status
// line.
func TestSwitchWorkspaceKey(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks()}
	m, _ := newTest(t, fb)
	m.Update(tea.WindowSizeMsg{Width: 200, Height: 40})
	if !strings.Contains(plainView(m), "w workspace") {
		t.Errorf("help lacks w:\n%s", plainView(m))
	}
	if !isQuit(press(m, "w")) || !m.SwitchRequested() {
		t.Fatal("idle w does not quit to switch")
	}

	m, _ = newTest(t, fb)
	m.workspace = "alpha"
	lines := strings.Split(plainView(m), "\n")
	if st := lines[len(lines)-2]; !strings.HasSuffix(st, "workspace alpha") || ansi.StringWidth(st) != 120 {
		t.Errorf("status line %q", st)
	}
	op := press(m, "s")
	if isQuit(press(m, "w")) || !m.quitRequested || !m.switching {
		t.Fatal("w quit with an op in flight")
	}
	if !strings.Contains(plainView(m), "switching workspace when they finish") {
		t.Errorf("no notice:\n%s", plainView(m))
	}
	if _, cmd := m.Update(op()); !isQuit(cmd) || !m.SwitchRequested() {
		t.Fatal("no switch after the op ended")
	}

	// q after w: a plain quit.
	m, _ = newTest(t, fb)
	op = press(m, "s")
	press(m, "w")
	press(m, "q")
	if _, cmd := m.Update(op()); !isQuit(cmd) || m.SwitchRequested() {
		t.Fatal("q after w still switches")
	}
	// ctrl+c after w forces a quit, no switch.
	m, _ = newTest(t, fb)
	press(m, "s")
	press(m, "w")
	if !isQuit(press(m, "ctrl+c")) || !m.Forced() || m.SwitchRequested() {
		t.Fatal("ctrl+c after w")
	}
	// w while in an input mode is typed, not a switch.
	m, _ = newTest(t, fb)
	key(m, "/")
	key(m, "w")
	if m.switching || m.filter != "w" {
		t.Fatalf("w in the filter: switching %v filter %q", m.switching, m.filter)
	}
}
