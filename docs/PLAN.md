# atelier — implementation plan

Source of truth for requirements: `SPEC.md`. This file fixes the package layout, data model,
inter-package interfaces and CLI surface so milestones can be built independently.

## 0. Basics

- Module: `github.com/andreujuanc/atelier`, Go 1.24, binary `atl` from `./cmd/atl`.
- Deps (only these unless a reviewer approves more):
  - `modernc.org/sqlite` (no cgo)
  - `github.com/spf13/cobra` (interspersed flags: `atl new "title" --project P`)
  - `github.com/BurntSushi/toml`
  - `github.com/charmbracelet/bubbletea` (v1), `github.com/charmbracelet/lipgloss`, `github.com/charmbracelet/bubbles`
- External tools are invoked with `os/exec` (never via a shell, except project setup/teardown scripts which run with `sh -c`).
- Tests use real `git` and real `tmux` on an isolated socket (`ATL_TMUX_SOCKET`); `claude` and `gh` are stub scripts on `PATH`. Tests that need tmux skip if it is not installed.
- All timestamps stored as INTEGER unix milliseconds (UTC); Go side uses `time.Time`.
- `go build ./... && go test ./...` must pass after every milestone.

## 1. Package layout

```
cmd/atl/main.go          fast path: os.Args[1]=="hook" → hook.Main(); else cli.Execute()
internal/model           pure types + enums + validation + Slugify            (M1)
internal/config          Paths (env/XDG resolution)                            (M1)
internal/store           SQLite open, migrations, CRUD                         (M1)
internal/gitx            git fetch / worktree / status / diff wrappers          (M2)
internal/tmux            tmux client wrapper (socket-aware)                     (M3)
internal/app             service layer used by CLI and TUI                      (M3 session.go, M5 task.go, M7 diff.go/pr.go, M8 recover.go)
internal/hook            `atl hook <event>` handler, state machine, ro guard    (M4)
internal/gen             CLAUDE.md + .claude/settings.json generation           (M4)
internal/project         project TOML load (M5), script runner + env (M7)
internal/cli             cobra commands                                         (M5)
internal/tui             Bubble Tea UI                                          (M6)
test/e2e.sh              end-to-end script (+ test/stubs/claude, test/stubs/gh) (M8)
```

Dependency direction (no cycles): `model ← config ← store`; `gitx`, `tmux` depend on nothing internal;
`gen ← model`; `hook ← store, config, model`; `project ← config`; `app ← everything above`;
`tui ← app`; `cli ← app, hook, tui`.

## 2. Data model

### Go types (`internal/model`)

```go
type Status string        // "backlog" | "active" | "review" | "done" | "cancelled"
type Mode string          // "rw" | "ro"
type SessionState string  // "running" | "waiting" | "idle" | "ended" | "stopped"

var AllStatuses = []Status{StatusBacklog, StatusActive, StatusReview, StatusDone, StatusCancelled} // order = TUI keys 1-5
func ParseStatus(string) (Status, error)
func ParseMode(string) (Mode, error)
func (s Status) Open() bool            // backlog|active|review
func (s SessionState) Live() bool      // running|waiting|idle

type Task struct {
    ID          int64
    Slug        string   // unique, [a-z0-9-], ≤40 chars
    Title       string
    Description string
    Status      Status
    Project     string   // "" allowed
    Branch      string   // "atl/" + Slug
    PortOffset  int      // 0,100,200...
    Repos       []TaskRepo
    CreatedAt, UpdatedAt time.Time
}

type TaskRepo struct {
    Name       string // dir name under $ATL_REPOS
    Mode       Mode
    BaseBranch string // origin's default branch at task creation, e.g. "main" ("" for ro)
    BaseCommit string // sha of origin/<BaseBranch> the task branch was created from ("" for ro)
}

type Session struct {
    ID              int64
    TaskID          int64
    ClaudeSessionID string // "" until SessionStart hook fires
    TmuxTarget      string // tmux window id, e.g. "@12" ("" if none)
    Name            string // tmux window name, e.g. "claude-3"
    State           SessionState
    CreatedAt, LastEventAt time.Time
}

func Slugify(title string) string // lowercase, non-alnum runs → "-", trim "-", cut to 40, "" → "task"
```

### SQLite schema (`internal/store`, migration v1; versioning via `PRAGMA user_version`)

```sql
CREATE TABLE tasks (
  id INTEGER PRIMARY KEY,
  slug TEXT NOT NULL UNIQUE,
  title TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('backlog','active','review','done','cancelled')),
  project TEXT NOT NULL DEFAULT '',
  branch TEXT NOT NULL,
  port_offset INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE task_repos (
  task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  mode TEXT NOT NULL CHECK (mode IN ('rw','ro')),
  base_branch TEXT NOT NULL DEFAULT '',
  base_commit TEXT NOT NULL DEFAULT '',
  position INTEGER NOT NULL,
  PRIMARY KEY (task_id, name)
);
CREATE TABLE sessions (
  id INTEGER PRIMARY KEY,
  task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  claude_session_id TEXT NOT NULL DEFAULT '',
  tmux_target TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL CHECK (state IN ('running','waiting','idle','ended','stopped')),
  created_at INTEGER NOT NULL,
  last_event_at INTEGER NOT NULL
);
CREATE INDEX sessions_task ON sessions(task_id);
-- migration v2: ALTER TABLE sessions ADD COLUMN launch_seq INTEGER NOT NULL DEFAULT 0 (bumped by ClaimStopped / SetSessionLaunch)
CREATE INDEX sessions_claude ON sessions(claude_session_id);
```

Connection pragmas: `journal_mode=WAL`, `busy_timeout=3000`, `foreign_keys=ON`, `synchronous=NORMAL`.
Migrations are an ordered `[]string`; `Open` applies those with index ≥ user_version inside a tx.

## 3. Interfaces between packages

### `internal/config`

```go
type Paths struct {
    Repos     string // $ATL_REPOS            default ~/repos
    Tasks     string // $ATL_TASKS            default ~/tasks
    DB        string // $XDG_DATA_HOME/atelier/atelier.db   default ~/.local/share/atelier/atelier.db
    StateDir  string // $XDG_STATE_HOME/atelier             default ~/.local/state/atelier
    ConfigDir string // $XDG_CONFIG_HOME/atelier            default ~/.config/atelier
}
func Resolve() (Paths, error)                 // reads env each call; ~ expanded; paths absolute
func (p Paths) TaskDir(slug string) string    // Tasks/slug
func (p Paths) RepoPath(name string) string   // Repos/name
func (p Paths) WorktreePath(slug, repo string) string // Tasks/slug/repo
func (p Paths) HookLog() string               // StateDir/hook.log
func (p Paths) ProjectFile(name string) string // ConfigDir/projects/<name>.toml
```

Other env vars honoured across packages: `ATL_TMUX_SOCKET` (tmux `-L` name, tests), `ATL_BIN` (path written into
hook commands; default `os.Executable()`), `PAGER` (default `less -R`).

### `internal/store`

```go
var ErrNotFound, ErrAmbiguous error
func Open(path string) (*Store, error)   // mkdir parent, pragmas, migrate
func (s *Store) Close() error
// tasks
func (s *Store) CreateTask(ctx, t *model.Task) error            // inserts task + repos in one tx; sets t.ID, timestamps
func (s *Store) GetTask(ctx, id int64) (*model.Task, error)      // with Repos (ordered by position)
func (s *Store) GetTaskBySlug(ctx, slug string) (*model.Task, error)
func (s *Store) ResolveTask(ctx, ref string) (*model.Task, error) // exact slug, else numeric id, else unique slug prefix
func (s *Store) ListTasks(ctx, statuses ...model.Status) ([]model.Task, error) // none = all; order: status order, updated_at desc
func (s *Store) CreateTaskAutoPort(ctx, t *model.Task) error   // one tx: unique slug + free port + insert (used by NewTask)
func (s *Store) DeleteTask(ctx, id int64) error
func (s *Store) AddTaskRepo(ctx, taskID int64, r model.TaskRepo) error // position = max+1; ErrDuplicate; ErrTaskClosed if task closed
func (s *Store) SetTaskStatus(ctx, id int64, st model.Status) error // bumps updated_at
func (s *Store) UniqueSlug(ctx, base string) (string, error)   // base, base-2, base-3 ...
func (s *Store) NextPortOffset(ctx) (int, error)               // lowest multiple of 100 not used by an Open() task
// sessions
func (s *Store) CreateSession(ctx, sess *model.Session) error  // sets ID, timestamps
func (s *Store) GetSession(ctx, id int64) (*model.Session, error)
func (s *Store) GetSessionByClaudeID(ctx, cid string) (*model.Session, error) // most recent match
func (s *Store) ListSessions(ctx, taskID int64) ([]model.Session, error)      // created_at asc
func (s *Store) ListSessionsByState(ctx, states ...model.SessionState) ([]model.Session, error)
func (s *Store) UpdateSession(ctx, sess *model.Session) error  // writes all mutable fields
func (s *Store) SetSessionState(ctx, id int64, st model.SessionState, at time.Time) error
func (s *Store) ListLaunchedLive(ctx) ([]model.Session, ...)                  // live rows with tmux_target != '' + launch_seq snapshot (Recover)
func (s *Store) MarkStoppedIfLaunch(ctx, id, target, launchSeq, at) (bool, error) // CAS on target + launch_seq (Recover)
func (s *Store) ClaimStopped(ctx, id int64, at time.Time) (bool, error)      // stopped → idle, clears tmux_target (Resume claim) = ClaimFrom(stopped)
func (s *Store) ClaimFrom(ctx, id int64, from SessionState, at time.Time) (bool, error) // from ∈ {stopped, ended}: CAS on the state the caller saw → idle, clears target, launch_seq+1 (OpenSession claim)
func (s *Store) SetSessionLaunch(ctx, id int64, tmuxTarget, name string) error // only those two columns
func (s *Store) SetClaudeSession(ctx, id int64, cid string, at time.Time) error   // claude id + state idle (hook SessionStart)
func (s *Store) SetClaudeSessionID(ctx, id int64, cid string, at time.Time) error // claude id only (SessionStart source=compact)
func (s *Store) SetStateIf(ctx, id int64, from, to model.SessionState, at time.Time) (bool, error)
func (s *Store) TouchSession(ctx, id int64, at time.Time) error
func OpenWithTimeout(path string, busy time.Duration) (*Store, error)        // hook uses 500ms; Open = 3s
func (s *Store) TouchTask(ctx, id int64) error                 // updated_at = now
```

### `internal/gitx` (all take a context; errors include git's stderr)

```go
func IsRepo(path string) bool
func CurrentBranch(ctx, repo string) (string, error)   // "" if detached
func HeadCommit(ctx, repo string) (string, error)
func Fetch(ctx, repo string) error                     // git -C repo fetch origin
func DefaultBranch(ctx, repo string) (string, error)   // origin's default: refs/remotes/origin/HEAD, else origin/main, else origin/master; error if no origin
func RevParse(ctx, repo, rev string) (string, error)   // full sha
func AddWorktree(ctx, repo, path, branch, startPoint string) error // git -C repo worktree add -b branch path startPoint
func RemoveWorktree(ctx, repo, path string, force bool) error // worktree remove [--force], then worktree prune
func IsDirty(ctx, worktree string) (bool, error)       // status --porcelain non-empty (untracked counts)
func CommitsAhead(ctx, worktree, base string) (int, error) // rev-list --count base..HEAD
func Diff(ctx, worktree, base string, color bool, w io.Writer) error // git diff [--color=always] base
func DeleteBranch(ctx, repo, branch string) error      // branch -D (NewTask rollback only)
func RemoteBranchExists(ctx, worktree, branch string) (bool, error) // git ls-remote --heads origin branch (atelier never pushes)
```

### `internal/tmux`

```go
type Client struct{ Socket string }            // Socket=="" → default server; else `tmux -L Socket`
func New() *Client                             // Socket from $ATL_TMUX_SOCKET
func SessionName(slug string) string           // "atl-" + slug
func (c *Client) HasSession(ctx, name string) bool
type WindowSpec struct {
    Session string            // tmux session name; created detached if missing
    Name    string            // window name
    Dir     string            // start dir
    Env     map[string]string // passed with -e
    Argv    []string          // command
    Tag     string            // stored as window user option @atl_session
}
func (c *Client) NewWindow(ctx, spec WindowSpec) (windowID string, err error) // returns "@N"
func (c *Client) TaggedWindows(ctx) (map[string]string, error) // tag → window id, all sessions; no server → empty map, nil
func (c *Client) KillTaggedWindows(ctx, tag, except string) error // exact @atl_session match; Resume kills stale windows after launching
func (c *Client) KillSession(ctx, name string) error           // missing session is not an error
func (c *Client) AttachCmd(name string) *exec.Cmd               // $TMUX set → switch-client -t, else attach-session -t
func (c *Client) SelectWindow(ctx, window string) error         // select-window -t @N (open a chosen session's window)
```

Windows are identified by tag `@atl_session=<sessions.id>` (window ids are reused after a server restart, so
liveness checks use the tag, never the id alone).

### `internal/gen`

```go
type RepoInfo struct { Name string; Mode model.Mode; Path string } // Path = worktree (rw) or canonical (ro)
func WriteClaudeMD(taskDir string, t model.Task, repos []RepoInfo) error // <taskDir>/CLAUDE.md
func WriteSettings(taskDir, atlBin string) error                        // <taskDir>/.claude/settings.json
```

CLAUDE.md contents: "generated by atelier" comment, `# <title>`, description, branch, a repo table/list with
absolute path + rw/ro, an explicit rule that ro repos must not be modified by any means — not with
Edit/Write tools, not via Bash (no redirects, `sed -i`, `git commit`/`checkout`/`stash`, formatters, package
installs, etc.) — and that Edit/Write into them is blocked by a hook; and `ATL_PORT_OFFSET=<n>` with a note
that docker compose files can use `${ATL_PORT_OFFSET}`.

settings.json (atlBin quoted for shell):
```json
{ "hooks": {
  "SessionStart":     [{"hooks":[{"type":"command","command":"<atl> hook SessionStart"}]}],
  "UserPromptSubmit": [{"hooks":[{"type":"command","command":"<atl> hook UserPromptSubmit"}]}],
  "Notification":     [{"hooks":[{"type":"command","command":"<atl> hook Notification"}]}],
  "Stop":             [{"hooks":[{"type":"command","command":"<atl> hook Stop"}]}],
  "SessionEnd":       [{"hooks":[{"type":"command","command":"<atl> hook SessionEnd"}]}],
  "PreToolUse":  [{"matcher":"Edit|Write|MultiEdit|NotebookEdit","hooks":[{"type":"command","command":"<atl> hook PreToolUse"}]}],
  "PostToolUse": [{"hooks":[{"type":"command","command":"<atl> hook PostToolUse"}]}]
}}
```

### `internal/hook`

```go
type Input struct {
    SessionID     string          `json:"session_id"`
    Cwd           string          `json:"cwd"`
    HookEventName string          `json:"hook_event_name"`
    Source        string          `json:"source"`      // SessionStart
    Message       string          `json:"message"`           // Notification
    NotificationType string       `json:"notification_type"` // Notification: permission_prompt | idle_prompt | ...
    ToolName      string          `json:"tool_name"`
    ToolInput     json.RawMessage `json:"tool_input"`  // file_path / notebook_path
}
func Main(args []string) int  // args = events after "hook"; reads os.Stdin, os.Getenv; returns exit code
func Handle(ctx, event string, in Input, env func(string) string, st *store.Store, p config.Paths, now time.Time) (exit int, msg string, err error) // testable core
```

Session resolution order: `ATL_SESSION` env (sessions.id, set by atelier on the tmux window) → row by
`claude_session_id` → task from `ATL_TASK` env or from `cwd` under `Paths.Tasks/<slug>` (create a new row) →
otherwise no-op.

State machine:

| event | effect |
|---|---|
| SessionStart | attach/create row, set `claude_session_id` (overwrite: `/clear` and resume may change it), state=idle |
| UserPromptSubmit | running |
| Notification | waiting **only for permission prompts**: `notification_type == "permission_prompt"`, or (type empty) message contains "permission" (case-insensitive). Idle notifications (`idle_prompt`, "waiting for your input") and others leave state unchanged (still bump `last_event_at`) |
| PostToolUse | running **only if** current state is waiting (clears waiting after a permission prompt is answered) |
| Stop | idle |
| SessionEnd | ended |
| PreToolUse | ro guard only; no state change |

Every event updates `last_event_at`. Unknown events: no-op, exit 0.

ro guard: take `tool_input.file_path` (or `notebook_path`), make absolute against `cwd`, `filepath.Clean`, resolve
symlinks of the longest existing prefix; compare (path-component-wise, also after EvalSymlinks of the ro root)
against `Paths.RepoPath(name)` of every ro repo of the resolved task. Inside → exit 2, stderr
`atelier: <path> is inside read-only repo <name>; edit it in a rw repo instead`. Task unresolvable or any error →
exit 0 (fail open) and log.

SessionStart output: for sessions inside a task, after the DB update, print one JSON line
`{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":...}}` (task, repos + base, ro warning, port offset,
≤5 latest notes, ≤2500 chars). If the hook's environment has a non-blank `ATL_CONTEXT`, its sanitised text (newlines
kept, other control/format chars → spaces, ≤1000 bytes) is added as an "Environment notes (ATL_CONTEXT)" section after the
port offset; notes are dropped before it is cut. This is the ONLY stdout output of `atl hook`.

Performance/robustness: no cobra on this path; one DB open; total budget <50ms. All errors appended to
`Paths.HookLog()` with timestamp + event; exit 0 except the ro block. Never prints to stdout.

### `internal/project`

```go
type RepoSpec struct { Name string `toml:"name"`; Mode model.Mode `toml:"mode"` } // mode default rw
type Project struct {
    Name     string            `toml:"-"`
    Repos    []RepoSpec        `toml:"repos"`
    Setup    string            `toml:"setup"`    // sh -c in each rw worktree after creation
    Teardown string            `toml:"teardown"` // sh -c in each rw worktree before removal
    Env      map[string]string `toml:"env"`
}
func Load(p config.Paths, name string) (*Project, error)
func List(p config.Paths) ([]string, error)
// M7:
type ScriptEnv struct { Task model.Task; TaskDir, Repo, Worktree string }
func (pr *Project) EnvFor(e ScriptEnv) []string // os.Environ + pr.Env + ATL_TASK, ATL_TASK_DIR, ATL_REPO, ATL_WORKTREE, ATL_BRANCH, ATL_PORT_OFFSET
func RunScript(ctx, script, dir string, env []string, out io.Writer) error // no-op if script == ""
```

Example `~/.config/atelier/projects/shop.toml`:
```toml
setup = "make dev-setup"
teardown = "docker compose down -v"
[[repos]]
name = "api"
mode = "rw"
[[repos]]
name = "design-system"
mode = "ro"
[env]
COMPOSE_PROJECT_NAME_PREFIX = "shop"
```

### `internal/app` (service layer; CLI and TUI call only this)

```go
type App struct {
    Paths config.Paths
    Store *store.Store
    Tmux  *tmux.Client
    Out   io.Writer          // progress/warnings
    Now   func() time.Time
    AtlBin string            // for hook commands
}
func New(out io.Writer) (*App, error)  // Resolve paths, open store, tmux.New(), AtlBin from $ATL_BIN or os.Executable
func (a *App) Close() error

type RepoArg struct { Name string; Mode model.Mode }
type NewTaskOpts struct { Title, Description, Project string; Repos []RepoArg; Backlog, NoFetch bool
    ExactRepos bool } // ExactRepos: Repos is the full list (TUI picker); project still used for setup/env
type TaskView struct { Task model.Task; Sessions []model.Session; RepoStatus []RepoStatus }
type RepoStatus struct { Name string; Mode model.Mode; Path string; Dirty bool; Err string }

// M3 session.go
func ClaudeArgv(roPaths []string, resumeID string) []string   // ["claude", ("--resume", id)?, "--add-dir", p...]
func (a *App) NewSession(ctx, ref string) (*model.Session, error) // row (state idle) → tmux window → save target
func (a *App) Resume(ctx, ref string) (int, error)  // Recover, then relaunch every stopped session with a claude id; stopped w/o id → ended
func (a *App) Open(ctx, ref string) (*exec.Cmd, error) // ensure tmux session (resume stopped, else NewSession), return AttachCmd (plain `atl open`)
// open.go: choosing a session (TUI enter, `atl open --session/--new`)
type SessionChoice struct { Session model.Session; Live bool; Window string; Title, LastPrompt string }
func (a *App) OpenChoices(ctx, ref string) ([]SessionChoice, error) // Recover; "openable" sessions: live = a live (not dead) tagged window, whatever the DB state (an ended row keeps one across /clear); then (open tasks only) resumable = ended/stopped, no live window, a claude id, and `claude --resume` in the task dir can find it: recorded transcript exists and sits directly in ${CLAUDE_CONFIG_DIR:-~/.claude}/projects/<task dir, non-[A-Za-z0-9] → '-'> (no path recorded: only if atl launched it, i.e. name set); each group last_event_at desc
func (a *App) OpenOrChoose(ctx, ref string) (*exec.Cmd, []SessionChoice, error) // choices if any (nothing launched), else OpenNew's attach cmd
func (a *App) OpenSession(ctx, ref string, sessionID int64) (*exec.Cmd, error) // live tagged window (any DB state) → select it, never resume over it; past → refuse unless resumable (as OpenChoices), else ClaimFrom + claude --resume (failed launch restores the state); then AttachCmd. resumeClaimed (also Resume) adopts a still-live tagged window instead of launching
func (a *App) OpenNew(ctx, ref string) (*exec.Cmd, error)                      // NewSession, select its window, AttachCmd
// M5 task.go
func (a *App) NewTask(ctx, o NewTaskOpts) (*model.Task, error)
func (a *App) List(ctx, statuses ...model.Status) ([]TaskView, error) // RepoStatus filled (dirty check) only by Get
func (a *App) Get(ctx, ref string) (*TaskView, error)
func (a *App) SetStatus(ctx, ref string, st model.Status) error  // plain status change, no side effects
func (a *App) Done(ctx, ref string, force bool) (*model.Task, error)
func (a *App) Cancel(ctx, ref string, force bool) (*model.Task, error)
func (a *App) RepoPathFor(t *model.Task, repo model.TaskRepo) string // worktree (rw) or canonical (ro)
func (a *App) AvailableRepos(ctx) ([]string, error) // git repos directly under Paths.Repos (symlinks followed)
func (a *App) AddRepo(ctx, ref string, r RepoArg, noFetch bool) (*model.Task, error) // open tasks; per-task flock; rw = fetch + worktree on task branch from origin default; regenerates CLAUDE.md; setup for that repo only
func (a *App) Doctor(ctx) []Check  // type Check struct{ Name string; OK, Warn bool; Detail string }
// M7
func (a *App) Diff(ctx, ref string, w io.Writer, color bool) error
func (a *App) PR(ctx, ref string) ([]PRResult, error) // type PRResult struct{ Repo, URL string; Skipped bool; Reason string }
// M8
func (a *App) Recover(ctx) (int, error) // live sessions whose tagged window is gone → stopped; returns count
```

Tmux window launch details (NewSession/Resume): session `atl-<slug>`, window name `claude-<sessions.id>`,
dir = task dir, tag = sessions.id, env = project env + `ATL_TASK=<slug>`, `ATL_SESSION=<id>`,
`ATL_PORT_OFFSET=<n>`, `ATL_TASK_DIR=<dir>`; argv = `ClaudeArgv(ro canonical paths, resumeID)`. Before launching,
`gen.WriteSettings` is re-run (keeps the atl path current); `CLAUDE.md` is written only if missing.

NewTask flow: validate title; load project (if given) and merge repos (`--repo` overrides mode of a project repo or adds
one); ≥1 repo required; every repo must be `gitx.IsRepo(Paths.RepoPath(name))`; slug = `UniqueSlug(Slugify(title))`
(also must not exist on disk); port = `NextPortOffset`; mkdir task dir; for each rw repo: `gitx.Fetch` (skipped with
`--no-fetch`), `base_branch` from the task's base ("" auto = gitx.IntegrationBranch: origin/dev → develop → default; ":default" = DefaultBranch; else explicit branch), `base_commit = RevParse("refs/remotes/origin/"+base_branch)`,
`AddWorktree(repo, wt, "atl/<slug>", base_commit)` → on any failure roll back created worktrees + dir; write CLAUDE.md + settings.json; `CreateTask`
(status active, or backlog with `--backlog`); then (M7) run project setup in each rw worktree — failure is a warning,
task stays. Worktrees are created even for backlog tasks.

Done/Cancel flow: resolve task; if !force and any rw worktree dirty → error listing dirty repos; kill tmux session;
mark live/stopped sessions ended; (M7) run teardown per rw worktree (failure aborts unless force); RemoveWorktree
(force passes `--force`); remove generated `CLAUDE.md` and `.claude/`; remove task dir only if empty (else warn);
status done/cancelled. Branches: after its worktree is removed, `atl/<slug>` is deleted iff gitx.UniqueCommits == 0 (no commits unique to it vs other local branches/remote-tracking refs), else kept and reported.

Diff: for each rw repo, header `==> <repo> (<branch> vs <base_commit[:8]>)` then `git diff <base_commit>`; CLI pipes
through `$PAGER` when stdout is a TTY.

PR: atelier never pushes. For each rw repo: no commits (`CommitsAhead(base_commit) == 0`) → Skipped "no commits";
`!RemoteBranchExists("atl/<slug>")` → Skipped with reason `push atl/<slug> first` (printed); otherwise in the worktree
`gh pr create --head atl/<slug> --base <base_branch> --title <title> --body <description + "Created by atelier">`;
capture URL from stdout. If ≥1 PR created → status review.

Recover only considers rows with a non-empty tmux_target (rows created by hooks for claude processes atl didn't launch are never stopped). Writes from Recover/launch/hooks use targeted or conditional updates, never read-modify-write of a whole row. atelier assumes one tmux server (`ATL_TMUX_SOCKET` for non-default sockets).

Recover runs at the start of every `atl` invocation except `hook`, and every 5s inside the TUI.

## 4. CLI surface (`internal/cli`, cobra)

Task reference `<task>` = slug, numeric id, or unique slug prefix. Exit codes: 0 ok, 1 error, 2 usage.

| command | behaviour |
|---|---|
| `atl` | TUI |
| `atl new <title...> [--project P] [--repo name:rw\|ro ...] [--backlog] [--no-fetch] [-m/--description D]` | create task; prints slug + task dir |
| `atl ls [--status S[,S...]\|all] [--json]` | default: open statuses. Text columns: `ID SLUG STATUS PROJECT SESSIONS TITLE` (sessions like `2 (1 waiting)`) |
| `atl add <task> <name>[:rw\|ro] [--no-fetch]` | add a repo to an open task; prints `slug\trepo\tmode\tpath` |
| `atl repos` | list repos available under `$ATL_REPOS` |
| `atl open <task> [--session ID \| --new]` | `App.Open` (no flags; unchanged), `App.OpenSession` (`--session`) or `App.OpenNew` (`--new`), then run attach cmd with stdio attached. The flags are mutually exclusive (usage error) |
| `atl session <task>` | new Claude session (does not attach; prints window) |
| `atl resume <task>` | `App.Resume`; prints count |
| `atl status <task> <status>` | `App.SetStatus` |
| `atl diff <task>` | `App.Diff` through pager |
| `atl pr <task>` | prints one line per repo (URL or skipped reason) |
| `atl done <task> [--force]` / `atl cancel <task> [--force]` | |
| `atl hook <event>` | internal; handled before cobra in main.go |
| `atl doctor` | checks: git, tmux, claude, gh on PATH (+version), Tasks dir, DB opens + migrated, hook log dir writable. `$ATL_REPOS` (default ~/repos) missing → **warning** (not a failure). Exit 1 if any check fails (warnings don't) |

`atl ls --json` → array of:
```json
{"id":1,"slug":"fix-login","title":"Fix login","description":"","status":"active","project":"shop",
 "branch":"atl/fix-login","port_offset":100,"task_dir":"/home/u/tasks/fix-login",
 "repos":[{"name":"api","mode":"rw","path":"/home/u/tasks/fix-login/api"}],
 "sessions":[{"id":3,"claude_session_id":"…","state":"waiting","tmux_target":"@4","last_event_at":"RFC3339"}],
 "created_at":"RFC3339","updated_at":"RFC3339"}
```

## 5. TUI (`internal/tui`)

- Left pane: tasks grouped under status headers (backlog, active, review, done, cancelled; done/cancelled collapsed
  to last 10). Task row shows a `●` marker if any session is waiting.
- Right pane: title, slug, project, branch, port offset, task dir; repos (`rw`/`ro`, dirty `*`); sessions
  (`claude-<id>  <state>  <age since last_event_at>`), waiting rendered highlighted (bold + accent colour).
- Repo picker (new-task form and `a`): list of `App.AvailableRepos`, type to filter, space cycles none→rw→ro, enter confirms, esc clears filter then cancels; project repos preselected.
- Keys: `n` new (form: title, project, repos via picker, description, backlog toggle), `enter` open (`App.OpenOrChoose`;
  attach cmd → tea.ExecProcess; choices → session picker), `s` new session, `r` resume, `d` done / `c` cancel (y/N confirm; if refused because dirty, offer `F`
  force), `p` pr, `D` diff (ExecProcess `$PAGER`), `1-5` set status (`AllStatuses` order, plain SetStatus), `a` add repos to the selected task (picker),
  `/` filter (substring on slug/title), `j/k`/arrows move, `q`/ctrl-c quit.
- Session picker (`enter` on a task with openable sessions: live, or resumable as `App.OpenChoices` defines; closed tasks offer
  no past sessions): row 0 "New session" (`App.OpenNew`), then `App.OpenChoices` order; row =
  state, age, title (transcript: newest `custom-title`, else newest `ai-title`/`summary`, else first user prompt, else window
  name; head+tail 1 MiB of a large transcript), window name and last prompt if they fit. ↑/↓ (ctrl+p/n, j/k, pgup/pgdown,
  home/end) move, enter → `App.OpenSession` / `App.OpenNew` then ExecProcess, esc cancels. Same look as the repo picker.
- Ticks: reload from DB every 1s (`App.List`; dirty status via `App.Get` for selected task only, at most every 3s);
  `App.Recover` every 5s. Errors shown in a status line, never crash.
- Model logic (update/selection/grouping/filter) unit-tested without a terminal.

## 6. Milestone ownership

| M | owns | notes |
|---|---|---|
| M1 | `go.mod`, `cmd/atl/main.go` (stub), `internal/model`, `internal/config`, `internal/store`, `.gitignore` | |
| M2 | `internal/gitx` | tests build temp repos |
| M3 | `internal/tmux`, `internal/app/app.go`, `internal/app/session.go`, `internal/app/recover.go` | Recover implemented here (Resume needs it) |
| M4 | `internal/hook`, `internal/gen` | |
| M5 | `internal/cli`, `internal/project/project.go` (Load/List), `internal/app/task.go`, `internal/app/doctor.go`, `cmd/atl/main.go` | |
| M6 | `internal/tui` | |
| M7 | `internal/project/scripts.go`, `internal/app/diff.go`, `internal/app/pr.go`, edits to `task.go`/`session.go` for scripts + env | |
| M8 | recovery startup wiring in cli/tui, `test/e2e.sh`, `test/stubs/*` | |

## 7. atelier assistant (follow-up feature)

One Claude Code session for atelier operations (not repo work): "what's the status of X?", "add repo Y to X".
User decisions: ONE assistant for all tasks; reads freely, ASKS before any mutating action; never edits repos.

Part A — data the assistant reads:
- Migration v3: `sessions.transcript_path TEXT NOT NULL DEFAULT ''`. Hook input gains `transcript_path`; SessionStart
  (all sources) stores it; any other event fills it if the row's value is empty. Targeted updates only.
- Migration v3 also: `task_notes(id INTEGER PK, task_id FK ON DELETE CASCADE, body TEXT, author TEXT CHECK IN ('user','assistant'), created_at INTEGER)`.
- `atl show <task> [--json]`: task fields, task dir, repos (mode, path, dirty, commits ahead of base, last commit sha/subject/date,
  pushed = local ref refs/remotes/origin/atl/<slug> exists — no network), sessions (state, last_event_at, claude id,
  transcript_path, title from the transcript or ""), notes (newest first).
- `atl transcript <task> [--session ID] [--tail N=20]`: condensed last N user/assistant text turns from the transcript JSONL
  (tool calls/results summarised as one line or skipped); default = most recent session with a transcript.
- `atl note <task> <text...> [--author user|assistant]`, `atl notes <task>`. TUI: details pane shows latest note; key `m` adds a note.

Part B — the assistant:
- Dir `$XDG_DATA_HOME/atelier/assistant/` (persists in the box data mount) with generated `CLAUDE.md` (role, atl command
  reference, how to answer status questions: `atl show --json`, `atl transcript`, notes, read-only git in worktrees; always
  confirm before mutating; suggest saving a note after status discussions) and `.claude/settings.json`:
  `permissions.defaultMode = "default"`, allow read-only `atl ls/show/transcript/notes/repos`, ask for
  mutating atl commands (rules in `Bash(atl x *)` form); PreToolUse hook `atl hook AssistantGuard` blocks Edit/Write/MultiEdit/NotebookEdit outside the
  assistant dir. No state-tracking hooks (the assistant is not a task session; untagged window → ignored by Recover).
- tmux session `atl_assistant` (underscore cannot collide with task slugs), window `assistant`, untagged (Recover ignores it).
  Launch `claude --continue [prompt]` only if a previous conversation exists for the assistant dir
  (`$CLAUDE_CONFIG_DIR|~/.claude/projects/<dir, non-alnum→'-'>/*.jsonl`), else `claude [prompt]`. Launch is serialised by a flock;
  a missing/dead assistant window is relaunched.
- `atl assistant [task]` / TUI key `A`: fresh launch → "Let's talk about task <slug>." as the initial prompt. Existing session →
  NEVER type into it (could answer a permission prompt); put the sentence in the tmux paste buffer and show a hint. Then attach.
- Project `permissions.deny` covers common repo mutators (git commit/push/checkout/reset/…, sed -i, rm, mv, cp, tee); Bash/MCP
  cannot be fully sandboxed — the Edit/Write guard + deny rules + CLAUDE.md cover the common cases.
