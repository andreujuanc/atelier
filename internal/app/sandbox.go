package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/andreujuanc/atelier/internal/gitx"
	"github.com/andreujuanc/atelier/internal/hook"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/sandbox"
	"github.com/andreujuanc/atelier/internal/tmux"
)

// hookBin is the atl path written into a task's hook commands: the running
// binary, or its copy inside the task container when tasks are sandboxed.
func (a *App) hookBin() string {
	if a.Sandbox != nil {
		return sandbox.InBin
	}
	return a.AtlBin
}

func (a *App) docker() sandbox.Runner {
	if a.Docker != nil {
		return a.Docker
	}
	return sandbox.Docker
}

func (a *App) sandboxID(t *model.Task) string {
	return sandbox.ID(t.Slug, a.Paths.TaskDir(t.Slug))
}

// outsideSandbox refuses what atl cannot do from inside a task container
// (there is no tmux server, no canonical clone and no Docker daemon there).
func outsideSandbox(what string) error {
	if sandbox.Inside() {
		return fmt.Errorf("cannot %s from inside a task container; run atl where atelier itself runs", what)
	}
	return nil
}

// pathsEnv is the environment that makes atl in the task container resolve
// the same paths as this one.
func (a *App) pathsEnv() map[string]string {
	env := map[string]string{"ATL_REPOS": a.Paths.Repos, "ATL_TASKS": a.Paths.Tasks}
	if a.Paths.Workspace != "" {
		env["ATL_WORKSPACE"] = a.Paths.Workspace
		return env
	}
	env["XDG_DATA_HOME"] = filepath.Dir(filepath.Dir(a.Paths.DB))
	env["XDG_STATE_HOME"] = filepath.Dir(a.Paths.StateDir)
	env["XDG_CONFIG_HOME"] = filepath.Dir(a.Paths.ConfigDir)
	return env
}

// sandboxSpec describes the task's container. It sees, at the same paths as
// here: the task dir; the git database of each rw repo's canonical clone
// (a worktree keeps its objects and refs there) and each ro repo, read-only;
// Claude Code's config dir (login, settings, transcripts); and what the atl
// hooks need (database, hook log, project config). The run dir, with the
// gate's socket and the atl binary, is at sandbox.RunMount. It does not see
// the host's Docker socket, the SSH agent, other repos or other tasks.
func (a *App) sandboxSpec(ctx context.Context, t *model.Task) (sandbox.Spec, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return sandbox.Spec{}, err
	}
	dir := a.Paths.TaskDir(t.Slug)
	id := a.sandboxID(t)
	image := a.Sandbox.Image
	if pr, err := a.taskProject(t); err != nil {
		a.warnf("%v; using the default task image", err)
	} else if pr != nil && pr.Image != "" {
		image = pr.Image
	}
	spec := sandbox.Spec{
		ID:      id,
		Image:   image,
		User:    fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		Workdir: dir,
		Env: map[string]string{
			"HOME":            home,
			"DOCKER_HOST":     "unix://" + sandbox.InSocket,
			"DOCKER_BUILDKIT": "0", // the gate does not carry BuildKit sessions
			sandbox.InEnv:     id,
			"ATL_CONTAINER":   sandbox.ContainerName(id),
		},
	}
	for k, v := range a.pathsEnv() {
		spec.Env[k] = v
	}
	for _, k := range []string{"LANG", "LC_ALL", "TZ"} {
		if v := os.Getenv(k); v != "" {
			spec.Env[k] = v
		}
	}
	// $ATL_CONTEXT describes the place atl runs in, not a task's own
	// container: sandboxed sessions get $ATL_SANDBOX_CONTEXT instead.
	if v := os.Getenv(hook.SandboxContextVar); v != "" {
		spec.Env["ATL_CONTEXT"] = v
	}
	// The user's git config stays outside (it may hold credentials); only
	// who they are goes in, so that commits made in the task are theirs.
	gitDir := dir
	for _, r := range t.Repos {
		if wt := a.Paths.WorktreePath(t.Slug, r.Name); r.Mode == model.ModeRW && fileExists(wt) {
			gitDir = wt // a repo may set its own identity
			break
		}
	}
	if name := gitx.ConfigValue(ctx, gitDir, "user.name"); name != "" {
		spec.Env["GIT_AUTHOR_NAME"], spec.Env["GIT_COMMITTER_NAME"] = name, name
	}
	if email := gitx.ConfigValue(ctx, gitDir, "user.email"); email != "" {
		spec.Env["GIT_AUTHOR_EMAIL"], spec.Env["GIT_COMMITTER_EMAIL"] = email, email
	}
	same := func(path string, ro bool) {
		spec.Mounts = append(spec.Mounts, sandbox.Mount{Src: path, Dst: path, RO: ro})
	}
	same(dir, false)
	for _, r := range t.Repos {
		repo := a.Paths.RepoPath(r.Name)
		if r.Mode == model.ModeRO {
			same(repo, true)
			continue
		}
		git := filepath.Join(repo, ".git")
		if fi, err := os.Stat(git); err != nil || !fi.IsDir() {
			a.warnf("repo %s: %s is not a directory; git will not work for it in the task container", r.Name, git)
			continue
		}
		same(git, false)
	}
	claude, err := sandboxClaudeDir()
	if err != nil {
		return sandbox.Spec{}, err
	}
	if err := os.MkdirAll(claude, 0o700); err != nil {
		return sandbox.Spec{}, err
	}
	same(claude, false)
	spec.Env["CLAUDE_CONFIG_DIR"] = claude
	if err := os.MkdirAll(a.Paths.StateDir, 0o755); err != nil {
		return sandbox.Spec{}, err
	}
	same(filepath.Dir(a.Paths.DB), false)
	same(a.Paths.StateDir, false)
	if fileExists(a.Paths.ConfigDir) {
		same(a.Paths.ConfigDir, true)
	}
	spec.Mounts = append(spec.Mounts, sandbox.Mount{Src: a.Paths.RunDir(t.Slug), Dst: sandbox.RunMount, RO: true})
	return spec, nil
}

// sandboxClaudeDir returns Claude Code's config dir for task containers. It
// must come from $CLAUDE_CONFIG_DIR: only then does Claude Code keep its
// state file (.claude.json) inside the dir, where the container sees it;
// without the variable that file lives in the home directory.
func sandboxClaudeDir() (string, error) {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		return "", errors.New("the sandbox needs $CLAUDE_CONFIG_DIR (e.g. ~/.claude), so that Claude Code keeps all its state in one directory the task container can be given")
	}
	return filepath.Abs(dir)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ensureSandbox makes the task's gate and container run, creating them if
// needed. A container that predates a change to the task's repos is replaced
// when no session runs in it; otherwise it is kept and a warning says so.
func (a *App) ensureSandbox(ctx context.Context, t *model.Task) error {
	runDir := a.Paths.RunDir(t.Slug)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return err
	}
	// The run dir is named by a hash: say whose it is.
	if err := os.WriteFile(filepath.Join(runDir, "task"), []byte(t.Slug+"\n"), 0o644); err != nil {
		return err
	}
	if err := sandbox.InstallBin(a.AtlBin, runDir); err != nil {
		return fmt.Errorf("install atl for the task container: %w", err)
	}
	startGate := a.StartGate
	if startGate == nil {
		startGate = sandbox.Start
	}
	if err := startGate(a.AtlBin, t.Slug, runDir, a.gateLog(t)); err != nil {
		return err
	}
	spec, err := a.sandboxSpec(ctx, t)
	if err != nil {
		return err
	}
	busy := func() bool {
		live, err := a.hasLiveWindow(ctx, t, tmux.SessionName(t.Slug))
		return err != nil || live
	}
	stale, err := sandbox.Ensure(ctx, a.docker(), spec, busy)
	if err != nil {
		return fmt.Errorf("task container (image %s): %w", spec.Image, err)
	}
	if stale {
		a.warnf("task %s: its container predates a change (repos, image or atl paths); it is replaced once the task has no running sessions", t.Slug)
	}
	return nil
}

func (a *App) gateLog(t *model.Task) string {
	return filepath.Join(a.Paths.StateDir, "gate-"+t.Slug+".log")
}

// removeSandbox removes the task's container, everything the task created
// through its gate, the gate and the run dir. It runs for any task that has
// a run dir, so a task started while the sandbox was on is cleaned up after
// it was switched off. Failures are warnings: they never keep a task open.
func (a *App) removeSandbox(ctx context.Context, t *model.Task) {
	runDir := a.Paths.RunDir(t.Slug)
	if a.Sandbox == nil && !fileExists(runDir) {
		return
	}
	if err := sandbox.Cleanup(ctx, a.docker(), a.sandboxID(t)); err != nil {
		a.warnf("task %s: clean up its containers: %v", t.Slug, err)
	}
	sandbox.Stop(runDir)
	if err := os.RemoveAll(runDir); err != nil {
		a.warnf("%v", err)
	}
	if err := os.Remove(a.gateLog(t)); err != nil && !errors.Is(err, os.ErrNotExist) {
		a.warnf("%v", err)
	}
}

// sandboxArgv wraps a Claude command line so that it runs in the task's
// container, passing on the window's environment (env's keys).
func (a *App) sandboxArgv(t *model.Task, dir string, env map[string]string, argv []string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return sandbox.ExecArgv(a.sandboxID(t), dir, keys, argv)
}
