#!/usr/bin/env bash
# End-to-end test for atelier: builds atl, creates fake repos and a project in
# a temp dir, and exercises new → session → hooks → crash recovery → resume →
# diff → pr → done → notes → cancel against an isolated tmux server. `claude` and `gh`
# are stub scripts (test/stubs). Requires bash, go, git, tmux and jq or
# python3. Run from anywhere: test/e2e.sh. Prints "E2E OK" on success.
set -euo pipefail

ROOT="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
STUBS="$ROOT/test/stubs"

fail() {
	echo "E2E FAIL: $*" >&2
	exit 1
}
step() { echo "--- $*"; }

for tool in git tmux go; do
	command -v "$tool" >/dev/null || fail "$tool not found on PATH"
done
if command -v jq >/dev/null; then
	JSON=jq
elif command -v python3 >/dev/null; then
	JSON=python3
else
	fail "need jq or python3"
fi

W="$(cd "$(mktemp -d "${TMPDIR:-/tmp}/atl-e2e.XXXXXX")" && pwd -P)"
SOCK="atl-e2e-$$"
export TMUX_TMPDIR="$W/t" # keeps the isolated server's socket inside $W
mkdir -p "$TMUX_TMPDIR"

OUTER="$SOCK-outer" # the copy relay's terminal (see "copy relay")
GATE_PID=

cleanup() {
	local rc=$?
	if [[ -n $GATE_PID ]]; then kill "$GATE_PID" 2>/dev/null || true; fi
	tmux -L "$OUTER" kill-server >/dev/null 2>&1 || true
	tmux -L "$SOCK" kill-server >/dev/null 2>&1 || true
	if [[ $rc -ne 0 && -n "${KEEP_E2E:-}" ]]; then
		echo "keeping $W" >&2
	else
		rm -rf "$W"
	fi
	exit $rc
}
trap cleanup EXIT

# Build with the caller's environment (Go caches live under the real $HOME).
step build
mkdir -p "$W/bin"
(cd "$ROOT" && CGO_ENABLED=0 go build -buildmode=exe -o "$W/bin/atl" ./cmd/atl) || fail "go build"

# Isolated environment.
unset TMUX TMUX_PANE CLAUDE_CONFIG_DIR
# git takes config from the environment too (GIT_CONFIG_PARAMETERS and
# GIT_CONFIG_COUNT, e.g. when run under `git -c`), and GIT_DIR & co. point it
# at the caller's repo: only the config files below may count. Likewise for
# atl's own ATL_* (e.g. ATL_SANDBOX_ID when run inside a task container).
for v in $(compgen -e); do
	if [[ $v == GIT_* || $v == ATL_* ]]; then unset "$v"; fi
done
export PATH="$W/bin:$STUBS:$PATH"
export HOME="$W/home"
export ATL_REPOS="$W/repos"
export ATL_TASKS="$W/tasks"
export ATL_WORKSPACES="$W/workspaces"
WS="$ATL_WORKSPACES/e2e" # made by `atl workspace new` below
export ATL_TMUX_SOCKET="$SOCK"
export GIT_CONFIG_GLOBAL="$W/gitconfig"
export GIT_CONFIG_NOSYSTEM=1
export SHELL=/bin/sh
# The sandbox is on by default; this test runs sessions next to atl (the
# stub claude records what it gets there), so it switches the sandbox off.
export ATL_SANDBOX=off
export E2E_OUT="$W/out"
export E2E_LOG="$W/out/claude.log"
export E2E_GH_LOG="$W/out/gh.log"
mkdir -p "$HOME" "$ATL_REPOS" "$ATL_TASKS" "$E2E_OUT" "$W/origin" "$W/seed"
touch "$E2E_LOG"
cat >"$GIT_CONFIG_GLOBAL" <<'EOF'
[user]
	name = E2E
	email = e2e@example.com
[init]
	defaultBranch = main
[advice]
	detachedHead = false
EOF

# The workspace: atelier.toml takes ATL_REPOS from the environment.
out="$(atl workspace new e2e)" || fail "atl workspace new failed: $out"
[[ $out == "e2e	$WS" ]] || fail "atl workspace new output: $out"
grep -qF "repos = \"$ATL_REPOS\"" "$WS/atelier.toml" || fail "atelier.toml lacks repos: $(cat "$WS/atelier.toml")"
export ATL_WORKSPACE="$WS"

# ---- helpers ---------------------------------------------------------------

tm() { tmux -L "$SOCK" "$@"; }

# start_tmux starts the isolated server. After `tm kill-server` the old
# server may still be exiting: a client that reaches it fails with "server
# exited unexpectedly", so it tries again for a few seconds.
start_tmux() {
	local i
	for i in $(seq 50); do
		tm -f /dev/null new-session -d -s placeholder "sleep 86400" 2>/dev/null && return
		sleep 0.1
	done
	tm -f /dev/null new-session -d -s placeholder "sleep 86400" || fail "start tmux server"
}

# ls_json prints `atl ls --status all --json`.
ls_json() { atl ls --status all --json; }

# task_field SLUG FIELD → the task's field.
task_field() {
	if [[ $JSON == jq ]]; then
		ls_json | jq -r --arg s "$1" --arg f "$2" '.[] | select(.slug == $s) | .[$f] // empty'
	else
		ls_json | python3 -c 'import json,sys
for t in json.load(sys.stdin):
    if t["slug"] == sys.argv[1]: print(t.get(sys.argv[2], ""))' "$1" "$2"
	fi
}

# repo_base SLUG REPO → the repo's base_branch from `atl show --json`.
repo_base() {
	if [[ $JSON == jq ]]; then
		atl show "$1" --json | jq -r --arg r "$2" '.repos[] | select(.name == $r) | .base_branch'
	else
		atl show "$1" --json | python3 -c 'import json,sys
for r in json.load(sys.stdin)["repos"]:
    if r["name"] == sys.argv[1]: print(r["base_branch"])' "$2"
	fi
}

# sess_field SLUG FIELD → the field of the task's last session ("" if none).
sess_field() {
	if [[ $JSON == jq ]]; then
		ls_json | jq -r --arg s "$1" --arg f "$2" '.[] | select(.slug == $s) | (.sessions // [])[-1][$f] // empty'
	else
		ls_json | python3 -c 'import json,sys
for t in json.load(sys.stdin):
    if t["slug"] == sys.argv[1] and t.get("sessions"): print(t["sessions"][-1].get(sys.argv[2], ""))' "$1" "$2"
	fi
}

# sess_count SLUG → number of sessions of the task.
sess_count() {
	if [[ $JSON == jq ]]; then
		ls_json | jq -r --arg s "$1" '.[] | select(.slug == $s) | (.sessions // []) | length'
	else
		ls_json | python3 -c 'import json,sys
for t in json.load(sys.stdin):
    if t["slug"] == sys.argv[1]: print(len(t.get("sessions") or []))' "$1"
	fi
}

expect_eq() { # what got want
	[[ "$2" == "$3" ]] || fail "$1: got '$2', want '$3'"
}

expect_state() { # slug state context
	expect_eq "session state ($3)" "$(sess_field "$1" state)" "$2"
}

# wait_for DESC CMD... retries CMD for up to 10s.
wait_for() {
	local desc="$1"
	shift
	local i
	for ((i = 0; i < 100; i++)); do
		if "$@"; then return 0; fi
		sleep 0.1
	done
	fail "timed out waiting for: $desc"
}

# hook EVENT JSON → runs `atl hook EVENT` with JSON on stdin, returns its exit code.
hook() {
	local rc=0
	printf '%s' "$2" | atl hook "$1" || rc=$?
	return $rc
}

# ---- fake repos and project ------------------------------------------------

step "fake repos"
# atl pr hands gh the GitHub repository of the pinned origin, which is the
# literal remote.origin.url, so api's origin is a GitHub URL; the user's own
# (global) rule sends every fetch and push to the local bare repo. docs keeps
# a local path as its origin.
API_URL="https://github.com/example/api.git"
cat >>"$GIT_CONFIG_GLOBAL" <<EOF
[url "$W/origin/api.git"]
	insteadOf = $API_URL
EOF
for r in api docs; do
	git init -q --bare "$W/origin/$r.git"
	git -C "$W/origin/$r.git" symbolic-ref HEAD refs/heads/main
	git init -q "$W/seed/$r"
	echo "# $r" >"$W/seed/$r/README.md"
	git -C "$W/seed/$r" add README.md
	git -C "$W/seed/$r" commit -q -m "init $r"
	git -C "$W/seed/$r" push -q "$W/origin/$r.git" HEAD:main
	url="$W/origin/$r.git"
	if [[ $r == api ]]; then url="$API_URL"; fi
	git clone -q "$url" "$ATL_REPOS/$r"
done
expect_eq "api origin URL" "$(git -C "$ATL_REPOS/api" config remote.origin.url)" "$API_URL"

# Make the canonical api clone stale and off its default branch: origin's main
# gains a commit the clone has not fetched, and the clone sits on a local-only
# branch with a local-only commit. Tasks must still branch from origin's main
# (production patches) or, by default, from origin's integration branch dev,
# which is one commit ahead of main and also not fetched yet. docs has only
# main: auto falls back to it.
git clone -q "$W/origin/api.git" "$W/seed/api-upstream"
echo "upstream change" >"$W/seed/api-upstream/upstream.txt"
git -C "$W/seed/api-upstream" add upstream.txt
git -C "$W/seed/api-upstream" commit -q -m "upstream change"
git -C "$W/seed/api-upstream" push -q origin HEAD:main
echo "dev work" >"$W/seed/api-upstream/dev.txt"
git -C "$W/seed/api-upstream" add dev.txt
git -C "$W/seed/api-upstream" commit -q -m "dev work"
git -C "$W/seed/api-upstream" push -q origin HEAD:dev
ORIGIN_DEV="$(git -C "$W/origin/api.git" rev-parse dev)"
git -C "$ATL_REPOS/api" checkout -q -b local-work
echo "local only" >"$ATL_REPOS/api/local.txt"
git -C "$ATL_REPOS/api" add local.txt
git -C "$ATL_REPOS/api" commit -q -m "local only"
ORIGIN_MAIN="$(git -C "$W/origin/api.git" rev-parse main)"
CANON_HEAD="$(git -C "$ATL_REPOS/api" rev-parse HEAD)"
[[ "$(git -C "$ATL_REPOS/api" rev-parse main)" != "$ORIGIN_MAIN" ]] || fail "canonical api main is not stale"

mkdir -p "$WS/config/projects"
cat >"$WS/config/projects/shop.toml" <<'EOF'
setup = 'printf "%s\n" "$ATL_PORT_OFFSET" > "$E2E_OUT/setup-$ATL_TASK"'
teardown = 'touch "$E2E_OUT/teardown-$ATL_TASK"'

[env]
FOO = "bar"

[[repos]]
name = "api"
mode = "rw"

[[repos]]
name = "docs"
mode = "ro"
EOF

start_tmux

# ---- doctor ----------------------------------------------------------------

step doctor
out="$(atl doctor 2>&1)" || fail "atl doctor failed:
$out"
if grep -q FAIL <<<"$out"; then fail "doctor reported FAIL:
$out"; fi
grep -qE "^ok +workspace +e2e: $WS \(from [$]ATL_WORKSPACE\)$" <<<"$out" || fail "doctor lacks the workspace:
$out"
grep -qE "^ok +repos +[$]ATL_REPOS = $ATL_REPOS \(env\)$" <<<"$out" || fail "doctor lacks repos from env:
$out"
out="$(atl workspace list)" || fail "atl workspace list failed: $out"
grep -qE "^\* e2e +$WS$" <<<"$out" || fail "atl workspace list: $out"

# ---- new -------------------------------------------------------------------

step "new task"
T="$ATL_TASKS/fix-login"
out="$(atl new "Fix login" --project shop)" || fail "atl new failed: $out"
expect_eq "atl new output" "$out" "fix-login	$T"
[[ "$(git -C "$T/api" rev-parse --abbrev-ref HEAD)" == "atl/fix-login" ]] || fail "api worktree not on atl/fix-login"
expect_eq "api worktree base (origin dev)" "$(git -C "$T/api" rev-parse HEAD)" "$ORIGIN_DEV"
expect_eq "api base_branch" "$(repo_base fix-login api)" "dev"
grep -qF 'branched from `origin/dev`' "$T/CLAUDE.md" || fail "CLAUDE.md does not name the base branch:
$(cat "$T/CLAUDE.md")"
expect_eq "canonical api branch after new" "$(git -C "$ATL_REPOS/api" rev-parse --abbrev-ref HEAD)" "local-work"
expect_eq "canonical api HEAD after new" "$(git -C "$ATL_REPOS/api" rev-parse HEAD)" "$CANON_HEAD"
git -C "$ATL_REPOS/api" worktree list --porcelain | grep -qx "worktree $T/api" || fail "api is not a registered worktree"
[[ ! -e "$T/docs" ]] || fail "docs (ro) must not be a worktree in the task dir"
grep -q "docs.*$ATL_REPOS/docs.*READ-ONLY" "$T/CLAUDE.md" || fail "CLAUDE.md does not list docs as read-only:
$(cat "$T/CLAUDE.md")"
grep -q "ATL_PORT_OFFSET=0" "$T/CLAUDE.md" || fail "CLAUDE.md lacks ATL_PORT_OFFSET=0"
grep -q "$W/bin/atl.*hook SessionStart" "$T/.claude/settings.json" || fail "settings.json hooks do not point at $W/bin/atl:
$(cat "$T/.claude/settings.json")"
grep -q "hook PreToolUse" "$T/.claude/settings.json" || fail "settings.json lacks the PreToolUse hook"
grep -qF "\"command\": \"'$W/bin/atl' statusline\"" "$T/.claude/settings.json" || fail "settings.json lacks the statusLine:
$(cat "$T/.claude/settings.json")"
expect_eq "setup port offset" "$(cat "$E2E_OUT/setup-fix-login")" "0"
expect_eq "task status" "$(task_field fix-login status)" "active"

# docs is rw here (overriding the project's ro): it has no dev/develop on
# origin, so auto falls back to its default branch.
out="$(atl new "Second task" --project shop --repo docs:rw)" || fail "atl new (second) failed: $out"
expect_eq "second port offset" "$(task_field second-task port_offset)" "100"
expect_eq "second setup port offset" "$(cat "$E2E_OUT/setup-second-task")" "100"
expect_eq "second api base_branch" "$(repo_base second-task api)" "dev"
expect_eq "docs base_branch (fallback)" "$(repo_base second-task docs)" "main"
expect_eq "docs worktree base (origin main)" "$(git -C "$ATL_TASKS/second-task/docs" rev-parse HEAD)" \
	"$(git -C "$W/origin/docs.git" rev-parse main)"

# A production patch branches from origin's default branch.
out="$(atl new "Hotfix" --repo api --base production)" || fail "atl new --base production failed: $out"
expect_eq "hotfix api base_branch" "$(repo_base hotfix api)" "main"
expect_eq "hotfix worktree base (origin main)" "$(git -C "$ATL_TASKS/hotfix/api" rev-parse HEAD)" "$ORIGIN_MAIN"
out="$(atl cancel hotfix 2>&1)" || fail "atl cancel hotfix failed: $out"

# ---- session + hooks -------------------------------------------------------

step "session"
out="$(atl session fix-login)" || fail "atl session failed: $out"
tm has-session -t "=atl-fix-login" 2>/dev/null || fail "tmux session atl-fix-login missing"
has_cid() { [[ -n "$(sess_field fix-login claude_session_id)" ]]; }
wait_for "SessionStart hook from stub claude" has_cid
CID="$(sess_field fix-login claude_session_id)"
expect_state fix-login idle "after SessionStart"
line="$(grep "sid=$CID" "$E2E_LOG" | head -n1)"
[[ "$line" == *"task=fix-login "* && "$line" == *" port=0 "* && "$line" == *" foo=bar "* && "$line" == *" cwd=$T "* ]] ||
	fail "stub claude env/cwd wrong: $line"
[[ "$line" == *"argv=[--add-dir][$ATL_REPOS/docs]" ]] || fail "stub claude argv wrong: $line"

ev() { printf '{"session_id":"%s","cwd":"%s","hook_event_name":"%s"%s}' "$CID" "$T" "$1" "${2:-}"; }
step "hook states"
hook UserPromptSubmit "$(ev UserPromptSubmit)" || fail "hook UserPromptSubmit"
expect_state fix-login running UserPromptSubmit
hook Notification "$(ev Notification ',"notification_type":"permission_prompt","message":"Claude needs your permission to use Bash"')" || fail "hook Notification"
expect_state fix-login waiting "Notification permission_prompt"
hook PostToolUse "$(ev PostToolUse ',"tool_name":"Bash","tool_input":{"command":"ls"}')" || fail "hook PostToolUse"
expect_state fix-login running PostToolUse
hook Stop "$(ev Stop)" || fail "hook Stop"
expect_state fix-login idle Stop
hook Notification "$(ev Notification ',"notification_type":"idle_prompt","message":"Claude is waiting for your input"')" || fail "hook Notification idle"
expect_state fix-login idle "Notification idle_prompt after Stop"

step "ro guard"
rc=0
hook PreToolUse "$(ev PreToolUse ",\"tool_name\":\"Edit\",\"tool_input\":{\"file_path\":\"$ATL_REPOS/docs/README.md\"}")" 2>/dev/null || rc=$?
expect_eq "PreToolUse on ro repo exit code" "$rc" "2"
rc=0
hook PreToolUse "$(ev PreToolUse ",\"tool_name\":\"Write\",\"tool_input\":{\"file_path\":\"$T/api/new.txt\"}")" || rc=$?
expect_eq "PreToolUse on rw worktree exit code" "$rc" "0"

step "status line"
out="$(printf '{"session_id":"%s","cwd":"%s","workspace":{"current_dir":"%s"}}' "$CID" "$T/api" "$T/api" | NO_COLOR=1 atl statusline)" ||
	fail "atl statusline failed"
expect_eq "status line by session id" "$out" "● Fix login"
out="$(printf '{"session_id":"nope","cwd":"%s"}' "$ATL_TASKS/second-task" | NO_COLOR=1 atl statusline)" || fail "atl statusline (cwd) failed"
expect_eq "status line by cwd" "$out" "● Second task"
out="$(printf 'not json' | ATL_TASK=fix-login atl statusline 2>&1)" || fail "atl statusline (bad input) exit code"
expect_eq "status line on bad input" "$out" ""

# ---- copy relay ------------------------------------------------------------

# In a task container wl-copy is atl, linked as wl-copy next to it; it asks
# the task's gate, which puts the text in the clipboard of the terminal on
# the task's tmux session (load-buffer -w: OSC 52). The gate runs here as atl
# starts it, without Docker (only its Docker socket would need the daemon);
# the terminal is a pane of a second tmux server that takes OSC 52
# (set-clipboard on), and atl's server takes any terminal for one that does.
step "copy relay"
mkdir -p "$W/relay"
for tool in wl-copy wl-paste; do ln -s "$W/bin/atl" "$W/relay/$tool"; done
# Reading the clipboard is refused, and prints nothing.
out="$(printf 'x' | ATL_SANDBOX_ID=e2e "$W/relay/wl-paste" 2>/dev/null)" && fail "wl-paste succeeded"
expect_eq "wl-paste output" "$out" ""
atl gate fix-login --task-id "$(task_field fix-login id)" >"$E2E_OUT/gate.log" 2>&1 &
GATE_PID=$!
ctl_sock() { find "$WS/run" -name atl.sock -type s 2>/dev/null | head -n1; }
gate_up() { [[ -n "$(ctl_sock)" ]]; }
wait_for "the gate's control socket" gate_up
CTL="$(ctl_sock)"
relay() { ATL_SANDBOX_ID=e2e ATL_CONTROL_SOCK="$CTL" "$W/relay/wl-copy" "$@"; }
TEXT='echo "copied from a task" \
  --flag=x'
rc=0
printf '%s' "$TEXT" | relay 2>"$E2E_OUT/wl-copy.err" || rc=$?
expect_eq "wl-copy without a terminal on the session: exit code" "$rc" "1"
grep -q "no terminal is attached" "$E2E_OUT/wl-copy.err" || fail "wl-copy without a terminal: $(cat "$E2E_OUT/wl-copy.err")"
tm set-option -as terminal-features ',*:clipboard'
tmux -L "$OUTER" -f /dev/null new-session -d -s term "env -u TMUX tmux -L $SOCK attach-session -t =atl-fix-login" \; \
	set-option -g set-clipboard on || fail "start the terminal's tmux server"
attached() { [[ -n "$(tm list-clients -t =atl-fix-login -F '#{client_name}')" ]]; }
wait_for "a terminal on atl-fix-login" attached
before="$(tm list-buffers | wc -l)"
printf '%s' "$TEXT" | relay || fail "wl-copy failed"
printf '%s' "$TEXT" | relay --primary || fail "wl-copy --primary failed"
expect_eq "tmux paste buffer" "$(tm show-buffer)" "$TEXT"
expect_eq "paste buffers added (the primary selection is not copied again)" "$(($(tm list-buffers | wc -l) - before))" "1"
clipboard() { [[ "$(tmux -L "$OUTER" show-buffer 2>/dev/null)" == "$TEXT" ]]; }
wait_for "the copy in the terminal's clipboard" clipboard
grep -qF "copy  ${#TEXT} bytes for fix-login to the clipboard" "$E2E_OUT/gate.log" || fail "gate log lacks the copy: $(cat "$E2E_OUT/gate.log")"
if grep -qF "copied from a task" "$E2E_OUT/gate.log"; then fail "the gate logged the copied text"; fi
kill "$GATE_PID"
wait "$GATE_PID" 2>/dev/null || true
GATE_PID=
tmux -L "$OUTER" kill-server
rm -rf "$(dirname "$CTL")" # as before the gate: no run dir, nothing for atl done to clean up

# ---- crash recovery + resume -----------------------------------------------

step "crash recovery"
tm kill-server
start_tmux
expect_state fix-login stopped "after tmux server crash (startup recover)"

step resume
out="$(atl resume fix-login)" || fail "atl resume failed: $out"
expect_eq "atl resume count" "$out" "1"
resumed() { grep -qF "sid=$CID argv=[--resume=$CID][--add-dir][$ATL_REPOS/docs]" "$E2E_LOG"; }
wait_for "stub claude relaunched with --resume $CID" resumed
expect_state fix-login idle "after resume"
expect_eq "claude session id after resume" "$(sess_field fix-login claude_session_id)" "$CID"
expect_eq "session count after resume" "$(sess_count fix-login)" "1"
tm has-session -t "=atl-fix-login" 2>/dev/null || fail "tmux session atl-fix-login not recreated"
[[ -n "$(sess_field fix-login tmux_target)" ]] || fail "resumed session has no tmux target"

step "open --session (resume an ended session)"
rc=0
atl open fix-login --session 1 --new >/dev/null 2>&1 || rc=$?
expect_eq "open --session with --new exit code" "$rc" "2"
SID="$(sess_field fix-login id)"
hook SessionEnd "$(ev SessionEnd)" || fail "hook SessionEnd"
expect_state fix-login ended "after the user exits claude"
tm kill-window -t "$(sess_field fix-login tmux_target)"
# Attaching needs a terminal, so only the resume before it can succeed here.
out="$(atl open fix-login --session "$SID" </dev/null 2>&1)" && fail "atl open attached without a terminal: $out"
[[ "$out" == *"attach:"* ]] || fail "atl open --session failed before attaching: $out"
resumed_twice() { [[ "$(grep -cF "sid=$CID argv=[--resume=$CID][--add-dir][$ATL_REPOS/docs]" "$E2E_LOG")" == 2 ]]; }
wait_for "stub claude relaunched again with --resume $CID" resumed_twice
expect_state fix-login idle "after open --session"
expect_eq "session count after open --session" "$(sess_count fix-login)" "1"

# ---- diff ------------------------------------------------------------------

step diff
echo "login fixed" >"$T/api/login.txt"
git -C "$T/api" add login.txt
git -C "$T/api" commit -q -m "fix login"
out="$(atl diff fix-login)" || fail "atl diff failed: $out"
[[ "$out" == *"==> api (atl/fix-login vs "* ]] || fail "atl diff header missing:
$out"
[[ "$out" == *"+login fixed"* ]] || fail "atl diff lacks the change:
$out"

# ---- pr --------------------------------------------------------------------

step "pr (unpushed)"
out="$(atl pr fix-login)" || fail "atl pr (unpushed) failed: $out"
[[ "$out" == *"skipped: push atl/fix-login first"* ]] || fail "atl pr did not skip the unpushed branch: $out"
[[ ! -s "$E2E_GH_LOG" ]] || fail "gh was called for an unpushed branch: $(cat "$E2E_GH_LOG")"
expect_eq "status after skipped pr" "$(task_field fix-login status)" "active"

step "pr (pushed)"
heads="$(git -C "$T/api" ls-remote --heads origin atl/fix-login)" || fail "git ls-remote failed"
expect_eq "remote atl/fix-login before manual push (atelier never pushes)" "$heads" ""
[[ ! -s "$E2E_GH_LOG" ]] || fail "gh was called before the manual push: $(cat "$E2E_GH_LOG")"
git -C "$T/api" push -q origin atl/fix-login
out="$(atl pr fix-login)" || fail "atl pr failed: $out"
[[ "$out" == *"https://github.com/example/api/pull/1"* ]] || fail "atl pr output: $out"
expect_eq "gh calls" "$(grep -c '^gh ' "$E2E_GH_LOG")" "1"
grep -qF "[pr][create][--repo][example/api][--head][atl/fix-login][--base][dev]" "$E2E_GH_LOG" || fail "gh not called as expected: $(cat "$E2E_GH_LOG")"
# gh runs in an empty directory of its own (in the worktree it would read the
# task's remotes and run git there), removed afterwards.
gh_cwd="$(sed -n 's/^gh cwd=\(.*\) entries=[0-9]* argv=.*/\1/p' "$E2E_GH_LOG")"
[[ -n "$gh_cwd" && "$gh_cwd" != "$W" && "$gh_cwd" != "$W/"* ]] || fail "gh run in the test's tree: $(cat "$E2E_GH_LOG")"
grep -qF "cwd=$gh_cwd entries=0 " "$E2E_GH_LOG" || fail "gh's directory was not empty: $(cat "$E2E_GH_LOG")"
[[ ! -e "$gh_cwd" ]] || fail "gh's directory $gh_cwd was left behind"
expect_eq "status after pr" "$(task_field fix-login status)" "review"

step "pr (non-GitHub origin)"
# docs's origin is a local path: gh could not be told its repository.
D="$ATL_TASKS/second-task/docs"
echo "docs change" >>"$D/README.md"
git -C "$D" commit -q -am "docs change"
git -C "$D" push -q origin atl/second-task
out="$(atl pr second-task)" || fail "atl pr second-task failed: $out"
[[ "$out" == *"docs	skipped: origin $W/origin/docs.git is not a GitHub repository URL"* ]] ||
	fail "atl pr did not skip docs with its local origin: $out"
expect_eq "gh calls after skipped pr" "$(grep -c '^gh ' "$E2E_GH_LOG")" "1"
expect_eq "second-task status after skipped pr" "$(task_field second-task status)" "active"

# ---- done ------------------------------------------------------------------

step done
echo "dirty" >>"$T/api/login.txt"
if out="$(atl done fix-login 2>&1)"; then fail "atl done succeeded on a dirty worktree: $out"; fi
[[ "$out" == *"uncommitted changes"* ]] || fail "atl done refusal message: $out"
expect_eq "status after refused done" "$(task_field fix-login status)" "review"
[[ -d "$T/api" ]] || fail "refused done removed the worktree"

out="$(atl done fix-login --force 2>&1)" || fail "atl done --force failed: $out"
[[ -e "$E2E_OUT/teardown-fix-login" ]] || fail "teardown did not run"
[[ ! -e "$T/api" ]] || fail "worktree still exists"
if git -C "$ATL_REPOS/api" worktree list --porcelain | grep -qx "worktree $T/api"; then
	fail "api worktree still registered: $(git -C "$ATL_REPOS/api" worktree list)"
fi
# Its commit was pushed (origin/atl/fix-login), so the branch is deleted.
if git -C "$ATL_REPOS/api" rev-parse -q --verify refs/heads/atl/fix-login >/dev/null; then
	fail "pushed branch atl/fix-login was kept"
fi
[[ "$out" == *"deleted branch atl/fix-login in api"* ]] || fail "atl done branch message: $out"
if tm has-session -t "=atl-fix-login" 2>/dev/null; then fail "tmux session atl-fix-login still exists"; fi
expect_eq "status after done" "$(task_field fix-login status)" "done"

# ---- notes -----------------------------------------------------------------

step "notes (closed task)"
printf 'Goal: fix login\n  - shipped\n' | atl notes fix-login --set >/dev/null || fail "atl notes --set failed"
atl note fix-login --author assistant "post-mortem done" >/dev/null || fail "atl note failed"
out="$(atl notes fix-login)" || fail "atl notes failed"
expect_eq "notes" "$out" "Goal: fix login
  - shipped
- $(date +%F) [assistant] post-mortem done"
expect_eq "show --notes" "$(atl show fix-login --notes)" "$out"
# an empty stdin with --set is refused (no hint on stderr: /dev/null is no terminal)
if err="$(atl notes fix-login --set </dev/null 2>&1 >/dev/null)"; then fail "atl notes --set with empty stdin succeeded"; fi
case "$err" in *"use --clear"*) ;; *) fail "atl notes --set empty stdin: unexpected error: $err" ;; esac
case "$err" in *"end with ctrl-d"*) fail "stdin hint printed for /dev/null: $err" ;; esac
expect_eq "notes after refused --set" "$(atl notes fix-login)" "$out"
# a bad ref fails before stdin is read (a fifo with no writer would block)
mkfifo "$E2E_OUT/nowriter"
rc=0
timeout 10 atl notes nope --set <>"$E2E_OUT/nowriter" >/dev/null 2>&1 || rc=$?
[ "$rc" -ne 124 ] || fail "atl notes --set read stdin before resolving the task"
[ "$rc" -ne 0 ] || fail "atl notes nope --set succeeded"
atl notes fix-login --clear </dev/null >/dev/null || fail "atl notes --clear failed"
expect_eq "notes after clear" "$(atl notes fix-login)" ""

# ---- cancel ----------------------------------------------------------------

step cancel
echo "wip" >"$ATL_TASKS/second-task/api/wip.txt"
git -C "$ATL_TASKS/second-task/api" add wip.txt
git -C "$ATL_TASKS/second-task/api" commit -q -m "unpushed work"
out="$(atl cancel second-task 2>&1)" || fail "atl cancel failed: $out"
[[ "$out" == *"kept branch atl/second-task in api (1 unpushed commit(s))"* ]] || fail "atl cancel branch message: $out"
git -C "$ATL_REPOS/api" rev-parse -q --verify refs/heads/atl/second-task >/dev/null || fail "unpushed branch atl/second-task was deleted"
expect_eq "status after cancel" "$(task_field second-task status)" "cancelled"
[[ -e "$E2E_OUT/teardown-second-task" ]] || fail "teardown did not run for second-task"
[[ ! -e "$ATL_TASKS/second-task/api" ]] || fail "second-task worktree still exists"
expect_eq "worktrees of api" "$(git -C "$ATL_REPOS/api" worktree list --porcelain | grep -c '^worktree ')" "1"

# ---- reopen ----------------------------------------------------------------

# An open status brings a closed task back: api on the branch cancel kept,
# docs (pushed, so cancel deleted its branch) on a new one from origin's.
step reopen
S="$ATL_TASKS/second-task"
rm -f "$E2E_OUT/setup-second-task"
out="$(atl status second-task active 2>&1)" || fail "atl status (reopen) failed: $out"
[[ "$out" == *"repo docs: branch atl/second-task is gone here; made again from origin/atl/second-task"* ]] ||
	fail "reopen did not say where docs' branch came from: $out"
[[ "$out" != *"warning"* ]] || fail "reopen warned: $out"
expect_eq "status after reopen" "$(task_field second-task status)" "active"
expect_eq "reopened api branch" "$(git -C "$S/api" rev-parse --abbrev-ref HEAD)" "atl/second-task"
expect_eq "reopened api commit" "$(git -C "$S/api" log -1 --format=%s)" "unpushed work"
expect_eq "reopened docs branch" "$(git -C "$S/docs" rev-parse --abbrev-ref HEAD)" "atl/second-task"
expect_eq "reopened docs commit" "$(git -C "$S/docs" log -1 --format=%s)" "docs change"
[[ -f "$S/CLAUDE.md" && -f "$S/.claude/settings.json" ]] || fail "reopen did not write CLAUDE.md and settings.json"
expect_eq "setup after reopen" "$(cat "$E2E_OUT/setup-second-task")" "100"
out="$(atl session second-task)" || fail "atl session after reopen failed: $out"
has_cid2() { [[ -n "$(sess_field second-task claude_session_id)" ]]; }
wait_for "SessionStart hook in the reopened task" has_cid2
out="$(atl cancel second-task 2>&1)" || fail "atl cancel (after reopen) failed: $out"
[[ "$out" == *"kept branch atl/second-task in api (1 unpushed commit(s))"* &&
	"$out" == *"deleted branch atl/second-task in docs"* ]] || fail "atl cancel (after reopen) branch messages: $out"
[[ ! -e "$S" ]] || fail "second-task dir still exists"
expect_eq "worktrees of api after the second cancel" "$(git -C "$ATL_REPOS/api" worktree list --porcelain | grep -c '^worktree ')" "1"

# ---- done from inside the task's tmux session ------------------------------

# A shell in a window of the task's own tmux session runs atl done: closing
# kills that session, which hangs up on the shell and atl; atl still finishes.
# Its teardown gets SIGHUP's default disposition (not atl's ignoring it):
# it records the mask of ignored signals of its shell.
step "done from the task's own tmux session"
cat >"$WS/config/projects/selfclose.toml" <<'EOF'
teardown = 'grep "^SigIgn:" /proc/$$/status >"$E2E_OUT/self-close-sigign"'

[[repos]]
name = "api"
mode = "rw"
EOF
out="$(atl new "Self close" --project selfclose --no-fetch)" || fail "atl new (self close) failed: $out"
out="$(atl session self-close)" || fail "atl session self-close failed: $out"
tm new-window -d -t "=atl-self-close:" "atl done self-close >'$E2E_OUT/self-close.log' 2>&1; sleep 60" ||
	fail "start a shell window in atl-self-close"
self_closed() { [[ "$(task_field self-close status)" == done ]]; }
wait_for "atl done run from the task's own tmux session" self_closed
[[ ! -e "$ATL_TASKS/self-close" ]] || fail "self-close dir still exists: $(cat "$E2E_OUT/self-close.log")"
expect_eq "worktrees of api after done from inside" "$(git -C "$ATL_REPOS/api" worktree list --porcelain | grep -c '^worktree ')" "1"
sigign="$(sed -n 's/^SigIgn:[[:space:]]*//p' "$E2E_OUT/self-close-sigign")"
[[ -n $sigign ]] || fail "teardown did not record its ignored signals: $(cat "$E2E_OUT/self-close.log")"
((((16#$sigign) & 1) == 0)) || fail "teardown inherited an ignored SIGHUP (SigIgn $sigign)"

echo "E2E OK"
