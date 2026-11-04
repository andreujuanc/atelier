package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/store"
)

// Session context limits.
const (
	contextMaxLen = 2500 // bytes (so also at most this many characters)
	contextEnvLen = 1000 // bytes of ATL_CONTEXT text
	// contextMinNotes is the least room worth a notes section: with less,
	// the notes are left out (`atl notes <slug>` still has them).
	contextMinNotes = 40
)

// envContextVar names the environment variable whose text is appended to
// every task session's start context (e.g. set per devcontainer).
const envContextVar = "ATL_CONTEXT"

// sessionStartOutput is Claude Code's SessionStart hook JSON output.
type sessionStartOutput struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// writeSessionContext builds the task context for task id and writes it to
// w as one SessionStart JSON object. envText is the raw ATL_CONTEXT value
// (may be empty). Nothing is written on error.
func writeSessionContext(ctx context.Context, w io.Writer, st *store.Store, p config.Paths, taskID int64, envText string, now time.Time) error {
	text, err := sessionContext(ctx, st, p, taskID, envText, now)
	if err != nil {
		return err
	}
	var out sessionStartOutput
	out.HookSpecificOutput.HookEventName = EventSessionStart
	out.HookSpecificOutput.AdditionalContext = text
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// sessionContext loads task id and its notes (one task query, one repos
// query, one notes query) and renders them.
func sessionContext(ctx context.Context, st *store.Store, p config.Paths, taskID int64, envText string, now time.Time) (string, error) {
	t, err := st.GetTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	notes, err := st.GetNotes(ctx, taskID)
	if err != nil {
		return "", err
	}
	return renderContext(*t, notes, p, envText, now), nil
}

// renderContext renders the SessionStart task context: plain markdown of
// at most contextMaxLen bytes. envText (the raw ATL_CONTEXT value) is
// sanitised and, if not blank, rendered as its own section after
// ATL_PORT_OFFSET. The notes text comes last (before the footer) and is the
// first thing cut: when it does not fit, its END is kept (appended notes
// are the newest), cut at a line start (or a rune boundary when one line
// is too long) after a truncation marker.
func renderContext(t model.Task, notes string, p config.Paths, envText string, now time.Time) string {
	slug := oneLine(t.Slug)
	branch := t.Branch
	if branch == "" {
		branch = "atl/" + t.Slug
	}
	var head strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&head, format, a...) }
	w("# atelier task context (auto-generated, current as of %s)\n\n", now.Format(time.RFC3339))
	w("- Title: %s\n", oneLine(t.Title))
	w("- Slug: %s\n", slug)
	w("- Status: %s\n", oneLine(string(t.Status)))
	w("- Branch: %s\n", oneLine(branch))
	w("- Task dir: %s\n\n", oneLine(p.TaskDir(t.Slug)))
	w("Repos:\n")
	for _, r := range t.Repos {
		name := oneLine(r.Name)
		if r.Mode == model.ModeRO {
			w("- %s (ro, %s, READ-ONLY: do not modify by any means)\n", name, oneLine(p.RepoPath(r.Name)))
			continue
		}
		from := ""
		if r.BaseBranch != "" {
			from = ", branched from origin/" + oneLine(r.BaseBranch)
		}
		w("- %s (rw, worktree %s%s)\n", name, oneLine(p.WorktreePath(t.Slug, r.Name)), from)
	}
	w("\nATL_PORT_OFFSET=%d\n", t.PortOffset)
	if env := envBlock(envText); env != "" {
		w("\nEnvironment notes (%s):\n%s\n", envContextVar, env)
	}

	footer := fmt.Sprintf("\nUse `atl note %s \"...\"` to leave a status note for later sessions.\n", slug)
	fixed := head.String()
	if len(fixed)+len(footer) > contextMaxLen {
		// Pathological (huge title or many repos): no room for notes.
		return truncateBytes(fixed, contextMaxLen-len(footer)-len("…\n")) + "…\n" + footer
	}
	const title = "\nNotes:\n"
	room := contextMaxLen - len(fixed) - len(footer) - len(title)
	if body := notesTail(notesBlock(notes), room, slug); body != "" {
		return fixed + title + body + "\n" + footer
	}
	return fixed + footer
}

// notesTail returns text if it fits in room bytes counting a final
// newline, else its end after a truncation marker line, both together
// within room: the kept part starts at a line start when there is one in
// it, else at a rune boundary. "" when room is too small for a useful part.
func notesTail(text string, room int, slug string) string {
	if text == "" {
		return ""
	}
	room-- // the newline after the section
	if len(text) <= room {
		return text
	}
	marker := "… (earlier notes truncated; `atl notes " + slug + "` for all)\n"
	keep := room - len(marker)
	if keep < contextMinNotes {
		return ""
	}
	start := len(text) - keep
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	tail := text[start:]
	// Prefer a line boundary unless the line start is in the last bytes
	// (one huge line): then keep its end from a rune boundary.
	if i := strings.IndexByte(tail, '\n'); i >= 0 && len(tail)-i-1 >= contextMinNotes {
		tail = tail[i+1:]
	}
	return marker + tail
}

// notesBlock sanitises the notes text for the context: line breaks are
// kept, tabs become spaces, other control and format characters are
// dropped or become spaces (as in envBlock), and surrounding blank lines
// and trailing spaces are trimmed.
func notesBlock(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimRight(s, " \n")
	return strings.TrimLeft(s, "\n")
}

// oneLine sanitises s for a single line: control and format characters
// (newlines, tabs, escapes, bidi overrides, ...) become spaces and runs of
// whitespace collapse to one space.
func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// envBlock sanitises the ATL_CONTEXT text: line breaks are kept (\r\n and
// \r become \n), other control and format characters become spaces (as in
// oneLine), surrounding whitespace is trimmed, and the result is cut to
// contextEnvLen bytes on a rune boundary, ending with "…" when cut.
func envBlock(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) > contextEnvLen {
		s = strings.TrimRight(truncateBytes(s, contextEnvLen-len("…")), " \n") + "…"
	}
	return s
}

// truncateBytes cuts s to at most n bytes on a rune boundary.
func truncateBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
