// Package cli implements atl's command line with cobra. Every command calls
// only internal/app (and internal/hook for the hidden hook command).
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/andreujuanc/atelier/internal/app"
	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/sandbox"
	"github.com/andreujuanc/atelier/internal/workspace"
)

// Exit codes.
const (
	exitOK    = 0
	exitErr   = 1
	exitUsage = 2
)

// usageError marks errors caused by bad command-line usage (exit 2).
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

func usagef(format string, args ...any) error {
	return usageError{fmt.Errorf(format, args...)}
}

// exitCodeError carries an explicit exit code with nothing to print
// (hook, doctor failures).
type exitCodeError struct{ code int }

func (e exitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// Execute runs atl with os.Args and returns the process exit code.
func Execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runIn(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
}

// cli holds per-invocation state: the output streams and the lazily
// created App.
type cli struct {
	stdout, stderr io.Writer
	app            *app.App
	recovered      bool   // startup Recover already ran
	wsFlag         string // --workspace
	ws             *workspace.Applied
	wsErr          error
	wsDone         bool            // applyWorkspace ran
	forAssistant   bool            // --for-assistant (hidden): the assistant's gate runs this command (sandbox.ForAssistantFlag)
	ctx            context.Context // the invocation's context (for Recover)
}

// App returns the App, creating it on first use and then running startup
// recovery (see recoverOnce).
func (c *cli) App() (*app.App, error) {
	a, err := c.openApp()
	if err != nil {
		return nil, err
	}
	c.recoverOnce(a)
	return a, nil
}

// openApp returns the App, creating it on first use, without recovery (the
// TUI runs its own Recover at start and every few seconds).
func (c *cli) openApp() (*app.App, error) {
	if c.app == nil {
		if err := c.applyWorkspace(); err != nil {
			return nil, err
		}
		a, err := app.New(c.stderr)
		if err != nil {
			return nil, err
		}
		a.Workspace = c.ws
		c.app = a
	}
	return c.app, nil
}

// recoverOnce runs App.Recover the first time it is called: sessions whose
// tmux window is gone become stopped on every atl start (spec "Crash
// recovery"). Failures are warnings, and are not even printed when tmux is
// not installed (doctor reports that).
func (c *cli) recoverOnce(a *app.App) {
	if c.recovered {
		return
	}
	c.recovered = true
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := a.Recover(ctx); err != nil && !errors.Is(err, exec.ErrNotFound) {
		fmt.Fprintf(c.stderr, "atl: warning: %v\n", err)
	}
}

// doctorApp returns an App for doctor, which must work even when the
// database cannot be opened, and without a workspace: then the App has no
// paths and only WorkspaceErr, which only doctor can use.
func (c *cli) doctorApp() (*app.App, error) {
	if c.app != nil {
		return c.app, nil
	}
	if err := c.applyWorkspace(); err != nil {
		return &app.App{Out: c.stderr, Now: time.Now, WorkspaceErr: err}, nil
	}
	return c.pathsApp()
}

// pathsApp returns an App with the workspace applied and its paths, but
// without the database (atl token).
func (c *cli) pathsApp() (*app.App, error) {
	if c.app != nil {
		return c.app, nil
	}
	if err := c.applyWorkspace(); err != nil {
		return nil, err
	}
	p, err := config.Resolve()
	if err != nil {
		return nil, err
	}
	return &app.App{Paths: p, Tmux: app.NewTmux(p, c.stderr), Out: c.stderr, Now: time.Now, Workspace: c.ws}, nil
}

// run is runIn with an empty stdin.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runIn(ctx, args, strings.NewReader(""), stdout, stderr)
}

// runIn runs atl with args; stdin is what commands read (atl notes --set).
func runIn(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	c := &cli{stdout: stdout, stderr: stderr, ctx: ctx}
	if sandbox.InAssistant() && (len(args) == 0 || (args[0] != "hook" && args[0] != "statusline")) {
		return c.assistantViaGate(args, stdin)
	}
	defer func() {
		if c.app != nil {
			c.app.Close()
		}
		// Only for tests, which run atl many times in one process.
		c.ws.Undo()
	}()
	root := c.rootCmd()
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	cmd, err := root.ExecuteContextC(ctx)
	if err == nil {
		return exitOK
	}
	var ec exitCodeError
	if errors.As(err, &ec) {
		return ec.code
	}
	fmt.Fprintf(stderr, "atl: %v\n", err)
	if isUsage(err) {
		if cmd == nil {
			cmd = root
		}
		fmt.Fprintf(stderr, "Run '%s --help' for usage.\n", cmd.CommandPath())
		return exitUsage
	}
	return exitErr
}

// maxAssistantStdin bounds the stdin atl in the assistant's container sends
// with a command (the gate takes at most this much).
const maxAssistantStdin = 1 << 20

// assistantViaGate runs atl in the assistant's container: the command line
// goes, as it is, to the assistant's gate, which runs it where atl runs, in
// its own workspace, if the assistant may run it (sandbox.CheckAssistantArgs:
// the gate decides, not this side); its output and exit status come back.
// Standard input is sent only for `notes --set` (read to the end first),
// the one command that reads it, as the gate reads the command line
// (readsStdin): another may run with a stdin that never ends.
func (c *cli) assistantViaGate(args []string, stdin io.Reader) int {
	var body []byte
	if readsStdin(args) {
		if isTerminal(stdin) {
			fmt.Fprintln(c.stderr, "atl: reading standard input; end with ctrl-d")
		}
		data, err := io.ReadAll(io.LimitReader(stdin, maxAssistantStdin+1))
		if err != nil {
			fmt.Fprintf(c.stderr, "atl: read stdin: %v\n", err)
			return exitErr
		}
		if len(data) > maxAssistantStdin {
			fmt.Fprintf(c.stderr, "atl: stdin is over %d bytes, the limit here\n", maxAssistantStdin)
			return exitErr
		}
		body = data
	}
	req := sandbox.ControlRequest{Op: sandbox.OpAtl, Args: args}
	code, _, err := sandbox.Ask(os.Getenv("ATL_CONTROL_SOCK"), req, body, c.stdout, c.stderr, 0)
	if err != nil {
		fmt.Fprintf(c.stderr, "atl: in the assistant's container atl asks its gate, which did not answer: %v\n", err)
		return exitErr
	}
	return code
}

// readsStdin reports whether the command line is `atl notes ... --set`
// (in any spelling, and not with --help), which reads stdin, as the
// assistant's gate reads it (resolveAtl).
func readsStdin(args []string) bool {
	path, stdin, err := resolveAtl(args)
	return err == nil && path == "atl notes" && stdin
}

func isUsage(err error) bool {
	var ue usageError
	if errors.As(err, &ue) {
		return true
	}
	// Errors cobra builds itself while finding the command.
	msg := err.Error()
	return strings.HasPrefix(msg, "unknown command") || strings.HasPrefix(msg, "unknown flag") ||
		strings.HasPrefix(msg, "unknown shorthand flag")
}

// args wraps a cobra positional-args validator so its errors are usage errors.
func args(v cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, a []string) error {
		if err := v(cmd, a); err != nil {
			return usageError{err}
		}
		return nil
	}
}

// insideContainer refuses, in a sandboxed task's container, every command
// but the ones that work there: they ask the task's gate and never open
// atelier's database, which is not in the container (hook, statusline,
// note, notes, push, fetch), or need nothing at all (help, completion).
func insideContainer(cmd *cobra.Command) error {
	if !sandbox.Inside() {
		return nil
	}
	for c := cmd; c.HasParent(); c = c.Parent() {
		switch c.Name() {
		case "hook", "statusline", "note", "notes", "push", "fetch", "help", "completion":
			return nil
		}
	}
	what := cmd.CommandPath()
	if !cmd.HasParent() {
		what = "atl (the TUI)"
	}
	return fmt.Errorf("%s does not work inside a task container (in here atl only does hooks, the status line, note, notes, push and fetch, through the task's gate); run it where atelier runs", what)
}

func (c *cli) rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "atl",
		Short: "atelier: task studio for parallel Claude Code work",
		Long: "atelier groups Claude Code sessions by task. Each task has its own git worktrees\n" +
			"across one or more repos; Claude runs inside tmux.",
		SilenceErrors: true,
		SilenceUsage:  true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return insideContainer(cmd)
		},
		// With no subcommand: the TUI on a terminal (the workspace
		// selector when no workspace is selected), else the help.
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !isTerminal(os.Stdin) || !isTerminal(c.stdout) {
				return cmd.Help()
			}
			if err := c.applyWorkspace(); err != nil {
				var none *workspace.NoneError
				if errors.As(err, &none) {
					return c.selectWorkspace(cmd.Context(), "")
				}
				return err
			}
			return c.runTUI(cmd.Context())
		},
	}
	root.CompletionOptions.HiddenDefaultCmd = true
	root.PersistentFlags().StringVarP(&c.wsFlag, "workspace", "w", "", "the workspace: a name under $"+workspace.RootVar+" or a path (default: $"+workspace.Var+", else the one the current directory is in)")
	root.PersistentFlags().BoolVar(&c.forAssistant, sandbox.ForAssistantFlag, false, "run for the assistant (its gate passes it)")
	root.PersistentFlags().MarkHidden(sandbox.ForAssistantFlag)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	root.AddCommand(
		c.newCmd(), c.addCmd(), c.reposCmd(), c.lsCmd(), c.openCmd(), c.sessionCmd(), c.resumeCmd(), c.statusCmd(), c.renameCmd(),
		c.diffCmd(), c.prCmd(), c.finishCmd("done"), c.finishCmd("cancel"), c.doctorCmd(), c.hookCmd(), c.statuslineCmd(), c.gateCmd(),
		c.remoteCmd(true), c.remoteCmd(false), c.tokenCmd(),
		c.showCmd(), c.transcriptCmd(), c.noteCmd(), c.notesCmd(), c.assistantCmd(), c.workspaceCmd(),
	)
	return root
}

// applyWorkspace selects the workspace (--workspace, $ATL_WORKSPACE, the
// current directory) and applies it to the environment, once, before
// anything reads the environment for paths. In a task container it does
// nothing: atl there talks to the task's gate and never opens a workspace.
func (c *cli) applyWorkspace() error {
	if c.wsDone {
		return c.wsErr
	}
	c.wsDone = true
	if sandbox.Inside() {
		return nil
	}
	ws, by, err := workspace.Open(c.wsFlag)
	if err == nil {
		c.ws, err = workspace.Apply(ws, by)
	}
	c.wsErr = err
	return err
}
