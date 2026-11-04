package project

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
)

// ScriptEnv describes where a setup/teardown script runs.
type ScriptEnv struct {
	Task     model.Task
	TaskDir  string
	Repo     string // repo name
	Worktree string // the repo's worktree in the task dir
}

// AtlVars returns the atelier variables exported to scripts: ATL_TASK,
// ATL_TASK_DIR, ATL_REPO, ATL_WORKTREE, ATL_BRANCH and ATL_PORT_OFFSET.
func (e ScriptEnv) AtlVars() map[string]string {
	return map[string]string{
		"ATL_TASK":        e.Task.Slug,
		"ATL_TASK_DIR":    e.TaskDir,
		"ATL_REPO":        e.Repo,
		"ATL_WORKTREE":    e.Worktree,
		"ATL_BRANCH":      e.Task.Branch,
		"ATL_PORT_OFFSET": strconv.Itoa(e.Task.PortOffset),
	}
}

// EnvFor returns the environment for a script: os.Environ(), overridden by
// the project's env, overridden by the atelier variables (see AtlVars). An
// inherited ATL_SESSION (atl run from inside a Claude window) is dropped: it
// names a session, not the script's context. Keys are unique (last wins)
// and sorted. pr may be nil (task without a project).
func (pr *Project) EnvFor(e ScriptEnv) []string {
	m := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		m[k] = v
	}
	if pr != nil {
		for k, v := range pr.Env {
			m[k] = v
		}
	}
	// After the project env, so a project can't reintroduce it either.
	delete(m, "ATL_SESSION")
	for k, v := range e.AtlVars() {
		m[k] = v
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k + "=" + m[k]
	}
	return out
}

// scriptKillDelay is how long a cancelled script's process group has to
// exit after SIGTERM before it is sent SIGKILL. It is also how long a
// finished script waits for background processes holding its output open.
const scriptKillDelay = 5 * time.Second

// RunScript runs script with `sh -c` in dir with env (nil: inherit), stdin
// from /dev/null and stdout+stderr to out (nil: discarded). A blank script
// is a no-op. A non-zero exit yields an error naming the exit status. A
// background process left holding the output open does not block it for
// more than scriptKillDelay after the shell exits.
//
// The script runs in its own process group. Cancelling ctx sends SIGTERM to
// the whole group (so children such as `make` → `docker compose` are not
// orphaned); whatever is still alive scriptKillDelay later gets SIGKILL.
func RunScript(ctx context.Context, script, dir string, env []string, out io.Writer) error {
	if strings.TrimSpace(script) == "" {
		return nil
	}
	if out == nil {
		out = io.Discard
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var cancelledAt time.Time // set by Cancel; read after Run (Wait syncs with it)
	cmd.Cancel = func() error {
		cancelledAt = time.Now()
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
			return os.ErrProcessDone // the group is already gone
		}
		return nil
	}
	// After WaitDelay, Go SIGKILLs sh and stops waiting for the output;
	// killGroup below takes care of the rest of the group.
	cmd.WaitDelay = scriptKillDelay
	err := cmd.Run()
	if !cancelledAt.IsZero() {
		killGroup(cmd.Process.Pid, cancelledAt.Add(scriptKillDelay))
	}
	if err == nil || errors.Is(err, exec.ErrWaitDelay) {
		return nil
	}
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("script interrupted: %w", cerr)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("script failed: %w", err) // "exit status N" or "signal: ..."
	}
	return fmt.Errorf("run script: %w", err)
}

// killGroup waits until process group pgid is gone or deadline passes, then
// sends SIGKILL to whatever is left of it.
func killGroup(pgid int, deadline time.Time) {
	for time.Now().Before(deadline) {
		if syscall.Kill(-pgid, 0) != nil {
			return // no process left in the group
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}
