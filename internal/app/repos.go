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

// taskLockWait is how long lockTask waits for another operation on the
// same task (AddRepo, done/cancel, a CLAUDE.md rewrite) to finish, and
// lockLaunches for the task's launches.
const taskLockWait = 30 * time.Second

// lockTask takes an exclusive flock on <StateDir>/locks/task-<id>.lock,
// waiting up to taskLockWait (or until ctx ends). The returned func
// releases it.
func (a *App) lockTask(ctx context.Context, id int64) (func(), error) {
	path, err := a.lockPath(fmt.Sprintf("task-%d.lock", id))
	if err != nil {
		return nil, err
	}
	return lockFile(ctx, path, syscall.LOCK_EX, taskLockWait, "another operation on this task is in progress")
}

// lockLaunches takes task id's launch lock exclusive: it waits for the
// launches under way, up to taskLockWait (or until ctx ends), and keeps new
// ones out until the returned func releases it. The launch lock
// (<StateDir>/locks/task-<id>.launch.lock) is apart from the task's lock
// (lockTask): each launch holds it shared (lockLaunch); closing the task
// and adding repos to it hold it exclusive (after lockTask). So no session
// is launched into a task being closed or with repos it would not see,
// and a slow launch (a docker run, an image pull) holds up only those.
func (a *App) lockLaunches(ctx context.Context, id int64) (func(), error) {
	return a.launchLock(ctx, id, syscall.LOCK_EX)
}

// launchLock takes task id's launch lock (see lockLaunches) of kind how.
func (a *App) launchLock(ctx context.Context, id int64, how int) (func(), error) {
	path, err := a.lockPath(fmt.Sprintf("task-%d.launch.lock", id))
	if err != nil {
		return nil, err
	}
	return lockFile(ctx, path, how, taskLockWait, "the task is being closed, or a session is being started in it")
}

// lockPath is the path of lock file name in <StateDir>/locks, made if missing.
func (a *App) lockPath(name string) (string, error) {
	dir := filepath.Join(a.Paths.StateDir, "locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create lock dir: %w", err)
	}
	return filepath.Join(dir, name), nil
}

// lockFile takes a flock of kind how (syscall.LOCK_EX or LOCK_SH) on path
// (created if missing), waiting up to wait (or until ctx ends); busy
// describes the holder in the timeout error. The returned func releases
// it. It follows no symlink at path and takes only a regular file (never a
// pipe or device: it does not wait to open one).
func lockFile(ctx context.Context, path string, how int, wait time.Duration, busy string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("open lock: %s is not a regular file", path)
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
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
//
// Adding a repo the task has as ro again as rw upgrades it: the worktree is
// created as above and the same repo row becomes rw. Any other repeat is an
// error.
//
// It refuses (ErrRestartNeeded), before anything changes, while the task
// has sessions that would not see the repo (see addNeedsRestart): AddRepos
// is the call that restarts them around the add.
func (a *App) AddRepo(ctx context.Context, ref string, r RepoArg, noFetch bool) (*model.Task, error) {
	if err := outsideSandbox("add a repo"); err != nil {
		return nil, err
	}
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
	// A launch under way has read the task's repos: it ends first, and
	// none starts until the repo is in (see lockLaunches).
	unlockLaunches, err := a.lockLaunches(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	defer unlockLaunches()
	return a.addRepoLocked(ctx, t.ID, name, mode, noFetch)
}

// addRepoLocked is AddRepo for the task with id, under the task's lock,
// with name and mode checked (validRepoArg).
func (a *App) addRepoLocked(ctx context.Context, id int64, name string, mode model.Mode, noFetch bool) (*model.Task, error) {
	// Re-read under the lock: another AddRepo may have just finished.
	t, err := a.Store.GetTask(ctx, id)
	if err != nil {
		return nil, err
	}
	if !t.Status.Open() {
		return nil, fmt.Errorf("task %s is %s; repos can only be added to open tasks", t.Slug, t.Status)
	}
	if err := a.undoKilledAdd(ctx, t, name, mode); err != nil {
		return nil, err
	}
	upgrade, err := a.checkAdd(ctx, t, name, mode)
	if err != nil {
		return nil, err
	}
	// A repo the running sessions would not see is never added under them:
	// AddRepos restarts them around the add.
	if need, err := a.addNeedsRestart(ctx, t, []RepoArg{{Name: name, Mode: mode}}); err != nil {
		return nil, err
	} else if need {
		return nil, fmt.Errorf("task %s has running sessions: %w", t.Slug, ErrRestartNeeded)
	}
	repo := a.Paths.RepoPath(name)
	tr := model.TaskRepo{Name: name, Mode: mode}
	wt := a.Paths.WorktreePath(t.Slug, name)
	createdBranch := false
	if mode == model.ModeRW {
		if !noFetch {
			fmt.Fprintf(a.out(), "fetching %s...\n", name)
			if err := a.fetchOrigin(ctx, name, repo); err != nil {
				return nil, fmt.Errorf("repo %s: %w", name, err)
			}
		}
		if tr.BaseBranch, tr.BaseCommit, err = resolveBase(ctx, name, repo, t.Base); err != nil {
			return nil, err
		}
		if err := checkBranchFree(ctx, name, repo, t.Branch); err != nil {
			return nil, err
		}
		if _, err := os.Lstat(wt); err == nil {
			return nil, fmt.Errorf("repo %s: %s already exists; remove it first", name, wt)
		}
		if err := os.MkdirAll(a.Paths.TaskDir(t.Slug), 0o755); err != nil {
			return nil, fmt.Errorf("create task dir: %w", err)
		}
		if createdBranch, err = a.addTaskWorktree(ctx, name, wt, t.Branch, tr.BaseCommit, false); err != nil {
			return nil, fmt.Errorf("repo %s: %w", name, err)
		}
		if afterWorktreeAdd != nil {
			afterWorktreeAdd(wt)
		}
	}
	record := a.Store.AddTaskRepo
	if upgrade {
		record = a.Store.UpgradeTaskRepo
	}
	if err := record(ctx, t.ID, tr); err != nil {
		if mode == model.ModeRW {
			a.undoWorktree(ctx, name, wt, t.Branch, true, createdBranch)
		}
		switch {
		case errors.Is(err, store.ErrDuplicate):
			return nil, fmt.Errorf("task %s already has repo %s", t.Slug, name)
		case upgrade && errors.Is(err, store.ErrNotFound):
			return nil, fmt.Errorf("task %s no longer has repo %s as ro; not upgraded", t.Slug, name)
		case errors.Is(err, store.ErrTaskClosed):
			return nil, fmt.Errorf("task %s was closed meanwhile; repo %s not added", t.Slug, name)
		}
		return nil, err
	}
	if nt, err := a.Store.GetTask(ctx, t.ID); err == nil {
		t = nt
	} else if upgrade {
		for i := range t.Repos {
			if t.Repos[i].Name == name {
				t.Repos[i] = tr
			}
		}
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
	}
	msg := fmt.Sprintf("added %s (%s) to %s", name, mode, t.Slug)
	if upgrade {
		msg = fmt.Sprintf("upgraded %s to rw in %s", name, t.Slug)
	}
	if mode == model.ModeRW {
		msg += ": worktree at " + wt
	}
	fmt.Fprintln(a.out(), msg)
	if need, err := a.addNeedsRestart(ctx, t, []RepoArg{{Name: name, Mode: mode}}); err == nil && need {
		// A session started while the repo was being added (the check
		// above found none): it may not see it.
		a.warnf("a session started in %s while %s was being added may not see it; restart the task's sessions (close them, then r)",
			t.Slug, name)
	}
	return t, nil
}

// afterWorktreeAdd, if set (tests only), runs in AddRepo between the
// worktree's creation and the repo's insert.
var afterWorktreeAdd func(wt string)

// addTaskWorktree adds worktree wt of rw repo name on the task branch:
// the existing local branch (existing) or a new one at start. It reports
// whether it created the branch. On failure it undoes only what it
// verifiably created: git may create the branch (and worktree) before
// failing, e.g. when the checkout fails; "already exists" means git
// refused up front and whatever is there belongs to someone else. If it
// cannot tell whether the branch exists beforehand, it adds nothing: a
// branch that is there would otherwise be deleted as its own.
func (a *App) addTaskWorktree(ctx context.Context, name, wt, branch, start string, existing bool) (createdBranch bool, err error) {
	repo := a.Paths.RepoPath(name)
	branchExisted, err := gitx.RefExists(ctx, repo, "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	if existing {
		err = a.addBranchWorktree(ctx, name, repo, wt, branch)
	} else {
		err = a.addWorktree(ctx, name, repo, wt, branch, start)
	}
	if err == nil {
		return !branchExisted, nil
	}
	if !strings.Contains(err.Error(), "already exists") {
		_, werr := os.Lstat(wt)
		createdBr := false
		if !branchExisted {
			createdBr, _ = gitx.RefExists(context.WithoutCancel(ctx), repo, "refs/heads/"+branch)
		}
		// A missing worktree is only pruned.
		a.undoWorktree(ctx, name, wt, branch, werr == nil || !branchExisted, createdBr)
	}
	return false, err
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

// checkBranchFree refuses to create the task branch in rw repo name (clone
// at repo) when the clone has it, or a branch below it, locally or under
// origin/ (as of the last fetch): like a new task's slug (see slugTaken),
// atl never takes over a branch it did not create.
func checkBranchFree(ctx context.Context, name, repo, branch string) error {
	for _, ref := range []string{"refs/heads/" + branch, "refs/remotes/origin/" + branch} {
		ok, err := gitx.RefOrBelowExists(ctx, repo, ref)
		if err != nil {
			return fmt.Errorf("repo %s: %w", name, err)
		}
		if ok {
			return fmt.Errorf("repo %s: %s (or a branch below it) already exists in its clone; atl does not take over a branch it did not create",
				name, strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/remotes/"))
		}
	}
	return nil
}

// checkAdd checks that repo name can be added to t in mode: not in the task
// yet, or there as ro and now rw (an upgrade, which it reports); a usable
// git repository; for rw, no file at its worktree path.
func (a *App) checkAdd(ctx context.Context, t *model.Task, name string, mode model.Mode) (upgrade bool, err error) {
	for _, x := range t.Repos {
		if x.Name == name {
			if x.Mode != model.ModeRO || mode != model.ModeRW {
				return false, fmt.Errorf("task %s already has repo %s (%s)", t.Slug, name, x.Mode)
			}
			upgrade = true
		}
	}
	if err := checkRepo(ctx, name, a.Paths.RepoPath(name)); err != nil {
		return false, err
	}
	if mode == model.ModeRW {
		wt := a.Paths.WorktreePath(t.Slug, name)
		if _, err := os.Lstat(wt); err == nil {
			return false, fmt.Errorf("repo %s: %s already exists; remove it first", name, wt)
		}
	}
	return upgrade, nil
}

// undoKilledAdd removes what an add of rw repo name to t left when its atl
// was killed after it made the worktree and before it recorded the repo
// (which is otherwise refused forever: the path and the task branch are
// taken), as that add would have undone it on a failure: a worktree at the
// repo's path in the task dir, of the repo's clone, on the task branch,
// clean and with no commit that another branch or remote-tracking ref lacks.
// Anything else there is left alone (checkAdd refuses it). It is called
// under the task's lock, before checkAdd.
func (a *App) undoKilledAdd(ctx context.Context, t *model.Task, name string, mode model.Mode) error {
	if mode != model.ModeRW {
		return nil
	}
	for _, r := range t.Repos {
		if r.Name == name && r.Mode == model.ModeRW {
			return nil
		}
	}
	repo, wt := a.Paths.RepoPath(name), a.Paths.WorktreePath(t.Slug, name)
	if _, err := os.Lstat(wt); err != nil || gitx.CheckWorktree(ctx, wt, repo) != nil {
		return nil
	}
	if b, err := gitx.CurrentBranch(ctx, wt); err != nil || b != t.Branch {
		return nil
	}
	tip, err := gitx.RevParse(ctx, repo, "refs/heads/"+t.Branch)
	if err != nil {
		return nil
	}
	if n, err := gitx.UniqueCommitsAt(ctx, repo, t.Branch, tip); err != nil || n > 0 {
		return nil
	}
	if dirty, err := gitx.IsDirty(ctx, wt); err != nil || dirty {
		return nil
	}
	fmt.Fprintf(a.out(), "repo %s: removing %s and branch %s, left by an add that did not finish\n", name, wt, t.Branch)
	if err := gitx.RemoveWorktree(ctx, repo, wt, false); err != nil {
		return fmt.Errorf("repo %s: remove the worktree an unfinished add left: %w", name, err)
	}
	if err := gitx.DeleteBranchIfAt(ctx, repo, t.Branch, tip); err != nil {
		return fmt.Errorf("repo %s: delete the branch an unfinished add left: %w", name, err)
	}
	return nil
}
