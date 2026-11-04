package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/andreujuanc/atelier/internal/app"
	"github.com/andreujuanc/atelier/internal/hook"
	"github.com/andreujuanc/atelier/internal/model"
)

func (c *cli) newCmd() *cobra.Command {
	var o app.NewTaskOpts
	var repos []string
	cmd := &cobra.Command{
		Use:   "new <title...> [--project P] [--repo name:rw|ro ...] [--backlog] [--no-fetch] [-m description]",
		Short: "Create a task with worktrees on branch atl/<slug>; prints slug and task dir",
		Args:  args(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			o.Title = strings.Join(a, " ")
			if strings.TrimSpace(o.Title) == "" {
				return usagef("title is required")
			}
			for _, r := range repos {
				ra, err := parseRepoArg("--repo", r)
				if err != nil {
					return err
				}
				o.Repos = append(o.Repos, ra)
			}
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
	f.BoolVar(&o.Backlog, "backlog", false, "create in backlog instead of active")
	f.BoolVar(&o.NoFetch, "no-fetch", false, "do not fetch origin before branching")
	f.StringVarP(&o.Description, "description", "m", "", "task description")
	return cmd
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
	var noFetch bool
	cmd := &cobra.Command{
		Use:   "add <task> <repo>[:rw|ro] [--no-fetch]",
		Short: "Add a repo to an open task (rw: worktree on atl/<slug>); prints slug, repo, mode and path",
		Args:  args(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, a []string) error {
			ra, err := parseRepoArg("repo", a[1])
			if err != nil {
				return err
			}
			ap, err := c.App()
			if err != nil {
				return err
			}
			t, err := ap.AddRepo(cmd.Context(), a[0], ra, noFetch)
			if err != nil {
				return err
			}
			for _, r := range t.Repos {
				if r.Name == strings.TrimSpace(ra.Name) {
					fmt.Fprintf(c.stdout, "%s\t%s\t%s\t%s\n", t.Slug, r.Name, r.Mode, ap.RepoPathFor(t, r))
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noFetch, "no-fetch", false, "do not fetch origin before branching (rw)")
	return cmd
}

func (c *cli) reposCmd() *cobra.Command {
	return &cobra.Command{
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
				enc := json.NewEncoder(c.stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}
			tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSLUG\tSTATUS\tPROJECT\tSESSIONS\tTITLE")
			for _, v := range views {
				t := v.Task
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Slug, t.Status, orDash(t.Project),
					sessionsSummary(v.Sessions), t.Title)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&status, "status", "", "comma-separated statuses, or all")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func (c *cli) openCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "open <task>",
		Short: "Ensure the task's tmux session (resume or start Claude) and attach to it",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			ap, err := c.App()
			if err != nil {
				return err
			}
			attach, err := ap.Open(cmd.Context(), a[0])
			if err != nil {
				return err
			}
			attach.Stdin, attach.Stdout, attach.Stderr = os.Stdin, c.stdout, c.stderr
			if err := attach.Run(); err != nil {
				return fmt.Errorf("attach: %w", err)
			}
			return nil
		},
	}
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
			attach.Stdin, attach.Stdout, attach.Stderr = os.Stdin, c.stdout, c.stderr
			if err := attach.Run(); err != nil {
				return fmt.Errorf("attach: %w", err)
			}
			return nil
		},
	}
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
		Short: "Set a task's status (no other side effects)",
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
			return ap.SetStatus(cmd.Context(), a[0], st)
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

// isTerminal reports whether w is a character device (a TTY).
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
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
			res, err := ap.PR(cmd.Context(), a[0])
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
	short := "Finish a task: kill tmux, remove worktrees (branches kept), status done"
	if verb == "cancel" {
		short = "Cancel a task: kill tmux, remove worktrees (branches kept), status cancelled"
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
		Short: "Check git, tmux, claude, gh, directories and the database",
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
