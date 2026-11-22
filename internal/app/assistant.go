package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/gen"
	"github.com/andreujuanc/atelier/internal/sandbox"
	"github.com/andreujuanc/atelier/internal/tmux"
)

// AssistantSession is the tmux session of the atelier assistant. Task
// sessions are named "atl-<slug>", so the underscore can never collide.
const AssistantSession = "atl_assistant"

// AssistantWindow is the name of the assistant's window in AssistantSession.
const AssistantWindow = "assistant"

// assistantLockWait bounds how long Assistant waits for a concurrent
// Assistant call (from any process) to finish launching.
const assistantLockWait = 10 * time.Second

// AssistantPrompt is the sentence that points the assistant at a task.
func AssistantPrompt(slug string) string { return "Let's talk about task " + slug + "." }

// Assistant makes sure the atelier assistant runs — one Claude Code session
// for all tasks, in Paths.AssistantDir(), whose files are regenerated first —
// and returns the command that attaches the terminal to it.
//
// If taskRef is non-empty it must name a task: a freshly launched assistant
// gets "Let's talk about task <slug>." as its initial prompt. Nothing is ever
// typed into a running assistant (keys could answer a pending permission
// prompt): the sentence is put in the tmux paste buffer instead and hint
// tells the user how to paste it (best effort; on failure hint is empty and
// a warning is printed).
//
// The assistant runs in window AssistantWindow of AssistantSession; if that
// window is missing or its pane is dead the session is replaced. Concurrent
// calls (from any process) are serialised by a lock file in the state dir
// (assistantLock). The window is untagged, so Recover, Done and Cancel never touch it,
// and it has no state-tracking hooks. Next to atl (the sandbox off) what
// its container may have written in its dir is moved aside first
// (quarantineAssistant).
//
// With the sandbox on, Claude runs in the assistant's own container (see
// ensureAssistantSandbox), which every call makes sure of, gate included,
// and the window runs docker exec into it, as a task session's window does.
func (a *App) Assistant(ctx context.Context, taskRef string) (attach *exec.Cmd, hint string, err error) {
	var prompt []string
	slug := ""
	if taskRef != "" {
		t, err := a.Store.ResolveTask(ctx, taskRef)
		if err != nil {
			return nil, "", err
		}
		slug = t.Slug
		prompt = []string{AssistantPrompt(slug)}
	}
	dir := a.Paths.AssistantDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", fmt.Errorf("assistant: create %s: %w", dir, err)
	}
	if err := os.MkdirAll(a.Paths.StateDir, 0o755); err != nil {
		return nil, "", fmt.Errorf("assistant: %w", err)
	}
	unlock, err := lockFile(ctx, a.assistantLock(), assistantLockWait,
		"another atl is launching the assistant")
	if err != nil {
		return nil, "", fmt.Errorf("assistant: %w", err)
	}
	defer unlock()
	if a.Sandbox == nil {
		a.quarantineAssistant(dir)
	}
	if err := gen.WriteAssistantFiles(dir, a.hookBin(), a.Sandbox != nil); err != nil {
		return nil, "", fmt.Errorf("assistant: %w", err)
	}

	alive, err := a.Tmux.WindowAlive(ctx, AssistantSession, AssistantWindow)
	if err != nil {
		return nil, "", fmt.Errorf("assistant: %w", err)
	}
	container := ""
	if a.Sandbox != nil {
		if container, err = a.ensureAssistantSandbox(ctx, alive); err != nil {
			return nil, "", fmt.Errorf("assistant: %w", err)
		}
	}
	if alive {
		if len(prompt) > 0 {
			if err := a.Tmux.SetBuffer(ctx, prompt[0]); err != nil {
				a.warnf("assistant: %v", err)
			} else {
				hint = fmt.Sprintf("assistant already running: press %s ] to paste %q",
					a.Tmux.PrefixKey(ctx), prompt[0])
			}
		}
		return a.Tmux.AttachCmd(AssistantSession), hint, nil
	}

	// A session without a live assistant window (claude exited with
	// remain-on-exit, or the window was closed) is replaced.
	if a.Tmux.HasSession(ctx, AssistantSession) {
		if err := a.Tmux.KillSession(ctx, AssistantSession); err != nil {
			return nil, "", fmt.Errorf("assistant: %w", err)
		}
	}
	env := map[string]string{sandbox.AssistantEnv: "1"}
	claudeDir := config.ClaudeConfigDir()
	if a.Sandbox != nil {
		claudeDir = a.Paths.AssistantClaudeDir()
	}
	argv := assistantArgv(hasClaudeConversation(claudeDir, dir), prompt)
	var auth map[string]string
	if a.Sandbox != nil {
		// As for a task session (see launch): the token by name, from the
		// tmux server's global environment; docker exec once the terminal
		// is attached (the caller attaches right away).
		if auth, err = a.sandboxAuth(); err != nil {
			return nil, "", fmt.Errorf("assistant: %w", err)
		}
		argv = sandbox.WindowArgv(true, a.sandboxArgv(container, dir, env, auth, argv))
	}
	if _, err := a.Tmux.NewWindow(ctx, tmux.WindowSpec{
		Session:   AssistantSession,
		Name:      AssistantWindow,
		Dir:       dir,
		Env:       a.windowEnv(env),
		Argv:      argv,
		GlobalEnv: auth,
	}); err != nil {
		return nil, "", fmt.Errorf("assistant: %w", err)
	}
	return a.Tmux.AttachCmd(AssistantSession), "", nil
}

// assistantID names the assistant's sandbox (sandbox.AssistantID).
func (a *App) assistantID() string { return sandbox.AssistantID(a.Paths.AssistantDir()) }

// assistantGateLog is the assistant's gate log, next to hook.log. Not
// "gate-assistant.log": that is the log of a task whose slug is assistant.
func (a *App) assistantGateLog() string {
	return filepath.Join(a.Paths.StateDir, "gate-_assistant.log")
}

// assistantSpec describes the assistant's container: one per workspace,
// made from the workspace's task image ($ATL_SANDBOX_IMAGE, else the
// default; never a project's). It sees, at the same paths as here, the
// assistant dir and its own Claude config dir (Paths.AssistantClaudeDir,
// as $CLAUDE_CONFIG_DIR), and its run dir (the atl binary, its gate's
// sockets) at sandbox.RunMount, read-only; nothing else: no tasks, no repos,
// no other part of the workspace, no $ATL_SANDBOX_MOUNTS, and no Docker
// (no $DOCKER_HOST; its gate refuses every Docker request). atl in it sends
// its commands to that gate (sandbox.AssistantEnv), which runs the ones
// the assistant may use where atl runs. $ATL_CONTEXT, which describes the
// place atl runs in, is passed as it is: the assistant manages it.
func (a *App) assistantSpec() (sandbox.Spec, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return sandbox.Spec{}, err
	}
	dir := a.Paths.AssistantDir()
	id := a.assistantID()
	claude := a.Paths.AssistantClaudeDir()
	spec := sandbox.Spec{
		ID:      id,
		Image:   a.Sandbox.Image,
		User:    fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		Workdir: dir,
		Env: map[string]string{
			"HOME":               home,
			sandbox.InEnv:        id,
			sandbox.AssistantEnv: "1",
			"CLAUDE_CONFIG_DIR":  claude,
		},
		Mounts: []sandbox.Mount{
			{Src: dir, Dst: dir},
			{Src: claude, Dst: claude},
			{Src: a.Paths.AssistantRunDir(), Dst: sandbox.RunMount, RO: true},
		},
	}
	for _, k := range []string{"LANG", "LC_ALL", "TZ", "ATL_CONTEXT"} {
		if v := os.Getenv(k); v != "" {
			spec.Env[k] = v
		}
	}
	return spec, nil
}

// ensureAssistantSandbox makes the assistant's gate and container run, as
// ensureSandbox does for a task: Docker, a static atl binary and a Claude
// Code token first (each a one-line error), then the atl binary in its run
// dir, its gate (replaced when it runs another atl binary), and its
// container, created on first use with its Claude dir seeded, started after
// a reboot, and replaced when its spec changed (image, atl paths, ...)
// unless busy (the assistant's window is alive). It returns the container's
// full ID. The container is never removed by atl: no task closes it (remove
// it with docker rm -f; the next atl assistant makes a new one).
func (a *App) ensureAssistantSandbox(ctx context.Context, busy bool) (string, error) {
	if a.Paths.Workspace == "" {
		return "", errors.New("the assistant's container needs a workspace")
	}
	if _, err := a.checkDocker(ctx); err != nil {
		return "", err
	}
	if err := a.checkAtlBin(a.AtlBin); err != nil {
		return "", err
	}
	if _, err := a.claudeAuth(); err != nil {
		return "", err
	}
	runDir := a.Paths.AssistantRunDir()
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return "", err
	}
	if err := sandbox.InstallBin(a.AtlBin, runDir); err != nil {
		return "", fmt.Errorf("install atl for the assistant's container: %w", err)
	}
	if err := os.MkdirAll(a.Paths.AssistantClaudeDir(), 0o700); err != nil {
		return "", err
	}
	startGate := a.StartAssistantGate
	if startGate == nil {
		startGate = sandbox.StartAssistant
	}
	if err := startGate(a.AtlBin, a.Paths.Workspace, runDir, a.assistantGateLog()); err != nil {
		return "", err
	}
	spec, err := a.assistantSpec()
	if err != nil {
		return "", err
	}
	fresh := func() error {
		if spec.Image == sandbox.DefaultImage {
			if missing, err := sandbox.ImageMissing(ctx, a.docker(), spec.Image); err == nil && missing {
				return missingImage(spec.Image, "default")
			}
		}
		return a.seedClaudeDirAt(a.Paths.AssistantClaudeDir(), a.Paths.AssistantDir(), "the assistant")
	}
	container, stale, err := sandbox.Ensure(ctx, a.docker(), spec, func() bool { return busy }, fresh)
	if err != nil {
		if errors.Is(err, sandbox.ErrNotAgent) {
			return "", err
		}
		if missing, ierr := sandbox.ImageMissing(ctx, a.docker(), spec.Image); ierr == nil && missing {
			from := "$ATL_SANDBOX_IMAGE"
			if spec.Image == sandbox.DefaultImage {
				from = "default"
			}
			return "", missingImage(spec.Image, from)
		}
		return "", fmt.Errorf("the assistant's container (image %s): %w", spec.Image, err)
	}
	if stale {
		a.warnf("the assistant's container predates a change (image or atl paths); it is replaced once the assistant is closed")
	}
	return container, nil
}

// assistantArgv is the assistant's command: claude, continuing the last
// conversation if there is one, with the optional initial prompt.
func assistantArgv(continueLast bool, prompt []string) []string {
	argv := []string{"claude"}
	if continueLast {
		argv = append(argv, "--continue")
	}
	return append(argv, prompt...)
}

// claudeProjectKey encodes an absolute directory the way Claude Code names
// its per-project transcript dir under <config>/projects: every character
// other than [A-Za-z0-9] becomes '-' (two for a character outside the BMP,
// which JavaScript sees as two UTF-16 code units).
func claudeProjectKey(dir string) string {
	var b strings.Builder
	for _, r := range dir {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r > 0xFFFF:
			b.WriteString("--")
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// claudeProjectDirs returns Claude Code's transcript dirs for dir in the
// Claude config dir cfg: <cfg>/projects/<key> for dir's absolute path and,
// if different, its symlink-resolved path (nil if cfg is "").
func claudeProjectDirs(cfg, dir string) []string {
	if cfg == "" {
		return nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil
	}
	forms := []string{abs}
	if r, err := filepath.EvalSymlinks(abs); err == nil && r != abs {
		forms = append(forms, r)
	}
	var out []string
	for _, d := range forms {
		out = append(out, filepath.Join(cfg, "projects", claudeProjectKey(d)))
	}
	return out
}

// hasClaudeConversation reports whether Claude Code has a recorded
// conversation (a *.jsonl transcript) for dir, under its absolute or
// symlink-resolved path, in the Claude config dir cfg.
func hasClaudeConversation(cfg, dir string) bool {
	for _, d := range claudeProjectDirs(cfg, dir) {
		entries, _ := os.ReadDir(d)
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
				return true
			}
		}
	}
	return false
}

// inClaudeProject reports whether transcript sits directly in one of
// Claude Code's transcript dirs for dir in the Claude config dir cfg
// (claudeProjectDirs), comparing symlink-resolved paths too: only then can
// `claude --resume <id>` run in dir with that config dir find that
// conversation.
func inClaudeProject(cfg, dir, transcript string) bool {
	if !filepath.IsAbs(transcript) {
		return false
	}
	td := filepath.Dir(filepath.Clean(transcript))
	rtd, rerr := filepath.EvalSymlinks(td)
	for _, p := range claudeProjectDirs(cfg, dir) {
		if td == p {
			return true
		}
		if rerr == nil {
			if rp, err := filepath.EvalSymlinks(p); err == nil && rp == rtd {
				return true
			}
		}
	}
	return false
}

// assistantLock is the lock that serialises launches of the assistant: in
// the state dir, which no container sees (the assistant dir is its
// container's to write: a lock there could be a symlink to anywhere).
func (a *App) assistantLock() string {
	return filepath.Join(a.Paths.StateDir, "assistant.lock")
}

// assistantUntrusted are the files of the assistant dir that Claude Code
// reads as instructions, settings, tools or hooks and atl never writes: its
// container may have written them (the assistant dir is read-write there),
// and next to atl (the sandbox off) they would act on the host.
var assistantUntrusted = []string{"CLAUDE.local.md", ".mcp.json", ".claude/settings.local.json",
	".claude/skills", ".claude/agents", ".claude/commands"}

// quarantineAssistant moves, before the assistant runs next to atl (the
// sandbox off), what of assistantUntrusted is in its dir, and its
// .claude/settings.json unless it is what atl would write afresh
// (foreignAssistantSettings; atl then writes it from scratch), or all of .claude
// when that is not a plain directory, into a new dir
// <state dir>/assistant-quarantine/<time>/, at the same relative paths, and
// says so in one warning. Nothing is deleted or followed: a symlink is moved
// as it is. A move that fails is a warning too.
func (a *App) quarantineAssistant(dir string) {
	cands := assistantUntrusted
	if fi, err := os.Lstat(filepath.Join(dir, ".claude")); err == nil && !fi.IsDir() {
		cands = []string{"CLAUDE.local.md", ".mcp.json", ".claude"}
	} else if a.foreignAssistantSettings(dir) {
		cands = append(slices.Clone(cands), ".claude/settings.json")
	}
	q := filepath.Join(a.Paths.StateDir, "assistant-quarantine", a.now().UTC().Format("20060102T150405.000000000Z"))
	var moved []string
	for _, rel := range cands {
		src := filepath.Join(dir, rel)
		if _, err := os.Lstat(src); err != nil {
			continue
		}
		dst := filepath.Join(q, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			a.warnf("assistant: quarantine %s: %v", rel, err)
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			a.warnf("assistant: quarantine %s: %v", rel, err)
			continue
		}
		moved = append(moved, rel)
	}
	if len(moved) > 0 {
		a.warnf("assistant: moved %s of %s to %s: its container could have written them, and next to atl they would act on the host",
			strings.Join(moved, ", "), dir, q)
	}
}

// foreignAssistantSettings reports whether the assistant dir's
// .claude/settings.json is there and is not what atl would write afresh
// (gen.WriteAssistantFiles into an empty dir): next to atl, atl owns that
// file and does not merge it, since the assistant's container could have
// added hooks or allow rules to it. A file atl cannot read as a regular
// file counts as foreign; one it cannot render the fresh form for does not
// (the launch then fails writing it, as before).
func (a *App) foreignAssistantSettings(dir string) bool {
	path := filepath.Join(dir, ".claude", "settings.json")
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if !fi.Mode().IsRegular() {
		return true
	}
	have, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	tmp, err := os.MkdirTemp("", "atl-assistant-")
	if err != nil {
		return false
	}
	defer os.RemoveAll(tmp)
	if err := gen.WriteAssistantFiles(tmp, a.hookBin(), a.Sandbox != nil); err != nil {
		return false
	}
	fresh, err := os.ReadFile(filepath.Join(tmp, ".claude", "settings.json"))
	return err == nil && !bytes.Equal(have, fresh)
}
