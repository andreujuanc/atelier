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

On SessionStart (startup, resume, `/clear`, compaction) the hook injects fresh task context,
in this order: title, status, branch, repos with base branch or read-only warning,
`ATL_PORT_OFFSET` (in a sandbox, the sandbox note and the read-only mounts line, at most 240
bytes); the learnings you accepted, as a "User preferences" section (`[learn] inject_budget`,
800 bytes, at most 4000; see [learning.md](learning.md#5-delivery)); the environment notes
(below); an atl cheat sheet, the atl commands the session may run (in a sandbox the complete
list, the gate refusing the rest; on the host also `show`/`diff`/`transcript`, and which
commands to leave to the user; at most 855 bytes), so agents need not run `atl help`; and
last the task's notes.

The whole context stays within 9,900 characters: Claude Code caps a hook's context at
10,000 characters ([hooks](https://code.claude.com/docs/en/hooks)) and past that replaces
all of it with a file path and a 2,000-character preview. Characters are counted as
JavaScript counts them (UTF-16 code units: an emoji counts 2); the 100 spare are a safety
margin. The notes get the room the rest leaves, about 8,400 characters in a usual task
(7,500 sandboxed): when they do not fit, the oldest lines are dropped (the end is kept, from
a line start, or a character boundary when one line is too long) after a line "… (earlier
notes truncated; `atl notes <slug>` for all)"; with less than 40 characters of room they are
left out. The other sections always fit: at their largest (4000-byte preferences,
1000-byte environment notes, sandbox note, mounts line, cheat sheet, 200-character title)
they take about 7,500 characters, leaving room for a dozen repos; a task block that would
not fit (very many repos) is cut at a line end, a "…" line in place of the rest,
`ATL_PORT_OFFSET` kept. The assistant gets the preferences section alone as its start context
(`atl hook AssistantStart`).

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
