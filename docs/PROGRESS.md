# atelier — progress

Resume point for the orchestrator. Spec: `SPEC.md`. Plan: `docs/PLAN.md`.

## Status

| Milestone | State |
|---|---|
| Phase 0 — PLAN.md | done, approved 2026-11-04 with 5 changes (applied) |
| M1 models + store + migrations | done (reviewed, 1 fix round) |
| M2 git worktree management | done (reviewed, 1 fix round) |
| M3 tmux + session launch/resume/attach | done (reviewed, 1 fix round) |
| M4 hook, state machine, ro guard, CLAUDE.md/settings.json | done (reviewed, 2 fix rounds + guard re-review) |
| M5 CLI | done (reviewed, 1 fix round) |
| M6 TUI | done (reviewed, 1 fix round) |
| M7 projects, scripts, ports, diff, pr | done (approved + 1 fix round) |
| M8 crash recovery + e2e | in progress |

## Decisions

- Module path `github.com/andreujuanc/atelier`; cobra for CLI (interspersed flags), BurntSushi/toml, bubbletea v1.
- tmux windows tagged with `@atl_session=<sessions.id>`; liveness checks use the tag (window ids get reused).
- Hook ↔ session mapping via `ATL_SESSION`/`ATL_TASK` env set on the tmux window; fallback by claude session id, then cwd.
- Extra `PostToolUse` hook clears `waiting` → `running` after a permission prompt is answered.
- ro guard fails open on internal errors (spec: never block on errors except the guard itself).
- Worktrees created even for `--backlog` tasks; branches kept on done/cancel; task dir removed only if empty.
- Port offset = lowest free multiple of 100 among open (backlog/active/review) tasks.
- (user) New tasks: `git fetch origin` per rw repo (`--no-fetch` skips); branch from origin/<default>; that sha = base_commit.
- (user) atelier never pushes; `atl pr` skips unpushed branches with "push atl/<slug> first".
- (user) Notification → waiting only for permission prompts; idle notifications leave state.
- (user) doctor warns (not fails) if $ATL_REPOS missing. CLAUDE.md forbids modifying ro repos by any means incl. Bash.
- (M1) modernc.org/sqlite pinned v1.46.1 (last version supporting go 1.24). DSN `_pragma=` + `_txlock=immediate`.
- (M1) Extra API: AllSessionStates, ParseSessionState, Valid() on enums. UniqueSlug keeps ≤40 chars. Relative XDG_* ignored.
- (M1, open) NextPortOffset is not race-safe across concurrent `atl new`; M5 should allocate offset + insert in one store tx (e.g. retry, or CreateTask assigning offset).
- (M2) gitx: exported *gitx.Error (Args, Stderr, ExitCode, Err; Unwrap → ctx errors). GIT_OPTIONAL_LOCKS=0 everywhere; repo-local GIT_* env stripped. Diff returns nil on SIGPIPE/EPIPE.
- (M2→M5) NewTask must RevParse("refs/remotes/origin/"+base_branch) (PLAN updated). IsRepo false on git "dubious ownership" — app should surface rev-parse stderr in its error.
- (M3) Recover implemented in M3 (Resume needs it); M8 keeps startup wiring + e2e.
- (M3) needs tmux ≥ 3.2. Liveness = tagged window with ≥1 non-dead pane. Store gained MarkStoppedIfLive / ClaimStopped / SetSessionLaunch.
- (M3, known) tiny residual race Recover-vs-Resume; remain-on-exit dead windows are not killed on resume (M8 could kill dead tagged windows).
- (M4) hook latency ~5ms. stdin cap 64 MiB; oversized PreToolUse fails closed. Guard: kernel-style path walk (cleaned + uncleaned), ~ expansion, exact-key file_path/notebook_path.
- (M4) ATL_SESSION trusted for SessionStart only if row's claude id empty or source ∈ {clear,resume,compact}; nested `claude` (startup) gets its own row. compact doesn't change state.
- (M4) settings.json merged (user keys/hooks kept; atl* `hook <Event>` entries replaced), atomic write. Known: case-insensitive FS (macOS) not handled by guard.
- (M5) NewTask inserts the row first (CreateTaskAutoPort: atomic slug+port), then worktrees/gen; full rollback incl. branches it created. Existing task dir on disk → error (not a new slug).
- (M5) doctor: Warn = OK&&Warn; tmux < 3.2 FAIL. Project TOML rejects unknown keys. Repo names .claude/CLAUDE.md/./.. rejected.
- (M5) Done/Cancel prune hand-deleted worktrees; tmux errors (other than not installed) abort unless --force.
- (M6) bubbletea v1.3.10 / lipgloss v1.1.0 / bubbles v0.21.0 (newer bubbles bumps go directive). Root `atl` runs TUI only on a TTY. TUI Backend interface + adapter over *app.App; App.Out → status line while running.
- (M6) q with ops in flight waits for them (second ctrl+c forces). D runs `atl diff <slug>` via ExecProcess. Titles are whitespace-collapsed at creation.
- (M7) Scripts: `sh -c` in worktree, own process group (SIGTERM then SIGKILL after 5s on cancel), stdin /dev/null, output streamed to App.Out prefixed. Env: inherited < project env < ATL_*; ATL_SESSION dropped.
- (M7) setup failure = warning; teardown failure aborts done/cancel unless --force. `atl pr` never pushes; exit 1 only if gh failed. gitx.Untracked added.
- (M7, open) flaky TestResumeConcurrent ("resumed 4, want 3") under -race → duplicate window; M3 Resume/Recover race. Assigned to M8.
