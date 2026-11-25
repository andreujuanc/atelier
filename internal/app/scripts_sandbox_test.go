package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/sandbox"
)

// dockerExecStub stands in for the docker CLI that runs a project script
// in the task container (sandbox.ScriptArgs). It records its arguments,
// NUL-separated, in a file of its own in $1 of the template, then runs the
// command after the container here: in the -w dir, with only the -e
// variables and a PATH, as a container would, and in a session of its own
// (setsid -w), as dockerd's process is: a signal to the docker client's
// process group does not reach it.
const dockerExecStub = `#!/bin/sh
printf '%%s\0' "$@" > %q/exec-$$
[ "$1" = exec ] || exit 1
shift
w=/ ne=0
while [ $# -gt 0 ]; do
	case $1 in
	-u) shift 2 ;;
	-w) w=$2; shift 2 ;;
	-e) set -- "$@" "$2"; shift 2; ne=$((ne + 1)) ;;
	*) break ;;
	esac
done
shift # the container
i=$(($# - ne))
while [ $i -gt 0 ]; do
	set -- "$@" "$1"
	shift
	i=$((i - 1))
done
cd "$w" || exit 126
exec setsid -w env -i PATH=/usr/local/bin:/usr/bin:/bin "$@"
`

// scriptSandbox turns the sandbox on for e, with a fake Docker daemon for
// atl's own calls, no gate, and dockerExecStub as the docker on PATH. It
// returns the daemon and the dir where the stub records its calls.
func scriptSandbox(t *testing.T, e *taskEnv) (*fakeDocker, string) {
	t.Helper()
	bin, calls := filepath.Join(e.root, "bin"), filepath.Join(e.root, "execs")
	for _, d := range []string{bin, calls} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fmt.Sprintf(dockerExecStub, calls)), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	atl := filepath.Join(e.root, "atl-binary")
	if err := os.WriteFile(atl, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fd := &fakeDocker{}
	e.app.AtlBin = atl
	e.app.Sandbox = &sandbox.Config{Image: "task-image"}
	e.app.Docker = fd.run
	e.app.StartGate = func(string, string, int64, string, string, []string) error { return nil }
	return fd, calls
}

// scriptExecs returns the arguments of each docker call of the stub.
func scriptExecs(t *testing.T, dir string) [][]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "exec-*"))
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for _, f := range files {
		out = append(out, strings.Split(strings.TrimSuffix(readFile(t, f), "\x00"), "\x00"))
	}
	return out
}

// scriptProcs counts the live processes whose environment has
// ATL_SCRIPT=run.
func scriptProcs(run string) int {
	n := 0
	dirs, _ := filepath.Glob("/proc/[0-9]*")
	for _, d := range dirs {
		b, err := os.ReadFile(filepath.Join(d, "environ"))
		if err != nil || !slices.Contains(strings.Split(string(b), "\x00"), sandbox.ScriptVar+"="+run) {
			continue
		}
		if st, err := os.ReadFile(filepath.Join(d, "stat")); err == nil && !strings.Contains(string(st), ") Z ") {
			n++
		}
	}
	return n
}

// TestSandboxSetupRunsInTaskContainer: with the sandbox on, a project's
// setup runs in the task's container, which it starts first: docker exec
// as the user sessions run as, in the worktree (same path), with the
// project's env and the ATL_* variables but nothing of atl's own
// environment, its output streamed as before. A repo added later gets its
// setup there too, in a container that has the repo.
func TestSandboxSetupRunsInTaskContainer(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "lib", "main")
	e.addRepo(t, "docs", "main")
	fd, calls := scriptSandbox(t, e)
	t.Setenv("ATL_TEST_HOST_ONLY", "host-secret")
	t.Setenv("ATL_SESSION", "42") // atl run from a Claude window
	e.writeProject(t, "shop", `
setup = 'env > env.out; pwd > pwd.out; echo "hello from $ATL_REPO"'
[[repos]]
name = "api"
[[repos]]
name = "docs"
mode = "ro"
[env]
FOO = "bar"
ATL_PORT_OFFSET = "9999"
ATL_SCRIPT = "forged"
`)
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "boxed setup", Project: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if out := e.outString(); strings.Contains(out, "warning") || !strings.Contains(out, "setup api: hello from api\n") {
		t.Fatalf("output:\n%s", out)
	}
	if n := len(fd.find("run")); n != 1 {
		t.Fatalf("docker run calls = %d, want 1 (the task container, for the setup)", n)
	}
	wt := e.app.Paths.WorktreePath(task.Slug, "api")
	execs := scriptExecs(t, calls)
	if len(execs) != 1 {
		t.Fatalf("docker exec calls = %q, want 1", execs)
	}
	user := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	args := execs[0]
	if !slices.Equal(args[:5], []string{"exec", "-u", user, "-w", wt}) || !slices.Contains(args, fakeContainerID) {
		t.Errorf("docker %q", args)
	}
	if got := strings.TrimSpace(readFile(t, filepath.Join(wt, "pwd.out"))); got != wt {
		t.Errorf("setup ran in %s, want %s", got, wt)
	}
	env := readFile(t, filepath.Join(wt, "env.out"))
	for _, want := range []string{"FOO=bar", "ATL_REPO=api", "ATL_WORKTREE=" + wt, "ATL_TASK=" + task.Slug,
		"ATL_TASK_DIR=" + e.app.Paths.TaskDir(task.Slug), "ATL_BRANCH=atl/boxed-setup",
		"ATL_PORT_OFFSET=" + strconv.Itoa(task.PortOffset), "PATH=" + sandbox.RunMount + "/bin:"} {
		if !strings.Contains("\n"+env, "\n"+want) {
			t.Errorf("setup env lacks %s:\n%s", want, env)
		}
	}
	for _, never := range []string{"host-secret", "ATL_SESSION=", "ATL_SCRIPT=forged", e.root + "/bin"} {
		if strings.Contains(env, never) {
			t.Errorf("setup env has %q:\n%s", never, env)
		}
	}

	// atl add: the new repo's setup, in a container that has it.
	if _, err := e.app.AddRepo(ctx, task.Slug, RepoArg{Name: "lib", Mode: model.ModeRW}, true); err != nil {
		t.Fatal(err)
	}
	lib := e.app.Paths.WorktreePath(task.Slug, "lib")
	if got := strings.TrimSpace(readFile(t, filepath.Join(lib, "pwd.out"))); got != lib {
		t.Errorf("lib's setup ran in %s", got)
	}
	if n := len(scriptExecs(t, calls)); n != 2 {
		t.Errorf("docker exec calls after the add = %d, want 2", n)
	}
	runs := fd.find("run")
	if len(runs) != 2 || !strings.Contains(strings.Join(runs[1], " "), "src="+filepath.Join(e.app.Paths.RepoPath("lib"), ".git")) {
		t.Errorf("the container was not replaced by one with lib: %q", runs)
	}
}

// TestSandboxSetupNoContainer: a container that cannot be started makes
// the setup a warning, as a failed setup is; the task is kept.
func TestSandboxSetupNoContainer(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	fd, calls := scriptSandbox(t, e)
	fd.missing = map[string]bool{"task-image": true}
	e.writeProject(t, "shop", "setup = 'touch ran'\n[[repos]]\nname = \"api\"\n")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "no box", Project: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if out := e.outString(); !strings.Contains(out, "warning: setup failed in api: cannot start the task's container to run it in: task image task-image") {
		t.Fatalf("output:\n%s", out)
	}
	if exists(filepath.Join(e.app.Paths.WorktreePath(task.Slug, "api"), "ran")) || len(scriptExecs(t, calls)) != 0 {
		t.Fatal("the setup ran")
	}
}

// TestSandboxTeardownRunsInTaskContainer: with the sandbox on, a project's
// teardown runs in the task's container, before the task's Docker objects
// and gate go (removeSandbox). A container that is not there, or was made
// from another spec (here: another image), is started first; one that
// cannot be started makes the close fail with the cause, or with --force a
// warning, and the close goes on without the teardown. The exit status
// still decides, as on the host.
func TestSandboxTeardownRunsInTaskContainer(t *testing.T) {
	for _, c := range []struct {
		name  string
		label string // the container there: "" none, "old" another spec
	}{{"no container", ""}, {"another image", "old"}} {
		t.Run(c.name, func(t *testing.T) {
			e := newTaskEnv(t)
			ctx := context.Background()
			e.addRepo(t, "api", "main")
			marker := filepath.Join(t.TempDir(), "marker")
			e.writeProject(t, "shop", `
teardown = 'echo "down $ATL_REPO"; printf "%s %s\n" "$ATL_REPO" "$(cat README)" >> "$MARKER"'
[[repos]]
name = "api"
[env]
MARKER = "`+marker+`"
`)
			task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "boxed td", Project: "shop"})
			if err != nil {
				t.Fatal(err)
			}
			fd, calls := scriptSandbox(t, e)
			fd.label, fd.agent = c.label, e.app.sandboxID(task)
			var tornDown, listed bool
			fd.onPs = func() {
				if !listed {
					listed, tornDown = true, exists(marker)
				}
			}
			if _, err := e.app.Done(ctx, task.Slug, false); err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, marker); got != "api README\n" {
				t.Errorf("marker = %q", got)
			}
			if !listed || !tornDown {
				t.Errorf("the task's Docker objects were listed (%v) before the teardown ran", listed)
			}
			if !strings.Contains(e.outString(), "teardown api: down api\n") {
				t.Errorf("output:\n%s", e.outString())
			}
			if n := len(fd.find("run")); n != 1 {
				t.Errorf("docker run calls = %d, want 1", n)
			}
			if c.label == "old" && len(fd.find("rm")) < 2 { // the old one, then the cleanup's
				t.Errorf("the old container was not replaced: %q", fd.calls)
			}
			execs := scriptExecs(t, calls)
			if len(execs) != 1 || !slices.Contains(execs[0], fakeContainerID) {
				t.Errorf("docker exec calls = %q", execs)
			}
		})
	}

	t.Run("exit status", func(t *testing.T) {
		e := newTaskEnv(t)
		ctx := context.Background()
		e.addRepo(t, "api", "main")
		e.writeProject(t, "shop", "teardown = 'echo nope >&2; exit 4'\n[[repos]]\nname = \"api\"\n")
		task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "td fail", Project: "shop"})
		if err != nil {
			t.Fatal(err)
		}
		scriptSandbox(t, e)
		if _, err := e.app.Done(ctx, task.Slug, false); err == nil || !strings.Contains(err.Error(), "exit status 4") {
			t.Fatalf("Done = %v", err)
		}
		if !strings.Contains(e.outString(), "teardown api: nope\n") {
			t.Errorf("output:\n%s", e.outString())
		}
		if got, err := e.app.Done(ctx, task.Slug, true); err != nil || got.Status != model.StatusDone {
			t.Fatalf("forced Done = %v", err)
		}
		if !strings.Contains(e.outString(), "warning: teardown failed in api: script failed: exit status 4") {
			t.Errorf("output:\n%s", e.outString())
		}
	})

	t.Run("cannot start", func(t *testing.T) {
		e := newTaskEnv(t)
		ctx := context.Background()
		e.addRepo(t, "api", "main")
		e.writeProject(t, "shop", "teardown = 'touch ran'\n[[repos]]\nname = \"api\"\n")
		task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "td gone", Project: "shop"})
		if err != nil {
			t.Fatal(err)
		}
		wt := e.app.Paths.WorktreePath(task.Slug, "api")
		fd, calls := scriptSandbox(t, e)
		fd.down = true
		_, err = e.app.Done(ctx, task.Slug, false)
		if err == nil || !strings.Contains(err.Error(), "teardown: cannot start the task's container to run it in: cannot reach Docker") ||
			!strings.Contains(err.Error(), "--force") {
			t.Fatalf("Done = %v", err)
		}
		if st, _ := e.app.Store.GetTask(ctx, task.ID); st.Status != model.StatusActive || !exists(wt) {
			t.Fatalf("refused close: status %s, worktree there %v", st.Status, exists(wt))
		}
		if got, err := e.app.Done(ctx, task.Slug, true); err != nil || got.Status != model.StatusDone {
			t.Fatalf("forced Done = %v", err)
		}
		if !strings.Contains(e.outString(), "warning: teardown not run: cannot start the task's container") {
			t.Errorf("output:\n%s", e.outString())
		}
		if len(scriptExecs(t, calls)) != 0 {
			t.Error("the teardown ran")
		}
	})
}

// TestSandboxScriptInterrupt: an interrupted script (Ctrl-C, a cancelled
// ctx) ends in the container, children included: killing the docker client
// alone would leave them running there (the stub runs them out of its
// process group, as dockerd does).
func TestSandboxScriptInterrupt(t *testing.T) {
	e := newTaskEnv(t)
	e.addRepo(t, "api", "main")
	marker := filepath.Join(t.TempDir(), "started")
	e.writeProject(t, "shop", `
teardown = 'touch "$MARKER"; sleep 300 & sleep 300'
[[repos]]
name = "api"
[env]
MARKER = "`+marker+`"
`)
	task, err := e.app.NewTask(context.Background(), NewTaskOpts{Title: "td stop", Project: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	fd, _ := scriptSandbox(t, e)
	var kills [][]string
	fd.onExec = func(args []string) { // the clean-up runs here, where the stub's processes are
		if slices.Contains(args, "atl-kill-script") {
			kills = append(kills, args)
			if out, err := exec.Command("sh", append([]string{"-c"}, args[4:]...)...).CombinedOutput(); err != nil {
				t.Errorf("kill script: %v: %s", err, out)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.app.runTeardown(ctx, task, false) }()
	for deadline := time.Now().Add(20 * time.Second); !exists(marker) && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if !exists(marker) {
		t.Fatal("the teardown did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "interrupted") {
			t.Fatalf("runTeardown = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("runTeardown did not return after the cancel")
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if len(kills) != 1 {
		t.Fatalf("clean-ups in the container: %q", kills)
	}
	run := kills[0][len(kills[0])-4] // ... kill <run> 0 0 ATL_SCRIPT
	if !slices.Equal(kills[0], sandbox.KillScriptArgs(fakeContainerID, run)) {
		t.Errorf("clean-up %q", kills[0])
	}
	for deadline := time.Now().Add(10 * time.Second); scriptProcs(run) > 0 && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if n := scriptProcs(run); n != 0 {
		t.Errorf("%d processes of the script still run", n)
	}
}

// TestScriptsOnHostWithSandboxOff: with the sandbox off, scripts run here
// as before, with atl's environment, and Docker is not asked.
func TestScriptsOnHostWithSandboxOff(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	fd := &fakeDocker{}
	e.app.Docker = fd.run
	t.Setenv("ATL_TEST_HOST_ONLY", "from-host")
	e.writeProject(t, "shop", `setup = 'echo "$ATL_TEST_HOST_ONLY" > host.out'
[[repos]]
name = "api"
`)
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "on host", Project: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(e.app.Paths.WorktreePath(task.Slug, "api"), "host.out")); got != "from-host\n" {
		t.Errorf("host.out = %q", got)
	}
	if len(fd.calls) != 0 {
		t.Errorf("docker %q", fd.calls)
	}
}
