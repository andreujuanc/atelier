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
//
// Every allow rule names a command that is read-only whatever its
// arguments. `atl notes` is not (`--set` replaces the notes, `--clear`
// clears them), and a pattern cannot reliably tell `atl notes X` from
// `atl notes X --set`
// (quoting, `--set=true`, extra spaces, flag order all change the
// string), so all of `atl notes` asks and the notes are read with
// `atl show <task> --notes` (or --json) under the `atl show` allow rule.
// Ask rules win over allow rules, also over a user's global ones.
var (
	assistantAllow = []string{
		"Bash(atl ls *)", "Bash(atl show *)", "Bash(atl transcript *)",
		"Bash(atl repos *)", "Bash(atl --help *)", "Read", "Grep", "Glob",
	}
	assistantAsk = []string{
		"Bash(atl new *)", "Bash(atl add *)", "Bash(atl status *)", "Bash(atl session *)",
		"Bash(atl resume *)", "Bash(atl pr *)", "Bash(atl done *)", "Bash(atl cancel *)",
		"Bash(atl note *)", "Bash(atl notes *)", "Bash(atl open *)", "Bash(atl rename *)",
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
		"## Your role: task management ONLY",
		"",
		"Your ONLY job is managing atelier tasks with `atl` commands: status and catch-up summaries, notes,",
		"which repos a task has, creating/renaming/finishing/cancelling tasks, resuming sessions, PRs.",
		"",
		"You do NOT do code work. Do NOT read source code in depth, review code, propose code changes, debug,",
		"or offer to implement anything. You run in your own folder: any code you touched here would not be in",
		"the task's git worktrees. You may glance at `git log` / `atl show` output only to summarise progress.",
		"",
		"When the user wants code work (or asks about code details), say that belongs in the task's own Claude",
		"session and tell them how to get there: in the atl TUI select the task and press Enter (or run",
		"`atl open <task>`). The task session sees the task's worktrees.",
		"",
		"The user prefers simple, one-step-at-a-time guidance. Keep answers short.",
		"",
		"## \"Let's talk about task <slug>.\"",
		"",
		"When greeted with \"Let's talk about task <slug>.\", immediately gather status (no need to ask):",
		"`atl show <slug> --json` (includes the notes), `atl transcript <slug> --tail 30`. Then reply with a short",
		"catch-up, one line each: goal, done, pending, blockers, suggested next step. Then ask what the user",
		"wants to do with the task (e.g. resume it, add a repo, save a note, finish it).",
		"",
		"## Rules",
		"",
		"- Reading is free: run the read-only commands below whenever useful.",
		"- ALWAYS ask for explicit confirmation before any command that changes state (`atl new`, `add`,",
		"  `status`, `rename`, `session`, `resume`, `open`, `pr`, `done`, `cancel`, `note`, `notes --set`,",
		"  `notes --clear`).",
		"  First state the exact command you want to run, wait for the user's yes, then run it.",
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
		"  ahead, last commit, pushed), sessions (state, last event, transcript path), the notes text.",
		"- `atl show <task> --notes` — only the task's notes: one free-form text per task that the user edits",
		"  in the atl TUI; `atl note` appends dated lines at its end. Read notes this way (runs without asking).",
		"- `atl transcript <task> [--session ID] [--tail N]` — last N turns (default 20) of a session's",
		"  conversation; default is the most recent session with a transcript.",
		"- `atl notes <task>` — prints the same notes text, but asks for permission here (its `--set` and",
		"  `--clear` forms write), so prefer `atl show <task> --notes`.",
		"- `atl repos` — git repos available in $ATL_REPOS.",
		"- `atl diff <task>` — git diff of each rw repo against its base commit (read-only, may ask permission).",
		"- `atl --help`, `atl <command> --help` — usage.",
		"",
		"Changes state (ALWAYS confirm first):",
		"",
		"- `atl note <task> <text> --author assistant` — append a line `- YYYY-MM-DD [assistant] <text>` to the",
		"  task's notes (the user's text above it is kept). This is how you add to the notes.",
		"- `atl notes <task> --set` — REPLACE the whole notes text with stdin (an empty stdin is an error), e.g.",
		"  `atl notes X --set <<'EOF'` … `EOF`. It overwrites what the user wrote: only when the user asks,",
		"  show them the full new text and get their yes first. To add something, use `atl note` instead.",
		"- `atl notes <task> --clear` — DELETE the whole notes text; only when the user explicitly asks.",
		"- `atl new <title...> [--project P] [--repo name:rw|ro ...] [--base auto|default|production|<branch>] [--backlog] [-m description]`",
		"  — create a task with worktrees. `--base`: auto (default; origin/dev, else develop, else the default",
		"  branch) for feature work, `default` (or `production`) for a production patch from origin's default branch, or a branch name.",
		"- `atl add <task> <repo>[:rw|ro]` — add a repo to an open task.",
		"- `atl status <task> <backlog|active|review|done|cancelled>` — set the status only (no other effects).",
		"- `atl rename <task> <new title...>` — change the task's title only; the slug, branch `atl/<slug>`, tmux",
		"  session and dirs keep the original name (refer to the task by its slug afterwards too).",
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
		"2. The notes text in that output (`notes`); `atl show X --notes` prints just the notes.",
		"3. `atl transcript X --tail 30` (if the task has several sessions, also the others:",
		"   `atl transcript X --session ID --tail 30`).",
		"4. Optionally a glance at `git log` in the worktree paths from `atl show` (no in-depth code reading).",
		"5. Summarise briefly: goal, what was done, what is pending, blockers, suggested next step.",
		"",
		"After a status discussion, offer to append a short note, e.g.",
		"`atl note X \"<summary>\" --author assistant` — and run it only after the user agrees.",
	)
	return b.String()
}
