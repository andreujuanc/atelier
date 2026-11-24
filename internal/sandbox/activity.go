package sandbox

import (
	"path"
	"sort"
	"strconv"
	"strings"
)

// activityMarker names the probe's own processes (activityScript's $0),
// which ParseActivity leaves out together with their children.
const activityMarker = "atl-activity"

// activityScript lists, in a task container, every process: one line per
// process, "<pid>\t<ppid>\t<ATL_SESSION value or empty>\t<command line>",
// the command line's NULs and newlines as spaces (a command line with a
// newline would otherwise make lines of its own). Processes without a command line
// (zombies) are skipped. It runs with plain sh and tr as the container's
// user: the environment of a process of another user (root, from a
// `docker exec -u 0`) cannot be read, so its session is empty. atl only
// parses what it prints (ParseActivity), into a list it shows the user.
const activityScript = `for d in /proc/[0-9]*; do
	p=${d#/proc/}
	[ "$p" = "$$" ] && continue
	c=$(tr '\0\n' '  ' 2> /dev/null < "$d/cmdline") || continue
	[ -n "$c" ] || continue
	pp=
	while read -r k v; do
		[ "$k" = PPid: ] && { pp=$v; break; }
	done < "$d/status" 2> /dev/null
	[ -n "$pp" ] || continue
	s=
	for e in $(tr '\0' '\n' 2> /dev/null < "$d/environ" | grep '^ATL_SESSION=[0-9]*$'); do s=${e#ATL_SESSION=}; done
	printf '%s\t%s\t%s\t%s\n' "$p" "$pp" "$s" "$c"
done`

// ActivityArgs are the docker arguments that list the processes of the task
// container (by the full ID Agent found) for ParseActivity.
func ActivityArgs(container string) []string {
	return []string{"exec", container, "sh", "-c", activityScript, activityMarker}
}

// Proc is a process that runs in a task container besides what an idle
// task has there: a command a Claude session runs (Claude Code's Bash
// tool, in the foreground or the background), a process left running by a
// command that ended (a daemonised dev server, now a child of the
// container's init), or one that something else started there (a shell of
// a `docker exec`).
type Proc struct {
	PID     int
	Session int64 // the session (ATL_SESSION) it belongs to; 0 if none
	Command string
}

// shells are the programs Claude Code's Bash tool runs commands with.
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ash": true, "ksh": true, "fish": true}

// ParseActivity reads the output of ActivityArgs and returns what runs in
// the container besides an idle task, sorted by PID. An idle task has: the
// container's init (PID 1) and its `sleep infinity`; one root process per
// Claude session (started by docker exec, so its parent PID reads 0, with
// ATL_SESSION set); and their children that are not shells (MCP servers,
// ripgrep, ...: Claude's own helpers, which a restart ends without loss).
// atl's own commands (this probe, the session clean-ups, the hooks and the
// status line Claude runs) are left out too.
//
// roots holds the sessions whose Claude (a root process with their
// ATL_SESSION) is there: a live session missing from it hides what it
// runs (its commands would not be its children), which the caller must
// not take for idle.
// What a counted process started is not listed apart: its Command is that
// of its first child when it has one (a Bash tool shell's command line is
// Claude Code's wrapper; the child is the command it runs).
func ParseActivity(out string) (procs []Proc, roots map[int64]bool) {
	type proc struct {
		pid, ppid int
		session   int64
		args      string
	}
	all := map[int]*proc{}
	children := map[int][]int{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, "\t", 4)
		if len(f) != 4 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil || pid <= 0 || ppid < 0 {
			continue
		}
		sid, _ := strconv.ParseInt(f[2], 10, 64)
		all[pid] = &proc{pid: pid, ppid: ppid, session: sid, args: strings.TrimSpace(f[3])}
		children[ppid] = append(children[ppid], pid)
	}
	for _, c := range children {
		sort.Ints(c)
	}
	argv0 := func(p *proc) string {
		a := p.args
		if i := strings.IndexByte(a, ' '); i >= 0 {
			a = a[:i]
		}
		return strings.TrimPrefix(path.Base(a), "-") // a login shell's argv[0] starts with -
	}
	ours := func(p *proc) bool {
		// atl's hooks and status line, which Claude runs (in a shell) for a
		// moment on its events: not something a restart would cut off.
		// They name it shell-quoted (gen.HookCommand) or not.
		for _, bin := range []string{InBin, "'" + InBin + "'"} {
			if strings.Contains(p.args, bin+" hook ") || strings.Contains(p.args, bin+" statusline") {
				return true
			}
		}
		for _, w := range strings.Fields(p.args) {
			switch w {
			case activityMarker, "atl-kill-session", "atl-end-session":
				return true
			}
		}
		return false
	}
	command := func(p *proc) string {
		if c := children[p.pid]; len(c) > 0 {
			if k := all[c[0]]; k != nil && k.args != "" {
				return k.args
			}
		}
		return p.args
	}
	roots = map[int64]bool{}
	add := func(p *proc, session int64) {
		if session == 0 {
			session = p.session
		}
		procs = append(procs, Proc{PID: p.pid, Session: session, Command: command(p)})
	}
	for _, pid := range sortedKeys(all) {
		p := all[pid]
		if pid == 1 || ours(p) {
			continue
		}
		switch parent := all[p.ppid]; {
		case p.ppid == 0:
			// Started by docker exec: a session's Claude, else counted.
			if p.session == 0 {
				add(p, 0)
			} else {
				roots[p.session] = true
			}
		case p.ppid == 1:
			// The container's sleep, else an orphan (a daemonised server).
			if p.args != "sleep infinity" {
				add(p, 0)
			}
		case parent != nil && parent.ppid == 0 && parent.session != 0 && !ours(parent):
			// A child of a session's Claude: its commands run in shells.
			if shells[argv0(p)] {
				add(p, parent.session)
			}
		}
	}
	return procs, roots
}

func sortedKeys[V any](m map[int]V) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}
