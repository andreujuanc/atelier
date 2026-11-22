package app

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/andreujuanc/atelier/internal/sandbox"
)

// attachClient attaches a control-mode tmux client (which needs no
// terminal) to session, as the user's terminal would: a sandboxed window
// that waits for one then starts docker exec.
func (e *env) attachClient(t *testing.T, session string) {
	t.Helper()
	client := exec.Command("tmux", "-L", e.sock, "-C", "attach-session", "-t", "="+session)
	stdin, err := client.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); _ = client.Process.Kill(); _ = client.Wait() })
}

// assistantSandboxEnv is sandboxEnv in a workspace, with the assistant's
// gate starter recording its calls.
func assistantSandboxEnv(t *testing.T) (*env, *fakeDocker, *[][]string) {
	t.Helper()
	e, fd := sandboxEnv(t)
	e.app.Paths.Workspace = filepath.Dir(e.out)
	var gates [][]string
	e.app.StartAssistantGate = func(bin, ws, runDir, logPath string) error {
		gates = append(gates, []string{bin, ws, runDir, logPath})
		return nil
	}
	return e, fd, &gates
}

// TestSandboxAssistant: with the sandbox on the assistant runs in a
// container of its own, made from the workspace's task image, that sees its
// dir, its own Claude dir and its run dir, nothing else, and has no Docker;
// its gate is started for the workspace, and the window runs Claude there
// through docker exec with the token by name. It is reused while it runs,
// and replaced after a change once the assistant is closed.
func TestSandboxAssistant(t *testing.T) {
	e, fd, gates := assistantSandboxEnv(t)
	ctx := context.Background()
	p := e.app.Paths
	dir, claude, runDir := p.AssistantDir(), p.AssistantClaudeDir(), p.AssistantRunDir()
	userCfg := os.Getenv("CLAUDE_CONFIG_DIR")
	if err := os.MkdirAll(userCfg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userCfg, "CLAUDE.md"), []byte("my rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd, _, err := e.app.Assistant(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cmd.Args[len(cmd.Args)-3:], " "); got != "attach-session -t =atl_assistant" {
		t.Errorf("attach cmd = %v", cmd.Args)
	}
	e.attachClient(t, AssistantSession)
	if got := e.assistantArgv(t); len(got) != 0 {
		t.Errorf("claude argv = %q", got)
	}

	if want := [][]string{{e.app.AtlBin, p.Workspace, runDir, filepath.Join(p.StateDir, "gate-_assistant.log")}}; !slices.EqualFunc(*gates, want, slices.Equal) {
		t.Errorf("gates started: %q, want %q", *gates, want)
	}
	runs := fd.find("run")
	if len(runs) != 1 {
		t.Fatalf("docker run calls: %q", fd.calls)
	}
	run := strings.Join(runs[0], " ")
	id := sandbox.AssistantID(dir)
	for _, want := range []string{
		"--name atl-" + id + " ",
		"--label atelier.agent=" + id + " ",
		"--mount type=bind,src=" + dir + ",dst=" + dir + " ",
		"--mount type=bind,src=" + claude + ",dst=" + claude + " ",
		"--mount type=bind,src=" + runDir + ",dst=/run/atl,readonly ",
		"--env CLAUDE_CONFIG_DIR=" + claude + " ",
		"--env ATL_SANDBOX_ID=" + id + " ",
		"--env ATL_ASSISTANT=1 ",
		"--env HOME=" + os.Getenv("HOME") + " ",
		// What ATL_CONTEXT is for: the place atl runs in, which the assistant manages.
		"--env ATL_CONTEXT=rules of the shared box ",
		" task-image sleep infinity",
	} {
		if !strings.Contains(run, want) {
			t.Errorf("docker run lacks %q:\n%s", want, run)
		}
	}
	if n := strings.Count(run, "--mount "); n != 3 {
		t.Errorf("docker run has %d mounts, want 3:\n%s", n, run)
	}
	for _, never := range []string{"DOCKER_HOST", "docker.sock", p.Tasks, p.Repos, "rules of a task container", "GIT_",
		userCfg, testToken, "CLAUDE_CODE_OAUTH_TOKEN", "ATL_CONTAINER"} {
		if strings.Contains(run, never) {
			t.Errorf("docker run has %q:\n%s", never, run)
		}
	}

	// Claude in the container: the token by name, never on a command line.
	if env := readFile(t, filepath.Join(e.out, "env-")); !strings.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN="+testToken+"\n") ||
		!strings.Contains(env, "ATL_ASSISTANT=1\n") {
		t.Errorf("claude's environment:\n%s", env)
	}
	if wc := e.tmux(t, "display-message", "-p", "-t", "=atl_assistant:", "#{pane_start_command}"); !strings.Contains(wc, "atl-window attach docker exec") ||
		!strings.Contains(wc, "-e CLAUDE_CODE_OAUTH_TOKEN ") || !strings.Contains(wc, fakeContainerID) || strings.Contains(wc, testToken) {
		t.Errorf("window command: %s", wc)
	}
	// Its files: the guard runs the container's atl; CLAUDE.md says where it is.
	if s := readFile(t, filepath.Join(dir, ".claude", "settings.json")); !strings.Contains(s, "'/run/atl/bin/atl' hook AssistantGuard") {
		t.Errorf("settings:\n%s", s)
	}
	if md := readFile(t, filepath.Join(dir, "CLAUDE.md")); !strings.Contains(md, "container of your own") {
		t.Errorf("CLAUDE.md does not say it runs in a container:\n%s", md)
	}
	if _, err := os.Stat(filepath.Join(runDir, "bin", "atl")); err != nil {
		t.Errorf("atl not installed in the run dir: %v", err)
	}
	if got := readFile(t, filepath.Join(claude, "CLAUDE.md")); got != "my rules\n" {
		t.Errorf("Claude dir not seeded: %q", got)
	}
	if _, err := os.Stat(filepath.Join(claude, ".claude.json")); err != nil {
		t.Errorf("no state file: %v", err)
	}

	// Running: reused, gate checked again.
	if _, _, err := e.app.Assistant(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if n := len(fd.find("run")); n != 1 || len(*gates) != 2 {
		t.Errorf("second call: %d docker runs, %d gate starts", n, len(*gates))
	}
	// Another image: kept while the assistant runs, replaced once it is closed.
	e.app.Sandbox.Image = "other-image"
	if _, _, err := e.app.Assistant(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if n := len(fd.find("run")); n != 1 || !strings.Contains(e.app.Out.(*bytes.Buffer).String(), "the assistant's container predates a change") {
		t.Errorf("changed spec under a running assistant: %d docker runs; output %q", n, e.app.Out.(*bytes.Buffer).String())
	}
	e.tmux(t, "kill-session", "-t", "=atl_assistant")
	if _, _, err := e.app.Assistant(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if runs := fd.find("run"); len(runs) != 2 || !strings.Contains(strings.Join(runs[1], " "), " other-image sleep infinity") ||
		len(fd.find("rm")) != 1 {
		t.Errorf("not replaced: %q", fd.calls)
	}
}

// TestSandboxAssistantFails: without Docker, the default image or a token
// the assistant does not start, with one line, and nothing is created.
func TestSandboxAssistantFails(t *testing.T) {
	e, fd, gates := assistantSandboxEnv(t)
	ctx := context.Background()
	fd.down = true
	if _, _, err := e.app.Assistant(ctx, ""); err == nil || !strings.Contains(err.Error(), "cannot reach Docker") || strings.Contains(err.Error(), "\n") {
		t.Errorf("Docker down: %v", err)
	}
	fd.down = false
	e.app.Sandbox.Image = sandbox.DefaultImage
	fd.missing = map[string]bool{sandbox.DefaultImage: true}
	if _, _, err := e.app.Assistant(ctx, ""); err == nil || !strings.Contains(err.Error(), "is not on the Docker daemon") ||
		!strings.Contains(err.Error(), sandbox.BuildDefaultImage) {
		t.Errorf("image missing: %v", err)
	}
	fd.missing = nil
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	if _, _, err := e.app.Assistant(ctx, ""); err == nil || !strings.Contains(err.Error(), "atl token") {
		t.Errorf("no token: %v", err)
	}
	if n := len(fd.find("run")); n != 0 {
		t.Errorf("docker run calls: %d", n)
	}
	if e.app.Tmux.HasSession(ctx, AssistantSession) {
		t.Error("assistant session started")
	}
	if len(*gates) != 1 { // only the image check comes after the gate
		t.Errorf("gate starts: %q", *gates)
	}
}
