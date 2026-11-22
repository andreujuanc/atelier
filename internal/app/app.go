// Package app is atelier's service layer. The CLI and the TUI call only this
// package; it ties together the store, git, tmux and file generation.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/gen"
	"github.com/andreujuanc/atelier/internal/hook"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/nofollow"
	"github.com/andreujuanc/atelier/internal/sandbox"
	"github.com/andreujuanc/atelier/internal/store"
	"github.com/andreujuanc/atelier/internal/tmux"
	"github.com/andreujuanc/atelier/internal/workspace"
)

// App holds everything a command needs.
type App struct {
	Paths  config.Paths
	Store  *store.Store
	Tmux   *tmux.Client
	Out    io.Writer        // progress/warnings
	Now    func() time.Time // nil → time.Now
	AtlBin string           // for hook commands

	// Sandbox, if not nil, runs every task's Claude sessions in the task's
	// own Docker container (the default; nil with $ATL_SANDBOX=off, and in a
	// task container; see package sandbox).
	Sandbox *sandbox.Config
	Docker  sandbox.Runner // nil → the docker CLI
	// StartGate starts a task's gate if none runs; nil → sandbox.Start.
	StartGate func(atlBin, slug string, taskID int64, runDir, logPath string, roMounts []string) error
	// CheckBin checks that AtlBin can run in a task container; nil →
	// sandbox.CheckStaticBin.
	CheckBin func(path string) error

	// Workspace is the workspace the CLI applied (nil when none was, as
	// in a task container); WorkspaceErr why none could be (doctor reports
	// it and checks only the tools).
	Workspace    *workspace.Applied
	WorkspaceErr error
}

// New resolves paths from the environment, opens the store and returns an
// App writing progress to out (io.Discard if nil).
func New(out io.Writer) (*App, error) {
	if out == nil {
		out = io.Discard
	}
	paths, err := config.Resolve()
	if err != nil {
		return nil, err
	}
	if err := hook.CheckEnvContext(); err != nil {
		return nil, err
	}
	bin, err := atlBinary()
	if err != nil {
		return nil, err
	}
	sb, err := sandbox.FromEnv()
	if err != nil {
		return nil, err
	}
	if sb != nil {
		if sb.Mounts, err = sandbox.ROMounts(paths); err != nil {
			return nil, err
		}
	}
	st, err := store.Open(paths.DB)
	if err != nil {
		return nil, err
	}
	return &App{Paths: paths, Store: st, Tmux: NewTmux(paths, out), Out: out, Now: time.Now, AtlBin: bin, Sandbox: sb}, nil
}

// atlBinary is the atl binary hooks run and task containers get: $ATL_BIN,
// else the running one.
func atlBinary() (string, error) {
	if bin := os.Getenv("ATL_BIN"); bin != "" {
		return bin, nil
	}
	bin, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate atl binary (set $ATL_BIN): %w", err)
	}
	return bin, nil
}

// NewTmux returns the client for atl's tmux server ($ATL_TMUX_SOCKET), which
// starts it with the config atl generates (Paths.TmuxConf, see
// tmux.Client.UseConfig). When that file cannot be written a warning goes
// to out and the server starts with tmux's own config.
func NewTmux(paths config.Paths, out io.Writer) *tmux.Client {
	c := tmux.New()
	if err := c.UseConfig(paths.TmuxConf(), paths.UserTmuxConf()); err != nil && out != nil {
		fmt.Fprintf(out, "warning: %v\n", err)
	}
	return c
}

// Close closes the store.
func (a *App) Close() error {
	if a.Store == nil {
		return nil
	}
	return a.Store.Close()
}

func (a *App) out() io.Writer {
	if a.Out == nil {
		return io.Discard
	}
	return a.Out
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// prepareTaskDir refreshes generated files in the task dir before a Claude
// window is launched: .claude/settings.json is always rewritten (keeping the
// atl path current) and CLAUDE.md is written only if missing.
func (a *App) prepareTaskDir(ctx context.Context, t *model.Task) error {
	dir := a.Paths.TaskDir(t.Slug)
	if err := gen.WriteSettings(dir, a.hookBin()); err != nil {
		return err
	}
	d, err := nofollow.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if _, err := d.Lstat("CLAUDE.md"); errors.Is(err, os.ErrNotExist) {
		return gen.WriteClaudeMD(dir, *t, a.repoInfos(t))
	} else if err != nil {
		return err
	}
	return nil
}

// launchEnv is the environment passed to a Claude window: the project's env
// (if the task has a project that still loads; otherwise a warning goes to
// a.Out) overridden by ATL_TASK, ATL_SESSION, ATL_PORT_OFFSET and
// ATL_TASK_DIR.
func (a *App) launchEnv(t *model.Task, s *model.Session) map[string]string {
	env := map[string]string{}
	pr, err := a.taskProject(t)
	if err != nil {
		// err already names the project: `project "x": ...`.
		a.warnf("%v; launching without project env", err)
	}
	if pr != nil {
		for k, v := range pr.Env {
			env[k] = v
		}
	}
	env["ATL_TASK"] = t.Slug
	env["ATL_SESSION"] = strconv.FormatInt(s.ID, 10)
	env["ATL_PORT_OFFSET"] = strconv.Itoa(t.PortOffset)
	env["ATL_TASK_DIR"] = a.Paths.TaskDir(t.Slug)
	return env
}

// roPaths returns the canonical paths of the task's ro repos, in repo order.
func (a *App) roPaths(t *model.Task) []string {
	var out []string
	for _, r := range t.Repos {
		if r.Mode == model.ModeRO {
			out = append(out, a.Paths.RepoPath(r.Name))
		}
	}
	return out
}

// windowEnv is env, the -e of a tmux window atl creates, with the
// workspace's variables (workspace.WindowEnv) when atl runs in one: a
// window inherits the tmux server's environment from when the server
// started, which need not be this workspace's or current. env wins.
func (a *App) windowEnv(env map[string]string) map[string]string {
	if a.Workspace == nil {
		return env
	}
	out := workspace.WindowEnv()
	for k, v := range env {
		out[k] = v
	}
	return out
}
