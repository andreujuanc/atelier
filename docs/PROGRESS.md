# atelier — progress

Resume point for the orchestrator. Spec: `SPEC.md`. Plan: `docs/PLAN.md`.

## Status

| Milestone | State |
|---|---|
| Phase 0 — PLAN.md | done, approved 2026-11-04 with 5 changes (applied) |
| M1 models + store + migrations | done (reviewed, 1 fix round) |
| M2 git worktree management | in progress |
| M3 tmux + session launch/resume/attach | todo |
| M4 hook, state machine, ro guard, CLAUDE.md/settings.json | todo |
| M5 CLI | todo |
| M6 TUI | todo |
| M7 projects, scripts, ports, diff, pr | todo |
| M8 crash recovery + e2e | todo |

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
