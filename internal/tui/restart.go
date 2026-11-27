package tui

import (
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/andreujuanc/atelier/internal/app"
)

// restartPoll is how often the restart dialog checks again whether things
// still run in the task.
const restartPoll = 2 * time.Second

// restartState is the dialog an `a` run opens when adding the repos needs
// the task's sessions restarted (app.AddRepos) while things run in it
// (app.BusyError). Nothing has changed yet. Every restartPoll it checks
// again (app.AddCheck, which changes nothing); once nothing runs, it runs
// AddRepos, which adds the repos and restarts the sessions (and refuses
// again if something started meanwhile). Ticking the box and enter forces
// the restart, which ends what runs; esc gives up, with nothing changed.
// While AddRepos runs, keys wait.
type restartState struct {
	id      int64
	slug    string
	repos   []app.RepoArg
	running []string // what runs, as the last check found it
	ticked  bool     // the "Force restart" box
	// unticked: enter was pressed with the box unticked (shows a hint).
	unticked bool
	checking bool // an AddCheck of this dialog runs
	adding   bool // an AddRepos of this dialog runs
	force    bool // confirmed: the forced AddRepos runs, or follows the check in flight
	seq      int  // of the tick or check it waits for (restartSeq)
}

// addDoneMsg ends an AddRepos call of an `a` run (force: forced).
type addDoneMsg struct {
	id    int64
	slug  string
	repos []app.RepoArg
	force bool
	res   *app.AddResult
	err   error
}

// restartTickMsg asks the restart dialog of task id to check again.
type restartTickMsg struct {
	id  int64
	seq int
}

// restartCheckMsg ends an AddCheck of the restart dialog of task id.
type restartCheckMsg struct {
	id      int64
	seq     int
	running []string
	err     error
}

// addRun runs AddRepos for repos on task id.
func (m *Model) addRun(id int64, slug string, repos []app.RepoArg, force bool) tea.Cmd {
	m.busy[id] = "add " + summary(repos) + " to " + slug
	be, ctx, flush := m.be, m.ctx, m.flushOut
	return func() tea.Msg {
		res, err := be.AddRepos(ctx, slug, repos, app.AddOpts{Force: force})
		if flush != nil {
			flush() // AddRepos' output lines land before this result
		}
		return addDoneMsg{id: id, slug: slug, repos: repos, force: force, res: res, err: err}
	}
}

// dialogAdd runs dialog r's AddRepos.
func (m *Model) dialogAdd(r *restartState) tea.Cmd {
	r.adding = true
	return m.addRun(r.id, r.slug, r.repos, r.force)
}

// restartTick schedules the next check of dialog r.
func (m *Model) restartTick(r *restartState) tea.Cmd {
	m.restartSeq++
	r.seq = m.restartSeq
	return m.after(restartPoll, restartTickMsg{id: r.id, seq: r.seq})
}

// closeRestart closes the restart dialog.
func (m *Model) closeRestart() {
	m.restart = nil
	if m.mode == modeRestart {
		m.mode = modeNormal
	}
}

// dialogFor returns the open restart dialog if it is task id's.
func (m *Model) dialogFor(id int64) *restartState {
	if r := m.restart; r != nil && r.id == id {
		return r
	}
	return nil
}

func (m *Model) addDone(msg addDoneMsg) tea.Cmd {
	delete(m.busy, msg.id)
	r := m.dialogFor(msg.id)
	if r != nil && !r.adding {
		r = nil // not this dialog's call
	}
	var busy *app.BusyError
	if errors.As(msg.err, &busy) && !msg.force {
		switch {
		case r != nil: // something started since the check: wait again
			r.adding, r.running = false, busy.Running
			if r.force {
				return m.dialogAdd(r)
			}
			return m.restartTick(r)
		case !m.canTakeOver():
			m.setStatusErr(firstLine("add to "+msg.slug+" not done: "+busy.Error()), true)
			return nil
		}
		m.restart = &restartState{id: msg.id, slug: msg.slug, repos: msg.repos, running: busy.Running}
		m.mode = modeRestart
		return m.restartTick(m.restart)
	}
	if r != nil {
		m.closeRestart()
	}
	var added []app.RepoArg
	var failed []string
	restarted := 0
	if msg.res != nil {
		added, restarted = msg.res.Added, msg.res.Restarted
		for _, f := range msg.res.Failed {
			failed = append(failed, firstLine(f.Error()))
		}
	}
	text := addSummary(msg.slug, added, failed, restarted)
	switch {
	case msg.err != nil && text != "":
		m.setStatusErr(strings.TrimSpace(text+"; "+msg.err.Error()), true)
	case msg.err != nil:
		m.setErr(fmt.Errorf("add: %w", msg.err))
	case len(failed) > 0:
		m.setStatusErr(firstLine(text), true)
	default:
		m.setOK(firstLine(text))
	}
	return m.reload()
}

// addSummary is the status text after an `a` run.
func addSummary(slug string, added []app.RepoArg, failed []string, restarted int) string {
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "added "+summary(added)+" to "+slug)
	}
	if len(failed) > 0 {
		parts = append(parts, "failed: "+strings.Join(failed, "; "))
	}
	if restarted > 0 {
		parts = append(parts, fmt.Sprintf("restarted %d session(s)", restarted))
	}
	return strings.Join(parts, "; ")
}

func (m *Model) restartTicked(msg restartTickMsg) tea.Cmd {
	r := m.dialogFor(msg.id)
	if r == nil || r.seq != msg.seq || r.checking || r.adding || r.force {
		return nil
	}
	if m.quitRequested {
		m.closeRestart()
		return nil
	}
	r.checking = true
	be, ctx, id, seq, slug, repos := m.be, m.ctx, r.id, r.seq, r.slug, r.repos
	return func() tea.Msg {
		running, err := be.AddCheck(ctx, slug, repos)
		return restartCheckMsg{id: id, seq: seq, running: running, err: err}
	}
}

func (m *Model) restartChecked(msg restartCheckMsg) tea.Cmd {
	r := m.dialogFor(msg.id)
	if r == nil || r.seq != msg.seq || !r.checking {
		return nil // given up on (esc), or not this dialog's
	}
	r.checking = false
	switch {
	case r.force:
		return m.dialogAdd(r)
	case msg.err != nil:
		r.running = []string{"cannot tell what runs: " + firstLine(msg.err.Error())}
	case len(msg.running) > 0:
		r.running = msg.running
	case m.quitRequested:
		m.closeRestart()
		return nil
	default:
		return m.dialogAdd(r) // idle: add, restarting
	}
	return m.restartTick(r)
}

// restartKey handles a key in the restart dialog: space ticks the box,
// enter forces the restart once it is ticked, esc and n give up (nothing
// changes: a check in flight only reads). While the add runs, keys wait.
func (m *Model) restartKey(key string) tea.Cmd {
	r := m.restart
	if r == nil {
		m.mode = modeNormal
		return nil
	}
	if r.force || r.adding {
		return nil // the add and restart run: wait for them
	}
	switch key {
	case "esc", "n", "N":
		m.closeRestart()
		m.setStatus("add to " + r.slug + " cancelled; nothing changed")
		return nil
	case "space":
		r.ticked, r.unticked = !r.ticked, false
		return nil
	case "enter":
		if !r.ticked {
			r.unticked = true
			return nil
		}
		r.force = true
		if r.checking {
			return nil // sent when the check in flight returns
		}
		return m.dialogAdd(r)
	}
	return nil
}

// restartWrap is the widest the restart dialog's text runs.
const restartWrap = 72

// lines renders the restart dialog's text (width w, height h); spin is the
// spinner's frame.
func (r *restartState) lines(w, h int, spin string) []string {
	if w <= 0 || h <= 0 {
		return nil
	}
	wrap := func(s string, st lipgloss.Style) []string { return wrapText(s, min(w, restartWrap), w, st) }
	head := wrap("Cannot add "+clean(summary(r.repos))+" to "+clean(r.slug)+" yet", titleStyle)
	why := wrap("Running sessions cannot see a repo added under them, so adding it restarts them: "+
		"they are closed and the same conversations resumed. Things are running in the task now:", lipgloss.NewStyle())
	items := make([][]string, len(r.running))
	for i, s := range r.running {
		items[i] = wrap("• "+clean(s), markStyle)
	}
	// listOf is the first n items, and how many more there are.
	listOf := func(n int) []string {
		var l []string
		for _, it := range items[:n] {
			l = append(l, it...)
		}
		if n < len(items) {
			l = append(l, dimStyle.Render(trunc(fmt.Sprintf("… and %d more", len(items)-n), w)))
		}
		return l
	}
	var wait, last []string
	if r.force || r.adding {
		wait = wrap(spin+" Restarting the sessions and adding…", markStyle)
	} else {
		wait = wrap(spin+" Waiting: added, with the restart, as soon as nothing runs.", dimStyle)
		last = []string{trunc(checkbox(r.ticked, "Force restart (ends what runs)"), w)}
		if r.unticked {
			last = append(last, wrap("Tick the box with space to force the restart.", markStyle)...)
		}
	}
	join := func(sep bool, list []string) []string {
		var out []string
		for i, b := range [][]string{head, why, list, wait, last} {
			if len(b) == 0 {
				continue
			}
			if sep && i > 0 && i != 2 {
				out = append(out, "")
			}
			out = append(out, b...)
		}
		return out
	}
	n := len(items)
	out := join(true, listOf(n))
	if len(out) > h {
		out = join(false, listOf(n))
	}
	// Too tall: the list gives way first (its first item kept), then the top.
	for len(out) > h && n > 1 {
		n--
		out = join(false, listOf(n))
	}
	if len(out) > h {
		out = out[len(out)-h:] // keep the box visible on very short terminals
	}
	return out
}

func (r *restartState) dialog(w, h int, spin string) *dialog {
	iw := dialogWidth(restartWrap, w)
	return box("Restart needed", r.lines(iw, max(h-2, 1), spin), iw, errColor)
}
