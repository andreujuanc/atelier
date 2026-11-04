# atelier (`atl`)

A terminal task studio for parallel Claude Code work. Each task gets its own git
worktrees (one per read-write repo, on branch `atl/<slug>`); the real Claude Code TUI
runs inside tmux (session `atl-<slug>`, one window per Claude session); atelier
tracks session state in SQLite via Claude Code hooks.

Requires git, tmux ≥ 3.2, `claude`, and `gh` (for `atl pr`). Build with
`go build -o ~/bin/atl ./cmd/atl`, then run `atl doctor`.

## Usage

```sh
atl new "Fix login" --project shop        # task + worktrees, prints "<slug>\t<dir>"
atl new "Spike" --repo api:rw --repo docs:ro -m "why" [--backlog] [--no-fetch]
atl ls [--status active,review|all] [--json]
atl open fix-login        # ensure the tmux session (resume or start Claude), attach
atl session fix-login     # start another Claude session (window) in the task
atl resume fix-login      # after a crash/reboot: relaunch stopped sessions with claude --resume
atl status fix-login review
atl diff fix-login        # git diff of each rw repo vs its base commit (paged on a TTY)
atl pr fix-login          # gh pr create per rw repo with pushed commits; status → review
atl done fix-login [--force]    # teardown, remove worktrees (branches kept), kill tmux
atl cancel fix-login [--force]
atl doctor
atl                       # TUI (on a terminal)
```

`<task>` is a slug, numeric id, or unique slug prefix. atelier never pushes:
push `atl/<slug>` yourself before `atl pr`.

Every `atl` invocation (except `hook` and `doctor`) first marks sessions whose tmux
window is gone as `stopped`; the TUI also does so every few seconds.

## Environment

| variable | default |
|---|---|
| `ATL_REPOS` | `~/repos` — canonical clones, `$ATL_REPOS/<name>` (never edited) |
| `ATL_TASKS` | `~/tasks` — task dirs, `$ATL_TASKS/<slug>/` |
| `XDG_DATA_HOME` | `~/.local/share` — DB at `atelier/atelier.db` |
| `XDG_STATE_HOME` | `~/.local/state` — hook errors in `atelier/hook.log` |
| `XDG_CONFIG_HOME` | `~/.config` — projects in `atelier/projects/<name>.toml` |
| `ATL_TMUX_SOCKET` | unset — tmux socket name (`tmux -L`) if you don't use the default server |
| `ATL_BIN` | the running binary — `atl` path written into hook commands |

Claude windows and scripts get `ATL_TASK`, `ATL_TASK_DIR`, `ATL_PORT_OFFSET`
(0, 100, 200… unique per open task) plus the project's `env`; scripts also get
`ATL_REPO`, `ATL_WORKTREE`, `ATL_BRANCH`.

## Projects

`~/.config/atelier/projects/shop.toml`:

```toml
setup = "npm ci"                          # sh -c in each rw worktree after creation
teardown = "docker compose down -v"       # sh -c in each rw worktree before removal

[env]
COMPOSE_PROJECT_NAME = "shop"

[[repos]]
name = "api"
mode = "rw"     # git worktree on atl/<slug>

[[repos]]
name = "docs"
mode = "ro"     # not a worktree: passed to claude with --add-dir, edits blocked
```

## Hooks

atelier writes `<taskdir>/.claude/settings.json` (merged with any existing keys) with
hooks that run `atl hook <event>` for SessionStart, UserPromptSubmit, Notification,
PostToolUse, Stop, SessionEnd and PreToolUse. They keep session state current
(`running`, `waiting` on permission prompts, `idle`, `ended`; `stopped` comes from
crash recovery), and the PreToolUse hook blocks Edit/Write/MultiEdit/NotebookEdit
on paths inside read-only repos. Hook failures never block Claude (except the
read-only guard) and are logged to `hook.log`.

## Tests

`go test ./...` runs unit tests and `test/e2e.sh` (skipped with `-short`), which
builds `atl`, creates fake repos and a project in a temp dir and drives
new → session → hooks → crash recovery → resume → diff → pr → done against an
isolated tmux server, with `claude` and `gh` replaced by `test/stubs`.
