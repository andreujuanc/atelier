#!/usr/bin/env bash
# End-to-end test for atelier: builds atl, creates fake repos and a project in
# a temp dir, and exercises new → session → hooks → crash recovery → resume →
# diff → pr → done/cancel against an isolated tmux server. `claude` and `gh`
# are stub scripts (test/stubs). Requires bash, go, git, tmux and jq or
# python3. Run from anywhere: test/e2e.sh. Prints "E2E OK" on success.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
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

cleanup() {
	local rc=$?
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
(cd "$ROOT" && go build -o "$W/bin/atl" ./cmd/atl) || fail "go build"

# Isolated environment.
unset TMUX TMUX_PANE ATL_BIN ATL_SESSION ATL_TASK ATL_TASK_DIR ATL_PORT_OFFSET
export PATH="$W/bin:$STUBS:$PATH"
export HOME="$W/home"
export ATL_REPOS="$W/repos"
export ATL_TASKS="$W/tasks"
export XDG_DATA_HOME="$W/data"
export XDG_STATE_HOME="$W/state"
export XDG_CONFIG_HOME="$W/config"
export ATL_TMUX_SOCKET="$SOCK"
export GIT_CONFIG_GLOBAL="$W/gitconfig"
export GIT_CONFIG_NOSYSTEM=1
export SHELL=/bin/sh
export E2E_OUT="$W/out"
export E2E_LOG="$W/out/claude.log"
export E2E_GH_LOG="$W/out/gh.log"
mkdir -p "$HOME" "$ATL_REPOS" "$ATL_TASKS" "$XDG_CONFIG_HOME/atelier/projects" "$E2E_OUT" "$W/origin" "$W/seed"
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

# ---- helpers ---------------------------------------------------------------

tm() { tmux -L "$SOCK" "$@"; }

start_tmux() {
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
for r in api docs; do
	git init -q --bare "$W/origin/$r.git"
	git -C "$W/origin/$r.git" symbolic-ref HEAD refs/heads/main
	git init -q "$W/seed/$r"
	echo "# $r" >"$W/seed/$r/README.md"
	git -C "$W/seed/$r" add README.md
	git -C "$W/seed/$r" commit -q -m "init $r"
	git -C "$W/seed/$r" push -q "$W/origin/$r.git" HEAD:main
	git clone -q "$W/origin/$r.git" "$ATL_REPOS/$r"
done

# Make the canonical api clone stale and off its default branch: origin's main
# gains a commit the clone has not fetched, and the clone sits on a local-only
# branch with a local-only commit. Tasks must still branch from origin's main.
git clone -q "$W/origin/api.git" "$W/seed/api-upstream"
echo "upstream change" >"$W/seed/api-upstream/upstream.txt"
git -C "$W/seed/api-upstream" add upstream.txt
git -C "$W/seed/api-upstream" commit -q -m "upstream change"
git -C "$W/seed/api-upstream" push -q origin HEAD:main
git -C "$ATL_REPOS/api" checkout -q -b local-work
echo "local only" >"$ATL_REPOS/api/local.txt"
git -C "$ATL_REPOS/api" add local.txt
git -C "$ATL_REPOS/api" commit -q -m "local only"
ORIGIN_MAIN="$(git -C "$W/origin/api.git" rev-parse main)"
CANON_HEAD="$(git -C "$ATL_REPOS/api" rev-parse HEAD)"
[[ "$(git -C "$ATL_REPOS/api" rev-parse main)" != "$ORIGIN_MAIN" ]] || fail "canonical api main is not stale"

cat >"$XDG_CONFIG_HOME/atelier/projects/shop.toml" <<'EOF'
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

# ---- new -------------------------------------------------------------------

step "new task"
T="$ATL_TASKS/fix-login"
out="$(atl new "Fix login" --project shop)" || fail "atl new failed: $out"
expect_eq "atl new output" "$out" "fix-login	$T"
[[ "$(git -C "$T/api" rev-parse --abbrev-ref HEAD)" == "atl/fix-login" ]] || fail "api worktree not on atl/fix-login"
expect_eq "api worktree base (origin main)" "$(git -C "$T/api" rev-parse HEAD)" "$ORIGIN_MAIN"
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
expect_eq "setup port offset" "$(cat "$E2E_OUT/setup-fix-login")" "0"
expect_eq "task status" "$(task_field fix-login status)" "active"

out="$(atl new "Second task" --project shop)" || fail "atl new (second) failed: $out"
expect_eq "second port offset" "$(task_field second-task port_offset)" "100"
expect_eq "second setup port offset" "$(cat "$E2E_OUT/setup-second-task")" "100"

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

# ---- crash recovery + resume -----------------------------------------------

step "crash recovery"
tm kill-server
start_tmux
expect_state fix-login stopped "after tmux server crash (startup recover)"

step resume
out="$(atl resume fix-login)" || fail "atl resume failed: $out"
expect_eq "atl resume count" "$out" "1"
resumed() { grep -qF "sid=$CID argv=[--resume][$CID][--add-dir][$ATL_REPOS/docs]" "$E2E_LOG"; }
wait_for "stub claude relaunched with --resume $CID" resumed
expect_state fix-login idle "after resume"
expect_eq "claude session id after resume" "$(sess_field fix-login claude_session_id)" "$CID"
expect_eq "session count after resume" "$(sess_count fix-login)" "1"
tm has-session -t "=atl-fix-login" 2>/dev/null || fail "tmux session atl-fix-login not recreated"
[[ -n "$(sess_field fix-login tmux_target)" ]] || fail "resumed session has no tmux target"

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
grep -qF "[pr][create][--head][atl/fix-login][--base][main]" "$E2E_GH_LOG" || fail "gh not called as expected: $(cat "$E2E_GH_LOG")"
grep -qF "cwd=$T/api " "$E2E_GH_LOG" || fail "gh not run in the api worktree: $(cat "$E2E_GH_LOG")"
expect_eq "status after pr" "$(task_field fix-login status)" "review"

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

echo "E2E OK"
