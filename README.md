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
atl new "Hotfix" --project shop --base default   # branch from origin's default branch (production)
atl add fix-login web [--no-fetch]  # add a repo to an open task (rw: worktree on atl/<slug>)
atl add fix-login docs:ro           # ro: listed in CLAUDE.md; running sessions need a new one (s)
atl repos                 # git repos in $ATL_REPOS, one per line
atl ls [--status active,review|all] [--json]
atl open fix-login        # ensure the tmux session (resume or start Claude), attach
atl session fix-login     # start another Claude session (window) in the task
atl resume fix-login      # after a crash/reboot: relaunch stopped sessions with claude --resume
atl status fix-login review
atl diff fix-login        # git diff of each rw repo vs its base commit (paged on a TTY)
atl pr fix-login          # gh pr create (--base = the repo's base branch) per rw repo with pushed commits; status → review
atl show fix-login [--json]     # task, repos (dirty, commits ahead, last commit, pushed), sessions, notes
atl transcript fix-login [--session ID] [--tail N]  # last N (20; 0 = all) turns of a Claude session
atl note fix-login waiting on design review [--author user|assistant]
atl notes fix-login       # notes, newest first
atl assistant [fix-login] # the atelier assistant (one Claude session for all tasks); attach
atl done fix-login [--force]    # teardown, remove worktrees and fully pushed branches, kill tmux
atl cancel fix-login [--force]
atl doctor
atl                       # TUI (on a terminal)
```

TUI keys: `n` new task, `enter` open, `s` new session, `r` resume, `a` add repos to the
selected task, `m` add a note to the selected task (`enter` saves, `esc` cancels), `d` done,
`c` cancel, `p` pr, `D` diff, `A` assistant (about the selected task), `1`-`5` status,
`/` filter, `q` quit. The details pane shows the task's latest note.
Repos (in the new-task form and for `a`) are picked from a list of `$ATL_REPOS`: type to
filter, `↑`/`↓` move, `space` cycles none → rw → ro, `enter` confirms, `esc` cancels.
Choosing a project in the form preselects its repos. The form's Base field toggles
(`space`, `←`/`→`; `enter` moves on) between "auto (dev → develop → default)" and
"production patch (default branch)".

`<task>` is a slug, numeric id, or unique slug prefix. atelier never pushes:
push `atl/<slug>` yourself before `atl pr`.

`atl done`/`atl cancel` remove each worktree and then delete its `atl/<slug>` branch
only if no commits on the branch would be lost (the branch's reflog is not kept): every
commit on it must be reachable from another local branch or a remote-tracking ref (local
refs only, no fetch), e.g. after you pushed it or when it has no commits of its own. Otherwise the branch is kept and reported
(`kept branch atl/<slug> in <repo> (N unpushed commit(s))`); `--force` only discards
uncommitted changes, never unpushed commits.

`atl show` never touches the network: "pushed" means the local remote-tracking ref
`origin/atl/<slug>` exists (as of your last push or fetch). `--json` prints a stable
snake_case object (see `internal/cli/info.go`). `atl transcript` reads the Claude Code
transcript JSONL recorded by the hooks and condenses it to user/assistant text; tool
calls become one-line `[tool: Name] …` summaries and tool results are skipped. Notes
work on closed tasks too.

`atl assistant [task]` (TUI `A`) opens the atelier assistant: a single Claude Code session,
in tmux session `atl_assistant`, for managing tasks rather than writing code ("what's the
status of X?", "where did I leave off?", "add repo Y to X"). It runs in
`$XDG_DATA_HOME/atelier/assistant/` with a generated `CLAUDE.md` (role and `atl` command
reference, always rewritten) and `.claude/settings.json` (merged): read-only `atl`
commands (`ls`, `show`, `transcript`, `notes`, `repos`) and Read/Grep/Glob are allowed,
every state-changing `atl` command asks first, common repo mutators (`git commit`, `push`,
`checkout`, `reset`, `rebase`, `merge`, `stash`, `rm`, `mv`, `cp`, `sed -i`, `tee`, ...) are
denied, and the `atl hook AssistantGuard` PreToolUse hook blocks Edit/Write/MultiEdit/NotebookEdit
outside that dir (and on its own `CLAUDE.md` and `.claude/`, in any letter case). Bash and MCP
tools cannot be fully sandboxed; the deny rules cover the common cases so that your global
allow rules can't let the assistant change repos. It runs `claude --continue` when Claude Code
has a previous conversation for that dir (a `*.jsonl` under
`${CLAUDE_CONFIG_DIR:-~/.claude}/projects/<dir with non-alphanumerics as ->`), else `claude`,
so the conversation survives restarts. With a task, "Let's talk about task <slug>." is passed
as the initial prompt. Keys are never typed into a running assistant (they could answer a
pending permission prompt): the sentence is put in the tmux paste buffer instead and a hint
(stderr, or the TUI status line) says to press `<prefix> ]` to paste it. Concurrent launches
are serialised by `.launch.lock` in that dir; a session whose `assistant` window is missing or
dead is replaced. Its tmux window is not tracked
by atelier: crash recovery, `done` and `cancel` never touch it.

Every `atl` invocation (except `hook` and `doctor`) first marks sessions whose tmux
window is gone as `stopped`; the TUI also does so every few seconds.

## Base branch

Each rw repo's `atl/<slug>` branch starts from a base branch on origin (after `git fetch
origin`, unless `--no-fetch`), chosen per task with `atl new --base`:

- `auto` (default): `origin/dev` if it exists, else `origin/develop`, else origin's default
  branch (`origin/HEAD`, `main` or `master`). Feature work starts from the integration branch.
- `default` (or `production`): origin's default branch, for production patches.
- any other value: that exact branch (e.g. `main`, `release/1.2`), which must exist as
  `origin/<name>` in every rw repo, else `atl new` fails naming the repo and nothing is created.
  `auto`, `default` and `production` are reserved words: a branch with one of those names
  cannot be chosen.

The choice is stored on the task, so `atl add` (and TUI `a`) resolves the same way for repos
added later (tasks created before this choice existed keep origin's default branch). Each repo records the branch it started from: `atl pr` targets it (`gh pr create
--base <branch>`), `atl diff` compares against the commit it started from, the task's
`CLAUDE.md` says "branched from `origin/<branch>`", `atl show` prints `base origin/<branch>`
per repo (`--json`: task `base` = `""` auto, `":default"` or the branch name; repo
`base_branch`), and the TUI details pane shows `← <branch>` per rw repo.

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
read-only guard) and are logged to `hook.log`. The assistant's `AssistantGuard` hook
(see above) fails closed: input it cannot inspect is refused.

On SessionStart (startup, resume, `/clear`, compaction) inside a task, `atl hook
SessionStart` also prints Claude Code's `hookSpecificOutput.additionalContext` JSON with
fresh task context (title, status, branch, repos with their base branch or read-only
warning, `ATL_PORT_OFFSET`, the 5 newest notes; at most 2500 characters), so a session
sees current notes without any file being rewritten. This is the only hook output on
stdout; if the context cannot be built the error goes to `hook.log` and nothing is printed.

## Tests

`go test ./...` runs unit tests and `test/e2e.sh` (skipped with `-short`), which
builds `atl`, creates fake repos and a project in a temp dir and drives
new → session → hooks → crash recovery → resume → diff → pr → done against an
isolated tmux server, with `claude` and `gh` replaced by `test/stubs`.
