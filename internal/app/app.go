// Package app is atelier's service layer. The CLI and the TUI call only this
// package; it ties together the store, git, tmux and file generation.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/gen"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/store"
	"github.com/andreujuanc/atelier/internal/tmux"
)

// App holds everything a command needs.
type App struct {
	Paths  config.Paths
	Store  *store.Store
	Tmux   *tmux.Client
	Out    io.Writer        // progress/warnings
	Now    func() time.Time // nil → time.Now
	AtlBin string           // for hook commands
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
	bin := os.Getenv("ATL_BIN")
	if bin == "" {
		if bin, err = os.Executable(); err != nil {
			return nil, fmt.Errorf("locate atl binary (set $ATL_BIN): %w", err)
		}
	}
	st, err := store.Open(paths.DB)
	if err != nil {
		return nil, err
	}
	return &App{Paths: paths, Store: st, Tmux: tmux.New(), Out: out, Now: time.Now, AtlBin: bin}, nil
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
	if err := gen.WriteSettings(dir, a.AtlBin); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "CLAUDE.md")); errors.Is(err, os.ErrNotExist) {
		return gen.WriteClaudeMD(dir, *t, a.repoInfos(t))
	} else if err != nil {
		return err
	}
	return nil
}

// launchEnv is the environment passed to a Claude window.
// TODO(M7): merge the project's env underneath these.
func (a *App) launchEnv(t *model.Task, s *model.Session) map[string]string {
	return map[string]string{
		"ATL_TASK":        t.Slug,
		"ATL_SESSION":     strconv.FormatInt(s.ID, 10),
		"ATL_PORT_OFFSET": strconv.Itoa(t.PortOffset),
		"ATL_TASK_DIR":    a.Paths.TaskDir(t.Slug),
	}
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
