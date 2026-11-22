package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/andreujuanc/atelier/internal/app"
	"github.com/andreujuanc/atelier/internal/tui"
	"github.com/andreujuanc/atelier/internal/workspace"
)

// workspaceCmd is `atl workspace list|new|open`. None of them needs a
// selected workspace.
func (c *cli) workspaceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workspace",
		Short: "List, create (or adopt) and open workspaces under $" + workspace.RootVar,
		Args:  args(cobra.NoArgs),
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(c.workspaceListCmd(), c.workspaceNewCmd(), c.workspaceOpenCmd())
	return cmd
}

type jsonWorkspace struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Workspace bool   `json:"workspace"` // false: no atelier.toml
	Current   bool   `json:"current"`
	OpenTasks int    `json:"open_tasks"`
	Waiting   int    `json:"waiting_sessions"`
	Error     string `json:"error,omitempty"` // its database could not be read
}

func (c *cli) workspaceListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list [--json]",
		Short: "Workspaces under $" + workspace.RootVar + ": name, open tasks, waiting sessions, path (* the current one)",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			root, err := workspace.Root()
			if err != nil {
				return err
			}
			entries, err := app.ListWorkspaces(cmd.Context(), root)
			if err != nil {
				return err
			}
			current := ""
			if sel, err := workspace.Select(c.wsFlag); err == nil && sel != nil {
				current = filepath.Clean(sel.Dir)
			}
			out := make([]jsonWorkspace, 0, len(entries))
			for _, e := range entries {
				w := jsonWorkspace{Name: e.Name, Path: e.Dir, Workspace: e.Workspace, Current: filepath.Clean(e.Dir) == current,
					OpenTasks: e.OpenTasks, Waiting: e.Waiting}
				if e.Err != nil {
					w.Error = e.Err.Error()
				}
				out = append(out, w)
			}
			if asJSON {
				enc := json.NewEncoder(c.stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}
			if len(out) == 0 {
				fmt.Fprintf(c.stderr, "atl: no workspaces in %s: create one with `atl workspace new <name>`\n", root)
				return nil
			}
			tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
			for _, w := range out {
				mark := " "
				if w.Current {
					mark = "*"
				}
				var what string
				switch {
				case !w.Workspace:
					what = "not a workspace (no " + workspace.File + ")"
				case w.Error != "":
					what = "database: " + w.Error
				default:
					what = fmt.Sprintf("%d open", w.OpenTasks)
					if w.Waiting > 0 {
						what += fmt.Sprintf(", %d waiting", w.Waiting)
					}
				}
				fmt.Fprintf(tw, "%s %s\t%s\t%s\n", mark, w.Name, what, w.Path)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	return cmd
}

func (c *cli) workspaceNewCmd() *cobra.Command {
	var cfg workspace.Config
	var mounts []string
	var image string
	cmd := &cobra.Command{
		Use: "new <name> [--repos DIR] [--mount PATH]... [--image IMAGE]",
		Short: "Create workspace <name> (atelier.toml, tasks/ data/ config/ state/), or adopt the directory if it has no atelier.toml; " +
			"settings not given are taken from $ATL_REPOS, $ATL_SANDBOX_MOUNTS, $ATL_SANDBOX_IMAGE, $ATL_SANDBOX_CONTEXT and $ATL_CONTEXT",
		Args: args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			name := a[0]
			if err := workspace.CheckName(name); err != nil {
				return usageError{err}
			}
			root, err := workspace.Root()
			if err != nil {
				return err
			}
			var ms []string
			for _, m := range mounts {
				if m = strings.TrimSpace(m); m == "" {
					return usagef("--mount: empty path")
				}
				ms = append(ms, m)
			}
			cfg.Repos = strings.TrimSpace(cfg.Repos)
			cfg.Sandbox = &workspace.SandboxConfig{Image: strings.TrimSpace(image), Mounts: ms}
			dir, notes, err := createWorkspace(root, name, cfg)
			if err != nil {
				return err
			}
			for _, n := range notes {
				fmt.Fprintf(c.stderr, "atl: note: %s\n", n)
			}
			fmt.Fprintf(c.stdout, "%s\t%s\n", name, dir)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&cfg.Repos, "repos", "", "the canonical clones (default: $ATL_REPOS if set, else <workspace>/repos; needed to adopt a box's workspace, one with a database, outside that box)")
	f.StringArrayVar(&mounts, "mount", nil, "a host path every task gets read-only (repeatable; default: $ATL_SANDBOX_MOUNTS)")
	f.StringVar(&image, "image", "", "the task container image (default: $ATL_SANDBOX_IMAGE, else atelier-task)")
	return cmd
}

func (c *cli) workspaceOpenCmd() *cobra.Command {
	var preselect string
	cmd := &cobra.Command{
		Use: "open [<name>]",
		Short: "The TUI in workspace <name> (same as atl --workspace <name>); without a name, the workspace selector " +
			"(whatever $" + workspace.Var + " and the current directory say)",
		Args: args(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			terminal := isTerminal(os.Stdin) && isTerminal(c.stdout)
			if len(a) == 0 && c.wsFlag == "" {
				if !terminal {
					return fmt.Errorf("atl workspace open without a name shows the workspace selector, which needs a terminal; name the workspace: atl workspace open <name>")
				}
				return c.selectWorkspace(cmd.Context(), preselect)
			}
			if len(a) == 1 {
				if c.wsFlag != "" && c.wsFlag != a[0] {
					return usagef("both --workspace %s and workspace %s given", c.wsFlag, a[0])
				}
				c.wsFlag = a[0]
			}
			if err := c.applyWorkspace(); err != nil {
				return err
			}
			if !terminal {
				return fmt.Errorf("atl workspace open needs a terminal")
			}
			return c.runTUI(cmd.Context())
		},
	}
	// What the TUI's switch key (w) passes: the workspace it leaves.
	cmd.Flags().StringVar(&preselect, "preselect", "", "the workspace the selector starts on")
	_ = cmd.Flags().MarkHidden("preselect")
	return cmd
}

// createWorkspace is `atl workspace new`: cfg with what it does not set
// taken from the environment, then created (or adopted) under root.
func createWorkspace(root, name string, cfg workspace.Config) (string, []string, error) {
	workspace.FillFromEnv(&cfg)
	return workspace.Create(root, name, cfg)
}

// execProcess replaces this process with another program; tests replace it.
var execProcess = syscall.Exec

// reexec replaces this process with atl args in atl's environment as it
// was before a workspace was applied (workspace.OriginalEnv), so that
// nothing of this workspace leaks into the next one. It returns only on
// failure.
func (c *cli) reexec(args ...string) error {
	bin := os.Getenv("ATL_BIN")
	if bin == "" {
		var err error
		if bin, err = os.Executable(); err != nil {
			return fmt.Errorf("locate atl binary (set $ATL_BIN): %w", err)
		}
	}
	if c.app != nil {
		c.app.Close()
		c.app = nil
	}
	if err := execProcess(bin, append([]string{bin}, args...), workspace.OriginalEnv()); err != nil {
		return fmt.Errorf("run %s %s: %w", bin, strings.Join(args, " "), err)
	}
	return nil
}

// runTUI runs the task TUI in the selected workspace, recording it as the
// last one opened (the selector preselects it). When the user leaves it
// for the selector (w), atl re-executes itself as `atl workspace open`.
func (c *cli) runTUI(ctx context.Context) error {
	ap, err := c.openApp()
	if err != nil {
		return err
	}
	current := ""
	if root, err := workspace.Root(); err == nil && c.ws != nil && c.ws.Workspace != nil && filepath.Dir(c.ws.Dir) == root {
		current = c.ws.Name
		_ = workspace.SaveLast(root, current) // only a convenience
	}
	err = tui.RunContext(ctx, ap)
	if !errors.Is(err, tui.ErrSwitchWorkspace) {
		return err
	}
	args := []string{"workspace", "open"}
	if current != "" {
		args = append(args, "--preselect", current)
	}
	return c.reexec(args...)
}

// selectWorkspace shows the workspace selector (on preselect, else the
// last workspace opened) and re-executes atl as `atl -w <name>` in the one
// chosen; nothing when the user quits.
func (c *cli) selectWorkspace(ctx context.Context, preselect string) error {
	root, err := workspace.Root()
	if err != nil {
		return err
	}
	if preselect == "" {
		preselect = workspace.Last(root)
	}
	home, _ := os.UserHomeDir()
	name, err := tui.RunWorkspaces(ctx, tui.WorkspaceSelector{
		Root:      root,
		Home:      home,
		Preselect: preselect,
		Load:      func(ctx context.Context) ([]app.WorkspaceInfo, error) { return app.ListWorkspaces(ctx, root) },
		Create: func(name string) ([]string, error) {
			if err := selectorAdoptable(root, name); err != nil {
				return nil, err
			}
			_, notes, err := createWorkspace(root, name, workspace.Config{})
			return notes, err
		},
	})
	if err != nil || name == "" {
		return err
	}
	return c.openChosen(name)
}

// selectorAdoptable returns why the selector does not create or adopt
// workspace name: a directory without atelier.toml that has a database (a
// box's workspace) is adopted only by `atl workspace new` with its repos
// (the selector has no flags), even in that box.
func selectorAdoptable(root, name string) error {
	dir := filepath.Join(root, name)
	if _, err := os.Lstat(filepath.Join(dir, workspace.File)); errors.Is(err, os.ErrNotExist) && workspace.HasDB(dir) {
		return &workspace.AdoptDBError{Name: name, Dir: dir}
	}
	return nil
}

// openChosen re-executes atl in workspace name (the selector's choice).
func (c *cli) openChosen(name string) error { return c.reexec("--workspace", name) }
