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
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/gen"
	"github.com/andreujuanc/atelier/internal/nofollow"
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
// and it has no state-tracking hooks. Next to atl (the sandbox off), after
// the assistant ran in its container, that container is removed and what
// it may have written in the dir is moved aside first
// (leaveAssistantContainer).
//
// With the sandbox on, Claude runs in the assistant's own container (see
// ensureAssistantSandbox), which every call makes sure of, gate included,
// and the window runs docker exec into it, as a task session's window does.
// A new window starts in a container where nothing of an earlier window
// runs (see endAssistantProcs). With a live window, a failure to make sure
// of the container (Docker gone a moment, no token) is a warning: the
// terminal attaches all the same.
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
		if err := a.leaveAssistantContainer(ctx, dir); err != nil {
			return nil, "", fmt.Errorf("assistant: %w", err)
		}
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
			if !alive {
				return nil, "", fmt.Errorf("assistant: %w", err)
			}
			a.warnf("assistant: %v (attaching to the assistant that runs)", err)
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
// place atl runs in, is passed as it is: the assistant manages it. Its
// image is never pulled (--pull=never): the assistant runs the image's
// claude with its token, so the image must be one the user built or
// pulled, not whatever a registry has under that name.
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
		NoPull:  true,
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
// a reboot, and replaced when its spec changed (assistantSpec: the image,
// the paths it mounts, $HOME, $LANG, $LC_ALL, $TZ, $ATL_CONTEXT, the user)
// unless busy (the assistant's window is alive). When not busy, what an
// earlier window left running in it is ended first (endAssistantProcs).
// Before the container is made or started, the marker that it ran is
// written (assistantMarker). It returns the container's full ID. No task
// closes the container; a launch next to atl removes it
// (leaveAssistantContainer), and so may you (docker rm -f).
func (a *App) ensureAssistantSandbox(ctx context.Context, busy bool) (string, error) {
	if a.Paths.Workspace == "" {
		return "", errors.New("the assistant's container needs a workspace")
	}
	if err := a.checkSandboxNeeds(ctx); err != nil {
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
	if !busy {
		if err := a.endAssistantProcs(ctx); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(a.assistantMarker(), []byte(a.now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		return "", err
	}
	fresh := func() error {
		if missing, err := sandbox.ImageMissing(ctx, a.docker(), spec.Image); err == nil && missing {
			return assistantImageMissing(spec.Image)
		}
		return a.seedClaudeDirAt(a.Paths.AssistantClaudeDir(), a.Paths.AssistantDir(), "the assistant")
	}
	container, stale, err := sandbox.Ensure(ctx, a.docker(), spec, func() bool { return busy }, fresh)
	if err != nil {
		if errors.Is(err, sandbox.ErrNotAgent) {
			return "", err
		}
		if missing, ierr := sandbox.ImageMissing(ctx, a.docker(), spec.Image); ierr == nil && missing {
			return "", assistantImageMissing(spec.Image)
		}
		return "", fmt.Errorf("the assistant's container (image %s): %w", spec.Image, err)
	}
	if stale {
		a.warnf("the assistant's container predates a change (image, paths, $HOME, $LANG, $LC_ALL, $TZ or $ATL_CONTEXT); it is replaced once the assistant is closed")
	}
	return container, nil
}

// assistantImageMissing is the one line that says the assistant's image
// is not on the Docker daemon, which atl does not pull for it.
func assistantImageMissing(image string) error {
	if image == sandbox.DefaultImage {
		return missingImage(image, "default")
	}
	return errors.New("task image " + image + " (from $ATL_SANDBOX_IMAGE) is not on the Docker daemon, and atl does not pull it for the assistant " +
		"(it runs the image's claude): build or pull it yourself")
}

// endAssistantProcs ends everything that runs in the assistant's container
// (only atl's own, see sandbox.Agent) by stopping it; ensureAssistantSandbox
// then starts it again. It runs when the assistant's window is gone: dockerd
// keeps what a dead docker exec client started, so the old claude would go
// on with nobody attached, next to a second `claude --continue` on the same
// conversation, and what it started in the background would keep asking
// the gate. Nothing else runs there (no task uses it), so the container
// stops whole, which no process in it can dodge. A container that is not
// there or not running has nothing to end.
func (a *App) endAssistantProcs(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	c, err := sandbox.Agent(ctx, a.docker(), a.assistantID())
	switch {
	case sandbox.IsNoSuch(err):
		return nil
	case err != nil:
		return err
	case !c.Running:
		return nil
	}
	if _, err := a.docker()(ctx, "stop", "-t", "2", c.ID); err != nil && !sandbox.IsNoSuch(err) {
		return fmt.Errorf("end what the assistant's last window left running in its container: %w", err)
	}
	return nil
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

// assistantMarker is the file that says the assistant ran in its
// container since its dir was last cleaned for a launch next to atl
// (leaveAssistantContainer): in the state dir, which no container sees.
func (a *App) assistantMarker() string {
	return filepath.Join(a.Paths.StateDir, "assistant-in-container")
}

// assistantKept are the files of the assistant dir that atl writes itself
// and rewrites right after leaveAssistantContainer: all else there is
// moved. .claude/settings.json only when it is what atl would write afresh
// (foreignAssistantSettings), as the merge would keep what else is in it.
const (
	assistantKeptMD       = "CLAUDE.md"
	assistantKeptSettings = "settings.json"
)

// leaveAssistantContainer readies the assistant dir for a launch next to
// atl (the sandbox off) after the assistant ran in its container
// (assistantMarker present; without it, nothing is done): its container
// is removed, its tmux session (whose window ran in it) ended and its gate
// stopped, then everything in the dir that atl does not write
// itself (assistantKept) is moved, symlinks as they are, into a new dir
// <state dir>/assistant-quarantine/<time>/, at the same relative paths, and
// one warning names what moved; then the marker goes. The container had
// the dir read-write: what it left there (instructions, settings, MCP
// servers, a .git with hooks or core.fsmonitor that any git run there
// would execute) would act on the host. If Docker cannot confirm that the
// container is gone, or a file cannot be moved, it fails (the marker
// stays): the assistant does not start next to atl.
func (a *App) leaveAssistantContainer(ctx context.Context, dir string) error {
	marker := a.assistantMarker()
	if _, err := os.Lstat(marker); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := a.removeAssistantContainer(ctx); err != nil {
		return fmt.Errorf("the assistant ran in its container, which can write to %s, and atl cannot make sure that container is gone (%s): "+
			"start Docker so that atl removes it, then start the assistant again", dir, strings.Join(strings.Fields(err.Error()), " "))
	}
	if a.Tmux.HasSession(ctx, AssistantSession) {
		if err := a.Tmux.KillSession(ctx, AssistantSession); err != nil {
			return err
		}
	}
	sandbox.Stop(a.Paths.AssistantRunDir())
	moved, q, err := a.quarantineAssistant(dir)
	if len(moved) > 0 {
		a.warnf("assistant: moved %s of %s to %s: its container could have written them, and next to atl they would act on the host",
			strings.Join(moved, ", "), dir, q)
	}
	if err != nil {
		return err
	}
	return os.Remove(marker)
}

// removeAssistantContainer removes the assistant's container (only atl's
// own, see sandbox.Agent); one that is not there is fine.
func (a *App) removeAssistantContainer(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	c, err := sandbox.Agent(ctx, a.docker(), a.assistantID())
	if sandbox.IsNoSuch(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := a.docker()(ctx, "rm", "-f", c.ID); err != nil && !sandbox.IsNoSuch(err) {
		return err
	}
	return nil
}

// quarantineAssistant moves everything in the assistant dir but
// assistantKept into a new quarantine dir (see leaveAssistantContainer)
// and returns what it moved, relative to dir, and that dir. Nothing is
// followed: dir and its .claude are opened with O_NOFOLLOW, and each entry
// is renamed as it is (a symlink stays a symlink; a .claude that is not a
// directory goes whole). It stops at the first move that fails.
func (a *App) quarantineAssistant(dir string) (moved []string, q string, err error) {
	q = filepath.Join(a.Paths.StateDir, "assistant-quarantine", a.now().UTC().Format("20060102T150405.000000000Z"))
	keepSettings := !a.foreignAssistantSettings(dir)
	d, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, q, nil
	}
	if err != nil {
		return nil, q, fmt.Errorf("assistant: open %s: %w", dir, err)
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, q, fmt.Errorf("assistant: read %s: %w", dir, err)
	}
	slices.Sort(names)
	move := func(fd int, name, rel string) error {
		dst := filepath.Join(q, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := unix.Renameat(fd, name, unix.AT_FDCWD, dst); err != nil {
			return fmt.Errorf("assistant: move %s of %s to %s: %w", rel, dir, q, err)
		}
		moved = append(moved, rel)
		return nil
	}
	dfd := int(d.Fd())
	for _, name := range names {
		switch name {
		case assistantKeptMD:
			if regularAt(dfd, name) {
				continue
			}
		case ".claude":
			c, err := openDirAt(dfd, name)
			if err != nil { // a symlink, or not a directory
				break
			}
			inner, err := c.Readdirnames(-1)
			if err != nil {
				c.Close()
				return moved, q, fmt.Errorf("assistant: read %s: %w", filepath.Join(dir, name), err)
			}
			slices.Sort(inner)
			for _, n := range inner {
				if n == assistantKeptSettings && keepSettings && regularAt(int(c.Fd()), n) {
					continue
				}
				if err := move(int(c.Fd()), n, filepath.Join(name, n)); err != nil {
					c.Close()
					return moved, q, err
				}
			}
			c.Close()
			continue
		}
		if err := move(dfd, name, name); err != nil {
			return moved, q, err
		}
	}
	return moved, q, nil
}

// regularAt reports whether name in the directory fd is a regular file
// (not following a symlink).
func regularAt(fd int, name string) bool {
	var st unix.Stat_t
	return unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFREG
}

// openDirAt opens the directory name in the directory fd, not following
// a symlink.
func openDirAt(fd int, name string) (*os.File, error) {
	cfd, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(cfd), name), nil
}

// maxAssistantSettings bounds what foreignAssistantSettings reads; a
// larger file is foreign.
const maxAssistantSettings = 1 << 20

// foreignAssistantSettings reports whether the assistant dir's
// .claude/settings.json is there and is not what atl would write afresh
// (gen.WriteAssistantFiles into an empty dir): next to atl, atl owns that
// file and does not merge it, since the assistant's container could have
// added hooks or allow rules to it. It is read without following a
// symlink, as a regular file only, at most maxAssistantSettings bytes; a
// file atl cannot read so counts as foreign, and so does one it cannot
// render the fresh form for.
func (a *App) foreignAssistantSettings(dir string) bool {
	d, err := nofollow.Open(dir)
	if err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	defer d.Close()
	if _, err := d.Lstat(".claude/settings.json"); errors.Is(err, os.ErrNotExist) {
		return false
	}
	have, err := d.ReadFile(".claude/settings.json", maxAssistantSettings)
	if err != nil {
		return true
	}
	tmp, err := os.MkdirTemp("", "atl-assistant-")
	if err != nil {
		return true
	}
	defer os.RemoveAll(tmp)
	if err := gen.WriteAssistantFiles(tmp, a.hookBin(), a.Sandbox != nil); err != nil {
		return true
	}
	fresh, err := os.ReadFile(filepath.Join(tmp, ".claude", "settings.json"))
	return err != nil || !bytes.Equal(have, fresh)
}
