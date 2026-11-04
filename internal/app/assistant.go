package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/gen"
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
// calls (from any process) are serialised by a lock file in the assistant
// dir. The window is untagged, so Recover, Done and Cancel never touch it,
// and it has no state-tracking hooks.
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
	unlock, err := lockFile(ctx, filepath.Join(dir, ".launch.lock"), assistantLockWait,
		"another atl is launching the assistant")
	if err != nil {
		return nil, "", fmt.Errorf("assistant: %w", err)
	}
	defer unlock()
	if err := gen.WriteAssistantFiles(dir, a.AtlBin); err != nil {
		return nil, "", fmt.Errorf("assistant: %w", err)
	}

	alive, err := a.Tmux.WindowAlive(ctx, AssistantSession, AssistantWindow)
	if err != nil {
		return nil, "", fmt.Errorf("assistant: %w", err)
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
	if _, err := a.Tmux.NewWindow(ctx, tmux.WindowSpec{
		Session: AssistantSession,
		Name:    AssistantWindow,
		Dir:     dir,
		Env:     map[string]string{"ATL_ASSISTANT": "1"},
		Argv:    assistantArgv(hasClaudeConversation(dir), prompt),
	}); err != nil {
		return nil, "", fmt.Errorf("assistant: %w", err)
	}
	return a.Tmux.AttachCmd(AssistantSession), "", nil
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

// claudeProjectDirs returns Claude Code's transcript dirs for dir:
// <config>/projects/<key> for dir's absolute path and, if different, its
// symlink-resolved path (nil if the config dir is unknown).
func claudeProjectDirs(dir string) []string {
	cfg := config.ClaudeConfigDir()
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
// symlink-resolved path.
func hasClaudeConversation(dir string) bool {
	for _, d := range claudeProjectDirs(dir) {
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
// Claude Code's transcript dirs for dir (claudeProjectDirs), comparing
// symlink-resolved paths too: only then can `claude --resume <id>` run in
// dir find that conversation.
func inClaudeProject(dir, transcript string) bool {
	if !filepath.IsAbs(transcript) {
		return false
	}
	td := filepath.Dir(filepath.Clean(transcript))
	rtd, rerr := filepath.EvalSymlinks(td)
	for _, p := range claudeProjectDirs(dir) {
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
