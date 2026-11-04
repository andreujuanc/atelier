package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/andreujuanc/atelier/internal/gen"
	"github.com/andreujuanc/atelier/internal/gitx"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/store"
)

// AvailableRepos returns the sorted names of the direct subdirectories of
// Paths.Repos (symlinks to directories included) that are git repositories
// usable as a task repo. A missing Repos directory yields an empty list.
func (a *App) AvailableRepos(ctx context.Context) ([]string, error) {
	entries, err := os.ReadDir(a.Paths.Repos)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list repos in %s: %w", a.Paths.Repos, err)
	}
	var cands []string
	for _, e := range entries {
		name := e.Name()
		if _, _, err := validRepoArg(name, model.ModeRW); err != nil || name != e.Name() {
			continue
		}
		if !e.IsDir() && e.Type()&os.ModeSymlink == 0 {
			continue
		}
		cands = append(cands, name)
	}
	// IsRepo runs git twice per directory: check several at once.
	ok := make([]bool, len(cands))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, name := range cands {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, path string) {
			defer func() { <-sem; wg.Done() }()
			ok[i] = gitx.IsRepo(path) // follows symlinks (os.Stat)
		}(i, filepath.Join(a.Paths.Repos, name))
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	names := []string{}
	for i, name := range cands {
		if ok[i] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// addRepoLockWait is how long AddRepo waits for another AddRepo on the
// same task to finish.
const addRepoLockWait = 30 * time.Second

// lockTask takes an exclusive flock on <StateDir>/locks/task-<id>.lock,
// waiting up to addRepoLockWait (or until ctx ends). The returned func
// releases it.
func (a *App) lockTask(ctx context.Context, id int64) (func(), error) {
	dir := filepath.Join(a.Paths.StateDir, "locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("task-%d.lock", id))
	return lockFile(ctx, path, addRepoLockWait, "another operation is adding a repo to this task")
}

// lockFile takes an exclusive flock on path (created if missing), waiting
// up to wait (or until ctx ends); busy describes the holder in the timeout
// error. The returned func releases it.
func lockFile(ctx context.Context, path string, wait time.Duration, busy string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("%s (lock %s): %w", busy, path, ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// AddRepo adds repo r (mode rw if empty) to the open task ref. For rw it
// fetches origin (unless noFetch) and creates a worktree on the task's
// branch at the base branch chosen by the task's Base (see resolveBase); ro needs nothing on disk. The repo is
// then recorded (a failed insert, e.g. because the task was closed
// meanwhile, undoes the worktree and, if this call created it, the
// branch), CLAUDE.md is regenerated and the project's setup script runs in
// the new worktree (a failure is a warning). Concurrent AddRepo calls on
// one task (from any process) are serialised by a lock file. It returns
// the updated task.
func (a *App) AddRepo(ctx context.Context, ref string, r RepoArg, noFetch bool) (*model.Task, error) {
	name, mode, err := validRepoArg(r.Name, r.Mode)
	if err != nil {
		return nil, err
	}
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return nil, err
	}
	unlock, err := a.lockTask(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// Re-read under the lock: another AddRepo may have just finished.
	if t, err = a.Store.GetTask(ctx, t.ID); err != nil {
		return nil, err
	}
	if !t.Status.Open() {
		return nil, fmt.Errorf("task %s is %s; repos can only be added to open tasks", t.Slug, t.Status)
	}
	for _, x := range t.Repos {
		if x.Name == name {
			return nil, fmt.Errorf("task %s already has repo %s (%s)", t.Slug, name, x.Mode)
		}
	}
	repo := a.Paths.RepoPath(name)
	if err := checkRepo(ctx, name, repo); err != nil {
		return nil, err
	}
	tr := model.TaskRepo{Name: name, Mode: mode}
	wt := a.Paths.WorktreePath(t.Slug, name)
	createdBranch := false
	if mode == model.ModeRW {
		if !noFetch {
			fmt.Fprintf(a.out(), "fetching %s...\n", name)
			if err := gitx.Fetch(ctx, repo); err != nil {
				return nil, fmt.Errorf("repo %s: %w", name, err)
			}
		}
		if tr.BaseBranch, tr.BaseCommit, err = resolveBase(ctx, name, repo, t.Base); err != nil {
			return nil, err
		}
		if _, err := os.Lstat(wt); err == nil {
			return nil, fmt.Errorf("repo %s: %s already exists; remove it first", name, wt)
		}
		if err := os.MkdirAll(a.Paths.TaskDir(t.Slug), 0o755); err != nil {
			return nil, fmt.Errorf("create task dir: %w", err)
		}
		_, berr := gitx.RevParse(ctx, repo, "refs/heads/"+t.Branch)
		branchExisted := berr == nil
		if err := gitx.AddWorktree(ctx, repo, wt, t.Branch, tr.BaseCommit); err != nil {
			// git may have created the branch (and worktree) before
			// failing, e.g. in a post-checkout hook: undo only what this
			// call verifiably created. "already exists" means git refused
			// up front and whatever is there belongs to someone else.
			if !strings.Contains(err.Error(), "already exists") {
				_, werr := os.Lstat(wt)
				_, nberr := gitx.RevParse(context.WithoutCancel(ctx), repo, "refs/heads/"+t.Branch)
				createdWT, createdBr := werr == nil, !branchExisted && nberr == nil
				if createdWT || createdBr {
					a.undoWorktree(ctx, name, wt, t.Branch, createdWT, createdBr)
				}
			}
			return nil, fmt.Errorf("repo %s: %w", name, err)
		}
		createdBranch = !branchExisted
	}
	if err := a.Store.AddTaskRepo(ctx, t.ID, tr); err != nil {
		if mode == model.ModeRW {
			a.undoWorktree(ctx, name, wt, t.Branch, true, createdBranch)
		}
		switch {
		case errors.Is(err, store.ErrDuplicate):
			return nil, fmt.Errorf("task %s already has repo %s", t.Slug, name)
		case errors.Is(err, store.ErrTaskClosed):
			return nil, fmt.Errorf("task %s was closed meanwhile; repo %s not added", t.Slug, name)
		}
		return nil, err
	}
	if nt, err := a.Store.GetTask(ctx, t.ID); err == nil {
		t = nt
	} else {
		t.Repos = append(t.Repos, tr)
	}

	// atelier owns CLAUDE.md: rewrite it so it lists the new repo.
	if err := gen.WriteClaudeMD(a.Paths.TaskDir(t.Slug), *t, a.repoInfos(t)); err != nil {
		a.warnf("repo %s added but CLAUDE.md not updated: %v", name, err)
	}
	if mode == model.ModeRW {
		if err := a.runSetup(ctx, t, func(x model.TaskRepo) bool { return x.Name == name }); err != nil {
			a.warnf("setup for %s in task %s failed (repo kept): %v", name, t.Slug, err)
		}
		fmt.Fprintf(a.out(), "added %s (rw) to %s: worktree at %s; running sessions can already see it\n", name, t.Slug, wt)
	} else {
		// ro repos reach Claude only through --add-dir at launch.
		fmt.Fprintf(a.out(), "added %s (ro) to %s: running Claude sessions need a new session (s) to see %s\n", name, t.Slug, name)
	}
	return t, nil
}

// undoWorktree removes (if removeWT) worktree wt of repo name and, if
// deleteBranch and the branch exists, deletes it; failures are warnings.
func (a *App) undoWorktree(ctx context.Context, name, wt, branch string, removeWT, deleteBranch bool) {
	bg := context.WithoutCancel(ctx)
	repo := a.Paths.RepoPath(name)
	if removeWT {
		if err := gitx.RemoveWorktree(bg, repo, wt, true); err != nil {
			a.warnf("rollback: remove worktree %s: %v", wt, err)
		}
	}
	if !deleteBranch {
		return
	}
	if _, err := gitx.RevParse(bg, repo, "refs/heads/"+branch); err == nil {
		if derr := gitx.DeleteBranch(bg, repo, branch); derr != nil {
			a.warnf("rollback: delete branch %s in %s: %v", branch, name, derr)
		}
	}
}
