package tui

import (
	"fmt"
	"strconv"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/andreujuanc/atelier/internal/app"
	"github.com/andreujuanc/atelier/internal/model"
)

// sessionPicker is the modal list Enter shows for a task that has sessions
// to choose from: "New session" first, then the task's live sessions and
// its resumable past ones (in the order app.OpenChoices returns them),
// opening on the most recently active session. It
// looks and moves like the repo picker: up/down (ctrl+p/ctrl+n, j/k),
// pgup/pgdown, home/end move, enter picks, esc cancels.
type sessionPicker struct {
	taskID  int64
	slug    string
	choices []app.SessionChoice
	cursor  int // 0: "New session"; i > 0: choices[i-1]
	offset  int // first visible row shown
}

// newSessionPicker starts with the most recently active session highlighted
// (live or past; the first in list order on a tie), so enter continues
// where you left off; "New session" is one row up.
func newSessionPicker(id int64, slug string, choices []app.SessionChoice) *sessionPicker {
	p := &sessionPicker{taskID: id, slug: slug, choices: choices}
	for i, c := range choices {
		if p.cursor == 0 || c.Session.LastEventAt.After(choices[p.cursor-1].Session.LastEventAt) {
			p.cursor = i + 1
		}
	}
	return p
}

func (p *sessionPicker) rows() int { return len(p.choices) + 1 }

// picked returns the highlighted session, or nil for "New session".
func (p *sessionPicker) picked() *app.SessionChoice {
	if p.cursor <= 0 || p.cursor > len(p.choices) {
		return nil
	}
	return &p.choices[p.cursor-1]
}

// update handles a key.
func (p *sessionPicker) update(msg tea.KeyPressMsg) pickResult {
	switch msg.String() {
	case "esc":
		return pickCancel
	case "enter":
		return pickConfirm
	case "up", "ctrl+p", "k":
		p.cursor--
	case "down", "ctrl+n", "j":
		p.cursor++
	case "pgup":
		p.cursor -= 10
	case "pgdown":
		p.cursor += 10
	case "home":
		p.cursor = 0
	case "end":
		p.cursor = p.rows() - 1
	}
	if p.cursor >= p.rows() {
		p.cursor = p.rows() - 1
	}
	if p.cursor < 0 {
		p.cursor = 0
	}
	return pickContinue
}

// dialog is the picker as a box over the screen (w×h above the status
// line).
func (p *sessionPicker) dialog(w, h int, now time.Time) *dialog {
	iw := dialogWidth(96, w)
	ih := min(p.rows()+2, max(h-2, 1))
	return box("Open "+clean(p.slug), p.lines(iw, ih, now), iw, primary)
}

// lines renders the picker in w×h cells; now dates the relative times.
func (p *sessionPicker) lines(w, h int, now time.Time) []string {
	if w <= 0 || h <= 0 {
		return nil
	}
	var head []string
	if h >= 3 {
		live := 0
		for _, c := range p.choices {
			if c.Live {
				live++
			}
		}
		head = append(head, trunc(okStyle.Render(strconv.Itoa(live))+dimStyle.Render(" live (attach)  ")+
			labelStyle.Render(strconv.Itoa(len(p.choices)-live))+dimStyle.Render(" past (resume with claude --resume)"), w), "")
	}
	listH := h - len(head)
	out := append([]string(nil), head...)
	p.offset = scrollOffset(p.cursor, p.offset, p.rows(), listH)
	for i := p.offset; i < p.rows() && i < p.offset+listH; i++ {
		if i == 0 {
			out = append(out, p.newRow(i == p.cursor, w))
		} else {
			out = append(out, choiceRow(p.choices[i-1], i == p.cursor, w, now))
		}
	}
	for len(out) < h {
		out = append(out, "")
	}
	return out
}

func (p *sessionPicker) newRow(cur bool, w int) string {
	return rowText(cur, w, seg{"+ New session", keyStyle}, seg{"  starts Claude in a new window", dimStyle})
}

// stateStyle colours a session state like the list's dots.
func stateStyle(st model.SessionState) lipgloss.Style {
	switch st {
	case model.StateWaiting:
		return waitStyle
	case model.StateRunning:
		return runStyle
	case model.StateIdle:
		return idleStyle
	case model.StateStopped:
		return stopStyle
	}
	return dimStyle
}

// choiceRow renders one session: state and relative last activity in fixed
// columns, then the title (the main text), the window name and, if there
// is room left, the last prompt; the title wins the space over the rest.
func choiceRow(c app.SessionChoice, cur bool, w int, now time.Time) string {
	s := c.Session
	age := "-"
	if !s.LastEventAt.IsZero() {
		age = humanAge(now.Sub(s.LastEventAt)) + " ago"
	}
	glyph := sessionGlyph(s.State)
	state := fmt.Sprintf("%-7s", clean(string(s.State)))
	ageCol := fmt.Sprintf("%-7s", age)
	name := sessionName(s)
	title := clean(c.Title)
	if title == "" {
		title = name
	}
	nameSuffix := ""
	if name != title {
		nameSuffix = "  " + name
	}
	rest := w - 2 - 2 - len(state) - 2 - len(ageCol) - 2
	if rest < 0 {
		rest = 0
	}
	if rest-ansi.StringWidth(nameSuffix) < 12 {
		nameSuffix = ""
	}
	titleT := trunc(title, rest-ansi.StringWidth(nameSuffix))
	snippet := ""
	if lp := clean(c.LastPrompt); lp != "" && lp != title {
		if room := rest - ansi.StringWidth(titleT) - ansi.StringWidth(nameSuffix) - len("  last: "); room >= 12 {
			snippet = "  last: " + trunc(lp, room)
		}
	}
	st := stateStyle(s.State)
	ts := lipgloss.NewStyle()
	if cur {
		ts = ts.Bold(true)
	}
	return rowText(cur, w, seg{glyph + " " + state, st}, seg{"  " + ageCol + "  ", dimStyle}, seg{titleT, ts},
		seg{nameSuffix, labelStyle}, seg{snippet, dimStyle})
}
