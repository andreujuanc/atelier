package tui

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/andreujuanc/atelier/internal/app"
	"github.com/andreujuanc/atelier/internal/model"
)

// maxEditLines is the most lines the notes editor takes: the textarea
// silently drops lines past its own limit (10000), which a save would then
// lose.
const maxEditLines = 9000

// notesAsk is a pending y/n question in the notes editor.
type notesAsk int

const (
	askNone    notesAsk = iota
	askDiscard          // esc with unsaved changes
	askQuit             // ctrl+c with unsaved changes
)

// notesEditor is the in-place notes editor (`m`). It stays bound to the
// task it was opened on, whatever the selection does meanwhile.
type notesEditor struct {
	id   int64
	slug string
	// task is the task as it was when editing started; the details pane
	// shows it (refreshed from the list when the task is still there)
	// while editing.
	task app.TaskView
	ta   textarea.Model
	// base is the stored text the next save compares against (the text the
	// editor started from, or the newer text a conflict reported).
	base string
	// start is the editor's value when loaded: unsaved changes = the value
	// differs from it.
	start string
	// conflict: the last save found the notes changed meanwhile; the next
	// ctrl+s overwrites them (compares against the text it reported).
	conflict bool
	saving   bool // a save is in flight
	// sending is the text the save in flight sends: while saving, unsaved
	// changes = the value differs from it.
	sending string
	loading bool // a ctrl+r reload is in flight
	// ask is a pending y/n question: every key answers it (nothing reaches
	// the editor or normal mode); only an answer or closing the editor
	// clears it.
	ask notesAsk
}

// notesLoad is a pending `m`: the task whose notes are loading.
type notesLoad struct {
	id   int64
	slug string
}

func newNotesEditor(t app.TaskView, text string) *notesEditor {
	ta := textarea.New()
	ta.Prompt = ""
	ta.ShowLineNumbers = false
	ta.CharLimit = 0 // the store caps the size in bytes (model.MaxNotesLen)
	ta.MaxHeight = 0 // no line limit (and any height)
	ta.MaxWidth = 0
	ta.Placeholder = "notes for this task (ctrl+s saves, esc leaves)"
	plain := lipgloss.NewStyle()
	ta.FocusedStyle.Base, ta.FocusedStyle.CursorLine, ta.FocusedStyle.Text = plain, plain, plain
	ta.FocusedStyle.Placeholder = dimStyle
	ta.BlurredStyle = ta.FocusedStyle
	ta.Cursor.SetMode(cursor.CursorStatic)
	e := &notesEditor{id: t.Task.ID, slug: t.Task.Slug, task: t, ta: ta}
	e.ta.Focus()
	e.load(text)
	return e
}

// load replaces the editor's text with text (stored notes), cursor at the
// top, and makes it the base with no unsaved changes.
func (e *notesEditor) load(text string) {
	e.ta.SetValue(text)
	// Start at the top, like the notes area shows them.
	e.ta, _ = e.ta.Update(tea.KeyMsg{Type: tea.KeyCtrlHome})
	e.base, e.start, e.conflict = text, e.ta.Value(), false
}

// dirty reports unsaved changes: edits beyond the save in flight, if any,
// else beyond the loaded (or last saved) text.
func (e *notesEditor) dirty() bool {
	if e.saving {
		return e.ta.Value() != e.sending
	}
	return e.ta.Value() != e.start
}

// openNotes starts editing task t's notes: their current text is loaded
// first (the editor opens when it arrives). Meanwhile the TUI is in
// modeNotesLoading, which swallows keys (a fast "mjs" must not run j and
// s) and esc cancels. Like the other operations on a task it is refused
// while the task is busy (normalKey checks that).
func (m *Model) openNotes(t *app.TaskView) tea.Cmd {
	if m.quitRequested {
		m.setStatus(quitText(len(m.busyOps())))
		return nil
	}
	id, slug, be, ctx := t.Task.ID, t.Task.Slug, m.be, m.ctx
	m.busy[id] = "notes " + slug
	m.notesLoad = &notesLoad{id: id, slug: slug}
	m.mode = modeNotesLoading
	return func() tea.Msg {
		text, err := be.GetNotes(ctx, slug)
		return notesLoadedMsg{id: id, slug: slug, text: text, err: err}
	}
}

func (m *Model) notesLoaded(msg notesLoadedMsg) tea.Cmd {
	if msg.reload {
		e := m.notes
		if e == nil || e.id != msg.id || !e.loading {
			return nil // the editor was left meanwhile
		}
		e.loading = false
		if msg.err != nil {
			m.setErr(fmt.Errorf("reload notes: %w", msg.err))
			return nil
		}
		if strings.Count(msg.text, "\n") >= maxEditLines {
			m.setStatusErr("reload notes: too many lines to edit here; use atl notes "+msg.slug+" --set", true)
			return nil
		}
		// A pending question stays: its answer still means the same.
		e.load(msg.text)
		if e.ask == askNone {
			m.setStatus("notes of " + msg.slug + " reloaded (your edits were dropped)" + tabsHint(msg.text))
		}
		return nil
	}
	delete(m.busy, msg.id)
	if l := m.notesLoad; l == nil || l.id != msg.id || m.mode != modeNotesLoading {
		return nil // cancelled (esc, ctrl+c) meanwhile
	}
	m.notesLoad = nil
	m.mode = modeNormal
	if msg.err != nil {
		m.setErr(fmt.Errorf("notes: %w", msg.err))
		return nil
	}
	if m.quitRequested || m.notes != nil { // defensive: the load state prevents both
		m.setStatus("notes " + msg.slug + ": press m again to edit")
		return nil
	}
	if strings.Count(msg.text, "\n") >= maxEditLines {
		m.setStatusErr("notes: too many lines to edit here; use atl notes "+msg.slug+" --set", true)
		return nil
	}
	tv := app.TaskView{Task: model.Task{ID: msg.id, Slug: msg.slug, Title: msg.slug}}
	for _, t := range m.tasks {
		if t.Task.ID == msg.id {
			tv = t
		}
	}
	m.notes = newNotesEditor(tv, msg.text)
	m.mode = modeNotes
	m.setStatus(strings.TrimPrefix(tabsHint(msg.text), "; "))
	m.syncNotesSize()
	return nil
}

// tabsHint warns (as "; …") when text has characters a save from the
// editor changes: the textarea turns tabs into four spaces and drops
// U+FFFD (what invalid UTF-8 was stored as). "" if there are none.
func tabsHint(text string) string {
	if !strings.ContainsAny(text, "\t\uFFFD") {
		return ""
	}
	return "; note: saving here turns tabs into 4 spaces and drops \uFFFD characters (atl notes --set keeps them)"
}

// cancelNotesLoad leaves modeNotesLoading; the load's result is dropped
// when it arrives (the task stays busy until then).
func (m *Model) cancelNotesLoad() {
	m.notesLoad = nil
	m.mode = modeNormal
}

// notesLoadingKey handles a key while the notes are loading: esc cancels,
// every other key is swallowed (ctrl+c is handled by handleKey).
func (m *Model) notesLoadingKey(msg tea.KeyMsg) tea.Cmd {
	if msg.String() == "esc" {
		slug := ""
		if m.notesLoad != nil {
			slug = m.notesLoad.slug
		}
		m.cancelNotesLoad()
		m.setStatus("notes " + slug + ": cancelled")
	}
	return nil
}

// closeNotes leaves edit mode (any unsaved text and pending question are
// dropped with the editor).
func (m *Model) closeNotes() {
	m.notes = nil
	m.mode = modeNormal
}

// notesKey handles a key while the notes editor is focused: every key goes
// to the editor except ctrl+s (save), ctrl+r (reload), esc (leave) and the
// answer to a pending question.
func (m *Model) notesKey(msg tea.KeyMsg) tea.Cmd {
	e := m.notes
	if e == nil {
		m.mode = modeNormal
		return nil
	}
	k := msg.String()
	if e.ask != askNone {
		ask := e.ask
		e.ask = askNone
		if k != "y" && k != "Y" {
			if !m.statusErr { // keep a save error that arrived meanwhile
				m.setStatus("back to editing the notes of " + e.slug)
			}
			return nil
		}
		m.closeNotes()
		if ask == askQuit {
			return m.requestQuit("q")
		}
		if e.saving {
			m.setStatus("notes of " + e.slug + ": newer edits discarded; the save in progress goes on")
		} else {
			m.setStatus("notes of " + e.slug + ": changes discarded")
		}
		return nil
	}
	switch k {
	case "ctrl+s":
		return m.saveNotes()
	case "ctrl+r":
		if e.saving {
			m.setStatus("saving the notes of " + e.slug + "…")
			return nil
		}
		e.loading = true
		id, slug, be, ctx := e.id, e.slug, m.be, m.ctx
		return func() tea.Msg {
			text, err := be.GetNotes(ctx, slug)
			return notesLoadedMsg{id: id, slug: slug, text: text, reload: true, err: err}
		}
	case "esc":
		if e.dirty() {
			e.ask = askDiscard
			return nil
		}
		m.closeNotes()
		if e.saving {
			m.setStatus("notes of " + e.slug + ": saving…")
		} else {
			m.setStatus("notes of " + e.slug + ": no changes")
		}
		return nil
	}
	var cmd tea.Cmd
	e.ta, cmd = e.ta.Update(msg)
	return cmd
}

// notesQuit handles ctrl+c while editing: with unsaved changes it asks
// first (a second ctrl+c, or y, quits); without, it quits as usual.
func (m *Model) notesQuit() tea.Cmd {
	e := m.notes
	if e.dirty() && e.ask != askQuit {
		e.ask = askQuit
		return nil
	}
	m.closeNotes()
	return m.requestQuit("ctrl+c")
}

// notesUpdate passes non-key messages (cursor) to the editor.
func (m *Model) notesUpdate(msg tea.Msg) tea.Cmd {
	if m.notes == nil {
		return nil
	}
	var cmd tea.Cmd
	m.notes.ta, cmd = m.notes.ta.Update(msg)
	return cmd
}

// saveNotes saves the editor's text with a compare-and-set against base.
func (m *Model) saveNotes() tea.Cmd {
	e := m.notes
	if e.saving || e.loading {
		m.setStatus("wait: the notes of " + e.slug + " are being saved or reloaded…")
		return nil
	}
	if op, busy := m.busy[e.id]; busy {
		m.setStatus("busy: " + op + "… (ctrl+s again to save)")
		return nil
	}
	e.saving, e.sending = true, e.ta.Value()
	id, slug, text, base, be, ctx := e.id, e.slug, e.sending, e.base, m.be, m.ctx
	m.busy[id] = "save notes " + slug
	return func() tea.Msg {
		saved, err := be.SetNotes(ctx, slug, text, base)
		return notesSavedMsg{id: id, slug: slug, sent: text, saved: saved, err: err}
	}
}

func (m *Model) notesSaved(msg notesSavedMsg) tea.Cmd {
	delete(m.busy, msg.id)
	e := m.notes
	if e == nil || e.id != msg.id {
		// The editor was left while saving.
		if msg.err != nil {
			m.setErr(fmt.Errorf("save notes of %s (editor closed, text not saved): %w", msg.slug, msg.err))
		} else {
			m.setStatus("notes of " + msg.slug + " saved")
		}
		return m.reload()
	}
	e.saving = false
	var conflict *model.NotesConflictError
	switch {
	case errors.As(msg.err, &conflict):
		e.base, e.conflict = conflict.Current, true
		m.setStatusErr("notes of "+msg.slug+" changed meanwhile (an agent appended?): ctrl+s overwrites with your text, ctrl+r reloads theirs (drops your edits)", true)
		return m.reload()
	case msg.err != nil:
		m.setErr(fmt.Errorf("save notes: %w", msg.err))
		return nil
	}
	e.base, e.conflict = msg.saved, false
	if e.ta.Value() != msg.sent || e.ask != askNone {
		// Typed on while saving: keep editing what is left unsaved. A
		// question asked meanwhile (about those newer edits) stays open:
		// closing under it would let its answer leak into normal mode.
		e.start = msg.sent
		if e.ask == askNone {
			m.setStatus("notes of " + msg.slug + " saved; newer edits not saved yet (ctrl+s)")
		}
		return m.reload()
	}
	m.closeNotes()
	m.setStatus("notes of " + msg.slug + " saved")
	return m.reload()
}

// notesTask is the task the details pane shows: the one being edited
// (fresh from the list if it is there), else the selected one.
func (m *Model) notesTask() *app.TaskView {
	e := m.notes
	if e == nil {
		return m.selected()
	}
	for i := range m.tasks {
		if m.tasks[i].Task.ID == e.id {
			e.task = m.tasks[i]
			break
		}
	}
	return &e.task
}

// detailPaneSize is the size of the details pane: the right pane, or the
// whole body on a narrow terminal (where it only shows while editing).
func (m *Model) detailPaneSize() (w, h int) {
	h = m.height - 2
	if h < 0 {
		h = 0
	}
	if m.width >= minSplitWidth {
		return m.width - listWidth(m.width) - 3, h
	}
	return m.width, h
}

// notesLayout splits a details pane of height h with nInfo detail lines:
// the notes area (a blank line, its header and notesH lines) takes the
// rest, at least minNotesLines (fewer detail lines are kept for it).
func notesLayout(nInfo, h int) (keepInfo, notesH int) {
	const minNotesLines = 3
	keepInfo = nInfo
	if h-nInfo-2 < minNotesLines {
		keepInfo = h - 2 - minNotesLines
		if keepInfo < 0 {
			keepInfo = 0
		}
	}
	notesH = h - keepInfo - 2
	if notesH < 1 {
		notesH = 1
	}
	return keepInfo, notesH
}

// syncNotesSize fits the editor to its area (after a resize, or when the
// details above it grow or shrink).
func (m *Model) syncNotesSize() {
	e := m.notes
	if e == nil {
		return
	}
	w, h := m.detailPaneSize()
	_, notesH := notesLayout(len(m.infoLines(m.notesTask(), w)), h)
	if w < 1 {
		w = 1
	}
	if e.ta.Width() != w {
		e.ta.SetWidth(w)
	}
	if e.ta.Height() != notesH {
		e.ta.SetHeight(notesH)
	}
}

// notesLines renders the notes area of task t (width w, n text lines):
// its header, then the editor when t is being edited, else the notes text
// wrapped and from the top.
func (m *Model) notesLines(t *app.TaskView, w, n int) []string {
	out := []string{""}
	e := m.notes
	if e != nil && e.id == t.Task.ID {
		hdr := headerStyle.Render("Notes") + " "
		switch {
		case e.conflict:
			hdr += errStyle.Render("changed meanwhile: ctrl+s overwrite · ctrl+r reload")
		case e.saving && !e.dirty():
			hdr += markStyle.Render("saving…")
		case e.dirty():
			hdr += markStyle.Render("editing*") + dimStyle.Render(" · ctrl+s save · esc leave")
		default:
			hdr += markStyle.Render("editing") + dimStyle.Render(" · ctrl+s save · esc leave")
		}
		out = append(out, trunc(hdr, w))
		lines := strings.Split(strings.TrimSuffix(e.ta.View(), "\n"), "\n")
		if len(lines) > n {
			lines = lines[:n]
		}
		for _, l := range lines {
			out = append(out, trunc(l, w))
		}
		return out
	}
	out = append(out, headerStyle.Render("Notes"))
	if m.detail == nil || m.detail.Task.ID != t.Task.ID {
		return append(out, dimStyle.Render("…"))
	}
	text := notesDisplay(m.detail.Notes)
	if strings.TrimSpace(text) == "" {
		return append(out, dimStyle.Render(trunc("(no notes — press m to write)", w)))
	}
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if l == "" {
			lines = append(lines, "")
			continue
		}
		lines = append(lines, strings.Split(ansi.Wrap(l, w, ""), "\n")...)
	}
	if len(lines) > n {
		more := len(lines) - (n - 1)
		lines = append(lines[:n-1], dimStyle.Render(fmt.Sprintf("… %d more line(s) — m to edit and scroll", more)))
	}
	for _, l := range lines {
		out = append(out, trunc(l, w))
	}
	return out
}

// notesDisplay makes notes text safe to render: tabs become four spaces
// (as in the editor), other control characters except newlines are
// dropped, and trailing blank lines are trimmed.
func notesDisplay(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\t", "    ")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, s)
	return strings.TrimRightFunc(s, unicode.IsSpace)
}
