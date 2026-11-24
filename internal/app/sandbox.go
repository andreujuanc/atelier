package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

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

// sandboxSpec describes the task's container. It sees, at the same paths as
// here: the task dir; the git database of each rw repo's canonical clone
// (a worktree keeps its objects and refs there) and each ro repo, read-only;
// and the task's own Claude config dir (Paths.ClaudeDir, see
// seedClaudeDir), as $CLAUDE_CONFIG_DIR; and the host paths of
// $ATL_SANDBOX_MOUNTS (sandbox.ROMounts), read-only. The run dir, with the
// gate's sockets and the atl binary, is at sandbox.RunMount. It does not see the
// host's Docker socket, the SSH agent, other repos, other tasks, the user's
// Claude config dir (login, other sessions' transcripts), nor atelier's
// database, state or config dirs: atl in there (hooks, status line, notes,
// push, fetch) asks the task's gate, which answers for this task only (see
// hook.TaskServer). Claude Code's token is not part of the spec: it is
// passed to each session by name (see sandboxArgv), and the file `atl
// token` stores it in (Paths.ClaudeToken) is in no mount.
func (a *App) sandboxSpec(ctx context.Context, t *model.Task) (sandbox.Spec, error) {
	image, _ := a.taskImage(t)
	return a.sandboxSpecFor(ctx, t, image)
}

// sandboxSpecFor is sandboxSpec with the task image image (taskImage).
func (a *App) sandboxSpecFor(ctx context.Context, t *model.Task, image string) (sandbox.Spec, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return sandbox.Spec{}, err
	}
	dir := a.Paths.TaskDir(t.Slug)
	id := a.sandboxID(t)
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
		if r.Mode != model.ModeRW {
			continue
		}
		if wt, err := a.checkWorktree(ctx, t, r); err == nil {
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
	// With $CLAUDE_CONFIG_DIR set, Claude Code keeps its state file
	// (.claude.json) in that dir too, not in the home directory.
	claude := a.Paths.ClaudeDir(t.Slug)
	if err := os.MkdirAll(claude, 0o700); err != nil {
		return sandbox.Spec{}, err
	}
	same(claude, false)
	spec.Env["CLAUDE_CONFIG_DIR"] = claude
	spec.Mounts = append(spec.Mounts, sandbox.Mount{Src: a.Paths.RunDir(t.Slug), Dst: sandbox.RunMount, RO: true})
	// What the workspace gives every task, read-only ($ATL_SANDBOX_MOUNTS,
	// checked when atl started).
	for _, m := range a.Sandbox.Mounts {
		same(m, true)
	}
	return spec, nil
}

// taskImage returns the image of the task's container and where its name
// comes from: the task's project, else the sandbox setting
// ($ATL_SANDBOX_IMAGE or sandbox.DefaultImage).
func (a *App) taskImage(t *model.Task) (image, from string) {
	if pr, err := a.taskProject(t); err != nil {
		a.warnf("%v; using the default task image", err)
	} else if pr != nil && pr.Image != "" {
		return pr.Image, "project " + t.Project
	}
	if a.Sandbox.Image == sandbox.DefaultImage {
		return a.Sandbox.Image, "default"
	}
	return a.Sandbox.Image, "$ATL_SANDBOX_IMAGE"
}

// missingImage is the one line that says image is not on the Docker
// daemon: with the command that builds it if it is sandbox.DefaultImage.
func missingImage(image, from string) error {
	return errors.New("task image " + missingImageDetail(image, from))
}

func missingImageDetail(image, from string) string {
	if image == sandbox.DefaultImage {
		return image + " is not on the Docker daemon (each task's sessions run in a container made from it); " +
			"build it with " + sandbox.BuildImages + " (it builds the box image too), or only it with: " + sandbox.BuildDefaultImage
	}
	return image + " (from " + from + ") is not on the Docker daemon and could not be pulled; build or pull it"
}

// checkDocker returns the Docker daemon's version, or, when it does not
// answer, the one line that says what to do (dockerUnreachable).
func (a *App) checkDocker(ctx context.Context) (version string, err error) {
	version, err = a.docker()(ctx, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return "", dockerUnreachable(err)
	}
	return version, nil
}

// dockerUnreachable is the one line that says the sandbox cannot reach the
// Docker daemon, with docker's reason, and what to do. The sandbox is on
// without the user asking for it, so it says why Docker is needed, and it is
// the one place that names the way out, $ATL_SANDBOX=off.
func dockerUnreachable(err error) error {
	return fmt.Errorf("cannot reach Docker, which runs each task's sessions in the task's own container (%s): "+
		"start Docker or give atl access to it, or set ATL_SANDBOX=off to run sessions without a container",
		strings.Join(strings.Fields(err.Error()), " "))
}

// checkAtlBin refuses an atl binary that cannot run in a task container
// (it is copied there, see sandbox.InstallBin): one line that says how to
// build one that can.
func (a *App) checkAtlBin(path string) error {
	check := a.CheckBin
	if check == nil {
		check = sandbox.CheckStaticBin
	}
	if err := check(path); err != nil {
		return sandbox.StaticBinError(err)
	}
	return nil
}

// checkSandboxNeeds refuses, one line each and in this order, to start a
// container without a Docker daemon (without one, `atl token` would not
// help), an atl binary that runs in it (checkAtlBin) or a Claude Code token
// (claudeAuth).
func (a *App) checkSandboxNeeds(ctx context.Context) error {
	if _, err := a.checkDocker(ctx); err != nil {
		return err
	}
	if err := a.checkAtlBin(a.AtlBin); err != nil {
		return err
	}
	_, err := a.claudeAuth()
	return err
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ensureSandbox makes the task's gate and container run, creating them if
// needed, and returns the container's full ID. A container that predates a
// change to the task's repos is replaced when no session runs in it;
// otherwise it is kept and a warning says so. A container that has the
// task container's name but is not atl's own (sandbox.Agent) is an error,
// and left alone. A new container gets its Claude dir seeded first
// (seedClaudeDir). Without a Docker daemon (checked first: without one,
// `atl token` would not help), an atl binary that runs in the container (a
// static Linux one, see checkAtlBin) or a token for Claude Code
// (claudeAuth) nothing is started.
func (a *App) ensureSandbox(ctx context.Context, t *model.Task) (string, error) {
	if err := a.checkSandboxNeeds(ctx); err != nil {
		return "", err
	}
	runDir := a.Paths.RunDir(t.Slug)
	if err := sandbox.MakeRunDir(runDir); err != nil {
		return "", err
	}
	// The run dir is named by a hash: say whose it is.
	if err := os.WriteFile(filepath.Join(runDir, "task"), []byte(t.Slug+"\n"), 0o644); err != nil {
		return "", err
	}
	if err := sandbox.InstallBin(a.AtlBin, runDir); err != nil {
		return "", fmt.Errorf("install atl for the task container: %w", err)
	}
	// Before the container can reach the repos' config (see pinOrigin).
	if err := a.pinOrigins(ctx, t); err != nil {
		return "", err
	}
	startGate := a.StartGate
	if startGate == nil {
		startGate = sandbox.Start
	}
	if err := startGate(a.AtlBin, t.Slug, t.ID, runDir, a.gateLog(t), a.Sandbox.Mounts); err != nil {
		return "", err
	}
	image, from := a.taskImage(t)
	spec, err := a.sandboxSpecFor(ctx, t, image)
	if err != nil {
		return "", err
	}
	busy := func() bool {
		live, err := a.hasLiveWindow(ctx, t, tmux.SessionName(t.Slug))
		return err != nil || live
	}
	// A new container needs its image: the default one is never pulled (it
	// is built for the user), others docker may pull.
	fresh := func() error {
		if spec.Image == sandbox.DefaultImage {
			if missing, err := sandbox.ImageMissing(ctx, a.docker(), spec.Image); err == nil && missing {
				return missingImage(spec.Image, "default")
			}
		}
		return a.seedClaudeDir(t)
	}
	container, stale, err := sandbox.Ensure(ctx, a.docker(), spec, busy, fresh)
	if err != nil {
		if errors.Is(err, sandbox.ErrNotAgent) {
			return "", err
		}
		if missing, ierr := sandbox.ImageMissing(ctx, a.docker(), spec.Image); ierr == nil && missing {
			return "", missingImage(spec.Image, from)
		}
		return "", fmt.Errorf("task container (image %s): %w", spec.Image, err)
	}
	if stale {
		a.warnf("task %s: its container predates a change (repos, image, atl paths or $%s); it is replaced once the task has no running sessions",
			t.Slug, sandbox.MountsVar)
	}
	return container, nil
}

func (a *App) gateLog(t *model.Task) string {
	return filepath.Join(a.Paths.StateDir, "gate-"+t.Slug+".log")
}

// removeSandbox removes the task's container, everything the task created
// through its gate, the gate and the run dir, and empties the task's Claude
// dir but for its transcripts (pruneClaudeDir). It runs for any task that
// has a run dir, so a task started while the sandbox was on is cleaned up
// after it was switched off. Failures are warnings: they never keep a task
// open.
func (a *App) removeSandbox(ctx context.Context, t *model.Task) {
	runDir := a.Paths.RunDir(t.Slug)
	if a.Sandbox == nil && !fileExists(runDir) && !fileExists(a.Paths.ClaudeDir(t.Slug)) {
		return
	}
	// The container goes first: then nothing writes to the Claude dir.
	defer a.pruneClaudeDir(t)
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

// killSessionProcs ends what session id started in the task's container
// (sandbox.KillSessionArgs): its window is gone, and dockerd keeps what a
// dead docker exec client started running, so Claude would go on there with
// nobody attached, and a resume would run a second Claude on the same
// conversation and worktree. A container that is not there or not running
// has nothing to end; one that has the name but is not atl's own
// (sandbox.Agent) is not touched; other failures are warnings. Nothing the
// container prints is used.
func (a *App) killSessionProcs(ctx context.Context, t *model.Task, id int64) {
	if a.Sandbox == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	c, err := sandbox.Agent(ctx, a.docker(), a.sandboxID(t))
	if err == nil && c.Running {
		_, err = a.docker()(ctx, sandbox.KillSessionArgs(c.ID, id)...)
	}
	if err != nil && !noContainer(err) {
		a.warnf("task %s: end what session %d left running in its container: %v", t.Slug, id, err)
	}
}

// noContainer reports whether a docker exec failed because the container is
// not there or not running.
func noContainer(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no such container") || strings.Contains(s, "is not running")
}

// sandboxArgv wraps a Claude command line so that it runs in the task's
// container, passing on the window's environment (env's keys) and the
// variables named in auth (see sandboxAuth). All go by name only: docker
// exec takes their values from the window's environment, so a token is on
// no command line and not in the container's stored config, and a new one
// reaches the next session without a new container. But for env's
// tmux.PaneVars (a project's PATH, SHELL), which are the container's and
// which tmux would not give the window anyway (see sandboxWindowEnv): they
// go by value.
func (a *App) sandboxArgv(container, dir string, env, auth map[string]string, argv []string) []string {
	keys := make([]string, 0, len(env)+len(auth))
	for k, v := range env {
		if slices.Contains(tmux.PaneVars, k) {
			k += "=" + v
		}
		keys = append(keys, k)
	}
	for k := range auth {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return sandbox.ExecArgv(container, dir, keys, argv)
}

// sandboxWindowEnv is env without its tmux.PaneVars, for the window of a
// sandboxed session: what runs there (sh, tmux, docker) runs where atl runs,
// with atl's PATH and SHELL; sandboxArgv passes env's to the container.
func sandboxWindowEnv(env map[string]string) map[string]string {
	out := maps.Clone(env)
	for _, k := range tmux.PaneVars {
		delete(out, k)
	}
	return out
}
