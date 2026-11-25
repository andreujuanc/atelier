package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"

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
	Name    string `json:"name"`
	Path    string `json:"path"`
	Adopted bool   `json:"adopted"` // false: no atelier.toml
	Current bool   `json:"current"`
}

func (c *cli) workspaceListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list [--json]",
		Short: "Workspaces under $" + workspace.RootVar + ": name and path (* the current one; directories without atelier.toml marked not adopted)",
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
				out = append(out, jsonWorkspace{Name: e.Name, Path: e.Dir, Adopted: e.Workspace, Current: filepath.Clean(e.Dir) == current})
			}
			if asJSON {
				return c.printJSON(out)
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
				fmt.Fprintf(tw, "%s %s\t%s", mark, w.Name, w.Path)
				if !w.Adopted {
					fmt.Fprint(tw, "\tnot adopted (no "+workspace.File+")")
				}
				fmt.Fprintln(tw)
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
		Short: "Create workspace <name> (atelier.toml, tasks/ data/ config/ state/), or adopt the directory if it has no atelier.toml " +
			"(taking the box's settings from its .devcontainer/devcontainer.json); settings not given are taken from $ATL_REPOS, " +
			"$ATL_SANDBOX_MOUNTS, $ATL_SANDBOX_IMAGE, $ATL_SANDBOX_CONTEXT, $ATL_SANDBOX_REGISTRIES and $ATL_CONTEXT",
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
			p, err := workspace.Create(root, name, cfg)
			if err != nil {
				return err
			}
			for _, l := range planLines(p) {
				fmt.Fprintf(c.stderr, "atl: %s\n", l)
			}
			fmt.Fprintf(c.stdout, "%s\t%s\n", name, p.Dir)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&cfg.Repos, "repos", "", "the canonical clones (default: ATL_REPOS of the adopted directory's .devcontainer/devcontainer.json, "+
		"else $ATL_REPOS, else <workspace>/repos; needed to adopt a box's workspace, one with a database, when neither has it)")
	f.StringArrayVar(&mounts, "mount", nil, "a host path every task gets read-only (repeatable; default: from .devcontainer/devcontainer.json, else $ATL_SANDBOX_MOUNTS)")
	f.StringVar(&image, "image", "", "the task container image (default: from .devcontainer/devcontainer.json, else $ATL_SANDBOX_IMAGE, else atelier-task)")
	return cmd
}

// planLines is what `atl workspace new` says about p: each setting written
// with where it came from, the values skipped, warnings and notes.
func planLines(p *workspace.Plan) []string {
	var out []string
	for _, s := range p.Settings {
		out = append(out, s.String())
	}
	for _, s := range p.Skipped {
		out = append(out, "skipped "+s)
	}
	for _, w := range p.Warnings {
		out = append(out, "warning: "+w)
	}
	for _, n := range p.Notes {
		out = append(out, "note: "+n)
	}
	return out
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

// execProcess replaces this process with another program; tests replace it.
var execProcess = syscall.Exec

// reexec replaces this process with atl args in atl's environment as it
// was before a workspace was applied (workspace.OriginalEnv), so that
// nothing of this workspace leaks into the next one. It returns only on
// failure.
func (c *cli) reexec(args ...string) error {
	bin, err := atlBin()
	if err != nil {
		return err
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
	if errors.Is(err, tui.ErrRestart) {
		// Updated from the TUI (U): the same TUI again, on the new binary
		// (os.Executable names the replaced file's path, which now holds
		// it).
		if c.ws != nil && c.ws.Workspace != nil {
			return c.reexec("--workspace", c.ws.Dir)
		}
		return c.reexec()
	}
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
		Load:      func(context.Context) ([]workspace.Entry, error) { return workspace.List(root) },
		// The same as `atl workspace new <name>` without flags.
		Prepare: func(name string) (*workspace.Plan, error) { return workspace.Prepare(root, name, workspace.Config{}) },
		Create:  func(p *workspace.Plan) error { return p.Write() },
	})
	if err != nil || name == "" {
		return err
	}
	return c.openChosen(name)
}

// openChosen re-executes atl in workspace name (the selector's choice).
func (c *cli) openChosen(name string) error { return c.reexec("--workspace", name) }
