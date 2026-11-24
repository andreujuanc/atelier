package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/andreujuanc/atelier/internal/app"
	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/hook"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/project"
	"github.com/andreujuanc/atelier/internal/sandbox"
	"github.com/andreujuanc/atelier/internal/tmux"
	"github.com/andreujuanc/atelier/internal/workspace"
)

func (c *cli) newCmd() *cobra.Command {
	var o app.NewTaskOpts
	var repos []string
	var base string
	cmd := &cobra.Command{
		Use:   "new <title...> [--project P] [--repo name:rw|ro ...] [--base auto|default|production|<branch>] [--backlog] [--no-fetch] [-m description]",
		Short: "Create a task with worktrees on branch atl/<slug>; prints slug and task dir",
		Args:  args(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			o.Title = strings.Join(a, " ")
			if strings.TrimSpace(o.Title) == "" {
				return usagef("title is required")
			}
			if err := model.CheckTitleLen(strings.Join(strings.Fields(o.Title), " ")); err != nil {
				return usageError{err}
			}
			for _, r := range repos {
				ra, err := parseRepoArg("--repo", r)
				if err != nil {
					return err
				}
				o.Repos = append(o.Repos, ra)
			}
			b, err := parseBaseFlag(base)
			if err != nil {
				return err
			}
			o.Base = b
			ap, err := c.App()
			if err != nil {
				return err
			}
			t, err := ap.NewTask(cmd.Context(), o)
			if err != nil {
				return err
			}
			fmt.Fprintf(c.stdout, "%s\t%s\n", t.Slug, ap.Paths.TaskDir(t.Slug))
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.Project, "project", "p", "", "project whose repos to use")
	f.StringArrayVar(&repos, "repo", nil, "repo as name[:rw|ro] (repeatable; overrides the project's mode)")
	f.StringVar(&base, "base", "auto", "branch rw repos start from: auto (origin/dev, else develop, else default branch), default|production (origin's default branch), or a branch name on origin (auto, default and production are reserved words, so a branch with one of those names cannot be chosen)")
	f.BoolVar(&o.Backlog, "backlog", false, "create in backlog instead of active")
	f.BoolVar(&o.NoFetch, "no-fetch", false, "do not fetch origin before branching")
	f.StringVarP(&o.Description, "description", "m", "", "task description")
	return cmd
}

// parseBaseFlag maps `atl new --base` to a task base choice: "auto" (or
// empty) → model.BaseAuto, "default"/"production" → model.BaseDefault, any
// other value is a branch name (e.g. "main"). The words are reserved: a
// branch literally named auto, default or production cannot be chosen.
func parseBaseFlag(s string) (string, error) {
	s = strings.TrimSpace(s)
	var b string
	switch s {
	case "", "auto":
		b = model.BaseAuto
	case "default", "production":
		b = model.BaseDefault
	default:
		b = s
	}
	b, err := app.NormalizeBase(b)
	if err != nil {
		return "", usagef("--base: %v", err)
	}
	return b, nil
}

// parseRepoArg parses name[:rw|ro] (mode defaults to rw); label names the
// argument in usage errors.
func parseRepoArg(label, s string) (app.RepoArg, error) {
	name, mode, hasMode := strings.Cut(s, ":")
	ra := app.RepoArg{Name: strings.TrimSpace(name), Mode: model.ModeRW}
	if ra.Name == "" {
		return ra, usagef("%s %q: empty repo name", label, s)
	}
	if hasMode {
		m, err := model.ParseMode(mode)
		if err != nil {
			return ra, usagef("%s %q: %v", label, s, err)
		}
		ra.Mode = m
	}
	return ra, nil
}

func (c *cli) addCmd() *cobra.Command {
	var noFetch, forceRestart bool
	cmd := &cobra.Command{
		Use:   "add <task> <repo>[:rw|ro]... [--no-fetch] [--force-restart]",
		Short: "Add repos to an open task (rw: worktree on atl/<slug>), restarting its sessions; prints slug, repo, mode and path",
		Long: "Add repos to an open task (rw: worktree on atl/<slug>); prints slug, repo, mode and path per repo added.\n\n" +
			"Running sessions cannot see a repo added under them (sandboxed: their container cannot get a new mount;\n" +
			"ro: Claude gets it at launch), so when the task has any, atl closes them, adds the repos and resumes the\n" +
			"same conversations in a new container. While things run in the task (Claude working or waiting for a\n" +
			"permission answer, commands, background processes, shells) it refuses and changes nothing, unless\n" +
			"--force-restart, which ends them.",
		Args: args(cobra.MinimumNArgs(2)),
		RunE: func(cmd *cobra.Command, a []string) error {
			var repos []app.RepoArg
			for _, s := range a[1:] {
				ra, err := parseRepoArg("repo", s)
				if err != nil {
					return err
				}
				repos = append(repos, ra)
			}
			ap, err := c.App()
			if err != nil {
				return err
			}
			res, err := ap.AddRepos(cmd.Context(), a[0], repos, app.AddOpts{NoFetch: noFetch, Force: forceRestart})
			var busy *app.BusyError
			if errors.As(err, &busy) {
				return fmt.Errorf("%w\nwait for them to finish, or pass --force-restart to end them", err)
			}
			var errs []error
			if res != nil {
				for _, ra := range res.Added {
					for _, r := range res.Task.Repos {
						if r.Name == strings.TrimSpace(ra.Name) {
							fmt.Fprintf(c.stdout, "%s\t%s\t%s\t%s\n", res.Task.Slug, r.Name, r.Mode, ap.RepoPathFor(res.Task, r))
						}
					}
				}
				errs = res.Failed
			}
			return errors.Join(append(errs, err)...) // the repos that failed, then the resume's error
		},
	}
	cmd.Flags().BoolVar(&noFetch, "no-fetch", false, "do not fetch origin before branching (rw)")
	cmd.Flags().BoolVar(&forceRestart, "force-restart", false, "restart the task's sessions even while things run in it (ends them)")
	return cmd
}

func (c *cli) reposCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repos",
		Short: "List the git repos in $ATL_REPOS, one per line",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ap, err := c.App()
			if err != nil {
				return err
			}
			names, err := ap.AvailableRepos(cmd.Context())
			if err != nil {
				return err
			}
			for _, n := range names {
				fmt.Fprintln(c.stdout, n)
			}
			return nil
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "pin <repo>...",
		Short: "Pin each repo's origin anew from its config (accept a changed origin URL for atl push/fetch)",
		Args:  args(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			ap, err := c.App()
			if err != nil {
				return err
			}
			for _, name := range a {
				old, had, now, err := ap.RepinOrigin(cmd.Context(), name)
				if err != nil {
					return err
				}
				switch {
				case !had:
					fmt.Fprintf(c.stdout, "%s: pinned %s\n", name, now)
				case old.Equal(now):
					fmt.Fprintf(c.stdout, "%s: pinned %s (unchanged)\n", name, now)
				default:
					fmt.Fprintf(c.stdout, "%s: pinned %s (was %s)\n", name, now, old)
				}
			}
			return nil
		},
	})
	return cmd
}

// parseStatuses parses --status: "" → open statuses, "all" → nil (every
// status), else a comma-separated list.
func parseStatuses(s string) ([]model.Status, error) {
	s = strings.TrimSpace(s)
	switch s {
	case "":
		var open []model.Status
		for _, st := range model.AllStatuses {
			if st.Open() {
				open = append(open, st)
			}
		}
		return open, nil
	case "all":
		return nil, nil
	}
	var out []model.Status
	for _, part := range strings.Split(s, ",") {
		st, err := model.ParseStatus(strings.TrimSpace(part))
		if err != nil {
			return nil, usagef("--status: %v", err)
		}
		out = append(out, st)
	}
	return out, nil
}

type jsonRepo struct {
	Name string `json:"name"`
	Mode string `json:"mode"`
	Path string `json:"path"`
}

type jsonSession struct {
	ID              int64  `json:"id"`
	ClaudeSessionID string `json:"claude_session_id"`
	State           string `json:"state"`
	TmuxTarget      string `json:"tmux_target"`
	LastEventAt     string `json:"last_event_at"`
}

type jsonTask struct {
	ID          int64         `json:"id"`
	Slug        string        `json:"slug"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	Status      string        `json:"status"`
	Project     string        `json:"project"`
	Branch      string        `json:"branch"`
	PortOffset  int           `json:"port_offset"`
	TaskDir     string        `json:"task_dir"`
	Repos       []jsonRepo    `json:"repos"`
	Sessions    []jsonSession `json:"sessions"`
	CreatedAt   string        `json:"created_at"`
	UpdatedAt   string        `json:"updated_at"`
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// printJSON prints v on stdout as indented JSON (the --json outputs).
func (c *cli) printJSON(v any) error {
	enc := json.NewEncoder(c.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func toJSON(ap *app.App, v app.TaskView) jsonTask {
	t := v.Task
	jt := jsonTask{
		ID: t.ID, Slug: t.Slug, Title: t.Title, Description: t.Description, Status: string(t.Status),
		Project: t.Project, Branch: t.Branch, PortOffset: t.PortOffset, TaskDir: ap.Paths.TaskDir(t.Slug),
		Repos: []jsonRepo{}, Sessions: []jsonSession{},
		CreatedAt: rfc3339(t.CreatedAt), UpdatedAt: rfc3339(t.UpdatedAt),
	}
	for _, r := range t.Repos {
		jt.Repos = append(jt.Repos, jsonRepo{Name: r.Name, Mode: string(r.Mode), Path: ap.RepoPathFor(&t, r)})
	}
	for _, s := range v.Sessions {
		jt.Sessions = append(jt.Sessions, jsonSession{ID: s.ID, ClaudeSessionID: s.ClaudeSessionID,
			State: string(s.State), TmuxTarget: s.TmuxTarget, LastEventAt: rfc3339(s.LastEventAt)})
	}
	return jt
}

// sessionsSummary renders e.g. "2 (1 waiting)": the number of sessions that
// have not ended, with waiting and stopped counts; "-" if none.
func sessionsSummary(sessions []model.Session) string {
	n, waiting, stopped := 0, 0, 0
	for _, s := range sessions {
		switch s.State {
		case model.StateEnded:
			continue
		case model.StateWaiting:
			waiting++
		case model.StateStopped:
			stopped++
		}
		n++
	}
	if n == 0 {
		return "-"
	}
	var extra []string
	if waiting > 0 {
		extra = append(extra, fmt.Sprintf("%d waiting", waiting))
	}
	if stopped > 0 {
		extra = append(extra, fmt.Sprintf("%d stopped", stopped))
	}
	if len(extra) == 0 {
		return fmt.Sprint(n)
	}
	return fmt.Sprintf("%d (%s)", n, strings.Join(extra, ", "))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (c *cli) lsCmd() *cobra.Command {
	var status string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ls [--status S[,S...]|all] [--json]",
		Short: "List tasks (default: backlog, active, review)",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			statuses, err := parseStatuses(status)
			if err != nil {
				return err
			}
			ap, err := c.App()
			if err != nil {
				return err
			}
			views, err := ap.List(cmd.Context(), statuses...)
			if err != nil {
				return err
			}
			if asJSON {
				out := make([]jsonTask, 0, len(views))
				for _, v := range views {
					out = append(out, toJSON(ap, v))
				}
				return c.printJSON(out)
			}
			tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSLUG\tSTATUS\tPROJECT\tSESSIONS\tTITLE")
			for _, v := range views {
				t := v.Task
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Slug, t.Status, orDash(t.Project),
					sessionsSummary(v.Sessions), sanitize(t.Title))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&status, "status", "", "comma-separated statuses, or all")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func (c *cli) openCmd() *cobra.Command {
	var sessionID int64
	var newSession bool
	cmd := &cobra.Command{
		Use:   "open <task> [--session ID | --new]",
		Short: "Ensure the task's tmux session (resume or start Claude) and attach to it",
		Long: "Ensure the task's tmux session and attach to it.\n\n" +
			"Without flags: attach if a Claude window is live, else resume the task's stopped sessions,\n" +
			"else start a new session. --session ID opens that session of the task (one with a live\n" +
			"window is selected, an ended or stopped one is resumed with claude --resume if it can\n" +
			"be found from the task dir); --new starts a new session. Either way the window is\n" +
			"selected before attaching.",
		Args: args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			hasSession := cmd.Flags().Changed("session")
			if hasSession && newSession {
				return usagef("--session and --new are mutually exclusive")
			}
			if hasSession && sessionID <= 0 {
				return usagef("--session must be a positive session id")
			}
			ap, err := c.App()
			if err != nil {
				return err
			}
			var attach *exec.Cmd
			switch {
			case hasSession:
				attach, err = ap.OpenSession(cmd.Context(), a[0], sessionID)
			case newSession:
				attach, err = ap.OpenNew(cmd.Context(), a[0])
			default:
				attach, err = ap.Open(cmd.Context(), a[0])
			}
			if err != nil {
				return err
			}
			return c.attach(attach)
		},
	}
	cmd.Flags().Int64Var(&sessionID, "session", 0, "open this session (id from atl show): attach to it if live, else resume it")
	cmd.Flags().BoolVar(&newSession, "new", false, "start a new Claude session and attach to it")
	return cmd
}

func (c *cli) assistantCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "assistant [task]",
		Short: "Open the atelier assistant (one Claude session for managing all tasks); with a task, ask it about that task",
		Args:  args(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			ref := ""
			if len(a) == 1 {
				if ref = strings.TrimSpace(a[0]); ref == "" {
					return usagef("empty task")
				}
			}
			ap, err := c.App()
			if err != nil {
				return err
			}
			attach, hint, err := ap.Assistant(cmd.Context(), ref)
			if err != nil {
				return err
			}
			if hint != "" {
				fmt.Fprintln(c.stderr, hint)
			}
			return c.attach(attach)
		},
	}
}

// attach runs a tmux attach command on the terminal.
func (c *cli) attach(attach *exec.Cmd) error {
	if err := c.runOnTerminal(attach); err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	return nil
}

// runOnTerminal runs cmd with the terminal's stdin and atl's stdout and
// stderr.
func (c *cli) runOnTerminal(cmd *exec.Cmd) error {
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, c.stdout, c.stderr
	return cmd.Run()
}

func (c *cli) sessionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "session <task>",
		Short: "Start a new Claude session in the task (does not attach); prints window name and id",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			ap, err := c.App()
			if err != nil {
				return err
			}
			s, err := ap.NewSession(cmd.Context(), a[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(c.stdout, "%s\t%s\n", s.Name, s.TmuxTarget)
			return nil
		},
	}
}

func (c *cli) resumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume <task>",
		Short: "Recreate the task's tmux session and resume its stopped Claude sessions; prints the count",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			ap, err := c.App()
			if err != nil {
				return err
			}
			n, err := ap.Resume(cmd.Context(), a[0])
			fmt.Fprintln(c.stdout, n)
			return err
		},
	}
}

func (c *cli) statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <task> <backlog|active|review|done|cancelled>",
		Short: "Set a task's status; a done or cancelled task set to an open status is reopened (task dir, worktrees, CLAUDE.md, setup)",
		Args:  args(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, a []string) error {
			st, err := model.ParseStatus(a[1])
			if err != nil {
				return usageError{err}
			}
			ap, err := c.App()
			if err != nil {
				return err
			}
			if c.forAssistant {
				return ap.SetOpenStatus(cmd.Context(), a[0], st)
			}
			return ap.SetStatus(cmd.Context(), a[0], st)
		},
	}
}

func (c *cli) renameCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rename <task> <new title...>",
		Short: "Change a task's title (slug, branch, tmux session and dirs keep the original name); prints the new title",
		Args:  args(cobra.MinimumNArgs(2)),
		RunE: func(cmd *cobra.Command, a []string) error {
			title, err := model.NormalizeTitle(strings.Join(a[1:], " "))
			if err != nil {
				return usageError{err}
			}
			ap, err := c.App()
			if err != nil {
				return err
			}
			t, err := ap.Rename(cmd.Context(), a[0], title)
			if err != nil {
				return err
			}
			fmt.Fprintln(c.stdout, t.Title)
			return nil
		},
	}
}

func (c *cli) diffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diff <task>",
		Short: "Show git diff of each rw repo against its base commit (paged)",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			ap, err := c.App()
			if err != nil {
				return err
			}
			return c.paged(func(w io.Writer, color bool) error {
				return ap.Diff(cmd.Context(), a[0], w, color)
			})
		},
	}
}

// notTTYDevices are character devices that are never terminals.
var notTTYDevices = []string{"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom"}

// isTerminal reports whether v (an io.Reader or io.Writer) is a terminal:
// an *os.File on a character device other than /dev/null and the like
// (an ioctl-free check: no terminal library is a dependency).
func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	for _, p := range notTTYDevices {
		if di, err := os.Stat(p); err == nil && os.SameFile(fi, di) {
			return false
		}
	}
	return true
}

// paged runs write with stdout piped through $PAGER (default `less -R`)
// when stdout is a TTY, with colour; otherwise writes directly without.
func (c *cli) paged(write func(w io.Writer, color bool) error) error {
	if !isTerminal(c.stdout) {
		return write(c.stdout, false)
	}
	pager := strings.Fields(os.Getenv("PAGER"))
	if len(pager) == 0 {
		pager = []string{"less", "-R"}
	}
	cmd := exec.Command(pager[0], pager[1:]...)
	cmd.Stdout, cmd.Stderr = c.stdout, c.stderr
	if os.Getenv("LESS") == "" {
		cmd.Env = append(os.Environ(), "LESS=FRX")
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		in.Close()
		return write(c.stdout, true) // no usable pager: print directly
	}
	werr := write(in, true)
	in.Close()
	perr := cmd.Wait()
	// The pager quitting early (e.g. `q` in less) closes the pipe; that is
	// not an error if the pager itself exited successfully.
	if errors.Is(werr, syscall.EPIPE) {
		werr = nil
	}
	if perr != nil && werr == nil {
		werr = fmt.Errorf("pager %s: %w", pager[0], perr)
	}
	return werr
}

func (c *cli) prCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pr <task>",
		Short: "Create a GitHub PR (gh) per rw repo with pushed commits; status → review",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			ap, err := c.App()
			if err != nil {
				return err
			}
			pr := ap.PR
			if c.forAssistant {
				pr = ap.PROpen
			}
			res, err := pr(cmd.Context(), a[0])
			ghFailed := false
			for _, r := range res {
				fmt.Fprintf(c.stdout, "%s\t%s\n", r.Repo, r)
				if strings.HasPrefix(r.Reason, "gh:") {
					ghFailed = true
				}
			}
			if err == nil && ghFailed {
				// The lines above say why; skips (no commits, not pushed) are not failures.
				err = exitCodeError{exitErr}
			}
			return err
		},
	}
}

func (c *cli) finishCmd(verb string) *cobra.Command {
	var force bool
	short := "Finish a task: kill tmux, remove worktrees and the branches with nothing unpushed, status done"
	if verb == "cancel" {
		short = "Cancel a task: kill tmux, remove worktrees and the branches with nothing unpushed, status cancelled"
	}
	cmd := &cobra.Command{
		Use:   verb + " <task> [--force]",
		Short: short,
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			ap, err := c.App()
			if err != nil {
				return err
			}
			finish := ap.Done
			if verb == "cancel" {
				finish = ap.Cancel
			}
			// Closing kills the task's tmux session: run from a window
			// of it, this process gets its SIGHUP and would die with the
			// task half closed (worktrees left, status still open).
			// Caught, not ignored: an ignored signal stays ignored in
			// what atl starts (teardown and its background processes).
			hup := make(chan os.Signal, 1)
			signal.Notify(hup, syscall.SIGHUP)
			defer signal.Stop(hup)
			t, err := finish(cmd.Context(), a[0], force)
			if err != nil {
				return err
			}
			fmt.Fprintf(c.stdout, "%s\t%s\n", t.Slug, t.Status)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "discard uncommitted changes and continue past errors")
	return cmd
}

func (c *cli) doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check git, tmux, claude, gh, directories, the database and the sandbox",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ap, err := c.doctorApp()
			if err != nil {
				return err
			}
			failed := false
			tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
			for _, ch := range ap.Doctor(cmd.Context()) {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", ch.Level(), ch.Name, ch.Detail)
				failed = failed || !ch.OK
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if failed {
				return exitCodeError{exitErr}
			}
			return nil
		},
	}
}

// statuslineCmd is Claude Code's status line command in task sessions
// (main.go runs it before cobra; this keeps `atl statusline` a known command).
func (c *cli) statuslineCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "statusline",
		Short:              "Claude Code status line: the task title (internal)",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(*cobra.Command, []string) error {
			hook.StatusLineMain()
			return nil
		},
	}
}

// gateCmd serves a task's gated Docker socket (see sandbox.Gate) and its
// control socket (sandbox.Control): push and fetch, and the hooks, status
// line and notes of the task's container, which it answers itself from the
// database (hook.TaskServer) for this one task only, and the copy relay
// (sandbox.CopyMain) to the terminal on this task's tmux session. atl starts it itself,
// detached, when it launches a session in a sandboxed task.
func (c *cli) gateCmd() *cobra.Command {
	var taskID int64
	var roMounts []string
	var assistant bool
	cmd := &cobra.Command{
		Use:    "gate <slug> [--ro-mount PATH]... --task-id ID | gate --assistant",
		Short:  "Serve a sandboxed task's (or the assistant's) Docker socket and atl requests (internal)",
		Hidden: true,
		Args:   args(cobra.RangeArgs(0, 1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			// atl starts it with the workspace applied ($ATL_WORKSPACE a
			// path); a name works too.
			if err := c.applyWorkspace(); err != nil {
				return err
			}
			p, err := config.Resolve()
			if err != nil {
				return err
			}
			if assistant {
				if len(a) != 0 || taskID != 0 || len(roMounts) != 0 {
					return usagef("--assistant takes no slug, --task-id or --ro-mount")
				}
				return c.assistantGate(cmd.Context(), p)
			}
			if len(a) != 1 {
				return usagef("gate needs a task slug (or --assistant)")
			}
			slug := a[0]
			if slug == "" || strings.ContainsAny(slug, `/\`) || slug == "." || slug == ".." {
				return usagef("invalid task slug %q", slug)
			}
			if taskID <= 0 {
				return usagef("--task-id is required")
			}
			// atl checked them (sandbox.ROMounts); the same rules again.
			for _, m := range roMounts {
				if err := sandbox.CheckROMount(p, m); err != nil {
					return fmt.Errorf("--ro-mount: %w", err)
				}
			}
			return c.taskGate(cmd.Context(), p, slug, taskID, roMounts)
		},
	}
	cmd.Flags().Int64Var(&taskID, "task-id", 0, "the task's database id (must still have this slug)")
	cmd.Flags().StringArrayVar(&roMounts, "ro-mount", nil, "a host path of $"+sandbox.MountsVar+" (resolved) the task may bind-mount, read-only only")
	cmd.Flags().BoolVar(&assistant, "assistant", false, "serve the assistant's container of the workspace instead of a task's")
	return cmd
}

// taskGate is `atl gate <slug> --task-id ID`: the gate of task slug, whose
// database id is taskID, with the read-only mount sources roMounts (both
// checked by the caller). It stops once the task is gone.
func (c *cli) taskGate(ctx context.Context, p config.Paths, slug string, taskID int64, roMounts []string) error {
	dir := p.TaskDir(slug)
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("task dir: %w", err)
	}
	runDir := p.RunDir(slug)
	id := sandbox.ID(slug, dir)
	bin, err := atlBin()
	if err != nil {
		return err
	}
	// The task by database id, checked against the slug once: a
	// request never names a task.
	ts, err := hook.OpenTaskServer(p, taskID, slug)
	if err != nil {
		return err
	}
	defer ts.Close()
	g := &sandbox.Gate{
		ID:         id,
		Root:       root,
		SocketHost: sandbox.SocketPath(runDir),
		Agent:      sandbox.ContainerName(id),
		Upstream:   sandbox.UpstreamSocket(),
		Log:        sandbox.NewLogger(),
		Verbose:    os.Getenv("ATL_GATE_DEBUG") != "",
		Ident:      sandbox.Ident(taskID, bin, roMounts),
		ROMounts:   roMounts,
		Reserved:   reservedImages(p, os.Getenv("ATL_SANDBOX_IMAGE"), os.Getenv("ATL_BOX_IMAGE")),
	}
	ts.SetROMounts(roMounts)
	// A session ended (Claude exits): what it left running in the
	// container goes once Claude is gone.
	ts.OnSessionEnd(func(session int64) {
		go func() {
			if err := g.EndSession(session); err != nil {
				g.Log.Printf("note  end what session %d left running: %v", session, err)
			}
		}()
	})
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { // a gate outliving its task stops
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if gone, _ := ts.Gone(ctx); gone {
					g.Log.Printf("task %d (%s) is gone; stopping", taskID, slug)
					cancel()
					return
				}
			}
		}
	}()
	ctl := &sandbox.Control{Slug: slug, TaskID: taskID, Atl: bin, Task: ts, Log: g.Log}
	// The copy relay (wl-copy in the container): to the terminal
	// attached to this task's tmux session, on atl's tmux server.
	tc, session := tmux.New(), tmux.SessionName(slug)
	ctl.Copy = func(ctx context.Context, text []byte) error { return tc.Copy(ctx, session, text) }
	return sandbox.Serve(ctx, g, ctl, runDir)
}

// reservedImages is a task gate's sandbox.Gate.Reserved: the image names
// atl runs its own containers from, besides the default task image and
// box image. The task image and box image the gate was started with (its
// environment is atl's, the workspace applied) are fixed; the workspace's
// sandbox.image and every project's image are read again on each call, so
// a name the user sets while the gate runs is reserved at once. A file
// that cannot be read is an error: the gate then refuses.
func reservedImages(p config.Paths, startImages ...string) func() ([]string, error) {
	return func() ([]string, error) {
		names := append([]string(nil), startImages...)
		if p.Workspace != "" {
			c, err := workspace.ReadConfig(p.Workspace)
			if err != nil {
				return nil, err
			}
			if c.Sandbox != nil {
				names = append(names, c.Sandbox.Image)
			}
		}
		projects, err := project.List(p)
		if err != nil {
			return nil, err
		}
		for _, n := range projects {
			pr, err := project.Load(p, n)
			if err != nil {
				return nil, err
			}
			names = append(names, pr.Image)
		}
		return names, nil
	}
}

// atlBin is the atl binary a gate runs push, fetch and the assistant's
// commands with, and the one atl re-executes: $ATL_BIN, else the running one.
func atlBin() (string, error) {
	if bin := os.Getenv("ATL_BIN"); bin != "" {
		return bin, nil
	}
	bin, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate atl binary (set $ATL_BIN): %w", err)
	}
	return bin, nil
}

// assistantGate is `atl gate --assistant`: the gate of the workspace's
// assistant container. Its Docker socket refuses everything (the assistant
// has no Docker access); its control socket answers the assistant's guard
// hook (hook.AssistantServer), runs the atl commands the assistant may use
// in this workspace (sandbox.OpAtl, see sandbox.CheckAssistantArgs) and
// relays copies to the terminal on the assistant's tmux session. Whether it
// serves the assistant is fixed here, when it starts: a task's gate never
// runs atl commands.
func (c *cli) assistantGate(ctx context.Context, p config.Paths) error {
	if p.Workspace == "" {
		return errors.New("the assistant's gate needs a workspace")
	}
	dir := p.AssistantDir()
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("assistant dir: %w", err)
	}
	bin, err := atlBin()
	if err != nil {
		return err
	}
	runDir := p.AssistantRunDir()
	id := sandbox.AssistantID(dir)
	g := &sandbox.Gate{
		ID:         id,
		Root:       root,
		SocketHost: sandbox.SocketPath(runDir),
		Log:        sandbox.NewLogger(),
		Verbose:    os.Getenv("ATL_GATE_DEBUG") != "",
		Ident:      sandbox.AssistantIdent(p.Workspace, bin),
		NoDocker:   true,
	}
	ctl := &sandbox.Control{Slug: app.AssistantSession, Atl: bin, Task: hook.NewAssistantServer(p), Log: g.Log, Resolve: resolveAtl,
		Workspace: p.Workspace}
	tc := tmux.New()
	ctl.Copy = func(ctx context.Context, text []byte) error { return tc.Copy(ctx, app.AssistantSession, text) }
	return sandbox.Serve(ctx, g, ctl, runDir)
}

// remoteCmd is `atl push` (push true) or `atl fetch`. Inside a task
// container it opens nothing: it asks the task's gate (see sandbox.Control),
// which runs the same command where atl runs, with its git credentials.
func (c *cli) remoteCmd(push bool) *cobra.Command {
	var repo string
	var force bool
	var taskID int64
	op, short := sandbox.OpFetch, "git fetch origin in each repo of the task (rw and ro), or only --repo"
	use := "fetch [task] [--repo NAME]"
	if push {
		op, short = sandbox.OpPush, "Push the task branch (atl/<slug>) of each rw repo, or only --repo, to origin"
		use = "push [task] [--repo NAME] [--force]"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Long: short + ".\n\n" +
			"task defaults to $ATL_TASK (set in every task session). Inside a sandboxed task's container,\n" +
			"which has no git credentials, the task's gate runs it where atl runs.",
		Args: args(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			ref := os.Getenv("ATL_TASK")
			if len(a) == 1 {
				ref = strings.TrimSpace(a[0])
			}
			if ref == "" {
				return usagef("which task? (no task argument and no $ATL_TASK)")
			}
			repo = strings.TrimSpace(repo)
			if sandbox.Inside() {
				if taskID != 0 {
					return usagef("--task-id is for the task's gate, not for a task container")
				}
				return c.viaGate(sandbox.ControlRequest{Op: op, Repo: repo, Force: force}, ref, nil, 0)
			}
			ap, err := c.App()
			if err != nil {
				return err
			}
			var res []app.RemoteResult
			if push {
				res, err = ap.Push(cmd.Context(), ref, app.PushOpts{Repo: repo, Force: force, TaskID: taskID}, c.stderr)
			} else {
				res, err = ap.Fetch(cmd.Context(), ref, app.FetchOpts{Repo: repo, TaskID: taskID}, c.stderr)
			}
			failed := false
			for _, r := range res {
				status := "pushed"
				if !push {
					status = "fetched"
				}
				if r.Err != nil {
					status, failed = "failed: "+r.Err.Error(), true
				}
				fmt.Fprintf(c.stdout, "%s\t%s\n", r.Repo, status)
			}
			if err == nil && failed {
				err = exitCodeError{exitErr} // the lines above say why
			}
			return err
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "only this repo of the task")
	if push {
		cmd.Flags().BoolVar(&force, "force", false, "push with --force-with-lease (after a rebase)")
	}
	// The gate names its task by id and slug, so that nothing else (a
	// prefix, a slug that is all digits, a task created after its own was
	// deleted) can be taken for it.
	cmd.Flags().Int64Var(&taskID, "task-id", 0, "only the task with this id, which must have the exact slug given (internal)")
	cmd.Flags().MarkHidden("task-id")
	return cmd
}

// ownTask checks, in a task container, that ref is the container's own task
// ($ATL_TASK): the only one atl op can work on there.
func ownTask(op, ref string) error {
	switch self := os.Getenv("ATL_TASK"); {
	case self == "":
		return fmt.Errorf("inside a task container atl %s needs $ATL_TASK (it works on the container's own task only)", op)
	case ref != self:
		return usagef("inside a task container atl %s works on this task (%s) only", op, self)
	}
	return nil
}

// viaGateTimeout bounds note and notes through the gate (push and fetch
// may wait for the user: they have the gate's own limit).
const viaGateTimeout = 30 * time.Second

// viaGate sends req (with body) to the task's gate from inside its container
// and relays the output and exit status; timeout 0 waits as long as the gate
// does. The gate acts on its own task only, so a task named on the command
// line must be this one. push and fetch get all output on stdout, as before;
// note and notes keep stdout and stderr apart.
func (c *cli) viaGate(req sandbox.ControlRequest, ref string, body []byte, timeout time.Duration) error {
	if err := ownTask(req.Op, ref); err != nil {
		return err
	}
	stderr := c.stderr
	if req.Op == sandbox.OpPush || req.Op == sandbox.OpFetch {
		stderr = c.stdout
	}
	code, _, err := sandbox.Ask(os.Getenv("ATL_CONTROL_SOCK"), req, body, c.stdout, stderr, timeout)
	if err != nil {
		return err
	}
	if code != 0 {
		return exitCodeError{code}
	}
	return nil
}

func (c *cli) hookCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "hook <event>",
		Short:              "Claude Code hook handler (internal)",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, a []string) error {
			if code := hook.Main(a); code != 0 {
				return exitCodeError{code}
			}
			return nil
		},
	}
}

// resolveAtl reads argv, an atl command line without the binary, as atl
// run with it will (runIn: cobra's ExecuteC, which sets up the help and
// completion commands and then Finds the command): it returns the
// CommandPath of the command that runs ("atl notes"), and whether that
// reads stdin (notes with --set, and not --help). The assistant's gate
// checks both before it runs anything (sandbox.Control.Resolve).
func resolveAtl(argv []string) (path string, readsStdin bool, err error) {
	c := &cli{stdout: io.Discard, stderr: io.Discard}
	root := c.rootCmd()
	root.SetArgs(argv)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd(argv...)
	cmd, flags, err := root.Find(argv)
	if err != nil {
		return "", false, err
	}
	if cmd.Flags().Lookup("set") != nil {
		cmd.InitDefaultHelpFlag()
		if err := cmd.ParseFlags(flags); err != nil {
			return "", false, err
		}
		set, _ := cmd.Flags().GetBool("set")
		help, _ := cmd.Flags().GetBool("help")
		readsStdin = set && !help
	}
	return cmd.CommandPath(), readsStdin, nil
}
