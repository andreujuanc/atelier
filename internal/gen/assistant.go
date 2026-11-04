package gen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AssistantGuardEvent is the `atl hook` event that keeps the atelier
// assistant from editing files outside its own directory.
const AssistantGuardEvent = "AssistantGuard"

// assistantAllow, assistantAsk and assistantDeny are the permission rules
// atelier owns in the assistant's settings: read-only commands run without
// asking, every command that changes state asks first, and common
// repo-mutating commands are denied, so that a user's global allow rules
// cannot let the assistant change repos.
var (
	assistantAllow = []string{
		"Bash(atl ls *)", "Bash(atl show *)", "Bash(atl transcript *)", "Bash(atl notes *)",
		"Bash(atl repos *)", "Bash(atl --help *)", "Read", "Grep", "Glob",
	}
	assistantAsk = []string{
		"Bash(atl new *)", "Bash(atl add *)", "Bash(atl status *)", "Bash(atl session *)",
		"Bash(atl resume *)", "Bash(atl pr *)", "Bash(atl done *)", "Bash(atl cancel *)",
		"Bash(atl note *)", "Bash(atl open *)",
	}
	assistantDeny = []string{
		"Bash(git commit *)", "Bash(git push *)", "Bash(git checkout *)", "Bash(git switch *)",
		"Bash(git reset *)", "Bash(git rebase *)", "Bash(git merge *)", "Bash(git stash *)",
		"Bash(git restore *)", "Bash(git clean *)", "Bash(git am *)", "Bash(git apply *)",
		"Bash(git cherry-pick *)", "Bash(git branch -D *)", "Bash(git branch -d *)",
		"Bash(sed -i *)", "Bash(rm *)", "Bash(mv *)", "Bash(cp *)", "Bash(tee *)",
	}
	// retiredAssistantRules lists allow/ask rules earlier atelier versions
	// wrote that are no longer wanted; they are removed on rewrite. Earlier
	// versions spelled Bash rules "Bash(atl ls:*)".
	retiredAssistantRules = colonForms(assistantAllow, assistantAsk)
)

// colonForms returns the legacy "Bash(cmd:*)" spelling of every
// "Bash(cmd *)" rule in lists.
func colonForms(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		for _, r := range l {
			if strings.HasPrefix(r, "Bash(") && strings.HasSuffix(r, " *)") {
				out = append(out, strings.TrimSuffix(r, " *)")+":*)")
			}
		}
	}
	return out
}

// WriteAssistantFiles writes the atelier assistant's CLAUDE.md (always
// regenerated) and merges its .claude/settings.json in dir, creating dir if
// needed. The settings merge keeps every user key, hook and permission rule
// and replaces only atelier's own (see WriteSettings for the hook rules).
func WriteAssistantFiles(dir, atlBin string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("gen: create assistant dir: %w", err)
	}
	path := filepath.Join(dir, "CLAUDE.md")
	if err := writeAtomic(path, []byte(AssistantClaudeMD())); err != nil {
		return fmt.Errorf("gen: write %s: %w", path, err)
	}
	return writeAssistantSettings(dir, atlBin)
}

func writeAssistantSettings(dir, atlBin string) error {
	path, root, err := loadSettings(dir)
	if err != nil {
		return err
	}
	backedUp := false
	backup := func() error {
		if backedUp {
			return nil
		}
		backedUp = true
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("gen: back up %s: %w", path, err)
		}
		if err := os.WriteFile(path+".bak", data, 0o644); err != nil {
			return fmt.Errorf("gen: back up %s: %w", path, err)
		}
		return nil
	}

	perms, ok := root["permissions"].(map[string]any)
	if !ok {
		if _, present := root["permissions"]; present {
			if err := backup(); err != nil {
				return err
			}
		}
		perms = map[string]any{}
	}
	perms["defaultMode"] = "default"
	set := func(lists ...[]string) map[string]bool {
		m := map[string]bool{}
		for _, l := range lists {
			for _, r := range l {
				m[r] = true
			}
		}
		return m
	}
	// In allow and ask every atelier rule (current or retired) is replaced;
	// in deny only atelier's deny rules, so a user's deny entries are kept
	// even when they match an atelier allow/ask rule.
	ownedAllowAsk := set(assistantAllow, assistantAsk, assistantDeny, retiredAssistantRules)
	for _, l := range []struct {
		key   string
		rules []string
		owned map[string]bool
	}{
		{"allow", assistantAllow, ownedAllowAsk},
		{"ask", assistantAsk, ownedAllowAsk},
		{"deny", assistantDeny, set(assistantDeny)},
	} {
		list, ok := perms[l.key].([]any)
		if !ok {
			if _, present := perms[l.key]; present {
				if err := backup(); err != nil {
					return err
				}
			}
			list = nil
		}
		kept := make([]any, 0, len(list)+len(l.rules))
		for _, v := range list {
			if s, ok := v.(string); ok && l.owned[s] {
				continue
			}
			kept = append(kept, v)
		}
		for _, r := range l.rules {
			kept = append(kept, r)
		}
		perms[l.key] = kept
	}
	root["permissions"] = perms

	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	setAtelierHook(hooks, "PreToolUse", "Edit|Write|MultiEdit|NotebookEdit", AssistantGuardEvent, atlBin)
	root["hooks"] = hooks
	return saveSettings(path, root)
}

// AssistantClaudeMD renders the atelier assistant's CLAUDE.md.
func AssistantClaudeMD() string {
	var b strings.Builder
	w := func(s ...string) {
		for _, l := range s {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	w("<!-- generated by atelier; rewritten every time the assistant starts — do not edit by hand -->",
		"",
		"# atelier assistant",
		"",
		"You are the atelier assistant. atelier (`atl`) groups Claude Code sessions by task: each task has a",
		"slug, a status (backlog, active, review, done, cancelled), git worktrees on branch `atl/<slug>` and",
		"Claude Code sessions running in tmux.",
		"",
		"You help the user manage their atelier tasks: the status of stalled tasks, catching up on where they",
		"left off, organising (statuses, notes, new tasks, adding repos). You do NOT write code: never edit,",
		"create or delete files in repos, worktrees or task dirs — the task's own Claude sessions do that work.",
		"",
		"The user prefers simple, one-step-at-a-time guidance. Keep answers short.",
		"",
		"## Rules",
		"",
		"- Reading is free: run the read-only commands below whenever useful.",
		"- ALWAYS ask for explicit confirmation before any command that changes state (`atl new`, `add`,",
		"  `status`, `session`, `resume`, `open`, `pr`, `done`, `cancel`, `note`). First state the exact command",
		"  you want to run, wait for the user's yes, then run it.",
		"- Never edit files in repos, worktrees or task dirs, by any means (no Edit/Write tools, no shell",
		"  redirects, `sed -i`, `git commit`, `git checkout`, ...). Edits outside this directory are blocked by",
		"  an atelier hook. You may keep scratch notes in this directory (not in CLAUDE.md or .claude/).",
		"- Git in worktrees is read-only: `git log`, `git diff`, `git status`, `git show` only.",
		"- Common repo-changing commands are denied in this directory's settings (`git commit`, `push`,",
		"  `checkout`, `switch`, `reset`, `rebase`, `merge`, `stash`, `restore`, `clean`, `am`, `apply`,",
		"  `cherry-pick`, `branch -d/-D`, `sed -i`, `rm`, `mv`, `cp`, `tee`): shell commands cannot be fully",
		"  sandboxed, so these rules keep the user's global allow rules from letting you change repos. Do not",
		"  try to work around them.",
		"- When unsure which task the user means, run `atl ls` and ask.",
		"",
		"## atl command reference",
		"",
		"Read-only (run freely):",
		"",
		"- `atl ls [--status S[,S...]|all] [--json]` — list tasks (default: backlog, active, review).",
		"- `atl show <task> [--json]` — task details: repos with worktree paths and git state (dirty, commits",
		"  ahead, last commit, pushed), sessions (state, last event, transcript path), notes.",
		"- `atl transcript <task> [--session ID] [--tail N]` — last N turns (default 20) of a session's",
		"  conversation; default is the most recent session with a transcript.",
		"- `atl notes <task>` — the task's notes, newest first.",
		"- `atl repos` — git repos available in $ATL_REPOS.",
		"- `atl diff <task>` — git diff of each rw repo against its base commit (read-only, may ask permission).",
		"- `atl --help`, `atl <command> --help` — usage.",
		"",
		"Changes state (ALWAYS confirm first):",
		"",
		"- `atl note <task> <text> --author assistant` — add a note to the task.",
		"- `atl new <title...> [--project P] [--repo name:rw|ro ...] [--backlog] [-m description]` — create a",
		"  task with worktrees.",
		"- `atl add <task> <repo>[:rw|ro]` — add a repo to an open task.",
		"- `atl status <task> <backlog|active|review|done|cancelled>` — set the status only (no other effects).",
		"- `atl session <task>` — start a new Claude session in the task (not attached).",
		"- `atl resume <task>` — resume the task's stopped Claude sessions.",
		"- `atl open <task>` — start/resume the task's session and attach the terminal (better done by the user",
		"  from the atelier TUI).",
		"- `atl pr <task>` — open GitHub PRs for pushed branches; status → review.",
		"- `atl done <task>` — finish: kill its tmux session, remove worktrees, status done.",
		"- `atl cancel <task>` — same as done, status cancelled.",
		"",
		"## \"What's the status of X?\" / \"Where did I leave off?\"",
		"",
		"1. `atl show X --json`",
		"2. `atl notes X`",
		"3. `atl transcript X --tail 30` (if the task has several sessions, also the others:",
		"   `atl transcript X --session ID --tail 30`).",
		"4. Optionally read-only git in the worktree paths from `atl show` (`git log`, `git diff`, `git status`).",
		"5. Summarise briefly: goal, what was done, what is pending, blockers, suggested next step.",
		"",
		"After a status discussion, offer to save a short note, e.g.",
		"`atl note X \"<summary>\" --author assistant` — and run it only after the user agrees.",
	)
	return b.String()
}
