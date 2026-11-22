# Hooks, session state and status line

## Hooks

Before every Claude launch, atl writes `<task dir>/.claude/settings.json`, merged with keys
already there (yours are kept). It adds:

- hooks running `atl hook <event>` for SessionStart, UserPromptSubmit, Notification,
  PostToolUse, Stop, SessionEnd and PreToolUse;
- a `statusLine` running `atl statusline` (see below).

Your global `~/.claude/settings.json` is only read, never written.

What the hooks do:

- keep session state: `running`, `waiting` (permission prompt), `idle`, `ended`
  (`stopped` comes from crash recovery, see [tasks.md](tasks.md#crash-recovery));
- record each session's transcript path;
- PreToolUse blocks Edit/Write/MultiEdit/NotebookEdit inside ro repos and inside the
  canonical clone of the task's rw repos (edits belong in the worktree).

Hook failures never block Claude (except that guard) and go to `<workspace>/state/hook.log`.
In a sandboxed task the gate runs them.

## Start context

On SessionStart (startup, resume, `/clear`, compaction) the hook injects fresh task context:
title, status, branch, repos with base branch or read-only warning, `ATL_PORT_OFFSET`, and
the notes. At most 2500 bytes; if notes do not fit, the newest end is kept.

`context` / `ATL_CONTEXT` (or `[sandbox] context` in sandboxed tasks) is added as an
"Environment notes" section, max 1000 bytes. A longer value makes `atl` and `atl doctor`
fail.

## Status line

Each task session shows its task under Claude's prompt, e.g. `● Fix login`; the dot has the
session state's colour (`NO_COLOR` disables colour).

It appears only if you have no global `statusLine` in `~/.claude/settings.json` (or
`$CLAUDE_CONFIG_DIR/settings.json`): project settings would override yours, so atl leaves it
out. To get both, call `atl statusline` from your own script with the same JSON on stdin; it
prints one line or nothing. A `statusLine` of yours in the task's `.claude/settings.json` is
left alone.

In a sandboxed task your status line script runs in the container and must exist there.
Like hooks, Claude Code runs it only in trusted dirs and not with `disableAllHooks`.
