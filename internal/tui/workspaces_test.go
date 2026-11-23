package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/andreujuanc/atelier/internal/workspace"
)

const wsTestRoot = "/home/u/atelier/workspaces"

// fakeWorkspaces backs a selector: Load returns rows; Prepare plans a
// workspace (refused for the names in refuse), with settings, skipped and
// notes; Create adds one (or adopts a directory) unless createErr.
type fakeWorkspaces struct {
	rows      []workspace.Entry
	loads     int
	prepared  []string
	created   []string
	refuse    map[string]error
	createErr error
	settings  []workspace.PlannedSetting
	skipped   []string
	notes     []string
}

func (f *fakeWorkspaces) selector(preselect string) WorkspaceSelector {
	return WorkspaceSelector{
		Root: wsTestRoot, Home: "/home/u", Preselect: preselect,
		Load: func(context.Context) ([]workspace.Entry, error) {
			f.loads++
			return append([]workspace.Entry(nil), f.rows...), nil
		},
		Prepare: func(name string) (*workspace.Plan, error) {
			f.prepared = append(f.prepared, name)
			if err := f.refuse[name]; err != nil {
				return nil, err
			}
			adopt := false
			for _, r := range f.rows {
				adopt = adopt || r.Name == name
			}
			return &workspace.Plan{Name: name, Dir: wsTestRoot + "/" + name, Adopt: adopt, Settings: f.settings, Skipped: f.skipped, Notes: f.notes}, nil
		},
		Create: func(p *workspace.Plan) error {
			f.created = append(f.created, p.Name)
			if f.createErr != nil {
				return f.createErr
			}
			for i := range f.rows {
				if f.rows[i].Name == p.Name {
					f.rows[i].Workspace = true
					return nil
				}
			}
			f.rows = append(f.rows, wsRow(p.Name, true))
			return nil
		},
	}
}

func wsRow(name string, ws bool) workspace.Entry {
	return workspace.Entry{Name: name, Dir: wsTestRoot + "/" + name, Workspace: ws}
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

func wsView(m *WorkspacePicker) string { return ansi.Strip(m.View().Content) }

func sampleWorkspaces() []workspace.Entry {
	return []workspace.Entry{wsRow("alpha", true), wsRow("beta", true), wsRow("old", false)}
}

// TestWorkspacePickerView: a plain list, the name and the path; a
// directory without atelier.toml dimmed and marked not adopted.
func TestWorkspacePickerView(t *testing.T) {
	f := &fakeWorkspaces{rows: sampleWorkspaces()}
	m := newWSTest(t, f, "")
	v := wsView(m)
	for _, want := range []string{
		"atelier workspaces  ~/atelier/workspaces",
		" alpha  ~/atelier/workspaces/alpha ",
		" beta   ~/atelier/workspaces/beta\n",
		" old    ~/atelier/workspaces/old  not adopted\n",
		"enter open  n new workspace  / filter",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "open\n") || strings.Contains(v, "waiting") {
		t.Errorf("counts shown:\n%s", v)
	}
	// The path dimmed; the directory without atelier.toml dimmed.
	raw := m.View().Content
	if !strings.Contains(raw, dimStyle.Render("~/atelier/workspaces/beta")) || !strings.Contains(raw, dimStyle.Render("old")) ||
		!strings.Contains(raw, dimStyle.Render("  not adopted")) {
		t.Errorf("not dimmed:\n%q", raw)
	}
	// No preselection: the first row.
	if r := m.selected(); r == nil || r.Name != "alpha" {
		t.Fatalf("selected %v", r)
	}
	// Every size, also with the adopt confirmation: no line wider than
	// the screen, never more lines.
	for _, adopt := range []bool{false, true} {
		if adopt {
			f.settings = []workspace.PlannedSetting{{Key: "repos", Value: "~/src/work", From: workspace.DevcontainerFile}}
			f.skipped = []string{"CLAUDE_CODE_OAUTH_TOKEN: a secret, never written to atelier.toml (on the host, `atl token` stores it)"}
			m.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
			wsKey(m, "G")
			wsKey(m, "enter")
			if m.mode != wsAdopt {
				t.Fatalf("no confirmation: %v", m.mode)
			}
		}
		for w := 0; w <= 60; w += 3 {
			for _, h := range []int{0, 1, 2, 4, 7} {
				m.Update(tea.WindowSizeMsg{Width: w, Height: h})
				v := m.View().Content
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
}

func TestWorkspacePickerOpen(t *testing.T) {
	f := &fakeWorkspaces{rows: sampleWorkspaces()}
	m := newWSTest(t, f, "beta")
	if r := m.selected(); r == nil || r.Name != "beta" {
		t.Fatalf("preselected %v", r)
	}
	wsKey(m, "k")
	wsKey(m, "up") // stops at the top
	if r := m.selected(); r.Name != "alpha" {
		t.Fatalf("after up: %s", r.Name)
	}
	wsKey(m, "jj") // one event, two moves
	if r := m.selected(); r.Name != "old" {
		t.Fatalf("after jj: %s", r.Name)
	}
	// A preselection that is gone: the first row.
	m = newWSTest(t, f, "gone")
	if r := m.selected(); r.Name != "alpha" {
		t.Fatalf("preselected gone: %s", r.Name)
	}
	// enter on a workspace opens it (quits with its name).
	wsKey(m, "j")
	if !wsKey(m, "enter") || m.Chosen() != "beta" {
		t.Fatalf("enter: chosen %q", m.Chosen())
	}
	if len(f.prepared) != 0 {
		t.Errorf("opening prepared %v", f.prepared)
	}
	// q and ctrl+c quit without a choice.
	for _, k := range []string{"q", "ctrl+c"} {
		m = newWSTest(t, f, "")
		if !wsKey(m, k) || m.Chosen() != "" {
			t.Errorf("%s: chosen %q", k, m.Chosen())
		}
	}
}

// TestWorkspacePickerAdopt: enter on a directory without atelier.toml
// shows what adopting it writes (Prepare: nothing written yet); y adopts
// it, n and esc cancel, other keys do nothing.
func TestWorkspacePickerAdopt(t *testing.T) {
	f := &fakeWorkspaces{rows: sampleWorkspaces(), notes: []string{`tmux_socket = "default": …`},
		settings: []workspace.PlannedSetting{
			{Key: "repos", Value: "~/src/work", From: workspace.DevcontainerFile},
			{Key: "sandbox.mounts", Value: "~/.work.npmrc", From: workspace.DevcontainerFile},
		},
		skipped: []string{"ANTHROPIC_API_KEY: a secret, never written to atelier.toml"}}
	m := newWSTest(t, f, "old")
	for _, cancel := range []string{"n", "esc"} {
		if wsKey(m, "enter") || m.mode != wsAdopt || m.Chosen() != "" {
			t.Fatalf("enter on old: mode %v chosen %q", m.mode, m.Chosen())
		}
		v := wsView(m)
		for _, want := range []string{
			"Adopt ~/atelier/workspaces/old as workspace old?",
			"It writes atelier.toml (keeping what is there) with:",
			"  repos = ~/src/work (from .devcontainer/devcontainer.json)",
			"  sandbox.mounts = ~/.work.npmrc (from .devcontainer/devcontainer.json)",
			"  skipped ANTHROPIC_API_KEY: a secret",
			"  note: tmux_socket",
			"Adopt it? y/n",
			"y adopt (as `atl workspace new old` does)  n/esc cancel",
		} {
			if !strings.Contains(v, want) {
				t.Errorf("confirmation lacks %q:\n%s", want, v)
			}
		}
		if strings.Contains(v, "~/atelier/workspaces/alpha") {
			t.Errorf("the list under the confirmation:\n%s", v)
		}
		if wsKey(m, "x"); m.mode != wsAdopt {
			t.Fatal("x left the confirmation")
		}
		if wsKey(m, cancel) || m.mode != wsNormal || len(f.created) != 0 {
			t.Fatalf("%s did not cancel: mode %v, created %v", cancel, m.mode, f.created)
		}
	}
	wsKey(m, "enter")
	if wsKey(m, "y") {
		t.Fatal("adopting quit")
	}
	if len(f.created) != 1 || f.created[0] != "old" || m.mode != wsNormal {
		t.Fatalf("created %v, mode %v", f.created, m.mode)
	}
	v := wsView(m)
	if !strings.Contains(v, "workspace old adopted — enter opens it; note: tmux_socket") || strings.Contains(v, "not adopted") {
		t.Errorf("after adopt:\n%s", v)
	}
	if r := m.selected(); r.Name != "old" || !r.Workspace || f.loads != 2 {
		t.Fatalf("selected %+v, loads %d", r, f.loads)
	}
	if !wsKey(m, "enter") || m.Chosen() != "old" {
		t.Fatalf("open adopted: %q", m.Chosen())
	}
	// Nothing to write: said.
	f = &fakeWorkspaces{rows: sampleWorkspaces()}
	m = newWSTest(t, f, "old")
	wsKey(m, "enter")
	if v := wsView(m); !strings.Contains(v, "no settings: every one at its default") {
		t.Errorf("no settings:\n%s", v)
	}
	// A failure is shown; back to the list.
	f.createErr = errors.New("workspace path too long")
	wsKey(m, "y")
	if m.mode != wsNormal || !strings.Contains(wsView(m), "workspace path too long") {
		t.Errorf("adopt error: mode %v\n%s", m.mode, wsView(m))
	}
}

// TestWorkspacePickerRefused: when adopting would be refused (a box's
// workspace whose repos are not known), enter and n show why and how,
// and nothing is asked or written.
func TestWorkspacePickerRefused(t *testing.T) {
	const hint = "/home/u/atelier/workspaces/box has a database (a devcontainer box's workspace?) and no repos were found: adopt it with `atl workspace new box --repos DIR [--mount PATH]...`"
	f := &fakeWorkspaces{rows: append(sampleWorkspaces(), wsRow("box", false)), refuse: map[string]error{"box": errors.New(hint)}}
	m := newWSTest(t, f, "box")
	m.Update(tea.WindowSizeMsg{Width: 250, Height: 20})
	if wsKey(m, "enter") || m.mode != wsNormal || len(f.created) != 0 || !strings.Contains(wsView(m), hint) {
		t.Fatalf("enter on box: mode %v, created %v\n%s", m.mode, f.created, wsView(m))
	}
	wsKey(m, "y") // no question was asked
	if len(f.created) != 0 {
		t.Fatalf("y after the hint created %v", f.created)
	}
	wsKey(m, "n")
	typeWS(m, "box")
	if wsKey(m, "enter") || len(f.created) != 0 || m.mode != wsNew || !strings.Contains(wsView(m), hint) {
		t.Fatalf("n box: created %v, mode %v\n%s", f.created, m.mode, wsView(m))
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
	if v := wsView(m); !strings.Contains(v, "~/atelier/workspaces/old exists without atelier.toml: enter shows what adopting it writes") {
		t.Errorf("typing old:\n%s", v)
	}
	// enter: the adopt confirmation, as from the list.
	wsKey(m, "enter")
	if m.mode != wsAdopt || len(f.created) != 0 || !strings.Contains(wsView(m), "Adopt ~/atelier/workspaces/old as workspace old?") {
		t.Fatalf("n old: mode %v, created %v\n%s", m.mode, f.created, wsView(m))
	}
	wsKey(m, "y")
	if len(f.created) != 1 || !strings.Contains(wsView(m), "workspace old adopted — enter opens it") {
		t.Errorf("n old y: created %v\n%s", f.created, wsView(m))
	}
	// esc cancels.
	wsKey(m, "n")
	wsKey(m, "esc")
	if m.mode != wsNormal || len(f.created) != 1 {
		t.Fatalf("esc: mode %v", m.mode)
	}
	// A new one is created at once, listed and selected; q in the name is
	// typed.
	wsKey(m, "n")
	typeWS(m, "aq-1")
	if wsKey(m, "enter") {
		t.Fatal("create quit")
	}
	if len(f.created) != 2 || f.created[1] != "aq-1" || m.mode != wsNormal {
		t.Fatalf("created %v, mode %v", f.created, m.mode)
	}
	if r := m.selected(); r == nil || r.Name != "aq-1" {
		t.Fatalf("selected %v", r)
	}
	if v := wsView(m); !strings.Contains(v, "workspace aq-1 created") || !strings.Contains(v, "aq-1   ~/atelier/workspaces/aq-1") {
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
	typeWS(m, "a")
	if v := wsView(m); strings.Contains(v, "old ") || len(m.vis) != 2 || !strings.Contains(v, "alpha ") || !strings.Contains(v, "beta ") {
		t.Errorf("filter a:\n%s", v)
	}
	if r := m.selected(); r.Name != "alpha" {
		t.Fatalf("filtered selection %s", r.Name)
	}
	wsKey(m, "down")
	wsKey(m, "enter") // apply
	if m.mode != wsNormal || m.filter != "a" || m.selected().Name != "beta" {
		t.Fatalf("apply: mode %v filter %q sel %s", m.mode, m.filter, m.selected().Name)
	}
	if !strings.Contains(wsView(m), "filter: /a") {
		t.Errorf("no filter hint:\n%s", wsView(m))
	}
	wsKey(m, "esc") // clears, the selection stays
	if m.filter != "" || len(m.vis) != 3 || m.selected().Name != "beta" {
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
	m = NewWorkspacePicker(context.Background(), WorkspaceSelector{Root: "/r", Load: func(context.Context) ([]workspace.Entry, error) {
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
	// The status line, above the help (two lines at 120 columns).
	if st := lines[len(lines)-3]; !strings.HasSuffix(st, "workspace alpha") || ansi.StringWidth(st) != 120 {
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
