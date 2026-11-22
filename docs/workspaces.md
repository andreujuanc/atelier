# Workspaces

A workspace is `$ATL_WORKSPACES/<name>/` (default root `~/atelier/workspaces`). It is a
workspace only if it has `atelier.toml`. Names: lowercase letters, digits, `-`, `_`; start
with a letter or digit; at most 32 characters.

```
<workspace>/
  atelier.toml  settings (see README)
  tasks/        task dirs and worktrees (unless ATL_TASKS is set)
  data/         atelier.db, assistant/
  config/       projects/<name>.toml
  state/        hook.log, gate-<slug>.log, refused.log, tmux.conf (generated)
  run/          gate sockets, task and assistant Claude dirs, origin pins, claude-token.json
  repos/        canonical clones (unless `repos` points elsewhere)
  gitconfig     optional: global git config for atl and all it runs
  tmux.conf     optional: your settings for the workspace's tmux server
```

## Which workspace

First match wins:

1. `-w` / `--workspace <name|path>`
2. `$ATL_WORKSPACE` (name or absolute path)
3. the current directory, if it is in `$ATL_WORKSPACES/<name>` (no search up for
   `atelier.toml`)

With none, `atl` on a terminal shows the workspace selector; other commands fail and list
the workspaces. `atl workspace …` needs no workspace.

## How settings apply

atl exports each `atelier.toml` setting as its environment variable (`repos` → `ATL_REPOS`,
`git_config` → `GIT_CONFIG_GLOBAL`, `context` → `ATL_CONTEXT`, `tmux_socket` →
`ATL_TMUX_SOCKET`, `[sandbox]` → `ATL_SANDBOX_IMAGE`, `ATL_SANDBOX_MOUNTS`,
`ATL_SANDBOX_CONTEXT`), so everything it starts inherits them.

- A variable you set wins over the file.
- A variable atl set itself (recorded in `ATL_WORKSPACE_VARS`) does not: `atl -w other` run
  from a window of this workspace gets `other`'s settings.
- `atl doctor` shows each setting, its value and its source (`env`, `atelier.toml`,
  `default`).

`git_config`: who commits, `insteadOf` rules, credential helpers. Task containers get only
`user.name` and `user.email` from it. `atl doctor` warns if the file is missing.

## tmux

Each workspace has its own tmux server, `tmux -L atl-<name>`; atl never touches your own
server. Attach by hand:

```bash
tmux -L atl-<name> attach -t atl-<slug>
```

atl starts that server with `<workspace>/state/tmux.conf`, which it regenerates (edits there
are lost): no status bar, mouse on, `escape-time 10`, **Ctrl-q** detaches, `tmux-256color`
with RGB. It ends with `source-file -q <workspace>/tmux.conf`: put your settings there (e.g.
`source-file ~/.tmux.conf`), but keep Ctrl-q free. Neither `/etc/tmux.conf` nor
`~/.tmux.conf` is read otherwise.

tmux reads config only at server start; the server restarts after its last session ends.
`atl doctor` warns when the running server lacks the generated file.

`tmux_socket = "default"` uses your default tmux server and your config. Sessions on another
server are invisible to atl, which then marks them stopped and ends them.

## Between workspaces

Workspaces share the Docker daemon, the host's ports and, if `repos` is the same directory,
the canonical clones. Containers and gate sockets are named by a hash of the task dir, so they
never collide.

**Port offset.** A new task gets the lowest multiple of 100 that no open task (backlog,
active, review) in any workspace has. Other workspaces' databases are only read; one that
cannot be read is skipped with a warning. Reopened tasks get their old offset back, even if
taken meanwhile. Two simultaneous `atl new` in different workspaces can collide: `atl doctor`
warns about duplicate offsets (`personal/fix-login and work/api-docs (+300)`); close one.

## Commands

`atl workspace new <name> [--repos DIR] [--mount PATH]... [--image IMAGE]` creates
`<name>/` with `atelier.toml` and the subdirectories. On an existing directory without
`atelier.toml` it adopts it, keeping its contents. It refuses when:

- `atelier.toml` already exists;
- the gate socket path would exceed 107 bytes;
- the directory has a database (a box's workspace) but no repos are given (`--repos`,
  `$ATL_REPOS` or the box's `devcontainer.json`): the default `repos/` would leave the box's
  clones behind.

Each setting comes from the flag, else `.devcontainer/devcontainer.json`, else an `ATL_*`
variable you set. It prints each setting written with its source, and each value skipped.

`atl workspace list [--json]`: every valid-named directory, `*` on the selected one,
"not adopted" for those without `atelier.toml`.

`atl workspace open [<name>]`: the TUI in that workspace; without a name, the selector.

**Selector keys:** `↑`/`↓` (`k`/`j`) move, `enter` open, `n` new, `/` filter, `r` reload,
`q` quit. `enter` on a "not adopted" directory previews what adopting writes; `y` adopts.
It starts on the last opened workspace (`$ATL_WORKSPACES/.last`). TUI `w` returns to it.

## Moving from a devcontainer box

The box used the host's paths, Docker daemon and user, so tasks, worktrees, the database and
containers stay where they are. Move all workspaces of a host together: a box sees only its
own workspace, so port offsets can collide while some still run in a box.

1. In the box: let sessions reach a stopping point, quit `atl`, stop the box.
2. On the host: `atl workspace new <name>` (or `enter` on it in the selector). It reads
   `.devcontainer/devcontainer.json` and writes `atelier.toml`; flags override.
3. `atl -w <name>`; resume sessions with `enter`, `atl open` or `atl resume`. `atl doctor`
   should show `$ATL_TMUX_SOCKET = atl-<name> (default)`.

What carries over:

| in `devcontainer.json` | in `atelier.toml` |
|---|---|
| `ATL_REPOS` | `repos` |
| `ATL_SANDBOX_MOUNTS` | `[sandbox] mounts` |
| `ATL_SANDBOX_IMAGE`, `ATL_SANDBOX_CONTEXT`, `ATL_CONTEXT`, `ATL_TMUX_SOCKET` | `[sandbox] image`, `[sandbox] context`, `context`, `tmux_socket` |
| a file mounted as `~/.gitconfig` | `git_config` (omitted for `<workspace>/gitconfig`, the default) |
| `ATL_SANDBOX=off` | not carried over (warning) |
| tokens, keys, secrets | never written: use `atl token` (the stored one is still in `run/`) or the environment |
| workspace/repo/`~/.claude`/Docker socket mounts, `--group-add`, `--security-opt` | not needed on the host |

Variables come from `containerEnv` and `remoteEnv` (wins). `${localEnv:VAR}` is expanded;
other `${…}` values are skipped. `.devcontainer/` is left in place.

Example, a box with an extra npm config:

```console
$ atl workspace new work
atl: repos = ~/src/work (from .devcontainer/devcontainer.json)
atl: sandbox.mounts = ~/.work.npmrc (from .devcontainer/devcontainer.json)
atl: skipped the mount on /home/you/.gitconfig: its source is /home/you/atelier/workspaces/work/gitconfig, which git_config already defaults to
work	/home/you/atelier/workspaces/work
```

writes (with absolute paths):

```toml
repos = "~/src/work"

[sandbox]
mounts = ["~/.work.npmrc"]
```

Upgrading a box in place instead: run `atl workspace new <name>` inside the box. Because the
box's sessions run on tmux's default server, it writes `tmux_socket = "default"`. Delete that
line when moving to the host. Never run atl for one workspace in the box and on the host at
the same time.

## Legacy: the box

`./build-images` (without `ATL_BUILD_BOX=0`) also builds `atelier`, an image with atl,
claude, tmux, git and gh, to run atl itself in a container. Run it with `docker run -it
atelier` plus a devcontainer's mounts, start `atl` in the plain shell (not inside tmux), and
use Ctrl-q to return to the TUI. `ATL_BOX_IMAGE` renames it. New setups should run atl on the
host.
