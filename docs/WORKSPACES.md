# Workspaces and atl on the host — design

Status: in progress (task `atelier-in-host`). Milestones and their state are at the end.

## Why

Today a "workspace" is informal: a directory (`~/atelier/workspaces/<name>`) with a
`.devcontainer/devcontainer.json` that runs the box image with `ATL_WORKSPACE`, `ATL_REPOS`,
`ATL_SANDBOX_MOUNTS`, a per-workspace `gitconfig` mounted as `~/.gitconfig`, `~/.claude`
and the Docker socket. Since every task's sessions run in the task's own container (the
sandbox), the box adds no security boundary (it holds the Docker socket, which is
root-equivalent on the host); it only packages tools and carries settings. This work
makes the workspace a first-class, self-describing directory, and lets `atl` run directly
on the host.

## Concepts

- **Workspaces root**: `$ATL_WORKSPACES`, default `~/atelier/workspaces` (`~` expanded,
  made absolute).
- **Workspace**: a directory `$ATL_WORKSPACES/<name>/` that contains `atelier.toml`. The
  file is the marker: a directory without it is not a workspace. The layout under it is
  the one `ATL_WORKSPACE` already defines (`tasks/ data/ config/ state/ run/`).
- **Name**: `^[a-z0-9][a-z0-9_-]{0,31}$`.

## `atelier.toml`

Strict TOML (unknown keys are an error, as in project files). Every key is optional.

```toml
repos = "~/src/work"     # canonical clones (was ATL_REPOS); relative = to the workspace dir; default "repos"
git_config = "gitconfig"          # global git config for atl and all it runs (GIT_CONFIG_GLOBAL); relative = to the workspace dir;
                                  # default: "gitconfig" if that file exists in the workspace, else unset (git's own default)
context = "..."                   # ATL_CONTEXT (what runs next to atl: the assistant)
tmux_socket = "atl-work"            # tmux -L name of atl's tmux server (was ATL_TMUX_SOCKET); default "atl-<name>"

[sandbox]
image = "atelier-task"            # ATL_SANDBOX_IMAGE
mounts = ["~/.work.npmrc"]    # ATL_SANDBOX_MOUNTS (list instead of ':'-separated)
context = "..."                   # ATL_SANDBOX_CONTEXT
```

No secrets go in it (the Claude token stays in `<ws>/run/claude-token.json`).

## How a workspace is applied: the environment

The rest of atl (config.Resolve, sandbox.FromEnv, ROMounts, tmux.New, the gate, the
hooks, scripts) already reads environment variables, and every child process atl starts
(the gate, the tmux server and its windows, scripts, `atl diff` from the TUI) inherits
atl's environment. So applying a workspace means: **set environment variables in atl's own
process before anything else reads them.**

| setting | variable set |
|---|---|
| the workspace dir | `ATL_WORKSPACE` (absolute path) |
| repos | `ATL_REPOS` |
| git_config | `GIT_CONFIG_GLOBAL` |
| context | `ATL_CONTEXT` |
| tmux_socket | `ATL_TMUX_SOCKET` |
| sandbox.image | `ATL_SANDBOX_IMAGE` |
| sandbox.mounts | `ATL_SANDBOX_MOUNTS` (joined with `:`) |
| sandbox.context | `ATL_SANDBOX_CONTEXT` |

Precedence: a variable already set in the environment wins over the file (so the
devcontainer boxes keep working during the move, and for tests); doctor shows where each
value came from (env, atelier.toml, default). `ATL_SANDBOX` (`off`) is not a file setting:
it is going away.

atl keeps a snapshot of its original environment (before applying) for re-executing
itself in another workspace (the selector's switch).

## Selecting the workspace

In order, the first that applies:

1. `--workspace <name|path>` / `-w` (a global flag on the root command).
2. `$ATL_WORKSPACE` (a name, or an absolute path; set by atl for everything it starts, and
   by the old devcontainers).
3. The current directory: if it is `$ATL_WORKSPACES/<name>` or inside it (a path-prefix
   check against the root, after resolving symlinks; atl does not hunt for marker files
   up the tree).
4. None: `atl` with no subcommand on a terminal shows the **workspace selector**; any other
   command that needs a workspace fails with one line naming the ways to pick one and the
   existing workspaces.

A selected directory without `atelier.toml` is an error that says how to adopt it
(`atl workspace new <name>`).

Commands that do not need a workspace: `atl workspace …`, help/completion, and everything
that runs inside a task container (`sandbox.Inside()`: those talk to the task's gate and
never resolve a workspace). `atl doctor` without a workspace reports that and still checks
the tools.

There is no XDG mode from the CLI any more: the CLI always applies a workspace before
opening the App. (`config.Resolve` keeps its XDG branch for now; removing it is a later
cleanup.)

## `atl workspace`

- `atl workspace list [--json]` — every workspace under the root: name, path, open tasks
  (backlog/active/review) and, when any, sessions waiting. A directory under the root
  without `atelier.toml` is listed as "not a workspace (no atelier.toml)" so it can be
  adopted. Marks the current one (if any) with `*`.
- `atl workspace new <name> [--repos DIR] [--mount PATH]... [--image IMAGE]` — creates
  `$ATL_WORKSPACES/<name>/` with `atelier.toml`, `tasks/ data/ config/ state/`, and the
  repos dir if it is inside the workspace. If the directory exists without `atelier.toml`
  it is adopted (existing contents are kept). Refuses when `atelier.toml` exists. Values
  not given as flags are taken from the current environment when set (`ATL_REPOS`,
  `ATL_SANDBOX_MOUNTS`, `ATL_SANDBOX_IMAGE`, `ATL_SANDBOX_CONTEXT`, `ATL_CONTEXT`), so
  running it inside an old box writes that box's settings. Refuses a name whose gate
  socket path (`<ws>/run/<12 chars>/docker.sock`) would exceed 107 bytes.
- `atl workspace open <name>` — the TUI in that workspace (same as `atl -w <name>`).

## Collisions between workspaces

All workspaces share one Docker daemon, the host's ports and possibly canonical clones.

- **Port offsets**: a new task's offset avoids the offsets of open tasks in *every*
  workspace under the root (their databases are read read-only; an unreadable one is
  skipped with a warning). Doctor warns about open tasks in two workspaces with the same
  offset.
- **Slugs**: a new task's slug gets `-2`, `-3`, … (as for slugs taken in the database)
  also when its task dir exists on disk, or when branch `atl/<slug>` already exists in
  the canonical clone of one of its rw repos.
- **Containers, gate sockets, Claude dirs**: already distinct (named by a hash of the task
  dir).
- **tmux**: each workspace has its own tmux server (`tmux_socket`, default `atl-<name>`),
  so `atl-<slug>` sessions of two workspaces never meet, and atl never touches the user's
  own default tmux server.

## atl on the host

- The `atl` binary is copied into each task's run dir and runs in task containers, so it
  must be a static Linux binary: build with `CGO_ENABLED=0`. Doctor checks the running
  binary (ELF, no interpreter) and starting a sandboxed session refuses a dynamic one with
  a line that says how to build it.
- atl starts its tmux server with a config it generates (`<ws>/state/tmux.conf`, passed
  with `-f`), carrying what the box's `/etc/tmux.conf` had: status off, mouse on,
  escape-time 10, `C-q` detach-client, `default-terminal tmux-256color`, RGB.
- Host requirements: tmux ≥ 3.2, git, gh, claude, the Docker CLI with access to the
  daemon (the `docker` group, as the box had via `--group-add`).
- The box image stays buildable and works (its env wins), but is no longer the way in.

## Milestones

| | | state |
|---|---|---|
| W1 | workspace package, atelier.toml, selection, `atl workspace list/new/open`, env apply, per-workspace tmux socket, git_config, doctor + README | done |
| W2 | collisions: cross-workspace port offsets, slug suffix for dir/branch, doctor warning | |
| W3 | workspace selector TUI (`atl` with none selected), switch workspace from the TUI | |
| W4 | host mode: generated tmux.conf, static-binary check, README install-on-host + migration from devcontainers | |

## Decisions

- "Already set" means set and non-empty, as everywhere else in atl (`config.Resolve`).
- `ATL_WORKSPACE` itself is always set to the selected dir (the flag wins over the env).
- What atl sets itself it records in `ATL_WORKSPACE_VARS` as `NAME=<12 hex of sha256 of the
  value>`, comma-separated. A variable is atl's only while its value still matches; atl's
  variables are always recomputed from the current `atelier.toml` (same workspace or
  another, and with no `ATL_WORKSPACE` at all), so `atl -w work` from `personal`'s assistant gets
  `work`'s values and an edited file reaches children; a value that does not match is the
  user's and always wins. `atl workspace new` takes only the user's values.
- Every tmux window atl creates (task windows, the assistant, the token window) gets
  `ATL_WORKSPACE`, `ATL_WORKSPACE_VARS` and the setting variables with `-e`, empty when
  unset, except `GIT_CONFIG_GLOBAL`, which is passed only when set: set but empty, git
  reads no global config at all (so a stale one in an old server env stays possible).
  tmux commands run without `CLAUDECODE`, `ATL_SESSION`, `ATL_CONTROL_SOCK`, `ATL_TASK`,
  `ATL_TASK_DIR`, `ATL_PORT_OFFSET`, and with a socket name without `TMUX`/`TMUX_PANE`, so
  a server atl starts does not take them.
- Adoption (W1 review): `atl workspace new` on an existing dir with `data/atelier.db`,
  while `ATL_TMUX_SOCKET` is not the user's, writes `tmux_socket = "default"` with a
  comment and prints a note: the box ran its sessions on the default server, and on
  `atl-<name>` Recover would stop them and atl would end Claude in their containers.
- Attaching: `switch-client` only when ``'s socket (first comma field) is the same
  file as this server's socket (`` or `/tmp`, `tmux-<uid>/<name>`), or when no
  socket name is set; otherwise `attach-session` with `TMUX`/`TMUX_PANE` removed (a nested
  client).
- The default tmux socket of a workspace selected by a path not directly under the root is
  `atl-<name>-<6 hex of the dir's hash>`. The cwd rule also matches workspaces that are
  symlinks under the root.
- `atl token` needs a workspace like every other command; only doctor uses an App
  without paths. Doctor warns when `git_config` names a missing file. Sandbox mounts may
  not be, contain or lie in ``, `~/.gitconfig`, `~/.config/git` (or
  `/git`).
- A `--workspace`/`$ATL_WORKSPACE` value with a `/` (or a leading `~`, or `.`/`..`) is a
  path, anything else a name; a path's workspace name is its base name. A relative path
  in the flag is relative to the current directory.
- The cwd rule ignores directories under the root whose name is not a valid workspace
  name; `atl workspace list` skips them too.
- `atl workspace new`: flag paths (`--repos`, `--mount`) are made absolute against the
  current directory (`~/` mounts are kept as written); a repos dir inside the workspace is
  written relative to it, and omitted when it is the default `repos`. The "how to adopt"
  hint names `ATL_WORKSPACES=<parent>` when the selected directory is not under the root.
- `atl gate` also applies the workspace (so `ATL_WORKSPACE=<name>` works for it); hooks and
  the status line keep their fast path and read the environment atl gave them.
- Doctor without a workspace: the four tool checks and a failing `workspace` check, nothing
  that needs paths. With one: a `workspace` line (name, dir, how selected) and one line
  per setting (`$VAR = value (source)`, contexts by size, unset ones as unset).
- Applying is undone at the end of each CLI run (only matters to tests, which run atl many
  times in one process). The original-environment snapshot is taken at the first Apply.
- `sandbox.maxSocketPath` became the exported `sandbox.MaxSocketPath` for the budget check.
- The README drops the XDG rows and "without a workspace" paths (the CLI always runs in a
  workspace); `config.Resolve` keeps its XDG branch. CLI tests and `test/e2e.sh` now run in
  a workspace (e2e creates it with `atl workspace new`).
