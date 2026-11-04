package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/andreujuanc/atelier/internal/app"
	"github.com/andreujuanc/atelier/internal/model"
)

// Refresh intervals.
const (
	listEvery    = time.Second
	detailEvery  = 3 * time.Second
	recoverEvery = 5 * time.Second
	dirtyEvery   = 5 * time.Second // after the previous check finished
	dirtyTimeout = 30 * time.Second
	selDebounce  = 150 * time.Millisecond
)

type mode int

const (
	modeNormal mode = iota
	modeFilter
	modeForm
	modeConfirm
	modePicker
	modeNote
	modeSessions
)

// confirmState is a pending done/cancel confirmation.
type confirmState struct {
	verb  string // "done" or "cancel"
	id    int64
	slug  string
	dirty string // non-empty: the refusal; F forces
}

// Messages.
type (
	listTickMsg    struct{}
	recoverTickMsg struct{}
	dirtyTickMsg   struct{}
	selTickMsg     struct{ seq int }

	listMsg struct {
		tasks []app.TaskView
		err   error
	}
	detailMsg struct {
		seq  int
		id   int64
		view *app.TaskView
		err  error
	}
	recoverMsg struct {
		n   int
		err error
	}
	dirtyMsg struct {
		dirty map[int64]bool
		err   error
	}
	projectsMsg struct {
		names []string
		err   error
	}
	// opMsg ends an async operation on a task.
	opMsg struct {
		id   int64
		op   string
		text string
		err  error
	}
	finishMsg struct {
		id    int64
		slug  string
		verb  string
		force bool
		err   error
	}
	newTaskMsg struct {
		task *model.Task
		err  error
	}
	// openMsg ends an open step: cmd attaches; choices (cmd nil) are the
	// sessions to pick from (OpenOrChoose).
	openMsg struct {
		id      int64
		slug    string
		cmd     *exec.Cmd
		choices []app.SessionChoice
		err     error
	}
	assistantMsg struct {
		cmd  *exec.Cmd
		hint string
		err  error
	}
	execDoneMsg struct {
		op, slug string
		stderr   *bytes.Buffer
		err      error
	}
	warnMsg string

	reposMsg struct {
		names []string
		err   error
	}
	projectReposMsg struct {
		name  string
		repos []app.RepoArg
		err   error
	}
	// addStepMsg ends AddRepo for repos[i] of an `a` run on a task.
	addStepMsg struct {
		id     int64
		slug   string
		repos  []app.RepoArg
		i      int
		err    error
		added  []app.RepoArg // before this step
		failed []string      // before this step
	}
)

// Model is the TUI state. All backend calls run as tea.Cmds.
type Model struct {
	be  Backend
	ctx context.Context
	now func() time.Time
	// after schedules msg after d (tea.Tick); tests replace it.
	after func(d time.Duration, msg tea.Msg) tea.Cmd
	// dirtyTimeout bounds one DirtyTasks call (a hung git must not leave
	// dirtying set forever); tests shorten it.
	dirtyTimeout time.Duration
	// diffCmd returns the command that shows a task's diff (paged).
	diffCmd func(slug string) *exec.Cmd
	// flushOut, if set, returns once every App.Out line written so far has
	// been delivered to the program (as warnMsg): an op's result message
	// sent after it is then handled after the op's output lines.
	flushOut func()

	width, height int

	tasks  []app.TaskView
	loaded bool
	rows   []row
	selIdx int
	selID  int64
	offset int // first visible list row
	selSeq int
	// selPending: a debounced detail fetch for a new selection is scheduled.
	selPending bool
	// stickyID is the task selected before a filter hid it; it is selected
	// again once visible (e.g. when the filter is cleared).
	stickyID int64

	detail   *app.TaskView
	detailAt time.Time
	// detailSeq numbers detail requests; detailInflight is the latest one
	// still outstanding (0: none); detailApplied the latest one shown.
	detailSeq      int
	detailInflight int
	detailApplied  int

	listing    bool
	recovering bool
	recoverErr string

	// dirty is the last DirtyTasks result (open task id → uncommitted
	// changes in a rw worktree), kept between refreshes; dirtying: a
	// DirtyTasks call is in flight.
	dirty    map[int64]bool
	dirtying bool
	dirtyErr string

	mode        mode
	filter      string
	filterInput textinput.Model
	form        *newForm
	lastForm    *newForm // kept after a failed create, to retry
	confirm     *confirmState
	picker      *picker
	// pickTask is the task the open picker adds repos to (`a`); nil: the
	// picker belongs to the new-task form.
	pickTask *app.TaskView
	repos    []string // last AvailableRepos result
	// sessPick is the session picker Enter opened (modeSessions).
	sessPick *sessionPicker
	// noteInput is the `m` note being typed for task noteID/noteSlug.
	noteInput textinput.Model
	noteID    int64
	noteSlug  string
	// pending holds dirty refusals that arrived outside normal mode; they
	// are shown once back in normal mode.
	pending []*confirmState

	busy     map[int64]string // task id → op in progress
	creating bool
	// assisting: an Assistant call is in flight (A pressed, not yet attached).
	assisting bool
	projects  []string

	status    string
	statusErr bool
	// statusFromOut: the status is an App.Out line (warnMsg), which a later
	// line may replace; other error statuses are kept until the next action.
	statusFromOut bool

	// quitRequested: quit once no operation is in flight. forceQuit: the
	// user forced the quit with operations still running.
	quitRequested bool
	forceQuit     bool
}

// Forced reports whether the user forced the quit (second ctrl+c) while
// operations were still running.
func (m *Model) Forced() bool { return m.forceQuit }

// New returns a Model over be.
func New(ctx context.Context, be Backend) *Model {
	fi := textinput.New()
	fi.Prompt = "/"
	fi.Placeholder = "filter"
	fi.Cursor.SetMode(cursor.CursorStatic)
	ni := textinput.New()
	ni.Prompt = "note> "
	ni.Placeholder = "a note for this task (enter saves, esc cancels)"
	ni.Cursor.SetMode(cursor.CursorStatic)
	ni.CharLimit = 2000
	return &Model{
		be:  be,
		ctx: ctx,
		now: time.Now,
		after: func(d time.Duration, msg tea.Msg) tea.Cmd {
			return tea.Tick(d, func(time.Time) tea.Msg { return msg })
		},
		diffCmd:      func(slug string) *exec.Cmd { return exec.Command("atl", "diff", slug) },
		filterInput:  fi,
		noteInput:    ni,
		busy:         map[int64]string{},
		dirtyTimeout: dirtyTimeout,
		width:        80,
		height:       24,
	}
}

// Init loads tasks, projects and runs Recover, and starts the tickers.
// The dirty check is a single chain: each result arms the next tick.
func (m *Model) Init() tea.Cmd {
	m.listing, m.recovering, m.dirtying = true, true, true
	return tea.Batch(m.listCmd(), m.recoverCmd(), m.projectsCmd(), m.dirtyCmd(),
		m.after(listEvery, listTickMsg{}), m.after(recoverEvery, recoverTickMsg{}))
}

// --- commands ---

func (m *Model) listCmd() tea.Cmd {
	be, ctx := m.be, m.ctx
	return func() tea.Msg {
		ts, err := be.List(ctx)
		return listMsg{ts, err}
	}
}

func (m *Model) recoverCmd() tea.Cmd {
	be, ctx := m.be, m.ctx
	return func() tea.Msg {
		n, err := be.Recover(ctx)
		return recoverMsg{n, err}
	}
}

func (m *Model) dirtyCmd() tea.Cmd {
	be, ctx, timeout := m.be, m.ctx, m.dirtyTimeout
	return func() tea.Msg {
		tctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		d, err := be.DirtyTasks(tctx)
		if err != nil && errors.Is(tctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			err = fmt.Errorf("timed out after %s", timeout)
		}
		return dirtyMsg{d, err}
	}
}

func (m *Model) projectsCmd() tea.Cmd {
	be := m.be
	return func() tea.Msg {
		names, err := be.ProjectNames()
		return projectsMsg{names, err}
	}
}

func (m *Model) reposCmd() tea.Cmd {
	be, ctx := m.be, m.ctx
	return func() tea.Msg {
		names, err := be.AvailableRepos(ctx)
		return reposMsg{names, err}
	}
}

// syncProject preselects the form's project repos when the project field
// names a known project other than the one already applied (loading them
// asynchronously), and drops the preselection when the field is cleared.
func (m *Model) syncProject() tea.Cmd {
	f := m.form
	if f == nil {
		return nil
	}
	name := f.project()
	if name == f.preProject || (name != "" && (name == f.loadingProject || name == f.projErrFor)) {
		return nil
	}
	if name == "" {
		m.applyFormProject("", nil)
		return nil
	}
	known := false
	for _, p := range f.projects {
		known = known || p == name
	}
	if !known {
		return nil
	}
	f.loadingProject = name
	be := m.be
	return func() tea.Msg {
		repos, err := be.ProjectRepos(name)
		return projectReposMsg{name, repos, err}
	}
}

// applyFormProject preselects project name's repos in the form and, if the
// form's picker is open, in its selection too (confirming it replaces the
// form's repos).
func (m *Model) applyFormProject(name string, repos []app.RepoArg) {
	f := m.form
	oldFrom := f.fromProject
	f.applyProject(name, repos)
	if m.picker != nil && m.pickTask == nil {
		m.picker.sel = mergeProject(m.picker.sel, oldFrom, repos)
	}
}

// openPicker shows the repo picker (for the form if t is nil, else to add
// repos to t) and refreshes the available repos.
func (m *Model) openPicker(t *app.TaskView, title string, sel []app.RepoArg, locked map[string]model.Mode) tea.Cmd {
	m.picker = newPicker(title, sel, locked)
	if m.repos != nil {
		m.picker.available = m.repos // shown until the refresh arrives
		m.picker.initCursor()
	}
	m.pickTask = t
	m.mode = modePicker
	return m.reposCmd()
}

// addStep runs AddRepo for repos[i] of task id.
func (m *Model) addStep(id int64, slug string, repos []app.RepoArg, i int, added []app.RepoArg, failed []string) tea.Cmd {
	m.busy[id] = fmt.Sprintf("add %s (%d/%d) to %s", repos[i].Name, i+1, len(repos), slug)
	be, ctx, flush := m.be, m.ctx, m.flushOut
	return func() tea.Msg {
		_, err := be.AddRepo(ctx, slug, repos[i], false)
		if flush != nil {
			flush() // AddRepo's output lines land before this result
		}
		return addStepMsg{id: id, slug: slug, repos: repos, i: i, err: err, added: added, failed: failed}
	}
}

// addSummary is the status text after an `a` run.
func addSummary(slug string, added []app.RepoArg, failed []string) string {
	var parts []string
	if len(added) > 0 {
		s := "added " + summary(added) + " to " + slug
		var ro []string
		for _, r := range added {
			if r.Mode == model.ModeRO {
				ro = append(ro, r.Name)
			}
		}
		if len(ro) > 0 {
			s += "; start a new session (s) to see " + strings.Join(ro, ", ")
		}
		parts = append(parts, s)
	}
	if len(failed) > 0 {
		parts = append(parts, "failed: "+strings.Join(failed, "; "))
	}
	return strings.Join(parts, "; ")
}

func (m *Model) detailCmd(id int64, slug string) tea.Cmd {
	be, ctx := m.be, m.ctx
	m.detailSeq++
	seq := m.detailSeq
	m.detailInflight = seq
	return func() tea.Msg {
		v, err := be.Get(ctx, slug)
		return detailMsg{seq: seq, id: id, view: v, err: err}
	}
}

// reload asks for a fresh list (unless one is in flight) and the selected
// task's detail.
func (m *Model) reload() tea.Cmd {
	var cmds []tea.Cmd
	if !m.listing {
		m.listing = true
		cmds = append(cmds, m.listCmd())
	}
	if t := m.selected(); t != nil {
		cmds = append(cmds, m.detailCmd(t.Task.ID, t.Task.Slug))
	}
	return tea.Batch(cmds...)
}

// startOp marks task t busy with op and runs fn asynchronously; fn returns
// the status text.
func (m *Model) startOp(t *app.TaskView, op string, fn func(ctx context.Context) (string, error)) tea.Cmd {
	id, ctx := t.Task.ID, m.ctx
	m.busy[id] = op + " " + t.Task.Slug
	return func() tea.Msg {
		text, err := fn(ctx)
		return opMsg{id: id, op: op, text: text, err: err}
	}
}

// --- selection ---

func (m *Model) selected() *app.TaskView {
	if m.selIdx >= 0 && m.selIdx < len(m.rows) && m.rows[m.selIdx].isTask() {
		return m.rows[m.selIdx].task
	}
	return nil
}

// rebuild recomputes rows and restores the selection by task ID, falling
// back to the task nearest the previous position. It reports whether the
// selected task changed.
func (m *Model) rebuild() bool {
	prev := m.selID
	m.rows = buildRows(m.tasks, m.filter)
	if m.stickyID != 0 {
		if m.rowOf(m.stickyID) >= 0 {
			m.selID, m.stickyID = m.stickyID, 0
		} else if m.filter == "" {
			m.stickyID = 0 // gone
		}
	}
	found := m.rowOf(m.selID)
	if found < 0 {
		if m.filter != "" && m.selID != 0 && m.stickyID == 0 {
			m.stickyID = m.selID
		}
		found = m.nearestTask(m.selIdx)
	}
	m.selIdx = found
	m.selID = 0
	if t := m.selected(); t != nil {
		m.selID = t.Task.ID
	}
	m.clampOffset()
	return m.selID != prev
}

// rowOf returns the row index of task id, or -1.
func (m *Model) rowOf(id int64) int {
	if id == 0 {
		return -1
	}
	for i, r := range m.rows {
		if r.isTask() && r.task.Task.ID == id {
			return i
		}
	}
	return -1
}

// nearestTask returns the task row nearest to i (looking down first), or -1.
func (m *Model) nearestTask(i int) int {
	if i >= len(m.rows) {
		i = len(m.rows) - 1
	}
	if i < 0 {
		i = 0
	}
	for d := 0; d < len(m.rows); d++ {
		if j := i + d; j < len(m.rows) && m.rows[j].isTask() {
			return j
		}
		if j := i - d; j >= 0 && j < len(m.rows) && m.rows[j].isTask() {
			return j
		}
	}
	return -1
}

// move moves the selection by delta task rows, skipping headers.
func (m *Model) move(delta int) tea.Cmd {
	if len(m.rows) == 0 {
		return nil
	}
	i := m.selIdx
	for step := 0; step < abs(delta); step++ {
		j := i
		for {
			if delta > 0 {
				j++
			} else {
				j--
			}
			if j < 0 || j >= len(m.rows) {
				j = i
				break
			}
			if m.rows[j].isTask() {
				break
			}
		}
		i = j
	}
	if i == m.selIdx || !m.rows[i].isTask() {
		return nil
	}
	m.selIdx = i
	m.selID = m.rows[i].task.Task.ID
	m.stickyID = 0 // an explicit choice replaces the remembered task
	m.clampOffset()
	return m.selectionChanged()
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// selectionChanged debounces a detail fetch for the new selection.
func (m *Model) selectionChanged() tea.Cmd {
	m.selSeq++
	m.selPending = true
	return m.after(selDebounce, selTickMsg{m.selSeq})
}

// listHeight is the number of list rows visible.
func (m *Model) listHeight() int {
	h := m.height - 2
	if h < 1 {
		h = 1
	}
	return h
}

func (m *Model) clampOffset() {
	h := m.listHeight()
	if m.selIdx >= 0 {
		if m.selIdx < m.offset {
			m.offset = m.selIdx
			// show the group header above the first task if possible
			if m.offset > 0 && m.rows[m.offset-1].header {
				m.offset--
			}
		}
		if m.selIdx >= m.offset+h {
			m.offset = m.selIdx - h + 1
		}
	}
	if max := len(m.rows) - h; m.offset > max {
		m.offset = max
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

// --- status line ---

func (m *Model) setStatus(s string) { m.setStatusErr(s, false) }

func (m *Model) setErr(err error) { m.setStatusErr(firstLine(err.Error()), true) }

func (m *Model) setStatusErr(s string, isErr bool) {
	m.status, m.statusErr, m.statusFromOut = s, isErr, false
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}

// --- update ---

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.update(msg)
	if m.forceQuit {
		return m, tea.Quit
	}
	if m.mode == modeNormal && len(m.pending) > 0 && !m.quitRequested {
		m.confirm, m.pending = m.pending[0], m.pending[1:]
		m.mode = modeConfirm
	}
	if m.quitRequested {
		n := len(m.busyOps())
		if n == 0 {
			return m, tea.Quit
		}
		m.setStatus(quitText(n))
	}
	return m, cmd
}

func quitText(n int) string {
	return fmt.Sprintf("%d operation(s) running — press q again to quit after they finish, ctrl+c again to force", n)
}

// requestQuit quits now if nothing is in flight; otherwise the first
// request quits once all operations end and a second ctrl+c forces.
func (m *Model) requestQuit(key string) tea.Cmd {
	if len(m.busyOps()) == 0 {
		return tea.Quit
	}
	if m.quitRequested && key == "ctrl+c" {
		m.forceQuit = true
		return tea.Quit
	}
	m.quitRequested = true
	return nil
}

func (m *Model) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.clampOffset()
		if m.form != nil {
			m.form.setWidth(m.formInputWidth())
		}
		m.noteInput.Width = m.noteInputWidth()
		return nil
	case tea.KeyMsg:
		return m.handleKey(msg)

	case listTickMsg:
		cmds := []tea.Cmd{m.after(listEvery, listTickMsg{})}
		if !m.listing {
			m.listing = true
			cmds = append(cmds, m.listCmd())
		}
		// While a debounced selection fetch is pending, leave it to the
		// debounce: fast scrolling must not fetch every task passed.
		if t := m.selected(); t != nil && m.detailInflight == 0 && !m.selPending &&
			(m.detail == nil || m.detail.Task.ID != t.Task.ID || m.now().Sub(m.detailAt) >= detailEvery) {
			cmds = append(cmds, m.detailCmd(t.Task.ID, t.Task.Slug))
		}
		return tea.Batch(cmds...)
	case recoverTickMsg:
		cmds := []tea.Cmd{m.after(recoverEvery, recoverTickMsg{})}
		if !m.recovering {
			m.recovering = true
			cmds = append(cmds, m.recoverCmd())
		}
		return tea.Batch(cmds...)
	case dirtyTickMsg:
		// Not rescheduled here: the check's dirtyMsg arms the next tick.
		if m.dirtying {
			return nil
		}
		m.dirtying = true
		return m.dirtyCmd()
	case selTickMsg:
		if msg.seq != m.selSeq {
			return nil
		}
		m.selPending = false
		if t := m.selected(); t != nil {
			return m.detailCmd(t.Task.ID, t.Task.Slug)
		}
		return nil

	case listMsg:
		m.listing = false
		if msg.err != nil {
			m.setErr(fmt.Errorf("list: %w", msg.err))
			return nil
		}
		m.tasks, m.loaded = msg.tasks, true
		if m.rebuild() {
			return m.selectionChanged()
		}
		return nil
	case detailMsg:
		if msg.seq == m.detailInflight {
			m.detailInflight = 0
		}
		// Drop replies for another task or older than the one shown.
		if t := m.selected(); t == nil || t.Task.ID != msg.id || msg.seq < m.detailApplied {
			return nil
		}
		m.detailApplied = msg.seq
		if msg.err != nil {
			m.setErr(msg.err)
			return nil
		}
		m.detail, m.detailAt = msg.view, m.now()
		return nil
	case recoverMsg:
		m.recovering = false
		if msg.err != nil {
			// Shown once per distinct error: tmux may simply be missing.
			if e := msg.err.Error(); e != m.recoverErr {
				m.recoverErr = e
				m.setErr(msg.err)
			}
			return nil
		}
		m.recoverErr = ""
		if msg.n > 0 {
			m.setStatus(fmt.Sprintf("%d session(s) marked stopped (tmux window gone); r resumes", msg.n))
			return m.reload()
		}
		return nil
	case dirtyMsg:
		m.dirtying = false
		next := m.after(dirtyEvery, dirtyTickMsg{})
		if msg.err != nil {
			// Keep the last result; show each distinct error once.
			if e := msg.err.Error(); e != m.dirtyErr {
				m.dirtyErr = e
				m.setErr(fmt.Errorf("dirty check: %w", msg.err))
			}
			return next
		}
		m.dirtyErr = ""
		m.dirty = msg.dirty
		return next
	case projectsMsg:
		if msg.err == nil {
			m.projects = msg.names
			if m.form != nil {
				m.form.projects = msg.names
				return m.syncProject()
			}
		}
		return nil
	case projectReposMsg:
		f := m.form
		if f == nil {
			return nil
		}
		if f.loadingProject == msg.name {
			f.loadingProject = ""
		}
		if f.project() != msg.name {
			return m.syncProject() // stale: load the current one
		}
		if msg.err != nil {
			// Not preselected: submitting sends the project as is and
			// NewTask reports the problem.
			f.projErr, f.projErrFor = firstLine(msg.err.Error()), msg.name
			return nil
		}
		m.applyFormProject(msg.name, msg.repos)
		return nil
	case reposMsg:
		if msg.err == nil {
			m.repos = msg.names
		}
		if m.picker != nil {
			m.picker.setAvailable(msg.names, msg.err)
		}
		return nil
	case addStepMsg:
		added, failed := msg.added, msg.failed
		r := msg.repos[msg.i]
		if msg.err != nil {
			failed = append(append([]string(nil), failed...), r.Name+": "+firstLine(msg.err.Error()))
		} else {
			added = append(append([]app.RepoArg(nil), added...), r)
		}
		if next := msg.i + 1; next < len(msg.repos) {
			return m.addStep(msg.id, msg.slug, msg.repos, next, added, failed)
		}
		delete(m.busy, msg.id)
		m.setStatusErr(firstLine(addSummary(msg.slug, added, failed)), len(failed) > 0)
		return m.reload()
	case warnMsg:
		if m.statusErr && !m.statusFromOut {
			return nil // never hide an error summary behind progress
		}
		s := firstLine(string(msg))
		m.setStatusErr(s, strings.HasPrefix(s, "warning:"))
		m.statusFromOut = true
		return nil

	case opMsg:
		delete(m.busy, msg.id)
		if msg.err != nil {
			if msg.text != "" {
				m.setStatusErr(firstLine(msg.text+"; "+msg.err.Error()), true)
			} else {
				m.setErr(fmt.Errorf("%s: %w", msg.op, msg.err))
			}
		} else {
			m.setStatus(msg.text)
		}
		return m.reload()
	case finishMsg:
		delete(m.busy, msg.id)
		if msg.err != nil {
			if !msg.force && isDirtyErr(msg.err) {
				c := &confirmState{verb: msg.verb, id: msg.id, slug: msg.slug, dirty: firstLine(msg.err.Error())}
				if m.mode != modeNormal {
					// Don't yank the user out of the form/filter/another
					// prompt: Update shows it once back in normal mode.
					if m.form != nil {
						m.lastForm = m.form
					}
					m.pending = append(m.pending, c)
					return nil
				}
				m.confirm, m.mode = c, modeConfirm
				return nil
			}
			m.setErr(fmt.Errorf("%s: %w", msg.verb, msg.err))
			return m.reload()
		}
		m.setStatus(fmt.Sprintf("%s %s", msg.slug, map[string]string{"done": "done", "cancel": "cancelled"}[msg.verb]))
		return m.reload()
	case newTaskMsg:
		m.creating = false
		if msg.err != nil {
			m.setErr(fmt.Errorf("new task: %w", msg.err))
			return nil
		}
		m.lastForm = nil
		m.selID, m.stickyID = msg.task.ID, 0
		m.selIdx = len(m.rows) // fallback if it is filtered out
		m.setStatus(fmt.Sprintf("created %s (%s)", msg.task.Slug, m.be.TaskDir(msg.task.Slug)))
		return tea.Batch(m.reload(), m.selectionChanged())
	case openMsg:
		delete(m.busy, msg.id)
		if msg.err != nil {
			m.setErr(fmt.Errorf("open: %w", msg.err))
			return m.reload()
		}
		if msg.cmd != nil {
			return m.exec("open", msg.slug, msg.cmd)
		}
		if len(msg.choices) == 0 { // defensive: the backend returns one or the other
			return m.reload()
		}
		if m.mode != modeNormal || m.quitRequested {
			// Don't yank the user out of the form/filter/another prompt.
			m.setStatus("open " + msg.slug + ": it has sessions to choose from — press enter again")
			return nil
		}
		m.sessPick = newSessionPicker(msg.id, msg.slug, msg.choices)
		m.mode = modeSessions
		return nil
	case assistantMsg:
		m.assisting = false
		if msg.err != nil {
			m.setErr(fmt.Errorf("assistant: %w", msg.err))
			return nil
		}
		if msg.hint != "" {
			m.setStatus(msg.hint)
		}
		return m.exec("assistant", "", msg.cmd)
	case execDoneMsg:
		if msg.err != nil {
			prefix := strings.TrimSpace(msg.op + " " + msg.slug)
			text := prefix + ": " + msg.err.Error()
			if msg.stderr != nil {
				if s := lastLine(msg.stderr.String()); s != "" {
					text = prefix + ": " + s
				}
			}
			m.setStatusErr(text, true)
		} else if msg.op == "open" {
			m.setStatus("attached " + msg.slug)
		}
		return m.reload()
	}

	switch m.mode {
	case modeFilter:
		var cmd tea.Cmd
		m.filterInput, cmd = m.filterInput.Update(msg)
		return cmd
	case modeNote:
		var cmd tea.Cmd
		m.noteInput, cmd = m.noteInput.Update(msg)
		return cmd
	}
	return nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// isDirtyErr reports whether a done/cancel refusal is about uncommitted
// changes.
func isDirtyErr(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "uncommitted") || strings.Contains(s, "dirty")
}

// exec runs c with the terminal handed over; stderr is captured for the
// status line.
func (m *Model) exec(op, slug string, c *exec.Cmd) tea.Cmd {
	buf := &bytes.Buffer{}
	if c.Stderr == nil {
		c.Stderr = buf
	}
	return tea.ExecProcess(c, func(err error) tea.Msg {
		return execDoneMsg{op: op, slug: slug, stderr: buf, err: err}
	})
}

func (m *Model) handleKey(msg tea.KeyMsg) tea.Cmd {
	key := msg.String()
	if key == "ctrl+c" {
		return m.requestQuit(key)
	}
	// Bubble Tea may deliver fast-typed letters as one multi-rune event
	// ("jjj"): in normal mode replay them one key at a time (the mode may
	// change on the way, e.g. "/abc"). Pastes are ignored there.
	if m.mode == modeNormal && msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && !msg.Alt {
		if msg.Paste {
			return nil
		}
		var cmds []tea.Cmd
		for _, r := range msg.Runes {
			cmds = append(cmds, m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}))
		}
		return tea.Batch(cmds...)
	}
	switch m.mode {
	case modePicker:
		return m.pickerKey(msg)
	case modeForm:
		return m.formKey(msg)
	case modeConfirm:
		return m.confirmKey(key)
	case modeFilter:
		return m.filterKey(msg)
	case modeNote:
		return m.noteKey(msg)
	case modeSessions:
		return m.sessionsKey(msg)
	}
	return m.normalKey(key)
}

// noteKey handles a key while a note is being typed (`m`).
func (m *Model) noteKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.mode = modeNormal
		m.noteInput.Blur()
		m.setStatus("note cancelled")
		return nil
	case "enter":
		body := strings.TrimSpace(m.noteInput.Value())
		if body == "" {
			m.mode = modeNormal
			m.noteInput.Blur()
			m.setStatus("note: empty, not saved")
			return nil
		}
		if op, busy := m.busy[m.noteID]; busy {
			m.setStatus("busy: " + op + "… (enter again to save, esc cancels)")
			return nil
		}
		m.mode = modeNormal
		m.noteInput.Blur()
		id, slug, be, ctx := m.noteID, m.noteSlug, m.be, m.ctx
		m.busy[id] = "note " + slug
		return func() tea.Msg {
			text := "note added to " + slug
			if _, err := be.AddNote(ctx, slug, body, model.AuthorUser); err != nil {
				return opMsg{id: id, op: "note", err: err}
			}
			return opMsg{id: id, op: "note", text: text}
		}
	}
	var cmd tea.Cmd
	m.noteInput, cmd = m.noteInput.Update(msg)
	return cmd
}

func (m *Model) filterKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.mode = modeNormal
		m.filterInput.Blur()
		m.filterInput.SetValue("")
		return m.applyFilter("")
	case "enter":
		m.mode = modeNormal
		m.filterInput.Blur()
		return m.applyFilter(m.filterInput.Value())
	case "up":
		return m.move(-1)
	case "down":
		return m.move(1)
	}
	var cmd tea.Cmd
	m.filterInput, cmd = m.filterInput.Update(msg)
	return tea.Batch(cmd, m.applyFilter(m.filterInput.Value()))
}

func (m *Model) applyFilter(f string) tea.Cmd {
	m.filter = f
	if m.rebuild() {
		return m.selectionChanged()
	}
	return nil
}

// sessionsKey handles a key in the session picker: enter attaches to a live
// session, resumes a past one or starts a new one; esc launches nothing.
func (m *Model) sessionsKey(msg tea.KeyMsg) tea.Cmd {
	p := m.sessPick
	if p == nil {
		m.mode = modeNormal
		return nil
	}
	res := p.update(msg)
	if res == pickContinue {
		return nil
	}
	m.sessPick, m.mode = nil, modeNormal
	if res == pickCancel {
		m.setStatus("open " + p.slug + " cancelled")
		return nil
	}
	if op, busy := m.busy[p.taskID]; busy {
		m.setStatus("busy: " + op + "…")
		return nil
	}
	id, slug, be, ctx := p.taskID, p.slug, m.be, m.ctx
	m.busy[id] = "open " + slug
	if c := p.picked(); c != nil {
		sid := c.Session.ID
		return func() tea.Msg {
			cmd, err := be.OpenSession(ctx, slug, sid)
			return openMsg{id: id, slug: slug, cmd: cmd, err: err}
		}
	}
	return func() tea.Msg {
		cmd, err := be.OpenNew(ctx, slug)
		return openMsg{id: id, slug: slug, cmd: cmd, err: err}
	}
}

// pickerKey handles a key in the repo picker.
func (m *Model) pickerKey(msg tea.KeyMsg) tea.Cmd {
	p, t := m.picker, m.pickTask
	res := p.update(msg)
	if res == pickContinue {
		return nil
	}
	m.picker, m.pickTask = nil, nil
	if t == nil { // the new-task form's picker
		m.mode = modeForm
		if m.form != nil {
			if res == pickConfirm {
				m.form.repos = p.selection()
				m.form.err = ""
				// Enter on Repos would reopen the picker: move on.
				m.form.setFocus(fieldBase)
			}
		} else {
			m.mode = modeNormal
		}
		return nil
	}
	m.mode = modeNormal
	if res == pickCancel {
		m.setStatus("add cancelled")
		return nil
	}
	sel := p.selection()
	if len(sel) == 0 {
		m.setStatus("add: nothing selected")
		return nil
	}
	if op, busy := m.busy[t.Task.ID]; busy {
		m.setStatus("busy: " + op + "…")
		return nil
	}
	return m.addStep(t.Task.ID, t.Task.Slug, sel, 0, nil, nil)
}

func (m *Model) formKey(msg tea.KeyMsg) tea.Cmd {
	res, cmd := m.form.update(msg)
	switch res {
	case formPick:
		f := m.form
		f.err = ""
		return m.openPicker(nil, "Repos for the new task", f.repos, nil)
	case formCancel:
		m.form, m.mode = nil, modeNormal
		m.setStatus("new task cancelled")
		return nil
	case formSubmit:
		o, err := m.form.opts()
		if err != nil { // already validated; defensive
			m.form.err = err.Error()
			return nil
		}
		m.lastForm = m.form
		m.form, m.mode = nil, modeNormal
		m.creating = true
		m.setStatus("creating " + o.Title + "…")
		be, ctx := m.be, m.ctx
		return func() tea.Msg {
			t, err := be.NewTask(ctx, o)
			return newTaskMsg{t, err}
		}
	}
	return tea.Batch(cmd, m.syncProject())
}

func (m *Model) confirmKey(key string) tea.Cmd {
	c := m.confirm
	m.confirm, m.mode = nil, modeNormal
	force := false
	switch {
	case c.dirty == "" && (key == "y" || key == "Y"):
	case c.dirty != "" && key == "F":
		force = true
	default:
		m.setStatus(c.verb + " " + c.slug + " aborted")
		return nil
	}
	if _, busy := m.busy[c.id]; busy {
		m.setStatus("busy: " + m.busy[c.id])
		return nil
	}
	m.busy[c.id] = c.verb + " " + c.slug
	be, ctx := m.be, m.ctx
	return func() tea.Msg {
		var err error
		if c.verb == "cancel" {
			_, err = be.Cancel(ctx, c.slug, force)
		} else {
			_, err = be.Done(ctx, c.slug, force)
		}
		return finishMsg{id: c.id, slug: c.slug, verb: c.verb, force: force, err: err}
	}
}

func (m *Model) normalKey(key string) tea.Cmd {
	switch key {
	case "q":
		return m.requestQuit(key)
	case "j", "down":
		return m.move(1)
	case "k", "up":
		return m.move(-1)
	case "pgdown":
		return m.move(m.listHeight())
	case "pgup":
		return m.move(-m.listHeight())
	case "/":
		m.mode = modeFilter
		m.filterInput.SetValue(m.filter)
		m.filterInput.CursorEnd()
		m.filterInput.Focus()
		return nil
	case "esc":
		if m.filter != "" {
			m.filterInput.SetValue("")
			return m.applyFilter("")
		}
		return nil
	case "n":
		if m.creating {
			m.setStatus("busy: a task is being created")
			return nil
		}
		if m.lastForm != nil {
			m.form = m.lastForm
		} else {
			m.form = newTaskForm(m.projects)
		}
		m.form.projects = m.projects
		m.form.projErr, m.form.projErrFor = "", "" // retry a failed project load
		m.form.setWidth(m.formInputWidth())
		m.mode = modeForm
		return tea.Batch(m.projectsCmd(), m.syncProject())
	case "A":
		return m.assistant()
	}

	t := m.selected()
	if t == nil {
		if strings.ContainsAny(key, "srdcpamD12345") && len(key) == 1 || key == "enter" {
			m.setStatus("no task selected")
		}
		return nil
	}
	slug := t.Task.Slug
	switch key {
	case "enter", "s", "r", "d", "c", "p", "a", "m", "1", "2", "3", "4", "5":
		if op, busy := m.busy[t.Task.ID]; busy {
			m.setStatus("busy: " + op + "…")
			return nil
		}
	}
	switch key {
	case "enter":
		m.busy[t.Task.ID] = "open " + slug
		be, ctx, id := m.be, m.ctx, t.Task.ID
		return func() tea.Msg {
			c, choices, err := be.OpenOrChoose(ctx, slug)
			return openMsg{id: id, slug: slug, cmd: c, choices: choices, err: err}
		}
	case "s":
		be := m.be
		return m.startOp(t, "session", func(ctx context.Context) (string, error) {
			s, err := be.NewSession(ctx, slug)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("started %s in %s", s.Name, slug), nil
		})
	case "r":
		be := m.be
		return m.startOp(t, "resume", func(ctx context.Context) (string, error) {
			n, err := be.Resume(ctx, slug)
			text := fmt.Sprintf("resumed %d session(s) in %s", n, slug)
			if err != nil && n == 0 {
				text = ""
			}
			return text, err
		})
	case "d", "c":
		verb := map[string]string{"d": "done", "c": "cancel"}[key]
		if !t.Task.Status.Open() {
			m.setStatusErr(fmt.Sprintf("%s: task %s is already %s", verb, slug, t.Task.Status), true)
			return nil
		}
		m.confirm = &confirmState{verb: verb, id: t.Task.ID, slug: slug}
		m.mode = modeConfirm
		return nil
	case "p":
		be := m.be
		return m.startOp(t, "pr", func(ctx context.Context) (string, error) {
			res, err := be.PR(ctx, slug)
			return prSummary(slug, res), err
		})
	case "a":
		if !t.Task.Status.Open() {
			m.setStatusErr(fmt.Sprintf("add: task is closed (%s is %s)", slug, t.Task.Status), true)
			return nil
		}
		locked := map[string]model.Mode{}
		for _, r := range t.Task.Repos {
			locked[r.Name] = r.Mode
		}
		tv := *t
		return m.openPicker(&tv, "Add repos to "+clean(slug), nil, locked)
	case "m":
		m.noteID, m.noteSlug = t.Task.ID, slug
		m.noteInput.SetValue("")
		m.noteInput.Width = m.noteInputWidth()
		m.noteInput.Focus()
		m.mode = modeNote
		return nil
	case "D":
		return m.exec("diff", slug, m.diffCmd(slug))
	case "1", "2", "3", "4", "5":
		st := model.AllStatuses[int(key[0]-'1')]
		if st == t.Task.Status {
			return nil
		}
		be := m.be
		return m.startOp(t, "status", func(ctx context.Context) (string, error) {
			if err := be.SetStatus(ctx, slug, st); err != nil {
				return "", err
			}
			return fmt.Sprintf("%s → %s", slug, st), nil
		})
	}
	return nil
}

// assistant opens the atelier assistant, pointed at the selected task if
// there is one.
func (m *Model) assistant() tea.Cmd {
	if m.assisting {
		m.setStatus("busy: opening the assistant…")
		return nil
	}
	slug := ""
	if t := m.selected(); t != nil {
		slug = t.Task.Slug
	}
	m.assisting = true
	be, ctx := m.be, m.ctx
	return func() tea.Msg {
		c, hint, err := be.Assistant(ctx, slug)
		return assistantMsg{cmd: c, hint: hint, err: err}
	}
}

// prSummary formats PR results for the status line.
func prSummary(slug string, res []app.PRResult) string {
	if len(res) == 0 {
		return ""
	}
	parts := make([]string, 0, len(res))
	for _, r := range res {
		if r.Skipped {
			parts = append(parts, fmt.Sprintf("%s skipped (%s)", r.Repo, r.Reason))
		} else {
			parts = append(parts, fmt.Sprintf("%s %s", r.Repo, r.URL))
		}
	}
	return "pr " + slug + ": " + strings.Join(parts, "; ")
}

// busyOps returns the operations in flight, sorted.
func (m *Model) busyOps() []string {
	var ops []string
	for _, op := range m.busy {
		ops = append(ops, op)
	}
	if m.creating {
		ops = append(ops, "new task")
	}
	if m.assisting {
		ops = append(ops, "assistant")
	}
	sort.Strings(ops)
	return ops
}

// noteInputWidth is the visible width of the `m` note input: the full
// terminal width minus its prompt (0 or less means unlimited).
func (m *Model) noteInputWidth() int {
	return m.width - len(m.noteInput.Prompt) - 1
}
