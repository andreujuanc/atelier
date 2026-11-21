package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/gitx"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/sandbox"
)

// fakeDocker records docker CLI calls. No container exists until one is
// "run"; then inspect reports it running with the spec label it was given.
type fakeDocker struct {
	mu      sync.Mutex
	calls   [][]string
	label   string          // atelier.spec of the existing container; "" → none exists
	missing map[string]bool // images not on the daemon, which docker run cannot pull either
	down    bool            // the daemon cannot be reached
}

// errDockerDown is what the docker CLI says when the daemon is not there.
var errDockerDown = errors.New("docker version: Cannot connect to the Docker daemon at unix:///var/run/docker.sock.\n" +
	"Is the docker daemon running?")

func (f *fakeDocker) run(_ context.Context, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	if f.down {
		return "", errDockerDown
	}
	switch args[0] {
	case "version":
		return "29.0.0", nil
	case "inspect":
		if f.label == "" {
			return "", errors.New("docker inspect: Error: No such container: x")
		}
		return "true " + f.label, nil
	case "image":
		if image := args[len(args)-1]; f.missing[image] {
			return "", errors.New("docker image: Error response from daemon: No such image: " + image + ":latest")
		}
		return "sha256:0123", nil
	case "run":
		if image := args[len(args)-3]; f.missing[image] { // image sleep infinity
			return "", errors.New("docker run: Unable to find image '" + image + ":latest' locally\n" +
				"docker: Error response from daemon: pull access denied for " + image + ", repository does not exist")
		}
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

// changes returns the calls other than the reachability check (docker
// version), which starts nothing.
func (f *fakeDocker) changes() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if c[0] != "version" {
			out = append(out, c)
		}
	}
	return out
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

// testToken is the made-up Claude Code token of sandboxEnv.
const testToken = "sk-ant-oat01-test-token-made-up-1234567890"

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
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, ".claude")) // the user's, never a task's
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", testToken)
	t.Setenv("ANTHROPIC_API_KEY", "")
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
	e.app.StartGate = func(string, string, int64, string, string) error { return nil }
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
		// The task's own Claude dir, at the same path, and none other.
		"type=bind,src=" + p.ClaudeDir("boxed") + ",dst=" + p.ClaudeDir("boxed") + " ",
		"--env CLAUDE_CONFIG_DIR=" + p.ClaudeDir("boxed") + " ",
		"type=bind,src=" + p.RunDir("boxed") + ",dst=/run/atl,readonly",
		"--env DOCKER_HOST=unix:///run/atl/docker.sock",
		"--env ATL_SANDBOX_ID=" + id,
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
	for _, never := range []string{"docker.sock,dst=/var/run", "--privileged", "ssh", "shared box", ".gitconfig", "credential",
		os.Getenv("CLAUDE_CONFIG_DIR"), "CLAUDE_CODE_OAUTH_TOKEN", testToken} {
		if strings.Contains(run, never) {
			t.Errorf("docker run has %q:\n%s", never, run)
		}
	}
	// Only the canonical clone's git database is mounted, not its files.
	if strings.Contains(run, "src="+p.RepoPath("api")+",") {
		t.Errorf("docker run mounts the whole rw clone:\n%s", run)
	}

	// The token reaches Claude by name: from the window's environment
	// (set in tmux without a command line), never in the window's command.
	if env := readFile(t, filepath.Join(e.out, "env-"+strconv.FormatInt(s.ID, 10))); !strings.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN="+testToken+"\n") {
		t.Errorf("claude did not get the token:\n%s", env)
	}
	if cmd := e.tmux(t, "display-message", "-p", "-t", s.TmuxTarget, "#{pane_start_command}"); !strings.Contains(cmd, "-e CLAUDE_CODE_OAUTH_TOKEN ") ||
		strings.Contains(cmd, testToken) {
		t.Errorf("window command: %s", cmd)
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

	// Closing the task removes the container, what the task created, and the
	// run dir; of the Claude dir only the transcripts stay (atl show and atl
	// transcript read them for closed tasks).
	claude := p.ClaudeDir("boxed")
	transcript := filepath.Join(claude, "projects", "k", "t.jsonl")
	for _, f := range []string{transcript, filepath.Join(claude, "todos", "x.json")} {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	evil := filepath.Join(claude, "projects", "k", "evil.jsonl") // planted by the task
	if err := os.Symlink("/etc/passwd", evil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.Done(ctx, "boxed", true); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(claude); err != nil || len(entries) != 1 || entries[0].Name() != "projects" || !exists(transcript) {
		t.Errorf("Claude dir after done: %v, %v", entries, err)
	}
	if _, err := os.Lstat(evil); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a symlink in projects/ survived done: %v", err)
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

// TestSandboxPinsOrigins: the task's repos get their origin pinned before
// its container starts, in a place none of the container's mounts reaches;
// a repo without origin gets no pin and does not stop the sandbox.
func TestSandboxPinsOrigins(t *testing.T) {
	e, fd := sandboxEnv(t)
	ctx := context.Background()
	p := e.app.Paths
	root := filepath.Dir(e.out)
	for _, name := range []string{"api", "docs"} {
		origin := filepath.Join(root, "origins", name+".git")
		git(t, root, "init", "-q", "--bare", origin)
		git(t, root, "clone", "-q", origin, p.RepoPath(name))
	}
	git(t, root, "init", "-q", p.RepoPath("local"))
	task := e.task(t, "boxed", model.StatusActive, true, model.TaskRepo{Name: "api", Mode: model.ModeRW},
		model.TaskRepo{Name: "local", Mode: model.ModeRW}, model.TaskRepo{Name: "docs", Mode: model.ModeRO})
	pinnedAtRun := false
	e.app.Docker = func(ctx context.Context, args ...string) (string, error) {
		if args[0] == "run" {
			pinnedAtRun = exists(p.OriginPin("api")) && exists(p.OriginPin("docs"))
		}
		return fd.run(ctx, args...)
	}
	if err := e.app.ensureSandbox(ctx, task); err != nil {
		t.Fatal(err)
	}
	if len(fd.find("run")) != 1 || !pinnedAtRun {
		t.Errorf("container started %d times, origins pinned before: %v", len(fd.find("run")), pinnedAtRun)
	}
	for _, name := range []string{"api", "docs"} {
		var o gitx.Origin
		if err := json.Unmarshal([]byte(readFile(t, p.OriginPin(name))), &o); err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(root, "origins", name+".git"); !o.Equal(gitx.Origin{URLs: []string{want}}) {
			t.Errorf("%s pin = %+v, want %s", name, o, want)
		}
	}
	if exists(p.OriginPin("local")) {
		t.Error("a repo without origin was pinned")
	}

	// No mount of the container reaches the pins, with or without a workspace.
	for _, paths := range []config.Paths{p, {
		Workspace: filepath.Join(root, "ws"), Repos: p.Repos, Tasks: filepath.Join(root, "ws", "tasks"),
		DB: filepath.Join(root, "ws", "data", "atelier.db"), StateDir: filepath.Join(root, "ws", "state"),
		ConfigDir: filepath.Join(root, "ws", "config"),
	}} {
		e.app.Paths = paths
		spec, err := e.app.sandboxSpec(ctx, task)
		if err != nil {
			t.Fatal(err)
		}
		pin := paths.OriginPin("api")
		for _, m := range spec.Mounts {
			for _, private := range []string{pin, paths.RefusalLog()} {
				if rel, err := filepath.Rel(m.Src, private); err == nil && !strings.HasPrefix(rel, "..") {
					t.Errorf("mount %s reaches %s", m.Src, private)
				}
			}
		}
	}
}

// TestSandboxSpecHidesAtelierDirs: a task container mounts none of
// atelier's database, state and config dirs (atl in it asks the gate), and
// gets no variable that only told atl in there where they are; with or
// without a workspace.
func TestSandboxSpecHidesAtelierDirs(t *testing.T) {
	e, _ := sandboxEnv(t)
	ctx := context.Background()
	p := e.app.Paths
	root := filepath.Dir(e.out)
	task := e.task(t, "boxed", model.StatusActive, true)
	ws := filepath.Join(root, "ws")
	for _, paths := range []config.Paths{p, {
		Workspace: ws, Repos: p.Repos, Tasks: filepath.Join(ws, "tasks"),
		DB: filepath.Join(ws, "data", "atelier.db"), StateDir: filepath.Join(ws, "state"),
		ConfigDir: filepath.Join(ws, "config"),
	}} {
		for _, d := range []string{filepath.Dir(paths.DB), paths.StateDir, filepath.Join(paths.ConfigDir, "projects"), paths.TaskDir("boxed")} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		e.app.Paths = paths
		spec, err := e.app.sandboxSpec(ctx, task)
		if err != nil {
			t.Fatal(err)
		}
		// inside reports whether p is dir or below it.
		inside := func(dir, p string) bool {
			rel, err := filepath.Rel(dir, p)
			return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
		}
		for _, m := range spec.Mounts {
			for _, h := range []string{filepath.Dir(paths.DB), paths.StateDir, paths.ConfigDir} {
				if inside(m.Src, h) || inside(h, m.Src) {
					t.Errorf("mount %s reaches or is in %s", m.Src, h)
				}
			}
			if paths.Workspace != "" && inside(paths.Workspace, m.Src) &&
				m.Src != paths.TaskDir("boxed") && m.Src != paths.RunDir("boxed") && m.Src != paths.ClaudeDir("boxed") {
				t.Errorf("mount %s is in the workspace", m.Src)
			}
		}
		for _, k := range []string{"ATL_WORKSPACE", "ATL_REPOS", "ATL_TASKS", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME"} {
			if v, ok := spec.Env[k]; ok {
				t.Errorf("container env has %s=%s", k, v)
			}
		}
	}
}

// TestSandboxClaudeToken: without a token nothing starts and the error says
// how to get one; an API key alone does; either goes by name only, and the
// value is in no docker argument, spec (nor its hash) or output.
func TestSandboxClaudeToken(t *testing.T) {
	e, fd := sandboxEnv(t)
	ctx := context.Background()
	task := e.task(t, "boxed", model.StatusActive, true)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	_, err := e.app.NewSession(ctx, "boxed")
	if err == nil || !strings.Contains(err.Error(), "atl token") || !strings.Contains(err.Error(), "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("NewSession without a token: %v", err)
	}
	if len(fd.changes()) != 0 || exists(e.app.Paths.RunDir("boxed")) {
		t.Errorf("started something without a token: %q", fd.calls)
	}

	const key = "sk-ant-api03-made-up-key-0123456789abcdefghij"
	t.Setenv("ANTHROPIC_API_KEY", key)
	s, err := e.app.NewSession(ctx, "boxed")
	if err != nil {
		t.Fatal(err)
	}
	e.argv(t, s.ID)
	if env := readFile(t, filepath.Join(e.out, "env-"+strconv.FormatInt(s.ID, 10))); !strings.Contains(env, "ANTHROPIC_API_KEY="+key+"\n") {
		t.Errorf("claude did not get the API key:\n%s", env)
	}
	// Claude Code asks once whether to use an API key (default no): it is
	// approved in the task's state file, by its last 20 characters.
	var st struct {
		Responses struct{ Approved []string } `json:"customApiKeyResponses"`
	}
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(e.app.Paths.ClaudeDir("boxed"), ".claude.json"))), &st); err != nil ||
		!slices.Equal(st.Responses.Approved, []string{key[len(key)-20:]}) {
		t.Errorf("API key approval = %+v, %v", st, err)
	}

	spec, err := e.app.sandboxSpec(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", testToken+"-rotated")
	t.Setenv("ANTHROPIC_API_KEY", "")
	spec2, err := e.app.sandboxSpec(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Hash() != spec2.Hash() {
		t.Error("the spec hash depends on the token")
	}
	// A new token reaches the next session without a new container.
	s2, err := e.app.NewSession(ctx, "boxed")
	if err != nil {
		t.Fatal(err)
	}
	e.argv(t, s2.ID)
	if env := readFile(t, filepath.Join(e.out, "env-"+strconv.FormatInt(s2.ID, 10))); !strings.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN="+testToken+"-rotated\n") {
		t.Errorf("claude did not get the new token:\n%s", env)
	}
	if n := len(fd.find("run")); n != 1 {
		t.Errorf("docker run calls = %d, want 1", n)
	}
	var all strings.Builder
	for _, c := range fd.calls {
		all.WriteString(strings.Join(c, " ") + "\n")
	}
	for _, sp := range []sandbox.Spec{spec, spec2} {
		all.WriteString(strings.Join(sp.RunArgs(), " ") + "\n")
	}
	all.WriteString(e.app.Out.(*bytes.Buffer).String())
	for _, secret := range []string{testToken, key} {
		if strings.Contains(all.String(), secret) {
			t.Errorf("a secret is in docker's arguments, the spec or the output:\n%s", all.String())
		}
	}
}

// TestSandboxTaskImage: the project's image wins over $ATL_SANDBOX_IMAGE,
// which wins over the default. A missing default image stops the launch
// before anything is created, in one line with the command that builds it;
// another missing image is left to docker (it may pull it) and, when that
// fails, said in one line too.
func TestSandboxTaskImage(t *testing.T) {
	e, fd := sandboxEnv(t)
	ctx := context.Background()
	task := e.task(t, "boxed", model.StatusActive, true)

	e.app.Sandbox = &sandbox.Config{Image: sandbox.DefaultImage}
	if img, from := e.app.taskImage(task); img != "atelier-task" || from != "default" {
		t.Errorf("default: %s from %s", img, from)
	}
	e.app.Sandbox = &sandbox.Config{Image: "mine:1"}
	if img, from := e.app.taskImage(task); img != "mine:1" || from != "$ATL_SANDBOX_IMAGE" {
		t.Errorf("setting: %s from %s", img, from)
	}
	pf := e.app.Paths.ProjectFile("shop")
	if err := os.MkdirAll(filepath.Dir(pf), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pf, []byte("image = \"shop-image:2\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	task.Project = "shop"
	if img, from := e.app.taskImage(task); img != "shop-image:2" || from != "project shop" {
		t.Errorf("project: %s from %s", img, from)
	}
	if spec, err := e.app.sandboxSpec(ctx, task); err != nil || spec.Image != "shop-image:2" {
		t.Errorf("spec image: %s, %v", spec.Image, err)
	}
	task.Project = ""

	// The default image is missing: one line with the build command.
	e.app.Sandbox = &sandbox.Config{Image: sandbox.DefaultImage}
	fd.missing = map[string]bool{sandbox.DefaultImage: true}
	_, err := e.app.NewSession(ctx, "boxed")
	if err == nil || strings.Contains(err.Error(), "\n") || !strings.Contains(err.Error(), "task image atelier-task is not on the Docker daemon") ||
		!strings.Contains(err.Error(), sandbox.BuildDefaultImage) {
		t.Fatalf("NewSession without the default image: %v", err)
	}
	if n := len(fd.find("run")); n != 0 {
		t.Errorf("docker run without the default image: %d calls", n)
	}

	// Another image docker cannot pull either.
	e.app.Sandbox = &sandbox.Config{Image: "mine:1"}
	fd.missing = map[string]bool{"mine:1": true}
	err = e.app.ensureSandbox(ctx, task)
	if err == nil || strings.Contains(err.Error(), "\n") ||
		!strings.Contains(err.Error(), "task image mine:1 (from $ATL_SANDBOX_IMAGE) is not on the Docker daemon and could not be pulled") {
		t.Fatalf("ensureSandbox without the image: %v", err)
	}
	if n := len(fd.find("run")); n != 1 {
		t.Errorf("docker run calls = %d, want 1 (docker may pull)", n)
	}

	// Once it is there, the container starts.
	fd.missing = nil
	if err := e.app.ensureSandbox(ctx, task); err != nil {
		t.Fatal(err)
	}
}

// TestDoctorTaskImage: doctor fails on a missing default image with the
// build command, warns on another missing one, passes when it is there.
func TestDoctorTaskImage(t *testing.T) {
	e, fd := sandboxEnv(t)
	ctx := context.Background()
	t.Setenv("ATL_SANDBOX", "docker")
	check := func() Check {
		t.Helper()
		for _, c := range e.app.checkSandbox(ctx) {
			if c.Name == "task image" {
				return c
			}
		}
		t.Fatal("no task image check")
		return Check{}
	}
	t.Setenv("ATL_SANDBOX_IMAGE", "")
	fd.missing = map[string]bool{sandbox.DefaultImage: true}
	if c := check(); c.Level() != "FAIL" || !strings.Contains(c.Detail, sandbox.BuildDefaultImage) || strings.Contains(c.Detail, "\n") {
		t.Errorf("missing default: %s %q", c.Level(), c.Detail)
	}
	fd.missing = nil
	if c := check(); c.Level() != "ok" || c.Detail != sandbox.DefaultImage {
		t.Errorf("default there: %s %q", c.Level(), c.Detail)
	}
	t.Setenv("ATL_SANDBOX_IMAGE", "mine:1")
	fd.missing = map[string]bool{"mine:1": true}
	if c := check(); c.Level() != "warn" || !strings.Contains(c.Detail, "mine:1") {
		t.Errorf("missing other: %s %q", c.Level(), c.Detail)
	}
}

// TestDoctorSandboxMode: doctor says which mode is on. Off, it checks
// nothing of the sandbox and says the mode is going away; on (unset, empty
// or docker) it checks Docker, the task image and the token; when Docker
// cannot be reached it fails in one line that names the way out; an unknown
// value fails.
func TestDoctorSandboxMode(t *testing.T) {
	e, fd := sandboxEnv(t)
	ctx := context.Background()
	t.Setenv("ATL_SANDBOX_IMAGE", "")
	checks := func() map[string]Check {
		t.Helper()
		m := map[string]Check{}
		for _, c := range e.app.checkSandbox(ctx) {
			m[c.Name] = c
		}
		return m
	}

	t.Setenv("ATL_SANDBOX", "off")
	m := checks()
	if c := m["sandbox"]; c.Level() != "ok" || !strings.HasPrefix(c.Detail, "off") || !strings.Contains(c.Detail, "going away") {
		t.Errorf("off: %s %q", c.Level(), c.Detail)
	}
	if len(m) != 1 || len(fd.calls) != 0 {
		t.Errorf("off: checks %v, docker calls %q", m, fd.calls)
	}

	for v, want := range map[string]string{"": "docker (the default): ", "docker": "docker: "} {
		t.Setenv("ATL_SANDBOX", v)
		m := checks()
		if c := m["sandbox"]; c.Level() != "ok" || !strings.HasPrefix(c.Detail, want) {
			t.Errorf("ATL_SANDBOX=%q: sandbox %s %q", v, c.Level(), c.Detail)
		}
		if c := m["docker"]; c.Level() != "ok" || !strings.Contains(c.Detail, "daemon 29.0.0") {
			t.Errorf("ATL_SANDBOX=%q: docker %s %q", v, c.Level(), c.Detail)
		}
		if c := m["task image"]; c.Level() != "ok" || c.Detail != sandbox.DefaultImage {
			t.Errorf("ATL_SANDBOX=%q: task image %s %q", v, c.Level(), c.Detail)
		}
		if c := m["claude token"]; c.Level() != "ok" {
			t.Errorf("ATL_SANDBOX=%q: claude token %s %q", v, c.Level(), c.Detail)
		}
	}
	os.Unsetenv("ATL_SANDBOX") // t.Setenv above restores it
	if c := checks()["sandbox"]; !strings.HasPrefix(c.Detail, "docker (the default): ") {
		t.Errorf("unset: %s %q", c.Level(), c.Detail)
	}

	fd.down = true
	if c := checks()["docker"]; c.Level() != "FAIL" || strings.Contains(c.Detail, "\n") ||
		!strings.Contains(c.Detail, "cannot reach Docker") || !strings.Contains(c.Detail, "Is the docker daemon running?") ||
		!strings.Contains(c.Detail, "set ATL_SANDBOX=off") {
		t.Errorf("Docker down: %s %q", c.Level(), c.Detail)
	}
	fd.down = false

	t.Setenv("ATL_SANDBOX", "dokcer")
	if c := checks()["sandbox"]; c.Level() != "FAIL" || !strings.Contains(c.Detail, `"dokcer": want docker`) {
		t.Errorf("unknown value: %s %q", c.Level(), c.Detail)
	}
}

// TestSandboxDockerUnreachable: with the sandbox on and no Docker daemon,
// starting a session fails before anything else (even without a token) in
// one line that says why Docker is needed and what to do, the only message
// that names ATL_SANDBOX=off; the token and image messages do not.
func TestSandboxDockerUnreachable(t *testing.T) {
	e, fd := sandboxEnv(t)
	ctx := context.Background()
	task := e.task(t, "boxed", model.StatusActive, true)
	fd.down = true
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	_, err := e.app.NewSession(ctx, "boxed")
	if err == nil || strings.Contains(err.Error(), "\n") || !strings.Contains(err.Error(), "cannot reach Docker, which runs each task's sessions") ||
		!strings.Contains(err.Error(), "(docker version: Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?)") ||
		!strings.Contains(err.Error(), "start Docker or give atl access to it, or set ATL_SANDBOX=off") {
		t.Fatalf("NewSession without Docker: %v", err)
	}
	if len(fd.changes()) != 0 || exists(e.app.Paths.RunDir("boxed")) {
		t.Errorf("started something without Docker: %q", fd.calls)
	}

	// Docker is back: the next missing thing is the token, then the image.
	fd.down = false
	if err := e.app.ensureSandbox(ctx, task); err == nil || !strings.Contains(err.Error(), "atl token") ||
		strings.Contains(err.Error(), "ATL_SANDBOX") {
		t.Errorf("without a token: %v", err)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", testToken)
	e.app.Sandbox = &sandbox.Config{Image: sandbox.DefaultImage}
	fd.missing = map[string]bool{sandbox.DefaultImage: true}
	if err := e.app.ensureSandbox(ctx, task); err == nil || !strings.Contains(err.Error(), sandbox.BuildDefaultImage) ||
		strings.Contains(err.Error(), "ATL_SANDBOX") {
		t.Errorf("without the image: %v", err)
	}
	fd.missing = nil
	if err := e.app.ensureSandbox(ctx, task); err != nil {
		t.Fatal(err)
	}
}

// userClaudeDir makes a Claude config dir for the user at
// $CLAUDE_CONFIG_DIR with their setup, their secrets and a transcript.
func userClaudeDir(t *testing.T) string {
	t.Helper()
	user := os.Getenv("CLAUDE_CONFIG_DIR")
	for name, content := range map[string]string{
		"settings.json":           `{"theme":"x","apiKeyHelper":"/home/u/bin/key","statusLine":{"type":"command","command":"mine"}}`,
		"CLAUDE.md":               "# my rules\n",
		"keybindings.json":        `{"bindings":[]}`,
		"skills/review/SKILL.md":  "---\nname: review\n---\n",
		"agents/helper.md":        "an agent\n",
		"commands/go.md":          "a command\n",
		".credentials.json":       `{"claudeAiOauth":{"accessToken":"SECRET-LOGIN"}}`,
		".claude.json":            `{"theme":"light","oauthAccount":{"emailAddress":"me@example.com"},"mcpServers":{"x":{"env":{"K":"SECRET-MCP"}}}}`,
		"projects/-other/o.jsonl": `{"type":"user","message":{"content":"SECRET-TRANSCRIPT"}}`,
		"todos/t.json":            "[]",
	} {
		path := filepath.Join(user, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink in a copied dir does not carry the credentials over either.
	if err := os.Symlink(filepath.Join(user, ".credentials.json"), filepath.Join(user, "skills", "creds.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(user, "projects"), filepath.Join(user, "agents", "old")); err != nil {
		t.Fatal(err)
	}
	return user
}

// TestSandboxSeedsClaudeDir: a new container's Claude dir gets copies of the
// user's setup and a state file that skips the first-start screens, nothing
// secret; the copies are refreshed only for a new container, never written
// through what the task left there, and Claude Code's own state is kept.
func TestSandboxSeedsClaudeDir(t *testing.T) {
	e, fd := sandboxEnv(t)
	ctx := context.Background()
	user := userClaudeDir(t)
	task := e.task(t, "boxed", model.StatusActive, true)
	dir := e.app.Paths.ClaudeDir("boxed")
	if err := e.app.ensureSandbox(ctx, task); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CLAUDE.md", "keybindings.json", "skills/review/SKILL.md", "agents/helper.md", "commands/go.md"} {
		if got, want := readFile(t, filepath.Join(dir, name)), readFile(t, filepath.Join(user, name)); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, "settings.json"))), &settings); err != nil {
		t.Fatal(err)
	}
	if _, ok := settings["apiKeyHelper"]; ok || settings["statusLine"] == nil || settings["theme"] != "x" {
		t.Errorf("settings.json copy: %v", settings)
	}
	for _, never := range []string{".credentials.json", "projects", "todos", "skills/creds.md", "agents/old"} {
		if _, err := os.Lstat(filepath.Join(dir, never)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was carried over: %v", never, err)
		}
	}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b := readFile(t, path); strings.Contains(b, "SECRET") || strings.Contains(b, "me@example.com") {
				t.Errorf("%s holds a secret of the user:\n%s", path, b)
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Onboarded bool   `json:"hasCompletedOnboarding"`
		Theme     string `json:"theme"`
		Projects  map[string]struct {
			Trusted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	state := filepath.Join(dir, ".claude.json")
	if err := json.Unmarshal([]byte(readFile(t, state)), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Onboarded || st.Theme != "light" || !st.Projects[e.app.Paths.TaskDir("boxed")].Trusted {
		t.Errorf("state file: %+v", st)
	}

	// Claude Code writes its state; the task changes its copies and plants a
	// symlink to the user's settings. Sessions in the same container see
	// all that as it is.
	if err := os.WriteFile(state, []byte(`{"numStartups":3,"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("changed by the task\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "settings.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(user, "settings.json"), filepath.Join(dir, "settings.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(user, "CLAUDE.md"), []byte("# my new rules\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	userSettings := readFile(t, filepath.Join(user, "settings.json"))
	if err := e.app.ensureSandbox(ctx, task); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "CLAUDE.md")); got != "changed by the task\n" {
		t.Errorf("copies refreshed without a new container: %q", got)
	}

	// A new container: the copies are the user's again, the symlink is
	// replaced (the user's file untouched), Claude Code's state is kept.
	fd.label = "" // the container is gone
	if err := e.app.ensureSandbox(ctx, task); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "CLAUDE.md")); got != "# my new rules\n" {
		t.Errorf("CLAUDE.md not refreshed: %q", got)
	}
	if fi, err := os.Lstat(filepath.Join(dir, "settings.json")); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("settings.json: %v, %v", fi, err)
	}
	if got := readFile(t, filepath.Join(user, "settings.json")); got != userSettings {
		t.Errorf("the user's settings were written through: %s", got)
	}
	var st2 map[string]any
	if err := json.Unmarshal([]byte(readFile(t, state)), &st2); err != nil {
		t.Fatal(err)
	}
	if st2["numStartups"] != float64(3) || st2["theme"] != "dark" || st2["hasCompletedOnboarding"] != true {
		t.Errorf("state after refresh: %v", st2)
	}

	// Closing the task with no transcripts removes the dir altogether.
	e.app.removeSandbox(ctx, task)
	if exists(dir) {
		t.Errorf("Claude dir left: %s", dir)
	}
}

// TestSandboxClaudeDirsApart: two tasks get different Claude dirs, and no
// mount of either reaches the other's or the user's, with or without a
// workspace.
func TestSandboxClaudeDirsApart(t *testing.T) {
	e, _ := sandboxEnv(t)
	ctx := context.Background()
	p := e.app.Paths
	root := filepath.Dir(e.out)
	a := e.task(t, "alpha", model.StatusActive, true)
	b := e.task(t, "beta", model.StatusActive, true)
	inside := func(dir, p string) bool {
		rel, err := filepath.Rel(dir, p)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
	}
	for _, paths := range []config.Paths{p, {
		Workspace: filepath.Join(root, "ws"), Repos: p.Repos, Tasks: filepath.Join(root, "ws", "tasks"),
		DB: filepath.Join(root, "ws", "data", "atelier.db"), StateDir: filepath.Join(root, "ws", "state"),
		ConfigDir: filepath.Join(root, "ws", "config"),
	}} {
		e.app.Paths = paths
		for _, d := range []string{paths.TaskDir("alpha"), paths.TaskDir("beta")} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if paths.ClaudeDir("alpha") == paths.ClaudeDir("beta") {
			t.Fatal("two tasks share a Claude dir")
		}
		for _, c := range []struct {
			task  *model.Task
			other string
		}{{a, "beta"}, {b, "alpha"}} {
			spec, err := e.app.sandboxSpec(ctx, c.task)
			if err != nil {
				t.Fatal(err)
			}
			if spec.Env["CLAUDE_CONFIG_DIR"] != paths.ClaudeDir(c.task.Slug) {
				t.Errorf("%s: CLAUDE_CONFIG_DIR = %s", c.task.Slug, spec.Env["CLAUDE_CONFIG_DIR"])
			}
			for _, m := range spec.Mounts {
				for _, h := range []string{paths.ClaudeDir(c.other), os.Getenv("CLAUDE_CONFIG_DIR")} {
					if inside(m.Src, h) || inside(h, m.Src) {
						t.Errorf("%s: mount %s reaches %s", c.task.Slug, m.Src, h)
					}
				}
			}
		}
	}
}

// TestSandboxResumesFromTaskClaudeDir: with the sandbox on, a past session
// can be resumed when its transcript is in the task's Claude dir (where
// claude --resume in the container looks), not in the user's.
func TestSandboxResumesFromTaskClaudeDir(t *testing.T) {
	e, _ := sandboxEnv(t)
	task := e.task(t, "boxed", model.StatusActive, true)
	dir := e.app.Paths.TaskDir("boxed")
	cfg := e.app.claudeConfigDirFor(task)
	if cfg != e.app.Paths.ClaudeDir("boxed") {
		t.Fatalf("config dir for a sandboxed task: %s", cfg)
	}
	key := claudeProjectKey(dir)
	own := model.Session{Name: "claude-1", TranscriptPath: filepath.Join(cfg, "projects", key, "a.jsonl")}
	users := model.Session{Name: "claude-2", TranscriptPath: filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", key, "b.jsonl")}
	if err := resumableHere(cfg, dir, own); err != nil {
		t.Errorf("own transcript: %v", err)
	}
	if err := resumableHere(cfg, dir, users); err == nil {
		t.Error("a transcript in the user's dir counts as resumable in the task container")
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
