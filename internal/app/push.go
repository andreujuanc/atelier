package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/andreujuanc/atelier/internal/gitx"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/store"
)

// RemoteResult is the outcome of Push or Fetch for one repo of a task.
type RemoteResult struct {
	Repo string
	Err  error // nil: done
}

// PushOpts selects what Push does.
type PushOpts struct {
	Repo   string // only this repo of the task; "" → every rw repo
	Force  bool   // --force-with-lease
	TaskID int64  // if not 0: the task is the one with this id, and ref must be its exact slug
}

// FetchOpts selects what Fetch does.
type FetchOpts struct {
	Repo   string // only this repo of the task; "" → every repo
	TaskID int64  // as in PushOpts
}

// Push pushes the task's branch of each rw repo (or only o.Repo) to the
// repo's pinned origin (see pinOrigin), from the repo's canonical clone once
// the task's worktree checks out (checkWorktree), and never any other
// branch (see gitx.PushBranch). git's output goes to out. A
// failure in one repo does not stop the others; it is in that repo's result.
func (a *App) Push(ctx context.Context, ref string, o PushOpts, out io.Writer) ([]RemoteResult, error) {
	t, repos, err := a.remoteRepos(ctx, ref, o.TaskID, o.Repo)
	if err != nil {
		return nil, err
	}
	branch := t.Branch
	if branch == "" {
		branch = "atl/" + t.Slug
	}
	var todo []model.TaskRepo
	for _, r := range repos {
		if r.Mode == model.ModeRW {
			todo = append(todo, r)
		} else if o.Repo != "" {
			return nil, fmt.Errorf("repo %s is read-only in task %s: it has no branch to push", r.Name, t.Slug)
		}
	}
	if len(todo) == 0 {
		return nil, fmt.Errorf("task %s has no rw repos to push", t.Slug)
	}
	var res []RemoteResult
	for _, r := range todo {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		_, err := a.checkWorktree(ctx, t, r)
		if errors.Is(err, os.ErrNotExist) {
			err = errors.New("worktree missing")
		}
		if err != nil {
			res = append(res, RemoteResult{Repo: r.Name, Err: err})
			continue
		}
		// git runs in the canonical clone, which has the branch (refs are
		// shared by its worktrees): the task can rewrite its worktree's
		// .git file, or swap the worktree, after the check above.
		repo := a.Paths.RepoPath(r.Name)
		origin, err := a.remoteOrigin(ctx, r.Name, repo, out)
		if err == nil {
			err = a.noteRefusal(r.Name, "git push", gitx.PushBranch(ctx, repo, origin, branch, o.Force, out))
		}
		res = append(res, RemoteResult{Repo: r.Name, Err: err})
	}
	return res, nil
}

// Fetch runs `git fetch origin` from the pinned origin (see
// gitx.FetchOrigin) in the canonical clone of each repo of the task, rw
// (once its worktree checks out) and ro, or only for o.Repo. git's output
// goes to out.
func (a *App) Fetch(ctx context.Context, ref string, o FetchOpts, out io.Writer) ([]RemoteResult, error) {
	t, repos, err := a.remoteRepos(ctx, ref, o.TaskID, o.Repo)
	if err != nil {
		return nil, err
	}
	var res []RemoteResult
	for _, r := range repos {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		dir, err := a.repoDir(ctx, t, r)
		if err == nil {
			_, err = os.Stat(dir)
		}
		if errors.Is(err, os.ErrNotExist) {
			err = fmt.Errorf("%s missing", dir)
		}
		if err != nil {
			res = append(res, RemoteResult{Repo: r.Name, Err: err})
			continue
		}
		// In the canonical clone, as for Push: a fetch updates the refs its
		// worktrees share.
		dir = a.Paths.RepoPath(r.Name)
		origin, err := a.remoteOrigin(ctx, r.Name, dir, out)
		if err == nil {
			err = a.noteRefusal(r.Name, "git fetch", gitx.FetchOrigin(ctx, dir, origin, out))
		}
		res = append(res, RemoteResult{Repo: r.Name, Err: err})
	}
	return res, nil
}

// remoteRepos resolves the task and its repos, or only repo if it is not "".
// With an id, the task is that one and ref must be its slug exactly (a
// task's gate names its task so: no prefix or id-as-slug matching, and a
// deleted task never turns into another).
func (a *App) remoteRepos(ctx context.Context, ref string, id int64, repo string) (*model.Task, []model.TaskRepo, error) {
	var t *model.Task
	var err error
	if id != 0 {
		t, err = a.Store.GetTask(ctx, id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && t.Slug != ref) {
			return nil, nil, fmt.Errorf("task %d is not %s (any more)", id, ref)
		}
	} else {
		t, err = a.Store.ResolveTask(ctx, ref)
	}
	if err != nil {
		return nil, nil, err
	}
	if repo == "" {
		return t, t.Repos, nil
	}
	for _, r := range t.Repos {
		if r.Name == repo {
			return t, []model.TaskRepo{r}, nil
		}
	}
	return nil, nil, fmt.Errorf("task %s has no repo %q", t.Slug, repo)
}
