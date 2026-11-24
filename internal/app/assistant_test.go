package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/sandbox"
)

// assistantArgv waits for the stub claude's argv record of the assistant
// (launched without ATL_SESSION, so recorded as "argv-").
func (e *env) assistantArgv(t *testing.T) []string {
	t.Helper()
	path := filepath.Join(e.out, "argv-")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil {
			s := strings.TrimSuffix(string(b), "\n")
			if s == "" {
				return []string{}
			}
			return strings.Split(s, "\n")
		}
		if time.Now().After(deadline) {
			t.Fatal("claude stub never ran for the assistant")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// conversation records a fake Claude Code transcript for the assistant dir
// under $HOME/.claude/projects, so the next launch continues it.
func (e *env) conversation(t *testing.T) {
	t.Helper()
	d := filepath.Join(os.Getenv("HOME"), ".claude", "projects", claudeProjectKey(e.app.Paths.AssistantDir()))
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "abc.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAssistant(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.task(t, "alpha", model.StatusActive, true)
	e.task(t, "beta", model.StatusDone, false)

	cmd, hint, err := e.app.Assistant(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if hint != "" {
		t.Errorf("fresh launch hint %q", hint)
	}
	if got := strings.Join(cmd.Args[len(cmd.Args)-3:], " "); got != "attach-session -t =atl_assistant" {
		t.Errorf("attach cmd = %v", cmd.Args)
	}
	// No previous conversation: plain claude with the prompt.
	if got, want := e.assistantArgv(t), []string{"Let's talk about task alpha."}; !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %q, want %q", got, want)
	}
	dir := e.app.Paths.AssistantDir()
	if pwd, _ := os.ReadFile(filepath.Join(e.out, "pwd-")); strings.TrimSpace(string(pwd)) != dir {
		t.Errorf("pwd = %q, want %q", pwd, dir)
	}
	if envb, _ := os.ReadFile(filepath.Join(e.out, "env-")); !strings.Contains(string(envb), "ATL_ASSISTANT=1\n") {
		t.Errorf("ATL_ASSISTANT not set:\n%s", envb)
	}
	for _, f := range []string{"CLAUDE.md", filepath.Join(".claude", "settings.json")} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if s, _ := os.ReadFile(filepath.Join(dir, ".claude", "settings.json")); !strings.Contains(string(s), "'/usr/bin/atl' hook AssistantGuard") {
		t.Errorf("settings:\n%s", s)
	}
	if got := e.tmux(t, "list-windows", "-t", "=atl_assistant:", "-F", "#{window_name} #{@atl_session}"); got != "assistant" {
		t.Errorf("assistant window = %q (want named assistant, untagged)", got)
	}

	// Running already: a second call (closed tasks are fine) puts the
	// sentence in the paste buffer and returns a hint; nothing is typed.
	if err := os.Remove(filepath.Join(e.out, "argv-")); err != nil {
		t.Fatal(err)
	}
	_, hint, err = e.app.Assistant(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hint, "Ctrl-b ]") || !strings.Contains(hint, "Let's talk about task beta.") {
		t.Errorf("hint = %q", hint)
	}
	if got := e.tmux(t, "show-buffer"); got != "Let's talk about task beta." {
		t.Errorf("paste buffer = %q", got)
	}
	time.Sleep(100 * time.Millisecond)
	if got := e.tmux(t, "capture-pane", "-p", "-t", "=atl_assistant:"); strings.Contains(got, "beta") {
		t.Errorf("sentence typed into the assistant:\n%s", got)
	}
	if n := e.tmux(t, "list-windows", "-t", "=atl_assistant:", "-F", "x"); n != "x" {
		t.Errorf("windows after second call: %q", n)
	}
	// Without a task: just attach.
	if _, hint, err := e.app.Assistant(ctx, ""); err != nil || hint != "" {
		t.Fatal(hint, err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(e.out, "argv-")); err == nil {
		t.Error("assistant relaunched")
	}

	// Recover and Done of a task leave the assistant alone.
	s, err := e.app.NewSession(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	e.argv(t, s.ID)
	if n, err := e.app.Recover(ctx); n != 0 || err != nil {
		t.Fatalf("Recover = %d, %v", n, err)
	}
	if _, err := e.app.Done(ctx, "alpha", false); err != nil {
		t.Fatal(err)
	}
	if e.app.Tmux.HasSession(ctx, "atl-alpha") {
		t.Error("task session survived Done")
	}
	if ok, err := e.app.Tmux.WindowAlive(ctx, AssistantSession, AssistantWindow); !ok || err != nil {
		t.Errorf("assistant gone after Recover/Done: %v %v", ok, err)
	}
}

func TestAssistantUnknownTask(t *testing.T) {
	e := newEnv(t)
	if _, _, err := e.app.Assistant(context.Background(), "nope"); err == nil {
		t.Fatal("unknown task accepted")
	}
	if e.app.Tmux.HasSession(context.Background(), AssistantSession) {
		t.Error("assistant launched for an unknown task")
	}
}

func TestAssistantFreshWithoutTask(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.startServer(t, [2]string{"remain-on-exit", "on"})
	e.conversation(t)
	if _, _, err := e.app.Assistant(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if got := e.assistantArgv(t); !reflect.DeepEqual(got, []string{"--continue"}) {
		t.Errorf("argv = %q", got)
	}
	// A dead assistant (claude exited, remain-on-exit) is relaunched.
	e.crashClaude(t, "=atl_assistant:")
	if err := os.Remove(filepath.Join(e.out, "argv-")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.app.Assistant(ctx, "x-missing"); err == nil {
		t.Fatal("unknown task accepted")
	}
	e.task(t, "gamma", model.StatusActive, false)
	_, hint, err := e.app.Assistant(ctx, "gamma")
	if err != nil {
		t.Fatal(err)
	}
	if hint != "" {
		t.Errorf("relaunch hint %q", hint)
	}
	if got := e.assistantArgv(t); !reflect.DeepEqual(got, []string{"--continue", "Let's talk about task gamma."}) {
		t.Errorf("argv after relaunch = %q", got)
	}
	if n := e.tmux(t, "list-windows", "-t", "=atl_assistant:", "-F", "#{window_name} #{pane_dead}"); n != "assistant 0" {
		t.Errorf("windows after relaunch: %q", n)
	}
}

// A session whose assistant window is gone (another live window does not
// count) is replaced.
func TestAssistantMissingWindow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.tmux(t, "new-session", "-d", "-s", AssistantSession, "-n", "other", "sleep", "300")
	if _, _, err := e.app.Assistant(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if got := e.assistantArgv(t); len(got) != 0 {
		t.Errorf("argv = %q", got)
	}
	if n := e.tmux(t, "list-windows", "-t", "=atl_assistant:", "-F", "#{window_name}"); n != "assistant" {
		t.Errorf("windows: %q", n)
	}
}

func TestAssistantConcurrent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		var wg sync.WaitGroup
		errs := make([]error, 4)
		for j := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _, errs[j] = e.app.Assistant(ctx, "")
			}()
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if n := e.tmux(t, "list-windows", "-t", "=atl_assistant:", "-F", "#{window_name}"); n != "assistant" {
			t.Fatalf("round %d: windows %q", i, n)
		}
		e.tmux(t, "kill-session", "-t", "="+AssistantSession)
	}
}

func TestAssistantArgv(t *testing.T) {
	if got := assistantArgv(false, nil); !reflect.DeepEqual(got, []string{"claude"}) {
		t.Errorf("fresh = %q", got)
	}
	if got := assistantArgv(true, []string{"hi there"}); !reflect.DeepEqual(got, []string{"claude", "--continue", "hi there"}) {
		t.Errorf("continue = %q", got)
	}
}

func TestClaudeProjectKey(t *testing.T) {
	for in, want := range map[string]string{
		"/repos/atelier":                         "-repos-atelier",
		"/home/u/my-tasks/try-it":                "-home-u-my-tasks-try-it",
		"/home/u/.local/share/atelier/assistant": "-home-u--local-share-atelier-assistant",
		"/a/b_c d.e~f":                           "-a-b-c-d-e-f",
		"/x/é😀":                                  "-x----",
	} {
		if got := claudeProjectKey(in); got != want {
			t.Errorf("claudeProjectKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHasClaudeConversation(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	real := filepath.Join(root, "real", "assistant")
	link := filepath.Join(root, "link")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), link); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(link, "assistant")
	record := func(cfg, d, name string) {
		t.Helper()
		p := filepath.Join(cfg, "projects", claudeProjectKey(d))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if hasClaudeConversation(config.ClaudeConfigDir(), dir) {
		t.Fatal("conversation without any transcript")
	}
	record(filepath.Join(home, ".claude"), real, "notes.txt")
	if hasClaudeConversation(config.ClaudeConfigDir(), dir) {
		t.Fatal("non-jsonl file counted")
	}
	// Recorded under the resolved path (Claude's cwd is the physical dir).
	record(filepath.Join(home, ".claude"), real, "s1.jsonl")
	if !hasClaudeConversation(config.ClaudeConfigDir(), dir) || !hasClaudeConversation(config.ClaudeConfigDir(), real) {
		t.Fatal("conversation under ~/.claude not found")
	}
	// $CLAUDE_CONFIG_DIR takes precedence over ~/.claude.
	cfg := filepath.Join(root, "cfg")
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if hasClaudeConversation(config.ClaudeConfigDir(), dir) {
		t.Fatal("~/.claude used despite CLAUDE_CONFIG_DIR")
	}
	record(cfg, dir, "s2.jsonl")
	if !hasClaudeConversation(config.ClaudeConfigDir(), dir) {
		t.Fatal("conversation under CLAUDE_CONFIG_DIR not found")
	}
}

// TestAssistantQuarantine: next to atl (the sandbox off), the assistant's
// dir is cleaned only after the assistant ran in its container, which
// atl's marker in the state dir says (a file of that name in the assistant
// dir, which a container can write, is no marker): never before, so the
// user's own files there stay launch after launch, and only once after.
// Then the container goes first (with Docker unreachable the assistant
// does not start, and nothing moves), and everything in the dir but atl's
// own files moves into a quarantine dir under the state dir, with one
// warning: a .git whose core.fsmonitor any git there would run, a .claude
// that is a symlink (moved as it is, never followed), the rest.
func TestAssistantQuarantine(t *testing.T) {
	e, fd, _ := assistantSandboxEnv(t)
	ctx := context.Background()
	dir := e.app.Paths.AssistantDir()
	out := e.app.Out.(*bytes.Buffer)
	marker := filepath.Join(e.app.Paths.StateDir, "assistant-in-container")
	quarantines := func() []string {
		qs, _ := filepath.Glob(filepath.Join(e.app.Paths.StateDir, "assistant-quarantine", "*"))
		return qs
	}
	write := func(files map[string]string) {
		t.Helper()
		for rel, content := range files {
			p := filepath.Join(dir, rel)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	launch := func() error {
		t.Helper()
		if e.app.Tmux.HasSession(ctx, AssistantSession) {
			e.tmux(t, "kill-session", "-t", "="+AssistantSession)
		}
		_, _, err := e.app.Assistant(ctx, "")
		return err
	}
	inContainer := e.app.Sandbox

	// Never in its container: nothing moves, launch after launch.
	e.app.Sandbox = nil
	own := map[string]string{"CLAUDE.local.md": "mine\n", ".claude/settings.local.json": `{"permissions":{}}`, "assistant-in-container": "forged\n"}
	write(own)
	for i := 0; i < 2; i++ {
		if err := launch(); err != nil {
			t.Fatal(err)
		}
	}
	for rel, content := range own {
		if got, _ := os.ReadFile(filepath.Join(dir, rel)); string(got) != content {
			t.Errorf("%s: %q", rel, got)
		}
	}
	if qs := quarantines(); len(qs) != 0 || out.Len() != 0 {
		t.Fatalf("moved without the marker: %q %q", qs, out.String())
	}

	// In its container: the marker. What the container then writes.
	e.app.Sandbox = inContainer
	if err := launch(); err != nil {
		t.Fatal(err)
	}
	if !exists(marker) {
		t.Fatal("no marker after a launch in the container")
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(filepath.Join(outside, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	planted := map[string]string{".git/config": "[core]\n\tfsmonitor = /tmp/evil\n", ".git/HEAD": "ref: refs/heads/main\n",
		".mcp.json": "{}\n", "scratch.md": "x\n"}
	write(planted)
	if err := os.RemoveAll(filepath.Join(dir, ".claude")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, ".claude")); err != nil {
		t.Fatal(err)
	}

	// Next to atl, Docker unreachable: refused, nothing moved.
	e.app.Sandbox = nil
	fd.down = true
	if _, _, err := e.app.Assistant(ctx, ""); err == nil || !strings.Contains(err.Error(), "cannot make sure that container is gone") ||
		strings.Contains(err.Error(), "\n") {
		t.Errorf("Docker down: %v", err)
	}
	if !exists(filepath.Join(dir, ".git", "config")) || !exists(marker) || len(quarantines()) != 0 {
		t.Error("files moved or marker gone without the container removed")
	}
	fd.down = false

	// Next to atl, the container still running: removed first, then the move.
	gitAtRm := false
	docker := e.app.Docker
	e.app.Docker = func(ctx context.Context, args ...string) (string, error) {
		if args[0] == "rm" {
			gitAtRm = exists(filepath.Join(dir, ".git"))
		}
		return docker(ctx, args...)
	}
	out.Reset()
	e.app.Now = func() time.Time { return time.Now().Add(time.Hour) }
	if err := launch(); err != nil {
		t.Fatal(err)
	}
	if rms := fd.find("rm"); len(rms) != 1 || !slices.Contains(rms[0], fakeContainerID) || !gitAtRm {
		t.Errorf("container not removed before the move: %q (git there then: %v)", rms, gitAtRm)
	}
	qs := quarantines()
	if len(qs) != 1 {
		t.Fatalf("quarantine dirs: %q", qs)
	}
	for rel, content := range planted {
		if got, _ := os.ReadFile(filepath.Join(qs[0], rel)); string(got) != content || exists(filepath.Join(dir, rel)) {
			t.Errorf("%s: in dir %v, in quarantine %q", rel, exists(filepath.Join(dir, rel)), got)
		}
	}
	for _, rel := range []string{"CLAUDE.local.md", "assistant-in-container"} {
		if exists(filepath.Join(dir, rel)) || !exists(filepath.Join(qs[0], rel)) {
			t.Errorf("%s not moved", rel)
		}
	}
	if target, err := os.Readlink(filepath.Join(qs[0], ".claude")); err != nil || target != outside {
		t.Errorf("quarantined .claude: %q %v", target, err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 1 {
		t.Errorf("the symlinked .claude was followed: %v", entries)
	}
	if fi, err := os.Lstat(filepath.Join(dir, ".claude")); err != nil || !fi.IsDir() {
		t.Errorf(".claude after: %v %v", fi, err)
	}
	if s := readFile(t, filepath.Join(dir, ".claude", "settings.json")); strings.Contains(s, sandbox.InBin) || !strings.Contains(s, e.app.AtlBin) {
		t.Errorf("settings not written for next to atl:\n%s", s)
	}
	if fi, err := os.Lstat(filepath.Join(dir, "CLAUDE.md")); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("CLAUDE.md: %v %v", fi, err)
	}
	if exists(marker) {
		t.Error("marker left")
	}
	if o := out.String(); strings.Count(o, "warning:") != 1 || !strings.Contains(o, ".git") || !strings.Contains(o, qs[0]) {
		t.Errorf("output %q", o)
	}

	// Cleaned: the next launches move nothing.
	write(map[string]string{"CLAUDE.local.md": "mine again\n"})
	if err := launch(); err != nil {
		t.Fatal(err)
	}
	if len(quarantines()) != 1 || readFile(t, filepath.Join(dir, "CLAUDE.local.md")) != "mine again\n" {
		t.Error("moved again after the clean-up")
	}
}

// TestLockFileNoFollow: a lock path that is a symlink or not a regular
// file is refused, and nothing is created through it.
func TestLockFileNoFollow(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link.lock")
	if err := os.Symlink(filepath.Join(dir, "target"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := lockFile(context.Background(), link, syscall.LOCK_EX, time.Second, "busy"); err == nil || exists(filepath.Join(dir, "target")) {
		t.Errorf("symlink: %v", err)
	}
	fifo := filepath.Join(dir, "fifo.lock")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := lockFile(context.Background(), fifo, syscall.LOCK_EX, time.Second, "busy"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("fifo: %v", err)
	}
	unlock, err := lockFile(context.Background(), filepath.Join(dir, "plain.lock"), syscall.LOCK_EX, time.Second, "busy")
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

// TestAssistantSettingsOnHost: next to atl, after the assistant ran in its
// container, the assistant dir's .claude/settings.json is atl's alone: one
// that differs from what atl writes afresh (a hook its container added,
// say) is moved into the quarantine dir and written anew, not merged;
// atl's own file stays.
func TestAssistantSettingsOnHost(t *testing.T) {
	e := newEnv(t)
	e.app.Docker = (&fakeDocker{}).run
	ctx := context.Background()
	ran := func() { // the marker a launch in the container writes
		t.Helper()
		if err := os.WriteFile(filepath.Join(e.app.Paths.StateDir, "assistant-in-container"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	settings := filepath.Join(e.app.Paths.AssistantDir(), ".claude", "settings.json")
	if _, _, err := e.app.Assistant(ctx, ""); err != nil {
		t.Fatal(err)
	}
	fresh := readFile(t, settings)
	relaunch := func() []string {
		t.Helper()
		e.tmux(t, "kill-session", "-t", "="+AssistantSession)
		ran()
		if _, _, err := e.app.Assistant(ctx, ""); err != nil {
			t.Fatal(err)
		}
		qs, _ := filepath.Glob(filepath.Join(e.app.Paths.StateDir, "assistant-quarantine", "*"))
		return qs
	}
	if qs := relaunch(); len(qs) != 0 || e.app.Out.(*bytes.Buffer).Len() != 0 {
		t.Errorf("atl's own settings quarantined: %q %q", qs, e.app.Out.(*bytes.Buffer).String())
	}
	planted := strings.Replace(fresh, `"hooks": {`, `"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "evil"}]}], `, 1)
	if planted == fresh {
		t.Fatalf("settings have no hooks:\n%s", fresh)
	}
	if err := os.WriteFile(settings, []byte(planted), 0o644); err != nil {
		t.Fatal(err)
	}
	qs := relaunch()
	if len(qs) != 1 || readFile(t, filepath.Join(qs[0], ".claude", "settings.json")) != planted {
		t.Fatalf("quarantine: %q", qs)
	}
	if got := readFile(t, settings); got != fresh || strings.Contains(got, "evil") {
		t.Errorf("settings after:\n%s", got)
	}
	if out := e.app.Out.(*bytes.Buffer).String(); strings.Count(out, "warning:") != 1 || !strings.Contains(out, ".claude/settings.json") {
		t.Errorf("output %q", out)
	}
}
