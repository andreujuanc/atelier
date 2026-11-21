# atelier (`atl`)

A terminal task studio for parallel Claude Code work. Each task gets its own git
worktrees (one per read-write repo, on branch `atl/<slug>`); the real Claude Code TUI
runs inside tmux (session `atl-<slug>`, one window per Claude session); atelier
tracks session state in SQLite via Claude Code hooks.

Requires git, tmux ≥ 3.2, `claude`, `gh` (for `atl pr`), and Docker with the task image:
each task's Claude sessions run in the task's own container (the [sandbox](#sandbox), on by
default). Build with `go build -o ~/bin/atl ./cmd/atl`, then run `atl doctor`.

## Docker image

The `Dockerfile` builds an image with `atl`, `claude`, `tmux`, `git` and `gh`, so nothing
needs to be installed on the host. Build it with your host user, so paths inside the
container match the host (git worktrees store absolute paths):

```bash
docker build -t atelier \
  --build-arg USERNAME=$(whoami) --build-arg UID=$(id -u) --build-arg GID=$(id -g) .
```

Task containers (the [sandbox](#sandbox), on by default) use a second image from the same
file, the `task` target, built for your user the same way:

```bash
docker build --target task -t atelier-task \
  --build-arg USERNAME=$(whoami) --build-arg UID=$(id -u) --build-arg GID=$(id -g) .
```

It is lean on purpose: Claude Code, git, the Docker CLI with the compose plugin, Node.js
(the current LTS line, 26, from nodejs.org) with npm and corepack, and curl, jq, ripgrep,
unzip, zip, xz, less, procps, openssh-client and ca-certificates, with a UTF-8 locale
(`C.UTF-8`); not `atl` (it is mounted into task containers), tmux or gh. There is no other
language and no compiler (no Go, Python or C toolchain): those come from the project's own
`image` (see [Projects](#projects); e.g. one built `FROM atelier-task`), or the task
installs them itself, and what is installed by hand lasts only until the task's container
is replaced (see [Sandbox](#sandbox)). As the ordinary user, `npm install -g` installs
into `~/.local`, on `PATH`. `--build-arg NODE_MAJOR=…` picks another Node.js line. With
Docker's legacy builder, building the box builds this stage too (every stage before the
target is built; cached afterwards).

Run it with `docker run -it atelier` (plus your mounts). The container starts a plain
shell, not tmux: run `atl` there, press Enter on a task to open Claude full screen, and
press **Ctrl-q** to go back to the TUI (Claude keeps running in tmux). Don't start `atl`
inside a tmux session in the container: Ctrl-q would then detach that tmux client instead
of returning to the TUI.
Claude's own status line, under its prompt, shows which task you are in (see
[Status line](#status-line)).

## Usage

```sh
atl new "Fix login" --project shop        # task + worktrees, prints "<slug>\t<dir>"
atl new "Spike" --repo api:rw --repo docs:ro -m "why" [--backlog] [--no-fetch]
atl new "Hotfix" --project shop --base default   # branch from origin's default branch (production)
atl add fix-login web [--no-fetch]  # add a repo to an open task (rw: worktree on atl/<slug>)
atl add fix-login docs:ro           # ro: listed in CLAUDE.md; running sessions need a new one (s)
atl add fix-login docs:rw           # upgrade an ro repo to rw (worktree on atl/<slug>); rw → ro is not possible
atl repos                 # git repos in $ATL_REPOS, one per line
atl repos pin api         # pin api's origin anew from its config (accept a changed origin URL)
atl ls [--status active,review|all] [--json]
atl open fix-login        # ensure the tmux session (resume or start Claude), attach
atl open fix-login --session 3   # open that session: select it if live, else claude --resume it; attach
atl open fix-login --new  # start a new Claude session and attach to it
atl session fix-login     # start another Claude session (window) in the task
atl resume fix-login      # after a crash/reboot: relaunch stopped sessions with claude --resume
atl status fix-login review
atl rename fix-login Fix the login page   # title only: slug, branch atl/<slug>, tmux session and dirs keep the old name
atl diff fix-login        # git diff of each rw repo vs its base commit (paged on a TTY)
atl push [fix-login] [--repo api] [--force]  # push atl/<slug> of each rw repo to origin (--force: with lease); task defaults to $ATL_TASK
atl fetch [fix-login] [--repo api]           # git fetch origin in each repo of the task (rw and ro)
atl pr fix-login          # gh pr create (--repo = origin's, --base = the repo's base branch) per rw repo with pushed commits; status → review
atl show fix-login [--json]     # task, repos (dirty, commits ahead, last commit, pushed), sessions, notes
atl show fix-login --notes      # only the notes text (same as atl notes; read-only spelling)
atl transcript fix-login [--session ID] [--tail N]  # last N (20; 0 = all) turns of a Claude session
atl note fix-login waiting on design review [--author user|assistant]  # appends "- YYYY-MM-DD [user] waiting on…"
atl notes fix-login       # print the task's notes text
atl notes fix-login --set < notes.md   # replace the notes with stdin (an empty stdin is an error)
atl notes fix-login --clear            # clear the notes
atl assistant [fix-login] # the atelier assistant (one Claude session for all tasks); attach
atl done fix-login [--force]    # teardown, remove worktrees and fully pushed branches, kill tmux
atl cancel fix-login [--force]
atl doctor                # tools, dirs, database, sandbox mode (Docker, image, token); repos whose config atl refuses to run git with
atl token [--paste]       # sandbox: run claude setup-token and store its token for task sessions (see Sandbox)
atl                       # TUI (on a terminal)
```

TUI keys: `n` new task, `enter` open (see below), `s` new session, `r` resume, `a` add repos to the
selected task, `m` edit the selected task's notes in place (see [Notes](#notes)), `t` rename
the selected task (an input prefilled with its title; `enter` saves, `esc` cancels), `d` done,
`c` cancel, `p` pr, `D` diff, `A` assistant (about the selected task), `1`-`5` status,
`/` filter (matches the slug or the title), `q` quit (while operations are running it
waits for them and then quits; `ctrl+c` again forces it; meanwhile `n`, `t`, `a`, `d`, `c`
and `/` are refused so no input is lost). The task list shows each task's title
(with its session-state dot and `*` for uncommitted changes), truncated to the pane; the slug
is in the details pane, with the task's notes below the details.
Repos (in the new-task form and for `a`) are picked from a list of `$ATL_REPOS`: type to
filter, `↑`/`↓` move, `space` cycles none → rw → ro, `enter` confirms, `esc` cancels.
Repos already in the task are dimmed; `space` on an ro one marks it `[ro→rw]` to upgrade it.
Choosing a project in the form preselects its repos. The form's Base field toggles
(`space`, `←`/`→`; `enter` moves on) between "auto (dev → develop → default)" and
"production patch (default branch)".

`enter` on a task with no openable sessions starts a new Claude session and attaches. If
the task has openable sessions, it asks instead of guessing: a picker lists "New session"
first, then the live sessions (any session whose Claude window is still running, even if
its last hook said it ended, e.g. around `/clear`), then the past ones you can resume
(ended or stopped, with a Claude session id and a transcript that `claude --resume` run in
the task dir can find: it still exists and is in Claude's project dir for the task dir, so
a claude you ran in a subdirectory, or a nested `claude -p`, is not offered; a session
with no recorded transcript is offered only if atl launched it), each group newest
activity first. The most recently active session is highlighted, so `enter` continues it
(`↑` for a new one). Closed (done or cancelled) tasks offer no past sessions. Each row shows
the state, how long ago it was last active, the conversation title, the window name and,
if there is room, the last prompt. The title is Claude Code's own (a `/rename` title, else
the one it generated), else the first prompt, else the window name; it is read from the
transcript when the picker opens (only the first and last MiB of a large one). `↑`/`↓`
move, `enter` picks, `esc` cancels and launches nothing. A live session is attached to
with its window selected (never resumed over); a past one is resumed (`claude --resume
<id>`, only that one: other stopped sessions are left for `r`) and attached to; "New
session" works like `s` and attaches. If an open (or `A`) finishes after you moved on to
something else (the notes editor, a form, the filter, a prompt) or asked to quit, it does
not attach and steal your keys: the status line says "ready — press enter again" (`A`
again for the assistant). Plain `atl open` (no flags) keeps its old behaviour
for scripts: attach if a window is live, else resume the stopped sessions, else start a
new one. `atl open --session` and `--new` are mutually exclusive; `--session` attaches to
a session with a live window like the picker, and refuses another task's session, past
sessions it would not offer (no Claude id, transcript gone or recorded for another
directory, or no transcript and not launched by atl), and resuming in a closed task.

## Notes

Each task has one free-form notes text (goal, decisions, where you left off, ...). The TUI
shows it under the task's details, filling the rest of the details pane (wrapped, from the
top; "(no notes — press m to write)" when empty). `m` turns that area into an editor
prefilled with the text (until the text has loaded, keys are ignored and `esc` cancels);
while it is focused every key goes to the editor (`enter` is a new line, arrows move, the
text scrolls):

- `ctrl+s` saves and leaves the editor. The save only succeeds if the notes are still what
  the editor started from; if something changed them meanwhile (e.g. an agent ran
  `atl note`), the editor stays open with your text and the status line says so: `ctrl+s`
  again overwrites with your text, `ctrl+r` reloads theirs (dropping your edits).
- `esc` leaves the editor; with unsaved changes it asks first ("discard …? y/n"). While a
  save is in flight, only edits made after `ctrl+s` count as unsaved: leaving lets the save
  finish, and "discard" drops only those newer edits. A question stays open until you answer
  it (`y`, or any other key to go back to editing), even if the save finishes meanwhile.
- `ctrl+r` reloads the stored text (dropping your edits); `ctrl+c` with unsaved changes asks
  before quitting (`y` quits).
- The editor can't hold tabs: saving from the TUI turns every tab in the text into 4 spaces
  and drops U+FFFD characters (what invalid UTF-8 was stored as); opening such notes says so
  in the status line. Leaving without changes saves nothing. `atl notes <task> --set` keeps
  tabs.

The editor stays on the task it was opened for, even if the selection or the list changes
meanwhile; like other operations it is refused while the task is busy.

`atl note <task> <text...> [--author user|assistant]` appends a line
`- YYYY-MM-DD [author] text` (today's local date) at the end, so agents and the assistant can
add to the notes without touching what you wrote. `atl notes <task>` prints the whole text,
`atl notes <task> --set` replaces it with stdin, and `atl notes <task> --clear` clears it.
`--set` with an empty stdin (or only blanks) is an error, so an empty pipe by mistake never
wipes the notes; `--set --clear` accepts an empty stdin (and clears). Only `--set` ever
reads stdin, and only after the task is found. Notes are capped at 64 KiB; control characters other than newline and tab
are dropped. Notes work on closed tasks too. Before this, notes were a list of dated entries:
upgrading folds them into the text, oldest first, as `- YYYY-MM-DD [author] body` lines.

`atl rename` (TUI `t`) changes only the title, on a task of any status: the title is trimmed,
space runs are collapsed, and an empty title, one with control characters or one longer
than 200 characters is rejected (`atl new` has the same 200-character cap, and so do the
TUI's title inputs). Submitting the `t` input unchanged saves nothing, also for an older
title over 200 characters, which the input shows cut. The slug never changes, so keep using
it (or the id) to refer to the task. An open task's generated `CLAUDE.md` is rewritten with
the new title; if its task dir is gone (the task is being closed) nothing is recreated and
the rename only warns.

`<task>` is a slug, numeric id, or unique slug prefix. atelier pushes only when asked:
push `atl/<slug>` (yourself, or with `atl push`) before `atl pr`. `atl push` pushes
nothing but the task branch of the task's rw repos, to `origin`. `atl pr` opens each PR
in origin's repository (the pinned origin's, see **Origin pin**): with a fork as `origin`,
the PR is opened in the fork, not upstream. An ssh host alias in origin's URL
(`git@github-work:org/api.git`) counts as github.com when `ssh -G` says its host name is
github.com.

`atl done`/`atl cancel` remove each worktree and then delete its `atl/<slug>` branch
only if no commits on the branch would be lost (the branch's reflog is not kept): every
commit on it must be reachable from another local branch or a remote-tracking ref (local
refs only, no fetch), e.g. after you pushed it or when it has no commits of its own. Otherwise the branch is kept and reported
(`kept branch atl/<slug> in <repo> (N unpushed commit(s))`); `--force` only discards
uncommitted changes, never unpushed commits. They also refuse, even with `--force`, while a
worktree's HEAD has commits no branch, remote-tracking ref or tag reaches (a detached HEAD
or a rebase in progress): put them on a branch first.

`atl show` also prints each session's title (from its transcript; `--json`: `title`,
`""` when none). It never touches the network: "pushed" means the local remote-tracking ref
`origin/atl/<slug>` exists (as of your last push or fetch). `--json` prints a stable
snake_case object (see `internal/cli/info.go`); its `notes` is the notes text as a string
(it used to be an array of dated notes). `atl transcript` reads the Claude Code
transcript JSONL recorded by the hooks and condenses it to user/assistant text; tool
calls become one-line `[tool: Name] …` summaries and tool results are skipped. Notes
work on closed tasks too.

`atl assistant [task]` (TUI `A`) opens the atelier assistant: a single Claude Code session,
in tmux session `atl_assistant`, for managing tasks rather than writing code ("what's the
status of X?", "where did I leave off?", "add repo Y to X"). It runs in
`$XDG_DATA_HOME/atelier/assistant/` with a generated `CLAUDE.md` (role and `atl` command
reference, always rewritten) and `.claude/settings.json` (merged): read-only `atl`
commands (`ls`, `show`, `transcript`, `repos`) and Read/Grep/Glob are allowed,
every state-changing `atl` command asks first (`atl note` appends; `atl notes --set` replaces
the notes, and the assistant is told to show you the new text first; `atl notes --clear`
clears them). All of `atl notes` asks, even plain reads: a permission pattern can't reliably
tell `atl notes X` from `atl notes X --set` (quoting, `--set=true`, flag order), so the assistant reads notes with
`atl show <task> --notes` (or `--json`), which is allowed; common repo mutators (`git commit`, `push`,
`checkout`, `reset`, `rebase`, `merge`, `stash`, `rm`, `mv`, `cp`, `sed -i`, `tee`, ...) are
denied, and the `atl hook AssistantGuard` PreToolUse hook blocks Edit/Write/MultiEdit/NotebookEdit
outside that dir (and on its own `CLAUDE.md`, `CLAUDE.local.md`, `.mcp.json` and `.claude/`, in
any letter case). Bash and MCP
tools cannot be fully sandboxed; the deny rules cover the common cases so that your global
allow rules can't let the assistant change repos. It runs `claude --continue` when Claude Code
has a previous conversation for that dir (a `*.jsonl` under
`${CLAUDE_CONFIG_DIR:-~/.claude}/projects/<dir with non-alphanumerics as ->`), else `claude`,
so the conversation survives restarts. With a task, "Let's talk about task <slug>." is passed
as the initial prompt. Keys are never typed into a running assistant (they could answer a
pending permission prompt): the sentence is put in the tmux paste buffer instead and a hint
(stderr, or the TUI status line) says to press `<prefix> ]` to paste it. Concurrent launches
are serialised by `.launch.lock` in that dir; a session whose `assistant` window is missing or
dead is replaced. Its tmux window is not tracked
by atelier: crash recovery, `done` and `cancel` never touch it.

Every `atl` invocation (except `hook`, `statusline` and `doctor`) first marks sessions whose tmux
window is gone as `stopped`; the TUI also does so every few seconds.

## Base branch

Each rw repo's `atl/<slug>` branch starts from a base branch on origin (after `git fetch
origin`, unless `--no-fetch`), chosen per task with `atl new --base`:

- `auto` (default): `origin/dev` if it exists, else `origin/develop`, else origin's default
  branch (`origin/HEAD`, `main` or `master`). Feature work starts from the integration branch.
- `default` (or `production`): origin's default branch, for production patches.
- any other value: that exact branch (e.g. `main`, `release/1.2`), which must exist as
  `origin/<name>` in every rw repo, else `atl new` fails naming the repo and nothing is created.
  `auto`, `default` and `production` are reserved words: a branch with one of those names
  cannot be chosen.

The choice is stored on the task, so `atl add` (and TUI `a`) resolves the same way for repos
added later (tasks created before this choice existed keep origin's default branch). Each repo records the branch it started from: `atl pr` targets it (`gh pr create
--base <branch>`), `atl diff` compares against the commit it started from, the task's
`CLAUDE.md` says "branched from `origin/<branch>`", `atl show` prints `base origin/<branch>`
per repo (`--json`: task `base` = `""` auto, `":default"` or the branch name; repo
`base_branch`), and the TUI details pane shows `← <branch>` per rw repo.

## Environment

| variable | default |
|---|---|
| `ATL_WORKSPACE` | unset — one folder for everything atelier owns (see below) |
| `ATL_REPOS` | `~/repos` — canonical clones, `$ATL_REPOS/<name>` (never edited) |
| `ATL_TASKS` | `$ATL_WORKSPACE/tasks`, else `~/tasks` — task dirs, `$ATL_TASKS/<slug>/` |
| `XDG_DATA_HOME` | `~/.local/share` — DB at `atelier/atelier.db` (ignored with `ATL_WORKSPACE`) |
| `XDG_STATE_HOME` | `~/.local/state` — hook errors in `atelier/hook.log` (ignored with `ATL_WORKSPACE`) |
| `XDG_CONFIG_HOME` | `~/.config` — projects in `atelier/projects/<name>.toml` (ignored with `ATL_WORKSPACE`) |
| `ATL_TMUX_SOCKET` | unset — tmux socket name (`tmux -L`) if you don't use the default server |
| `ATL_BIN` | the running binary — `atl` path written into hook commands |
| `ATL_CONTEXT` | unset — extra text appended to every task session's start context (max 1000 bytes; atl refuses to start if longer) |
| `ATL_SANDBOX` | unset (on) — each task's sessions run in the task's own container (see [Sandbox](#sandbox)); unset, empty and `docker` mean on, `off` switches it off (going away), anything else is an error |
| `ATL_SANDBOX_IMAGE` | `atelier-task` — the task container image, unless the task's project names one (see [Docker image](#docker-image)) |
| `ATL_SANDBOX_CONTEXT` | unset — what sandboxed sessions get in place of `ATL_CONTEXT` (same limit) |
| `CLAUDE_CODE_OAUTH_TOKEN` | unset — with the sandbox on, the token (`claude setup-token`) that sandboxed sessions log in with; when set it wins over the one `atl token` stored |
| `ANTHROPIC_API_KEY` | unset — with the sandbox on, an API key for sandboxed sessions instead of a token (also wins over the stored one) |

With `ATL_WORKSPACE` set (it must be an existing directory, so a typo or a missing mount
is an error rather than a new empty database), atelier keeps everything in it, and you can
keep several independent workspaces (e.g. one per box):

```
$ATL_WORKSPACE/
  tasks/      task dirs and worktrees (unless ATL_TASKS is set)
  data/       atelier.db, assistant/
  config/     projects/<name>.toml
  state/      hook.log
```

`atl doctor` prints the workspace and every path it resolved.

Claude windows and scripts get `ATL_TASK`, `ATL_TASK_DIR`, `ATL_PORT_OFFSET`
(0, 100, 200… unique per open task) plus the project's `env`; scripts also get
`ATL_REPO`, `ATL_WORKTREE`, `ATL_BRANCH`.

## Sandbox

Each task gets its own Docker container, and its Claude sessions run there, not next to
`atl`, where they could reach everything `atl` can: in the Docker image that is the host's
Docker socket, your SSH agent, every repo and every other task. `atl` stays where it is and
is the only thing that talks to the Docker daemon directly. This is the default; there is
nothing to switch on.

`ATL_SANDBOX=off` (a setting of the workspace, so of the box) switches it off: sessions
run next to `atl` again, as they did before the sandbox. It is a way out while the sandbox
is new, and it is going away, so do not build on it. Unset, empty or `docker` mean on; any
other value is an error, and `atl` does not start. `atl doctor` says which mode is on.

When Docker cannot be reached, starting a session fails, and `atl doctor` fails, with one
line that gives docker's reason and says what to do: start Docker or give `atl` access to
it, or set `ATL_SANDBOX=off`.

```
atl (box)  ── docker exec ──▶  task container atl-<slug>-<hash>   one per task
   │                              claude, your tools, the task dir
   └─ atl gate <slug>  ◀── /run/atl/docker.sock ── docker / docker compose in the task
          │  │         ◀── /run/atl/atl.sock ───── atl hook / statusline / note / notes /
          │  │                                     push / fetch in the task
          │  └──▶ atelier's database, hook log     (this task's rows only)
          ▼
     Docker daemon
```

A task container sees, at the same paths as `atl`: the task dir; the git database
(`.git`) of each rw repo's clone, which the worktree needs and which every task with that
repo rw shares (so such tasks can reach each other, see the first item of what the sandbox
does not do yet); each ro repo, read-only; and the task's own Claude Code config dir (see
below). It does not see the Docker socket, the SSH agent, other repos, other tasks' dirs,
your Claude Code config dir (login, settings,
other sessions' transcripts), nor atelier's database, state dir (hook log, gate logs) or
config dir (projects). It is created on the task's first session, reused by later ones (so what a
session installs is there for the next), restarted after a reboot and removed by `atl
done`/`atl cancel`. When the task's repos change (`atl add`), it is replaced the next time
a session starts while no other session runs in it; `atl add` says so when sessions are
running in it now, since those do not see the new repo (a running container cannot get
another mount): close them, then start or resume a session.

**atl in the container.** The hooks, the status line, `atl note` and `atl notes` work in a
task container without the database: they send their input to the task's gate on
`/run/atl/atl.sock`, and the gate answers them itself, from the database where `atl` runs,
with the same code as outside (same session states, read-only guard, start context and
output). The gate knows its task by database id, set when it starts, and reads and writes
that task's rows only: the session ids, Claude session ids, working directory and
`ATL_TASK` a hook brings can only pick among this task's sessions, a Claude session id
that another task's session has is refused, and the start context and status line are
always this task's. Hook errors go to `hook.log` where `atl` runs, as before. A hook whose
gate cannot be reached fails open with the error on stderr (an oversized `PreToolUse`
input is still refused). For `atl notes` and `atl note` the task must be the container's
own (`$ATL_TASK`); every other `atl` command says in one line that it does not work in a
task container. A gate started by an older `atl` binary, or for a task that was deleted
since, is replaced the next time `atl` starts a session in the task.

The tmux window of a session runs `docker exec` into the container, so everything else
(session states, crash recovery, resume, the picker) works as before. When the window dies
before Claude does (it is closed, or tmux or the box restarts while the task containers
keep running), Docker leaves what it started running in the container; `atl` ends it (every
process there with the session's `ATL_SESSION`, Claude's children included) when it
notices the window gone, which it checks when it starts, and again right before it resumes
the session.

**Claude Code in the container.** Your Claude Code login stays where `atl` runs. Sessions
in a task log in with a token instead:

1. Run `atl token` where `atl` runs (it needs a Pro, Max, Ultra, Team or Enterprise
   plan). It runs `claude setup-token` in a tmux window you are attached to: open the URL
   it shows, approve access in the browser (paste the code back if it asks), and press
   Enter when the token is on the screen. `atl` reads the token from that screen and
   stores it in `<workspace>/run/claude-token.json` (without a workspace,
   `$XDG_STATE_HOME/atelier-run/claude-token.json`), mode 0600, a place no task container
   sees; the window and its scrollback are removed. If `atl` cannot pick out exactly one
   token of the expected shape (`sk-ant-oat01-…`) there, it asks you to paste it, without
   echo. `atl token --paste` stores a token you already have the same way (read from the
   terminal without echo, or from standard input).
2. Or put a token in the environment `atl` runs in as `CLAUDE_CODE_OAUTH_TOKEN`, e.g. in
   `containerEnv` of the devcontainer, or `remoteEnv` from a variable of your host:
   `"CLAUDE_CODE_OAUTH_TOKEN": "${localEnv:CLAUDE_CODE_OAUTH_TOKEN}"`. An API key in
   `ANTHROPIC_API_KEY` works too (Claude Code prefers it when both are set). Either
   variable, when set, wins over the stored token.
3. `atl doctor` says where the token comes from (the environment or the file) and, for a
   stored one, when it was stored and how old it is. The token lasts a year: doctor warns
   from 30 days before that, and fails after it; run `atl token` again then. (The age
   counts from when `atl` stored it: a token pasted with `--paste` may be older.) Without
   any token, starting a sandboxed session fails with one line that names `atl token`.

`atl` hands the token to each session by name: the tmux server gets it from `atl`'s own
environment or the stored file (read on its standard input, not from a command line) and
`docker exec -e CLAUDE_CODE_OAUTH_TOKEN` passes it on. So it is on no command line, not in
the container's stored configuration, and a new token reaches the next session without a
new container. `atl` never prints it. A token-authenticated session can only make model
requests: claude.ai connectors and Remote Control are not available in it.

Each task has its own Claude Code config dir, `<workspace>/run/claude/<hash>` (without a
workspace, `$XDG_STATE_HOME/atelier-run/claude/<hash>`), mounted at the same path in that
task's container only and set as `CLAUDE_CONFIG_DIR` there. It holds what Claude Code
keeps: its state file, the task's transcripts (so `claude --resume` works across a new
container), history. When a new container is created for the task, `atl` refreshes in it
copies of your own setup from your config dir (`$CLAUDE_CONFIG_DIR`, else `~/.claude`),
whichever of these exist: `settings.json` (without `apiKeyHelper` and the other settings
that run a program for credentials, and without the variables of its `env` block whose
name, in any case, contains `KEY`, `TOKEN`, `SECRET`, `PASSWORD`, `PASSWD`, `CREDENTIAL` or
`AUTH`, or starts with `AWS_`), `CLAUDE.md`, `keybindings.json`, `skills/`, `agents/` and
`commands/`. What is copied, the other `env` variables included, the task can read. They
are copies: a task can change its own, never yours, and a change of yours reaches a task
with its next new container. `atl` also sets in the task's
state file (`.claude.json`) that onboarding is done (with your theme), that the task dir is
trusted and, with an API key, that the key is approved, so no first-start screen
shows. Not carried over: your login (`.credentials.json`), your state file (account, your
own MCP servers and their settings, project trust), transcripts and `projects/`, plugins,
other files your settings may point to (hook or status line scripts in `~/.claude`), and
claude.ai connectors. Add MCP servers a task needs to the repo's `.mcp.json` or install
them in the session.

A hook's `transcript_path` comes from the task: the gate records it only if it lies in the
task's own Claude dir (symlinks resolved), and logs and ignores any other. Since the task
can change what is there afterwards, `atl show`, `atl transcript` and the picker read a
transcript only from the task's Claude dir or yours (where sessions with the sandbox off
keep theirs), as described under **Files a task can write** below. `atl done` and
`atl cancel` empty the task's Claude dir except its transcripts (`projects/`, symlinks and
other special files in it removed), which `atl show` and `atl transcript` still read for
closed tasks; delete the dir yourself when you no longer need them. Sessions started with
the sandbox off (before it was the default, or with `ATL_SANDBOX=off`) kept their
transcripts in your own config dir, which the container does not see: they cannot be
resumed in it.

**Files a task can write.** Where `atl` itself reads or writes files the task can write
too (the task dir's `CLAUDE.md` and `.claude/`, the task's Claude dir and transcripts, the
files of a worktree the dirty check hashes), it follows no symlink, neither on the way nor
at the end, reads only regular files and never waits on a pipe: what a task plants there
cannot make `atl` read or write anything else. A
`.claude/settings.json` that is a symlink or not a regular file is replaced without being
read, and a `.claude` that is a symlink makes the session launch fail.

**Tools.** The image of a task's container is the project's `image = "..."` (in its
toml), else `ATL_SANDBOX_IMAGE`, else `atelier-task`, the default task image (see
[Docker image](#docker-image) for how to build it and what it has). When the default image
is not on the Docker daemon, starting a session fails, and `atl doctor` fails, with one
line that gives the command to build it; another image that is missing is left to docker
to pull, and if that fails the launch says so in one line. An image needs `claude` and
`sh`, and should have the user that runs `atl` (same name, uid and home directory, as
atelier's images have), so that what the task writes in the task dir and its Claude dir
stays yours.

The default image has Node.js and no other language (see [Docker image](#docker-image)): a
project that needs Go, Python, a C toolchain or anything else names an image that has it.
Sessions can run whatever the image has and install more: as the image's ordinary user
`npm install -g` and the like; for system packages (a language included) the task may be
root in its own container: `docker exec -u 0 $ATL_CONTAINER apt-get install -y <pkg>`.
But what is installed by hand lives in the container only, and is gone when the task's
container is replaced (after a change of the task's repos or image): what a project keeps
needing belongs in its own `image`, e.g. one built `FROM atelier-task`. The session's start
context says so too.

**Docker.** `docker` and `docker compose` in a task talk to the task's *gate*, not to the
daemon. The gate (`atl gate <slug>`, started by `atl` and stopped when the task closes;
log in `gate-<slug>.log` next to `hook.log`) forwards only what a task may do:

- everything the task creates gets the label `atelier.task`; the task lists, inspects,
  stops and removes only what carries its own. Each container or network a call names (in
  the path or in the request: a network mode, volumes from, endpoint settings, network
  connect) is checked and forwarded by the full ID the check found, so a name that comes
  to mean something else by the time Docker reads it (Docker also takes a unique ID
  prefix for a name) never reaches another object; volumes are named exactly, and a volume
  delete of the task waits while another of its requests that names a volume (a container
  create, an inspect) is between its check and Docker's answer, so Docker never makes the
  container a fresh volume of that name without the label, and a delete never reaches
  another task's volume that took the name in between;
- bind mounts must come from the task dir (symlinks resolved); a mount of
  `/var/run/docker.sock` becomes a mount of the gate's socket, so tools that drive Docker
  themselves (localstack, testcontainers) stay inside the same rules;
- privileged mode, host network/PID/IPC, added capabilities, devices, security options,
  other runtimes, volume driver options and log drivers other than `json-file`, `local` and
  `none` (the others send logs to an address from where the daemon runs) are refused; a
  container setting the gate does not know is refused too;
- images are shared: a task may pull and build, not remove; a build from a remote URL
  (`docker build <git or https URL>`) is refused, since the daemon would fetch it with the
  host's credentials: build from a local context;
- the task's own container is joined to every network the task creates, so services
  answer by their compose service name; the task cannot stop or remove that container.

`atl done`/`atl cancel` remove what the task left on the daemon (containers, networks,
volumes).

What the sandbox does not do yet:

- A task can run code in every other task that has the same repo read-write. Both mount
  that repo's clone (`.git`, with its hooks and config) read-write, so a hook,
  `core.hooksPath`, `core.fsmonitor` or filter one task writes there runs in the other
  task's container the next time git runs there. From there it reaches that task's
  worktree, its Claude dir (transcripts), its token and its gate (`atl push`/`atl fetch`
  of its branch, its notes). Tasks that share an rw repo are not kept apart.
- The task can read Claude Code's token (or API key), which its sessions need to work:
  what runs in the task can use it to make model requests on your account, also from
  elsewhere, until you revoke it.
- The git database of a task's rw repos is writable from the task (other branches, hooks
  and config of that clone included). Only `atl`'s own git, where it runs, is guarded
  against that config (see **Git where atl runs** below), with one limit there; git in a
  task's container is not (see the first item).
- A task can swap a checked mount path for a symlink before the daemon mounts it.
- After the gate is restarted (`atl` upgraded, box restarted), services a task started with
  the Docker socket mounted (localstack, testcontainers) need to be recreated.
- Notes and transcripts a task writes reach the assistant where `atl` runs (`atl notes`,
  `atl transcript`) as prompt input.
- Only the classic image builder goes through the gate (`DOCKER_BUILDKIT=0` is set in the
  task); BuildKit sessions are refused.
- Tasks can bind any free host port, and there is no network egress policy.
- Project setup and teardown scripts still run where `atl` runs.
- `gh` and plain `git push`/`git fetch` have no credentials in a task: pushing and
  fetching go through `atl push`/`atl fetch` (see Git below), and `atl pr` runs where
  `atl` runs.

It needs a token for Claude Code (above), the workspace and repos at the same paths for
`atl` and the Docker daemon, and a workspace path short enough for the gate's socket
(`<workspace>/run/<12 chars>/docker.sock`, at most 107 bytes). `atl doctor` reports the
mode and checks the daemon, the image and the token.

**Session context.** A sandboxed session's start context has a built-in note on the
sandbox (tools run here, what Docker allows, `atl push`/`atl fetch` for git).
`ATL_CONTEXT` is *not* passed to task containers: it describes the place `atl` runs in,
and rules for a shared box ("run everything through docker", "never mount ...")
contradict a task's own container. Put
what sandboxed sessions should know in `ATL_SANDBOX_CONTEXT` instead; `ATL_CONTEXT` still
reaches whatever runs next to `atl` (the assistant, and tasks when the sandbox is off).

**Git.** Commits work in the task container and land on the task branch. Your git config
is not given to the container (it may hold credentials); only `user.name` and
`user.email` are, as `GIT_AUTHOR_*`/`GIT_COMMITTER_*`.

There are no push or fetch credentials in a task (no SSH agent, no `~/.ssh`), so plain
`git push`, `git fetch` and `gh` fail there. Instead a session runs `atl push` (when you
tell it to push) and `atl fetch` (to rebase on fresh remote branches). In the container
these open nothing: they send a request to the task's gate on `/run/atl/atl.sock`, and the
gate runs `atl push` or `atl fetch` for its task where `atl` runs (naming the task by
database id and exact slug, so no other task can be taken for it), with your SSH agent,
`~/.ssh/config` and git credentials, and passes the output and exit status back. The
request can only name the operation, one of the task's repos and `--force`; the task is
always the gate's own. So an agent can push its own branch, and cannot push `main`, tags
or another task's branch through its gate (a task that runs code in another task that
shares its rw repo, see above, uses that task's gate). Requests and refusals are in the gate log; a push may take its
time (up to 10 minutes), e.g. while your SSH key waits for a touch.

A push asked from a task runs without a terminal, so an SSH key that needs a PIN or a
passphrase typed at a prompt fails there (push from your own shell then); a key that only
needs a touch works, though this has not been tried with a hardware key yet.

**Git where atl runs.** `atl` itself runs git in the task's repos where it runs, next to
your SSH agent and the Docker socket: `git status` for the dirty marks and `atl show`/`atl
done`, `git diff` for `atl diff`, `worktree add`/`remove`, branch cleanup, the remote
check of `atl pr`, push and fetch. A repo's config can name programs git then runs
(`core.fsmonitor`, hooks, an ssh command, filters, ...), and that config is the task's to
write. This holds whether the sandbox is on or not (the plan is for it to be the only
mode), and it is done three ways:

- Every git `atl` runs is told not to: `--no-pager`, no fsmonitor, no hooks (the hooks
  directory and every hook configured in git config), no submodule recursion, no
  automatic gc, no editor, no lazy fetch from a partial clone's promisor remote, and no
  transport at all except for push, fetch and the remote check, which use your own
  credential helpers, askpass and ssh command, never the repo's (`ext::` remotes and
  bundle URIs off). `atl diff` runs no external diff or textconv program and shows a
  submodule as its commit only. `gh pr create` (for `atl pr`) runs outside the worktree
  with `--repo` set to the pinned origin's GitHub repository, so it neither reads the
  repo's remotes nor runs git in it.
- Where git would read a file through a filter (a `filter.<name>.clean` program the
  config names), `atl` gets its answer without git doing so: the dirty check compares the
  worktree with the index itself (stat data, and the file's hash where that is not
  conclusive); `atl done` and `worktree remove` use that check instead of git's. A file
  touched without a change reads as clean; one whose content is converted on checkout
  (a filter, `eol`, `text`, `working-tree-encoding`) and whose stat data changed reads as
  dirty, which is the safe answer.
- What cannot be avoided is checked right before, reading the config with the same
  command-line settings as the command, and refused: `atl diff` (it reads changed
  files through the filters) and the checkout of `atl new`/`atl add` refuse while the repo's
  own config (its `.git/config` or a worktree's `config.worktree`) defines a filter
  program; the exact filter commands `git lfs install` writes are let through. With
  git-lfs on, the checkout may download, so it is also refused while the
  repo's own config has git-lfs settings (other than `lfs.repositoryformatversion` and
  `lfs.<url>.access`/`locksverify`), `http.*` settings or URL rewrite rules, or its origin
  is not the pinned one. Push, fetch and the remote check are refused while it has
  `url.<base>.insteadOf`/`pushInsteadOf` rules, or `http.*` settings for an https origin.
  All of these (`atl diff`, the checkout, push, fetch and the remote check) are also
  refused while the repo's own config includes another file (`include.path`, or
  `includeIf.<condition>.path` whatever its condition): what a conditional include brings
  in can depend on what differs between the check and the command (the remote the
  command defines, the branch, the git dir).

The task can also replace its worktree, `<task dir>/<repo>`, with a symlink or point the
worktree's `.git` file into another repo. So before `atl` runs git or a project script in
a worktree (`atl push`, `atl fetch`, `atl pr`, `atl diff`, `atl show`, the dirty marks, the
teardown and worktree removal of `atl done`/`atl cancel`), it checks that the path is a
directory, not a symlink, and a worktree of that repo's canonical clone; if not, the
command fails for that repo naming the path (the dirty marks skip it), and `atl
done`/`atl cancel` refuse, or with `--force` close the task and leave the path alone. Push,
fetch and the remote check of `atl pr` then run in the canonical clone itself, which the
task cannot swap.

A refusal names the repo, the setting and its value, prints a warning, and is logged in
`refused.log` in the state dir (next to `hook.log`, which no task container sees). `atl
doctor` lists, per repo, the settings it would refuse on now. Settings `atl` overrides
anyway (a husky `core.hooksPath`, `core.fsmonitor=true`, your own `core.sshCommand` in a
repo) are not listed. Remove the setting and run the command again.

The limit: a check and git's own reading of the config are two moments, so a task
rewriting its config in a tight loop could slip a setting in between. Also, git cannot
fetch a missing object for `atl` (the promisor remote is the config's to choose), so a
partial clone (`clone --filter`) cannot be an rw repo: the checkout of `atl new` fails.

**Origin pin.** Push and fetch do not go wherever the repo's `origin` points now. `atl`
pins each repo's origin URLs (fetch and push, as the config says them, before any
`insteadOf` rule) in `<workspace>/run/origins/<repo>.json` (without a workspace,
`$XDG_STATE_HOME/atelier-run/origins/`), a place no task container sees, and pushes and
fetches there, through a remote defined on the command line under a random name; your own
`insteadOf`/`pushInsteadOf` rules (in `~/.gitconfig`) apply to it then, once.
`refs/remotes/origin/*`, `refs/remotes/origin/HEAD` (created when missing, as `git
fetch` does since git 2.48) and `--force-with-lease` work as before. `atl new` and `atl
add` fetch the pinned origin the same way before branching. The pin is written once per
repo (clones are shared between tasks), the first time `atl` needs it: at `atl new`/`atl
add`, before the container of a task using the repo starts, or at the first `atl
push`/`atl fetch`, whichever comes first. That is trust on first use: the URL is whatever
the clone said at that moment, and `atl` never rewrites it on its own. When the repo's
config says something else later, `atl` uses the pin and prints a warning naming both; to
accept the new URL, run `atl repos pin <repo>` (where `atl` runs; not from a task).

## Projects

`~/.config/atelier/projects/shop.toml`:

```toml
setup = "npm ci"                          # sh -c in each rw worktree after creation
teardown = "docker compose down -v"       # sh -c in each rw worktree before removal
image = "shop-dev"                        # task container image (see Sandbox)

[env]
COMPOSE_PROJECT_NAME = "shop"

[[repos]]
name = "api"
mode = "rw"     # git worktree on atl/<slug>

[[repos]]
name = "docs"
mode = "ro"     # not a worktree: passed to claude with --add-dir, edits blocked
```

## Hooks

atelier writes `<taskdir>/.claude/settings.json` (merged with any existing keys) with
hooks that run `atl hook <event>` for SessionStart, UserPromptSubmit, Notification,
PostToolUse, Stop, SessionEnd and PreToolUse, and a `statusLine` that runs
`atl statusline` (see [Status line](#status-line)). The file is rewritten before every
Claude launch (new session, resume, `open --session`), so the `atl` path stays current and
existing tasks pick up new entries on their next launch; your own keys and hooks in it are
kept. Your global `~/.claude/settings.json` is never touched (only read, for its
`statusLine`). They keep session state current
(`running`, `waiting` on permission prompts, `idle`, `ended`; `stopped` comes from
crash recovery) and record each session's transcript path, and the PreToolUse hook blocks Edit/Write/MultiEdit/NotebookEdit
on paths inside read-only repos, and inside the original clone (`$ATL_REPOS/<name>`) of the
task's rw repos, whose edits belong in the worktree. Hook failures never block Claude (except the
read-only guard) and are logged to `hook.log` (in a sandboxed task the gate runs them, see
[Sandbox](#sandbox)). The assistant's `AssistantGuard` hook
(see above) fails closed: input it cannot inspect is refused.

On SessionStart (startup, resume, `/clear`, compaction) inside a task, `atl hook
SessionStart` also prints Claude Code's `hookSpecificOutput.additionalContext` JSON with
fresh task context (title, status, branch, repos with their base branch or read-only
warning, `ATL_PORT_OFFSET`, the notes text; at most 2500 bytes), so a session
sees current notes without any file being rewritten. If the notes don't fit, their end is kept
(appended notes are the newest), cut at a line start, after "… (earlier notes truncated;
`atl notes <slug>` for all)". If `ATL_CONTEXT` is set in the hook's
environment (e.g. a devcontainer's `containerEnv`: "no language runtimes here; run tests
inside each repo's container via docker"), its text (newlines kept, up to 1000 bytes) is added
as an "Environment notes" section before the notes. A longer `ATL_CONTEXT` makes `atl` (and
`atl doctor`) fail with its size; the hook itself only cuts it, so a session never loses its context. This is the only hook output on
stdout; if the context cannot be built the error goes to `hook.log` and nothing is printed.

## Status line

A task's Claude session shows the task it belongs to at the bottom of Claude Code's
screen, e.g. `● Fix login`, also when it runs full screen with the tmux status bar hidden.
The dot has the colour the TUI uses for the session's state (waiting, running, idle);
`NO_COLOR` makes it plain, and `COLORFGBG` with a light background picks the TUI's
light-theme colours. Claude Code runs `atl statusline` with the session's JSON on stdin;
atelier finds the task from the Claude session id, else `ATL_TASK`, else the working
directory under `$ATL_TASKS/<slug>/`, and prints the title on one line (control characters
removed, cut at 80 columns). It prints nothing if it can't tell (errors go to `hook.log`).
If a task's `.claude/settings.json` already has a `statusLine` of yours, atelier leaves it
alone; its own entry (`<atl> statusline`) is updated in place. Like hooks, Claude Code runs
it only in trusted workspaces and not with `disableAllHooks`.

Atelier's status line only appears when you have no global status line of your own. A
task's project settings take precedence over your `~/.claude/settings.json` (or
`$CLAUDE_CONFIG_DIR/settings.json`), so if that file defines a `statusLine`, atelier does
not add its entry to task settings, and removes it on the next launch if an earlier one
wrote it; your global status line shows instead. To see atelier's anyway, remove your
global `statusLine`, or call `atl statusline` from your own status line script (pass it
the same JSON on stdin; it prints one line, or nothing). A global file that is missing or
can't be parsed counts as having no `statusLine`. In a sandboxed task your status line
runs in the task's container, from the copy of your settings there (see
[Sandbox](#sandbox)): a script it calls must exist in the container too.

`atl statusline` only reads the database: it never creates or migrates it, and prints
nothing while the database is at another schema version than the binary (the next `atl`
command or hook migrates it). In a sandboxed task it asks the task's gate, which shows
that task only.

## Tests

`go test ./...` runs unit tests and `test/e2e.sh` (skipped with `-short`), which
builds `atl`, creates fake repos and a project in a temp dir and drives
new → session → hooks → crash recovery → resume → open --session → diff → pr → done against an
isolated tmux server, with `claude` and `gh` replaced by `test/stubs`.

## License

MIT, see [LICENSE](LICENSE).
