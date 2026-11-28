package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/learn"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/store"
)

// Session context limits.
const (
	// contextMaxLen bounds the task context (bytes, so also at most this
	// many characters), not counting the "User preferences" section,
	// which has a budget of its own on top (contextPrefsLen).
	contextMaxLen = 2500
	contextEnvLen = 1000 // bytes of ATL_CONTEXT text
	// contextMinNotes is the least room worth a notes section: with less,
	// the notes are left out (`atl notes <slug>` still has them).
	contextMinNotes = 40
	// contextPrefsLen bounds the "User preferences" section's text (the
	// injection snapshot, cut to [learn] inject_budget when it was
	// rendered); it adds at most this plus its blank line and final
	// newline.
	contextPrefsLen = learn.MaxInjectBudget
	// contextTotalLen bounds the whole start context.
	contextTotalLen = contextMaxLen + contextPrefsLen + 2
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
func writeSessionContext(ctx context.Context, w io.Writer, st *store.Store, p config.Paths, taskID int64, envText string, sandboxed bool, roMounts []string, now time.Time) error {
	text, err := sessionContext(ctx, st, p, taskID, envText, sandboxed, roMounts, now)
	if err != nil {
		return err
	}
	return writeStartOutput(w, text)
}

// writeStartOutput writes text to w as one SessionStart JSON object.
func writeStartOutput(w io.Writer, text string) error {
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

// sessionContext loads task id, its notes and the injection snapshot (one
// task query, one repos query, one notes query, one snapshot query) and
// renders them.
func sessionContext(ctx context.Context, st *store.Store, p config.Paths, taskID int64, envText string, sandboxed bool, roMounts []string, now time.Time) (string, error) {
	t, err := st.GetTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	notes, err := st.GetNotes(ctx, taskID)
	if err != nil {
		return "", err
	}
	prefs, err := loadInjection(ctx, st)
	if err != nil {
		// Logged; the session still gets its task context, without the section.
		logError(p, io.Discard, EventSessionStart, "", fmt.Errorf("session context: user preferences: %w", err))
		prefs = ""
	}
	return renderStartContext(*t, notes, p, envText, prefs, sandboxed, roMounts, now), nil
}

// loadInjection reads the injection snapshot (store.Injection); tests
// change it.
var loadInjection = func(ctx context.Context, st *store.Store) (string, error) { return st.Injection(ctx) }

// prefsHead is the first line of the "User preferences" section.
const prefsHead = "User preferences (learned across tasks, approved by the user):"

// Preferences renders the injection snapshot (store.Injection) from
// texts, the accepted learnings, highest score first: prefsHead, then a
// line "- <text>" for each (sanitised as oneLine does; a text left empty
// is skipped), lines joined by newlines, no final newline. When they do
// not all fit in budget bytes, it keeps the lines that do (in order: it
// stops at the first that does not) and ends with "- … (N more: atl
// learnings)", within budget too. "" when there are no texts, or not even
// the head and that line fit.
func Preferences(texts []string, budget int) string {
	var lines []string
	for _, t := range texts {
		if l := oneLine(t); l != "" {
			lines = append(lines, "- "+l)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	size, n := len(prefsHead), 0
	for n < len(lines) && size+1+len(lines[n]) <= budget {
		size += 1 + len(lines[n])
		n++
	}
	if n == len(lines) {
		return prefsHead + "\n" + strings.Join(lines, "\n")
	}
	for ; n >= 0; n-- {
		more := fmt.Sprintf("- … (%d more: atl learnings)", len(lines)-n)
		kept := append(append([]string{prefsHead}, lines[:n]...), more)
		if s := strings.Join(kept, "\n"); len(s) <= budget {
			return s
		}
	}
	return ""
}

// prefsBlock sanitises the injection snapshot for the context as
// notesBlock does, and cuts it to contextPrefsLen bytes at a line end
// (atl wrote it within the budget: this only bounds what the hook passes
// on).
func prefsBlock(s string) string {
	s = notesBlock(s)
	if len(s) > contextPrefsLen {
		s = s[:strings.LastIndexByte(s[:contextPrefsLen+1], '\n')+1]
		s = strings.TrimRight(s, " \n")
	}
	return s
}

// prefsSection is the "User preferences" section of the start context:
// a blank line, then the snapshot; "" when it is empty.
func prefsSection(snapshot string) string {
	if b := prefsBlock(snapshot); b != "" {
		return "\n" + b + "\n"
	}
	return ""
}

// sandboxNote tells a session that runs in the task's own container (see
// package sandbox) what that changes: a repo with its own container setup
// is built and tested in it, through the gate (whose refusals it lists).
const sandboxNote = "Sandbox: you run in the task's own container. " +
	"Build, test, run a repo in its own container setup, if any (`.devcontainer`, compose, " +
	"or per its docs/CLAUDE.md). " +
	"Else work here, installing what's missing (system packages: " +
	"`docker exec -u 0 $ATL_CONTAINER apt-get install -y <pkg>`); installs are lost with this container: " +
	"tell the user what the image lacks. " +
	"Docker gate: bind mounts only from the task dir; network/volume names, build tags start with $COMPOSE_PROJECT_NAME " +
	"(per task; a 2nd compose project: `-p $COMPOSE_PROJECT_NAME-<repo>`); no privileged or host modes; " +
	"classic builder only (DOCKER_BUILDKIT=0); pull build bases first (allowed registries); " +
	"you see only this task's containers, networks, volumes; services answer by service name. " +
	"If a setup needs something refused (host file mount, privileged, BuildKit), don't edit " +
	"the repo: override it in a task-dir file (compose: an extra `-f`) and tell the user. " +
	"What this session starts ends with it (dev servers too); what should outlive it " +
	"goes in a Docker service. " +
	"No git credentials here (`git push`, `gh` fail): use `atl push`, `atl fetch`."

// renderContext renders the SessionStart task context: plain markdown of
// at most contextMaxLen bytes. envText (the raw ATL_CONTEXT value) is
// sanitised and, if not blank, rendered as its own section after
// ATL_PORT_OFFSET. The notes text comes last (before the footer) and is the
// first thing cut: when it does not fit, its END is kept (appended notes
// are the newest), cut at a line start (or a rune boundary when one line
// is too long) after a truncation marker.
func renderContext(t model.Task, notes string, p config.Paths, envText string, now time.Time) string {
	return renderSandboxContext(t, notes, p, envText, false, nil, now)
}

// mountsLineMax bounds the line that names the host paths a sandboxed task
// has read-only (see mountsLine).
const mountsLineMax = 240

// mountsHead starts the line that names the host paths a sandboxed task has
// read-only ($ATL_SANDBOX_MOUNTS): at the same paths in its container, and
// bind-mountable, read-only only, through its gate.
const mountsHead = "Host paths here read-only (your containers may bind-mount them, read-only): "

// mountsLine returns the line (with its newline) that names paths, at most
// limit bytes long: as many paths as fit, in order, then how many more; ""
// when not even their number fits, or there are none.
func mountsLine(paths []string, limit int) string {
	if len(paths) == 0 {
		return ""
	}
	for n := len(paths); n >= 0; n-- {
		names := make([]string, n)
		for i, p := range paths[:n] {
			names[i] = oneLine(p)
		}
		list := strings.Join(names, ", ")
		switch rest := len(paths) - n; {
		case n == 0:
			list = fmt.Sprintf("%d, not listed here", rest)
		case rest > 0:
			list += fmt.Sprintf(" and %d more", rest)
		}
		if line := mountsHead + list + "\n"; len(line) <= limit {
			return line
		}
	}
	return ""
}

// notesMarker is the line before notes that were cut from the start.
func notesMarker(slug string) string {
	return "… (earlier notes truncated; `atl notes " + slug + "` for all)\n"
}

// renderSandboxContext is renderContext with, if sandboxed, the sandbox
// note after ATL_PORT_OFFSET, followed by the line that names roMounts
// (mountsLine): at most mountsLineMax bytes, and less where it would leave
// the notes less than the useful part notesTail keeps of them.
func renderSandboxContext(t model.Task, notes string, p config.Paths, envText string, sandboxed bool, roMounts []string, now time.Time) string {
	return renderStartContext(t, notes, p, envText, "", sandboxed, roMounts, now)
}

// renderStartContext is renderSandboxContext with the "User preferences"
// section (prefsSection of snapshot, the injection snapshot) right after
// the task block (and the mounts line), before the environment notes and
// the notes. It has its own budget: the rest stays within contextMaxLen
// as without it, the whole within contextTotalLen.
func renderStartContext(t model.Task, notes string, p config.Paths, envText, snapshot string, sandboxed bool, roMounts []string, now time.Time) string {
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
	if sandboxed {
		w("\n%s\n", sandboxNote)
	}
	top := head.String()
	head.Reset()
	if env := envBlock(envText); env != "" {
		w("\nEnvironment notes (%s):\n%s\n", envContextVar, env)
	}

	footer := fmt.Sprintf("\nUse `atl note %s \"...\"` to leave a status note for later sessions.\n", slug)
	const title = "\nNotes:\n"
	text := notesBlock(notes)
	fixed := top + head.String()
	mounts := ""
	if sandboxed && len(roMounts) > 0 {
		// Out of the notes' room, leaving them what notesTail needs.
		budget := contextMaxLen - len(fixed) - len(footer)
		if text != "" {
			budget -= len(title) + min(len(text), len(notesMarker(slug))+contextMinNotes) + 1
		}
		mounts = mountsLine(roMounts, min(budget, mountsLineMax))
		fixed = top + mounts + head.String()
	}
	// The preferences go after the task block, out of no other part's room.
	block, prefs := len(top)+len(mounts), prefsSection(snapshot)
	if len(fixed)+len(footer) > contextMaxLen {
		// Pathological (huge title or many repos): no room for notes. The
		// cut ends at a line end, its marker on a line of its own, and the
		// preferences come whole after the task block's part that is kept.
		cut := truncateBytes(fixed, contextMaxLen-len(footer)-len("…\n"))
		cut = cut[:strings.LastIndexByte(cut, '\n')+1]
		if len(cut) <= block {
			return cut + "…\n" + prefs + footer
		}
		return cut[:block] + prefs + cut[block:] + "…\n" + footer
	}
	room := contextMaxLen - len(fixed) - len(footer) - len(title)
	fixed = fixed[:block] + prefs + fixed[block:]
	if body := notesTail(text, room, slug); body != "" {
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
	marker := notesMarker(slug)
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
	s = strings.TrimRight(keepLines(s), " \n")
	return strings.TrimLeft(s, "\n")
}

// keepLines turns \r\n and \r into \n and every other control or format
// character into a space (see unprintable).
func keepLines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.Map(func(r rune) rune {
		if r != '\n' && unprintable(r) {
			return ' '
		}
		return r
	}, s)
}

// unprintable reports whether r is a control or format character (newline,
// tab, escape, bidi override, ...) or invalid UTF-8.
func unprintable(r rune) bool {
	return r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
}

// oneLine sanitises s for a single line: control and format characters
// (newlines, tabs, escapes, bidi overrides, ...) become spaces and runs of
// whitespace collapse to one space.
func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if unprintable(r) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// CheckEnvContext returns an error when $ATL_CONTEXT, once sanitised, is
// longer than the hook passes on. atl refuses to start with such a value
// (the hook itself still only cuts it: failing there would silently lose
// the whole session context).
func CheckEnvContext() error {
	if err := checkEnvContext(os.Getenv(envContextVar)); err != nil {
		return err
	}
	return checkContextVar(SandboxContextVar, os.Getenv(SandboxContextVar))
}

// SandboxContextVar names the environment variable whose text sandboxed
// sessions get in place of $ATL_CONTEXT: atl passes it to a task container
// as that container's ATL_CONTEXT. Rules for the place atl itself runs in
// (a shared box) do not describe a task's own container.
const SandboxContextVar = "ATL_SANDBOX_CONTEXT"

func checkEnvContext(raw string) error { return checkContextVar(envContextVar, raw) }

func checkContextVar(name, raw string) error {
	if n := len(sanitiseEnv(raw)); n > contextEnvLen {
		return fmt.Errorf("$%s is %d bytes, max %d", name, n, contextEnvLen)
	}
	return nil
}

// envBlock sanitises the ATL_CONTEXT text (see sanitiseEnv) and cuts the
// result to contextEnvLen bytes on a rune boundary, ending with "…" when cut.
func envBlock(s string) string {
	s = sanitiseEnv(s)
	if len(s) > contextEnvLen {
		s = strings.TrimRight(truncateBytes(s, contextEnvLen-len("…")), " \n") + "…"
	}
	return s
}

// sanitiseEnv keeps line breaks (\r\n and \r become \n), turns other control
// and format characters into spaces (as in oneLine) and trims surrounding
// whitespace.
func sanitiseEnv(s string) string {
	return strings.TrimSpace(keepLines(s))
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
