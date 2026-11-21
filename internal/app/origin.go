package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/andreujuanc/atelier/internal/gen"
	"github.com/andreujuanc/atelier/internal/gitx"
	"github.com/andreujuanc/atelier/internal/model"
)

// pinOrigin returns repo name's pinned origin (see config.Paths.OriginPin),
// pinning it first if it has none: origin's URLs as the config of the repo
// at dir says them now (before any url.<base>.insteadOf rule: the user's
// own rules apply when it is used). ok is false when there is no pin and
// the repo has no origin.
//
// This is trust on first use. atl never clones a repo, so the first moment
// it can look is when a task needs it: ensureSandbox pins every repo of a
// task before the task's container starts, so before any sandboxed agent
// of that task can touch the repo's config. The pin is per repo, as clones
// are shared between tasks: a later task never inherits a URL an earlier
// task wrote. atl never rewrites it on its own; `atl repos pin` accepts the
// config's URL (RepinOrigin).
func (a *App) pinOrigin(ctx context.Context, name, dir string) (o gitx.Origin, ok bool, err error) {
	if o, ok, err = a.loadPin(name); err != nil || ok {
		return o, ok, err
	}
	if o, ok, err = gitx.CurrentOrigin(ctx, dir); err != nil || !ok {
		return o, false, err
	}
	return o, true, a.writePin(name, o)
}

// loadPin reads repo name's pin; ok is false when it has none.
func (a *App) loadPin(name string) (o gitx.Origin, ok bool, err error) {
	path := a.Paths.OriginPin(name)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return o, false, nil
	}
	if err != nil {
		return o, false, err
	}
	if err := json.Unmarshal(data, &o); err != nil || !o.Valid() {
		return o, false, fmt.Errorf("origin pin %s is damaged; run `atl repos pin %s` to pin the repo's current origin", path, name)
	}
	return o, true, nil
}

func (a *App) writePin(name string, o gitx.Origin) error {
	path := a.Paths.OriginPin(name)
	data, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := gen.WriteAtomic(path, append(data, '\n')); err != nil {
		return fmt.Errorf("pin origin: %w", err)
	}
	return nil
}

// RepinOrigin pins repo name's origin anew, from what the config of its
// canonical clone says now, and returns the old pin (had is false if there
// was none) and the new one. It is how the user accepts a changed origin;
// only where atl runs, never from a task container.
func (a *App) RepinOrigin(ctx context.Context, name string) (old gitx.Origin, had bool, now gitx.Origin, err error) {
	if err := outsideSandbox("pin a repo's origin"); err != nil {
		return old, false, now, err
	}
	if name, _, err = validRepoArg(name, model.ModeRW); err != nil {
		return old, false, now, err
	}
	dir := a.Paths.RepoPath(name)
	if err := checkRepo(ctx, name, dir); err != nil {
		return old, false, now, err
	}
	if old, had, err = a.loadPin(name); err != nil {
		had = false // damaged: replaced
	}
	now, ok, err := gitx.CurrentOrigin(ctx, dir)
	if err != nil {
		return old, had, now, err
	}
	if !ok {
		return old, had, now, fmt.Errorf("repo %s has no origin URL (remote.origin.url)", name)
	}
	return old, had, now, a.writePin(name, now)
}

// remoteOrigin is the origin to push to or fetch from for repo name at dir:
// its pin, pinned now if it has none. When the repo's config says something
// else now, the pin wins and a warning on out says so.
func (a *App) remoteOrigin(ctx context.Context, name, dir string, out io.Writer) (gitx.Origin, error) {
	pin, ok, err := a.pinOrigin(ctx, name, dir)
	if err != nil {
		return pin, err
	}
	if !ok {
		return pin, errors.New("the repo has no origin remote")
	}
	now, ok, err := gitx.CurrentOrigin(ctx, dir)
	if err != nil {
		return pin, err
	}
	if !now.Equal(pin) {
		says := "no origin"
		if ok {
			says = now.String()
		}
		if out == nil {
			out = io.Discard
		}
		fmt.Fprintf(out, "warning: repo %s: its config now has origin %s; using the pinned %s (run `atl repos pin %s` to accept the config's)\n",
			name, says, pin, name)
	}
	return pin, nil
}

// pinOrigins pins the origin of each repo of t that has one and no pin yet.
func (a *App) pinOrigins(ctx context.Context, t *model.Task) error {
	for _, r := range t.Repos {
		dir := a.Paths.RepoPath(r.Name)
		if fileExists(a.Paths.OriginPin(r.Name)) || !gitx.IsRepo(dir) {
			continue
		}
		if _, _, err := a.pinOrigin(ctx, r.Name, dir); err != nil {
			return fmt.Errorf("repo %s: %w", r.Name, err)
		}
	}
	return nil
}

// fetchOrigin is atl's own fetch of repo name's canonical clone at dir
// before it branches from it: the pinned, guarded fetch of `atl fetch`,
// with the sandbox on or off (a task may have written the clone's config
// either way, and the plan is for the sandbox to be the only mode).
func (a *App) fetchOrigin(ctx context.Context, name, dir string) error {
	o, err := a.remoteOrigin(ctx, name, dir, a.out())
	if err == nil {
		err = gitx.FetchOrigin(ctx, dir, o, nil)
	}
	return a.noteRefusal(name, "git fetch", err)
}

// addWorktree is gitx.AddWorktree for repo name (canonical clone at repo),
// with its pinned origin.
func (a *App) addWorktree(ctx context.Context, name, repo, wt, branch, start string) error {
	o, _, err := a.pinOrigin(ctx, name, repo)
	if err == nil {
		err = gitx.AddWorktree(ctx, repo, wt, branch, start, o)
	}
	return a.noteRefusal(name, "git worktree add", err)
}

// noteRefusal makes sure the user hears of a git command atl refused to run
// (err is a *gitx.RefusedError: a setting in the repo's own config would
// have made git run a program or go elsewhere): a warning on Out, and a
// line in Paths.RefusalLog, which no task container sees. It returns err.
func (a *App) noteRefusal(repo, what string, err error) error {
	var re *gitx.RefusedError
	if !errors.As(err, &re) {
		return err
	}
	log := a.Paths.RefusalLog()
	a.warnf("repo %s: atl did not run %s: %s; `atl doctor` lists such settings (logged in %s)", repo, what, re.Hazard, log)
	line := fmt.Sprintf("%s repo %s: refused %s in %s: %s\n", time.Now().UTC().Format(time.RFC3339), repo, what, re.Dir, re.Hazard)
	if err := os.MkdirAll(filepath.Dir(log), 0o755); err == nil {
		if f, err := os.OpenFile(log, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644); err == nil {
			_, _ = f.WriteString(line)
			_ = f.Close()
		}
	}
	return err
}
