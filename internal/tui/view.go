package tui

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/andreujuanc/atelier/internal/app"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/palette"
)

// dotFor returns the glyph (one cell) and style of a task row's state dot.
func dotFor(k dotKind) (string, lipgloss.Style) {
	st := k.state()
	if st == "" {
		return " ", lipgloss.NewStyle()
	}
	return palette.StateLook(st).Glyph, stateStyle(st)
}

// legends explain the list row marks (help line): the first that fits is
// shown, none when neither does. Built by applyTheme.
var legends []string

func buildLegends() []string {
	dots := dimStyle.Render("● ") + waitStyle.Render("waiting") + dimStyle.Render("/") +
		runStyle.Render("running") + dimStyle.Render("/") + idleStyle.Render("idle")
	star := "  " + warnStyle.Render("*") + dimStyle.Render(" uncommitted")
	return []string{
		dots + "  " + stopStyle.Render("○") + dimStyle.Render(" stopped") + star,
		dots + star,
	}
}

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

// --- layout ---

// layout is where the parts of the main screen go for the current size.
type layout struct {
	header bool // the top bar is shown
	bodyH  int  // rows between the top bar and the status line
	boxed  bool // the panes have borders
	split  bool // the details pane is shown next to the list
	// outer widths of the panes (the list takes the whole width unsplit)
	listW, detailW int
	// what the panes hold inside their borders
	listInW, listInH, detailInW, detailInH int
}

func (m *Model) layout() layout {
	w, h := m.width, m.height
	var l layout
	l.header = h >= 10
	l.bodyH = h - 1 - len(m.helpLines(w))
	if l.header {
		l.bodyH--
	}
	if l.bodyH < 0 {
		l.bodyH = 0
	}
	l.boxed = w >= 24 && l.bodyH >= 5
	l.split = w >= minSplitWidth
	inset, insetH := 0, 0
	if l.boxed {
		inset, insetH = 4, 2
	}
	switch {
	case l.split && l.boxed:
		l.listW = listWidth(w)
		l.detailW = w - l.listW - 1 // a column between the boxes
	case l.split:
		l.listW = listWidth(w)
		l.detailW = w - l.listW - 3 // " │ "
	default:
		l.listW, l.detailW = w, w
	}
	l.listInW, l.detailInW = max(l.listW-inset, 1), max(l.detailW-inset, 1)
	l.listInH = max(l.bodyH-insetH, 0)
	l.detailInH = l.listInH
	return l
}

// listWidth is the task list's width in a split view of width w.
func listWidth(w int) int {
	lw := w * 2 / 5
	if lw < 26 {
		lw = 26
	}
	if lw > 52 {
		lw = 52
	}
	return lw
}

// panel draws lines in a rounded box w×h with title set in its top border;
// focused boxes are drawn in the primary colour. h < 2 draws nothing.
func panel(title, right string, lines []string, w, h int, focused bool) []string {
	return panelB(title, right, "", lines, w, h, focused)
}

// panelB is panel with bottom set at the right of its bottom border.
func panelB(title, right, bottom string, lines []string, w, h int, focused bool) []string {
	if w < 4 || h < 2 {
		return nil
	}
	bc := border
	if focused {
		bc = primary
	}
	bs := lipgloss.NewStyle().Foreground(bc)
	ts := lipgloss.NewStyle().Bold(true)
	if focused {
		ts = ts.Foreground(primary)
	}
	inner := w - 2
	// ╭─ title ───── right ─╮
	top := bs.Render("╭─")
	used := 1 // of inner: the dash after the corner
	if t := clean(title); t != "" && inner > 6 {
		t = trunc(t, inner-5)
		top += " " + ts.Render(t) + " "
		used += ansi.StringWidth(t) + 2
	}
	rw := ansi.StringWidth(right)
	if right == "" || used+rw+3 > inner {
		right, rw = "", 0
	}
	if right != "" {
		top += bs.Render(strings.Repeat("─", inner-used-rw-3)) + " " + right + " " + bs.Render("─╮")
	} else {
		top += bs.Render(strings.Repeat("─", max(inner-used, 0)) + "╮")
	}
	out := make([]string, 0, h)
	out = append(out, top)
	side := bs.Render("│")
	for i := 0; i < h-2; i++ {
		l := ""
		if i < len(lines) {
			l = lines[i]
		}
		out = append(out, side+" "+fit(l, inner-2)+" "+side)
	}
	if bw := ansi.StringWidth(bottom); bottom != "" && bw+4 <= inner {
		return append(out, bs.Render("╰"+strings.Repeat("─", inner-bw-3))+" "+bottom+" "+bs.Render("─╯"))
	}
	return append(out, bs.Render("╰"+strings.Repeat("─", inner)+"╯"))
}

// rule is a section heading: the label, then a line to width w.
func rule(label string, w int) string {
	l := headerStyle.Render(label)
	n := w - ansi.StringWidth(label) - 1
	if n <= 0 {
		return trunc(l, w)
	}
	return l + " " + ruleStyle.Render(strings.Repeat("─", n))
}

// render draws the screen.
func (m *Model) render() string {
	w, h := m.width, m.height
	if w <= 0 || h <= 0 {
		return ""
	}
	l := m.layout()
	help := m.helpLines(w)
	var lines []string
	if l.header {
		lines = append(lines, m.headerLine(w))
	}
	lines = append(lines, m.bodyLines(l, w)...)
	if over := m.overlay(w, len(lines)); over != nil {
		lines = composite(lines, over, w)
	}
	lines = append(append(lines, m.statusLine(w)), help...)
	if len(lines) > h {
		lines = lines[len(lines)-h:]
	}
	return strings.Join(lines, "\n")
}

// bodyLines draws the panes (exactly l.bodyH lines).
func (m *Model) bodyLines(l layout, w int) []string {
	h := l.bodyH
	if h <= 0 {
		return nil
	}
	editing := m.mode == modeNotes
	var body []string
	switch {
	case !l.split && editing:
		// Too narrow for the split: the details pane (with the editor)
		// takes the whole body while editing.
		body = m.detailPane(l, w, h, true)
	case !l.split:
		body = m.listPane(l, w, h)
	case l.boxed:
		left := m.listPane(l, l.listW, h)
		right := m.detailPane(l, l.detailW, h, editing)
		for i := 0; i < h; i++ {
			body = append(body, fit(at(left, i), l.listW)+" "+at(right, i))
		}
	default:
		left := m.listLines(l.listInW, h)
		right := m.detailLines(l.detailInW, h)
		sep := sepStyle.Render(" │ ")
		for i := 0; i < h; i++ {
			body = append(body, fit(at(left, i), l.listW)+sep+trunc(at(right, i), l.detailW))
		}
	}
	for len(body) < h {
		body = append(body, "")
	}
	return body[:h]
}

func at(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return ""
}

// listPane is the task list, boxed when there is room.
func (m *Model) listPane(l layout, w, h int) []string {
	if !l.boxed {
		return m.listLines(w, h)
	}
	inner := m.listLines(w-4, h-2)
	bottom := m.scrollHint(h - 2)
	n := 0
	for _, r := range m.rows {
		if r.isTask() {
			n++
		}
	}
	title := "Tasks"
	right := ""
	if m.filter != "" {
		right = dimStyle.Render("/"+clean(m.filter)) + " " + labelStyle.Render(fmt.Sprintf("%d/%d", n, len(m.tasks)))
	} else if m.loaded {
		right = labelStyle.Render(strconv.Itoa(n))
	}
	return panelB(title, right, bottom, inner, w, h, m.mode != modeNotes)
}

// detailPane is the details pane, boxed when there is room.
func (m *Model) detailPane(l layout, w, h int, focused bool) []string {
	if !l.boxed {
		return m.detailLines(w, h)
	}
	inner := m.detailLines(w-4, h-2)
	title := ""
	if t := m.notesTask(); t != nil {
		title = t.Task.Slug
	}
	right := ""
	if t := m.notesTask(); t != nil {
		if op, ok := m.busy[t.Task.ID]; ok {
			right = keyStyle.Render(m.spinner()) + " " + dimStyle.Render(trunc(clean(op), max(w/3, 8)))
		}
	}
	return panel(title, right, inner, w, h, focused)
}

// headerLine is the top bar: the brand and workspace at the left, what the
// sessions are doing at the right.
func (m *Model) headerLine(w int) string {
	left := brandStyle.Render(" atelier ")
	if ws := clean(m.workspace); ws != "" {
		left += " " + titleStyle.Render(ws)
	}
	counts := map[model.SessionState]int{}
	open := 0
	for _, t := range m.tasks {
		if !t.Task.Status.Open() {
			continue
		}
		open++
		for _, s := range t.Sessions {
			counts[s.State]++
		}
	}
	var parts []string
	for _, st := range []model.SessionState{model.StateWaiting, model.StateRunning, model.StateIdle, model.StateStopped} {
		if n := counts[st]; n > 0 {
			look := palette.StateLook(st)
			parts = append(parts, stateStyle(st).Render(look.Glyph)+" "+
				stateStyle(st).Render(strconv.Itoa(n))+dimStyle.Render(" "+string(st)))
		}
	}
	if m.loaded {
		noun := "open tasks"
		if open == 1 {
			noun = "open task"
		}
		parts = append(parts, dimStyle.Render(strconv.Itoa(open)+" "+noun))
	}
	// Drop items from the left of the right side until it fits.
	for len(parts) > 0 {
		right := strings.Join(parts, dimStyle.Render("  "))
		if pad := w - ansi.StringWidth(left) - ansi.StringWidth(right) - 1; pad >= 2 {
			return left + strings.Repeat(" ", pad) + right + " "
		}
		parts = parts[1:]
	}
	return trunc(left, w)
}

func (m *Model) listLines(w, h int) []string {
	if h <= 0 {
		return nil
	}
	if len(m.rows) == 0 {
		if !m.loaded {
			return []string{dimStyle.Render(trunc(m.spinner()+" loading…", w))}
		}
		if m.filter != "" {
			return []string{dimStyle.Render(trunc("no tasks match /"+clean(m.filter), w)),
				"", descStyle.Render(trunc("esc clears the filter", w))}
		}
		return []string{dimStyle.Render(trunc("no tasks — press n to create one", w)), "",
			trunc(keyStyle.Render("n")+descStyle.Render(" new task  ")+keyStyle.Render("A")+descStyle.Render(" assistant"), w)}
	}
	off, end := m.listWindow(h)
	var out []string
	for i := off; i < end; i++ {
		out = append(out, m.rowLine(m.rows[i], i == m.selIdx, w))
	}
	return out
}

func (m *Model) rowLine(r row, sel bool, w int) string {
	if r.header {
		c := lipgloss.NewStyle().Foreground(statusColor(r.status)).Bold(true)
		label := strings.ToUpper(string(r.status))
		count := strconv.Itoa(r.count)
		if r.total > r.count {
			count += "/" + strconv.Itoa(r.total)
		}
		text := " " + c.Render(label) + " " + labelStyle.Render(count)
		used := ansi.StringWidth(" "+label+" "+count) + 1
		if n := w - used; n > 0 {
			text += " " + ruleStyle.Render(strings.Repeat("─", n))
		}
		return trunc(text, w)
	}
	t := r.task
	glyph, dotStyle := dotFor(aggregate(t.Sessions))
	busy := ""
	if _, ok := m.busy[t.Task.ID]; ok {
		busy = " " + m.spinner()
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
	// Width budget after the bar, the dot and busy suffix; the star is kept
	// even when the title has to be truncated.
	avail := w - 3 - ansi.StringWidth(busy)
	titleT := trunc(title, avail-len(star))
	if avail-ansi.StringWidth(titleT)-len(star) < 0 || titleT == "" {
		star = ""
	}
	base, starStyle, busyStyle := lipgloss.NewStyle(), warnStyle, keyStyle
	switch {
	case !t.Task.Status.Open():
		base = dimStyle
	case aggregate(t.Sessions) == dotWaiting:
		base = base.Bold(true) // a session waits for you: stand out
	}
	lead := " "
	if sel {
		base = onSelected(base).Bold(true)
		starStyle = onSelected(starStyle)
		busyStyle = onSelected(busyStyle)
		dotStyle = dotStyle.Background(surface)
		lead = barStyle.Render("▌")
	}
	line := base.Render(titleT)
	if star != "" {
		line += starStyle.Render(star)
	}
	if busy != "" {
		line += busyStyle.Render(busy)
	}
	// The last session activity at the right, when there is room.
	age := ""
	if a := lastActivity(t.Sessions); !a.IsZero() && t.Task.Status.Open() {
		age = humanAge(m.now().Sub(a))
	}
	used := ansi.StringWidth(titleT + star + busy)
	if age != "" && used+2+len(age) <= w-3 {
		as := dimStyle
		pad := strings.Repeat(" ", w-3-used-len(age))
		if sel {
			as, pad = onSelected(dimStyle), selStyle.Render(pad)
		}
		line += pad + as.Render(age)
	} else if sel {
		if n := w - 3 - used; n > 0 {
			line += selStyle.Render(strings.Repeat(" ", n))
		}
	}
	// The dot keeps its colour on the selected row; the space after it
	// takes the row's base style so the highlight is continuous.
	sep := " "
	if sel {
		sep = selStyle.Render(" ")
	}
	if w < 3 {
		return trunc(lead+dotStyle.Render(glyph), w)
	}
	return lead + dotStyle.Render(glyph) + sep + line
}

// detailLines renders the details pane (width w, height h) of the task
// being edited, else the selected one: its details, then the notes area
// filling the remaining height.
func (m *Model) detailLines(w, h int) []string {
	t := m.notesTask()
	if w <= 0 || h <= 0 {
		return nil
	}
	if t == nil {
		if len(m.rows) == 0 {
			if m.loaded && m.filter == "" && len(m.tasks) == 0 {
				return welcomeLines(w)
			}
			return nil
		}
		return []string{dimStyle.Render(trunc("no task selected", w))}
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

	title := clean(task.Title)
	if title == "" {
		title = clean(task.Slug)
	}
	// The title, the status at the right when there is room.
	st := chip(string(task.Status), statusColor(task.Status))
	if m.dirty[task.ID] && task.Status.Open() {
		st = warnStyle.Render("*") + " " + st
	}
	if sw := ansi.StringWidth(st); w-sw-2 >= 12 {
		tt := trunc(title, w-sw-2)
		add(titleStyle.Render(tt) + strings.Repeat(" ", w-ansi.StringWidth(tt)-sw) + st)
	} else {
		add(titleStyle.Render(title))
	}
	kv("slug", clean(task.Slug))
	kv("status", lipgloss.NewStyle().Foreground(statusColor(task.Status)).Render(clean(string(task.Status))))
	if p := clean(task.Project); p != "" {
		kv("project", lipgloss.NewStyle().Foreground(purple).Render(p))
	}
	kv("branch", orDash(clean(task.Branch)))
	kv("ports", "+"+strconv.Itoa(task.PortOffset))
	kv("dir", clean(m.be.TaskDir(task.Slug)))
	if d := clean(firstLine(task.Description)); d != "" {
		kv("desc", d)
	}

	add(rule("Repos", w))
	var status []app.RepoStatus
	if m.detail != nil && m.detail.Task.ID == task.ID {
		status = m.detail.RepoStatus
	}
	nameW := 0
	for _, r := range task.Repos {
		nameW = max(nameW, ansi.StringWidth(clean(r.Name)))
	}
	for _, r := range status {
		nameW = max(nameW, ansi.StringWidth(clean(r.Name)))
	}
	repoLine := func(mode model.Mode, name string) string {
		ms := dimStyle
		if mode == model.ModeRW {
			ms = keyStyle
		}
		return " " + ms.Render(fmt.Sprintf("%-2s", clean(string(mode)))) + "  " + fmt.Sprintf("%-*s", nameW, clean(name))
	}
	if len(status) == 0 {
		for _, r := range task.Repos {
			add(repoLine(r.Mode, r.Name) + baseSuffix(r.BaseBranch))
		}
	}
	for _, r := range status {
		line := repoLine(r.Mode, r.Name)
		switch {
		case r.Err == "missing" && !task.Status.Open():
			// done/cancelled tasks have their worktrees removed
			line += dimStyle.Render("  removed")
		case r.Err == "missing":
			line += errStyle.Render("  missing")
		case r.Err != "":
			line += errStyle.Render("  error: " + clean(firstLine(r.Err)))
		case r.Dirty:
			line += warnStyle.Render(" *")
		}
		add(line + baseSuffix(r.BaseBranch))
	}
	if len(task.Repos) == 0 && len(status) == 0 {
		add(dimStyle.Render(" none"))
	}

	add(rule("Sessions", w))
	if len(t.Sessions) == 0 {
		add(dimStyle.Render(" none — ") + keyStyle.Render("s") + dimStyle.Render(" starts one"))
	}
	now := m.now()
	sessW := 0
	for _, s := range t.Sessions {
		sessW = max(sessW, ansi.StringWidth(sessionName(s)))
	}
	for _, s := range t.Sessions {
		age := "-"
		if !s.LastEventAt.IsZero() {
			age = humanAge(now.Sub(s.LastEventAt))
		}
		glyph := palette.StateLook(s.State).Glyph
		if glyph == " " {
			glyph = "·"
		}
		st := stateStyle(s.State)
		name := fmt.Sprintf("%-*s", sessW, sessionName(s))
		state := fmt.Sprintf("%-7s", clean(string(s.State)))
		var line string
		switch s.State {
		case model.StateWaiting:
			line = " " + st.Render(glyph+" "+name+"  "+state) + "  " + dimStyle.Render(age) + "  " + waitStyle.Render("◀ needs you")
		case model.StateEnded:
			line = dimStyle.Render(" " + glyph + " " + name + "  " + state + "  " + age)
		default:
			line = " " + st.Render(glyph) + " " + name + "  " + st.Render(state) + "  " + dimStyle.Render(age)
		}
		add(line)
	}
	return out
}

// sessionName is s's window name, else its row id.
func sessionName(s model.Session) string {
	if n := clean(s.Name); n != "" {
		return n
	}
	return "claude-" + strconv.FormatInt(s.ID, 10)
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
	case m.mode == modeFilter:
		return trunc(m.filterInput.View(), w)
	case m.mode == modeNotes && m.notes != nil && m.notes.ask != askNone:
		what := "your changes"
		if m.notes.saving {
			what = "your newer edits (the save in progress goes on)"
		}
		q := "discard " + what + " to the notes of " + clean(m.notes.slug) + "? y/n"
		if m.notes.ask == askQuit {
			q = "quit and discard " + what + " to the notes of " + clean(m.notes.slug) + "? y/n"
		}
		return trunc(markStyle.Render("? "+q), w)
	case m.mode == modeRename:
		return trunc(m.renameInput.View(), w)
	}
	if ops := m.busyOps(); len(ops) > 0 {
		s = keyStyle.Render(m.spinner()+" working… ") + dimStyle.Render(clean(strings.Join(ops, ", ")))
		if m.status != "" {
			s += "  "
		}
	}
	if st := clean(m.status); st != "" {
		switch {
		case m.statusErr:
			s += errStyle.Render("✗ " + st)
		case m.statusOK:
			s += okStyle.Render("✓ ") + st
		default:
			s += st
		}
	}
	// At the right, when they fit: the filter, and where the keys are.
	var tags []string
	if m.filter != "" {
		tags = append(tags, dimStyle.Render("filter /"+clean(m.filter))+descStyle.Render(" (esc clears)"))
	}
	if m.mode == modeNormal {
		tags = append(tags, keyStyle.Render("?")+descStyle.Render(" all keys"))
	}
	for len(tags) > 0 {
		tag := strings.Join(tags, "   ")
		if pad := w - ansi.StringWidth(s) - ansi.StringWidth(tag); pad >= 2 {
			return s + strings.Repeat(" ", pad) + tag
		}
		tags = tags[:len(tags)-1]
	}
	return trunc(s, w)
}

// confirmWrap is the widest the done/cancel dialog's text runs.
const confirmWrap = 72

// lines renders the done/cancel dialog's text (width w, height h).
func (c *confirmState) lines(w, h int) []string {
	if w <= 0 || h <= 0 {
		return nil
	}
	ww := w
	if ww > confirmWrap {
		ww = confirmWrap
	}
	wrap := func(s string, st lipgloss.Style) []string {
		var out []string
		for _, l := range strings.Split(ansi.Wrap(s, ww, ""), "\n") {
			out = append(out, st.Render(trunc(strings.TrimRight(l, " "), w)))
		}
		return out
	}
	head := "Mark " + clean(c.slug) + " done?"
	if c.verb == "cancel" {
		head = "Cancel " + clean(c.slug) + "?"
	}
	blocks := [][]string{
		wrap(head, titleStyle),
		wrap("This deletes the task's worktrees and its task dir. Files that are not committed "+
			"cannot be recovered. Commits stay on the task branch.", lipgloss.NewStyle()),
	}
	if c.dirty {
		blocks = append(blocks, wrap("There are uncommitted changes. They will be deleted.", errStyle.Bold(true)))
	}
	box := dimStyle.Render("[ ]") + " Delete them"
	if c.ticked {
		box = errStyle.Bold(true).Render("[x]") + " Delete them"
	}
	last := []string{trunc(box, w)}
	if c.unticked {
		last = append(last, wrap("Tick the box with space to confirm.", markStyle)...)
	}
	blocks = append(blocks, last)
	join := func(sep bool) []string {
		var out []string
		for i, b := range blocks {
			if sep && i > 0 {
				out = append(out, "")
			}
			out = append(out, b...)
		}
		return out
	}
	out := join(true)
	if len(out) > h {
		out = join(false)
	}
	if len(out) > h {
		out = out[len(out)-h:] // keep the box visible on very short terminals
	}
	return out
}

// baseSuffix shows the origin branch a rw repo's task branch started from
// ("" when unknown, e.g. ro repos).
func baseSuffix(branch string) string {
	if branch == "" {
		return ""
	}
	return dimStyle.Render("  ← " + clean(branch))
}

// helpItem is a key and what it does; a blank key is plain text.
type helpItem struct{ key, desc string }

// listHelp is the help of the task list: its keys.
var listHelp = []helpItem{
	{"n", "new"}, {"enter", "open"}, {"s", "session"}, {"r", "resume"}, {"a", "add repo"},
	{"m", "notes"}, {"t", "rename"}, {"d", "done"}, {"c", "cancel"}, {"p", "pr"}, {"D", "diff"},
	{"A", "assistant"}, {"1-5", "status"}, {"/", "filter"}, {"w", "workspace"}, {"q", "quit"},
}

// listKeys is listHelp as plain text.
var listKeys = helpPlain(listHelp)

func (h helpItem) plain() string {
	switch {
	case h.key == "":
		return h.desc
	case h.desc == "":
		return h.key
	}
	return h.key + " " + h.desc
}

func (h helpItem) styled() string {
	switch {
	case h.key == "":
		return descStyle.Render(h.desc)
	case h.desc == "":
		return keyStyle.Render(h.key)
	}
	return keyStyle.Render(h.key) + descStyle.Render(" "+h.desc)
}

func helpPlain(items []helpItem) string {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = it.plain()
	}
	return strings.Join(parts, "  ")
}

// helpStyled renders items at width w, truncated with "…".
func helpStyled(items []helpItem, w int) string {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = it.styled()
	}
	return trunc(strings.Join(parts, "  "), w)
}

// helpLines is the help under the status line: one line, but the task
// list's keys wrap onto a second one when they do not fit the width (and
// the terminal has 8 lines or more), at a two-space gap between keys. The
// legend (see legends) follows the keys on their last line when it fits.
func (m *Model) helpLines(w int) []string {
	if m.mode != modeNormal || m.height < 8 || ansi.StringWidth(listKeys) <= w {
		return []string{m.helpLine(w)}
	}
	n := 1 // keys on the first line: as many as fit, at least one
	for n < len(listHelp) && ansi.StringWidth(helpPlain(listHelp[:n+1])) <= w {
		n++
	}
	groups := [][]helpItem{listHelp[:n]}
	if n < len(listHelp) {
		groups = append(groups, listHelp[n:]) // truncated when it does not fit either
	}
	out := make([]string, len(groups))
	for i, g := range groups {
		out[i] = helpStyled(g, w)
	}
	last := helpPlain(groups[len(groups)-1])
	for _, l := range legends {
		if ansi.StringWidth(last)+3+ansi.StringWidth(l) <= w {
			out[len(out)-1] += "   " + l
			break
		}
	}
	return out
}

func (m *Model) helpLine(w int) string {
	var items []helpItem
	switch m.mode {
	case modeForm:
		items = []helpItem{{"enter/↓", "next"}, {"↑", "prev"}, {"tab", "next/complete project"},
			{"enter/space", "on Repos picks"}, {"space/←/→", "on Base toggles"}, {"ctrl+b", "backlog"},
			{"enter", "on Backlog creates"}, {"esc", "cancel"}}
	case modePicker:
		items = []helpItem{{"", "type to filter"}, {"↑/↓", "move"}, {"space", "none/rw/ro"},
			{"enter", "confirm"}, {"esc", "clear filter/cancel"}}
	case modeSessions:
		items = []helpItem{{"↑/↓", "move"}, {"enter", "open (live: attach, past: resume)"}, {"esc", "cancel"}}
	case modeFilter:
		items = []helpItem{{"", "type to filter"}, {"enter", "apply"}, {"esc", "clear"}, {"↑/↓", "move"}}
	case modeHelp:
		items = []helpItem{{"", "any key closes"}}
	case modeNotes:
		e := m.notes
		switch {
		case e == nil:
		case e.ask == askQuit:
			items = []helpItem{{"y", "quit atl (unsaved notes are lost)"}, {"", "any other key: back to editing"}}
		case e.ask != askNone:
			items = []helpItem{{"y", "discard and leave"}, {"", "any other key: back to editing"}}
		case e.conflict:
			return errStyle.Render(trunc("notes changed meanwhile: ctrl+s overwrite with your text  ctrl+r reload theirs (drops your edits)  esc leave", w))
		default:
			items = []helpItem{{"", "editing notes of " + clean(e.slug) + ":"}, {"ctrl+s", "save"},
				{"esc", "leave (asks if unsaved)"}, {"ctrl+r", "reload (drops edits)"}, {"enter", "new line"},
				{"↑/↓", "move"}, {"ctrl+c", "quit"}}
		}
	case modeNotesLoading:
		slug := ""
		if m.notesLoad != nil {
			slug = m.notesLoad.slug
		}
		items = []helpItem{{"", m.spinner() + " loading the notes of " + clean(slug) + "…"}, {"esc", "cancel"}}
	case modeRename:
		if m.statusErr && m.status != "" {
			// a refused title: the error replaces the help until the next edit
			return errStyle.Render(trunc("✗ "+clean(m.status), w))
		}
		items = []helpItem{{"", "new title for " + clean(m.renameSlug) + " (slug, branch and dirs stay)"},
			{"enter", "save"}, {"esc", "cancel"}}
	case modeConfirm:
		items = []helpItem{{"space", "tick/untick"}, {"enter", "confirm (box ticked)"}, {"esc/n", "cancel"}}
	default:
		s := helpStyled(listHelp, w)
		for _, l := range legends {
			if ansi.StringWidth(listKeys)+3+ansi.StringWidth(l) <= w {
				return s + "   " + l
			}
		}
		return s
	}
	return helpStyled(items, w)
}

// --- overlays ---

// overlay is the dialog drawn over the body (h rows above the status
// line), nil when none is open.
func (m *Model) overlay(w, h int) *dialog {
	if h <= 0 || w <= 0 {
		return nil
	}
	switch {
	case m.mode == modeForm && m.form != nil:
		return m.formDialog(w, h)
	case m.mode == modePicker && m.picker != nil:
		return m.picker.dialog(w, h)
	case m.mode == modeSessions && m.sessPick != nil:
		return m.sessPick.dialog(w, h, m.now())
	case m.mode == modeConfirm && m.confirm != nil:
		return m.confirm.dialog(w, h)
	case m.mode == modeHelp:
		return helpDialog(w, h)
	}
	return nil
}

// dialog is a box to draw centred over the screen.
type dialog struct {
	lines []string // the box, border included
	w     int      // its width
	top   bool     // placed near the top rather than centred
}

// box frames content (inner width iw) as a dialog titled title in colour c.
func box(title string, content []string, iw int, c color.Color) *dialog {
	bs := lipgloss.NewStyle().Foreground(c)
	ts := lipgloss.NewStyle().Foreground(c).Bold(true)
	w := iw + 4
	top := bs.Render("╭─")
	used := 1
	if t := clean(title); t != "" {
		t = trunc(t, max(iw-2, 1))
		top += " " + ts.Render(t) + " "
		used += ansi.StringWidth(t) + 2
	}
	top += bs.Render(strings.Repeat("─", max(w-2-used, 0)) + "╮")
	out := []string{top}
	side := bs.Render("│")
	for _, l := range content {
		out = append(out, side+" "+fit(l, iw)+" "+side)
	}
	out = append(out, bs.Render("╰"+strings.Repeat("─", w-2)+"╯"))
	return &dialog{lines: out, w: w}
}

// composite draws d centred over lines (the screen above the status line,
// width w), the rest dimmed as a backdrop.
func composite(lines []string, d *dialog, w int) []string {
	h := len(lines)
	if d == nil || h == 0 {
		return lines
	}
	bh := len(d.lines)
	if bh > h {
		d.lines = d.lines[:h]
		bh = h
	}
	x := max((w-d.w)/2, 0)
	y := max((h-bh)/2, 0)
	if d.top {
		y = min(1, h-bh)
	}
	bd := lipgloss.NewStyle().Foreground(faint)
	out := make([]string, h)
	for i, l := range lines {
		plain := fit(ansi.Strip(l), w)
		if i < y || i >= y+bh {
			out[i] = bd.Render(plain)
			continue
		}
		left := ansi.Cut(plain, 0, x)
		right := ansi.Cut(plain, x+d.w, w)
		out[i] = bd.Render(left) + trunc(d.lines[i-y], w-x) + bd.Render(right)
	}
	return out
}

// dialogWidth is the inner width of a dialog at most want wide on a
// screen w wide.
func dialogWidth(want, w int) int {
	iw := min(want, w-6)
	if iw < 10 {
		iw = max(w-4, 1)
	}
	return iw
}

func (c *confirmState) dialog(w, h int) *dialog {
	iw := dialogWidth(confirmWrap, w)
	title := "Done"
	if c.verb == "cancel" {
		title = "Cancel task"
	}
	return box(title, c.lines(iw, max(h-2, 1)), iw, errColor)
}

// helpDialog lists every key of the task list with what it does.
func helpDialog(w, h int) *dialog {
	iw := dialogWidth(64, w)
	sections := []struct {
		title string
		items []helpItem
	}{
		{"Tasks", []helpItem{{"n", "new task"}, {"t", "rename the selected task"}, {"1-5", "set the status: backlog active review done cancelled"},
			{"d", "done: close, deleting worktrees"}, {"c", "cancel: close, deleting worktrees"}, {"a", "add repos to the task"},
			{"m", "edit the task's notes"}, {"p", "open pull requests"}, {"D", "show the diff (pager)"}}},
		{"Sessions", []helpItem{{"enter", "open: attach, or pick a session"}, {"s", "start a new session"}, {"r", "resume stopped sessions"},
			{"A", "the atelier assistant"}}},
		{"Moving", []helpItem{{"↑/↓ j/k", "move"}, {"pgup/pgdn", "page"}, {"/", "filter by slug or title"}, {"esc", "clear the filter"},
			{"w", "switch workspace"}, {"q", "quit"}}},
	}
	var out []string
	kw := 10
	for i, s := range sections {
		if i > 0 {
			out = append(out, "")
		}
		out = append(out, rule(s.title, iw))
		for _, it := range s.items {
			out = append(out, trunc(" "+keyStyle.Render(fmt.Sprintf("%-*s", kw, it.key))+" "+it.desc, iw))
		}
	}
	out = append(out, "")
	for _, l := range legends {
		if ansi.StringWidth(l) <= iw {
			out = append(out, l)
			break
		}
	}
	if len(out) > h-2 {
		out = out[:max(h-2, 0)]
	}
	return box("Keys", out, iw, primary)
}

// --- the new-task form ---

// formInputWidth is the width of the form's text inputs.
func (m *Model) formInputWidth() int {
	w := dialogWidth(formWidth, m.width) - 17
	if w > 60 {
		w = 60
	}
	if w < 1 {
		w = 1
	}
	return w
}

// formWidth is the widest the new-task dialog's content runs.
const formWidth = 80

func (m *Model) formDialog(w, h int) *dialog {
	iw := dialogWidth(formWidth, w)
	return box("New task", m.formLines(iw, max(h-2, 1)), iw, primary)
}

func (m *Model) formLines(w, h int) []string {
	f := m.form
	var out []string
	add := func(s string) { out = append(out, trunc(s, w)) }
	const indent = "               "
	focusRow := 0
	for i := 0; i < numFields; i++ {
		cursor := "  "
		label := labelStyle.Render(fmt.Sprintf("%-12s", fieldLabels[i]))
		if i == f.focus {
			cursor = keyStyle.Render("▌ ")
			label = keyStyle.Render(fmt.Sprintf("%-12s", fieldLabels[i]))
			focusRow = len(out)
		}
		var val string
		switch i {
		case fieldBacklog:
			val = dimStyle.Render("[ ]")
			if f.backlog {
				val = keyStyle.Render("[x]")
			}
			val += dimStyle.Render(" space / ctrl+b toggles")
		case fieldBase:
			on, off := keyStyle.Render("●"), dimStyle.Render("○")
			auto, prod := on+" auto (dev → develop → default)", off+dimStyle.Render(" production patch (default branch)")
			if f.prodBase {
				auto, prod = off+dimStyle.Render(" auto (dev → develop → default)"), on+" production patch (default branch)"
			}
			val = auto + "\n" + prod
		case fieldRepos:
			switch {
			case len(f.repos) > 0:
				val = styledSummary(f.repos)
			case f.project() != "" && f.project() != f.preProject:
				val = dimStyle.Render("project " + clean(f.project()) + "'s repos")
			default:
				val = dimStyle.Render("none")
			}
			if i == f.focus {
				val += dimStyle.Render("  ⏎ pick")
			}
		default:
			val = f.inputs[i].View()
		}
		for j, v := range strings.Split(val, "\n") {
			if j == 0 {
				add(cursor + " " + label + v)
			} else {
				add(indent + v)
			}
		}
		if i == fieldProject && i == f.focus {
			hint := "no projects configured"
			if len(f.projects) > 0 {
				hint = "projects: " + clean(strings.Join(f.projects, ", "))
			}
			add(dimStyle.Render(indent + hint))
		}
		if i == fieldProject && f.projErr != "" && f.projErrFor == f.project() {
			add(errStyle.Render(indent + clean(f.projErr)))
		}
		if i == fieldRepos && i == f.focus {
			add(dimStyle.Render(indent + "enter or space opens the repo picker"))
		}
		if i == fieldBase {
			for _, l := range strings.Split(ansi.Wrap(baseHint, max(w-len(indent), 10), ""), "\n") {
				add(dimStyle.Render(indent + strings.TrimSpace(l)))
			}
		}
		if i != numFields-1 {
			add("")
		}
	}
	if f.err != "" {
		add("")
		add(errStyle.Render("✗ " + f.err))
	}
	if len(out) > h {
		// keep the focused field visible on very short terminals
		start := focusRow + 2 - h + 1
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

// listWindow is the range of rows the list shows in h lines.
func (m *Model) listWindow(h int) (off, end int) {
	off = m.offset
	if m.selIdx >= off+h {
		// the help took a line since the offset was set (see helpLines)
		off = m.selIdx - h + 1
	}
	end = min(off+h, len(m.rows))
	return off, max(end, off)
}

// scrollHint says how many rows are above and below the visible ones.
func (m *Model) scrollHint(h int) string {
	if h <= 0 || len(m.rows) == 0 {
		return ""
	}
	off, end := m.listWindow(h)
	var parts []string
	if off > 0 {
		parts = append(parts, "↑ "+strconv.Itoa(off))
	}
	if n := len(m.rows) - end; n > 0 {
		parts = append(parts, "↓ "+strconv.Itoa(n))
	}
	if len(parts) == 0 {
		return ""
	}
	return dimStyle.Render(strings.Join(parts, "  ") + " more")
}

// welcomeLines fill the details pane while there are no tasks at all.
func welcomeLines(w int) []string {
	item := func(k, d string) string {
		return trunc(" "+keyStyle.Render(fmt.Sprintf("%-6s", k))+" "+d, w)
	}
	return []string{
		trunc(titleStyle.Render("Welcome to atelier"), w),
		trunc(dimStyle.Render("A task studio for parallel Claude Code work."), w),
		"",
		item("n", "create a task: its own worktrees, on branch atl/<slug>"),
		item("enter", "open it: Claude Code in tmux, one window per session"),
		item("A", "ask the atelier assistant"),
		item("?", "every key"),
	}
}
