package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/sandbox"
)

// fakeDocker records docker CLI calls. No container exists until one is
// "run"; then inspect reports it running with the spec label it was given.
type fakeDocker struct {
	mu    sync.Mutex
	calls [][]string
	label string // atelier.spec of the existing container; "" → none exists
}

func (f *fakeDocker) run(_ context.Context, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	switch args[0] {
	case "inspect":
		if f.label == "" {
			return "", errors.New("docker inspect: Error: No such container: x")
		}
		return "true " + f.label, nil
	case "run":
		for i, a := range args {
			if a == "--label" && strings.HasPrefix(args[i+1], "atelier.spec=") {
				f.label = strings.TrimPrefix(args[i+1], "atelier.spec=")
			}
		}
	case "rm":
		f.label = ""
	}
	return "", nil
}

func (f *fakeDocker) find(verb string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if c[0] == verb {
			out = append(out, c)
		}
	}
	return out
}

// sandboxEnv is newEnv with the sandbox on: a fake docker daemon for atl's
// own calls, no gate process, and a stub `docker` on PATH whose `exec` runs
// the command after "atl-exec" locally, so the stub claude still records.
func sandboxEnv(t *testing.T) (*env, *fakeDocker) {
	t.Helper()
	e := newEnv(t)
	root := filepath.Dir(e.out)
	stub := "#!/bin/sh\nwhile [ $# -gt 0 ] && [ \"$1\" != atl-exec ]; do shift; done\nshift\nexec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(root, "bin", "docker"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	atl := filepath.Join(root, "atl-binary")
	if err := os.WriteFile(atl, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, ".claude"))
	t.Setenv(sandbox.InEnv, "")
	t.Setenv("ATL_CONTEXT", "rules of the shared box")
	t.Setenv("ATL_SANDBOX_CONTEXT", "rules of a task container")
	gitconfig := "[user]\n\tname = Lab User\n\temail = lab@example.com\n[credential]\n\thelper = store\n"
	if err := os.WriteFile(filepath.Join(root, ".gitconfig"), []byte(gitconfig), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, ".gitconfig")) // TestMain points it elsewhere
	fd := &fakeDocker{}
	e.app.AtlBin = atl
	e.app.Sandbox = &sandbox.Config{Image: "task-image"}
	e.app.Docker = fd.run
	e.app.StartGate = func(string, string, string, string) error { return nil }
	return e, fd
}

func TestSandboxSessionRunsInTaskContainer(t *testing.T) {
	e, fd := sandboxEnv(t)
	ctx := context.Background()
	p := e.app.Paths
	for _, d := range []string{filepath.Join(p.RepoPath("api"), ".git"), p.RepoPath("docs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	task := e.task(t, "boxed", model.StatusActive, true,
		model.TaskRepo{Name: "api", Mode: model.ModeRW}, model.TaskRepo{Name: "docs", Mode: model.ModeRO})
	dir := p.TaskDir("boxed")

	s, err := e.app.NewSession(ctx, "boxed")
	if err != nil {
		t.Fatal(err)
	}
	// Claude ran through `docker exec` with its usual arguments.
	if got, want := e.argv(t, s.ID), []string{"--add-dir", p.RepoPath("docs")}; !slices.Equal(got, want) {
		t.Errorf("claude argv = %q, want %q", got, want)
	}

	runs := fd.find("run")
	if len(runs) != 1 {
		t.Fatalf("docker run calls = %d, want 1: %q", len(runs), fd.calls)
	}
	run := strings.Join(runs[0], " ")
	id := sandbox.ID("boxed", dir)
	for _, want := range []string{
		"--name atl-" + id,
		"--label atelier.agent=" + id,
		"type=bind,src=" + dir + ",dst=" + dir + " ",
		"type=bind,src=" + filepath.Join(p.RepoPath("api"), ".git") + ",dst=" + filepath.Join(p.RepoPath("api"), ".git") + " ",
		"type=bind,src=" + p.RepoPath("docs") + ",dst=" + p.RepoPath("docs") + ",readonly",
		"type=bind,src=" + os.Getenv("CLAUDE_CONFIG_DIR") + ",",
		"type=bind,src=" + p.RunDir("boxed") + ",dst=/run/atl,readonly",
		"--env DOCKER_HOST=unix:///run/atl/docker.sock",
		"--env ATL_SANDBOX_ID=" + id,
		"--env ATL_TASKS=" + p.Tasks,
		// The container's own rules, not the shared box's; who commits, not the git config.
		"--env ATL_CONTEXT=rules of a task container",
		"--env GIT_AUTHOR_NAME=Lab User",
		"--env GIT_COMMITTER_EMAIL=lab@example.com",
		" task-image sleep infinity",
	} {
		if !strings.Contains(run, want) {
			t.Errorf("docker run lacks %q:\n%s", want, run)
		}
	}
	for _, never := range []string{"docker.sock,dst=/var/run", "--privileged", "ssh", "shared box", ".gitconfig", "credential"} {
		if strings.Contains(run, never) {
			t.Errorf("docker run has %q:\n%s", never, run)
		}
	}
	// Only the canonical clone's git database is mounted, not its files.
	if strings.Contains(run, "src="+p.RepoPath("api")+",") {
		t.Errorf("docker run mounts the whole rw clone:\n%s", run)
	}

	// Hooks call the atl inside the container, and its copy is in the run dir.
	settings, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(settings, []byte("'/run/atl/bin/atl' hook SessionStart")) {
		t.Errorf("settings.json does not call the container's atl:\n%s", settings)
	}
	if _, err := os.Stat(filepath.Join(p.RunDir("boxed"), "bin", "atl")); err != nil {
		t.Errorf("atl was not installed in the run dir: %v", err)
	}

	// A second session reuses the container.
	if _, err := e.app.NewSession(ctx, "boxed"); err != nil {
		t.Fatal(err)
	}
	if n := len(fd.find("run")); n != 1 {
		t.Errorf("docker run calls after a second session = %d, want 1", n)
	}

	// A changed repo list does not replace a container with live sessions.
	task.Repos = append(task.Repos, model.TaskRepo{Name: "extra", Mode: model.ModeRO})
	if err := os.MkdirAll(p.RepoPath("extra"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.app.ensureSandbox(ctx, task); err != nil {
		t.Fatal(err)
	}
	if n := len(fd.find("run")); n != 1 {
		t.Errorf("container replaced under live sessions: %d docker run calls", n)
	}
	if out := e.app.Out.(*bytes.Buffer).String(); !strings.Contains(out, "predates a change") {
		t.Errorf("no warning about the stale container: %q", out)
	}

	// Closing the task removes the container, what the task created, and the run dir.
	if _, err := e.app.Done(ctx, "boxed", true); err != nil {
		t.Fatal(err)
	}
	var cleanup []string
	for _, c := range fd.calls {
		cleanup = append(cleanup, strings.Join(c, " "))
	}
	all := strings.Join(cleanup, "\n")
	for _, want := range []string{
		"rm -f atl-" + id,
		"ps -aq --filter label=atelier.task=" + id,
		"network ls -q --filter label=atelier.task=" + id,
		"volume ls -q --filter label=atelier.task=" + id,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("done did not run docker %s:\n%s", want, all)
		}
	}
	if _, err := os.Stat(p.RunDir("boxed")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("run dir left behind: %v", err)
	}
}

func TestSandboxNeedsClaudeConfigDir(t *testing.T) {
	e, fd := sandboxEnv(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	e.task(t, "boxed", model.StatusActive, true)
	if _, err := e.app.NewSession(context.Background(), "boxed"); err == nil || !strings.Contains(err.Error(), "CLAUDE_CONFIG_DIR") {
		t.Fatalf("NewSession error = %v, want one naming $CLAUDE_CONFIG_DIR", err)
	}
	if len(fd.find("run")) != 0 {
		t.Error("a container was created without a Claude config dir")
	}
}

func TestInsideSandboxRefusesToLaunchAndRecover(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.task(t, "t1", model.StatusActive, true)
	s, err := e.app.NewSession(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	e.argv(t, s.ID)

	t.Setenv(sandbox.InEnv, "t1-abcdef")
	if _, err := e.app.NewSession(ctx, "t1"); err == nil || !strings.Contains(err.Error(), "task container") {
		t.Errorf("NewSession inside a task container: %v", err)
	}
	// atl in a task container sees no tmux windows: it must not conclude
	// that every session stopped.
	e.tmux(t, "kill-server")
	if n, err := e.app.Recover(ctx); n != 0 || err != nil {
		t.Errorf("Recover inside a task container = %d, %v", n, err)
	}
	if got := e.session(t, s.ID).State; got == model.StateStopped {
		t.Error("session marked stopped from inside a task container")
	}
}
