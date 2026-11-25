package project

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/andreujuanc/atelier/internal/model"
)

func envMap(t *testing.T, env []string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := m[k]; dup {
			t.Fatalf("duplicate key %s in %v", k, env)
		}
		m[k] = v
	}
	return m
}

func TestEnvFor(t *testing.T) {
	t.Setenv("ATL_TEST_INHERITED", "inherited")
	t.Setenv("ATL_TEST_OVERRIDE", "inherited")
	t.Setenv("ATL_PORT_OFFSET", "999")
	t.Setenv("ATL_SESSION", "42")
	pr := &Project{Env: map[string]string{
		"ATL_TEST_OVERRIDE": "project",
		"FOO":               "bar=baz",
		"ATL_REPO":          "project-repo",
	}}
	se := ScriptEnv{
		Task:    model.Task{Slug: "fix-it", Branch: "atl/fix-it", PortOffset: 200},
		TaskDir: "/tasks/fix-it", Repo: "api", Worktree: "/tasks/fix-it/api",
	}
	env := pr.EnvFor(se)
	if !sort.StringsAreSorted(env) {
		t.Fatalf("env not sorted: %v", env)
	}
	m := envMap(t, env)
	want := map[string]string{
		"ATL_TEST_INHERITED": "inherited",
		"ATL_TEST_OVERRIDE":  "project",
		"FOO":                "bar=baz",
		"ATL_TASK":           "fix-it",
		"ATL_TASK_DIR":       "/tasks/fix-it",
		"ATL_REPO":           "api",
		"ATL_WORKTREE":       "/tasks/fix-it/api",
		"ATL_BRANCH":         "atl/fix-it",
		"ATL_PORT_OFFSET":    "200",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
	if _, ok := m["ATL_SESSION"]; ok {
		t.Error("inherited ATL_SESSION must be dropped")
	}
	if m["PATH"] != os.Getenv("PATH") {
		t.Error("PATH not inherited")
	}
	if got := strings.Join(pr.EnvFor(se), "\n"); got != strings.Join(env, "\n") {
		t.Error("EnvFor is not deterministic")
	}

	// No project: inherited + ATL_* only.
	var none *Project
	m = envMap(t, none.EnvFor(se))
	if m["ATL_TEST_OVERRIDE"] != "inherited" || m["ATL_PORT_OFFSET"] != "200" || m["ATL_REPO"] != "api" {
		t.Fatalf("nil project env: %v", m)
	}
	if _, ok := m["FOO"]; ok {
		t.Fatal("nil project must not add project env")
	}

	// A project env can't reintroduce ATL_SESSION either.
	pr.Env["ATL_SESSION"] = "7"
	if _, ok := envMap(t, pr.EnvFor(se))["ATL_SESSION"]; ok {
		t.Fatal("project ATL_SESSION must be dropped")
	}
}

// TestScriptVars: what a script in the task container gets from atl is the
// project's env and the ATL_* variables only: nothing of atl's own
// environment, and no ATL_SESSION.
func TestScriptVars(t *testing.T) {
	t.Setenv("ATL_TEST_INHERITED", "inherited")
	t.Setenv("ATL_SESSION", "42")
	pr := &Project{Env: map[string]string{"FOO": "bar", "ATL_REPO": "project-repo", "ATL_SESSION": "7"}}
	se := ScriptEnv{
		Task:    model.Task{Slug: "fix-it", Branch: "atl/fix-it", PortOffset: 200},
		TaskDir: "/tasks/fix-it", Repo: "api", Worktree: "/tasks/fix-it/api",
	}
	want := map[string]string{"FOO": "bar", "ATL_TASK": "fix-it", "ATL_TASK_DIR": "/tasks/fix-it", "ATL_REPO": "api",
		"ATL_WORKTREE": "/tasks/fix-it/api", "ATL_BRANCH": "atl/fix-it", "ATL_PORT_OFFSET": "200"}
	if got := pr.ScriptVars(se); !maps.Equal(got, want) {
		t.Errorf("ScriptVars = %v, want %v", got, want)
	}
	var none *Project
	delete(want, "FOO")
	if got := none.ScriptVars(se); !maps.Equal(got, want) {
		t.Errorf("nil project: ScriptVars = %v, want %v", got, want)
	}
}

func TestRunScript(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	var out bytes.Buffer
	env := append(os.Environ(), "GREETING=hi")
	if err := RunScript(ctx, `echo "$GREETING out"; echo err >&2; pwd; touch made`, dir, env, &out); err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(dir)
	got := out.String()
	if !strings.Contains(got, "hi out\n") || !strings.Contains(got, "err\n") || !strings.Contains(got, real) {
		t.Fatalf("output %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "made")); err != nil {
		t.Fatal("script did not run in dir:", err)
	}

	out.Reset()
	err := RunScript(ctx, "echo before; exit 3", dir, nil, &out)
	var ee *exec.ExitError
	if err == nil || !strings.Contains(err.Error(), "exit status 3") || !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Fatalf("err = %v", err)
	}
	if out.String() != "before\n" {
		t.Fatalf("output %q", out.String())
	}

	for _, s := range []string{"", "  \n\t"} {
		if err := RunScript(ctx, s, filepath.Join(dir, "does-not-exist"), nil, &out); err != nil {
			t.Fatalf("blank script %q: %v", s, err)
		}
	}
	// nil writer is fine.
	if err := RunScript(ctx, "echo x", dir, nil, nil); err != nil {
		t.Fatal(err)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := RunScript(cctx, "sleep 5", dir, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

// alive reports whether pid exists and is not a zombie.
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true // no procfs: trust kill
	}
	// Format: pid (comm) state ...; comm may contain spaces or parens.
	s := string(b)
	if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
		return s[i+2] != 'Z'
	}
	return true
}

func TestRunScriptCancelKillsGroup(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunScript(ctx, "sleep 30 & echo $! > pid; wait", dir, nil, nil)
	}()

	var pid int
	for deadline := time.Now().Add(5 * time.Second); ; {
		b, err := os.ReadFile(pidfile)
		if s := strings.TrimSpace(string(b)); err == nil && s != "" {
			if pid, err = strconv.Atoi(s); err != nil {
				t.Fatalf("pidfile %q: %v", s, err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("script did not write its pidfile")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunScript did not return promptly after cancel")
	}
	for deadline := start.Add(2 * time.Second); alive(pid); {
		if time.Now().After(deadline) {
			t.Fatalf("background sleep %d survived cancellation", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A script that wants the terminal (ssh asking about a host key or for a
// passphrase on /dev/tty) fails at once instead of being stopped
// (SIGTTIN) for good in a background process group of atl's terminal,
// which hung atl new until Ctrl-C. The test runs RunScript in a copy of
// itself whose controlling terminal is a pty, as atl's is.
func TestRunScriptNoTerminal(t *testing.T) {
	if os.Getenv("ATL_TEST_TTY_HELPER") == "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var out bytes.Buffer
		err := RunScript(ctx, "read x < /dev/tty || exit 3; echo read $x", t.TempDir(), nil, &out)
		fmt.Printf("result: %v | %s\n", err, strings.TrimSpace(out.String()))
		return
	}
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	defer ptmx.Close()
	if err := unix.IoctlSetPointerInt(int(ptmx.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Skipf("unlockpt: %v", err)
	}
	n, err := unix.IoctlGetInt(int(ptmx.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Skipf("ptsname: %v", err)
	}
	pts, err := os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("open pts: %v", err)
	}
	defer pts.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunScriptNoTerminal$")
	cmd.Env = append(os.Environ(), "ATL_TEST_TTY_HELPER=1")
	cmd.Stdin = pts
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0} // the pts, as its stdin
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper: %v\n%s", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "result: script failed") || strings.Contains(got, "interrupted") {
		t.Fatalf("a script reading /dev/tty: %s", got)
	}
}
