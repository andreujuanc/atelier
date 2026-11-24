# Tasks and sessions

## Slugs

`atl new` derives the slug from the title: lowercase, each run of characters outside
`a-z0-9` becomes `-`, at most 40 characters (`task` if empty). If taken, it becomes
`<slug>-2`, `<slug>-3`, … A slug is taken when:

- a task of the workspace has it (open or closed);
- something exists at `$ATL_TASKS/<slug>` (left alone);
- branch `atl/<slug>` or `atl/<slug>/…` exists, locally or under `origin/`, in the
  canonical clone of one of the task's rw repos. atl never takes over a branch it did not
  create.

The slug never changes, also after `atl rename`. `atl add` of an rw repo whose clone already
has `atl/<slug>` (or `atl/<slug>/…`), locally or under `origin/`, fails and keeps that branch.

Titles: trimmed, spaces collapsed, at most 200 characters, no control characters.
`atl rename` (TUI `t`) changes only the title and rewrites the task's `CLAUDE.md`.

## Opening sessions

TUI `enter`:

- no openable session: starts a new one and attaches;
- otherwise a picker: "New session", then live sessions, then resumable past ones, newest
  first. The most recent is highlighted, so `enter` continues it.

A past session is offered only if its transcript still exists in Claude's project dir for
the task dir (so a `claude` you ran in a subdirectory is not offered). Closed tasks offer no
past sessions. A live session is attached to, never resumed over; a past one is resumed with
`claude --resume <id>`.

If the open finishes after you moved on (editor, form, filter), atl does not steal your
keys: the status line says "ready — press enter again".

`atl open <task>` with no flags: attach if a window is live, else resume stopped sessions,
else start a new one. `--session ID` opens that session (as the picker would); `--new`
starts a new one. They are mutually exclusive.

## Crash recovery

Every `atl` command (except `hook`, `statusline`, `doctor`) marks sessions whose tmux window
is gone as `stopped`; the TUI does so every few seconds. In a sandboxed task, atl also ends
what that session left running in the container. `atl resume <task>` (TUI `r`) relaunches
stopped sessions with `claude --resume`.

## Notes

One free-form text per task, shown under the task's details in the TUI. Agents and the
assistant append with `atl note`; you edit with `m`.

Editor keys:

| key | action |
|---|---|
| `ctrl+s` | save and leave. If the notes changed meanwhile (an agent ran `atl note`), the editor stays open: `ctrl+s` again overwrites, `ctrl+r` reloads theirs |
| `esc` | leave (asks before discarding unsaved changes) |
| `ctrl+r` | reload the stored text, dropping your edits |

The TUI editor turns tabs into 4 spaces; `atl notes --set` keeps them.

CLI:

```sh
atl note <task> waiting on design review   # appends "- YYYY-MM-DD [user] waiting on design review"
atl notes <task>                           # print
atl notes <task> --set < notes.md          # replace; an empty stdin is an error
atl notes <task> --clear                   # clear
atl show <task> --notes                    # print (read-only spelling)
```

Limits: 64 KiB; control characters other than newline and tab are dropped. Notes work on
closed tasks.

## Closing: done and cancel

`atl done` / `atl cancel` (TUI `d` / `c`) run the project's teardown, remove each worktree,
the task dir, the task's container and everything the task created on Docker (containers,
networks, volumes), and kill its tmux session.

Branches: `atl/<slug>` is deleted only if every commit on it is reachable from another local
branch or remote-tracking ref (local refs only; no fetch). Otherwise it is kept and
reported: `kept branch atl/<slug> in <repo> (N unpushed commit(s))`.

- `--force` discards uncommitted changes. It never discards unpushed commits.
- Refused even with `--force`: a worktree whose HEAD has commits no branch, remote-tracking
  ref or tag reaches (detached HEAD, rebase in progress). Put them on a branch first.

TUI dialog: lists what will be deleted. If a worktree has uncommitted changes, tick
`[ ] Delete them` with `space`, then `enter` (equivalent to `--force`); `esc` or `n`
cancels. Changes that appear after the dialog opened make the close refuse.

The task's Claude dir keeps its transcripts after closing; delete
`<workspace>/run/claude/<hash>` yourself when no longer needed.

## Status and reopening

`atl status <task> done|cancelled` (TUI `4`/`5`) only changes the status; no cleanup.

Setting a done or cancelled task back to `backlog`, `active` or `review` reopens it. First
what is left of its sandbox is removed: its container and every container, network and volume
the task created (volume data included), its gate, and its Claude dir except transcripts; its
next session starts a new container. Then the task dir, `CLAUDE.md` and settings come back,
each rw worktree is recreated on `atl/<slug>` (from the kept local branch, else
`origin/atl/<slug>`, else a new branch from the base, with a warning), and the project's
setup runs. The task gets its old port offset back. Nothing is resumed automatically. If a
step fails, the task stays closed and what was made is removed (a removed sandbox stays so).
The assistant can do neither: its gate refuses closing and reopening.

## show and transcript

`atl show` never touches the network: "pushed" means `origin/atl/<slug>` exists locally (as
of your last push or fetch). `--json` is a stable snake_case object (see
`internal/cli/info.go`).

`atl transcript` condenses a session's Claude Code transcript to user/assistant text; tool
calls become one-line `[tool: Name] …` summaries, tool results are skipped. `--tail N` (default
20, 0 = all).
