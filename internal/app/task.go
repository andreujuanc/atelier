package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/andreujuanc/atelier/internal/gen"
	"github.com/andreujuanc/atelier/internal/gitx"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/project"
	"github.com/andreujuanc/atelier/internal/store"
	"github.com/andreujuanc/atelier/internal/tmux"
)

// RepoArg is a repo requested on the command line (`--repo name:mode`).
type RepoArg struct {
	Name string
	Mode model.Mode
}

// NewTaskOpts are the inputs of NewTask.
type NewTaskOpts struct {
	Title, Description, Project string
	Repos                       []RepoArg // override the mode of a project repo, or add one
	Backlog, NoFetch            bool
	// Base selects the branch every rw repo starts from: model.BaseAuto
	// (""; origin/dev, else origin/develop, else origin's default branch),
	// model.BaseDefault (origin's default branch) or an exact branch name
	// that must exist on origin in every rw repo. It is stored on the task
	// and used again for repos added later.
	Base string
	// ExactRepos: Repos is the complete repo list; the project's repos are
	// not merged in (the project is still loaded, for setup and env). Used
	// by the TUI, where the project only preselects repos in the picker.
	ExactRepos bool
}

// TaskView is a task with its sessions and (from Get only) repo status
// and notes text.
type TaskView struct {
	Task       model.Task
	Sessions   []model.Session
	RepoStatus []RepoStatus
	Notes      string // Get only: the task's notes text ("" if none)
}

// RepoStatus describes one repo of a task on disk.
type RepoStatus struct {
	Name       string
	Mode       model.Mode
	Path       string // worktree (rw) or canonical repo (ro)
	BaseBranch string // origin branch the task branch started from (rw)
	Dirty      bool   // rw only
	Err        string // "missing" if the worktree is gone, or the git error
}

// RepoPathFor returns where repo r of task t lives: its worktree (rw) or
// the canonical clone (ro).
func (a *App) RepoPathFor(t *model.Task, r model.TaskRepo) string {
	if r.Mode == model.ModeRO {
		return a.Paths.RepoPath(r.Name)
	}
	return a.Paths.WorktreePath(t.Slug, r.Name)
}

// checkWorktree returns the worktree path of rw repo r of task t, and nil
// if it is still a worktree of r's canonical clone (gitx.CheckWorktree).
// The task can write its task dir: before atl runs git or a script in the
// path, it makes sure the task has not put a symlink or another repo's
// worktree there. A missing worktree is an error satisfying
// errors.Is(err, os.ErrNotExist).
func (a *App) checkWorktree(ctx context.Context, t *model.Task, r model.TaskRepo) (string, error) {
	wt := a.Paths.WorktreePath(t.Slug, r.Name)
	return wt, gitx.CheckWorktree(ctx, wt, a.Paths.RepoPath(r.Name))
}

// repoDir is RepoPathFor, with an rw repo's worktree checked
// (checkWorktree); an ro repo's canonical clone is not the task's to write.
func (a *App) repoDir(ctx context.Context, t *model.Task, r model.TaskRepo) (string, error) {
	if r.Mode == model.ModeRO {
		return a.Paths.RepoPath(r.Name), nil
	}
	return a.checkWorktree(ctx, t, r)
}

func (a *App) repoInfos(t *model.Task) []gen.RepoInfo {
	out := make([]gen.RepoInfo, 0, len(t.Repos))
	for _, r := range t.Repos {
		out = append(out, gen.RepoInfo{Name: r.Name, Mode: r.Mode, Path: a.RepoPathFor(t, r), BaseBranch: r.BaseBranch})
	}
	return out
}

func (a *App) warnf(format string, args ...any) {
	fmt.Fprintf(a.out(), "warning: "+format+"\n", args...)
}

// mergeRepos combines the project's repos with opts.Repos: a --repo with the
// name of a project repo overrides its mode, others are appended in order.
func (a *App) mergeRepos(o NewTaskOpts) ([]model.TaskRepo, error) {
	var repos []model.TaskRepo
	idx := map[string]int{}
	add := func(name string, mode model.Mode) error {
		name, mode, err := validRepoArg(name, mode)
		if err != nil {
			return err
		}
		if i, ok := idx[name]; ok {
			repos[i].Mode = mode
			return nil
		}
		idx[name] = len(repos)
		repos = append(repos, model.TaskRepo{Name: name, Mode: mode})
		return nil
	}
	if o.Project != "" {
		pr, err := project.Load(a.Paths, o.Project)
		if err != nil {
			return nil, err
		}
		if !o.ExactRepos {
			for _, r := range pr.Repos {
				if err := add(r.Name, r.Mode); err != nil {
					return nil, err
				}
			}
		}
	}
	for _, r := range o.Repos {
		if err := add(r.Name, r.Mode); err != nil {
			return nil, err
		}
	}
	if len(repos) == 0 {
		if o.Project != "" {
			return nil, fmt.Errorf("project %s has no repos; add some with --repo name:rw|ro", o.Project)
		}
		return nil, errors.New("no repos: pass --project or --repo name:rw|ro")
	}
	return repos, nil
}

// validRepoArg trims and validates a requested repo name (a single path
// component, not reserved in the task dir) and mode (empty → rw).
func validRepoArg(name string, mode model.Mode) (string, model.Mode, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", "", fmt.Errorf("invalid repo name %q", name)
	}
	// The task dir also holds the generated CLAUDE.md and .claude/.
	if strings.EqualFold(name, ".claude") || strings.EqualFold(name, "CLAUDE.md") {
		return "", "", fmt.Errorf("invalid repo name %q (reserved in the task dir)", name)
	}
	if mode == "" {
		mode = model.ModeRW
	}
	if !mode.Valid() {
		return "", "", fmt.Errorf("repo %s: invalid mode %q (want rw or ro)", name, mode)
	}
	return name, mode, nil
}

// checkRepo explains why path is not a usable canonical clone, or returns nil.
func checkRepo(ctx context.Context, name, path string) error {
	if gitx.IsRepo(path) {
		return nil
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("repo %s: %s does not exist", name, path)
	} else if err != nil {
		return fmt.Errorf("repo %s: %w", name, err)
	}
	if _, err := gitx.RevParse(ctx, path, "HEAD"); err != nil {
		return fmt.Errorf("repo %s: %s is not a usable git repository: %w", name, path, err)
	}
	return fmt.Errorf("repo %s: %s is not the root of a git repository", name, path)
}

// NewTask creates a task: validates the inputs, fetches every rw repo and
// records its base branch (see NewTaskOpts.Base and resolveBase) and that
// branch's commit on origin, inserts the task row (allocating slug and port
// offset atomically: the slug also free of task dirs and atl/<slug>
// branches, see slugTaken; the offset also free of the open tasks of other
// workspaces, see otherWorkspaceTasks), creates the task dir with a
// worktree per rw repo on branch atl/<slug> starting at that commit, and
// writes CLAUDE.md and .claude/settings.json. Any failure up to this point
// after the insert rolls everything back. Then, once the task exists, the
// project's setup script (if any) runs in each rw worktree; a setup failure
// is only reported as a warning and leaves the task in place.
func (a *App) NewTask(ctx context.Context, o NewTaskOpts) (*model.Task, error) {
	if err := outsideSandbox("create a task"); err != nil {
		return nil, err
	}
	// The title is one line: collapse whitespace runs (newlines included).
	title := strings.Join(strings.Fields(o.Title), " ")
	if title == "" {
		return nil, errors.New("title is required")
	}
	// Then the checks of a rename (other control characters, UTF-8, the
	// length cap), so `atl rename`/TUI `t` can always resave it.
	title, err := model.NormalizeTitle(title)
	if err != nil {
		return nil, err
	}
	o.Project = strings.TrimSpace(o.Project)
	repos, err := a.mergeRepos(o)
	if err != nil {
		return nil, err
	}
	for _, r := range repos {
		if err := checkRepo(ctx, r.Name, a.Paths.RepoPath(r.Name)); err != nil {
			return nil, err
		}
	}
	base, err := NormalizeBase(o.Base)
	if err != nil {
		return nil, err
	}
	for i := range repos {
		r := &repos[i]
		if r.Mode != model.ModeRW {
			continue
		}
		path := a.Paths.RepoPath(r.Name)
		if !o.NoFetch {
			fmt.Fprintf(a.out(), "fetching %s...\n", r.Name)
			if err := a.fetchOrigin(ctx, r.Name, path); err != nil {
				return nil, fmt.Errorf("repo %s: %w", r.Name, err)
			}
		}
		if r.BaseBranch, r.BaseCommit, err = resolveBase(ctx, r.Name, path, base); err != nil {
			return nil, err
		}
	}

	status := model.StatusActive
	if o.Backlog {
		status = model.StatusBacklog
	}
	t := &model.Task{
		Slug:        model.Slugify(title),
		Title:       title,
		Description: strings.TrimSpace(model.SanitizeNotes(o.Description)), // as notes: keeps newlines and tabs
		Status:      status,
		Project:     o.Project,
		Base:        base,
		Repos:       repos,
	}
	// The port offset also avoids the open tasks of the other workspaces
	// (they share the host's ports); the slug also avoids task dirs and
	// atl/<slug> branches already there.
	others := a.otherWorkspaceTasks(ctx)
	for _, w := range others.Warnings {
		a.warnf("%s", w)
	}
	avoid := store.Avoid{SlugTaken: a.slugTaken(ctx, repos)}
	for _, ot := range others.Tasks {
		avoid.PortOffsets = append(avoid.PortOffsets, ot.PortOffset)
	}
	if err := a.Store.CreateTaskAutoPort(ctx, t, avoid); err != nil {
		return nil, err
	}
	if err := a.populateTaskDir(ctx, t); err != nil {
		return nil, err
	}
	if err := a.runSetup(ctx, t, nil); err != nil {
		a.warnf("setup for task %s failed (task kept): %v", t.Slug, err)
	}
	return t, nil
}

// NormalizeBase validates a task base choice and returns it trimmed:
// model.BaseAuto (""), model.BaseDefault (":default") or a branch name.
func NormalizeBase(base string) (string, error) {
	base = strings.TrimSpace(base)
	switch {
	case base == model.BaseAuto || base == model.BaseDefault:
		return base, nil
	case strings.HasPrefix(base, ":"):
		return "", fmt.Errorf("invalid base %q (want %q, a branch name, or empty for auto)", base, model.BaseDefault)
	case strings.HasPrefix(base, "-") || strings.ContainsAny(base, " \t\n~^:?*[\\") ||
		strings.Contains(base, "..") || strings.HasPrefix(base, "refs/") || strings.HasPrefix(base, "origin/"):
		return "", fmt.Errorf("invalid base branch %q (give the branch name as on origin, e.g. dev)", base)
	}
	return base, nil
}

// BaseLabel describes a task base choice for humans: "auto", "default" or
// the branch name.
func BaseLabel(base string) string {
	switch base {
	case model.BaseAuto:
		return "auto"
	case model.BaseDefault:
		return "default"
	}
	return base
}

// resolveBase returns the branch rw repo name (canonical clone at path)
// starts from under the task base choice base, and that branch's commit on
// origin (refs/remotes/origin/<branch>, as of the last fetch): BaseAuto →
// gitx.IntegrationBranch, BaseDefault → gitx.DefaultBranch, else the named
// branch, which must exist on origin.
func resolveBase(ctx context.Context, name, path, base string) (branch, commit string, err error) {
	switch base {
	case model.BaseAuto:
		branch, err = gitx.IntegrationBranch(ctx, path)
	case model.BaseDefault:
		branch, err = gitx.DefaultBranch(ctx, path)
	default:
		var ok bool
		if ok, err = gitx.RefExists(ctx, path, "refs/remotes/origin/"+base); err == nil && !ok {
			return "", "", fmt.Errorf("repo %s: base branch %q not found on origin (no refs/remotes/origin/%s)", name, base, base)
		}
		branch = base
	}
	if err != nil {
		return "", "", fmt.Errorf("repo %s: %w", name, err)
	}
	if commit, err = gitx.RevParse(ctx, path, "refs/remotes/origin/"+branch); err != nil {
		return "", "", fmt.Errorf("repo %s: %w", name, err)
	}
	return branch, commit, nil
}

// populateTaskDir creates the task dir, worktrees and generated files for a
// freshly inserted task (buildTaskDir, each rw repo on a new task branch at
// its base commit). On failure it undoes everything, including the task
// row, and returns the original error.
func (a *App) populateTaskDir(ctx context.Context, t *model.Task) error {
	var plan []rwCheckout
	for _, r := range t.Repos {
		if r.Mode == model.ModeRW {
			plan = append(plan, rwCheckout{Repo: r, Start: r.BaseCommit})
		}
	}
	if _, err := a.buildTaskDir(ctx, t, plan, false); err != nil {
		if rerr := a.Store.DeleteTask(context.WithoutCancel(ctx), t.ID); rerr != nil {
			a.warnf("rollback: delete task row %s: %v", t.Slug, rerr)
		}
		return err
	}
	return nil
}

// rwCheckout says how buildTaskDir puts the worktree of rw repo Repo in
// place: Keep, it is there already; Existing, on the task branch the
// canonical clone has; otherwise on a new task branch starting at Start.
type rwCheckout struct {
	Repo     model.TaskRepo
	Start    string
	Existing bool
	Keep     bool
}

// buildTaskDir puts in place what an open task t has on disk: the task dir,
// a worktree on t.Branch per entry of plan, and the generated CLAUDE.md and
// .claude/settings.json. The task dir must not exist yet unless dirMayExist.
// On failure it undoes what it made (worktrees, the branches it created, the
// task dir if it made it) and returns the error. On success it returns that
// undo, for a caller whose next step fails; generated files it wrote into a
// task dir it did not make are left.
func (a *App) buildTaskDir(ctx context.Context, t *model.Task, plan []rwCheckout, dirMayExist bool) (undo func(), err error) {
	dir := a.Paths.TaskDir(t.Slug)
	type made struct {
		name, wt string
		branch   bool // the branch too
	}
	var created []made
	madeDir := false
	rollback := func() {
		for i := len(created) - 1; i >= 0; i-- {
			c := created[i]
			a.undoWorktree(ctx, c.name, c.wt, t.Branch, true, c.branch)
		}
		if madeDir {
			if rerr := os.RemoveAll(dir); rerr != nil {
				a.warnf("rollback: remove %s: %v", dir, rerr)
			}
		}
	}
	defer func() {
		if err != nil {
			rollback()
		}
	}()

	if err := os.MkdirAll(a.Paths.Tasks, 0o755); err != nil {
		return nil, fmt.Errorf("create tasks dir: %w", err)
	}
	if err := os.Mkdir(dir, 0o755); err == nil {
		madeDir = true
	} else if !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create task dir: %w", err)
	} else if !dirMayExist {
		return nil, fmt.Errorf("task dir %s already exists; remove it or choose another title", dir)
	} else if fi, lerr := os.Lstat(dir); lerr != nil || !fi.IsDir() {
		return nil, fmt.Errorf("task dir %s is there but not a directory; remove it first", dir)
	}
	for _, c := range plan {
		if c.Keep {
			continue
		}
		name := c.Repo.Name
		wt := a.Paths.WorktreePath(t.Slug, name)
		createdBranch, err := a.addTaskWorktree(ctx, name, wt, t.Branch, c.Start, c.Existing)
		if err != nil {
			return nil, fmt.Errorf("repo %s: %w", name, err)
		}
		created = append(created, made{name, wt, createdBranch})
	}
	if err := gen.WriteClaudeMD(dir, *t, a.repoInfos(t)); err != nil {
		return nil, err
	}
	if err := gen.WriteSettings(dir, a.hookBin()); err != nil {
		return nil, err
	}
	return rollback, nil
}

// taskProject loads the task's project, or returns nil if it has none.
func (a *App) taskProject(t *model.Task) (*project.Project, error) {
	if t.Project == "" {
		return nil, nil
	}
	return project.Load(a.Paths, t.Project)
}

// scriptEnv is the script context for rw repo r of task t.
func (a *App) scriptEnv(t *model.Task, r model.TaskRepo) project.ScriptEnv {
	return project.ScriptEnv{Task: *t, TaskDir: a.Paths.TaskDir(t.Slug), Repo: r.Name,
		Worktree: a.Paths.WorktreePath(t.Slug, r.Name)}
}

// runScript runs one setup/teardown script for rw repo r, streaming its
// output to Out as "<kind> <repo>: <line>".
func (a *App) runScript(ctx context.Context, pr *project.Project, kind, script string, t *model.Task, r model.TaskRepo) error {
	se := a.scriptEnv(t, r)
	if _, err := a.checkWorktree(ctx, t, r); err != nil {
		return err
	}
	fmt.Fprintf(a.out(), "%s %s: $ %s\n", kind, r.Name, strings.TrimSpace(script))
	pw := &prefixWriter{w: a.out(), prefix: kind + " " + r.Name + ": "}
	err := project.RunScript(ctx, script, se.Worktree, pr.EnvFor(se), pw)
	pw.Flush()
	return err
}

// runSetup runs the project's setup script in each rw worktree (only those
// for which only returns true, if only is not nil). A failure in one repo
// is a warning; the remaining repos still run. The returned error is only
// for a project that cannot be loaded.
func (a *App) runSetup(ctx context.Context, t *model.Task, only func(model.TaskRepo) bool) error {
	pr, err := a.taskProject(t)
	if err != nil || pr == nil || strings.TrimSpace(pr.Setup) == "" {
		return err
	}
	for _, r := range t.Repos {
		if r.Mode != model.ModeRW || (only != nil && !only(r)) {
			continue
		}
		if err := a.runScript(ctx, pr, "setup", pr.Setup, t, r); err != nil {
			a.warnf("setup failed in %s: %v", r.Name, err)
		}
	}
	return nil
}

// runTeardown runs the project's teardown script in each rw worktree that
// still exists. Without force the first failure is returned (and the
// remaining repos are not torn down); with force failures are warnings. A
// project that can no longer be loaded is a warning, never an error, so it
// cannot block closing the task.
func (a *App) runTeardown(ctx context.Context, t *model.Task, force bool) error {
	pr, err := a.taskProject(t)
	if err != nil {
		a.warnf("skipping teardown: %v", err)
		return nil
	}
	if pr == nil || strings.TrimSpace(pr.Teardown) == "" {
		return nil
	}
	for _, r := range t.Repos {
		if r.Mode != model.ModeRW {
			continue
		}
		if _, err := os.Lstat(a.Paths.WorktreePath(t.Slug, r.Name)); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := a.runScript(ctx, pr, "teardown", pr.Teardown, t, r); err != nil { // checks the worktree
			if !force {
				return fmt.Errorf("repo %s: %w", r.Name, err)
			}
			a.warnf("teardown failed in %s: %v", r.Name, err)
		}
	}
	return nil
}

// prefixWriter writes each line to w with prefix; Flush emits a trailing
// partial line. '\r' ends a line too, so progress output (docker, npm)
// streams; empty segments (e.g. from "\r\n") are skipped.
type prefixWriter struct {
	w      io.Writer
	prefix string
	buf    []byte
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexAny(p.buf, "\r\n")
		if i < 0 {
			break
		}
		line := p.buf[:i]
		p.buf = p.buf[i+1:]
		if len(line) == 0 {
			continue
		}
		if _, err := fmt.Fprintf(p.w, "%s%s\n", p.prefix, line); err != nil {
			p.buf = nil // progress output is best effort; keep the script running
			return len(b), nil
		}
	}
	return len(b), nil
}

// Flush writes any buffered partial line.
func (p *prefixWriter) Flush() {
	if len(p.buf) > 0 {
		fmt.Fprintf(p.w, "%s%s\n", p.prefix, p.buf)
		p.buf = nil
	}
}

// List returns tasks with the given statuses (all if none) with their
// sessions; RepoStatus is not filled.
func (a *App) List(ctx context.Context, statuses ...model.Status) ([]TaskView, error) {
	tasks, err := a.Store.ListTasks(ctx, statuses...)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(tasks))
	for i, t := range tasks {
		ids[i] = t.ID
	}
	sessions, err := a.Store.ListSessionsByTask(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]TaskView, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, TaskView{Task: t, Sessions: sessions[t.ID]})
	}
	return out, nil
}

// Get returns one task with sessions, notes and repo status (dirty
// check for rw worktrees).
func (a *App) Get(ctx context.Context, ref string) (*TaskView, error) {
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return nil, err
	}
	sessions, err := a.Store.ListSessions(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	notes, err := a.Store.GetNotes(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	v := &TaskView{Task: *t, Sessions: sessions, Notes: notes, RepoStatus: make([]RepoStatus, 0, len(t.Repos))}
	for _, r := range t.Repos {
		rs := RepoStatus{Name: r.Name, Mode: r.Mode, Path: a.RepoPathFor(t, r), BaseBranch: r.BaseBranch}
		if r.Mode == model.ModeRW {
			if _, err := a.checkWorktree(ctx, t, r); errors.Is(err, os.ErrNotExist) {
				rs.Err = "missing"
			} else if err != nil {
				rs.Err = err.Error()
			} else if rs.Dirty, err = gitx.IsDirty(ctx, rs.Path); err != nil {
				rs.Err = err.Error()
			}
		}
		v.RepoStatus = append(v.RepoStatus, rs)
	}
	return v, nil
}

// SetStatus changes a task's status. Between open statuses, between closed
// ones (done ↔ cancelled) and from open to closed it changes nothing else
// (closing with cleanup is Done/Cancel). From closed to open it reopens the
// task (see reopen). It decides under the per-task lock, as done, cancel
// and reopen hold it: an open status set while done runs would otherwise
// mark the task it just closed open, without its worktrees.
func (a *App) SetStatus(ctx context.Context, ref string, st model.Status) error {
	if !st.Valid() {
		return fmt.Errorf("invalid status %q", st)
	}
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return err
	}
	unlock, err := a.lockTask(ctx, t.ID)
	if err != nil {
		return err
	}
	defer unlock()
	if t, err = a.Store.GetTask(ctx, t.ID); err != nil {
		return err
	}
	if t.Status.Open() && !st.Open() {
		// Closing it: no session is launched into it (see lockLaunches).
		unlockLaunches, err := a.lockLaunches(ctx, t.ID)
		if err != nil {
			return err
		}
		defer unlockLaunches()
	}
	if t.Status.Open() || !st.Open() {
		return a.Store.SetTaskStatus(ctx, t.ID, st)
	}
	return a.reopen(ctx, t, st)
}

// reopen brings closed task t back to the state a new task has, and then
// sets its status to the open status to; the caller holds the per-task
// lock (SetStatus). For each rw repo it makes the worktree on the task branch
// (buildTaskDir): on the local branch if the canonical clone still has it
// (done/cancel keep a branch with unpushed commits); else, after a fetch,
// on a new one from origin/<branch> if origin has it; else on a new one
// from the task's base (as NewTask and AddRepo do, see resolveBase), with a
// warning that the task's earlier commits are not on it, and that base is
// recorded. A worktree that is still there is kept. ro repos need their
// canonical clone only. It also clears what is left of the task's sandbox
// (see removeSandbox), writes CLAUDE.md and the settings and runs the
// project's setup in each new worktree (a failure is a warning). If a step
// fails, what this call made is undone, the task stays closed and the error
// says what failed.
func (a *App) reopen(ctx context.Context, t *model.Task, to model.Status) error {
	if err := outsideSandbox("reopen a task"); err != nil {
		return err
	}
	fail := func(err error) error {
		return fmt.Errorf("reopen task %s: %w (it stays %s)", t.Slug, err, t.Status)
	}
	if t.Branch == "" {
		t.Branch = "atl/" + t.Slug
	}
	for _, r := range t.Repos {
		if err := checkRepo(ctx, r.Name, a.Paths.RepoPath(r.Name)); err != nil {
			return fail(err)
		}
	}
	plan, rebased, err := a.reopenPlan(ctx, t)
	if err != nil {
		return fail(err)
	}
	// What closing the task could not remove (Docker did not answer, say):
	// a container left from before would have the old task dir mounted.
	if a.Sandbox != nil || fileExists(a.Paths.RunDir(t.Slug)) {
		a.removeSandbox(ctx, t)
	}
	// A worktree removed by hand (the task closed with `atl status`, say)
	// is still registered in git, which refuses to add it again until it
	// is pruned. Only a path still missing is pruned, and only now that
	// nothing of the task runs to put something there.
	for _, c := range plan {
		if c.Keep {
			continue
		}
		wt := a.Paths.WorktreePath(t.Slug, c.Repo.Name)
		if _, err := os.Lstat(wt); !errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := gitx.RemoveWorktree(ctx, a.Paths.RepoPath(c.Repo.Name), wt, false); err != nil {
			return fail(fmt.Errorf("repo %s: prune worktrees: %w", c.Repo.Name, err))
		}
	}
	undo, err := a.buildTaskDir(ctx, t, plan, true)
	if err != nil {
		return fail(err)
	}
	made := map[string]bool{}
	for _, c := range plan {
		made[c.Repo.Name] = !c.Keep
	}
	if err := a.runSetup(ctx, t, func(r model.TaskRepo) bool { return made[r.Name] }); err != nil {
		a.warnf("setup for task %s failed (task reopened): %v", t.Slug, err)
	}
	if err := a.Store.ReopenTask(ctx, t.ID, to, rebased); err != nil {
		undo()
		return fail(err)
	}
	return nil
}

// reopenPlan decides how reopen puts back the worktree of each rw repo of
// closed task t (see reopen). For a repo that starts again from the task's
// base it sets the new base in t.Repos and returns the repo in rebased.
func (a *App) reopenPlan(ctx context.Context, t *model.Task) (plan []rwCheckout, rebased []model.TaskRepo, err error) {
	for i := range t.Repos {
		r := &t.Repos[i]
		if r.Mode != model.ModeRW {
			continue
		}
		repo := a.Paths.RepoPath(r.Name)
		wt := a.Paths.WorktreePath(t.Slug, r.Name)
		if _, err := os.Lstat(wt); err == nil {
			// Closed with its worktree in place (`atl status`, or one that
			// could not be removed): kept if it is one of the repo's clone.
			if err := gitx.CheckWorktree(ctx, wt, repo); err != nil {
				return nil, nil, fmt.Errorf("repo %s: %w; remove it first", r.Name, err)
			}
			plan = append(plan, rwCheckout{Repo: *r, Keep: true})
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("repo %s: %w", r.Name, err)
		}
		local, err := gitx.RefExists(ctx, repo, "refs/heads/"+t.Branch)
		if err != nil {
			return nil, nil, fmt.Errorf("repo %s: %w", r.Name, err)
		}
		if local {
			plan = append(plan, rwCheckout{Repo: *r, Existing: true})
			continue
		}
		fmt.Fprintf(a.out(), "fetching %s...\n", r.Name)
		if err := a.fetchOrigin(ctx, r.Name, repo); err != nil {
			a.warnf("repo %s: fetch: %v; going by origin's branches as last fetched", r.Name, err)
		}
		remote, err := gitx.RefExists(ctx, repo, "refs/remotes/origin/"+t.Branch)
		if err != nil {
			return nil, nil, fmt.Errorf("repo %s: %w", r.Name, err)
		}
		if remote {
			tip, err := gitx.RevParse(ctx, repo, "refs/remotes/origin/"+t.Branch)
			if err != nil {
				return nil, nil, fmt.Errorf("repo %s: %w", r.Name, err)
			}
			fmt.Fprintf(a.out(), "repo %s: branch %s is gone here; made again from origin/%s\n", r.Name, t.Branch, t.Branch)
			plan = append(plan, rwCheckout{Repo: *r, Start: tip})
			continue
		}
		if r.BaseBranch, r.BaseCommit, err = resolveBase(ctx, r.Name, repo, t.Base); err != nil {
			return nil, nil, err
		}
		a.warnf("repo %s: branch %s is gone, here and on origin; it starts again from origin/%s, without the task's earlier commits",
			r.Name, t.Branch, r.BaseBranch)
		plan = append(plan, rwCheckout{Repo: *r, Start: r.BaseCommit})
		rebased = append(rebased, *r)
	}
	return plan, rebased, nil
}

// Rename changes a task's title (any status) and returns the updated task.
// The title is trimmed and its space runs collapsed; an empty title or one
// with control characters is rejected. Only the title changes: the slug,
// branch atl/<slug>, tmux session and dirs keep the original name. An open
// task's generated CLAUDE.md (if present) is rewritten with the new title;
// failing that is a warning, the rename stands.
func (a *App) Rename(ctx context.Context, ref, title string) (*model.Task, error) {
	title, err := model.NormalizeTitle(title)
	if err != nil {
		return nil, err
	}
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := a.Store.UpdateTaskTitle(ctx, t.ID, title); err != nil {
		return nil, err
	}
	if nt, err := a.Store.GetTask(ctx, t.ID); err == nil {
		t = nt
	} else {
		t.Title = title
	}
	if t.Status.Open() {
		if err := a.rewriteClaudeMD(ctx, t.ID); err != nil {
			a.warnf("task %s renamed but CLAUDE.md not updated: %v", t.Slug, err)
		}
	}
	return t, nil
}

// rewriteClaudeMD regenerates an open task's CLAUDE.md from the DB if the
// file exists (it is never created here: done/cancel remove it). It holds
// the per-task lock so it cannot interleave with AddRepo's rewrite or with
// done/cancel (which close the task under the lock, so a rewrite waiting
// for it then sees the task closed). It never creates the task dir either:
// a task dir that is gone is an error, which Rename reports as a warning.
func (a *App) rewriteClaudeMD(ctx context.Context, id int64) error {
	unlock, err := a.lockTask(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	t, err := a.Store.GetTask(ctx, id)
	if err != nil {
		return err
	}
	if !t.Status.Open() {
		return nil
	}
	dir := a.Paths.TaskDir(t.Slug)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("task dir %s is gone", dir)
	} else if err != nil {
		return err
	}
	// Lstat: the task dir is the task's to write; RewriteClaudeMD replaces a
	// symlink there rather than writing through it.
	if _, err := os.Lstat(filepath.Join(dir, "CLAUDE.md")); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if beforeClaudeMDRewrite != nil {
		beforeClaudeMDRewrite(dir)
	}
	return gen.RewriteClaudeMD(dir, *t, a.repoInfos(t))
}

// beforeClaudeMDRewrite, if set (tests only), runs in rewriteClaudeMD
// between its checks and the write.
var beforeClaudeMDRewrite func(dir string)

// Done closes a task as done and returns it (with its new status); see
// finish.
func (a *App) Done(ctx context.Context, ref string, force bool) (*model.Task, error) {
	return a.finish(ctx, ref, force, model.StatusDone)
}

// Cancel closes a task as cancelled and returns it (with its new status);
// see finish.
func (a *App) Cancel(ctx context.Context, ref string, force bool) (*model.Task, error) {
	return a.finish(ctx, ref, force, model.StatusCancelled)
}

// finish closes an open task: refuses if a rw worktree's HEAD has commits
// no branch, remote-tracking ref or tag reaches (even with force) or has
// uncommitted changes (unless force), kills its tmux session, ends its
// sessions (if the close then fails, those it ended that have a Claude id are
// left stopped, resumable), runs teardown, removes its worktrees and
// generated files, deletes each task
// branch that has no unpushed commits (see cleanupBranch), removes the task
// dir if it is then empty and sets the status. Worktrees
// whose dir was deleted by hand are skipped by the dirty check but still
// removed (pruned) so their branch is no longer "used by worktree".
func (a *App) finish(ctx context.Context, ref string, force bool, to model.Status) (*model.Task, error) {
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return nil, err
	}
	// Under the per-task lock (held until the status is set) a concurrent
	// AddRepo cannot add a worktree this call would not remove; re-read
	// the task so its repo list is current. Nothing below takes the lock
	// again (flock is per open file: that would deadlock).
	unlock, err := a.lockTask(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// And no session is launched into it from here on (see lockLaunches).
	unlockLaunches, err := a.lockLaunches(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	defer unlockLaunches()
	if t, err = a.Store.GetTask(ctx, t.ID); err != nil {
		return nil, err
	}
	if err := a.finishTask(ctx, t, force, to); err != nil {
		return nil, err
	}
	t.Status = to
	return t, nil
}

func (a *App) finishTask(ctx context.Context, t *model.Task, force bool, to model.Status) error {
	if err := outsideSandbox("close a task"); err != nil {
		return err
	}
	if !t.Status.Open() {
		return fmt.Errorf("task %s is already %s", t.Slug, t.Status)
	}
	type wt struct {
		name, repo, path string
		missing          bool
	}
	var wts []wt
	for _, r := range t.Repos {
		if r.Mode != model.ModeRW {
			continue
		}
		// What is not a worktree of the repo's clone any more (a symlink
		// the task put there, say) is left alone: git would act on
		// whatever it names.
		p, cerr := a.checkWorktree(ctx, t, r)
		missing := errors.Is(cerr, os.ErrNotExist)
		if cerr != nil && !missing {
			if !force {
				return fmt.Errorf("task %s: repo %s: %w (use --force to close the task and leave it alone)", t.Slug, r.Name, cerr)
			}
			a.warnf("repo %s: %v; left alone", r.Name, cerr)
			continue
		}
		wts = append(wts, wt{r.Name, a.Paths.RepoPath(r.Name), p, missing})
	}
	// Commits only HEAD reaches (detached HEAD, stopped rebase) would be
	// lost with the worktree; --force never discards commits.
	var orphans []string
	for _, w := range wts {
		if w.missing {
			continue
		}
		n, err := gitx.OrphanCommits(ctx, w.path)
		if err != nil {
			if !force {
				return fmt.Errorf("task %s: repo %s: %w (use --force to remove anyway)", t.Slug, w.name, err)
			}
			a.warnf("repo %s: %v", w.name, err)
			continue
		}
		if n > 0 {
			orphans = append(orphans, fmt.Sprintf("%s: HEAD has %d commit(s) not on any branch", w.name, n))
		}
	}
	if len(orphans) > 0 {
		return fmt.Errorf("task %s: %s (detached HEAD or rebase in progress); put them on a branch first",
			t.Slug, strings.Join(orphans, "; "))
	}
	if !force {
		var dirty []string
		for _, w := range wts {
			if w.missing {
				continue
			}
			d, err := gitx.IsDirty(ctx, w.path)
			if err != nil {
				return fmt.Errorf("task %s: repo %s: %w (use --force to remove anyway)", t.Slug, w.name, err)
			}
			if d {
				dirty = append(dirty, w.name)
			}
		}
		if len(dirty) > 0 {
			return fmt.Errorf("task %s has uncommitted changes in %s; commit or stash them, or use --force to discard",
				t.Slug, strings.Join(dirty, ", "))
		}
	}

	if err := a.Tmux.KillSession(ctx, tmux.SessionName(t.Slug)); err != nil && !errors.Is(err, exec.ErrNotFound) {
		if !force {
			return fmt.Errorf("task %s: kill tmux session: %w", t.Slug, err)
		}
		a.warnf("kill tmux session %s: %v", tmux.SessionName(t.Slug), err)
	}
	sessions, err := a.Store.ListSessions(ctx, t.ID)
	if err != nil {
		return err
	}
	// The sessions this close ends. Should it fail, the task stays open:
	// those with a conversation to resume are set back to stopped (their
	// windows are gone), so atl resume / atl open relaunch them.
	var ended []model.Session
	closed := false
	defer func() {
		if closed {
			return
		}
		for _, s := range ended {
			if s.ClaudeSessionID == "" {
				continue
			}
			if _, err := a.Store.SetStateIf(context.WithoutCancel(ctx), s.ID, model.StateEnded, model.StateStopped, a.now()); err != nil {
				a.warnf("session %s: %v", sessionName(s), err)
			}
		}
	}()
	for _, s := range sessions {
		if s.State == model.StateEnded {
			continue
		}
		if err := a.Store.SetSessionState(ctx, s.ID, model.StateEnded, a.now()); err != nil {
			return err
		}
		ended = append(ended, s)
	}
	if err := a.runTeardown(ctx, t, force); err != nil {
		if !force {
			// The task stays open, and so does its container: what the
			// sessions just ended (their windows are gone) left running in
			// it goes now. Otherwise removeSandbox takes it with the
			// container.
			for _, s := range ended {
				a.killSessionProcs(ctx, t, s.ID)
			}
			return fmt.Errorf("task %s: teardown: %w (use --force to continue anyway)", t.Slug, err)
		}
		a.warnf("teardown for task %s: %v", t.Slug, err)
	}
	// Before the worktrees go: the task's containers may hold them mounted.
	a.removeSandbox(ctx, t)
	var removed []wt
	for _, w := range wts {
		// Again, now that nothing of the task runs any more: what it left
		// there last is what git would remove.
		if err := gitx.CheckWorktree(ctx, w.path, w.repo); err != nil && !errors.Is(err, os.ErrNotExist) {
			if !force {
				return fmt.Errorf("task %s: repo %s: %w (use --force to close the task and leave it alone)", t.Slug, w.name, err)
			}
			a.warnf("repo %s: %v; left alone", w.name, err)
			continue
		}
		if err := gitx.RemoveWorktree(ctx, w.repo, w.path, force); err != nil {
			if !force {
				// It was clean before teardown, so teardown left changes.
				if d, derr := gitx.IsDirty(ctx, w.path); derr == nil && d {
					return fmt.Errorf("task %s: repo %s: worktree has changes after teardown (%w); re-run with --force to discard them",
						t.Slug, w.name, err)
				}
				return fmt.Errorf("task %s: repo %s: %w", t.Slug, w.name, err)
			}
			a.warnf("remove worktree %s: %v", w.path, err)
			continue
		}
		removed = append(removed, w)
	}
	for _, w := range removed {
		a.cleanupBranch(ctx, w.name, w.repo, t.Branch)
	}
	dir := a.Paths.TaskDir(t.Slug)
	if err := os.Remove(filepath.Join(dir, "CLAUDE.md")); err != nil && !errors.Is(err, os.ErrNotExist) {
		a.warnf("%v", err)
	}
	if err := os.RemoveAll(filepath.Join(dir, ".claude")); err != nil {
		a.warnf("%v", err)
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
			a.warnf("left %s (not empty)", dir)
		} else {
			a.warnf("left %s: %v", dir, err)
		}
	}
	if err := a.Store.SetTaskStatus(ctx, t.ID, to); err != nil {
		return err
	}
	closed = true
	return nil
}

// beforeBranchDelete, if set (tests only), runs in cleanupBranch between the
// unique-commit check and the delete.
var beforeBranchDelete func(repo, branch string)

// cleanupBranch deletes the task branch in the canonical repo after its
// worktree was removed, but only when no commits on it would be lost: every
// commit on it must be reachable from another local branch or a
// remote-tracking ref (local refs only; nothing is fetched). Otherwise the
// branch is kept. The tip is read before the check and the delete is a
// compare-and-delete against it, so a branch that moved meanwhile is kept. A
// missing branch is ignored; failures are warnings, never errors.
func (a *App) cleanupBranch(ctx context.Context, name, repo, branch string) {
	ok, err := gitx.RefExists(ctx, repo, "refs/heads/"+branch)
	if err != nil {
		a.warnf("branch %s in %s: %v", branch, name, err)
		return
	}
	if !ok {
		return
	}
	tip, err := gitx.RevParse(ctx, repo, "refs/heads/"+branch)
	if err != nil {
		a.warnf("kept branch %s in %s: %v", branch, name, err)
		return
	}
	n, err := gitx.UniqueCommitsAt(ctx, repo, branch, tip)
	if err != nil {
		a.warnf("kept branch %s in %s: %v", branch, name, err)
		return
	}
	if n > 0 {
		fmt.Fprintf(a.out(), "kept branch %s in %s (%d unpushed commit(s))\n", branch, name, n)
		return
	}
	if beforeBranchDelete != nil {
		beforeBranchDelete(repo, branch)
	}
	if err := gitx.DeleteBranchIfAt(ctx, repo, branch, tip); err != nil {
		a.warnf("delete branch %s in %s: %v", branch, name, err)
		return
	}
	fmt.Fprintf(a.out(), "deleted branch %s in %s\n", branch, name)
}

// SetOpenStatus is SetStatus for the assistant's gate (atl status run for
// the assistant): it only moves an open task between open statuses. A
// closed status is refused (closing is the user's: atl done or cancel),
// and so is a task that is closed: reopening it removes its Docker objects
// and restarts its sandbox (see reopen). The check and the change are made
// under the per-task lock, as done, cancel and reopen hold it.
func (a *App) SetOpenStatus(ctx context.Context, ref string, st model.Status) error {
	if !st.Valid() {
		return fmt.Errorf("invalid status %q", st)
	}
	if !st.Open() {
		return fmt.Errorf("status %s closes the task, which the assistant does not do: "+
			"ask the user to close it in the atl TUI (d done, c cancel) or with atl done/cancel in a terminal", st)
	}
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return err
	}
	unlock, err := a.lockTask(ctx, t.ID)
	if err != nil {
		return err
	}
	defer unlock()
	if t, err = a.Store.GetTask(ctx, t.ID); err != nil {
		return err
	}
	if !t.Status.Open() {
		return fmt.Errorf("task %s is %s, and reopening it (which removes its containers, networks and volumes and starts its sandbox anew) "+
			"is not for the assistant: ask the user to run atl status %s %s in a terminal", t.Slug, t.Status, t.Slug, st)
	}
	return a.Store.SetTaskStatus(ctx, t.ID, st)
}
