// Package palette holds the colours atelier draws session states with, so
// the TUI (lipgloss) and `atl statusline` (raw ANSI for Claude Code's status
// line) show a state the same way. It has no dependencies besides model.
package palette

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/andreujuanc/atelier/internal/model"
)

// Color is a pair of "#RRGGBB" colours for light and dark terminal
// backgrounds (the fields of a lipgloss.AdaptiveColor).
type Color struct{ Light, Dark string }

// The TUI's colours.
var (
	Accent = Color{Light: "#B5179E", Dark: "#F7768E"}
	Subtle = Color{Light: "#888888", Dark: "#666666"}
	Err    = Color{Light: "#C00000", Dark: "#FF5F5F"}
	OK     = Color{Light: "#2E8B57", Dark: "#9ECE6A"}
)

// Look is how a session state is drawn: the TUI list dot, the session
// lists and the status line dot.
type Look struct {
	Glyph       string // "●", "○" (stopped) or " " (no live or stopped session)
	Color       Color  // zero: the terminal's default colour
	Bold, Faint bool
}

// StateLook returns the look of a session state ("" = a task with no live
// or stopped session; see model.TaskState).
func StateLook(st model.SessionState) Look {
	switch st {
	case model.StateWaiting:
		return Look{Glyph: "●", Color: Accent, Bold: true}
	case model.StateRunning:
		return Look{Glyph: "●", Color: OK}
	case model.StateIdle:
		return Look{Glyph: "●", Color: Subtle}
	case model.StateStopped:
		return Look{Glyph: "○", Color: Err, Faint: true}
	}
	return Look{Glyph: " "}
}

// ANSI returns s wrapped in SGR escapes for l: 24-bit foreground (the Dark
// or Light variant), bold, faint, then a reset. A look with no colour and
// no attributes returns s unchanged.
func (l Look) ANSI(s string, dark bool) string {
	var codes []string
	if l.Bold {
		codes = append(codes, "1")
	}
	if l.Faint {
		codes = append(codes, "2")
	}
	hex := l.Color.Light
	if dark {
		hex = l.Color.Dark
	}
	if r, g, b, ok := parseHex(hex); ok {
		codes = append(codes, fmt.Sprintf("38;2;%d;%d;%d", r, g, b))
	}
	if len(codes) == 0 {
		return s
	}
	return "\x1b[" + strings.Join(codes, ";") + "m" + s + "\x1b[0m"
}

func parseHex(h string) (r, g, b uint8, ok bool) {
	if len(h) != 7 || h[0] != '#' {
		return 0, 0, 0, false
	}
	v, err := strconv.ParseUint(h[1:], 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return uint8(v >> 16), uint8(v >> 8), uint8(v), true
}

// DarkBackground guesses whether the terminal background is dark from
// $COLORFGBG ("fg;bg" or "fg;default;bg", set by some terminals): bg 7 or
// 9-15 is light. Unset or unparsable means dark, the common case. It never
// queries the terminal (the status line command has no terminal).
func DarkBackground(colorFGBG string) bool {
	if colorFGBG == "" {
		return true
	}
	parts := strings.Split(colorFGBG, ";")
	bg, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return true
	}
	return !(bg == 7 || (bg >= 9 && bg <= 15))
}
