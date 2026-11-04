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
	contextMaxLen  = 2500 // bytes (so also at most this many characters)
	contextNotes   = 5    // newest notes included
	contextNoteLen = 300  // characters per note body
)

// sessionStartOutput is Claude Code's SessionStart hook JSON output.
type sessionStartOutput struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// writeSessionContext builds the task context for task id and writes it to
// w as one SessionStart JSON object. Nothing is written on error.
func writeSessionContext(ctx context.Context, w io.Writer, st *store.Store, p config.Paths, taskID int64, now time.Time) error {
	text, err := sessionContext(ctx, st, p, taskID, now)
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

// sessionContext loads task id and its newest notes (one task query, one
// repos query, one notes query) and renders them.
func sessionContext(ctx context.Context, st *store.Store, p config.Paths, taskID int64, now time.Time) (string, error) {
	t, err := st.GetTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	notes, err := st.ListNotes(ctx, taskID, contextNotes)
	if err != nil {
		return "", err
	}
	return renderContext(*t, notes, p, now), nil
}

// renderContext renders the SessionStart task context: plain markdown of
// at most contextMaxLen bytes. Notes are dropped (oldest first, then the
// last one kept is shortened) before anything else is cut.
func renderContext(t model.Task, notes []model.Note, p config.Paths, now time.Time) string {
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

	footer := fmt.Sprintf("\nUse `atl note %s \"...\"` to leave a status note for later sessions.\n", slug)
	fixed := head.String()
	if len(fixed)+len(footer) > contextMaxLen {
		// Pathological (huge title or many repos): no room for notes.
		return truncateBytes(fixed, contextMaxLen-len(footer)-len("…\n")) + "…\n" + footer
	}

	var body strings.Builder
	if len(notes) > 0 {
		const title = "\nNotes (newest first):\n"
		room := contextMaxLen - len(fixed) - len(footer) - len(title)
		var lines []string
		for _, n := range notes {
			line := fmt.Sprintf("- %s [%s] %s\n", n.CreatedAt.UTC().Format("2006-01-02"), oneLine(n.Author),
				truncateRunes(oneLine(n.Body), contextNoteLen))
			if len(line) > room {
				// Shorten this note to fit if a useful part remains.
				const minNote = 40
				if room >= minNote {
					lines = append(lines, truncateBytes(line[:len(line)-1], room-len("…\n"))+"…\n")
				}
				break
			}
			room -= len(line)
			lines = append(lines, line)
		}
		if len(lines) > 0 {
			body.WriteString(title)
			for _, l := range lines {
				body.WriteString(l)
			}
		}
	}
	return fixed + body.String() + footer
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

// truncateRunes shortens s to at most n characters, ending with "…" when cut.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimRight(string(r[:n-1]), " ") + "…"
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
