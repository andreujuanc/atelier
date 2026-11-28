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
	"unicode/utf16"
	"unicode/utf8"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/learn"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/store"
)

// hookContextLimit is Claude Code's cap on a hook's additionalContext,
// in characters (https://code.claude.com/docs/en/hooks). Over it, Claude
// Code replaces the whole context with a file path and a preview of its
// first 2,000 characters; no setting raises it.
const hookContextLimit = 10000

// Session context limits. The whole context fits in contextMaxChars; the
// notes come last and get the room the rest leaves. The other sections
// have their own caps (the text they bound comes from outside the task),
// which at their largest still leave the task block room for a dozen repos
// (TestContextFixedFits); a task block beyond its room is cut.
const (
	// contextMaxChars bounds the whole start context, in chars (see
	// chars). It stays a little under hookContextLimit, in case Claude
	// Code's count differs at the edge (">" or ">=", line ends): the cap is
	// on the string, not on its JSON encoding.
	contextMaxChars = hookContextLimit - 100
	// contextMinNotes is the least room worth a notes section: with less,
	// the notes are left out (`atl notes <slug>`, in the cheat sheet, still
	// has them).
	contextMinNotes = 40
	contextEnvLen   = 1000 // bytes of ATL_CONTEXT text
	// contextPrefsLen bounds the "User preferences" section's text (the
	// injection snapshot, cut to [learn] inject_budget when it was
	// rendered); it adds at most this plus its blank line and final
	// newline.
	contextPrefsLen = learn.MaxInjectBudget
	// contextCheatLen bounds the atl cheat sheet of the start context
	// (cheatSheet): its longest variant with a slug of model.MaxSlugLen
	// bytes.
	contextCheatLen = len(cheatHost) + cheatHostSlugs*(model.MaxSlugLen-len(slugPlaceholder))
)

// slugPlaceholder stands for the task slug in the cheat sheet templates.
const slugPlaceholder = "<slug>"

// cheatCommon lists the atl commands every task session may run,
// sandboxed or not (the sandbox gate allows these).
const cheatCommon = "- `atl note <slug> \"...\"` — append a dated status note for later sessions; " +
	"authorship is recorded automatically (don't pass --author)\n" +
	"- `atl notes <slug>` — print the notes; `--set` replaces them from stdin, `--clear` empties them\n" +
	"- `atl push [--repo R] [--force]` — push atl/<slug> (--force = with-lease, after a rebase)\n" +
	"- `atl fetch [--repo R]` — git fetch origin\n"

// cheatSandboxed is the cheat sheet of a sandboxed session: the complete
// list of what its gate lets it run, so agents need not run `atl help`.
const cheatSandboxed = "\natl in this session (complete list, no need for `atl help`):\n" + cheatCommon +
	"Every other atl command is refused here; ask the user instead.\n"

// cheatHost is the cheat sheet of a session on the host, which can run
// every atl command: the useful ones, and those to leave to the user.
const cheatHost = "\natl in this session (no need for `atl help`):\n" + cheatCommon +
	"- `atl show <slug>` / `atl diff <slug>` / `atl transcript <slug> [--session ID]` — inspect the task\n" +
	"- `atl pr`, `done`, `cancel`, `status`, `add`, `rename`: only when the user asks " +
	"(they end or change the task and its sessions)\n"

// cheatHostSlugs is how many times cheatHost names the slug.
const cheatHostSlugs = 6

// cheatSheet is the atl cheat sheet of the start context (with a blank
// line before it), right before the notes: cheatSandboxed or cheatHost
// for slug. A slug
// longer than model.MaxSlugLen (atl makes none) stays slugPlaceholder, so
// the sheet stays within contextCheatLen.
func cheatSheet(slug string, sandboxed bool) string {
	if len(slug) > model.MaxSlugLen {
		slug = slugPlaceholder
	}
	tmpl := cheatHost
	if sandboxed {
		tmpl = cheatSandboxed
	}
	return strings.ReplaceAll(tmpl, slugPlaceholder, slug)
}

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

// renderContext renders the SessionStart task context, plain markdown of
// at most contextMaxChars chars: the task block, then the environment
// notes, the atl cheat sheet and the notes. envText (the raw ATL_CONTEXT
// value) is sanitised and, if not blank, rendered as its own section after
// ATL_PORT_OFFSET. The notes come last and get all the room the rest
// leaves: when they do not fit, their END is kept (appended notes are the
// newest), cut at a line start (or a rune boundary when one line is too
// long) after a truncation marker; with less than contextMinNotes chars of
// room for them, they are left out.
func renderContext(t model.Task, notes string, p config.Paths, envText string, now time.Time) string {
	return renderSandboxContext(t, notes, p, envText, false, nil, now)
}

// mountsLineMax bounds the line that names the host paths a sandboxed task
// has read-only (see mountsLine): $ATL_SANDBOX_MOUNTS can list any number.
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

// renderSandboxContext is renderContext with, if sandboxed, the sandbox
// note after ATL_PORT_OFFSET, followed by the line that names roMounts
// (mountsLine, at most mountsLineMax bytes).
func renderSandboxContext(t model.Task, notes string, p config.Paths, envText string, sandboxed bool, roMounts []string, now time.Time) string {
	return renderStartContext(t, notes, p, envText, "", sandboxed, roMounts, now)
}

// renderStartContext is renderSandboxContext with the "User preferences"
// section (prefsSection of snapshot, the injection snapshot) right after
// the task block (and the mounts line), before the environment notes and
// the notes. A task block too long to leave the other sections their room
// (pathological: very many repos or long paths) is cut at a line end, its
// ATL_PORT_OFFSET line kept, a "…" line in place of the rest.
func renderStartContext(t model.Task, notes string, p config.Paths, envText, snapshot string, sandboxed bool, roMounts []string, now time.Time) string {
	slug := oneLine(t.Slug)
	branch := t.Branch
	if branch == "" {
		branch = "atl/" + t.Slug
	}
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	// next returns what was written since the last call.
	next := func() string { s := b.String(); b.Reset(); return s }
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
	block := next()
	w("\nATL_PORT_OFFSET=%d\n", t.PortOffset)
	port := next()
	if sandboxed {
		w("\n%s\n", sandboxNote)
		b.WriteString(mountsLine(roMounts, mountsLineMax))
	}
	b.WriteString(prefsSection(snapshot))
	if env := envBlock(envText); env != "" {
		w("\nEnvironment notes (%s):\n%s\n", envContextVar, env)
	}
	b.WriteString(cheatSheet(slug, sandboxed))
	rest := next()

	if room := contextMaxChars - chars(port) - chars(rest); chars(block) > room {
		cut := truncateChars(block, room-chars("…\n"))
		block = cut[:strings.LastIndexByte(cut, '\n')+1] + "…\n"
	}
	out := block + port + rest
	const title = "\nNotes:\n"
	if body := notesTail(notesBlock(notes), contextMaxChars-chars(out)-chars(title), slug); body != "" {
		out += title + body + "\n"
	}
	return out
}

// notesMarker is the line before notes that were cut from the start.
func notesMarker(slug string) string {
	return "… (earlier notes truncated; `atl notes " + slug + "` for all)\n"
}

// notesTail returns text if it fits in room chars counting a final
// newline, else its end after a truncation marker line, both together
// within room: the kept part starts at a line start when there is one in
// it, else at a rune boundary. "" when room is too small for a useful part.
func notesTail(text string, room int, slug string) string {
	if text == "" {
		return ""
	}
	room-- // the newline after the section
	if chars(text) <= room {
		return text
	}
	marker := notesMarker(slug)
	keep := room - chars(marker)
	if keep < contextMinNotes {
		return ""
	}
	// The longest end of text within keep chars.
	start := len(text)
	for n := 0; start > 0; {
		r, size := utf8.DecodeLastRuneInString(text[:start])
		if n += runeChars(r); n > keep {
			break
		}
		start -= size
	}
	tail := text[start:]
	// Prefer a line boundary unless the line start is in the last chars
	// (one huge line): then keep its end from a rune boundary.
	if i := strings.IndexByte(tail, '\n'); i >= 0 && chars(tail[i+1:]) >= contextMinNotes {
		tail = tail[i+1:]
	}
	return marker + tail
}

// chars returns the length of s as JavaScript (and so Claude Code) counts
// it: in UTF-16 code units, a rune outside the BMP (an emoji) counting 2.
func chars(s string) int {
	n := 0
	for _, r := range s {
		n += runeChars(r)
	}
	return n
}

// runeChars is the length of r in UTF-16 code units (1 for invalid UTF-8,
// which ranging over a string yields as utf8.RuneError).
func runeChars(r rune) int {
	return max(utf16.RuneLen(r), 1)
}

// truncateChars cuts s to at most n chars (see chars) on a rune boundary.
func truncateChars(s string, n int) string {
	for i, r := range s {
		if n -= runeChars(r); n < 0 {
			return s[:i]
		}
	}
	return s
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
