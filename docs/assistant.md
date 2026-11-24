# The atelier assistant

`atl assistant [task]` (TUI `A`): one Claude Code session per workspace for managing tasks,
not writing code ("what's the status of X?", "where did I leave off?", "add repo Y to X").

- Runs in tmux session `atl_assistant`, in `<workspace>/data/assistant/`, with a generated
  `CLAUDE.md` (role, `atl` reference) and `.claude/settings.json` (merged with your keys;
  on the host, after it ran in its container, written from scratch).
- Continues its previous conversation (`claude --continue`) when one exists in its own
  Claude config dir (`<workspace>/run/claude/assistant/`); conversations from your host
  config dir are not seen.
- With a task, "Let's talk about task <slug>." is the first prompt. In an already running
  assistant it goes to the tmux paste buffer instead (typed keys could answer a pending
  permission prompt): press `<prefix> ]` to paste.
- Not a task session: crash recovery, `done` and `cancel` ignore it.
- With `ATL_SANDBOX=off` it runs next to atl, with `claude` from the host; if it ran in its
  container before, atl removes that container and moves its dir's files aside first
  ([sandbox.md](sandbox.md#switching-it-off-atl_sandboxoff)).
- Launches are serialised by `<workspace>/state/assistant.lock` (not in its dir, which its
  container can write).

## Its container

Container `atl-_assistant-<hash>`, one per workspace, from the workspace's task image
(`[sandbox] image`, never a project's; never pulled: build or pull it yourself). Logs in with
the same token as task sessions ([sandbox.md](sandbox.md#claude-code-login-the-token)).

| sees (same paths as on the host) | does not see |
|---|---|
| `<workspace>/data/assistant/`, read-write | task dirs, worktrees, repos |
| its Claude config dir, seeded from yours like a task's | the rest of the workspace |
| its run dir (atl binary, gate sockets) at `/run/atl`, read-only | `[sandbox] mounts` |
| | Docker (its gate refuses every Docker request) |

It gets `context` / `ATL_CONTEXT` (not `[sandbox] context`) and the copy relay.

- Created by the first `atl assistant`, reused, restarted after a reboot.
- Replaced, once its window is closed (`atl assistant` warns until then), when its image,
  the paths it mounts, your UID/GID, or `HOME`, `LANG`, `LC_ALL`, `TZ`, `ATL_CONTEXT` change.
- When its window is gone, the next `atl assistant` stops the container before it opens a
  new window, ending what the old one left running (an old `claude`, background jobs).
- Removed by the next `atl assistant` with `ATL_SANDBOX=off`, or by you:
  `docker rm -f atl-_assistant-<hash>` (`docker ps` shows the name). Its Claude dir and
  conversation stay; the next `atl assistant` creates a new container.

## Its gate

`atl` in the assistant's container sends the command line to the assistant's gate, which
runs `atl -w <workspace> <command> --for-assistant …` on the host, with the gate's
environment, and returns output and exit status. With `--for-assistant` (hidden; the gate
always adds it and refuses it in a request), `atl status` only moves an open task between
open statuses, `atl pr` refuses a closed task, and `atl notes --set`/`--clear` first append
the notes they replace, dated, to `<workspace>/state/notes-history/<slug>.md` (which no
container sees; the oldest entries go past 1 MiB). Log:
`<workspace>/state/gate-_assistant.log` (command names, exit status, refusals; never other
arguments or stdin).

A request runs only if both checks pass:

1. the command line itself: the command comes first and is on the list below, and no
   refused flag appears anywhere (also after `--`);
2. the command atl's own parser (cobra) resolves the line to, exactly as it will run it,
   is on the list (so `atl --help --help=false token …`, where cobra takes
   `--help=false` as the value of `--help`, cannot reach `token`).

| limit | |
|---|---|
| stdin | only for `atl notes … --set`; the gate refuses a body with any other command |
| running at once | 3; another request is refused at once (try again later); a slot is freed when the command exits, even if the client stops reading (each reply write has a 30 s deadline) |
| time | 10 minutes per command |
| output | 16 MiB (stdout and stderr together), then cut |

Allowed: `ls`, `show`, `transcript`, `repos`, `diff`, `new`, `add`, `status`, `session`,
`resume`, `pr`, `note`, `notes`, `rename`, `doctor`, `help`. Help is
`atl help <command>` or `atl <command> --help`.

Refused, with a one-line reason:

| request | why |
|---|---|
| any other command | not on the list |
| a flag first (`atl --help …`, `atl -h …`) | cobra may take the next argument as its value and run what follows |
| `atl open` | attaches a terminal: open the task in the TUI or with `atl open <task>` in a terminal |
| `atl done`, `atl cancel` | they remove the task's worktrees: close tasks yourself (TUI `d`/`c`, or the CLI) |
| `atl status` to `done`/`cancelled`, or on a closed task (refused by atl) | closing is yours; reopening removes the task's containers, networks and volumes |
| `atl pr` on a closed task (refused by atl) | it would move the task to `review` |
| `--for-assistant` in any spelling | the gate sets it |
| `atl repos pin` (`repos` with any argument) | accepting a changed origin URL is your decision |
| `-w` / `--workspace` in any spelling | the workspace is the gate's own |
| `-f` / `--force` in any spelling | no allowed command has it |
| `--author` other than `assistant` (`--author user`, `--author=user`), or `atl note` without `--author assistant` | its notes must not read as yours |

Text that looks like one of those flags is refused too.

The gate also answers the assistant's `AssistantGuard` hook. No answer from the gate means
the edit is refused.

## Permissions

| rule | commands |
|---|---|
| allowed | `atl ls`, `show`, `transcript`, `repos` (not `repos pin`), `help`; Read, Grep, Glob (the old `atl --help *` rule is removed) |
| ask first | `atl new`, `add`, `status`, `session`, `resume`, `pr`, `note`, `notes`, `rename` (all of `atl notes`: it reads notes with `atl show <task> --notes`); `done`, `cancel`, `open` too, which only matters with the sandbox off (its gate refuses them) |
| denied | `git commit`/`push`/`checkout`/`reset`/`rebase`/`merge`/`stash`/…, `rm`, `mv`, `cp`, `sed -i`, `tee` |
| hook | Edit/Write/MultiEdit/NotebookEdit outside its dir, or on its own `CLAUDE.md`, `.mcp.json`, `.claude/`: blocked (fails closed) |

With the sandbox on, these rules and Claude Code's permission prompt run inside the
container: an assistant that ignores them (steered by notes or transcripts a task wrote, say)
is held back only by its gate's allowlist. It can still, without asking you: create tasks,
add repos, rename, move open tasks between open statuses, start and resume sessions, open
PRs, add notes, and replace or clear notes (kept in the notes history); and edit anything in
its own dir. It cannot close, reopen or open a task, push, re-pin a repo's origin, reach
Docker or another workspace. Read what it asks to run before you approve.
