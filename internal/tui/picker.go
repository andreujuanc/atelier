package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/andreujuanc/atelier/internal/app"
	"github.com/andreujuanc/atelier/internal/model"
)

// picker is a modal, filterable multi-select list of repos with a mode
// (rw / ro) per selected repo. Typing filters (case-insensitive
// substring), up/down (ctrl+p/ctrl+n) move, space cycles the highlighted
// repo none → rw → ro → none, enter confirms and esc clears the filter (or
// cancels when it is empty). Locked repos (already in the task) are shown
// dimmed and cannot be selected; the cursor starts on the first unlocked
// repo.
type picker struct {
	title     string
	available []string              // repos found in $ATL_REPOS
	locked    map[string]model.Mode // name → its mode in the task
	sel       []app.RepoArg         // selection, in selection order
	filter    string
	cursor    int // index into visible()
	offset    int // first visible row shown
	loading   bool
	err       string
	// msg is a hint (e.g. "already in the task") shown until the selection
	// or the filter changes.
	msg string
	// moved: the user moved the cursor or filtered (the cursor no longer
	// starts on the first unlocked repo).
	moved bool
}

func newPicker(title string, sel []app.RepoArg, locked map[string]model.Mode) *picker {
	p := &picker{title: title, locked: locked, loading: true}
	p.sel = append(p.sel, sel...)
	return p
}

// setAvailable sets the repos found on disk (loading done).
func (p *picker) setAvailable(names []string, err error) {
	p.loading = false
	if err != nil {
		p.err = err.Error()
		return
	}
	p.err = ""
	p.available = names
	p.initCursor()
	p.clamp()
}

// initCursor puts the cursor on the first unlocked row unless the user has
// already moved it.
func (p *picker) initCursor() {
	if p.moved {
		return
	}
	p.cursor = 0
	for i, n := range p.visible() {
		if p.locked[n] == "" {
			p.cursor = i
			return
		}
	}
}

// items is every row: available repos plus selected repos that are not on
// disk (so they can still be seen and deselected), sorted by name.
func (p *picker) items() []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range p.available {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	extra := false
	for _, r := range p.sel {
		if !seen[r.Name] {
			seen[r.Name] = true
			out = append(out, r.Name)
			extra = true
		}
	}
	if extra {
		sort.Strings(out)
	}
	return out
}

// visible is items() matching the filter.
func (p *picker) visible() []string {
	f := strings.ToLower(strings.TrimSpace(p.filter))
	if f == "" {
		return p.items()
	}
	var out []string
	for _, n := range p.items() {
		if strings.Contains(strings.ToLower(n), f) {
			out = append(out, n)
		}
	}
	return out
}

func (p *picker) modeOf(name string) model.Mode {
	for _, r := range p.sel {
		if r.Name == name {
			return r.Mode
		}
	}
	return ""
}

// cycle moves name's mode none → rw → ro → none.
func (p *picker) cycle(name string) {
	for i, r := range p.sel {
		if r.Name != name {
			continue
		}
		if r.Mode == model.ModeRW {
			p.sel[i].Mode = model.ModeRO
		} else {
			p.sel = append(p.sel[:i:i], p.sel[i+1:]...)
		}
		return
	}
	p.sel = append(p.sel, app.RepoArg{Name: name, Mode: model.ModeRW})
}

// selection returns a copy of the selection.
func (p *picker) selection() []app.RepoArg {
	return append([]app.RepoArg(nil), p.sel...)
}

func (p *picker) current() string {
	v := p.visible()
	if p.cursor >= 0 && p.cursor < len(v) {
		return v[p.cursor]
	}
	return ""
}

func (p *picker) clamp() {
	n := len(p.visible())
	if p.cursor >= n {
		p.cursor = n - 1
	}
	if p.cursor < 0 {
		p.cursor = 0
	}
}

func (p *picker) setFilter(f string) {
	cur := p.current()
	p.msg, p.moved = "", true
	p.filter = f
	p.cursor = 0
	for i, n := range p.visible() {
		if n == cur {
			p.cursor = i
		}
	}
}

// pickResult is what a key did to the picker.
type pickResult int

const (
	pickContinue pickResult = iota
	pickCancel
	pickConfirm
)

// update handles a key.
func (p *picker) update(msg tea.KeyMsg) pickResult {
	k := msg.String()
	switch k {
	case "up", "ctrl+p", "down", "ctrl+n", "pgup", "pgdown", "home", "end":
		p.moved = true
	}
	switch k {
	case "esc":
		if p.filter != "" {
			p.setFilter("")
			break
		}
		return pickCancel
	case "enter":
		return pickConfirm
	case "up", "ctrl+p":
		p.cursor--
	case "down", "ctrl+n":
		p.cursor++
	case "pgup":
		p.cursor -= 10
	case "pgdown":
		p.cursor += 10
	case "home":
		p.cursor = 0
	case "end":
		p.cursor = len(p.visible()) - 1
	case " ":
		name := p.current()
		switch {
		case name == "":
		case p.locked[name] != "":
			p.msg = name + " is already in the task"
		default:
			p.msg = ""
			p.cycle(name)
		}
	case "backspace", "ctrl+h":
		if r := []rune(p.filter); len(r) > 0 {
			p.setFilter(string(r[:len(r)-1]))
		}
	case "ctrl+u":
		p.setFilter("")
	default:
		if msg.Type == tea.KeyRunes && !msg.Alt {
			var b strings.Builder
			for _, r := range msg.Runes {
				if r > ' ' && r != 0x7f {
					b.WriteRune(r)
				}
			}
			if b.Len() > 0 {
				p.setFilter(p.filter + b.String())
			}
		}
	}
	p.clamp()
	return pickContinue
}

// summary renders "a(rw) b(ro)" or "" for an empty selection.
func summary(sel []app.RepoArg) string {
	parts := make([]string, 0, len(sel))
	for _, r := range sel {
		parts = append(parts, fmt.Sprintf("%s(%s)", clean(r.Name), r.Mode))
	}
	return strings.Join(parts, " ")
}

// lines renders the picker in w×h cells.
func (p *picker) lines(w, h int) []string {
	if w <= 0 || h <= 0 {
		return nil
	}
	vis := p.visible()
	var head, foot []string
	if h >= 6 {
		head = append(head, titleStyle.Render(trunc(clean(p.title), w)))
	}
	if h >= 3 {
		count := fmt.Sprintf("  %d/%d", len(vis), len(p.items()))
		head = append(head, trunc(labelStyle.Render("filter: ")+clean(p.filter)+markStyle.Render("▏")+dimStyle.Render(count), w))
	}
	if h >= 2 {
		s := summary(p.sel)
		if s == "" {
			s = dimStyle.Render("none")
		}
		line := labelStyle.Render("selected: ") + s
		if p.msg != "" {
			line = errStyle.Render(clean(p.msg))
		}
		foot = append(foot, trunc(line, w))
	}
	listH := h - len(head) - len(foot)
	out := append([]string(nil), head...)

	var list []string
	switch {
	case p.err != "":
		list = []string{errStyle.Render(trunc("error: "+clean(firstLine(p.err)), w))}
	case p.loading && len(vis) == 0:
		list = []string{dimStyle.Render(trunc("loading repos…", w))}
	case len(vis) == 0 && p.filter != "":
		list = []string{dimStyle.Render(trunc("no repos match "+clean(p.filter), w))}
	case len(vis) == 0:
		list = []string{dimStyle.Render(trunc("no git repos found in $ATL_REPOS", w))}
	default:
		if p.cursor < p.offset {
			p.offset = p.cursor
		}
		if p.cursor >= p.offset+listH {
			p.offset = p.cursor - listH + 1
		}
		if max := len(vis) - listH; p.offset > max {
			p.offset = max
		}
		if p.offset < 0 {
			p.offset = 0
		}
		for i := p.offset; i < len(vis) && i < p.offset+listH; i++ {
			list = append(list, p.row(vis[i], i == p.cursor, w))
		}
	}
	if len(list) > listH {
		list = list[:listH]
	}
	for len(list) < listH {
		list = append(list, "")
	}
	out = append(out, list...)
	return append(out, foot...)
}

func (p *picker) row(name string, cur bool, w int) string {
	if m := p.locked[name]; m != "" {
		text := fmt.Sprintf("  [%s] %s  (in task)", m, clean(name))
		if cur {
			return selStyle.Render(fit(dimStyle.Render("> "+text[2:]), w))
		}
		return dimStyle.Render(trunc(text, w))
	}
	marker := "[  ]"
	switch p.modeOf(name) {
	case model.ModeRW:
		marker = "[rw]"
	case model.ModeRO:
		marker = "[ro]"
	}
	text := "  " + marker + " " + clean(name)
	if cur {
		return selStyle.Render(fit("> "+text[2:], w))
	}
	if marker != "[  ]" {
		return trunc("  "+markStyle.Render(marker)+" "+clean(name), w)
	}
	return trunc(text, w)
}
