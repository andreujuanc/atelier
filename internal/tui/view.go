package tui

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/andreujuanc/atelier/internal/app"
	"github.com/andreujuanc/atelier/internal/model"
)

var (
	accent      = lipgloss.AdaptiveColor{Light: "#B5179E", Dark: "#F7768E"}
	subtle      = lipgloss.AdaptiveColor{Light: "#888888", Dark: "#666666"}
	errColor    = lipgloss.AdaptiveColor{Light: "#C00000", Dark: "#FF5F5F"}
	okColor     = lipgloss.AdaptiveColor{Light: "#2E8B57", Dark: "#9ECE6A"}
	headerStyle = lipgloss.NewStyle().Bold(true)
	selStyle    = lipgloss.NewStyle().Reverse(true)
	markStyle   = lipgloss.NewStyle().Foreground(accent).Bold(true)
	waitStyle   = lipgloss.NewStyle().Foreground(accent).Bold(true)
	dimStyle    = lipgloss.NewStyle().Faint(true)
	labelStyle  = lipgloss.NewStyle().Foreground(subtle)
	errStyle    = lipgloss.NewStyle().Foreground(errColor)
	titleStyle  = lipgloss.NewStyle().Bold(true)
	sepStyle    = lipgloss.NewStyle().Foreground(subtle)
	runStyle    = lipgloss.NewStyle().Foreground(okColor)
	idleStyle   = lipgloss.NewStyle().Foreground(subtle)
	stopStyle   = lipgloss.NewStyle().Foreground(errColor).Faint(true)
)

// dotFor returns the glyph (one cell) and style of a task row's state dot.
func dotFor(k dotKind) (string, lipgloss.Style) {
	switch k {
	case dotWaiting:
		return "●", waitStyle
	case dotRunning:
		return "●", runStyle
	case dotIdle:
		return "●", idleStyle
	case dotStopped:
		return "○", stopStyle
	}
	return " ", lipgloss.NewStyle()
}

// legends explain the list row marks (help line): the first that fits is
// shown, none when neither does.
var legends = []string{
	legendDots + "  " + stopStyle.Render("○") + dimStyle.Render(" stopped") + legendStar,
	legendDots + legendStar,
}

var (
	legendDots = dimStyle.Render("● ") + waitStyle.Render("waiting") + dimStyle.Render("/") +
		runStyle.Render("running") + dimStyle.Render("/") + idleStyle.Render("idle")
	legendStar = "  " + markStyle.Render("*") + dimStyle.Render(" uncommitted")
)

// minSplitWidth is the narrowest terminal that shows the details pane.
const minSplitWidth = 40

// clean makes a user-supplied string safe to render on one line: control
// characters (newlines, tabs, ESC, …) become spaces and runs of spaces
// collapse to one.
func clean(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			if !space {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// fit truncates s (which may contain ANSI) to w cells and pads it to w.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = ansi.Truncate(s, w, "…")
	if pad := w - ansi.StringWidth(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// trunc truncates plain or styled s to w cells.
func trunc(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return ansi.Truncate(s, w, "…")
}

// View implements tea.Model.
func (m *Model) View() string {
	w, h := m.width, m.height
	if w <= 0 || h <= 0 {
		return ""
	}
	bodyH := h - 2
	if bodyH < 0 {
		bodyH = 0
	}
	var body []string
	if m.mode == modePicker && m.picker != nil {
		body = m.picker.lines(w, bodyH)
	} else if m.mode == modeSessions && m.sessPick != nil {
		body = m.sessPick.lines(w, bodyH, m.now())
	} else if m.mode == modeForm && m.form != nil {
		body = m.formLines(w, bodyH)
	} else if m.mode == modeNotes && w < minSplitWidth {
		// Too narrow for the split: the details pane (with the editor)
		// takes the whole body while editing.
		body = m.detailLines(w, bodyH)
	} else {
		lw := w
		if w >= minSplitWidth {
			lw = listWidth(w)
		}
		left := m.listLines(lw, bodyH)
		if w >= minSplitWidth {
			rw := w - lw - 3
			right := m.detailLines(rw, bodyH)
			sep := sepStyle.Render(" │ ")
			for i := 0; i < bodyH; i++ {
				var l, r string
				if i < len(left) {
					l = left[i]
				}
				if i < len(right) {
					r = right[i]
				}
				body = append(body, fit(l, lw)+sep+trunc(r, rw))
			}
		} else {
			body = left
		}
	}
	for len(body) < bodyH {
		body = append(body, "")
	}
	body = body[:bodyH]
	lines := append(body, m.statusLine(w), m.helpLine(w))
	if len(lines) > h {
		lines = lines[len(lines)-h:]
	}
	return strings.Join(lines, "\n")
}

// listWidth is the task list's width in a split view of width w.
func listWidth(w int) int {
	lw := w * 2 / 5
	if lw < 24 {
		lw = 24
	}
	if lw > 48 {
		lw = 48
	}
	return lw
}

func (m *Model) listLines(w, h int) []string {
	if h <= 0 {
		return nil
	}
	if len(m.rows) == 0 {
		msg := "loading…"
		if m.loaded {
			msg = "no tasks — press n to create one"
			if m.filter != "" {
				msg = "no tasks match /" + clean(m.filter)
			}
		}
		return []string{dimStyle.Render(trunc(msg, w))}
	}
	end := m.offset + h
	if end > len(m.rows) {
		end = len(m.rows)
	}
	var out []string
	for i := m.offset; i < end; i++ {
		out = append(out, m.rowLine(m.rows[i], i == m.selIdx, w))
	}
	return out
}

func (m *Model) rowLine(r row, sel bool, w int) string {
	if r.header {
		label := strings.ToUpper(string(r.status)) + " (" + strconv.Itoa(r.count)
		if r.total > r.count {
			label += "/" + strconv.Itoa(r.total)
		}
		label += ")"
		return headerStyle.Render(trunc(label, w))
	}
	t := r.task
	glyph, dotStyle := dotFor(aggregate(t.Sessions))
	busy := ""
	if _, ok := m.busy[t.Task.ID]; ok {
		busy = " …"
	}
	star := ""
	if t.Task.Status.Open() && m.dirty[t.Task.ID] {
		star = "*"
	}
	// The list shows the title only (the slug is in the details pane); a
	// task with a blank title falls back to its slug.
	title := clean(t.Task.Title)
	if title == "" {
		title = clean(t.Task.Slug)
	}
	// Width budget after the dot and busy suffix; the star is kept even
	// when the title has to be truncated.
	avail := w - 2 - ansi.StringWidth(busy)
	titleT := trunc(title, avail-len(star))
	if avail-ansi.StringWidth(titleT)-len(star) < 0 || titleT == "" {
		star = ""
	}
	base, starStyle := lipgloss.NewStyle(), markStyle
	if sel {
		base, starStyle = selStyle, markStyle.Reverse(true)
	}
	text := titleT + star + busy
	pad := ""
	if sel {
		if n := w - 2 - ansi.StringWidth(text); n > 0 {
			pad = strings.Repeat(" ", n)
		}
	}
	line := base.Render(titleT)
	if star != "" {
		line += starStyle.Render(star)
	}
	if s := busy + pad; s != "" {
		line += base.Render(s)
	}
	// The dot keeps its colour on the selected row; the space after it
	// takes the row's base style so the highlight is continuous.
	sep := base.Render(" ")
	if w < 2 {
		return trunc(dotStyle.Render(glyph)+sep, w)
	}
	return dotStyle.Render(glyph) + sep + line
}

// detailLines renders the details pane (width w, height h) of the task
// being edited, else the selected one: its details, then the notes area
// filling the remaining height.
func (m *Model) detailLines(w, h int) []string {
	t := m.notesTask()
	if t == nil || w <= 0 || h <= 0 {
		return nil
	}
	info := m.infoLines(t, w)
	keep, notesH := notesLayout(len(info), h)
	out := append(info[:keep:keep], m.notesLines(t, w, notesH)...)
	if len(out) > h {
		out = out[:h]
	}
	return out
}

// infoLines renders task t's details (title … sessions) at width w.
func (m *Model) infoLines(t *app.TaskView, w int) []string {
	if t == nil {
		return nil
	}
	task := t.Task
	var out []string
	add := func(s string) { out = append(out, trunc(s, w)) }
	kv := func(k, v string) { add(labelStyle.Render(fmt.Sprintf("%-9s", k)) + " " + v) }

	add(titleStyle.Render(clean(task.Title)))
	kv("slug", clean(task.Slug))
	kv("status", clean(string(task.Status)))
	kv("project", orDash(clean(task.Project)))
	kv("branch", orDash(clean(task.Branch)))
	kv("ports", "+"+strconv.Itoa(task.PortOffset))
	kv("dir", clean(m.be.TaskDir(task.Slug)))
	if d := clean(firstLine(task.Description)); d != "" {
		kv("desc", d)
	}

	add("")
	add(headerStyle.Render("Repos"))
	var status []app.RepoStatus
	if m.detail != nil && m.detail.Task.ID == task.ID {
		status = m.detail.RepoStatus
	}
	if len(status) == 0 {
		for _, r := range task.Repos {
			add(fmt.Sprintf("  %s  %s", clean(string(r.Mode)), clean(r.Name)) + baseSuffix(r.BaseBranch))
		}
	}
	for _, r := range status {
		line := fmt.Sprintf("  %s  %s", clean(string(r.Mode)), clean(r.Name))
		switch {
		case r.Err == "missing" && !task.Status.Open():
			// done/cancelled tasks have their worktrees removed
			line += dimStyle.Render("  removed")
		case r.Err == "missing":
			line += errStyle.Render("  missing")
		case r.Err != "":
			line += errStyle.Render("  error: " + clean(firstLine(r.Err)))
		case r.Dirty:
			line += markStyle.Render(" *")
		}
		add(line + baseSuffix(r.BaseBranch))
	}
	if len(task.Repos) == 0 && len(status) == 0 {
		add(dimStyle.Render("  none"))
	}

	add("")
	add(headerStyle.Render("Sessions"))
	if len(t.Sessions) == 0 {
		add(dimStyle.Render("  none — s starts one"))
	}
	now := m.now()
	for _, s := range t.Sessions {
		name := clean(s.Name)
		if name == "" {
			name = "claude-" + strconv.FormatInt(s.ID, 10)
		}
		age := "-"
		if !s.LastEventAt.IsZero() {
			age = humanAge(now.Sub(s.LastEventAt))
		}
		line := fmt.Sprintf("%-10s  %-7s  %s", name, clean(string(s.State)), age)
		switch s.State {
		case model.StateWaiting:
			line = waitStyle.Render("▶ " + line)
		case model.StateEnded:
			line = dimStyle.Render("  " + line)
		default:
			line = "  " + line
		}
		add(line)
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (m *Model) statusLine(w int) string {
	var s string
	switch {
	case m.mode == modeConfirm && m.confirm != nil:
		c := m.confirm
		if c.dirty != "" {
			s = "F force " + c.verb + ", any other key aborts — " + errStyle.Render(clean(c.dirty))
		} else {
			s = fmt.Sprintf("%s %s? y/N", c.verb, clean(c.slug))
		}
		return trunc(s, w)
	case m.mode == modeFilter:
		return trunc(m.filterInput.View(), w)
	case m.mode == modeNotes && m.notes != nil && m.notes.ask != askNone:
		q := "discard your changes to the notes of " + clean(m.notes.slug) + "? y/n"
		if m.notes.ask == askQuit {
			q = "quit and discard your changes to the notes of " + clean(m.notes.slug) + "? y/n"
		}
		return trunc(markStyle.Render(q), w)
	case m.mode == modeRename:
		return trunc(m.renameInput.View(), w)
	}
	if ops := m.busyOps(); len(ops) > 0 {
		s = markStyle.Render("working… ") + clean(strings.Join(ops, ", "))
		if m.status != "" {
			s += "  "
		}
	}
	if st := clean(m.status); st != "" {
		if m.statusErr {
			s += errStyle.Render(st)
		} else {
			s += st
		}
	}
	if s == "" && m.filter != "" {
		s = dimStyle.Render("filter: /" + clean(m.filter) + "  (esc clears)")
	}
	return trunc(s, w)
}

// baseSuffix shows the origin branch a rw repo's task branch started from
// ("" when unknown, e.g. ro repos).
func baseSuffix(branch string) string {
	if branch == "" {
		return ""
	}
	return dimStyle.Render("  ← " + clean(branch))
}

func (m *Model) helpLine(w int) string {
	var s string
	switch m.mode {
	case modeForm:
		s = "enter/↓ next  ↑ prev  tab next/complete project  enter/space on Repos picks  space/←/→ on Base toggles  ctrl+b backlog  enter on Backlog creates  esc cancel"
	case modePicker:
		s = "type to filter  ↑/↓ move  space none/rw/ro  enter confirm  esc clear filter/cancel"
	case modeSessions:
		s = "↑/↓ move  enter open (live: attach, past: resume)  esc cancel"
	case modeFilter:
		s = "type to filter  enter apply  esc clear  ↑/↓ move"
	case modeNotes:
		e := m.notes
		switch {
		case e == nil:
		case e.ask != askNone:
			s = "y discard and leave  any other key: back to editing"
		case e.conflict:
			return errStyle.Render(trunc("notes changed meanwhile: ctrl+s overwrite with your text  ctrl+r reload theirs (drops your edits)  esc leave", w))
		default:
			s = "editing notes of " + clean(e.slug) + ": ctrl+s save  esc leave (asks if unsaved)  ctrl+r reload (drops edits)  enter new line  ↑/↓ move  ctrl+c quit"
		}
	case modeRename:
		if m.statusErr && m.status != "" {
			// a refused title: the error replaces the help until the next edit
			return errStyle.Render(trunc(clean(m.status), w))
		}
		s = "new title for " + clean(m.renameSlug) + " (slug, branch and dirs stay)  enter save  esc cancel"
	case modeConfirm:
		s = ""
	default:
		s = "n new  enter open  s session  r resume  a add repo  m edit notes  t rename  d done  c cancel  p pr  D diff  A assistant  1-5 status  / filter  q quit"
		for _, l := range legends {
			if ansi.StringWidth(s)+2+ansi.StringWidth(l) <= w {
				return dimStyle.Render(s+"  ") + l
			}
		}
	}
	return dimStyle.Render(trunc(s, w))
}

// formInputWidth is the width of the form's text inputs.
func (m *Model) formInputWidth() int {
	w := m.width - 4 - 14
	if w > 60 {
		w = 60
	}
	if w < 1 {
		w = 1
	}
	return w
}

func (m *Model) formLines(w, h int) []string {
	f := m.form
	var out []string
	add := func(s string) { out = append(out, trunc(s, w)) }
	add(titleStyle.Render("New task"))
	add("")
	for i := 0; i < numFields; i++ {
		cursor := "  "
		label := labelStyle.Render(fmt.Sprintf("%-12s", fieldLabels[i]))
		if i == f.focus {
			cursor = markStyle.Render("> ")
			label = headerStyle.Render(fmt.Sprintf("%-12s", fieldLabels[i]))
		}
		var val string
		switch i {
		case fieldBacklog:
			val = "[ ]"
			if f.backlog {
				val = "[x]"
			}
			val += dimStyle.Render(" space / ctrl+b toggles")
		case fieldBase:
			auto, prod := "(•) auto (dev → develop → default)", "( ) production patch (default branch)"
			if f.prodBase {
				auto, prod = "( ) auto (dev → develop → default)", "(•) production patch (default branch)"
			}
			val = auto + "   " + prod
		case fieldRepos:
			switch {
			case len(f.repos) > 0:
				val = summary(f.repos)
			case f.project() != "" && f.project() != f.preProject:
				val = dimStyle.Render("project " + clean(f.project()) + "'s repos")
			default:
				val = dimStyle.Render("none")
			}
		default:
			val = f.inputs[i].View()
		}
		add(cursor + label + val)
		if i == fieldProject && i == f.focus {
			hint := "no projects configured"
			if len(f.projects) > 0 {
				hint = "projects: " + clean(strings.Join(f.projects, ", "))
			}
			add(dimStyle.Render("                " + hint))
		}
		if i == fieldProject && f.projErr != "" && f.projErrFor == f.project() {
			add(errStyle.Render("                " + clean(f.projErr)))
		}
		if i == fieldRepos && i == f.focus {
			add(dimStyle.Render("                enter or space opens the repo picker"))
		}
		if i == fieldBase {
			add(dimStyle.Render("                " + baseHint))
		}
	}
	add("")
	if f.err != "" {
		add(errStyle.Render(f.err))
	}
	if len(out) > h {
		// keep the focused field visible on very short terminals
		start := f.focus + 2 - h + 1
		if start < 0 {
			start = 0
		}
		if start+h > len(out) {
			start = len(out) - h
		}
		out = out[start : start+h]
	}
	return out
}
