package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
			entries, err := workspace.List(root)
			if err != nil {
				return err
			}
			current := ""
			if sel, err := workspace.Select(c.wsFlag); err == nil && sel != nil {
				current = filepath.Clean(sel.Dir)
			}
			out := make([]jsonWorkspace, 0, len(entries))
			for _, e := range entries {
				w := jsonWorkspace{Name: e.Name, Path: e.Dir, Workspace: e.Workspace, Current: filepath.Clean(e.Dir) == current}
				if e.Workspace {
					if w.OpenTasks, w.Waiting, err = app.WorkspaceCounts(cmd.Context(), e.Dir); err != nil {
						w.Error = err.Error()
					}
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
			workspace.FillFromEnv(&cfg)
			dir, notes, err := workspace.Create(root, name, cfg)
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
	f.StringVar(&cfg.Repos, "repos", "", "the canonical clones (default: $ATL_REPOS if set, else <workspace>/repos)")
	f.StringArrayVar(&mounts, "mount", nil, "a host path every task gets read-only (repeatable; default: $ATL_SANDBOX_MOUNTS)")
	f.StringVar(&image, "image", "", "the task container image (default: $ATL_SANDBOX_IMAGE, else atelier-task)")
	return cmd
}

func (c *cli) workspaceOpenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "open <name>",
		Short: "The TUI in workspace <name> (same as atl --workspace <name>)",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			if c.wsFlag != "" && c.wsFlag != a[0] {
				return usagef("both --workspace %s and workspace %s given", c.wsFlag, a[0])
			}
			c.wsFlag = a[0]
			if err := c.applyWorkspace(); err != nil {
				return err
			}
			if !isTerminal(os.Stdin) || !isTerminal(c.stdout) {
				return fmt.Errorf("atl workspace open needs a terminal")
			}
			ap, err := c.openApp()
			if err != nil {
				return err
			}
			return tui.RunContext(cmd.Context(), ap)
		},
	}
}
