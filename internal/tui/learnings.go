package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/andreujuanc/atelier/internal/app"
	"github.com/andreujuanc/atelier/internal/learn"
	"github.com/andreujuanc/atelier/internal/store"
)

// The learnings view (L) reviews what atl learn proposes, as `atl
// learnings` does: accept, reject, edit, rm. l runs atl learn itself on
// the terminal (learnExec), as D runs atl diff.

// learnView is the learnings view's state; nil while it is closed.
type learnView struct {
	// all: the candidates and the rejected are shown too (h), as
	// `atl learnings ls --all`.
	all     bool
	views   []app.LearningView // every learning, as last loaded
	items   []app.LearningView // those shown, in order (shownLearnings)
	loaded  bool
	loading bool
	// reload: the load in flight was asked for before a change; its
	// result is dropped for a fresh one.
	reload bool
	sel    int   // index in items
	selID  int64 // the selected learning (kept across reloads)
	offset int   // the first row shown
	// detail is the occurrences of a learning (the selected one once
	// loaded); detailSeq numbers the requests (older replies are dropped).
	detail    *learnDetail
	detailSeq int
	// edit is the edit box (e); nil when not editing.
	edit *learnEdit
}

// learnDetail is a learning's occurrences, newest first.
type learnDetail struct {
	id   int64
	occs []store.LearningOccurrence
	err  string
}

// learnEdit is the box a learning's text is edited in.
type learnEdit struct {
	id   int64
	orig string
	ta   textarea.Model
	err  string // why the last save was refused ("" if none)
	// askQuit: ctrl+c was pressed with unsaved changes; the next key
	// answers (y quits).
	askQuit bool
	w       int // the width the box was last fitted to
}

// askState is a yes/no question in a dialog: y or enter runs yes, n or
// esc does nothing (cancel is said on the status line); other keys are
// ignored. back is the mode it returns to.
type askState struct {
	title  string
	text   []string // paragraphs
	yesKey string   // what confirming does, for the keys line
	cancel string
	danger bool // drawn in the error colour (it loses something)
	back   mode
	yes    func() tea.Cmd
}

type (
	learningsMsg struct {
		views []app.LearningView
		err   error
	}
	learningMsg struct {
		seq  int
		id   int64
		occs []store.LearningOccurrence
		err  error
	}
	// learnChangedMsg ends an accept, reject, edit or rm of learning id.
	learnChangedMsg struct {
		op string
		id int64
		// next is the learning to select once it worked (0: keep id).
		next int64
		text string
		err  error
	}
	learnCountMsg struct {
		n   int
		err error
	}
)

// learnOrder is the order the view lists statuses in: what to review
// first (as `atl learnings`).
var learnOrder = map[string]int{learn.StatusProposed: 0, learn.StatusAccepted: 1, learn.StatusCandidate: 2, learn.StatusRejected: 3}

// shownLearnings is vs as the view lists them: the proposed, then the
// accepted (all: then the candidates, then the rejected), each by score,
// highest first.
func shownLearnings(vs []app.LearningView, all bool) []app.LearningView {
	var out []app.LearningView
	for _, v := range vs {
		if all || v.Status == learn.StatusProposed || v.Status == learn.StatusAccepted {
			out = append(out, v)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if learnOrder[a.Status] != learnOrder[b.Status] {
			return learnOrder[a.Status] < learnOrder[b.Status]
		}
		if a.Score.Score != b.Score.Score {
			return a.Score.Score > b.Score.Score
		}
		return a.ID < b.ID
	})
	return out
}

// --- commands ---

// countCmd counts the proposed learnings for the top bar, unless a count
// is in flight.
func (m *Model) countCmd() tea.Cmd {
	if m.counting {
		return nil
	}
	m.counting = true
	be, ctx := m.be, m.ctx
	return func() tea.Msg {
		n, err := be.ProposedLearnings(ctx)
		return learnCountMsg{n, err}
	}
}

// learnCounted keeps the count; a failed count keeps the last one (the
// top bar's hint is only a hint).
func (m *Model) learnCounted(msg learnCountMsg) tea.Cmd {
	m.counting = false
	if msg.err == nil {
		m.proposed = msg.n
	}
	return nil
}

// learnLoadCmd loads the learnings for the view; one in flight is asked
// to load again when it returns (its answer may predate a change).
func (m *Model) learnLoadCmd() tea.Cmd {
	lv := m.learn
	if lv == nil {
		return nil
	}
	if lv.loading {
		lv.reload = true
		return nil
	}
	lv.loading = true
	be, ctx := m.be, m.ctx
	return func() tea.Msg {
		vs, err := be.Learnings(ctx)
		return learningsMsg{vs, err}
	}
}

func (m *Model) learningsLoaded(msg learningsMsg) tea.Cmd {
	lv := m.learn
	if lv == nil {
		return nil // the view was closed meanwhile
	}
	lv.loading = false
	if lv.reload {
		lv.reload = false
		return m.learnLoadCmd()
	}
	if msg.err != nil {
		m.setErr(fmt.Errorf("learnings: %w", msg.err))
		return nil
	}
	lv.views, lv.loaded = msg.views, true
	m.learnRebuild()
	// Scores and occurrences may have changed: the details again.
	return m.learnDetailCmd()
}

// learnRebuild recomputes the rows shown and keeps the selection on the
// same learning, else near where it was.
func (m *Model) learnRebuild() {
	lv := m.learn
	lv.items = shownLearnings(lv.views, lv.all)
	found := -1
	for i, v := range lv.items {
		if v.ID == lv.selID {
			found = i
		}
	}
	if found < 0 {
		found = min(lv.sel, len(lv.items)-1)
	}
	lv.sel = max(found, 0)
	lv.selID = 0
	if v := lv.selected(); v != nil {
		lv.selID = v.ID
	}
}

// selected is the selected learning, nil if none.
func (lv *learnView) selected() *app.LearningView {
	if lv.sel >= 0 && lv.sel < len(lv.items) {
		return &lv.items[lv.sel]
	}
	return nil
}

// learnDetailCmd loads the occurrences of the selected learning.
func (m *Model) learnDetailCmd() tea.Cmd {
	lv := m.learn
	if lv == nil || lv.selID == 0 {
		return nil
	}
	lv.detailSeq++
	seq, id, be, ctx := lv.detailSeq, lv.selID, m.be, m.ctx
	return func() tea.Msg {
		_, occs, err := be.Learning(ctx, id)
		return learningMsg{seq: seq, id: id, occs: occs, err: err}
	}
}

func (m *Model) learningLoaded(msg learningMsg) tea.Cmd {
	lv := m.learn
	if lv == nil || msg.seq != lv.detailSeq {
		return nil // closed, or an older request
	}
	d := &learnDetail{id: msg.id}
	if msg.err != nil {
		d.err = firstLine(msg.err.Error())
	} else {
		d.occs = append([]store.LearningOccurrence(nil), msg.occs...)
		// Newest first (they come oldest first).
		sort.SliceStable(d.occs, func(i, j int) bool { return d.occs[i].At.After(d.occs[j].At) })
	}
	lv.detail = d
	return nil
}

// --- opening, closing, keys ---

// openLearnings opens the learnings view (L).
func (m *Model) openLearnings() tea.Cmd {
	if m.quitRequested {
		m.setStatus(m.quitText(m.opsRunning()))
		return nil
	}
	m.learn = &learnView{}
	m.mode = modeLearnings
	return m.learnLoadCmd()
}

// closeLearnings goes back to the task list (a change in flight goes on:
// its outcome is said on the status line).
func (m *Model) closeLearnings() tea.Cmd {
	m.learn = nil
	m.mode = modeNormal
	return m.countCmd()
}

// learnMove moves the selection by delta rows.
func (m *Model) learnMove(delta int) tea.Cmd {
	lv := m.learn
	if len(lv.items) == 0 {
		return nil
	}
	i := max(min(lv.sel+delta, len(lv.items)-1), 0)
	if i == lv.sel {
		return nil
	}
	lv.sel, lv.selID = i, lv.items[i].ID
	return m.learnDetailCmd()
}

// learnRowsHeight is how many learnings the list shows at once (for
// pgup/pgdn).
func (m *Model) learnRowsHeight() int {
	l := m.layout()
	_, listH, _ := m.learnSplit(l.bodyH, learnBoxed(l))
	return max(listH-1, 1)
}

// learningsKey handles a key in the learnings view.
func (m *Model) learningsKey(key string) tea.Cmd {
	lv := m.learn
	if lv == nil {
		m.mode = modeNormal
		return nil
	}
	switch key {
	case "esc", "q":
		return m.closeLearnings()
	case "j", "down":
		return m.learnMove(1)
	case "k", "up":
		return m.learnMove(-1)
	case "pgdown":
		return m.learnMove(m.learnRowsHeight())
	case "pgup":
		return m.learnMove(-m.learnRowsHeight())
	case "home":
		return m.learnMove(-len(lv.items))
	case "end":
		return m.learnMove(len(lv.items))
	case "r":
		m.setStatus("")
		return m.learnLoadCmd()
	case "h":
		lv.all = !lv.all
		m.learnRebuild()
		if lv.all {
			m.setStatus("showing the candidates and the rejected too (h hides them)")
		} else {
			m.setStatus("showing the proposed and the accepted (h shows the candidates and the rejected)")
		}
		return m.learnDetailCmd()
	case "?":
		m.mode = modeHelp
		return nil
	case "l":
		return m.askLearn(modeLearnings)
	}
	v := lv.selected()
	if v == nil {
		if len(key) == 1 && strings.Contains("axed", key) {
			m.setStatus("no learning selected")
		}
		return nil
	}
	switch key {
	case "a", "x", "e", "d":
		if m.learnBusy != "" {
			m.setStatus("busy: " + m.learnBusy + "…")
			return nil
		}
	}
	id, text := v.ID, v.Text
	switch key {
	case "a":
		if v.Status == learn.StatusAccepted || v.Status == learn.StatusRejected {
			m.setStatusErr(fmt.Sprintf("accept: learning %d is %s: only a proposed or candidate learning can be accepted", id, v.Status), true)
			return nil
		}
		be := m.be
		return m.learnChange("accept", id, func(ctx context.Context) (string, error) {
			return fmt.Sprintf("learning %d accepted: agents get it at their next start", id), be.DecideLearning(ctx, id, true)
		})
	case "x":
		if v.Status == learn.StatusRejected {
			m.setStatusErr(fmt.Sprintf("reject: learning %d is rejected already", id), true)
			return nil
		}
		be := m.be
		m.ask = &askState{
			title: "Reject learning " + strconv.FormatInt(id, 10),
			text: []string{"Reject \"" + clean(text) + "\"?",
				"It stays, as rejected: atl learn does not propose it again and agents never get it."},
			yesKey: "reject", cancel: "reject cancelled", danger: true, back: modeLearnings,
			yes: func() tea.Cmd {
				return m.learnChange("reject", id, func(ctx context.Context) (string, error) {
					return fmt.Sprintf("learning %d rejected", id), be.DecideLearning(ctx, id, false)
				})
			},
		}
		m.mode = modeAsk
		return nil
	case "d":
		be := m.be
		n := ""
		if d := lv.detail; d != nil && d.id == id && d.err == "" {
			n = fmt.Sprintf(" and its %d occurrence(s)", len(d.occs))
		}
		m.ask = &askState{
			title: "Delete learning " + strconv.FormatInt(id, 10),
			text: []string{"Delete \"" + clean(text) + "\"" + n + "?",
				"Its evidence comes back: what your tasks still say is analysed again by the next atl learn, " +
					"which may propose it again. To stop it being proposed, reject it (x) instead."},
			yesKey: "delete", cancel: "delete cancelled", danger: true, back: modeLearnings,
			yes: func() tea.Cmd {
				return m.learnChange("delete", id, func(ctx context.Context) (string, error) {
					k, err := be.DeleteLearning(ctx, id)
					return fmt.Sprintf("learning %d deleted (%d occurrence(s))", id, k), err
				})
			},
		}
		m.mode = modeAsk
		return nil
	case "e":
		m.openLearnEdit(id, text)
		return nil
	}
	return nil
}

// neighbour is the learning to select once learning id has left its
// section (accepted, rejected, deleted): the next row of the same status,
// else the previous one; 0 if it was the only one.
func (lv *learnView) neighbour(id int64) int64 {
	i := -1
	for j, v := range lv.items {
		if v.ID == id {
			i = j
		}
	}
	if i < 0 {
		return 0
	}
	st := lv.items[i].Status
	for j := i + 1; j < len(lv.items); j++ {
		if lv.items[j].Status == st {
			return lv.items[j].ID
		}
	}
	for j := i - 1; j >= 0; j-- {
		if lv.items[j].Status == st {
			return lv.items[j].ID
		}
	}
	return 0
}

// learnChange runs a change of learning id (op: what it does) on the
// backend; one at a time.
func (m *Model) learnChange(op string, id int64, fn func(ctx context.Context) (string, error)) tea.Cmd {
	m.learnBusy = fmt.Sprintf("%s learning %d", op, id)
	var next int64
	if op != "edit" && m.learn != nil {
		next = m.learn.neighbour(id)
	}
	ctx := m.ctx
	return func() tea.Msg {
		text, err := fn(ctx)
		return learnChangedMsg{op: op, id: id, next: next, text: text, err: err}
	}
}

// learnChanged reports a change: an edit refused stays in its box with
// the reason; anything else goes to the status line, and the view and
// the top bar's count are refreshed.
func (m *Model) learnChanged(msg learnChangedMsg) tea.Cmd {
	m.learnBusy = ""
	lv := m.learn
	if msg.err != nil {
		err := msg.err
		if errors.Is(err, store.ErrLearningStatus) {
			err = fmt.Errorf("not allowed from its status now (r refreshes the list)")
		}
		if msg.op == "edit" && lv != nil && lv.edit != nil && lv.edit.id == msg.id {
			lv.edit.err = firstLine(err.Error())
		}
		m.setErr(fmt.Errorf("%s learning %d: %w", msg.op, msg.id, err))
	} else {
		if msg.op == "edit" && lv != nil && lv.edit != nil && lv.edit.id == msg.id {
			m.closeLearnEdit()
		}
		if lv != nil && msg.next != 0 && lv.selID == msg.id {
			// Not after it to its new place: on to the next to review.
			lv.selID = msg.next
		}
		m.setOK(msg.text)
	}
	return tea.Batch(m.learnLoadCmd(), m.countCmd())
}

// openLearnEdit opens the edit box on learning id's text.
func (m *Model) openLearnEdit(id int64, text string) {
	ta := textarea.New()
	ta.Prompt = ""
	ta.ShowLineNumbers = false
	ta.CharLimit = 0 // EditLearning checks the length (and says so)
	ta.MaxHeight = 0
	ta.MaxWidth = 0
	// One line: enter does not break it (the box wraps it).
	ta.KeyMap.InsertNewline.SetEnabled(false)
	ta.Placeholder = "what agents should know (enter saves, esc cancels)"
	e := &learnEdit{id: id, orig: text, ta: ta}
	e.restyle()
	e.ta.Focus()
	e.ta.SetValue(text)
	e.ta.MoveToEnd()
	m.learn.edit = e
	m.mode = modeLearnEdit
	m.setStatus("")
	m.syncLearnEditSize()
}

func (e *learnEdit) restyle() {
	plain := lipgloss.NewStyle()
	bar := lipgloss.NewStyle().Foreground(primary)
	st := e.ta.Styles()
	st.Focused = textarea.StyleState{Base: plain, Text: plain, CursorLine: plain, Placeholder: dimStyle,
		EndOfBuffer: plain, Prompt: bar, LineNumber: plain, CursorLineNumber: plain}
	st.Blurred = st.Focused
	st.Cursor.Blink = false
	st.Cursor.Color = primary
	e.ta.SetStyles(st)
	e.ta.Prompt = "▎"
}

// dirty reports unsaved changes in the box.
func (e *learnEdit) dirty() bool { return e.ta.Value() != e.orig }

// learnEditQuit handles ctrl+c in the edit box: with unsaved changes it
// asks first (a second ctrl+c, or y, quits); without, it quits as usual.
func (m *Model) learnEditQuit() tea.Cmd {
	if m.learn != nil && m.learn.edit != nil {
		e := m.learn.edit
		if e.dirty() && !e.askQuit {
			e.askQuit = true
			return nil
		}
	}
	m.closeLearnEdit()
	return m.requestQuit("ctrl+c")
}

// closeLearnEdit leaves the edit box (its text is dropped).
func (m *Model) closeLearnEdit() {
	if m.learn != nil {
		m.learn.edit = nil
	}
	if m.mode == modeLearnEdit {
		m.mode = modeLearnings
	}
}

// learnEditKey handles a key in the edit box: ctrl+s saves, esc cancels,
// the rest edits.
func (m *Model) learnEditKey(msg tea.KeyPressMsg) tea.Cmd {
	var e *learnEdit
	if m.learn != nil {
		e = m.learn.edit
	}
	if e == nil {
		m.closeLearnEdit()
		return nil
	}
	k := msg.String()
	if e.askQuit {
		// The answer to "quit and drop the edit?": y quits, any other
		// key goes back to editing (as in the notes editor).
		e.askQuit = false
		if k == "y" || k == "Y" {
			m.closeLearnEdit()
			return m.requestQuit("q")
		}
		m.setStatus("back to editing learning " + strconv.FormatInt(e.id, 10))
		return nil
	}
	switch k {
	case "esc":
		m.closeLearnEdit()
		m.setStatus("edit cancelled")
		return nil
	case "ctrl+s", "enter":
		if m.learnBusy != "" {
			m.setStatus("busy: " + m.learnBusy + "… (" + k + " again to save)")
			return nil
		}
		text := e.ta.Value()
		if strings.TrimSpace(text) == strings.TrimSpace(e.orig) {
			m.closeLearnEdit()
			m.setStatus("edit: text unchanged")
			return nil
		}
		id, be := e.id, m.be
		e.err = ""
		return m.learnChange("edit", id, func(ctx context.Context) (string, error) {
			_, err := be.EditLearning(ctx, id, text)
			return fmt.Sprintf("learning %d edited", id), err
		})
	}
	if e.err != "" {
		e.err = "" // a refusal goes once the text changes
	}
	var cmd tea.Cmd
	e.ta, cmd = e.ta.Update(msg)
	return cmd
}

// learnEditPaste inserts pasted text into the edit box, on one line.
func (m *Model) learnEditPaste(msg tea.PasteMsg) tea.Cmd {
	if m.learn == nil || m.learn.edit == nil {
		return nil
	}
	e := m.learn.edit
	if e.askQuit {
		// A paste is not y: back to editing (not inserted).
		e.askQuit = false
		m.setStatus("back to editing learning " + strconv.FormatInt(e.id, 10))
		return nil
	}
	e.err = ""
	e.ta.InsertString(oneLine(msg.Content))
	return nil
}

// oneLine makes s one line: each run of whitespace and control characters
// (newlines, tabs) becomes one space.
func oneLine(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			if !space {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// learnEditHeight is the edit box's height at width w: room for the
// longest text allowed.
func learnEditHeight(w int) int {
	return min(max((learn.MaxLearningText+w-1)/max(w, 1), 2), 6)
}

// syncLearnEditSize fits the edit box to the details pane.
func (m *Model) syncLearnEditSize() {
	if m.learn == nil || m.learn.edit == nil {
		return
	}
	e := m.learn.edit
	w := max(m.learnDetailWidth()-1, 1) // the bar takes a column
	if e.w != w {
		e.ta.SetWidth(w)
		e.w = w
	}
	if h := learnEditHeight(w); e.ta.Height() != h {
		e.ta.SetHeight(h)
	}
}

// --- atl learn ---

// learnSummary says which models atl learn calls, at what effort.
func learnSummary(c learn.Config) string {
	pm, am := clean(c.PrecleanModel), clean(c.AnalyseModel)
	pe, ae := clean(c.PrecleanEffort), clean(c.AnalyseEffort)
	if pm == am {
		return fmt.Sprintf("%s (pre-clean at %s effort, analyse at %s)", pm, pe, ae)
	}
	return fmt.Sprintf("%s (pre-clean, %s effort) and %s (analyse, %s effort)", pm, pe, am, ae)
}

// askLearn asks before running atl learn (l): it calls a model and can
// take minutes. back is the mode the question returns to.
func (m *Model) askLearn(back mode) tea.Cmd {
	if m.quitRequested {
		m.setStatus(m.quitText(m.opsRunning()))
		return nil
	}
	c := m.be.LearnConfig()
	m.ask = &askState{
		title: "Run atl learn",
		text: []string{
			"Run atl learn? It calls " + learnSummary(c) + " for the tasks with new input. " +
				fmt.Sprintf("Each call costs money (at most $%.2f) and the run can take minutes.", c.MaxCallUSD),
			"It runs here, on the terminal, with each task's progress and cost; press enter at its end to come back. ctrl+c stops it.",
		},
		yesKey: "run", cancel: "atl learn not run", back: back,
		yes: func() tea.Cmd { return m.execLearn() },
	}
	m.mode = modeAsk
	return nil
}

// askKey handles a key in a yes/no dialog.
func (m *Model) askKey(key string) tea.Cmd {
	a := m.ask
	if a == nil {
		m.mode = modeNormal
		return nil
	}
	switch key {
	case "y", "Y", "enter":
		m.ask, m.mode = nil, a.back
		if m.quitRequested {
			// ctrl+c was pressed meanwhile: nothing new starts.
			m.setStatus(m.quitText(m.opsRunning()))
			return nil
		}
		return a.yes()
	case "n", "N", "esc":
		m.ask, m.mode = nil, a.back
		m.setStatus(a.cancel)
	}
	return nil
}

// execLearn runs atl learn on the terminal (learnExec) and comes back to
// the TUI when the user has read its output.
func (m *Model) execLearn() tea.Cmd {
	tail := &tailBuffer{max: 4 << 10}
	c := &learnExec{cmd: m.learnCmd(), stderr: tail}
	return guardedExec(m.execing, c, func(err error) tea.Msg {
		return execDoneMsg{op: "learn", stderr: bytes.NewBufferString(tail.String()), err: err}
	})
}

// learnDone reports an atl learn run (execDone) and refreshes what it may
// have changed.
func (m *Model) learnDone(msg execDoneMsg) tea.Cmd {
	if msg.err != nil {
		text := "learn: " + msg.err.Error()
		if msg.stderr != nil {
			if s := lastLine(msg.stderr.String()); s != "" {
				// atl says "atl: learn: …" itself: one "learn: ".
				text = "learn: " + strings.TrimPrefix(strings.TrimPrefix(s, "atl: "), "learn: ")
			}
		}
		m.setStatusErr(text, true)
	} else {
		m.setOK("atl learn finished")
	}
	return tea.Batch(m.countCmd(), m.learnLoadCmd(), m.reload())
}

// learnExec is a tea.ExecCommand: atl learn on the terminal, then a
// prompt that waits for enter, so that its output (each task's row, the
// cost, the new proposals) can be read before the TUI comes back. What it
// writes to stderr is also kept (its tail) for the status line.
type learnExec struct {
	cmd    *exec.Cmd
	stderr io.Writer
	in     io.Reader
	out    io.Writer
}

func (c *learnExec) SetStdin(r io.Reader) {
	c.in = r
	if c.cmd.Stdin == nil {
		c.cmd.Stdin = r
	}
}

func (c *learnExec) SetStdout(w io.Writer) {
	c.out = w
	if c.cmd.Stdout == nil {
		c.cmd.Stdout = w
	}
}

func (c *learnExec) SetStderr(w io.Writer) {
	if c.cmd.Stderr == nil {
		c.cmd.Stderr = io.MultiWriter(w, c.stderr)
	}
}

// Run runs atl learn, then waits for a line on the terminal.
func (c *learnExec) Run() error {
	err := c.cmd.Run()
	if c.out != nil && c.in != nil {
		what := "atl learn finished"
		if err != nil {
			what = "atl learn failed (" + err.Error() + ")"
		}
		fmt.Fprintf(c.out, "\n%s. Press enter to go back to atelier. ", what)
		readLine(c.in)
	}
	return err
}

// readLine reads r up to a newline or its end, a byte at a time (nothing
// past the newline is consumed).
func readLine(r io.Reader) {
	b := make([]byte, 1)
	for {
		n, err := r.Read(b)
		if err != nil || (n == 1 && (b[0] == '\n' || b[0] == '\r')) {
			return
		}
	}
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }

// --- drawing ---

// learnSplit is how the learnings view splits a body h rows high: the
// list's outer height (its box included when boxed) and its inner one
// (column header and rows), and the details' outer height.
func (m *Model) learnSplit(h int, boxed bool) (listOuter, listInner, detailOuter int) {
	n := 1
	if m.learn != nil {
		n = max(len(m.learn.items), 1)
	}
	want := n + 1 // the column header
	if boxed {
		listOuter = min(want+2, max(h/2, 4))
		listInner = listOuter - 2
	} else {
		listOuter = min(want, max(h/2, 1))
		listInner = listOuter
	}
	detailOuter = max(h-listOuter, 0)
	return listOuter, listInner, detailOuter
}

// learnBoxed reports whether the learnings view draws boxes.
func learnBoxed(l layout) bool { return l.boxed && l.bodyH >= 8 }

// learnDetailWidth is the width inside the details pane.
func (m *Model) learnDetailWidth() int {
	l := m.layout()
	if learnBoxed(l) {
		return max(m.width-4, 1)
	}
	return max(m.width, 1)
}

// learnBody draws the learnings view (exactly l.bodyH lines): the list,
// the selected learning's details below.
func (m *Model) learnBody(l layout, w int) []string {
	h := l.bodyH
	if h <= 0 {
		return nil
	}
	lv := m.learn
	boxed := learnBoxed(l)
	listOuter, listInner, detailOuter := m.learnSplit(h, boxed)
	editing := m.mode == modeLearnEdit
	var out []string
	if boxed {
		inner := m.learnListLines(w-4, listInner)
		out = append(out, panelB("Learnings", m.learnCounts(), m.learnScrollHint(listInner-1), inner, w, listOuter, !editing)...)
		title := ""
		if v := lv.selected(); v != nil {
			title = "Learning " + strconv.FormatInt(v.ID, 10)
		}
		out = append(out, panel(title, "", m.learnDetailLines(w-4, detailOuter-2), w, detailOuter, editing)...)
	} else {
		list := m.learnListLines(w, listInner)
		for i := 0; i < listOuter; i++ {
			out = append(out, at(list, i))
		}
		if detailOuter > 0 {
			out = append(out, rule("Details", w))
			out = append(out, m.learnDetailLines(w, detailOuter-1)...)
		}
	}
	for len(out) < h {
		out = append(out, "")
	}
	return out[:h]
}

// learnCounts is the list box's right title: how many of each status,
// and how many are hidden.
func (m *Model) learnCounts() string {
	lv := m.learn
	if !lv.loaded {
		return ""
	}
	n := map[string]int{}
	for _, v := range lv.views {
		n[v.Status]++
	}
	parts := []string{fmt.Sprintf("%d proposed", n[learn.StatusProposed]), fmt.Sprintf("%d accepted", n[learn.StatusAccepted])}
	if hidden := n[learn.StatusCandidate] + n[learn.StatusRejected]; !lv.all && hidden > 0 {
		parts = append(parts, fmt.Sprintf("%d hidden (h)", hidden))
	}
	return labelStyle.Render(strings.Join(parts, " · "))
}

// learnScrollHint says how many rows are above and below the visible
// ones (rows: how many fit).
func (m *Model) learnScrollHint(rows int) string {
	lv := m.learn
	if rows <= 0 || len(lv.items) == 0 {
		return ""
	}
	off := scrollOffset(lv.sel, lv.offset, len(lv.items), rows)
	var parts []string
	if off > 0 {
		parts = append(parts, "↑ "+strconv.Itoa(off))
	}
	if n := len(lv.items) - off - rows; n > 0 {
		parts = append(parts, "↓ "+strconv.Itoa(n))
	}
	if len(parts) == 0 {
		return ""
	}
	return dimStyle.Render(strings.Join(parts, "  ") + " more")
}

// Column widths of the learnings list.
const (
	lcStatus = 9
	lcScore  = 5
	lcTrend  = 5
	lcTasks  = 5
)

// learnFlags is what review must look at in a learning ("-" if nothing),
// as atl learnings shows it.
func learnFlags(v app.LearningView) string {
	var f []string
	if v.Outdated {
		f = append(f, "outdated")
	}
	if v.StillCorrected {
		f = append(f, "still-corrected")
	}
	if len(f) == 0 {
		return "-"
	}
	return strings.Join(f, ",")
}

// trendMark is a trend as an arrow: ↑ rising, ↓ fading, – about flat.
func trendMark(t float64) string {
	switch {
	case t > 0.05:
		return "↑"
	case t < -0.05:
		return "↓"
	}
	return "–"
}

// learnStatusStyle is how a learning's status is drawn.
func learnStatusStyle(status string) lipgloss.Style {
	switch status {
	case learn.StatusProposed:
		return markStyle
	case learn.StatusAccepted:
		return okStyle
	case learn.StatusRejected:
		return lipgloss.NewStyle().Foreground(faint)
	}
	return dimStyle
}

// learnListLines draws the list (w wide, h lines): a column header, then
// the rows around the selection.
func (m *Model) learnListLines(w, h int) []string {
	lv := m.learn
	if h <= 0 || w <= 0 {
		return nil
	}
	if !lv.loaded {
		return []string{dimStyle.Render(trunc("loading…", w))}
	}
	if len(lv.items) == 0 {
		msg := "no proposed or accepted learnings: h shows the candidates and the rejected, l runs atl learn"
		if lv.all || len(lv.views) == 0 {
			msg = "no learnings yet: l runs atl learn, which makes them from your tasks"
		}
		lines := wrapText(msg, w, w, dimStyle)
		return lines[:min(h, len(lines))]
	}
	flagsW := len("FLAGS")
	for _, v := range lv.items {
		flagsW = max(flagsW, len(learnFlags(v)))
	}
	col := func(s string, n int) string { return padRight(s, n) + "  " }
	out := []string{trunc("  "+labelStyle.Render(col("STATUS", lcStatus)+col("SCORE", lcScore)+col("TREND", lcTrend)+
		col("TASKS", lcTasks)+col("FLAGS", flagsW)+"TEXT"), w)}
	rows := h - 1
	if rows <= 0 {
		return out
	}
	lv.offset = scrollOffset(lv.sel, lv.offset, len(lv.items), rows)
	for i := lv.offset; i < min(lv.offset+rows, len(lv.items)); i++ {
		v := lv.items[i]
		textSt := lipgloss.NewStyle()
		if v.Status == learn.StatusCandidate || v.Status == learn.StatusRejected {
			textSt = dimStyle
		}
		trendSt := dimStyle
		switch trendMark(v.Trend) {
		case "↑":
			trendSt = okStyle
		case "↓":
			trendSt = warnStyle
		}
		flags := learnFlags(v)
		flagSt := dimStyle
		if flags != "-" {
			flagSt = warnStyle
		}
		out = append(out, rowText(i == lv.sel, w,
			seg{col(v.Status, lcStatus), learnStatusStyle(v.Status)},
			seg{col(fmt.Sprintf("%*.1f", lcScore, v.Score.Score), lcScore), lipgloss.NewStyle()},
			seg{col("  "+trendMark(v.Trend), lcTrend), trendSt},
			seg{col(fmt.Sprintf("%*d", lcTasks, v.Tasks), lcTasks), lipgloss.NewStyle()},
			seg{col(flags, flagsW), flagSt},
			seg{clean(v.Text), textSt}))
	}
	return out
}

// day is t's local date; "-" if zero.
func day(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02")
}

// learnDetailLines draws the selected learning (w wide, h lines): its
// text (or the edit box), its score and flags, then its occurrences,
// newest first, each quote wrapped.
func (m *Model) learnDetailLines(w, h int) []string {
	lv := m.learn
	if w <= 0 || h <= 0 {
		return nil
	}
	v := lv.selected()
	if v == nil {
		return nil
	}
	var out []string
	add := func(s string) { out = append(out, trunc(s, w)) }
	if e := lv.edit; e != nil && e.id == v.ID {
		hdr := keyStyle.Render("editing") + dimStyle.Render(" · enter save · esc cancel")
		if n := utf8.RuneCountInString(strings.TrimSpace(e.ta.Value())); n > learn.MaxLearningText {
			hdr += "  " + errStyle.Render(fmt.Sprintf("%d/%d", n, learn.MaxLearningText))
		} else {
			hdr += "  " + dimStyle.Render(fmt.Sprintf("%d/%d", n, learn.MaxLearningText))
		}
		add(hdr)
		for _, l := range strings.Split(strings.TrimSuffix(e.ta.View(), "\n"), "\n") {
			add(l)
		}
		if e.err != "" {
			out = append(out, wrapText("✗ "+clean(e.err), w, w, errStyle)...)
		}
	} else {
		out = append(out, wrapText(clean(v.Text), w, w, titleStyle)...)
	}
	sep := dimStyle.Render(" · ")
	meta := learnStatusStyle(v.Status).Render(v.Status) + sep +
		fmt.Sprintf("score %.1f", v.Score.Score) + dimStyle.Render(fmt.Sprintf(" (+%.1f, -%.1f)", v.Reinforce, v.Contradict)) + sep +
		fmt.Sprintf("trend %s %+.1f", trendMark(v.Trend), v.Trend) + sep +
		fmt.Sprintf("%d task(s)", v.Tasks) + sep +
		"last " + day(v.Last)
	out = append(out, wrapText(meta, w, w, lipgloss.NewStyle())...)
	if v.Outdated {
		out = append(out, wrapText("outdated: contradicted since you accepted it, from 2 or more tasks; reject or edit it", w, w, warnStyle)...)
	}
	if v.StillCorrected {
		out = append(out, wrapText("still-corrected: you still had to say it after accepting it", w, w, warnStyle)...)
	}
	var notes []string
	if v.Edited {
		notes = append(notes, "edited by you")
	}
	if !v.DecidedAt.IsZero() {
		notes = append(notes, "decided "+day(v.DecidedAt))
	}
	if len(notes) > 0 {
		add(dimStyle.Render(strings.Join(notes, " · ")))
	}

	d := lv.detail
	if d == nil || d.id != v.ID {
		add(rule("Occurrences", w))
		add(dimStyle.Render("…"))
		return clipDetail(out, h, w, v.ID)
	}
	if d.err != "" {
		add(rule("Occurrences", w))
		add(errStyle.Render("✗ " + clean(d.err)))
		return clipDetail(out, h, w, v.ID)
	}
	add(rule(fmt.Sprintf("Occurrences (%d)", len(d.occs)), w))
	if len(d.occs) == 0 {
		add(dimStyle.Render("none"))
	}
	unrelated := 0
	for _, o := range d.occs {
		pol := dimStyle
		if o.Polarity == "contradict" {
			pol = errStyle
		}
		line := day(o.At) + "  " + clean(o.TaskSlug) + "  " + dimStyle.Render(clean(o.Strength)) + "  " + pol.Render(clean(o.Polarity))
		if !learn.SharesWord(o.Quote, v.Text) {
			line += "  " + markStyle.Render("?")
			unrelated++
		}
		add(line)
		for _, q := range strings.Split(ansi.Wrap("\""+clean(o.Quote)+"\"", max(w-4, 8), ""), "\n") {
			add("    " + strings.TrimRight(q, " "))
		}
	}
	if unrelated > 0 {
		out = append(out, wrapText(fmt.Sprintf("? %d quote(s) share no word with the text: read them before you accept", unrelated), w, w, markStyle)...)
	}
	return clipDetail(out, h, w, v.ID)
}

// clipDetail cuts the details to h lines, the last saying where the rest
// is.
func clipDetail(out []string, h, w int, id int64) []string {
	if len(out) <= h {
		return out
	}
	more := len(out) - (h - 1)
	out = out[:h-1]
	return append(out, trunc(dimStyle.Render(fmt.Sprintf("… %d more line(s): atl learnings show %d", more, id)), w))
}

// learnHelp is the learnings view's keys.
func (m *Model) learnHelp() []helpItem {
	hidden := "show hidden"
	if m.learn != nil && m.learn.all {
		hidden = "hide candidates/rejected"
	}
	return []helpItem{{"↑/↓", "move"}, {"a", "accept"}, {"x", "reject"}, {"e", "edit"}, {"d", "delete"},
		{"h", hidden}, {"r", "refresh"}, {"l", "run atl learn"}, {"esc/q", "back"}, {"?", "all keys"}}
}

// dialog draws the question.
func (a *askState) dialog(w, h int) *dialog {
	iw := dialogWidth(confirmWrap, w)
	var out []string
	for i, p := range a.text {
		if i > 0 {
			out = append(out, "")
		}
		st := lipgloss.NewStyle()
		if i == 0 {
			st = titleStyle
		}
		out = append(out, wrapText(p, iw, iw, st)...)
	}
	out = append(out, "", trunc(keyStyle.Render("enter/y")+descStyle.Render(" "+a.yesKey)+"   "+
		keyStyle.Render("esc/n")+descStyle.Render(" cancel"), iw))
	if hh := max(h-2, 1); len(out) > hh {
		out = out[len(out)-hh:]
	}
	c := primary
	if a.danger {
		c = errColor
	}
	return box(a.title, out, iw, c)
}

// learnAsking reports whether the edit box asks whether to quit (ctrl+c
// with unsaved changes).
func (m *Model) learnAsking() bool {
	return m.mode == modeLearnEdit && m.learn != nil && m.learn.edit != nil && m.learn.edit.askQuit
}

// learnHelpSections is the key reference (?) of the learnings view.
var learnHelpSections = []helpSection{
	{"Learnings", []helpItem{{"↑/↓ j/k", "move"}, {"pgup/pgdn", "page"},
		{"a", "accept: agents get it at their next start"}, {"x", "reject (asks): not proposed again"},
		{"e", "edit the text (enter saves, esc cancels)"}, {"d", "delete (asks; atl learn may bring it back)"},
		{"h", "show/hide the candidates and the rejected"}, {"r", "refresh"},
		{"l", "run atl learn (asks first)"}, {"esc/q", "back to the tasks"}}},
	{"Details", []helpItem{{"?", "after a quote: it shares no word with the text"},
		{"outdated", "contradicted since you accepted it"}, {"still-corrected", "you still had to say it since"}}},
}
