# atelier (`atl`)

A terminal task studio for parallel Claude Code work. Each task gets its own git
worktrees (one per read-write repo, on branch `atl/<slug>`); the real Claude Code TUI
runs inside tmux (session `atl-<slug>`, one window per Claude session); atelier
tracks session state in SQLite via Claude Code hooks.

Requires git, tmux ≥ 3.2, `claude`, and `gh` (for `atl pr`). Build with
`go build -o ~/bin/atl ./cmd/atl`, then run `atl doctor`.

## Docker image

The `Dockerfile` builds an image with `atl`, `claude`, `tmux`, `git` and `gh`, so nothing
needs to be installed on the host. Build it with your host user, so paths inside the
container match the host (git worktrees store absolute paths):

```bash
docker build -t atelier \
  --build-arg USERNAME=$(whoami) --build-arg UID=$(id -u) --build-arg GID=$(id -g) .
```

Inside the image tmux stays out of the way: run `atl`, press Enter on a task to open
Claude full screen, and press **Ctrl-q** to go back to the TUI (Claude keeps running).

## Usage

```sh
atl new "Fix login" --project shop        # task + worktrees, prints "<slug>\t<dir>"
atl new "Spike" --repo api:rw --repo docs:ro -m "why" [--backlog] [--no-fetch]
atl add fix-login web [--no-fetch]  # add a repo to an open task (rw: worktree on atl/<slug>)
atl add fix-login docs:ro           # ro: listed in CLAUDE.md; running sessions need a new one (s)
atl repos                 # git repos in $ATL_REPOS, one per line
atl ls [--status active,review|all] [--json]
atl open fix-login        # ensure the tmux session (resume or start Claude), attach
atl session fix-login     # start another Claude session (window) in the task
atl resume fix-login      # after a crash/reboot: relaunch stopped sessions with claude --resume
atl status fix-login review
atl diff fix-login        # git diff of each rw repo vs its base commit (paged on a TTY)
atl pr fix-login          # gh pr create per rw repo with pushed commits; status → review
atl show fix-login [--json]     # task, repos (dirty, commits ahead, last commit, pushed), sessions, notes
atl transcript fix-login [--session ID] [--tail N]  # last N (20; 0 = all) turns of a Claude session
atl note fix-login waiting on design review [--author user|assistant]
atl notes fix-login       # notes, newest first
atl done fix-login [--force]    # teardown, remove worktrees and fully pushed branches, kill tmux
atl cancel fix-login [--force]
atl doctor
atl                       # TUI (on a terminal)
```

TUI keys: `n` new task, `enter` open, `s` new session, `r` resume, `a` add repos to the
selected task, `m` add a note to the selected task (`enter` saves, `esc` cancels), `d` done,
`c` cancel, `p` pr, `D` diff, `1`-`5` status, `/` filter, `q` quit. The details pane shows
the task's latest note.
Repos (in the new-task form and for `a`) are picked from a list of `$ATL_REPOS`: type to
filter, `↑`/`↓` move, `space` cycles none → rw → ro, `enter` confirms, `esc` cancels.
Choosing a project in the form preselects its repos.

`<task>` is a slug, numeric id, or unique slug prefix. atelier never pushes:
push `atl/<slug>` yourself before `atl pr`.

`atl done`/`atl cancel` remove each worktree and then delete its `atl/<slug>` branch
only if nothing would be lost: every commit on it must be reachable from another local
branch or a remote-tracking ref (local refs only, no fetch), e.g. after you pushed it or
when it has no commits of its own. Otherwise the branch is kept and reported
(`kept branch atl/<slug> in <repo> (N unpushed commit(s))`); `--force` only discards
uncommitted changes, never unpushed commits.

`atl show` never touches the network: "pushed" means the local remote-tracking ref
`origin/atl/<slug>` exists (as of your last push or fetch). `--json` prints a stable
snake_case object (see `internal/cli/info.go`). `atl transcript` reads the Claude Code
transcript JSONL recorded by the hooks and condenses it to user/assistant text; tool
calls become one-line `[tool: Name] …` summaries and tool results are skipped. Notes
work on closed tasks too.

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
crash recovery) and record each session's transcript path, and the PreToolUse hook blocks Edit/Write/MultiEdit/NotebookEdit
on paths inside read-only repos. Hook failures never block Claude (except the
read-only guard) and are logged to `hook.log`.

## Tests

`go test ./...` runs unit tests and `test/e2e.sh` (skipped with `-short`), which
builds `atl`, creates fake repos and a project in a temp dir and drives
new → session → hooks → crash recovery → resume → diff → pr → done against an
isolated tmux server, with `claude` and `gh` replaced by `test/stubs`.
