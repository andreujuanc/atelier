# Sandbox

Each task gets its own Docker container (`atl-<slug>-<hash>`); its Claude sessions run there.
The assistant gets one per workspace ([assistant.md](assistant.md)). atl stays on the host
and is the only thing that talks to the Docker daemon. Claude Code runs from the image, so
the host needs no `claude`. On by default. `ATL_SANDBOX=off` runs sessions (and the
assistant) next to atl instead, with the host's `claude`: a temporary escape hatch that is
going away.

```
atl (host) ── docker exec ──▶ task container: claude, your tools, the task dir
   │
   └─ atl gate <slug> ◀── /run/atl/docker.sock ── docker / docker compose in the task
          │  │        ◀── /run/atl/atl.sock ───── hooks, status line, note(s), push, fetch, wl-copy
          │  └──▶ atelier's database (this task's rows only)
          ▼
     Docker daemon
```

## Switching it off (`ATL_SANDBOX=off`)

With `ATL_SANDBOX=off`, Claude on the host reads what containers could write:

- The assistant dir, if the assistant ran in its container since the last host launch (a
  marker in `<workspace>/state/`, which no container sees). Before the assistant starts on
  the host, atl removes that container (if Docker cannot confirm it is gone, the assistant
  does not start), stops its gate, then moves everything in the dir except `CLAUDE.md` and
  `.claude/settings.json` (only if it is what atl writes fresh) to
  `<workspace>/state/assistant-quarantine/<time>/`, symlinks as they are, and prints one
  warning naming what moved: a `.git` with `core.fsmonitor` would run on any git there.
  Nothing is deleted; move back what you trust. Once per container run: your own files there
  stay on later host launches.
- Task dirs: no such step. A task's `CLAUDE.md`, `.claude/`, `.mcp.json` and worktrees were
  writable by its container; read them before you run a task's sessions on the host.

## What the container sees

At the same paths as on the host:

- the task dir (rw worktrees inside it);
- the `.git` of each rw repo's canonical clone (shared with every task that has that repo rw);
- each ro repo, read-only;
- the task's own Claude config dir (`<workspace>/run/claude/<hash>`, as `CLAUDE_CONFIG_DIR`);
- the paths in `[sandbox] mounts`, read-only.

It does not see: the Docker socket, your SSH agent, other repos, other tasks, your Claude
config dir, atelier's database, state dir (logs) or config dir (projects).

## Lifecycle

- Created on the task's first session; reused by later ones (installed tools persist);
  restarted after a reboot; removed by `atl done`/`atl cancel`.
- Replaced when the task's repos, image or mounts change, at the next session start while no
  other session runs in it. A running container cannot get a new mount, so adding a repo
  (`atl add`, TUI `a`) to a task with live sessions restarts them: their windows close, the
  container is removed and the same conversations are resumed (`claude --resume`) in a new
  one that has the repo. While things run in the task (Claude mid-turn, background work an
  idle Claude still has in flight: subagents, shells, monitors, as its Stop hook reports
  them; Bash tool commands in the foreground or background, processes they left running,
  `docker exec` shells) nothing is added: the TUI shows what runs, checks again every 2s (read-only) and adds once it is
  idle (or on a ticked "Force restart"); `atl add` refuses unless `--force-restart`. A
  session that has not reported its Claude id, or whose Claude is not found in the
  container, also counts as busy. Repos are checked and fetched before anything is
  closed (a bad repo restarts nothing), and once the sessions are closed an interrupt no
  longer stops the add and resume. A repo is never added under sessions that would not see
  it. Not seen as busy: with the sandbox off, processes an idle session left running on the
  host; and the background work of a Claude Code too old to report it to its Stop hook.
  Background work Claude ends without starting a turn (one you stop from its task list)
  may still show until its next turn: force it then.
- When a session ends, what it left running in the container (a background dev server) is
  killed. What should outlive a session belongs in a `docker compose` service.
- atl only touches a container named like the task's if it carries atl's `atelier.agent`
  label and no `atelier.task` label. Anything else under that name makes the session start
  fail; remove or rename it yourself.

## Image and tools

Image: the project's `image`, else `ATL_SANDBOX_IMAGE` / `[sandbox] image`, else
`atelier-task`. A missing `atelier-task` fails with the build command; another missing image
is pulled for a task, never for the assistant or `atl token` (`--pull=never`: they run the
image's `claude` with your token).

`atelier-task` has Claude Code, git, the Docker CLI with compose, Node.js LTS with npm and
corepack, and curl, jq, ripgrep, zip/unzip, xz, procps, ssh client. No other language, no
compiler, no `atl` (mounted in), no tmux, no `gh`. `--build-arg NODE_MAJOR=<n>` picks another
Node.js line.

A custom image needs `claude` and `sh`, and should have your user (same name, UID, home) so
files the task writes stay yours. An image built `FROM atelier-task` has both.

Where tools come from, in order:

1. The repo's own container setup (`.devcontainer`, compose files), run through the gate.
   If it needs something the gate refuses, the session overrides that part with a file in
   the task dir (e.g. a compose override passed with `-f`), never by editing the repo, and
   tells you.
2. The project's `image`.
3. Installed in the session: `npm install -g` as the user, or as root:
   `docker exec -u 0 $ATL_CONTAINER apt-get install -y <pkg>`. Lost when the container is
   replaced.

The session's start context tells Claude all of this.

## The Docker gate

`docker` and `docker compose` in a task talk to the gate, not the daemon. Log:
`<workspace>/state/gate-<slug>.log`.

| rule | why |
|---|---|
| everything the task creates gets label `atelier.task`; it sees, stops and removes only its own containers, networks, volumes | isolation between tasks |
| bind mounts only from the task dir, or read-only from `[sandbox] mounts` | no host access |
| a mount of `/var/run/docker.sock` becomes the gate's socket | testcontainers/localstack stay under the same rules |
| refused: privileged, host network/PID/IPC, added capabilities, devices, security options, other runtimes, volume driver options, log drivers other than `json-file`/`local`/`none`, unknown container settings | each is a way out of the container |
| images: pull and build allowed, remove refused; build from a remote URL refused | images are shared; the daemon would fetch with host credentials |
| a pull or build may not name a tag atl's own containers run from (`atelier-task`, `atelier`, the workspace's and every project's image, in any spelling); a build may not name a tag whose image has no `atelier.task` label (yours, or pulled: build under another tag), nor pull (`--pull`, `--platform`: pull the base image first); builds get the task's label; pulling all tags, tag, import, load, commit refused | a task could swap what later tasks, the assistant and `atl token` run |
| BuildKit refused (`DOCKER_BUILDKIT=0` is set) | the gate cannot inspect BuildKit sessions |
| the task's container is joined to every network the task creates; the task cannot stop, remove, or impersonate (`atl-<slug>-<hash>` names) it | services answer by compose service name |
| form-encoded request bodies refused | Docker would read values the gate did not check |

## Host files for every task

For files a build needs from the host, such as a private registry's `.npmrc`:

```toml
[sandbox]
mounts = ["~/.work.npmrc"]
```

or `ATL_SANDBOX_MOUNTS=~/.work.npmrc` (`:`-separated). Every task's container gets each path
read-only at the same path, and containers the task creates may bind-mount them (or paths
inside a listed directory) read-only only: a read-write request is made read-only, `z`/`Z`
are dropped.

Every task can read what is listed, whatever the file permissions (a task can be root in its
container). List only what you would hand to any task.

atl refuses to start when a path is not absolute, does not exist, is not a file or
directory, contains a comma or quote, is `/`, or overlaps what the sandbox keeps from tasks:
the workspace, its run dir, the repos dir, the tasks dir, your Claude config dir, your git
config (`GIT_CONFIG_GLOBAL`, `~/.gitconfig`, `~/.config/git`), the Docker socket, the SSH
agent socket, `/proc`, `/sys`, `/dev`. `atl doctor` lists each path and why it is refused.

## atl inside the container

Only hooks, the status line, `atl note`, `atl notes` (own task only), `atl push` and
`atl fetch` work; they ask the gate, which runs them on the host for this task only. Every
other command fails with one line (the assistant's container forwards an allowlist instead,
see [assistant.md](assistant.md#its-gate)). A hook whose gate is unreachable fails open,
error on stderr, except that an oversized PreToolUse input is still refused and the
assistant's guard fails closed.

A gate started by an older `atl`, with other mounts, or for a deleted task is replaced at the
next session start.

## Claude Code login: the token

Your Claude login, if any, stays on the host. Task sessions and the assistant log in with a
token:

```bash
atl token            # claude setup-token in a tmux window; open the URL, approve, press Enter
atl token --paste    # store a token you already have (no echo, or from stdin)
```

`atl token` runs `claude setup-token` in a throwaway container of the task image
(`docker run --rm -it --pull=never`, as your user, no mounts, only `TERM`, `LANG` and
`HOME=/tmp`); with `ATL_SANDBOX=off`, on the host. The token is minted by that image's own
`claude`, so the image is never pulled for it: a missing image (also a custom
`[sandbox] image`) is an error.

It is stored in `<workspace>/run/claude-token.json` (mode 0600), which no container sees.
`CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY` in atl's environment win over the stored token.

- It lasts a year: `atl doctor` warns 30 days before, fails after. Run `atl token` again.
- Passed to each session by name (`docker exec -e`): never on a command line or in the
  container's config. A new token reaches the next session without a new container.
- Token sessions can only make model requests: no claude.ai connectors, no Remote Control.

## The task's Claude config dir

Each task has its own `CLAUDE_CONFIG_DIR` (state, transcripts, history), so `claude --resume`
works across new containers. On each new container atl copies in, from your config dir:
`settings.json`, `CLAUDE.md`, `keybindings.json`, `skills/`, `agents/`, `commands/`.

- Stripped from `settings.json`: `apiKeyHelper` and other credential-running settings, and
  `env` entries whose name contains `KEY`, `TOKEN`, `SECRET`, `PASSWORD`, `PASSWD`,
  `CREDENTIAL` or `AUTH`, or starts with `AWS_`. Everything else copied is readable by the
  task.
- Not copied: your login, `.claude.json` (account, your MCP servers), transcripts,
  plugins, scripts your settings point to (a status line script must exist in the image).
  Put MCP servers a task needs in the repo's `.mcp.json`.
- Sessions started with the sandbox off keep transcripts in your own config dir and cannot
  be resumed in a container.

## Copying to your clipboard

Claude Code's mouse selection copies via `wl-copy`. In the container `wl-copy` is atl, which
sends the text to the gate, which puts it in a tmux buffer and your terminal's clipboard
(OSC 52) via the task's attached tmux client.

- Needs a terminal that accepts OSC 52 and that tmux knows does (`xterm*` by default; add
  others to `terminal-features`).
- Nothing is copied when no terminal is attached to the task's tmux session.
- At most 1 MiB per copy. `wl-copy --primary` copies nothing.
- Write-only: `wl-paste` is refused; pasting an image into a sandboxed Claude does not work
  (pasting text through your terminal does).
- Sessions run with `WAYLAND_DISPLAY=atl-copy-relay`, so Claude Code tries `xdg-open` for
  links and sign-ins, fails, and shows the URL.

## Start context

A sandboxed session's start context explains the sandbox (repo setup first, what the gate
refuses, `atl push`/`atl fetch`) and names the mounted host paths. `context` /
`ATL_CONTEXT` is not passed to task containers (it describes the host); use
`[sandbox] context` / `ATL_SANDBOX_CONTEXT`. The assistant gets `ATL_CONTEXT`, also in its container.

## What the sandbox does not do yet

- Tasks sharing an rw repo are not isolated: both mount its `.git` read-write, so a hook,
  `core.hooksPath`, `core.fsmonitor` or filter written by one runs in the other, reaching its
  worktree, transcripts, token and gate.
- The task can read the Claude token (or API key) and use it elsewhere until you revoke it.
- The git database of rw repos (other branches, hooks, config) is writable from the task.
  Only atl's own git on the host is guarded ([git.md](git.md#how-atl-runs-git-safely)).
- A task can swap a checked mount path for a symlink before the daemon mounts it.
- After a gate restart (atl upgraded), services that mounted the Docker socket must be
  recreated.
- Notes and transcripts a task writes reach the assistant as prompt input. Claude Code's
  permission prompt runs in the assistant's container, so a hostile one skips it: the gate's
  allowlist is the only real limit ([assistant.md](assistant.md#permissions)).
- Image names: rules are checked when the request arrives (a tag created while a build runs
  is overwritten); tasks can build over each other's tags (and an image built `FROM` a
  task's carries its label); a build pulls a missing `FROM` image under its own name (it
  matters only for a reserved name not built yet).
- The assistant's container can read the token too; it has no repo, task dir or Docker.
- A task can write (never read) your clipboard while your terminal is on its tmux session.
  Read what you paste into a shell.
- No BuildKit.
- Tasks can bind any free host port; no network egress policy.
- Project setup and teardown scripts run on the host.
- No git credentials or `gh` in the task: use `atl push`/`atl fetch`; `atl pr` runs on the
  host.

## Requirements and troubleshooting

- A static Linux `atl`, a token, Claude Code in the task image, and Docker reachable.
  `atl doctor` checks each and says what to do.
- The workspace, repos and mount paths must exist at the same paths for atl and the daemon.
- The gate socket `<workspace>/run/<12 chars>/docker.sock` must fit in 107 bytes:
  keep workspace paths short.
- Docker unreachable: start it, fix your access, or set `ATL_SANDBOX=off` temporarily.
- A file `atl` reads or writes that the task can also write (`CLAUDE.md`, `.claude/`,
  transcripts) is opened without following symlinks; a `.claude` symlink makes the launch
  fail.
