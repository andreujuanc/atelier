package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/andreujuanc/atelier/internal/model"
)

// checkView fails if a line of the view is wider than w or the view has
// more than h lines.
func checkView(t *testing.T, m *Model, w, h int) string {
	t.Helper()
	v := m.View()
	lines := strings.Split(v, "\n")
	if len(lines) != h {
		t.Errorf("view has %d lines, want %d", len(lines), h)
	}
	for _, l := range lines {
		if ansi.StringWidth(l) > w {
			t.Errorf("line wider than %d: %q", w, ansi.Strip(l))
		}
	}
	return ansi.Strip(v)
}

func TestNotesArea(t *testing.T) {
	long := strings.Repeat("word ", 60)
	fb := &fakeBackend{tasks: sampleTasks(), notesText: map[string]string{
		"alpha": "Goal: ship it\n\n\t- indented\x1b[31m red\n" + long,
	}}
	m, _ := newTest(t, fb)
	selectTask(t, m, "alpha")
	v := checkView(t, m, 120, 40)
	if !strings.Contains(v, "m edit notes") {
		t.Fatalf("help lacks m:\n%s", v)
	}
	sess := strings.Index(v, "Sessions")
	notes := strings.Index(v, "Notes")
	if sess < 0 || notes < sess {
		t.Fatalf("notes area not below the details:\n%s", v)
	}
	for _, want := range []string{"│ Goal: ship it", "│     - indented[31m red", "│ word word"} {
		if !strings.Contains(v, want) {
			t.Errorf("notes area lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(m.View(), "\x1b[31m red") {
		t.Fatal("unsanitized notes rendered")
	}
	// wrapped: the long line takes several rows, all within the pane
	if n := strings.Count(v, "│ word word"); n < 2 {
		t.Errorf("long line not wrapped (%d rows):\n%s", n, v)
	}

	// more lines than fit: the top is shown, then how many more
	var many []string
	for i := 0; i < 80; i++ {
		many = append(many, "line "+strings.Repeat("x", i%7))
	}
	fb.notesText["alpha"] = strings.Join(many, "\n")
	drain(m, m.reload())
	v = checkView(t, m, 120, 40)
	if !strings.Contains(v, "│ line \n") && !strings.Contains(v, "│ line") {
		t.Fatalf("top of the notes not shown:\n%s", v)
	}
	if !strings.Contains(v, "more line(s) — m to edit and scroll") {
		t.Fatalf("no 'more lines' marker:\n%s", v)
	}

	// no notes
	selectTask(t, m, "beta")
	if v := plainView(m); !strings.Contains(v, "(no notes — press m to write)") {
		t.Fatalf("empty notes:\n%s", v)
	}

	// short terminal: the notes area keeps a few lines, details are cut
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 12})
	selectTask(t, m, "alpha")
	v = checkView(t, m, 100, 12)
	if !strings.Contains(v, "Notes") || !strings.Contains(v, "│ line") {
		t.Fatalf("notes area lost on a short terminal:\n%s", v)
	}
}

func TestNotesEditSave(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks(), notesText: map[string]string{"gamma": "old text"}}
	m, _ := newTest(t, fb)
	selectTask(t, m, "gamma")

	key(m, "m")
	if fb.count("GetNotes") != 1 || m.mode != modeNotes || m.notes == nil || m.notes.ta.Value() != "old text" || m.notes.id != 3 {
		t.Fatalf("m: mode %v editor %+v", m.mode, m.notes)
	}
	if len(m.busy) != 0 {
		t.Fatalf("busy after opening: %v", m.busy)
	}
	v := checkView(t, m, 120, 40)
	for _, want := range []string{"Notes editing · ctrl+s save · esc leave", "│ old text", "ctrl+s save  esc leave (asks if unsaved)  ctrl+r reload"} {
		if !strings.Contains(v, want) {
			t.Errorf("edit view lacks %q:\n%s", want, v)
		}
	}

	// every key goes to the editor (also q, d, s, enter)
	key(m, "ctrl+end")
	key(m, "enter")
	typeText(m, "d s q")
	if m.mode != modeNotes || m.confirm != nil || fb.count("NewSession") != 0 || m.quitRequested {
		t.Fatal("keys leaked to normal mode")
	}
	if got := m.notes.ta.Value(); got != "old text\nd s q" {
		t.Fatalf("value = %q", got)
	}
	if v := plainView(m); !strings.Contains(v, "Notes editing*") || !strings.Contains(v, "│ d s q") {
		t.Fatalf("edited text / unsaved mark not shown:\n%s", v)
	}

	// ctrl+s saves (CAS against the text it started from), busy meanwhile
	cmd := press(m, "ctrl+s")
	if m.busy[3] != "save notes gamma" || !m.notes.saving {
		t.Fatalf("busy %v", m.busy)
	}
	drain(m, cmd)
	c := fb.last("SetNotes")
	if c == nil || c.args[0] != "gamma" || c.args[1] != "old text\nd s q" || c.args[2] != "old text" {
		t.Fatalf("SetNotes call %+v", c)
	}
	if m.mode != modeNormal || m.notes != nil || len(m.busy) != 0 || m.status != "notes of gamma saved" {
		t.Fatalf("after save: mode %v busy %v status %q", m.mode, m.busy, m.status)
	}
	if v := plainView(m); !strings.Contains(v, "│ old text") || !strings.Contains(v, "│ d s q") {
		t.Fatalf("saved notes not shown:\n%s", v)
	}

	// esc without changes leaves at once
	key(m, "m")
	key(m, "esc")
	if m.mode != modeNormal || !strings.Contains(m.status, "no changes") {
		t.Fatalf("esc: mode %v status %q", m.mode, m.status)
	}

	// esc with changes asks; n goes back (text kept), y discards
	key(m, "m")
	typeText(m, "Z")
	key(m, "esc")
	if m.mode != modeNotes || m.notes.ask != askDiscard {
		t.Fatalf("esc with changes: mode %v", m.mode)
	}
	if v := plainView(m); !strings.Contains(v, "discard your changes to the notes of gamma? y/n") {
		t.Fatalf("no discard question:\n%s", v)
	}
	key(m, "n")
	if m.mode != modeNotes || m.notes.ask != askNone || !strings.HasPrefix(m.notes.ta.Value(), "Z") {
		t.Fatalf("n: mode %v value %q", m.mode, m.notes.ta.Value())
	}
	key(m, "esc")
	key(m, "y")
	if m.mode != modeNormal || m.notes != nil || fb.count("SetNotes") != 1 || !strings.Contains(m.status, "discarded") {
		t.Fatalf("y: mode %v saves %d status %q", m.mode, fb.count("SetNotes"), m.status)
	}
	if fb.notesText["gamma"] != "old text\nd s q" {
		t.Fatalf("discard changed the notes: %q", fb.notesText["gamma"])
	}

	// save errors keep the editor open
	fb.setErr = errors.New("database is locked")
	key(m, "m")
	typeText(m, "E")
	key(m, "ctrl+s")
	if m.mode != modeNotes || !m.statusErr || !strings.Contains(m.status, "save notes: database is locked") || m.notes.saving {
		t.Fatalf("error: mode %v status %q", m.mode, m.status)
	}
	fb.setErr = nil
	key(m, "ctrl+s")
	if m.mode != modeNormal || !strings.HasPrefix(fb.notesText["gamma"], "E") {
		t.Fatalf("retry: mode %v notes %q", m.mode, fb.notesText["gamma"])
	}

	// load errors: no editor
	fb.notesErr = errors.New("boom")
	key(m, "m")
	if m.mode != modeNormal || !strings.Contains(m.status, "notes: boom") || len(m.busy) != 0 {
		t.Fatalf("load error: mode %v status %q busy %v", m.mode, m.status, m.busy)
	}
}

func TestNotesConflict(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks(), notesText: map[string]string{"gamma": "- 2026-01-01 [user] one"}}
	m, _ := newTest(t, fb)
	selectTask(t, m, "gamma")
	agent := "\n- 2026-01-02 [assistant] agent line"

	// an agent appends while the user edits: the save conflicts, the
	// editor stays open with the user's text
	key(m, "m")
	typeText(m, "mine ")
	fb.notesText["gamma"] += agent
	key(m, "ctrl+s")
	if m.mode != modeNotes || m.notes == nil || !m.notes.conflict || m.notes.ta.Value() != "mine - 2026-01-01 [user] one" {
		t.Fatalf("conflict: mode %v editor %+v", m.mode, m.notes)
	}
	if !m.statusErr || !strings.Contains(m.status, "changed meanwhile") || !strings.Contains(m.status, "ctrl+s overwrites") ||
		!strings.Contains(m.status, "ctrl+r reloads") {
		t.Fatalf("status %q", m.status)
	}
	v := plainView(m)
	if !strings.Contains(v, "ctrl+s overwrite with your text  ctrl+r reload theirs (drops your edits)") ||
		!strings.Contains(v, "changed meanwhile: ctrl+s overwrite") {
		t.Fatalf("conflict keys not shown:\n%s", v)
	}
	if fb.notesText["gamma"] != "- 2026-01-01 [user] one"+agent {
		t.Fatalf("conflict changed the notes: %q", fb.notesText["gamma"])
	}
	// a refresh meanwhile does not touch the editor
	drain(m, m.reload())
	if m.notes == nil || m.notes.ta.Value() != "mine - 2026-01-01 [user] one" {
		t.Fatal("refresh lost the user's text")
	}

	// second ctrl+s overwrites (compares against the text the conflict reported)
	key(m, "ctrl+s")
	c := fb.last("SetNotes")
	if c.args[2] != "- 2026-01-01 [user] one"+agent || m.mode != modeNormal || fb.notesText["gamma"] != "mine - 2026-01-01 [user] one" {
		t.Fatalf("overwrite: call %+v mode %v notes %q", c, m.mode, fb.notesText["gamma"])
	}

	// conflict, then ctrl+r reloads their text and drops the user's edits
	key(m, "m")
	typeText(m, "lost ")
	fb.notesText["gamma"] += agent
	key(m, "ctrl+s")
	if !m.notes.conflict {
		t.Fatal("no conflict")
	}
	key(m, "ctrl+r")
	if m.mode != modeNotes || m.notes.conflict || m.notes.dirty() || m.notes.ta.Value() != fb.notesText["gamma"] ||
		!strings.Contains(m.status, "reloaded") {
		t.Fatalf("reload: editor %q status %q", m.notes.ta.Value(), m.status)
	}
	key(m, "esc") // nothing unsaved: leaves without asking
	if m.mode != modeNormal {
		t.Fatalf("esc after reload: mode %v", m.mode)
	}
}

func TestNotesBoundToTask(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks(), notesText: map[string]string{"gamma": "g", "alpha": "a"}}
	m, _ := newTest(t, fb)
	selectTask(t, m, "gamma")
	key(m, "m")
	typeText(m, "edit ")

	// the selection moves (a task created meanwhile is selected) and the
	// list refreshes: the editor keeps its text and its task
	drain(m, func() tea.Msg { return newTaskMsg{task: &model.Task{ID: 1, Slug: "alpha"}} })
	if s := m.selected(); s == nil || s.Task.Slug != "alpha" {
		t.Fatalf("selection did not move: %+v", s)
	}
	m.Update(listTickMsg{})
	drain(m, m.reload())
	if m.mode != modeNotes || m.notes.slug != "gamma" || m.notes.ta.Value() != "edit g" {
		t.Fatalf("editor lost: mode %v %+v", m.mode, m.notes)
	}
	v := plainView(m)
	if !strings.Contains(v, "│ Title of gamma") || strings.Contains(v, "│ Title of alpha") {
		t.Fatalf("details pane not on the edited task:\n%s", v)
	}
	// the task vanishes from the list: still bound, still saves
	fb.tasks = fb.tasks[:2]
	drain(m, m.reload())
	if v := plainView(m); !strings.Contains(v, "│ Title of gamma") || !strings.Contains(v, "│ edit g") {
		t.Fatalf("edited task not shown:\n%s", v)
	}
	key(m, "ctrl+s")
	if c := fb.last("SetNotes"); c == nil || c.args[0] != "gamma" || fb.notesText["gamma"] != "edit g" || fb.notesText["alpha"] != "a" {
		t.Fatalf("saved to the wrong task: %+v", c)
	}
	if v := plainView(m); !strings.Contains(v, "│ Title of alpha") {
		t.Fatalf("details pane not back on the selection:\n%s", v)
	}
}

func TestNotesBusyAndTypingWhileSaving(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks(), notesText: map[string]string{"gamma": "g"}}
	m, _ := newTest(t, fb)
	selectTask(t, m, "gamma")

	// busy task: m refused, like other ops
	m.busy[3] = "pr gamma"
	key(m, "m")
	if m.mode != modeNormal || fb.count("GetNotes") != 0 || !strings.Contains(m.status, "busy: pr gamma") {
		t.Fatalf("busy: mode %v status %q", m.mode, m.status)
	}
	delete(m.busy, 3)

	// opening marks the task busy until the text arrives
	cmd := press(m, "m")
	if m.busy[3] != "notes gamma" {
		t.Fatalf("busy while loading: %v", m.busy)
	}
	drain(m, cmd)

	// typing on while a save is in flight: the newer text stays unsaved
	typeText(m, "1")
	cmd = press(m, "ctrl+s")
	if again := press(m, "ctrl+s"); again != nil || !strings.Contains(m.status, "being saved") {
		t.Fatalf("second save while saving: status %q", m.status)
	}
	typeText(m, "2")
	drain(m, cmd)
	if m.mode != modeNotes || !m.notes.dirty() || fb.notesText["gamma"] != "1g" || !strings.Contains(m.status, "newer edits not saved") {
		t.Fatalf("after save: mode %v notes %q status %q", m.mode, fb.notesText["gamma"], m.status)
	}
	key(m, "ctrl+s")
	if m.mode != modeNormal || fb.notesText["gamma"] != "12g" {
		t.Fatalf("second save: mode %v notes %q", m.mode, fb.notesText["gamma"])
	}
}

func TestNotesQuit(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks(), notesText: map[string]string{"gamma": "g"}}
	m, _ := newTest(t, fb)
	selectTask(t, m, "gamma")

	// no unsaved changes: ctrl+c quits
	key(m, "m")
	if !isQuit(press(m, "ctrl+c")) {
		t.Fatal("ctrl+c without changes did not quit")
	}

	// unsaved changes: ctrl+c asks; n goes back, y quits
	m, _ = newTest(t, fb)
	selectTask(t, m, "gamma")
	key(m, "m")
	typeText(m, "x")
	if isQuit(press(m, "ctrl+c")) || m.mode != modeNotes || m.notes.ask != askQuit {
		t.Fatal("ctrl+c with unsaved changes quit without asking")
	}
	if v := plainView(m); !strings.Contains(v, "quit and discard your changes to the notes of gamma? y/n") {
		t.Fatalf("no quit question:\n%s", v)
	}
	if isQuit(press(m, "n")) || m.mode != modeNotes || m.notes.ta.Value() != "xg" {
		t.Fatal("n did not go back to editing")
	}
	press(m, "ctrl+c")
	if !isQuit(press(m, "y")) {
		t.Fatal("y did not quit")
	}

	// a second ctrl+c also quits; esc's question turns into the quit one
	m, _ = newTest(t, fb)
	selectTask(t, m, "gamma")
	key(m, "m")
	typeText(m, "x")
	key(m, "esc")
	if isQuit(press(m, "ctrl+c")) || m.notes.ask != askQuit {
		t.Fatal("ctrl+c during the discard question")
	}
	if !isQuit(press(m, "ctrl+c")) {
		t.Fatal("second ctrl+c did not quit")
	}
	if fb.count("SetNotes") != 0 {
		t.Fatal("quitting saved")
	}
}

func TestNotesResize(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks(), notesText: map[string]string{"gamma": strings.Repeat("a long line of notes ", 20)}}
	m, _ := newTest(t, fb)
	selectTask(t, m, "gamma")
	key(m, "m")
	rw := 120 - listWidth(120) - 3
	if m.notes.ta.Width() != rw {
		t.Fatalf("editor width %d, want %d", m.notes.ta.Width(), rw)
	}
	checkView(t, m, 120, 40)

	for _, sz := range []struct{ w, h int }{{80, 30}, {60, 14}, {30, 20}, {120, 40}} {
		m.Update(tea.WindowSizeMsg{Width: sz.w, Height: sz.h})
		pw, ph := m.detailPaneSize()
		keep, notesH := notesLayout(len(m.infoLines(m.notesTask(), pw)), ph)
		if m.notes.ta.Width() != pw || m.notes.ta.Height() != notesH || keep+2+notesH != ph {
			t.Errorf("%dx%d: editor %dx%d, want %dx%d (keep %d)", sz.w, sz.h, m.notes.ta.Width(), m.notes.ta.Height(), pw, notesH, keep)
		}
		v := checkView(t, m, sz.w, sz.h)
		if !strings.Contains(v, "Notes editing") || !strings.Contains(v, "a long line") {
			t.Errorf("%dx%d: editor not shown:\n%s", sz.w, sz.h, v)
		}
	}
	// still editing the same text after all that
	if m.mode != modeNotes || m.notes.dirty() {
		t.Fatalf("resize changed the editor: mode %v dirty %v", m.mode, m.notes.dirty())
	}
}

// TestNotesLayout checks the split between details and the notes area.
func TestNotesLayout(t *testing.T) {
	for _, c := range []struct{ nInfo, h, keep, notesH int }{
		{10, 38, 10, 26},
		{10, 15, 10, 3},
		{10, 12, 7, 3},
		{10, 4, 0, 2},
		{0, 1, 0, 1},
	} {
		keep, notesH := notesLayout(c.nInfo, c.h)
		if keep != c.keep || notesH != c.notesH {
			t.Errorf("notesLayout(%d, %d) = %d, %d; want %d, %d", c.nInfo, c.h, keep, notesH, c.keep, c.notesH)
		}
	}
	if got := notesDisplay("a\tb\x1b[1m\r\nc\n\n"); got != "a    b[1m\nc" {
		t.Errorf("notesDisplay = %q", got)
	}
}
