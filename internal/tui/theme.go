package tui

import (
	"image/color"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"charm.land/lipgloss/v2"

	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/palette"
)

// The TUI's colours beyond the session-state ones (palette), as light/dark
// pairs. The text itself keeps the terminal's own foreground.
var (
	cPrimary = palette.Color{Light: "#2E7DE9", Dark: "#7AA2F7"} // focus, keys, the brand
	cPurple  = palette.Color{Light: "#9854F1", Dark: "#BB9AF7"}
	cWarn    = palette.Color{Light: "#8C6C3E", Dark: "#E0AF68"} // uncommitted changes
	cMuted   = palette.Color{Light: "#6172B0", Dark: "#737AA2"} // labels, hints
	cFaint   = palette.Color{Light: "#A1A6C5", Dark: "#4E5575"} // rules, backdrop
	cBorder  = palette.Color{Light: "#C4C8DA", Dark: "#3B4261"}
	cSurface = palette.Color{Light: "#E1E2E9", Dark: "#292E42"} // the selected row
	cOnBrand = palette.Color{Light: "#FFFFFF", Dark: "#1A1B26"} // text on a filled chip
	cOnSel   = palette.Color{Light: "#1A1B26", Dark: "#C0CAF5"} // text on the selected row
)

// isDark is whether the terminal background is dark (which variant of
// each colour is used). Dark until the terminal says otherwise.
var isDark = true

// Styles, set by applyTheme.
var (
	accent, subtle, errColor, okColor color.Color
	primary, purple, warn, muted      color.Color
	faint, border, surface, onBrand   color.Color
	onSel                             color.Color

	headerStyle lipgloss.Style // section and group headings
	selStyle    lipgloss.Style // the selected row's background
	markStyle   lipgloss.Style // attention: the dirty star, questions
	dimStyle    lipgloss.Style
	labelStyle  lipgloss.Style
	errStyle    lipgloss.Style
	okStyle     lipgloss.Style
	warnStyle   lipgloss.Style
	titleStyle  lipgloss.Style
	sepStyle    lipgloss.Style
	keyStyle    lipgloss.Style // a key in the help
	descStyle   lipgloss.Style // what the key does
	brandStyle  lipgloss.Style // the "atelier" chip
	barStyle    lipgloss.Style // the selected row's left bar
	ruleStyle   lipgloss.Style

	waitStyle, runStyle, idleStyle, stopStyle lipgloss.Style
)

func init() { applyTheme(true) }

// applyTheme sets every style for a dark (or light) background.
func applyTheme(dark bool) {
	isDark = dark
	accent, subtle = adaptive(palette.Accent), adaptive(palette.Subtle)
	errColor, okColor = adaptive(palette.Err), adaptive(palette.OK)
	primary, purple, warn, muted = adaptive(cPrimary), adaptive(cPurple), adaptive(cWarn), adaptive(cMuted)
	faint, border, surface, onBrand = adaptive(cFaint), adaptive(cBorder), adaptive(cSurface), adaptive(cOnBrand)
	onSel = adaptive(cOnSel)

	s := lipgloss.NewStyle()
	headerStyle = s.Bold(true)
	selStyle = s.Background(surface).Foreground(onSel)
	markStyle = s.Foreground(accent).Bold(true)
	dimStyle = s.Foreground(muted)
	labelStyle = s.Foreground(muted)
	errStyle = s.Foreground(errColor)
	okStyle = s.Foreground(okColor)
	warnStyle = s.Foreground(warn).Bold(true)
	titleStyle = s.Bold(true)
	sepStyle = s.Foreground(border)
	keyStyle = s.Foreground(primary).Bold(true)
	descStyle = s.Foreground(muted)
	brandStyle = s.Background(primary).Foreground(onBrand).Bold(true)
	barStyle = s.Foreground(primary).Background(surface)
	ruleStyle = s.Foreground(border)

	// Session state styles come from the palette, shared with `atl statusline`.
	waitStyle = lookStyle(palette.StateLook(model.StateWaiting))
	runStyle = lookStyle(palette.StateLook(model.StateRunning))
	idleStyle = lookStyle(palette.StateLook(model.StateIdle))
	stopStyle = lookStyle(palette.StateLook(model.StateStopped))
	legends = buildLegends()
}

func adaptive(c palette.Color) color.Color {
	return lipgloss.LightDark(isDark)(lipgloss.Color(c.Light), lipgloss.Color(c.Dark))
}

// lookStyle is the lipgloss style of a palette look.
func lookStyle(l palette.Look) lipgloss.Style {
	s := lipgloss.NewStyle()
	if l.Color != (palette.Color{}) {
		s = s.Foreground(adaptive(l.Color))
	}
	if l.Bold {
		s = s.Bold(true)
	}
	if l.Faint {
		s = s.Faint(true)
	}
	return s
}

// statusColor is the colour a task status is drawn with (group headings,
// the status chip).
func statusColor(st model.Status) color.Color {
	switch st {
	case model.StatusActive:
		return primary
	case model.StatusReview:
		return purple
	case model.StatusDone:
		return okColor
	case model.StatusCancelled:
		return faint
	}
	return muted // backlog
}

// chip renders s as a filled label in colour c.
func chip(s string, c color.Color) string {
	return lipgloss.NewStyle().Background(c).Foreground(onBrand).Bold(true).Render(" " + s + " ")
}

// seg is a piece of plain text and its style, for rowText.
type seg struct {
	text string
	st   lipgloss.Style
}

// rowText draws a list row of width w from segs (plain text each), after a
// two-cell lead. A selected row has a bar in the primary colour for its
// lead and the selection background across the whole width.
func rowText(sel bool, w int, segs ...seg) string {
	if w <= 0 {
		return ""
	}
	var b strings.Builder
	used := 2
	if sel {
		b.WriteString(barStyle.Render("▌") + selStyle.Render(" "))
	} else {
		b.WriteString("  ")
	}
	if w < 2 {
		return trunc(b.String(), w)
	}
	for _, s := range segs {
		rem := w - used
		if rem <= 0 {
			break
		}
		t := trunc(s.text, rem)
		st := s.st
		if sel {
			st = onSelected(st)
		}
		b.WriteString(st.Render(t))
		used += ansi.StringWidth(t)
	}
	if sel && used < w {
		b.WriteString(selStyle.Render(strings.Repeat(" ", w-used)))
	}
	return b.String()
}

// onSelected is st on the selected row: its background, and its text
// colour when st has none (readable whichever background the terminal
// really has).
func onSelected(st lipgloss.Style) lipgloss.Style {
	st = st.Background(surface)
	if _, none := st.GetForeground().(lipgloss.NoColor); none {
		st = st.Foreground(onSel)
	}
	return st
}
