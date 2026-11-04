// Package cli implements atl's command line with cobra. Every command calls
// only internal/app (and internal/hook for the hidden hook command).
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/andreujuanc/atelier/internal/app"
	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/tmux"
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
	return run(ctx, os.Args[1:], os.Stdout, os.Stderr)
}

// cli holds per-invocation state: the output streams and the lazily
// created App.
type cli struct {
	stdout, stderr io.Writer
	app            *app.App
}

// App returns the App, creating it on first use.
func (c *cli) App() (*app.App, error) {
	if c.app == nil {
		a, err := app.New(c.stderr)
		if err != nil {
			return nil, err
		}
		c.app = a
	}
	return c.app, nil
}

// doctorApp returns an App for doctor, which must work even when the
// database cannot be opened.
func (c *cli) doctorApp() (*app.App, error) {
	if c.app != nil {
		return c.app, nil
	}
	p, err := config.Resolve()
	if err != nil {
		return nil, err
	}
	return &app.App{Paths: p, Tmux: tmux.New(), Out: c.stderr, Now: time.Now}, nil
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	c := &cli{stdout: stdout, stderr: stderr}
	defer func() {
		if c.app != nil {
			c.app.Close()
		}
	}()
	root := c.rootCmd()
	root.SetArgs(args)
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

func (c *cli) rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "atl",
		Short: "atelier: task studio for parallel Claude Code work",
		Long: "atelier groups Claude Code sessions by task. Each task has its own git worktrees\n" +
			"across one or more repos; Claude runs inside tmux.",
		SilenceErrors: true,
		SilenceUsage:  true,
		// TODO(M6): run the TUI.
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	root.CompletionOptions.HiddenDefaultCmd = true
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	root.AddCommand(
		c.newCmd(), c.lsCmd(), c.openCmd(), c.sessionCmd(), c.resumeCmd(), c.statusCmd(),
		c.diffCmd(), c.prCmd(), c.finishCmd("done"), c.finishCmd("cancel"), c.doctorCmd(), c.hookCmd(),
	)
	return root
}
