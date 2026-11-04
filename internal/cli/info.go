package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/andreujuanc/atelier/internal/app"
	"github.com/andreujuanc/atelier/internal/model"
)

// wrapWidth is the column at which transcript and note text is wrapped.
const wrapWidth = 100

// `atl show --json` prints one object (stable; times are RFC3339 UTC):
//
//	{"id":1,"slug":"fix-login","title":"Fix login","description":"","status":"active","project":"shop",
//	 "branch":"atl/fix-login","port_offset":100,"base":"","task_dir":"/home/u/tasks/fix-login",
//	 "created_at":"…","updated_at":"…",
//	 "repos":[{"name":"api","mode":"rw","path":"/home/u/tasks/fix-login/api","base_branch":"dev","dirty":true,"ahead":2,
//	           "pushed":false,"unpushed":2,"last_commit":{"sha":"…","subject":"…","date":"…"},"error":""}],
//	 "sessions":[{"id":3,"name":"claude-3","claude_session_id":"…","state":"waiting","tmux_target":"@4",
//	              "created_at":"…","last_event_at":"…","transcript_path":"/home/u/.claude/projects/…/….jsonl",
//	              "title":"Fix the login redirect"}],
//	 "notes":"- 2026-11-04 [user] waiting on design review\n…"}
//
// base is the task's base choice: "" (auto: origin/dev, else develop, else
// the default branch), ":default" (origin's default branch) or a branch
// name. base_branch is the origin branch a rw repo's task branch started
// from ("" for ro). last_commit is null when HEAD could not be read; error is "missing" when
// the repo directory is gone, else a git error or "". unpushed is the
// number of commits not in origin/<branch> when pushed, else equal to
// ahead (0 when the repo directory is gone). ahead, pushed and unpushed are
// always 0/false for ro repos. A session's title comes from its transcript
// (Claude Code's title, else the first user prompt; "" if none). notes is the task's notes text, a string
// ("" when empty, never null; before migration v5 it was an array of dated notes). repos and sessions are
// never null.
type jsonShow struct {
	ID          int64             `json:"id"`
	Slug        string            `json:"slug"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Status      string            `json:"status"`
	Project     string            `json:"project"`
	Branch      string            `json:"branch"`
	PortOffset  int               `json:"port_offset"`
	Base        string            `json:"base"`
	TaskDir     string            `json:"task_dir"`
	CreatedAt   string            `json:"created_at"`
	UpdatedAt   string            `json:"updated_at"`
	Repos       []jsonRepoDetail  `json:"repos"`
	Sessions    []jsonShowSession `json:"sessions"`
	Notes       string            `json:"notes"`
}

type jsonRepoDetail struct {
	Name       string      `json:"name"`
	Mode       string      `json:"mode"`
	Path       string      `json:"path"`
	BaseBranch string      `json:"base_branch"`
	Dirty      bool        `json:"dirty"`
	Ahead      int         `json:"ahead"`
	Pushed     bool        `json:"pushed"`
	Unpushed   int         `json:"unpushed"`
	LastCommit *jsonCommit `json:"last_commit"`
	Error      string      `json:"error"`
}

type jsonCommit struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
	Date    string `json:"date"`
}

type jsonShowSession struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	ClaudeSessionID string `json:"claude_session_id"`
	State           string `json:"state"`
	TmuxTarget      string `json:"tmux_target"`
	CreatedAt       string `json:"created_at"`
	LastEventAt     string `json:"last_event_at"`
	TranscriptPath  string `json:"transcript_path"`
	Title           string `json:"title"`
}

func showJSON(d *app.TaskDetail) jsonShow {
	t := d.Task
	out := jsonShow{
		ID: t.ID, Slug: t.Slug, Title: t.Title, Description: t.Description, Status: string(t.Status),
		Project: t.Project, Branch: t.Branch, PortOffset: t.PortOffset, Base: t.Base, TaskDir: d.TaskDir,
		CreatedAt: rfc3339(t.CreatedAt), UpdatedAt: rfc3339(t.UpdatedAt),
		Repos: []jsonRepoDetail{}, Sessions: []jsonShowSession{}, Notes: d.Notes,
	}
	for _, r := range d.Repos {
		jr := jsonRepoDetail{Name: r.Name, Mode: string(r.Mode), Path: r.Path, BaseBranch: r.BaseBranch, Dirty: r.Dirty, Ahead: r.Ahead,
			Pushed: r.Pushed, Unpushed: r.Unpushed, Error: r.Err}
		if c := r.LastCommit; c != nil {
			jr.LastCommit = &jsonCommit{SHA: c.SHA, Subject: c.Subject, Date: rfc3339(c.Date)}
		}
		out.Repos = append(out.Repos, jr)
	}
	for _, s := range d.Sessions {
		out.Sessions = append(out.Sessions, jsonShowSession{ID: s.ID, Name: s.Name, ClaudeSessionID: s.ClaudeSessionID,
			State: string(s.State), TmuxTarget: s.TmuxTarget, CreatedAt: rfc3339(s.CreatedAt),
			LastEventAt: rfc3339(s.LastEventAt), TranscriptPath: s.TranscriptPath, Title: d.SessionTitles[s.ID]})
	}
	return out
}

// localTime formats t for humans (local time, minutes).
func localTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func (c *cli) showCmd() *cobra.Command {
	var asJSON, notesOnly bool
	cmd := &cobra.Command{
		Use:   "show <task> [--json | --notes]",
		Short: "Show everything about a task: repos (git state), sessions, notes",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			if asJSON && notesOnly {
				return usagef("--json and --notes are mutually exclusive")
			}
			ap, err := c.App()
			if err != nil {
				return err
			}
			if notesOnly {
				// Read-only spelling of `atl notes <task>` (the assistant
				// may run `atl show` without asking; `atl notes` asks
				// because of its --set form).
				return c.printNotes(cmd, ap, a[0])
			}
			d, err := ap.Show(cmd.Context(), a[0])
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(c.stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(showJSON(d))
			}
			writeShow(c.stdout, d)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().BoolVar(&notesOnly, "notes", false, "print only the task's notes text (like atl notes <task>)")
	return cmd
}

// printNotes prints task ref's notes text (nothing when empty).
func (c *cli) printNotes(cmd *cobra.Command, ap *app.App, ref string) error {
	_, text, err := ap.Notes(cmd.Context(), ref)
	if err != nil {
		return err
	}
	if text = model.SanitizeNotes(text); text != "" {
		fmt.Fprintln(c.stdout, text)
	}
	return nil
}

func writeShow(w io.Writer, d *app.TaskDetail) {
	t := d.Task
	kv := func(k, v string) { fmt.Fprintf(w, "  %-12s %s\n", k, v) }
	fmt.Fprintln(w, "Task")
	kv("id", strconv.FormatInt(t.ID, 10))
	kv("slug", t.Slug)
	kv("title", sanitize(t.Title))
	kv("status", string(t.Status))
	kv("project", orDash(t.Project))
	kv("branch", t.Branch)
	kv("base", sanitize(app.BaseLabel(t.Base)))
	kv("port offset", strconv.Itoa(t.PortOffset))
	kv("dir", d.TaskDir)
	kv("created", localTime(t.CreatedAt))
	kv("updated", localTime(t.UpdatedAt))
	if desc := strings.TrimSpace(sanitize(t.Description)); desc != "" {
		fmt.Fprintln(w, "  description")
		writeWrapped(w, "    ", desc)
	}

	fmt.Fprintln(w, "\nRepos")
	if len(d.Repos) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, r := range d.Repos {
		fmt.Fprintf(w, "  %s (%s)  %s\n", r.Name, r.Mode, r.Path)
		if r.BaseBranch != "" {
			fmt.Fprintf(w, "    base origin/%s\n", sanitize(r.BaseBranch))
		}
		if r.Err == "missing" {
			fmt.Fprintln(w, "    missing (worktree removed)")
			if r.Mode == model.ModeRW {
				fmt.Fprintf(w, "    %s\n", pushedText(r.Pushed, 0, t.Branch))
			}
			continue
		}
		state := "clean"
		if r.Dirty {
			state = "dirty"
		}
		if r.Mode == model.ModeRW {
			state += fmt.Sprintf(", %d commit(s) ahead of base, %s", r.Ahead, pushedText(r.Pushed, r.Unpushed, t.Branch))
		}
		fmt.Fprintf(w, "    %s\n", state)
		if lc := r.LastCommit; lc != nil {
			fmt.Fprintf(w, "    last commit %s %s (%s)\n", shortSHA(lc.SHA), sanitize(lc.Subject), localTime(lc.Date))
		}
		if r.Err != "" {
			fmt.Fprintf(w, "    error: %s\n", sanitize(r.Err))
		}
	}

	fmt.Fprintln(w, "\nSessions")
	if len(d.Sessions) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, s := range d.Sessions {
		name := s.Name
		if name == "" {
			name = "claude-" + strconv.FormatInt(s.ID, 10)
		}
		fmt.Fprintf(w, "  %d %s  %s  last event %s\n", s.ID, name, s.State, localTime(s.LastEventAt))
		if title := d.SessionTitles[s.ID]; title != "" {
			fmt.Fprintf(w, "    title       %s\n", sanitize(title))
		}
		if s.ClaudeSessionID != "" {
			fmt.Fprintf(w, "    claude id   %s\n", s.ClaudeSessionID)
		}
		if s.TranscriptPath != "" {
			fmt.Fprintf(w, "    transcript  %s\n", s.TranscriptPath)
		}
	}

	fmt.Fprintln(w, "\nNotes")
	if strings.TrimSpace(d.Notes) == "" {
		fmt.Fprintln(w, "  (none)")
		return
	}
	writeNotes(w, "  ", d.Notes)
}

// writeNotes prints a notes text wrapped at wrapWidth, each line prefixed
// by indent; a wrapped line's continuation keeps the line's own leading
// indentation plus two spaces (so list items stay readable).
func writeNotes(w io.Writer, indent, text string) {
	for _, l := range strings.Split(sanitize(text), "\n") {
		lead := l[:len(l)-len(strings.TrimLeft(l, " "))]
		first := indent + lead
		for i, wl := range wrapLine(l, wrapWidth-len([]rune(first))-2) {
			pre := first
			if i > 0 {
				pre += "  "
			}
			fmt.Fprintln(w, strings.TrimRight(pre+wl, " "))
		}
	}
}

func pushedText(pushed bool, unpushed int, branch string) string {
	if pushed && unpushed > 0 {
		return fmt.Sprintf("pushed (origin/%s, %d unpushed)", branch, unpushed)
	}
	if pushed {
		return "pushed (origin/" + branch + ")"
	}
	return "not pushed"
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func (c *cli) transcriptCmd() *cobra.Command {
	var session int64
	var tail int
	cmd := &cobra.Command{
		Use:   "transcript <task> [--session ID] [--tail N]",
		Short: "Print the last turns of a session's Claude conversation (default: latest session with a transcript)",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			if tail < 0 {
				return usagef("--tail must be >= 0 (0 = all)")
			}
			if session < 0 {
				return usagef("--session must be a session id")
			}
			ap, err := c.App()
			if err != nil {
				return err
			}
			turns, err := ap.Transcript(cmd.Context(), a[0], session, tail)
			if err != nil {
				return err
			}
			for i, tr := range turns {
				if i > 0 {
					fmt.Fprintln(c.stdout)
				}
				head := "[" + tr.Role
				if !tr.Time.IsZero() {
					head += " " + tr.Time.Local().Format("2006-01-02 15:04:05")
				}
				writeBlock(c.stdout, "", head+"]", tr.Text)
			}
			return nil
		},
	}
	cmd.Flags().Int64Var(&session, "session", 0, "session id (see atl show)")
	cmd.Flags().IntVar(&tail, "tail", 20, "number of turns (0 = all)")
	return cmd
}

func (c *cli) noteCmd() *cobra.Command {
	var author string
	cmd := &cobra.Command{
		Use:   "note <task> <text...> [--author user|assistant]",
		Short: "Append a dated line (- YYYY-MM-DD [author] text) to a task's notes",
		Args:  args(cobra.MinimumNArgs(2)),
		RunE: func(cmd *cobra.Command, a []string) error {
			if !model.ValidAuthor(author) {
				return usagef("--author must be user or assistant, not %q", author)
			}
			body := strings.TrimSpace(strings.Join(a[1:], " "))
			if body == "" {
				return usagef("note text is empty")
			}
			ap, err := c.App()
			if err != nil {
				return err
			}
			if _, err := ap.AppendNote(cmd.Context(), a[0], body, author); err != nil {
				return err
			}
			fmt.Fprintln(c.stdout, "note added")
			return nil
		},
	}
	cmd.Flags().StringVar(&author, "author", model.AuthorUser, "user or assistant")
	return cmd
}

func (c *cli) notesCmd() *cobra.Command {
	var set, clearNotes bool
	cmd := &cobra.Command{
		Use:   "notes <task> [--set] [--clear]",
		Short: "Print a task's notes; --set replaces them with stdin, --clear clears them",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			ap, err := c.App()
			if err != nil {
				return err
			}
			if !set && !clearNotes {
				return c.printNotes(cmd, ap, a[0])
			}
			// Resolve first: a bad ref fails before stdin is read.
			t, err := ap.Store.ResolveTask(cmd.Context(), a[0])
			if err != nil {
				return err
			}
			text := "" // --clear alone
			if set {
				data, err := c.readNotesStdin(cmd)
				if err != nil {
					return err
				}
				// Empty (once sanitised) only clears with --clear too, so
				// an empty pipe by mistake never wipes the notes.
				if !clearNotes && model.SanitizeNotes(data) == "" {
					return errors.New("stdin is empty: nothing to set (use --clear to clear the notes)")
				}
				text = data
			}
			text, err = ap.ReplaceNotes(cmd.Context(), t.Slug, text)
			if err != nil {
				return err
			}
			if text == "" {
				fmt.Fprintln(c.stdout, "notes cleared")
			} else {
				fmt.Fprintf(c.stdout, "notes replaced (%d bytes)\n", len(text))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&set, "set", false, "replace the notes with the text read from stdin (empty stdin is an error unless --clear is given too)")
	cmd.Flags().BoolVar(&clearNotes, "clear", false, "clear the notes (alone: stdin is not read; with --set: allow an empty stdin)")
	return cmd
}

// readNotesStdin reads the new notes text for `atl notes --set`, with a
// hint on stderr when stdin is a terminal.
func (c *cli) readNotesStdin(cmd *cobra.Command) (string, error) {
	// The store checks the sanitised size (\r\n, control characters and
	// trailing blanks don't count); reading stops well past the limit so
	// a huge stdin is never fully read.
	const readMax = 4 * model.MaxNotesLen
	in := cmd.InOrStdin()
	if isTerminal(in) {
		fmt.Fprintln(c.stderr, "atl: reading the new notes from stdin; end with ctrl-d")
	}
	data, err := io.ReadAll(io.LimitReader(in, readMax+1))
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	if len(data) > readMax {
		return "", fmt.Errorf("%w: stdin is over %d bytes, the limit is %d (64 KiB)",
			model.ErrNotesTooLarge, readMax, model.MaxNotesLen)
	}
	return string(data), nil
}

// writeBlock prints "<head> <text>" wrapped at wrapWidth, each line
// prefixed by indent and continuation lines indented by two more spaces.
func writeBlock(w io.Writer, indent, head, text string) {
	text = sanitize(text)
	lines := strings.Split(text, "\n")
	lines[0] = head + " " + lines[0]
	width := wrapWidth - len([]rune(indent)) - 2 // room for the continuation indent
	for i, l := range lines {
		pre := indent
		if i > 0 {
			pre += "  "
		}
		for _, wl := range wrapLine(l, width) {
			fmt.Fprintln(w, strings.TrimRight(pre+wl, " "))
			pre = indent + "  "
		}
	}
}

// writeWrapped prints text wrapped at wrapWidth with every line prefixed
// by indent.
func writeWrapped(w io.Writer, indent, text string) {
	for _, l := range strings.Split(text, "\n") {
		for _, wl := range wrapLine(l, wrapWidth-len([]rune(indent))) {
			fmt.Fprintln(w, strings.TrimRight(indent+wl, " "))
		}
	}
}

// wrapLine word-wraps s to lines of at most width runes, breaking words
// longer than width. An empty s yields one empty line.
func wrapLine(s string, width int) []string {
	if width < 20 {
		width = 20
	}
	var out []string
	var cur []rune
	flush := func() {
		out = append(out, string(cur))
		cur = cur[:0]
	}
	for _, word := range strings.Fields(s) {
		wr := []rune(word)
		for len(wr) > width {
			if len(cur) > 0 {
				flush()
			}
			out = append(out, string(wr[:width]))
			wr = wr[width:]
		}
		switch {
		case len(cur) == 0:
			cur = append(cur, wr...)
		case len(cur)+1+len(wr) <= width:
			cur = append(append(cur, ' '), wr...)
		default:
			flush()
			cur = append(cur, wr...)
		}
	}
	if len(cur) > 0 || len(out) == 0 {
		flush()
	}
	return out
}

// sanitize makes text safe to print on a terminal: tabs become spaces and
// other control characters (ESC, CR, ...) except newlines are dropped.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return r
		case r == '\t':
			return ' '
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, s)
}
