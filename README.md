# atelier (`atl`)

A terminal task studio for parallel Claude Code work. Each task gets its own git
worktrees (branch `atl/<slug>` in every read-write repo), runs the real Claude Code TUI in
tmux (one window per session), and runs those sessions in the task's own Docker container
behind a gate that limits what the task can do. Session state is tracked in SQLite through
Claude Code hooks.

## Install on the host

atl runs on a Linux host. Requirements, on `PATH`:

- tmux ≥ 3.2, git
- `gh` (logged in), for `atl pr`, which runs on the host
- Docker CLI with access to the system daemon (your user in the `docker` group). Rootless
  Docker and Podman are not supported: a rootless daemon maps your UID to another one, so
  tasks could not write their worktrees.

Not needed: Go (atl builds in Docker), and Claude Code: task sessions, the assistant and
`atl token` run `claude` from the task image. Only `ATL_SANDBOX=off` needs `claude` on the
host, logged in.

Pushing and fetching use your own SSH agent, `~/.ssh` and git credentials.

Build from a clone of your own outside your repos dir, which no task ever writes: a task with
atelier rw can write its canonical clone's `.git` (hooks, `core.fsmonitor`), which your git
there would run.

```bash
git clone https://github.com/andreujuanc/atelier ~/src/atelier-build && cd ~/src/atelier-build
```

Run the steps below there (later: `git pull` there, then again).

**1. Build `atl`** (a static Linux binary; it also runs inside containers).

Without Go (the Dockerfile's `build` stage writes `/out/atl`):

```bash
docker build --target build -t atl-build . \
  && id=$(docker create atl-build) \
  && docker cp "$id:/out/atl" ~/.local/bin/atl \
  && docker rm "$id"
```

With Go:

```bash
CGO_ENABLED=0 go build -buildmode=exe -o ~/.local/bin/atl ./cmd/atl
```

`CGO_ENABLED=0` and `-buildmode=exe` are required: a dynamically linked or PIE binary
fails in containers; `atl doctor` and session starts reject it.

**2. Build the task image** `atelier-task` (what task and assistant containers run):

```bash
ATL_BUILD_BOX=0 ./build-images
```

**3. Create a workspace and check it.**

```bash
atl workspace new work --repos ~/src/work   # ~/atelier/workspaces/work/atelier.toml
cd ~/atelier/workspaces/work                # or: atl -w work …, or ATL_WORKSPACE=work
atl doctor                                  # tools, settings, atl binary, Docker, image, token
atl token                                   # once per workspace: the token sessions and the assistant log in with
atl                                         # the TUI
```

`--repos` is the directory that holds your canonical clones (`~/src/work/api`,
`~/src/work/web`, …). atl never edits them; tasks get worktrees.

Legacy: running atl itself in a container (the "box", `./build-images` without
`ATL_BUILD_BOX=0`) still works; see [docs/workspaces.md](docs/workspaces.md#legacy-the-box).
Moving from a devcontainer box to the host:
[docs/workspaces.md](docs/workspaces.md#moving-from-a-devcontainer-box).

## Concepts

| term | meaning |
|---|---|
| workspace | one directory (`~/atelier/workspaces/<name>`) with its tasks, database, projects, settings and its own tmux server |
| repo | a canonical clone in the workspace's `repos` dir |
| project | a named set of repos plus setup/teardown scripts, env and image (`config/projects/<name>.toml`) |
| task | a unit of work: title, slug, status (`backlog`, `active`, `review`, `done`, `cancelled`), repos, port offset |
| rw repo | a git worktree at `<task dir>/<repo>` on branch `atl/<slug>` |
| ro repo | the canonical clone, given to Claude read-only (`--add-dir`; edits blocked) |
| session | one Claude Code process in a tmux window of the task's tmux session `atl-<slug>`; state `running`, `waiting`, `idle`, `ended` or `stopped` |
| sandbox | the Docker container a task's sessions run in (the assistant has one per workspace) |
| gate | `atl gate`, one per container: its only way to Docker, atl's database, git credentials and your clipboard |

Details: [docs/tasks.md](docs/tasks.md), [docs/sandbox.md](docs/sandbox.md).

## TUI

Run `atl` in a plain terminal or inside your own tmux. Opening a task attaches to the
task's tmux session; **Ctrl-q** returns to the TUI (Claude keeps running).

The top bar shows the workspace and what the sessions are doing (waiting for you,
running, idle, stopped). Colours follow the terminal's light or dark background.

| key | action |
|---|---|
| `n` | new task (title, project, repos, base branch) |
| `enter` | open: new session if none, else a picker of new / live / resumable sessions |
| `s` | new session in the task |
| `r` | resume stopped sessions |
| `a` | add repos to the task (or upgrade ro → rw); live sessions are restarted to see them |
| `m` | edit the task's notes (`ctrl+s` save, `esc` leave) |
| `t` | rename (title only) |
| `d` / `c` | done / cancel (asks first) |
| `p` | open PRs (`atl pr`) |
| `D` | diff |
| `A` | the atelier assistant, about the selected task |
| `1`-`5` | set status: backlog, active, review, done, cancelled |
| `/` | filter by slug or title |
| `w` | switch workspace |
| `?` | every key, with what it does (any key closes it) |
| `q` | quit (waits for running operations; `ctrl+c` again forces) |

Repo picker: type to filter, `↑`/`↓` move, `space` cycles none → rw → ro, `enter`
confirms, `esc` cancels.

## CLI

`<task>` is a slug, numeric id or unique slug prefix. `-w <workspace>` works on every command.

```sh
atl                                  # TUI (workspace selector if no workspace is selected)
atl new "Fix login" --project shop   # create a task; prints "<slug>\t<dir>"
atl new "Spike" --repo api:rw --repo docs:ro -m "why" [--backlog] [--no-fetch]
atl new "Hotfix" --project shop --base default   # base: auto (default) | default | production | <branch>
atl add <task> web[:rw|ro]... [--no-fetch] [--force-restart]   # add repos (restarts live sessions); docs:rw upgrades ro → rw
atl ls [--status active,review|all] [--json]
atl show <task> [--json | --notes]   # repos (dirty, ahead, pushed), sessions, notes
atl open <task> [--session ID | --new]           # attach (resume or start Claude)
atl session <task>                   # start another session
atl resume <task>                    # relaunch stopped sessions (after a crash/reboot)
atl status <task> <backlog|active|review|done|cancelled>   # reopening a closed task restores it
atl rename <task> <new title...>
atl diff <task>                      # each rw repo vs the commit it branched from
atl push [task] [--repo R] [--force] # push atl/<slug> to origin (--force: with lease)
atl fetch [task] [--repo R]          # git fetch origin in each repo of the task
atl pr <task>                        # gh pr create per rw repo with pushed commits; status → review
atl done <task> [--force]            # remove worktrees, Docker objects, fully pushed branches
atl cancel <task> [--force]
atl note <task> <text...> [--author user|assistant]   # append a dated line to the notes
atl notes <task> [--set < file | --clear]
atl transcript <task> [--session ID] [--tail N]       # condensed Claude transcript
atl assistant [task]                 # the atelier assistant (own container per workspace)
atl repos                            # git repos in the repos dir
atl repos pin <repo>                 # accept a changed origin URL
atl token [--paste]                  # claude setup-token in a throwaway container; store the token
atl doctor                           # check everything; says what to fix
atl workspace list [--json]
atl workspace new <name> [--repos DIR] [--mount PATH]... [--image IMAGE]
atl workspace open [<name>]          # TUI in that workspace, or the selector
```

`push`/`fetch` default to `$ATL_TASK`, so a session can run them with no argument. Inside a
task container only `note`, `notes`, `push`, `fetch` (and the hooks and status line) work; the
assistant's container forwards an allowlist ([docs/assistant.md](docs/assistant.md)).

## Configuration

### `<workspace>/atelier.toml`

Strict TOML: every key optional, unknown keys are an error. `~/` expands in `repos`,
`git_config` and `mounts`; relative paths are relative to the workspace. No secrets here.

| key | default | meaning |
|---|---|---|
| `repos` | `"repos"` | directory of canonical clones |
| `git_config` | `"gitconfig"` if that file exists, else git's own | global git config for atl and everything it runs (`GIT_CONFIG_GLOBAL`) |
| `context` | none | text added to the start context of the assistant and of unsandboxed sessions (max 1000 bytes) |
| `tmux_socket` | `"atl-<name>"` | `tmux -L` name of the workspace's tmux server |
| `[sandbox] image` | `"atelier-task"` | task container image (a project's `image` wins) |
| `[sandbox] mounts` | none | host paths every task gets read-only ([docs/sandbox.md](docs/sandbox.md#host-files-for-every-task)) |
| `[sandbox] context` | none | text added to sandboxed sessions' start context (max 1000 bytes) |

```toml
repos = "~/src/work"

[sandbox]
mounts = ["~/.work.npmrc"]
```

More: [docs/workspaces.md](docs/workspaces.md).

### Projects: `<workspace>/config/projects/<name>.toml`

```toml
setup = "npm ci"                      # sh -c in each rw worktree after creation (on the host)
teardown = "docker compose down -v"   # sh -c in each rw worktree before removal (on the host)
image = "shop-dev"                    # task container image, e.g. built FROM atelier-task

[env]                                 # added to sessions and scripts
COMPOSE_PROJECT_NAME = "shop"

[[repos]]
name = "api"
mode = "rw"

[[repos]]
name = "docs"
mode = "ro"
```

### Environment variables

A variable set in the environment wins over `atelier.toml`.

| variable | default | meaning |
|---|---|---|
| `ATL_WORKSPACES` | `~/atelier/workspaces` | the workspaces root |
| `ATL_WORKSPACE` | from the current directory | the workspace: name or absolute path |
| `ATL_REPOS` | `repos` | canonical clones |
| `ATL_TASKS` | `<workspace>/tasks` | task dirs |
| `ATL_TMUX_SOCKET` | `tmux_socket` | tmux server name |
| `ATL_CONTEXT` | `context` | see `context` |
| `ATL_SANDBOX` | on | `off` runs sessions next to atl, unsandboxed (temporary escape hatch; going away). Anything but unset, empty, `docker` or `off` is an error |
| `ATL_SANDBOX_IMAGE` | `[sandbox] image` | task image (also read by `build-images`) |
| `ATL_SANDBOX_MOUNTS` | `[sandbox] mounts` | `:`-separated absolute paths |
| `ATL_SANDBOX_CONTEXT` | `[sandbox] context` | see `[sandbox] context` |
| `CLAUDE_CODE_OAUTH_TOKEN` | the token from `atl token` | token for sandboxed sessions and the assistant; wins over the stored one |
| `ANTHROPIC_API_KEY` | none | API key instead of a token (also wins) |

Sessions and project scripts get `ATL_TASK`, `ATL_TASK_DIR` and `ATL_PORT_OFFSET` (0, 100,
200…, chosen to avoid other open tasks in every workspace: add it to every port you bind) plus the
project's `env`. Scripts also get `ATL_REPO`, `ATL_WORKTREE` and `ATL_BRANCH`.

## Docs

| doc | covers |
|---|---|
| [docs/tasks.md](docs/tasks.md) | slugs, opening sessions, notes, rename, done/cancel, reopening, show/transcript |
| [docs/sandbox.md](docs/sandbox.md) | the task container, the gate's Docker rules, mounts, the token, copying, limits |
| [docs/git.md](docs/git.md) | base branches, push/fetch through the gate, `atl pr`, how atl runs git safely |
| [docs/workspaces.md](docs/workspaces.md) | workspace selection, settings, tmux, port offsets, moving from a devcontainer box |
| [docs/hooks.md](docs/hooks.md) | hooks, session state, start context, status line |
| [docs/assistant.md](docs/assistant.md) | the atelier assistant |

Design notes: [docs/WORKSPACES.md](docs/WORKSPACES.md), [docs/PLAN.md](docs/PLAN.md),
[docs/PROGRESS.md](docs/PROGRESS.md).

## Tests

```bash
go test ./...          # unit tests + test/e2e.sh (skip e2e with -short)
```

## License

MIT, see [LICENSE](LICENSE).
