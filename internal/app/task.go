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
}

// TaskView is a task with its sessions and (from Get only) repo status.
type TaskView struct {
	Task       model.Task
	Sessions   []model.Session
	RepoStatus []RepoStatus
}

// RepoStatus describes one repo of a task on disk.
type RepoStatus struct {
	Name  string
	Mode  model.Mode
	Path  string // worktree (rw) or canonical repo (ro)
	Dirty bool   // rw only
	Err   string // "missing" if the worktree is gone, or the git error
}

// RepoPathFor returns where repo r of task t lives: its worktree (rw) or
// the canonical clone (ro).
func (a *App) RepoPathFor(t *model.Task, r model.TaskRepo) string {
	if r.Mode == model.ModeRO {
		return a.Paths.RepoPath(r.Name)
	}
	return a.Paths.WorktreePath(t.Slug, r.Name)
}

func (a *App) repoInfos(t *model.Task) []gen.RepoInfo {
	out := make([]gen.RepoInfo, 0, len(t.Repos))
	for _, r := range t.Repos {
		out = append(out, gen.RepoInfo{Name: r.Name, Mode: r.Mode, Path: a.RepoPathFor(t, r)})
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
		name = strings.TrimSpace(name)
		if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
			return fmt.Errorf("invalid repo name %q", name)
		}
		// The task dir also holds the generated CLAUDE.md and .claude/.
		if strings.EqualFold(name, ".claude") || strings.EqualFold(name, "CLAUDE.md") {
			return fmt.Errorf("invalid repo name %q (reserved in the task dir)", name)
		}
		if mode == "" {
			mode = model.ModeRW
		}
		if !mode.Valid() {
			return fmt.Errorf("repo %s: invalid mode %q (want rw or ro)", name, mode)
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
		for _, r := range pr.Repos {
			if err := add(r.Name, r.Mode); err != nil {
				return nil, err
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
// records origin's default branch and its commit, inserts the task row
// (allocating slug and port offset atomically), creates the task dir with a
// worktree per rw repo on branch atl/<slug> starting at that commit, and
// writes CLAUDE.md and .claude/settings.json. Any failure up to this point
// after the insert rolls everything back. Then, once the task exists, the
// project's setup script (if any) runs in each rw worktree; a setup failure
// is only reported as a warning and leaves the task in place.
func (a *App) NewTask(ctx context.Context, o NewTaskOpts) (*model.Task, error) {
	// The title is one line: collapse whitespace runs (newlines included).
	title := strings.Join(strings.Fields(o.Title), " ")
	if title == "" {
		return nil, errors.New("title is required")
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
	for i := range repos {
		r := &repos[i]
		if r.Mode != model.ModeRW {
			continue
		}
		path := a.Paths.RepoPath(r.Name)
		if !o.NoFetch {
			fmt.Fprintf(a.out(), "fetching %s...\n", r.Name)
			if err := gitx.Fetch(ctx, path); err != nil {
				return nil, fmt.Errorf("repo %s: %w", r.Name, err)
			}
		}
		if r.BaseBranch, err = gitx.DefaultBranch(ctx, path); err != nil {
			return nil, fmt.Errorf("repo %s: %w", r.Name, err)
		}
		if r.BaseCommit, err = gitx.RevParse(ctx, path, "refs/remotes/origin/"+r.BaseBranch); err != nil {
			return nil, fmt.Errorf("repo %s: %w", r.Name, err)
		}
	}

	status := model.StatusActive
	if o.Backlog {
		status = model.StatusBacklog
	}
	t := &model.Task{
		Slug:        model.Slugify(title),
		Title:       title,
		Description: strings.TrimSpace(o.Description),
		Status:      status,
		Project:     o.Project,
		Repos:       repos,
	}
	if err := a.Store.CreateTaskAutoPort(ctx, t); err != nil {
		return nil, err
	}
	if err := a.populateTaskDir(ctx, t); err != nil {
		return nil, err
	}
	if err := a.runSetup(ctx, t); err != nil {
		a.warnf("setup for task %s failed (task kept): %v", t.Slug, err)
	}
	return t, nil
}

// populateTaskDir creates the task dir, worktrees and generated files for a
// freshly inserted task. On failure it undoes everything, including the
// task row, and returns the original error.
func (a *App) populateTaskDir(ctx context.Context, t *model.Task) (err error) {
	dir := a.Paths.TaskDir(t.Slug)
	var created []model.TaskRepo // worktrees (and branches) this call made
	madeDir := false
	defer func() {
		if err == nil {
			return
		}
		bg := context.WithoutCancel(ctx)
		for i := len(created) - 1; i >= 0; i-- {
			r := created[i]
			repo := a.Paths.RepoPath(r.Name)
			if rerr := gitx.RemoveWorktree(bg, repo, a.Paths.WorktreePath(t.Slug, r.Name), true); rerr != nil {
				a.warnf("rollback: remove worktree %s: %v", r.Name, rerr)
			}
			if rerr := gitx.DeleteBranch(bg, repo, t.Branch); rerr != nil {
				a.warnf("rollback: delete branch %s in %s: %v", t.Branch, r.Name, rerr)
			}
		}
		if madeDir {
			if rerr := os.RemoveAll(dir); rerr != nil {
				a.warnf("rollback: remove %s: %v", dir, rerr)
			}
		}
		if rerr := a.Store.DeleteTask(bg, t.ID); rerr != nil {
			a.warnf("rollback: delete task row %s: %v", t.Slug, rerr)
		}
	}()

	if err := os.MkdirAll(a.Paths.Tasks, 0o755); err != nil {
		return fmt.Errorf("create tasks dir: %w", err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("task dir %s already exists; remove it or choose another title", dir)
		}
		return fmt.Errorf("create task dir: %w", err)
	}
	madeDir = true
	for _, r := range t.Repos {
		if r.Mode != model.ModeRW {
			continue
		}
		wt := a.Paths.WorktreePath(t.Slug, r.Name)
		repo := a.Paths.RepoPath(r.Name)
		_, berr := gitx.RevParse(ctx, repo, "refs/heads/"+t.Branch)
		branchExisted := berr == nil
		if err := gitx.AddWorktree(ctx, repo, wt, t.Branch, r.BaseCommit); err != nil {
			if !branchExisted {
				// git may have created the branch (and worktree) before
				// failing, e.g. in a post-checkout hook: undo them too.
				bg := context.WithoutCancel(ctx)
				_ = gitx.RemoveWorktree(bg, repo, wt, true)
				if _, rerr := gitx.RevParse(bg, repo, "refs/heads/"+t.Branch); rerr == nil {
					if derr := gitx.DeleteBranch(bg, repo, t.Branch); derr != nil {
						a.warnf("rollback: delete branch %s in %s: %v", t.Branch, r.Name, derr)
					}
				}
			}
			return fmt.Errorf("repo %s: %w", r.Name, err)
		}
		created = append(created, r)
	}
	if err := gen.WriteClaudeMD(dir, *t, a.repoInfos(t)); err != nil {
		return err
	}
	return gen.WriteSettings(dir, a.AtlBin)
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
	fmt.Fprintf(a.out(), "%s %s: $ %s\n", kind, r.Name, strings.TrimSpace(script))
	pw := &prefixWriter{w: a.out(), prefix: kind + " " + r.Name + ": "}
	err := project.RunScript(ctx, script, se.Worktree, pr.EnvFor(se), pw)
	pw.Flush()
	return err
}

// runSetup runs the project's setup script in each rw worktree. A failure
// in one repo is a warning; the remaining repos still run. The returned
// error is only for a project that cannot be loaded.
func (a *App) runSetup(ctx context.Context, t *model.Task) error {
	pr, err := a.taskProject(t)
	if err != nil || pr == nil || strings.TrimSpace(pr.Setup) == "" {
		return err
	}
	for _, r := range t.Repos {
		if r.Mode != model.ModeRW {
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
		if _, err := os.Stat(a.Paths.WorktreePath(t.Slug, r.Name)); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := a.runScript(ctx, pr, "teardown", pr.Teardown, t, r); err != nil {
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
	out := make([]TaskView, 0, len(tasks))
	for _, t := range tasks {
		sessions, err := a.Store.ListSessions(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, TaskView{Task: t, Sessions: sessions})
	}
	return out, nil
}

// Get returns one task with sessions and repo status (dirty check for rw
// worktrees).
func (a *App) Get(ctx context.Context, ref string) (*TaskView, error) {
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return nil, err
	}
	sessions, err := a.Store.ListSessions(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	v := &TaskView{Task: *t, Sessions: sessions, RepoStatus: make([]RepoStatus, 0, len(t.Repos))}
	for _, r := range t.Repos {
		rs := RepoStatus{Name: r.Name, Mode: r.Mode, Path: a.RepoPathFor(t, r)}
		if r.Mode == model.ModeRW {
			if _, err := os.Stat(rs.Path); errors.Is(err, os.ErrNotExist) {
				rs.Err = "missing"
			} else if rs.Dirty, err = gitx.IsDirty(ctx, rs.Path); err != nil {
				rs.Err = err.Error()
			}
		}
		v.RepoStatus = append(v.RepoStatus, rs)
	}
	return v, nil
}

// SetStatus changes a task's status with no other side effects.
func (a *App) SetStatus(ctx context.Context, ref string, st model.Status) error {
	if !st.Valid() {
		return fmt.Errorf("invalid status %q", st)
	}
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return err
	}
	return a.Store.SetTaskStatus(ctx, t.ID, st)
}

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

// finish closes an open task: refuses if a rw worktree has uncommitted
// changes (unless force), kills its tmux session, ends its sessions, runs
// teardown, removes its worktrees (branches are kept) and generated files,
// removes the task dir if it is then empty and sets the status. Worktrees
// whose dir was deleted by hand are skipped by the dirty check but still
// removed (pruned) so their branch is no longer "used by worktree".
func (a *App) finish(ctx context.Context, ref string, force bool, to model.Status) (*model.Task, error) {
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := a.finishTask(ctx, t, force, to); err != nil {
		return nil, err
	}
	t.Status = to
	return t, nil
}

func (a *App) finishTask(ctx context.Context, t *model.Task, force bool, to model.Status) error {
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
		p := a.Paths.WorktreePath(t.Slug, r.Name)
		_, serr := os.Stat(p)
		wts = append(wts, wt{r.Name, a.Paths.RepoPath(r.Name), p, errors.Is(serr, os.ErrNotExist)})
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
	for _, s := range sessions {
		if s.State == model.StateEnded {
			continue
		}
		if err := a.Store.SetSessionState(ctx, s.ID, model.StateEnded, a.now()); err != nil {
			return err
		}
	}
	if err := a.runTeardown(ctx, t, force); err != nil {
		if !force {
			return fmt.Errorf("task %s: teardown: %w (use --force to continue anyway)", t.Slug, err)
		}
		a.warnf("teardown for task %s: %v", t.Slug, err)
	}
	for _, w := range wts {
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
		}
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
	return a.Store.SetTaskStatus(ctx, t.ID, to)
}
