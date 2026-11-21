package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/andreujuanc/atelier/internal/gen"
	"github.com/andreujuanc/atelier/internal/gitx"
	"github.com/andreujuanc/atelier/internal/model"
)

// pinOrigin returns repo name's pinned origin (see config.Paths.OriginPin),
// pinning it first if it has none: origin's URLs as the repo at dir resolves
// them now. ok is false when there is no pin and the repo has no origin.
//
// This is trust on first use. atl never clones a repo, so the first moment
// it can look is when a task needs it: ensureSandbox pins every repo of a
// task before the task's container starts, so before any sandboxed agent
// of that task can touch the repo's config. The pin is per repo, as clones
// are shared between tasks: a later task never inherits a URL an earlier
// task wrote. atl never rewrites it; deleting it accepts the config's URL.
func (a *App) pinOrigin(ctx context.Context, name, dir string) (o gitx.Origin, ok bool, err error) {
	path := a.Paths.OriginPin(name)
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &o); err != nil || o.URL == "" || len(o.PushURLs) == 0 {
			return o, false, fmt.Errorf("origin pin %s is damaged; delete it to pin the repo's current origin", path)
		}
		return o, true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return o, false, err
	}
	if o, ok, err = gitx.CurrentOrigin(ctx, dir); err != nil || !ok {
		return o, false, err
	}
	data, err = json.MarshalIndent(o, "", "  ")
	if err != nil {
		return o, false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return o, false, err
	}
	if err := gen.WriteAtomic(path, append(data, '\n')); err != nil {
		return o, false, fmt.Errorf("pin origin: %w", err)
	}
	return o, true, nil
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
		fmt.Fprintf(out, "warning: repo %s: its config now has origin %s; using the pinned %s (delete %s to accept the config's)\n",
			name, says, pin, a.Paths.OriginPin(name))
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
// before it branches from it. With the sandbox on, a task may have written
// the clone's config, so it is the pinned, guarded fetch of `atl fetch`;
// without, it is a plain `git fetch origin` as the user's own git does it
// (a clone's own core.sshCommand or insteadOf rules are theirs then).
func (a *App) fetchOrigin(ctx context.Context, name, dir string) error {
	if a.Sandbox == nil {
		return gitx.Fetch(ctx, dir)
	}
	o, err := a.remoteOrigin(ctx, name, dir, a.out())
	if err != nil {
		return err
	}
	return gitx.FetchOrigin(ctx, dir, o, nil)
}
