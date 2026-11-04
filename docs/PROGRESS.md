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
| M8 crash recovery + e2e | done (reviewed, 1 fix round) |

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
- (M7, fixed in M8) flaky TestResumeConcurrent ("resumed 4, want 3") under -race → duplicate window; M3 Resume/Recover race. Assigned to M8.
- (M8) Race fix: Recover's stale snapshot could stop a session relaunched by a concurrent Resume (window ids reused after server restart) → migration v2 `launch_seq` + MarkStoppedIfLaunch CAS.
- (M8) Startup Recover on every App command except hook/doctor (TUI recovers in Init + every 5s). Open attaches only if a live tagged window exists, else resume → new. Resume launches first, then kills stale same-tag windows.
- (M8) `test/e2e.sh` (+ stubs, Go wrapper test/e2e_test.go) covers new → session → hook states → ro guard → crash/resume → diff → pr (no push) → done/cancel. README.md added.

## All milestones complete. Branch `impl` (not merged to main).

## Follow-ups
| Item | State |
|---|---|
| Dockerfile (atl + claude + tmux + git + gh + ssh; build args USERNAME/UID/GID; tmux conf: no status, Ctrl-q detach) | done |
| Repo picker in TUI + add repo to existing task (`a`, `atl add`, `atl repos`) | done (reviewed, 1 fix round) |
| SessionEnd on container shutdown marks sessions `ended` (should be `stopped`, resumable) | todo |
| atelier assistant — Part A (transcript_path, notes, show, transcript) | done (reviewed, 1 fix round) |
| atelier assistant — Part B (assistant session, `atl assistant`, TUI `A`) | done (reviewed, 1 fix round) |
| done/cancel delete the local atl/<slug> branch when nothing would be lost (no own commits, or all pushed); keep + report otherwise | done (reviewed + safety nits fixed) |
| Base branch per task: auto (origin/dev → develop → default), production (:default), or explicit; migration v4 (old tasks = :default); TUI Base toggle (space/←/→) | done (reviewed, 1 fix round) |
| Task context injected at SessionStart (hookSpecificOutput.additionalContext: task, repos+base, ro warning, port, 5 latest notes) instead of CLAUDE.md | done (reviewed) |
| TUI list: status dot per task (waiting>running>idle>stopped) + `*` for dirty open tasks (App.DirtyTasks, 5s async) | done (reviewed + nits) |
| Dockerfile: UTF-8 + tmux 256/RGB (rendering artifacts) | done |
| `ATL_CONTEXT` env var: per-environment text appended to every task session's SessionStart context (sanitised, ≤1000 bytes, notes dropped first) | done |
| TUI `enter` asks instead of assuming: session picker (New session, live, resumable past sessions with transcript titles); `atl open --session ID` / `--new`; Store.ClaimFrom, tmux SelectWindow; `atl show` session titles | done |
| TUI list rows show the task title only (slug in the details pane; `/` still matches slug + title); `atl rename <task> <title...>` / TUI `t` (title only: slug, branch, tmux, dirs unchanged; model.NormalizeTitle, Store.UpdateTaskTitle, App.Rename rewrites an open task's CLAUDE.md); assistant: `atl rename` in ask + command reference | done |
| One notes text per task, edited in place in the TUI: migration v5 (`tasks.notes`, task_notes folded in as `- YYYY-MM-DD [author] body` lines, table kept); Store GetNotes/SetNotes (CAS)/ReplaceNotes/AppendNote, 64 KiB cap, SanitizeNotes; `atl note` appends, `atl notes` prints, `atl notes --set` replaces from stdin, `atl show --notes`; TUI notes area under the details + `m` textarea editor (ctrl+s CAS save, conflict → ctrl+s overwrite / ctrl+r reload, esc/quit ask if unsaved, bound to its task); SessionStart "Notes:" keeps the end; `atl show --json` notes → string; assistant: all of `atl notes` in ask, reads via `atl show --notes` | done |
| Review fixes (rename + notes): 200-rune title cap shared by `atl new`/`atl rename`/TUI inputs, `t` unchanged submit compares against the prefilled value; rename's CLAUDE.md rewrite never recreates a removed task dir (gen.RewriteClaudeMD, warning); notes editor: unsaved = beyond the save in flight, questions never closed under the user (no key leaks), `m` loading mode swallows keys (esc cancels), help says `y` quits on the quit question; TUI saves turn tabs into 4 spaces / drop U+FFFD (documented + status hint; the textarea sanitiser can't be replaced); `atl notes`: real terminal check for the stdin hint, task resolved before reading stdin, empty `--set` is an error, `--clear` added | done |
| idea: Claude on the right pane of the TUI (tmux join-pane) instead of full-screen attach | idea (user wants it later) |
| optional: skip-fetch toggle in TUI new-task form / SSH ControlMaster in box (YubiKey touch per fetch) | idea |

- (follow-up) NewTaskOpts.ExactRepos for the picker; runSetup takes a repo filter; validRepoArg shared; store.ErrDuplicate; Backend gained ProjectRepos/AvailableRepos/AddRepo.
- (user, 2026-11-04) Assistant: one for all tasks; asks before mutating actions; never edits repos. Design in PLAN §7.
- (follow-up) Part A: migration v3 (sessions.transcript_path, task_notes); `atl show/transcript/note/notes`; TUI `m`. Transcript parser collapses injected entries, caps turns at 3000 runes.
- (follow-up) Branch cleanup: gitx.UniqueCommits (commits not reachable from any other local branch or remote-tracking ref; local refs only). Done/Cancel force-delete when 0, else keep + report.
- (follow-up) Assistant: never types into a live session (paste buffer + hint instead); `--continue` only if a conversation exists; flock-serialised launch; deny rules for common repo mutators. Known: TUI hint barely visible because attach starts immediately (could chain `display-message` into the attach cmd).
- (user) Team branches features from dev/develop, production patches from main → task base selection (PLAN §3 NewTask). PRs target each repo's base_branch.
- (user, 2026-11-04) Open picker: "ask instead of assuming". TUI enter → App.OpenOrChoose: no openable sessions → new session + attach; else picker (New session, live sessions, ended/stopped with claude id + existing transcript; live first, newest activity first). Live → select-window + attach; past → Store.ClaimFrom (CAS on the observed state, launch_seq+1) + `claude --resume`, failed launch restores ended/stopped; others untouched (`r` unchanged). Plain `atl open` unchanged; `--session`/`--new` mutually exclusive.
- (user) Session titles read from the transcript when the picker opens (no DB column): newest `{"type":"custom-title","customTitle"}` (/rename) wins over newest `{"type":"ai-title","aiTitle"}` / legacy `{"type":"summary"}` (formats checked against real transcripts: ai-title is re-appended often, also after a rename), else first user prompt, else window name. Only the first and last 1 MiB of a >2 MiB transcript are scanned. Rows with no recorded transcript path are kept (pre-v3 rows); a recorded but missing file is dropped.
- (user, 2026-11-04) Notes: ONE free-form text per task (the user read "note" in the TUI as a single note), edited in place under the details (`m` → bubbles/textarea; ctrl+s save, esc leave). Migration v5 adds `tasks.notes` and folds the dated task_notes rows into it (oldest first, `- YYYY-MM-DD [author] body`, local date); task_notes is kept, unused. `atl note` now appends such a line atomically (agents/assistant add without clobbering the user's text); saves from the TUI are a compare-and-set against the text the editor started from (conflict → keep the editor, ctrl+s overwrites, ctrl+r reloads). Cap 64 KiB; control chars except \n \t dropped; trailing whitespace trimmed.
- (breaking) `atl show --json`: `notes` is now the notes text, a string (`""` when empty, never null), instead of an array of `{id, author, body, created_at}`. `atl note` prints `note added` (was `note <id> added`); `atl notes` prints the raw text (was dated blocks, newest first).
- Assistant permissions: Claude Code Bash patterns can't safely tell `atl notes X` from `atl notes X --set` (an allow rule `Bash(atl notes *)` also matches `"--set"`, `--set=true`, other spacing/flag orders), so all of `atl notes` is in ask (ask beats allow, also a user's global `Bash(atl *)`), and the read-only spelling `atl show <task> --notes` (covered by the existing `Bash(atl show *)` allow; `atl show` has no writing flag) is what the generated CLAUDE.md tells the assistant to use. The old allow rule is removed on rewrite (it is an atelier-owned rule).
- SessionStart context: "Notes:" section with the notes text, cut from the start when it doesn't fit (end kept, line boundary, marker line); needs ≥40 bytes of room, else left out.
- (review) Notes tabs in the TUI: the bubbles textarea sanitiser (unexported, tabs → 4 spaces, U+FFFD dropped) can't be swapped, and mapping tabs to a placeholder rune would collide with real text and render oddly, so the behaviour is documented instead (README Notes, PLAN §5) and opening notes containing a tab or U+FFFD shows a status hint; `atl notes --set` keeps tabs.
- (review) `atl notes --set` with an empty (after sanitising) stdin is an error; `--clear` clears without reading stdin; `--set --clear` accepts an empty stdin. Breaking for scripts that cleared with `--set < /dev/null`.
