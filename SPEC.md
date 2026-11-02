# atelier — task studio for parallel Claude Code work

## Your role
You are the ORCHESTRATOR. Keep your own context small.
- You do NOT write implementation code yourself.
- Phase 0: write docs/PLAN.md (package layout, data model, interfaces between packages, CLI surface). Stop and show me PLAN.md before Phase 1.
- Phase 1+: for each milestone below, spawn ONE subagent (Task tool) with a self-contained brief:
  goal, files it owns, interfaces from PLAN.md it must respect, acceptance criteria.
  Subagents don't share your context — the brief must contain everything they need.
- Each implementer subagent must: write code + tests, run `go build ./... && go test ./...`, and reply with a ≤10-line summary (files touched, decisions, open issues). No code in the reply.
- After each milestone, spawn a REVIEWER subagent: review the diff against this spec and PLAN.md, report problems only. If problems, spawn a fix subagent.
- Keep docs/PROGRESS.md updated after every milestone (done / in progress / decisions). If you crash or restart, resume from PROGRESS.md.
- Commit after each milestone that passes review.

## What it is
A terminal tool that groups Claude Code sessions by task. Each task has its own git worktrees across one or more repos. No web UI, no browser, no custom chat rendering — the real Claude Code TUI runs inside tmux; atelier only orchestrates and tracks state.

## Stack
- Go, single static binary `atl`
- TUI: Bubble Tea + Lip Gloss
- SQLite via modernc.org/sqlite (no cgo)
- Shells out to: git, tmux, claude, gh

## Concepts
- Repo: a canonical clone in $ATL_REPOS (default ~/repos/<name>). Never edited directly.
- Project: named group of repos + optional setup/teardown scripts. Config: ~/.config/atelier/projects/<name>.toml
- Task: title, slug, status (backlog | active | review | done | cancelled), project, repos each with mode rw|ro, branch name `atl/<slug>`, port offset, created/updated.
- Session: belongs to a task. claude_session_id, tmux target, state (running | waiting | idle | ended | stopped), last_event_at.

## Filesystem
- Task dir: $ATL_TASKS (default ~/tasks)/<slug>/
- rw repo → `git -C $ATL_REPOS/<repo> worktree add <taskdir>/<repo> -b atl/<slug>`
- ro repo → NOT a worktree; passed to claude via `--add-dir $ATL_REPOS/<repo>`
- atelier generates <taskdir>/CLAUDE.md: task title, description, list of repos with paths and rw/ro mode, port offset.
- atelier generates <taskdir>/.claude/settings.json with hooks (below).
- DB: $XDG_DATA_HOME/atelier/atelier.db (default ~/.local/share/atelier/atelier.db)

## Claude Code hooks (state tracking)
Every hook runs `atl hook <event>`, which reads the hook JSON from stdin (session_id, cwd, hook_event_name, tool input when present) and updates the DB:
- SessionStart → create/attach session row, state=idle, store session_id
- UserPromptSubmit → running
- Notification → waiting (needs my input)
- Stop → idle (turn finished)
- SessionEnd → ended
- PreToolUse on Edit|Write|MultiEdit|NotebookEdit → block (exit code 2 + message) if the target path is inside any ro repo path. This enforces read-only.
`atl hook` must be fast (<50ms), never block on errors except the ro guard, and log failures to $XDG_STATE_HOME/atelier/hook.log.

## Sessions / tmux
- One tmux session per task: `atl-<slug>`. One tmux window per Claude session.
- New session: window runs `claude` in <taskdir> with `--add-dir` for each ro repo.
- Resume: window runs `claude --resume <claude_session_id>` with the same flags.
- Attach: if inside tmux → switch-client, else → attach.

## Projects / scripts / ports
- Project toml: repos list (default modes), setup (command run in each rw worktree after creation), teardown (run before worktree removal), env.
- Each task gets a unique port offset (0, 100, 200...). Exported to scripts and written into CLAUDE.md as ATL_PORT_OFFSET so docker compose files can use ${ATL_PORT_OFFSET}.

## Crash recovery
On any atl start: sessions whose tmux window no longer exists → state=stopped. `atl resume <task>` recreates the tmux session and resumes every non-ended session with `claude --resume`.

## CLI (every TUI action must also exist as CLI, for scripting from sway/fuzzel)
atl new <title> --project P [--repo name:rw|ro ...] [--backlog]
atl ls [--status S] [--json]
atl open <task>            # ensure tmux session, attach
atl session <task>         # new Claude session in task
atl resume <task>
atl status <task> <status>
atl diff <task>            # git diff per rw repo, paged
atl pr <task>              # gh pr create per rw repo that has commits; status → review
atl done <task> [--force]  # teardown, remove worktrees (refuse if uncommitted unless --force), kill tmux, status=done
atl cancel <task> [--force]
atl hook <event>           # internal
atl doctor                 # check git, tmux, claude, gh, dirs, DB

## TUI (`atl` with no args)
- Left: tasks grouped by status. Right: selected task's repos (rw/ro, dirty?) and sessions with state + age.
- Highlight sessions in `waiting` state.
- Keys: n new, enter open, s new session, r resume, d done, c cancel, p pr, D diff, 1-5 set status, / filter, q quit.
- Refresh from DB every 1s (hooks write there).

## Milestones
M1 models + SQLite store + migrations
M2 git worktree management
M3 tmux + session launch/resume/attach
M4 hook command, state machine, ro guard, CLAUDE.md + settings.json generation
M5 CLI
M6 TUI
M7 projects, scripts, port offsets, diff, pr
M8 crash recovery + an end-to-end test script that builds fake repos in a temp dir and exercises new → session → done (claude/gh mocked with stub scripts on PATH)

## Non-goals
Web UI, custom chat rendering, other agents than Claude Code, sway integration (later, via CLI).
