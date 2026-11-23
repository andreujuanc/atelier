# Workspaces and atl on the host — design

Status: done (task `atelier-in-host`). Milestones and their state are at the end.

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

- `atl workspace list [--json]` — every directory under the root: name and path; one
  without `atelier.toml` is marked "not adopted" so it can be adopted. Marks the current
  one (if any) with `*`. `--json`: `name`, `path`, `adopted`, `current`. No counts (see
  Decisions, the plain selector).
- `atl workspace new <name> [--repos DIR] [--mount PATH]... [--image IMAGE]` — creates
  `$ATL_WORKSPACES/<name>/` with `atelier.toml`, `tasks/ data/ config/ state/`, and the
  repos dir if it is inside the workspace. If the directory exists without `atelier.toml`
  it is adopted (existing contents are kept), and its box's
  `.devcontainer/devcontainer.json`, when there is one, is imported (see Decisions,
  devcontainer import). Refuses when `atelier.toml` exists. Precedence per setting: flags,
  then the devcontainer.json, then the user's environment (`ATL_REPOS`,
  `ATL_SANDBOX_MOUNTS`, `ATL_SANDBOX_IMAGE`, `ATL_SANDBOX_CONTEXT`, `ATL_CONTEXT`, so
  running it inside an old box writes that box's settings), then the defaults. Prints
  each setting written with its source and each skipped value with the reason. A
  directory with `data/atelier.db` (a box's workspace) is adopted outside its box only
  with the repos known (see Decisions, adoption). Refuses a name whose gate socket path
  (`<ws>/run/<12 chars>/docker.sock`) would exceed 107 bytes.
- `atl workspace open <name>` — the TUI in that workspace (same as `atl -w <name>`). Without
  a name: the workspace selector, ignoring `$ATL_WORKSPACE` and the current directory.

## Collisions between workspaces

All workspaces share one Docker daemon, the host's ports and possibly canonical clones.

- **Port offsets**: a new task's offset avoids the offsets of open tasks in *every*
  workspace under the root (their databases are read read-only; an unreadable one is
  skipped with a warning). Doctor warns about open tasks in two workspaces with the same
  offset, and about two open tasks of this workspace with the same offset (reopen).
- **Slugs**: a new task's slug gets `-2`, `-3`, … (as for slugs taken in the database)
  also when its task dir exists on disk, or when branch `atl/<slug>`, or one below it
  (`atl/<slug>/x`), already exists in the canonical clone of one of its rw repos (locally
  or under `origin/`).
- **Containers, gate sockets, Claude dirs**: already distinct (named by a hash of the task
  dir).
- **tmux**: each workspace has its own tmux server (`tmux_socket`, default `atl-<name>`),
  so `atl-<slug>` sessions of two workspaces never meet, and atl never touches the user's
  own default tmux server.

## atl on the host

- The `atl` binary is copied into each task's run dir and runs in task containers, so it
  must be a static Linux binary: build with `CGO_ENABLED=0 go build -buildmode=exe`. Doctor
  checks the running
  binary (ELF, no interpreter) and starting a sandboxed session refuses a dynamic one with
  a line that says how to build it.
- atl starts its tmux server with a config it generates (`<ws>/state/tmux.conf`, passed
  with `-f`), carrying what the box's `/etc/tmux.conf` had: status off, mouse on,
  escape-time 10, `C-q` detach-client, `default-terminal tmux-256color`, RGB; and last
  `source-file -q <ws>/tmux.conf`, the user's own settings.
- Host requirements: tmux ≥ 3.2, git, gh, the Docker CLI with access to the
  daemon (the `docker` group, as the box had via `--group-add`). Since W5 not claude: with
  the sandbox on, task sessions, the assistant and `atl token` run it in containers.
- The box image stays buildable and works (its env wins), but is no longer the way in.

## Milestones

| | | state |
|---|---|---|
| W1 | workspace package, atelier.toml, selection, `atl workspace list/new/open`, env apply, per-workspace tmux socket, git_config, doctor + README | done |
| W2 | collisions: cross-workspace port offsets, slug suffix for dir/branch, doctor warning | done |
| W3 | workspace selector TUI (`atl` with none selected), switch workspace from the TUI | done |
| W4 | host mode: generated tmux.conf, static-binary check, README install-on-host + migration from devcontainers | done |
| W5 | no Claude Code on the host: the assistant in its own container behind its own gate (allowlisted atl commands), `atl token` in a throwaway container, doctor without claude, build without Go | done |

## Decisions

- "Already set" means set and non-empty, as everywhere else in atl (`config.Resolve`).
- `ATL_WORKSPACE` itself is always set to the selected dir (the flag wins over the env).
- What atl sets itself it records in `ATL_WORKSPACE_VARS` as `NAME=<12 hex of sha256 of the
  value>`, comma-separated. `ATL_WORKSPACE` is recorded too (since the fix round), so an atl
  started by atl can tell it from one a box set (`Applied.UserVar`, `UserEnv`). A variable
  is atl's only while its value still matches; atl's variables are always recomputed from
  the current `atelier.toml` (same workspace or another, and with no `ATL_WORKSPACE` at
  all), so `atl -w work` from `personal`'s assistant gets `work`'s values and an edited file reaches
  children; a value that does not match is the user's and always wins. `atl workspace new`
  takes only the user's values.
- Every tmux window atl creates (task windows, the assistant, the token window) gets
  `ATL_WORKSPACE`, `ATL_WORKSPACE_VARS` and the setting variables with `-e`, empty when
  unset, except `GIT_CONFIG_GLOBAL`, which is passed only when set: set but empty, git
  reads no global config at all (so a stale one in an old server env stays possible).
  tmux commands run without `CLAUDECODE`, `ATL_SESSION`, `ATL_CONTROL_SOCK`, `ATL_TASK`,
  `ATL_TASK_DIR`, `ATL_PORT_OFFSET`, and with a socket name without `TMUX`/`TMUX_PANE`, so
  a server atl starts does not take them.
- Adoption (W1 review, changed in the fix round): `atl workspace new` on an existing dir
  with `data/atelier.db` (`workspace.HasDB`) is a box's workspace. Only when the user's
  `$ATL_WORKSPACE` (set, not atl's) resolves to that same dir (same path, or `os.SameFile`)
  are we inside its box: then, while `ATL_TMUX_SOCKET` is not the user's, it writes
  `tmux_socket = "default"` with a comment and prints a note (the box ran its sessions on
  the default server, and on `atl-<name>` Recover would stop them and atl would end Claude
  in their containers). Anywhere else (on the host, from an atl child, in another box) it
  needs the repos, `--repos`, `ATL_REPOS` of its devcontainer.json (since the change
  request after review) or a user-set `ATL_REPOS` (checked before a repos dir equal
  to the default is dropped from the file), and without them refuses with one line
  (`workspace.AdoptDBError`) naming `atl workspace new <name> --repos DIR [--mount PATH]...`
  and the README section; nothing is written. Writing `default` on the host put atl on
  the user's own tmux server (no `-f`, atl's variables in its global environment) and an
  empty `<ws>/repos` dropped the box's clones and mounts.
- Attaching: `switch-client` only when `$TMUX`'s socket (first comma field) is the same
  file as this server's socket (`$TMUX_TMPDIR` or `/tmp`, `tmux-<uid>/<name>`), or when no
  socket name is set; otherwise `attach-session` with `TMUX`/`TMUX_PANE` removed (a nested
  client).
- The default tmux socket of a workspace selected by a path not directly under the root is
  `atl-<name>-<6 hex of the dir's hash>`. The cwd rule also matches workspaces that are
  symlinks under the root.
- `atl token` needs a workspace like every other command; only doctor uses an App
  without paths. Doctor warns when `git_config` names a missing file. Sandbox mounts may
  not be, contain or lie in `$GIT_CONFIG_GLOBAL`, `~/.gitconfig`, `~/.config/git` (or
  `$XDG_CONFIG_HOME/git`).
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
- W2, port offsets: `store.CreateTaskAutoPort(ctx, t, store.Avoid{PortOffsets, SlugTaken})`
  (and `NextPortOffset(ctx, reserved...)`): the app passes in what lies outside the
  database; allocation and insert stay one `BEGIN IMMEDIATE` transaction. The other
  databases are read with `store.OpenReadOnly` (busy timeout 2 s) before that transaction.
  There is no lock across workspaces: two workspaces creating a task at the same instant
  can pick the same offset; doctor's warning covers it.
- The "other workspaces" are every directory under the root with a valid workspace name and
  a `data/atelier.db`, with or without `atelier.toml` (an old box not yet adopted still
  binds host ports). One without a database has no tasks and is skipped silently (a fresh
  workspace would otherwise warn on every `atl new`); one whose database cannot be opened
  or read (not SQLite, locked, `ErrSchemaVersion`) is skipped with a warning on `App.Out`,
  and in doctor's `port offsets` check. This workspace is left out by the resolved
  directory (`EvalSymlinks`) and, as well, by its database file (`os.SameFile`).
- Doctor's `port offsets` check compares across workspaces and, since the fix round, also
  within this one (two open tasks with the same offset, which reopen allows), and names
  the current workspace by its applied name, else the base name of `ATL_WORKSPACE`. With
  `Applied.UserVar` (a box) and no other workspace read, its pass says the other
  workspaces' offsets were not checked (a box mounts only its own workspace).
- Reopen keeps the task's offset (it never allocated one, not even against its own
  workspace), its slug and its branch: it reuses the kept `atl/<slug>` on purpose and its
  task dir may exist. Rename changes only the title. So neither applies the new checks.
- W2, slugs: `slugTaken` is called inside the transaction for each candidate the database
  leaves free: `Lstat` of `$ATL_TASKS/<slug>` (anything there counts, not only a
  directory), then `gitx.RefExists` for `refs/heads/atl/<slug>` and also
  `refs/remotes/origin/atl/<slug>` (after `atl new`'s fetch) in each rw repo's canonical
  clone: two workspaces with separate clones of one origin would otherwise push to the same
  branch. ro repos do not count. `buildTaskDir` keeps its "task dir already exists" error
  for a dir made between the check and the mkdir. `atl add` is unchanged (an existing
  `atl/<slug>` still fails it). Fix round: `gitx.RefOrBelowExists` (`for-each-ref --count=1
  -- <ref>`, whose literal pattern matches the ref and refs below it up to a slash, not
  `atl/<slug>-2`) replaces `RefExists` there, so a branch `atl/<slug>/x` (D/F conflict),
  local or under `origin/`, also takes the slug. `TestNewTaskRollback` now makes the second
  worktree fail with an unreadable blob (`breakCheckout`).
- W3, the selector (`tui.WorkspacePicker`, run by `tui.RunWorkspaces`) lists what
  `workspace.List` returns, as `atl workspace list` does. It is shown by `atl` alone on a
  terminal when `applyWorkspace` fails with `*workspace.NoneError` (any other selection
  error is printed as before; off a terminal `atl` still prints the help), and by `atl
  workspace open` without a name and without `-w` (off a terminal: an error naming `atl
  workspace open <name>`). `-w <name> workspace open` is the same as `workspace open
  <name>`.
- The plain selector (change request after review): the selector and `atl workspace list`
  show the name and the path (`~`-abbreviated, dimmed in the selector) and nothing else; a
  directory without `atelier.toml` is dimmed and marked "not adopted". The per-workspace
  open-task and waiting-session counts are gone, with `app.ListWorkspaces`,
  `app.WorkspaceCounts` and `app.WorkspaceInfo` (the read-only opens they did); W2's
  `openTasks`/`otherWorkspaceTasks` (port offsets) keep `store.OpenReadOnly`. `--json` is
  `name`, `path`, `adopted`, `current`.
- The selector creates and adopts through `WorkspaceSelector.Prepare` (`workspace.Prepare`
  with no flags: the same computation as `atl workspace new <name>`, nothing written) and
  `Create` (`Plan.Write`, the plan shown). `n` with a new name creates at once; `enter` on
  a "not adopted" row, or `n` with its name, shows the confirmation view in place of the
  list: each `PlannedSetting` (`key = value (from source)`), skipped values, warnings and
  notes, wrapped; `y` writes, `n`/`esc`/`q` cancel, other keys do nothing. A refusal from
  `Prepare` (`AdoptDBError`: a box's workspace with no repos found) is shown on the status
  line and nothing is asked. The name is checked as typed (`CheckName`, and not an
  existing workspace); the plan's notes go on the status line after it is written.
  `selectorAdoptable` and `WorkspaceInfo.HasDB` are gone: a box's workspace whose
  devcontainer.json names the repos is adopted from the selector like anywhere else (in
  its own box, with `tmux_socket = "default"` as on the CLI).
- Devcontainer import (change request after review): `workspace.Prepare` computes a
  `Plan` (the `Config` to write, `Settings` with their sources, `Skipped`, `Warnings`,
  `Notes`) and `Plan.Write` writes it; `Create` is both. When adopting (the directory
  exists), `<dir>/.devcontainer/devcontainer.json` is read if present: JSONC through a
  small pre-pass (`//` and `/* */` comments outside strings removed, a comma followed only
  by whitespace or comments before `}`/`]` dropped) and `encoding/json`; no new
  dependency. Variables of `containerEnv` and then `remoteEnv` (which wins, as in the
  box's shells): `ATL_REPOS`, `ATL_SANDBOX_MOUNTS` (`:`-separated), `ATL_SANDBOX_IMAGE`,
  `ATL_SANDBOX_CONTEXT`, `ATL_CONTEXT`, `ATL_TMUX_SOCKET` become their settings;
  `ATL_WORKSPACE` is ignored, with a warning when it names another directory (a name
  compared with the dir's base name, a path by path or `os.SameFile`); `ATL_SANDBOX` other
  than `docker` warns that it is not carried over; a name matching
  `KEY|TOKEN|SECRET|PASSWORD` is skipped as a secret (its value never printed or written;
  the reason names `atl token`); another `ATL_*` is skipped as not a setting; anything
  else is ignored silently. `${localEnv:VAR}` and `${localEnv:VAR:default}` (the default
  when VAR is unset or empty) expand from atl's environment; any other `${...}` skips the
  value with the reason. Mounts (string form `source=…,target=…,type=bind[,readonly]`,
  with `src`/`dst`/`destination` accepted, or the object form): only a `type=bind` whose
  target is a container user's `~/.gitconfig` (`/root/.gitconfig`,
  `/home/<user>/.gitconfig`, or `$HOME/.gitconfig` of atl's environment, since boxes
  mounted at the host's paths) counts: `git_config` = its source, relative when inside the
  workspace, nothing when it is `<ws>/gitconfig` (the default; said as skipped). The other
  mounts (workspace, repos, `~/.claude`, the Docker socket, the files of
  `ATL_SANDBOX_MOUNTS`) are the box's plumbing. Precedence per key: flags, the
  devcontainer.json, the user's environment (`FillFromEnv`'s rules: `UserEnv`), the
  default; recorded as `--repos`/`--mount`/`--image`, `.devcontainer/devcontainer.json`,
  `$VAR`. A devcontainer.json that does not parse is a warning (nothing taken from it). The
  `AdoptDBError` check runs after all three sources; `NotWorkspaceError` names the plain
  `atl workspace new <name>` when the repos are known (devcontainer or user env). A
  `tmux_socket` with `/` is refused before writing (ReadConfig would refuse the file). The
  CLI prints each `PlannedSetting`, then `skipped …`, `warning: …` and `note: …` lines on
  stderr (`atl: ` prefix), with paths `~`-abbreviated and contexts quoted (first 40
  characters); stdout stays `name<TAB>dir`. `.devcontainer/` is left in place.
- Last used: `$ATL_WORKSPACES/.last` holds the name of the workspace whose task TUI was
  started last (written, atomically, only for a workspace directly under the root; never
  creates the root; every error ignored). The selector preselects `--preselect` (hidden
  flag of `workspace open`, what `w` passes), else `.last`, else the first row. `.last`
  is not a valid workspace name, so `workspace.List` skips it.
- Opening and switching re-execute atl (`syscall.Exec`, through the package variable
  `execProcess` so tests can replace it) with `workspace.OriginalEnv()`: `$ATL_BIN`, else
  `os.Executable()`, as `atl --workspace <name>` from the selector and as `atl workspace open
  --preselect <current>` from the TUI. The App's database is closed first.
- `w` in the task TUI is a quit with `switching` set: it waits for operations exactly as
  `q` does (`requestQuit`), `q` or `ctrl+c` meanwhile clear it, and a forced quit is never
  a switch. `RunContext` returns `tui.ErrSwitchWorkspace` after the usual shutdown; the
  CLI then re-executes. The workspace's name is drawn at the right end of the task TUI's
  status line when it fits (there is no header line).
- W4, tmux config: `tmux.ServerConfig` (the box's `/etc/tmux.conf` settings, under a
  comment saying atl rewrites the file) is written by `Client.UseConfig(path, user)` to
  `Paths.TmuxConf()` (`<StateDir>/tmux.conf`) when the file's content differs
  (`gen.WriteAtomic`, parent dir created), and then passed with `-f` on every command of
  that client (the existing `config` field; tests still set it to `/dev/null`). tmux reads
  it only when a command starts the server (`new-session`, `start-server`, and
  `attach-session`, which starts one too: checked), and ignores it otherwise. Fix round: the
  file is `tmux.ServerConfigWith(Paths.UserTmuxConf())`, `ServerConfig` followed by
  `source-file -q '<ws>/tmux.conf'` (single-quoted, glob characters escaped with `\`, a
  `'` as `'"'"'`), so the user's own file is read last and wins; `-q` with a missing file
  is silent (checked with tmux 3.2a, 3.3a and 3.5a, also through a glob-escaped path). No
  workspace (XDG paths): no hook. The Dockerfile comparison skips that line. The App's
  client comes from `app.NewTmux(paths, out)` (in `app.New` and the CLI's paths-only App);
  a write error is a warning and the client goes without `-f`. Not for tmux's own choice of
  server (no socket name) nor `tmux_socket = "default"` (`Client.OwnServer`): if atl started
  the user's default server, `-f` would replace their config (the box's `/etc/tmux.conf`
  keeps applying there). The gate's copy-relay client gets no `-f` (it never starts a
  server). A test checks `ServerConfig` against the Dockerfile's printf lines.
- The copy relay needs nothing in the config: `load-buffer -w` sent OSC 52 to an attached
  `xterm-256color` client with `set-clipboard` external (the default) and also with it
  off (tmux 3.5a).
- Doctor's `tmux server` check: for tmux's default server, a pass saying it runs with the
  user's config; otherwise `#{config_files}` of the running server (which lists every
  `-f` file, existing or not, joined with commas unquoted: `tmux.ConfigListed` takes the
  generated file as listed when it is the whole value or a comma-delimited part of it, so a
  path with a comma works): a warning when the generated file is not among them (an
  older atl or a hand-started server: its options apply after a restart), a pass when it
  is or no server runs. It skips itself when tmux is not installed.
- W4, static binary: `sandbox.CheckStaticBin(path)`: ELF (else "is not a Linux (ELF)
  binary", also for an empty file), type `ET_EXEC` or `ET_DYN` (static-pie), no
  `PT_INTERP`, no `DT_NEEDED`. Neither the machine nor the OS ABI is compared: the
  Docker daemon's architecture need not be atl's, and `AtlBin` is the running binary
  unless `$ATL_BIN` says otherwise. `sandbox.StaticBinError` is the one-line refusal with
  `sandbox.BuildStatic`, `CGO_ENABLED=0 go build -buildmode=exe -o ~/.local/bin/atl
  ./cmd/atl`: a PIE (`PT_INTERP`, e.g. from `GOFLAGS=-buildmode=pie`) stays refused, since
  it needs the loader, which an image need not have; `-buildmode=exe` overrides that
  GOFLAGS, and the refusal names it as a cause. `App.CheckBin` (nil → `CheckStaticBin`)
  lets tests inject it: the app test Apps use a stand-in script for atl and set it to accept anything.
  `ensureSandbox` checks right after `checkDocker` (so "cannot reach Docker" stays the first
  thing a session says without a daemon) and before anything is written or started.
  Doctor's `atl binary` check runs only with the sandbox on, on `AtlBin`, else `$ATL_BIN`,
  else the running binary.
- No new doctor check for host basics: the tool checks (git, tmux ≥ 3.2, claude, gh) and
  the `docker` check (daemon reachable) already cover them.
- Fix round, README migration: the main path is host-side (in the box finish or close what
  runs and stop it; on the host `atl workspace new <name>`, which since the change request
  after review reads the box's devcontainer.json, flags overriding; `atl -w <name>` or the
  selector; resume with `claude --resume`). Adopting in the box (`tmux_socket = "default"`, removed when moving) is described only as the way
  to upgrade a box in place. It also says to move all workspaces of a host together: a box
  sees only its own workspace under `$ATL_WORKSPACES`. Rootless Docker and Podman are
  stated as unsupported (the gate's upstream is `DOCKER_HOST`'s unix socket or
  `/var/run/docker.sock`, not a context; a rootless daemon remaps the UID, so bind-mounted
  worktrees are not writable).
- Fix round, the task TUI's help: the list's keys (`listKeys`; "m edit notes" became "m
  notes" so that they fit two lines at 80 columns) wrap onto a second line at a two-space
  gap when they do not fit the width and the terminal has 8 lines or more; the second line
  is truncated when it does not fit either. The legend follows the keys on their last line
  when it fits. Only the list (`modeNormal`) wraps; other modes keep one truncated line.
  `listHeight` and `View` take the help's lines from `helpLines`, and `listLines` keeps
  the selection visible when the help grew since the offset was set.
- Fix round, `build-images`: `ATL_BUILD_BOX=0` builds only the task image (the script is
  configured by variables, its arguments go to `docker build`); a value other than 0 or 1
  is refused.
- Fix round, tests: `workspace.ResetOriginalEnv` (tests only) forgets the snapshot, so
  `TestReexec` compares the exec environment with the one before its own Apply exactly.
- Change request after review, fixtures: tests name their workspaces `alpha` and `beta`,
  the docs `personal` and `work` (paths like `~/src/work`, `/home/you`).
- W5, the assistant's container: one per workspace, sandbox ID `sandbox.AssistantID(dir)` =
  `_assistant-<6 hex of the assistant dir>` (container `atl-_assistant-…`). Slugs are
  `[a-z0-9-]` (`model.Slugify`), so no task ID (`<slug>-<6 hex>`) can be it; the name is in
  `ReservedName`'s pattern, so no task can create or rename a container to it. It carries
  `atelier.agent=<id>` and is found, reused, started and replaced through the same
  `sandbox.Agent`/`Ensure` as a task's. `App.assistantSpec`: the workspace's task image
  (`Sandbox.Image`, never a project's), `--user uid:gid`, workdir the assistant dir; mounts
  exactly the assistant dir (rw), `Paths.AssistantClaudeDir()` (`<run root>/claude/assistant`,
  rw, `CLAUDE_CONFIG_DIR`, seeded by `seedClaudeDirAt`, the old `seedClaudeDir` made generic)
  and `Paths.AssistantRunDir()` (`<run root>/assistant`) at `/run/atl` read-only; env `HOME`,
  `LANG`/`LC_ALL`/`TZ`, `ATL_SANDBOX_ID`, `ATL_ASSISTANT=1` (`sandbox.AssistantEnv`) and the
  host's `ATL_CONTEXT`; no `DOCKER_HOST`, no git identity, no `ATL_CONTAINER`. Both new dirs
  are names no task's hash (12 hex) can take. `ensureAssistantSandbox` runs on every `atl
  assistant` (also when the window is alive, so a dead gate comes back): Docker, static
  binary, token (one line each, as `ensureSandbox`), `InstallBin`, the gate, then `Ensure`
  with busy = the assistant window is alive (a changed spec warns and waits for the window
  to close). The window is `sandbox.WindowArgv(true, sandboxArgv(...))` with
  `ATL_ASSISTANT` passed by name and the token in `GlobalEnv`, as a task session.
  `--continue` is decided from the assistant's Claude dir with the sandbox on. atl never
  removes the container (README says `docker rm -f` it); doctor does not list it.
- W5, the assistant's gate: `atl gate --assistant` (`assistantGate`), started by
  `sandbox.StartAssistant` (the old `Start` split into `start(args, ident, ...)`), ident
  `AssistantIdent(workspace, bin)`, so another binary or workspace replaces it; injected in
  tests as `App.StartAssistantGate`. Its log is `gate-_assistant.log`, not the brief's
  `gate-assistant.log`, which is the log of a task with slug `assistant`. `Gate.NoDocker`
  refuses every Docker request (the socket stays: `Serve` and `Alive` use it). Its `Control`
  has `Workspace` set (fixed at start; a task's gate has it empty and refuses `OpAtl` as
  "not served by this gate"), `Task` = `hook.AssistantServer` (only `AssistantGuard`;
  other hook events exit 0, every other task op exits 2) and `Copy` to the `atl_assistant`
  tmux session.
- W5, `OpAtl` (`ControlRequest.Args`, the body is stdin, at most 1 MiB):
  `sandbox.CheckAssistantArgs` allows `ls show transcript repos diff new add status session
  resume pr done cancel note notes rename doctor help` and a leading `--help`/`-h`; refuses
  no command (the TUI), any other command (`open` with `ErrAssistantOpen`, the TUI/terminal
  hint), a flag before the command, `repos` with any positional argument (`repos pin`
  re-pins an origin: the user's decision; beyond the brief), and anywhere in the line (also
  after `--`, since a flag may take `--` as its value) `--workspace`, `--workspace=…`,
  `--force`, `--force=…` and any single-dash group containing `w` or `f` (`-w`, `-wx`, `-fw`;
  so `-mwhy` is refused too, which costs nothing: `-m why` works). `--force` is refused on
  every command, not only done/cancel (none of the others has it). NUL or invalid UTF-8 in
  an argument is refused. The gate runs `exec <Atl> -w <Workspace> <args>` with
  `controlEnv()` (now also without `ATL_ASSISTANT`), stdout and stderr apart, together
  capped at 16 MiB (`outputLimit`, then "output cut"), the push timeout (10 minutes) and
  hang-up cancellation (`Control.run`, shared with push/fetch). The log gets `run   atl <cmd>
  (for the assistant)` / `done … exit N` and refusals; never stdin or other arguments.
- W5, atl in the assistant's container: `runIn` sends every command line but `hook` and
  `statusline` (main.go's fast path) to the gate when `sandbox.InAssistant()` (`ATL_SANDBOX_ID`
  and `ATL_ASSISTANT` set), before cobra parses anything: the gate is the only judge. Stdin is
  read (to the end, at most 1 MiB) only when an argument is `--set` or `--set=…`: another
  command may run with a stdin that never ends. A task container that sets `ATL_ASSISTANT`
  itself only reaches its own gate, which refuses `OpAtl`. `atl hook` in any container now
  goes through the gate, `AssistantGuard` included (a task's gate refused it already;
  `viaGate` sends landings for it too and fails closed when the gate does not answer).
  `AssistantGuard` also checks `Input.landings`: each must be an allowed path.
- W5, generated files: `gen.WriteAssistantFiles(dir, atlBin, inContainer)` and
  `AssistantClaudeMD(inContainer)`; the host text is unchanged, the container text replaces
  a few passages (`inContainerMD`): no repos/worktrees/Docker, tasks through `atl show`/`atl
  diff`/`atl transcript`, `atl open` → the TUI or a terminal, `open` dropped from the
  confirm list, `--workspace`/`--force` refused. The permission rules are unchanged; the
  guard hook runs `/run/atl/bin/atl` (`hookBin`).
- W5, `atl token`: with the sandbox on (`sandbox.FromEnv` in the CLI; `pathsApp` stays
  sandbox-less) the window runs `tokenContainerScript` with `docker run --rm -it --name
  atl-token-<6 random hex> --user uid:gid -e TERM -e LANG <Sandbox.Image> claude setup-token`
  (a name, beyond the brief, so that `docker rm -f` can end a container whose client died
  with the window: with `-t` docker does not proxy the hangup). The script waits for the
  attached client and re-sends the size (SIGWINCH to the pane's process group) like
  `WindowArgv`, since Claude Code breaks the token at the width it sees. Docker and the
  default image are checked first (one line each). Sandbox off: the old script, unchanged.
- W5, doctor: with `sandbox.FromEnv()` on, the `claude` tool check is replaced by a pass
  "not needed on the host: sessions and the assistant run Claude in containers", with
  "(found: <version>)" when it is installed; off (or an invalid `ATL_SANDBOX`), required.
- W5, build: the Dockerfile's `build` stage now passes `-buildmode=exe` explicitly (it was
  the default there); README's main way is `docker build --target build` + `docker cp
  /out/atl`, and `sandbox.StaticBinError` points at it.
- W5 security review fixes. (1) `CheckAssistantArgs` accepted a leading `--help`/`-h`; cobra's
  `Find` does not know the help flag yet (it is added at execute time), so it takes the next
  argument as that flag's value: `--help --help=false token --paste` ran `token` (and
  `workspace new`, `repos pin`, `gate`, `push`, `open`), `token --paste` reading a token of
  the container's choosing from the request body. Now any first argument starting with `-`
  is refused (help is `atl help <cmd>` or `<cmd> --help`), and the gate also resolves the
  exact argv it will exec (`-w <ws>` included) with atl's own command tree,
  `Control.Resolve` = `cli.resolveAtl` (a fresh `rootCmd()`, `InitDefaultHelpCmd`,
  `InitDefaultCompletionCmd`, `Find`, as `ExecuteC` does): its `CommandPath` must pass
  `sandbox.AssistantCommandPath` (`atl <allowed>`; `atl repos pin` is not). Both checks
  must pass; a gate without `Resolve` refuses `OpAtl`. A non-empty body is refused unless
  the resolved command is `atl notes` with `--set` true (flags parsed by cobra, `--help`
  off). The allow rule `Bash(atl --help *)`, which ran such lines without a prompt, became
  `Bash(atl help *)`; the old rule and its colon form are retired (removed from existing
  settings). (2) At most `maxAtlRunning` = 3 atl commands run for the assistant at once;
  one more is refused at once (a buffered channel, no queue). (3) The launch lock moved
  from the container-writable `<assistant dir>/.launch.lock` to `<state dir>/assistant.lock`;
  `lockFile` opens with `O_NOFOLLOW|O_CLOEXEC|O_NONBLOCK` and refuses anything but a regular
  file. (4) With the sandbox off, `quarantineAssistant` moves `CLAUDE.local.md`,
  `.mcp.json`, `.claude/settings.local.json`, `.claude/{skills,agents,commands}` (all of
  `.claude` when it is not a plain directory) to `<state dir>/assistant-quarantine/<UTC
  time>/` before every host launch, renames only (symlinks moved, never followed), one
  warning. Also `.claude/settings.json` when it differs from what atl writes afresh
  (`foreignAssistantSettings`: `gen.WriteAssistantFiles` into a temp dir, bytes compared;
  not a regular file counts as foreign), so atl writes it from scratch instead of merging
  hooks and allow rules the container added; sandbox on: merged as before. (5) `atl token` runs `docker run
  --pull=never` and checks that any task image (not only the default) is on the daemon:
  the token is minted by the image's own claude.
- W5 red-team fixes: the run slot of an atl request is released as soon as the process has
  exited, before the exit status is written, and each reply write of the atl op has a
  deadline (`Control.WriteTimeout`, default 30 s); after a failed write the rest is
  dropped (`replyWriter.broken`), so a client that never reads cannot hold a slot or the
  process. `CheckAssistantArgs` refuses `--author <x>`/`--author=<x>` for any x but
  `assistant` (each occurrence), and `atl note` without `--author assistant` (the default
  author is the user, so that is refused too, beyond the brief).
- W5, after the user's call: the assistant's gate does not run `atl done` or `atl cancel`
  (they remove a task's worktrees); it refuses them with a line that says to close the task
  in the TUI (`d`/`c`) or the CLI. `atl status <task> done` stays allowed: it only changes
  the status. One assistant per workspace stays (the user's choice: keep workspaces apart).
- W5 follow-up, adversarial review (code in any container is hostile). (1) Sandbox-off
  quarantine was a denylist run on every host launch (missed `.git` with `core.fsmonitor`,
  moved the user's own `CLAUDE.local.md` each time, raced a running container). Now
  `ensureAssistantSandbox` writes `<state dir>/assistant-in-container` before `Ensure`, and a
  host launch with it (`leaveAssistantContainer`) removes the container (`sandbox.Agent` +
  `rm -f`; any Docker error refuses the launch, one line), kills the `atl_assistant`
  session, `sandbox.Stop`s the gate, renames (`renameat` from `O_NOFOLLOW` dir fds)
  everything but `CLAUDE.md` (regular) and `.claude/settings.json` (regular and equal to
  the fresh form) into `<state dir>/assistant-quarantine/<time>/`, then removes the marker;
  a failed move refuses the launch and keeps the marker. No marker: nothing moves.
  `foreignAssistantSettings` reads through `nofollow` (regular only, 1 MiB). (2) Task gates:
  `/build` `t=` (each) and `/images/create` (`fromImage` once, `tag` once; no tag at all =
  every tag, refused; by digest moves no tag) pass `Gate.checkTag`: not one of
  `reservedImages` (`atelier-task`, `atelier`, and `Gate.Reserved` = `cli.reservedImages`:
  `ATL_SANDBOX_IMAGE`/`ATL_BOX_IMAGE` from the gate's environment at start, `atelier.toml`'s
  `sandbox.image` and every project's `image` read again per request; unreadable refuses),
  compared after dockerd's normalisation (`parseImageRef`: docker.io, `library/`,
  `index.docker.io`, latest); a build tag also not an existing image without
  `atelier.task=<id>` (inspected by the normalised name). Pulls skip that (coordinator's
  call): a pull only brings the registry's content for that very name, and `compose pull`
  of a shared `postgres:16` must work. Builds get `labels` with `atelier.task=<id>` (and no
  `atelier.agent`). Tag, load, import (`fromSrc`), commit stay out of the route table. The
  assistant's `Spec.NoPull` adds `--pull=never` (any missing image is an error, one line).
  (3) Hidden global `--for-assistant` (`sandbox.ForAssistantFlag`): the assistant's gate
  inserts it right after the command (`-w <ws> <cmd> --for-assistant <rest>`, before any
  `--`); `CheckAssistantArgs` refuses it in a request, and `status` with `done`/`cancelled`
  in its arguments. With it `atl status` is `App.SetOpenStatus` (closed target or closed
  task refused, under `lockTask`) and `atl notes --set/--clear` is `App.ReplaceNotesKept`
  (old text appended to `<state dir>/notes-history/<slug>.md`, `O_NOFOLLOW`, then a
  compare-and-set `SetNotes` with that text as base, retried 3 times). A root flag rather
  than one on `status`: one insertion point, no per-command knowledge in the gate.
  (4) README builds from a separate clone, never a canonical one. (5) A gone assistant
  window: `endAssistantProcs` (from `ensureAssistantSandbox` when not busy) stops atl's own
  container (`docker stop -t 2`), which `Ensure` then starts: everything an old window
  started ends, whatever environment it scrubbed (an `ATL_SESSION`-style marker can be
  unset by the processes it marks). (6) A live window attaches with a warning when
  `ensureAssistantSandbox` fails. (7) Token container: `-e HOME=/tmp`. (10) The client's
  `readsStdin` is `resolveAtl` (`atl notes … --set --help` sends no body).
- W5 second pass (on 7bb24dd). (1) `App.PR` set `review` on any task, a closed one too,
  past the lock and reopen. Now `prReview` sets it under `lockTask` only on an open task; a
  closed one keeps its status with a warning (the PRs are still created). `PROpen`
  (`atl pr --for-assistant`) refuses a task that is not open before gh runs. (2) `/build`
  with `pull` true (dockerd's `BoolValue` reading, any occurrence) or a non-empty
  `platform` is refused: the legacy builder pulls `FROM` images (always with `pull`, on a
  platform mismatch with `platform`, moby's `GetImageAndReleasableLayer`) and stores them
  under their names, past the pull rule. Left open: a missing `FROM` image is pulled under
  its name (docs/sandbox.md). (3) The build rule is now "no `atelier.task` label at all"
  (the user's builds, pulled images), not "not this task's": tasks sharing a repo build the
  same compose names, and images tasks built before builds were labelled must stay
  rebuildable after a `docker rmi` (the refusal says so). Tasks overwriting each other's
  non-reserved tags is the old behaviour, listed in "does not do yet". (4)
  `notes-history/<slug>.md` is capped at 1 MiB (`maxNotesHistory`): under `flock`, the
  tail is read, the entry appended, and whole entries (found by their heading line) are
  dropped from the front; rewritten in place.
