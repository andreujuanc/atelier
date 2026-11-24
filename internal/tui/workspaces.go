package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/andreujuanc/atelier/internal/workspace"
)

// WorkspaceSelector is what the workspace selector (`atl` with no
// workspace selected, `atl workspace open` without a name) works with.
type WorkspaceSelector struct {
	Root      string // the workspaces root, shown in the title
	Home      string // the home directory, shown as "~" ("": none)
	Preselect string // the workspace selected at start (else the first)
	// Load lists the directories under the root.
	Load func(ctx context.Context) ([]workspace.Entry, error)
	// Prepare computes, without writing anything, what `atl workspace new
	// <name>` writes to create workspace name or adopt the directory
	// without atelier.toml; an error when it would refuse.
	Prepare func(name string) (*workspace.Plan, error)
	// Create writes what Prepare planned.
	Create func(p *workspace.Plan) error
}

// RunWorkspaces shows the workspace selector on the terminal and returns
// the name of the workspace chosen, "" when the user quit.
func RunWorkspaces(ctx context.Context, s WorkspaceSelector) (string, error) {
	m := NewWorkspacePicker(ctx, s)
	_, err := tea.NewProgram(m, tea.WithContext(ctx)).Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return m.Chosen(), nil
}

type wsMode int

const (
	wsNormal wsMode = iota
	wsFilter
	wsNew   // the new workspace's name is being typed (n)
	wsAdopt // a directory without atelier.toml: what adopting it writes, y/n
)

type (
	wsLoadedMsg struct {
		rows []workspace.Entry
		err  error
	}
	wsCreatedMsg struct {
		plan *workspace.Plan
		err  error
	}
)

// WorkspacePicker is the workspace selector's model.
type WorkspacePicker struct {
	ctx context.Context
	s   WorkspaceSelector

	width, height int

	rows    []workspace.Entry
	loaded  bool
	loading bool
	loadErr string
	vis     []int // indexes into rows of the rows shown (the filter)
	sel     int   // index into vis
	// selName is the selected row's name, kept across reloads and filtering.
	selName string
	offset  int // first visible row

	mode        wsMode
	filter      string
	filterInput textinput.Model
	nameInput   textinput.Model
	adopt       *workspace.Plan // what the adopt confirmation shows
	creating    bool

	status    string
	statusErr bool

	chosen string
}

// NewWorkspacePicker returns the selector's model.
func NewWorkspacePicker(ctx context.Context, s WorkspaceSelector) *WorkspacePicker {
	fi := textinput.New()
	fi.Prompt = "/"
	fi.Placeholder = "filter"
	styleInput(&fi)
	ni := textinput.New()
	ni.Prompt = "new workspace> "
	ni.Placeholder = "name (lowercase letters, digits, - and _)"
	styleInput(&ni)
	ni.CharLimit = 32
	return &WorkspacePicker{ctx: ctx, s: s, selName: s.Preselect, filterInput: fi, nameInput: ni, width: 80, height: 24}
}

// Chosen is the workspace the user opened ("" until then, or after quit).
func (m *WorkspacePicker) Chosen() string { return m.chosen }

// Init loads the list.
func (m *WorkspacePicker) Init() tea.Cmd { return m.load() }

func (m *WorkspacePicker) load() tea.Cmd {
	m.loading = true
	load, ctx := m.s.Load, m.ctx
	return func() tea.Msg {
		rows, err := load(ctx)
		return wsLoadedMsg{rows: rows, err: err}
	}
}

func (m *WorkspacePicker) create(p *workspace.Plan) tea.Cmd {
	m.creating = true
	create := m.s.Create
	return func() tea.Msg {
		return wsCreatedMsg{plan: p, err: create(p)}
	}
}

// prepare plans workspace name (see WorkspaceSelector.Prepare): a new one
// is created at once, adopting a directory is asked first, showing what
// it writes. A refusal (a box's workspace whose repos are not known) is
// shown on the status line.
func (m *WorkspacePicker) prepare(name string) tea.Cmd {
	p, err := m.s.Prepare(name)
	if err != nil {
		m.setStatus(firstLine(err.Error()), true)
		return nil
	}
	if !p.Adopt {
		return m.create(p)
	}
	m.nameInput.Blur()
	m.mode, m.adopt = wsAdopt, p
	m.setStatus("", false)
	return nil
}

// Update implements tea.Model.
func (m *WorkspacePicker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.clampOffset()
		return m, nil
	case tea.PasteMsg:
		return m, m.paste(msg)
	case tea.KeyPressMsg:
		return m, m.handleKey(msg)
	case wsLoadedMsg:
		m.loading, m.loaded = false, true
		if msg.err != nil {
			m.loadErr = clean(firstLine(msg.err.Error()))
			return m, nil
		}
		m.loadErr = ""
		m.rows = msg.rows
		m.rebuild()
		return m, nil
	case wsCreatedMsg:
		m.creating = false
		if msg.err != nil {
			if m.mode == wsAdopt {
				m.mode, m.adopt = wsNormal, nil
			}
			m.setStatus(firstLine(msg.err.Error()), true)
			return m, nil
		}
		verb := "created"
		if msg.plan.Adopt {
			verb = "adopted"
		}
		st := "workspace " + msg.plan.Name + " " + verb + " — enter opens it"
		for _, n := range msg.plan.Notes {
			st += "; note: " + n
		}
		m.setStatus(st, false)
		m.mode, m.adopt = wsNormal, nil
		m.nameInput.Blur()
		m.nameInput.SetValue("")
		m.filter = ""
		m.filterInput.SetValue("")
		m.selName = msg.plan.Name
		return m, m.load()
	}
	return m, nil
}

func (m *WorkspacePicker) setStatus(s string, isErr bool) { m.status, m.statusErr = s, isErr }

func (m *WorkspacePicker) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()
	if key == "ctrl+c" {
		return tea.Quit
	}
	if m.creating {
		m.setStatus("busy: creating the workspace…", false)
		return nil
	}
	switch m.mode {
	case wsFilter:
		return m.filterKey(msg)
	case wsNew:
		return m.newKey(msg)
	case wsAdopt:
		switch key {
		case "y", "Y":
			return m.create(m.adopt)
		case "n", "N", "esc", "q":
			m.mode, m.adopt = wsNormal, nil
			m.setStatus("", false)
		}
		return nil
	}
	// Fast-typed letters may arrive as one event ("jj"): one at a time.
	if rs := []rune(msg.Text); len(rs) > 1 && msg.Mod&(tea.ModAlt|tea.ModCtrl) == 0 {
		var cmds []tea.Cmd
		for _, r := range rs {
			cmds = append(cmds, m.handleKey(runeKey(r)))
		}
		return tea.Batch(cmds...)
	}
	switch key {
	case "q":
		return tea.Quit
	case "j", "down":
		m.move(1)
	case "k", "up":
		m.move(-1)
	case "pgdown":
		m.move(m.listHeight())
	case "pgup":
		m.move(-m.listHeight())
	case "home", "g":
		m.move(-len(m.vis))
	case "end", "G":
		m.move(len(m.vis))
	case "/":
		m.mode = wsFilter
		m.filterInput.SetValue(m.filter)
		m.filterInput.CursorEnd()
		m.filterInput.Focus()
	case "esc":
		if m.filter != "" {
			m.filterInput.SetValue("")
			m.applyFilter("")
		}
	case "n":
		m.mode = wsNew
		m.nameInput.SetValue("")
		m.nameInput.Focus()
		m.setStatus("", false)
	case "r":
		if !m.loading {
			return m.load()
		}
	case "enter":
		r := m.selected()
		if r == nil {
			m.setStatus("no workspace selected", false)
			return nil
		}
		if !r.Workspace {
			return m.prepare(r.Name)
		}
		m.chosen = r.Name
		return tea.Quit
	}
	return nil
}

func (m *WorkspacePicker) filterKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.mode = wsNormal
		m.filterInput.Blur()
		m.filterInput.SetValue("")
		m.applyFilter("")
		return nil
	case "enter":
		m.mode = wsNormal
		m.filterInput.Blur()
		m.applyFilter(m.filterInput.Value())
		return nil
	case "up":
		m.move(-1)
		return nil
	case "down":
		m.move(1)
		return nil
	}
	var cmd tea.Cmd
	m.filterInput, cmd = m.filterInput.Update(msg)
	m.applyFilter(m.filterInput.Value())
	return cmd
}

func (m *WorkspacePicker) newKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.mode = wsNormal
		m.nameInput.Blur()
		m.setStatus("", false)
		return nil
	case "enter":
		name := strings.TrimSpace(m.nameInput.Value())
		if err := m.checkNew(name); err != nil {
			m.setStatus(err.Error(), true)
			return nil
		}
		return m.prepare(name)
	}
	m.setStatus("", false)
	var cmd tea.Cmd
	m.nameInput, cmd = m.nameInput.Update(msg)
	return cmd
}

// checkNew returns why name cannot be a new workspace, nil if it can (a
// directory without atelier.toml is adopted).
func (m *WorkspacePicker) checkNew(name string) error {
	if name == "" {
		return errors.New("type the new workspace's name")
	}
	if err := workspace.CheckName(name); err != nil {
		return err
	}
	if r := m.row(name); r != nil && r.Workspace {
		return fmt.Errorf("workspace %s already exists", name)
	}
	return nil
}

func (m *WorkspacePicker) row(name string) *workspace.Entry {
	for i := range m.rows {
		if m.rows[i].Name == name {
			return &m.rows[i]
		}
	}
	return nil
}

func (m *WorkspacePicker) applyFilter(f string) {
	m.filter = f
	m.rebuild()
}

// rebuild recomputes the visible rows and keeps the selection on selName
// when it is visible, else the first row.
func (m *WorkspacePicker) rebuild() {
	f := strings.ToLower(strings.TrimSpace(m.filter))
	m.vis = m.vis[:0]
	for i, r := range m.rows {
		if f == "" || strings.Contains(r.Name, f) {
			m.vis = append(m.vis, i)
		}
	}
	m.sel = 0
	for i, ri := range m.vis {
		if m.rows[ri].Name == m.selName {
			m.sel = i
		}
	}
	if r := m.selected(); r != nil {
		m.selName = r.Name
	}
	m.clampOffset()
}

func (m *WorkspacePicker) selected() *workspace.Entry {
	if m.sel < 0 || m.sel >= len(m.vis) {
		return nil
	}
	return &m.rows[m.vis[m.sel]]
}

func (m *WorkspacePicker) move(delta int) {
	if len(m.vis) == 0 {
		return
	}
	m.sel += delta
	if m.sel < 0 {
		m.sel = 0
	}
	if m.sel >= len(m.vis) {
		m.sel = len(m.vis) - 1
	}
	m.selName = m.selected().Name
	m.clampOffset()
}

func (m *WorkspacePicker) clampOffset() {
	h := m.listHeight()
	if m.sel < m.offset {
		m.offset = m.sel
	}
	if h > 0 && m.sel >= m.offset+h {
		m.offset = m.sel - h + 1
	}
	if m.offset > len(m.vis)-h {
		m.offset = len(m.vis) - h
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

// --- view ---

// abbrev shows p with the home directory as "~".
func (m *WorkspacePicker) abbrev(p string) string {
	h := m.s.Home
	if h == "" || h == "/" {
		return p
	}
	if p == h {
		return "~"
	}
	if rest, ok := strings.CutPrefix(p, h+"/"); ok {
		return "~/" + rest
	}
	return p
}

// boxed reports whether the list is drawn in a box (room enough).
func (m *WorkspacePicker) boxed() bool { return m.width >= 24 && m.height >= 8 }

// listHeight is the number of rows the list shows: the screen less the
// top bar, the box (or a blank line), the status and the help lines.
func (m *WorkspacePicker) listHeight() int {
	used := 4
	if m.boxed() {
		used = 5
	}
	if h := m.height - used; h > 0 {
		return h
	}
	return 0
}

// render draws the screen.
func (m *WorkspacePicker) render() string {
	w, h := m.width, m.height
	if w <= 0 || h <= 0 {
		return ""
	}
	head := brandStyle.Render(" atelier ") + " " + titleStyle.Render("workspaces") + dimStyle.Render("  "+clean(m.abbrev(m.s.Root)))
	lines := []string{trunc(head, w)}
	var body []string
	title := "Workspaces"
	if m.mode == wsAdopt && m.adopt != nil {
		title = "Adopt"
		body = m.adoptLines(w-4, m.listHeight())
	} else {
		body = m.listLines(w-4, m.listHeight())
	}
	if m.boxed() {
		right := ""
		if m.loaded && m.loadErr == "" {
			right = labelStyle.Render(fmt.Sprintf("%d", len(m.vis)))
			if m.filter != "" {
				right = dimStyle.Render("/"+clean(m.filter)) + " " + labelStyle.Render(fmt.Sprintf("%d/%d", len(m.vis), len(m.rows)))
			}
		}
		lines = append(lines, panel(title, right, body, w, m.listHeight()+2, true)...)
	} else {
		lines = append(lines, "")
		for _, l := range body {
			lines = append(lines, trunc(l, w))
		}
	}
	for len(lines) < h-2 {
		lines = append(lines, "")
	}
	lines = append(lines, m.statusLine(w), m.helpLine(w))
	if len(lines) > h {
		lines = lines[len(lines)-h:]
	}
	return strings.Join(lines, "\n")
}

func (m *WorkspacePicker) listLines(w, h int) []string {
	if h <= 0 {
		return nil
	}
	var msg string
	switch {
	case m.loadErr != "":
		return []string{errStyle.Render(trunc("✗ cannot list the workspaces in "+clean(m.s.Root)+": "+m.loadErr, w))}
	case !m.loaded:
		msg = "loading…"
	case len(m.rows) == 0:
		return []string{dimStyle.Render(trunc("no workspaces in "+clean(m.abbrev(m.s.Root))+" yet — press n to create one (as `atl workspace new <name>` does)", w)), "",
			trunc(keyStyle.Render("n")+descStyle.Render(" new workspace"), w)}
	case len(m.vis) == 0:
		msg = "no workspaces match /" + clean(m.filter)
	}
	if msg != "" {
		return []string{dimStyle.Render(trunc(msg, w))}
	}
	nameW := 0
	for _, r := range m.rows {
		nameW = max(nameW, ansi.StringWidth(clean(r.Name)))
	}
	end := min(m.offset+h, len(m.vis))
	var out []string
	for i := m.offset; i < end; i++ {
		out = append(out, m.rowLine(m.rows[m.vis[i]], i == m.sel, w, nameW))
	}
	return out
}

// rowLine renders a row: the name and the path, dimmed; a directory
// without atelier.toml dimmed altogether and marked not adopted; the
// selected one highlighted across the width.
func (m *WorkspacePicker) rowLine(r workspace.Entry, sel bool, w, nameW int) string {
	name := lipgloss.NewStyle().Bold(true)
	if !r.Workspace {
		name = dimStyle
	}
	segs := []seg{{padRight(clean(r.Name), nameW), name}, {"  " + clean(m.abbrev(r.Dir)), dimStyle}}
	if !r.Workspace {
		segs = append(segs, seg{"  not adopted", warnStyle.Bold(false)})
	}
	return rowText(sel, w, segs...)
}

// adoptLines is the adopt confirmation: what adopting the directory
// writes (each setting and where it came from) and what it skipped, at
// most h lines.
func (m *WorkspacePicker) adoptLines(w, h int) []string {
	if h <= 0 {
		return nil
	}
	p := m.adopt
	out := []string{
		trunc(markStyle.Render("Adopt "+clean(m.abbrev(p.Dir))+" as workspace "+clean(p.Name)+"?"), w),
		dimStyle.Render(trunc("It writes "+workspace.File+" (keeping what is there) with:", w)),
		"",
	}
	item := func(s string, st lipgloss.Style) {
		wrapped := strings.Split(ansi.Wrap(clean(s), max(w-4, 1), ""), "\n")
		for i, l := range wrapped {
			ind := "    "
			if i == 0 {
				ind = "  • "
			}
			out = append(out, st.Render(trunc(ind+l, w)))
		}
	}
	if len(p.Settings) == 0 {
		item("no settings: every one at its default", dimStyle)
	}
	for _, s := range p.Settings {
		item(s.String(), lipgloss.NewStyle())
	}
	for _, s := range p.Skipped {
		item("skipped "+s, dimStyle)
	}
	for _, s := range p.Warnings {
		item("warning: "+s, errStyle)
	}
	for _, s := range p.Notes {
		item("note: "+s, markStyle)
	}
	if len(out) > h {
		out = out[:h]
	}
	return out
}

func (m *WorkspacePicker) statusLine(w int) string {
	switch m.mode {
	case wsFilter:
		return trunc(m.filterInput.View(), w)
	case wsNew:
		return trunc(m.nameInput.View(), w)
	case wsAdopt:
		if !m.creating {
			return trunc(markStyle.Render("? Adopt it? y/n"), w)
		}
	}
	switch {
	case m.creating:
		return trunc(keyStyle.Render("working… ")+"creating the workspace", w)
	case m.status != "" && m.statusErr:
		return trunc(errStyle.Render("✗ "+clean(m.status)), w)
	case m.status != "":
		return trunc(okStyle.Render("✓ ")+clean(m.status), w)
	case m.loading && m.loaded:
		return trunc(dimStyle.Render("refreshing…"), w)
	case m.filter != "":
		return trunc(dimStyle.Render("filter: /"+clean(m.filter)+"  (esc clears)"), w)
	}
	return ""
}

func (m *WorkspacePicker) helpLine(w int) string {
	var items []helpItem
	switch m.mode {
	case wsFilter:
		items = []helpItem{{"", "type to filter"}, {"enter", "apply"}, {"esc", "clear"}, {"↑/↓", "move"}}
	case wsNew:
		if m.statusErr && m.status != "" {
			return errStyle.Render(trunc("✗ "+clean(m.status), w))
		}
		name := strings.TrimSpace(m.nameInput.Value())
		items = []helpItem{{"enter", "create (as `atl workspace new <name>` does)"}, {"esc", "cancel"}}
		if name != "" {
			if err := m.checkNew(name); err != nil {
				return errStyle.Render(trunc("✗ "+clean(err.Error()), w))
			}
			if r := m.row(name); r != nil && !r.Workspace {
				items = []helpItem{{"", clean(m.abbrev(r.Dir)) + " exists without " + workspace.File + ":"},
					{"enter", "shows what adopting it writes"}, {"esc", "cancel"}}
			}
		}
	case wsAdopt:
		items = []helpItem{{"y", "adopt (as `atl workspace new " + clean(m.adopt.Name) + "` does)"}, {"n/esc", "cancel"}}
	default:
		items = []helpItem{{"↑/↓", "move"}, {"enter", "open"}, {"n", "new workspace"}, {"/", "filter"}, {"r", "refresh"}, {"q", "quit"}}
	}
	return helpStyled(items, w)
}
