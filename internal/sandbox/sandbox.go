// Package sandbox runs a task's Claude sessions in the task's own Docker
// container instead of next to atl, and gives that container a gated Docker
// socket (see Gate) in place of the host's.
//
// It is on by default; $ATL_SANDBOX=off switches it off for a workspace
// (a way out while it is new, going away). The image comes
// from the task's project (`image = "..."`), else $ATL_SANDBOX_IMAGE, else
// DefaultImage; it needs `claude` and a shell, and should have the same user
// as the one running atl (atelier's images do).
package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// DefaultImage is the task image when neither the project nor
// $ATL_SANDBOX_IMAGE names one: the target "task" of atelier's Dockerfile.
const DefaultImage = "atelier-task"

// checkout stands for the path of a checkout of atelier in BuildImages and
// BuildDefaultImage.
const checkout = "<atelier checkout>"

// BuildImages is the command that builds the box image and then
// DefaultImage for the user running it, from any directory: atelier's
// build-images script.
const BuildImages = checkout + "/build-images"

// BuildDefaultImage is the command that builds only DefaultImage for the
// user running it, from any directory.
const BuildDefaultImage = "docker build --target task -t " + DefaultImage +
	" --build-arg USERNAME=$(whoami) --build-arg UID=$(id -u) --build-arg GID=$(id -g) " + checkout

// ImageMissing reports whether image is not on the Docker daemon.
func ImageMissing(ctx context.Context, run Runner, image string) (bool, error) {
	_, err := run(ctx, "image", "inspect", "--format", "{{.Id}}", image)
	if noSuch(err) {
		return true, nil
	}
	return false, err
}

// Where a task container finds what atl gives it.
const (
	RunMount = "/run/atl"                // the task's run dir, read-only
	InSocket = RunMount + "/docker.sock" // the gate
	InBin    = RunMount + "/bin/atl"     // the atl binary that created the container
	// InEnv is set (to the sandbox ID) in every task container. atl running
	// in there opens no database: hooks, the status line, note, notes, push
	// and fetch ask the task's gate (see Control), everything else is refused.
	InEnv = "ATL_SANDBOX_ID"
)

const labelSpec = "atelier.spec"

// Config is the workspace's sandbox setting; nil means tasks run next to atl.
type Config struct {
	Image string // default task image
	// Mounts are the host paths every task gets read-only, symlinks
	// resolved ($ATL_SANDBOX_MOUNTS, see ROMounts; FromEnv leaves it empty).
	Mounts []string
}

// FromEnv reads $ATL_SANDBOX and $ATL_SANDBOX_IMAGE. The sandbox is on
// unless $ATL_SANDBOX is "off" (a way out while the sandbox is new; it is
// going away): unset, empty and "docker" all mean on, and any other value is
// an error (a typo must not pick a mode). It returns nil when the sandbox is
// off, and also inside a task container, where there is nothing to launch.
func FromEnv() (*Config, error) {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv("ATL_SANDBOX"))); v {
	case "off":
		return nil, nil
	case "", "docker":
	default:
		return nil, fmt.Errorf("$ATL_SANDBOX=%q: want docker (the default, also when unset) or off", v)
	}
	if Inside() {
		return nil, nil
	}
	image := strings.TrimSpace(os.Getenv("ATL_SANDBOX_IMAGE"))
	if image == "" {
		image = DefaultImage
	}
	return &Config{Image: image}, nil
}

// Inside reports whether this process runs in a task container.
func Inside() bool { return os.Getenv(InEnv) != "" }

func shortHash(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:n]
}

// ID names a task's sandbox on the Docker daemon: the slug plus a short hash
// of the task dir, so equal slugs in two workspaces on one daemon differ.
func ID(slug, taskDir string) string { return slug + "-" + shortHash(taskDir, 6) }

// ContainerName is the name of the task container of sandbox id.
func ContainerName(id string) string { return "atl-" + id }

// reservedName matches every ContainerName: atl-<slug>-<6 hex digits>.
var reservedName = regexp.MustCompile(`(?i)^atl-.+-[0-9a-f]{6}$`)

// ReservedName reports whether a container name given to Docker would be
// in the pattern of ContainerName, which only atl may use (see Gate). It
// reads the name as dockerd may: with any leading slash, in any case.
// (dockerd strips one leading slash and refuses a second, and its names are
// case-sensitive; both are covered with room to spare.)
func ReservedName(name string) bool {
	return reservedName.MatchString(strings.TrimLeft(name, "/"))
}

// ComposeProject is the Docker name namespace of sandbox id: atl sets it as
// the task container's $COMPOSE_PROJECT_NAME, and the task's gate allows
// new network and volume names, compose project labels and container
// names in the pattern of any task's namespace (Namespaced) only in it
// (see InNamespace). It is atl_ and the 6 hex digits of the ID (the hash
// of the task dir); an ID not made by ID gets a hash of its own. Every
// namespace has the same length, so none is a prefix of another's; and
// its underscore keeps it apart from ContainerName's atl-.
func ComposeProject(id string) string {
	hash := id[strings.LastIndexByte(id, '-')+1:]
	if !composeHash.MatchString(hash) {
		hash = shortHash(id, 6)
	}
	return "atl_" + hash
}

var composeHash = regexp.MustCompile(`^[0-9a-f]{6}$`)

// namespaced matches every name in the pattern of a ComposeProject
// namespace, in any case and with any leading slash.
var namespaced = regexp.MustCompile(`(?i)^/*atl_[0-9a-f]{6}($|[-_])`)

// InNamespace reports whether name, a network, volume or compose project
// name, is in namespace ns (a ComposeProject): ns itself, or ns followed
// by - or _ and more. It compares exactly, as dockerd and compose do (the
// case counts: compose project names are lower case).
func InNamespace(ns, name string) bool {
	return name == ns || strings.HasPrefix(name, ns+"-") || strings.HasPrefix(name, ns+"_")
}

// Namespaced reports whether name is in the pattern of any task's
// namespace (see ComposeProject), in any case and with any leading slash.
func Namespaced(name string) bool { return namespaced.MatchString(name) }

// Mount is a bind mount of a host path into the task container.
type Mount struct {
	Src, Dst string
	RO       bool
}

// Spec describes a task container.
type Spec struct {
	ID, Image string
	User      string // "uid:gid"
	Workdir   string
	Mounts    []Mount
	Env       map[string]string
	// NoPull: docker run never pulls the image (--pull=never); it must be
	// on the daemon already, built or pulled by the user.
	NoPull bool
}

// Hash identifies the spec; it is stored on the container (label
// atelier.spec) to notice that the container predates a change.
func (s Spec) Hash() string { return shortHash(strings.Join(s.runArgs(""), "\x00"), 16) }

// RunArgs is the docker command line that creates the container. It only
// sleeps: sessions are started in it with docker exec.
func (s Spec) RunArgs() []string { return s.runArgs(s.Hash()) }

func (s Spec) runArgs(hash string) []string {
	args := []string{"run", "-d", "--init", "--name", ContainerName(s.ID),
		"--label", LabelAgent + "=" + s.ID}
	if s.NoPull {
		args = append(args, "--pull=never")
	}
	if hash != "" {
		args = append(args, "--label", labelSpec+"="+hash)
	}
	if s.User != "" {
		args = append(args, "--user", s.User)
	}
	if s.Workdir != "" {
		args = append(args, "--workdir", s.Workdir)
	}
	for _, m := range s.Mounts {
		v := "type=bind,src=" + m.Src + ",dst=" + m.Dst
		if m.RO {
			v += ",readonly"
		}
		args = append(args, "--mount", v)
	}
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--env", k+"="+s.Env[k])
	}
	return append(args, s.Image, "sleep", "infinity")
}

// ExecArgv is the command for a tmux window that runs argv in the task
// container, named by the full ID Ensure returned (not by its name, which
// is only checked there), in workdir. The variables named in envKeys are
// passed on from the window's environment; an entry NAME=VALUE sets NAME to
// VALUE. The atl binary of the run dir
// comes first on PATH, so `atl` in the container is the atl that made it,
// and so does its wl-copy, the copy relay, which $WAYLAND_DISPLAY (set to
// CopyDisplay) makes Claude Code use (see CopyMain).
//
// docker's default detach sequence starts with ctrl-p, which it then holds
// back until the next key; an unlikely sequence keeps ctrl-p usable.
func ExecArgv(container, workdir string, envKeys, argv []string) []string {
	out := []string{"docker", "exec", "-it", "--detach-keys", "ctrl-^,ctrl-^,ctrl-^", "-w", workdir}
	keys := append([]string{"TERM", "COLORTERM", "COLORFGBG", "NO_COLOR"}, envKeys...)
	sort.Strings(keys)
	for i, k := range keys {
		if i == 0 || k != keys[i-1] {
			out = append(out, "-e", k)
		}
	}
	out = append(out, container, "sh", "-c", `PATH=`+filepath.Dir(InBin)+`:$PATH WAYLAND_DISPLAY=`+CopyDisplay+` exec "$@"`, "atl-exec")
	return append(out, argv...)
}

// PipeExecArgv is the command that runs argv in container (its full ID)
// with no terminal: stdin is passed on (docker exec -i), stdout and stderr
// come back. The variables named in envKeys are passed on from the
// command's environment, by name, so their values are on no command line.
func PipeExecArgv(container string, envKeys, argv []string) []string {
	out := []string{"docker", "exec", "-i"}
	keys := slices.Clone(envKeys)
	sort.Strings(keys)
	for _, k := range slices.Compact(keys) {
		out = append(out, "-e", k)
	}
	out = append(out, container)
	return append(out, argv...)
}

// windowScript runs, where atl runs, as the command of a session's tmux
// window: it starts docker exec (its arguments after the first) so that
// the terminal size Claude sees in the task container is right from its
// first frame and follows every change of the window's size.
//
// docker exec -t reads the window's size when it creates the exec, sends it
// once more right after the start, and only then listens for SIGWINCH: a
// resize in between is lost for good, and Claude keeps a detached window's
// 80x24 in a larger terminal until the next resize. atl attaching the
// terminal right after it created the window (atl open, the TUI's Enter)
// lands there often. So:
//   - with $1 = attach (an attach follows), it waits, up to 10 seconds,
//     for a client on the window's tmux session before it starts docker
//     exec, which then reads the size the window has with that client;
//   - a helper sends docker exec SIGWINCH 1, 3 and 7 seconds after it
//     started, which makes it send the window's current size again: a
//     resize during its start (a client attaching from elsewhere then) is
//     made good. The helper holds none of the window's terminal, and ends
//     with docker exec (the hangup the window's end brings) or at the first
//     signal that finds it gone.
//
// It needs sh, tmux, sleep and kill, where atl runs.
const windowScript = `if [ "$1" = attach ]; then
	i=0
	while [ $i -lt 100 ]; do
		n=$(tmux display-message -p -t "$TMUX_PANE" '#{session_attached}' 2> /dev/null) || break
		[ "${n:-0}" = 0 ] || break
		sleep 0.1 2> /dev/null || { sleep 1; i=$((i + 9)); }
		i=$((i + 1))
	done
fi
shift
pid=$$
(for d in 1 2 4; do sleep $d; kill -WINCH $pid || exit 0; done) < /dev/null > /dev/null 2>&1 &
exec "$@"`

// WindowArgv wraps exec, the docker exec command of ExecArgv, as the
// command of a session's tmux window (see windowScript). attach says that
// atl attaches a terminal to the window's tmux session right after it is
// created: docker exec then waits for that client.
func WindowArgv(attach bool, exec []string) []string {
	mode := "detached"
	if attach {
		mode = "attach"
	}
	return append([]string{"sh", "-c", windowScript, "atl-window", mode}, exec...)
}

// sessionScript ends, in a task container, the processes of a session:
// those whose environment has ATL_SESSION=<session>, which ExecArgv's
// window passes to Claude and Claude's children inherit (a dev server it
// started in the background included). TERM first, KILL after 5 seconds.
// It runs with plain sh, tr and grep as the container's user, whose
// processes the session's are: root in the container could not read their
// /proc/<pid>/environ (that takes CAP_SYS_PTRACE, which containers do not
// have). atl reads nothing of what it prints (the container is the task's).
//
// Arguments: mode (kill or end), session, and for tests the parent PID of
// a root (default 0) and how long end waits (default 120 seconds); then the
// variable that marks the processes (default ATL_SESSION; ScriptVar for a
// project script's, see KillScriptArgs).
//
// kill ends them all. end is for a session whose Claude is ending: the
// session's roots, the processes docker exec started for it (their parent
// is outside the container, so their parent PID reads 0 in it: Claude, the
// window's command), are waited for to exit, up to the wait; then the rest
// is ended, unless a root of the session runs again (a resume started it
// anew). A root that is still there after the wait means the session goes
// on (a SessionEnd for /clear, or a nested claude): nothing is touched.
// Processes of other sessions never carry this session's ATL_SESSION.
const sessionScript = `mode=$1 s="${5:-ATL_SESSION}=$2" rootppid=${3:-0} wait=${4:-120}
# scan: the session's processes but this script, one per line, roots as r<pid>
scan() {
	for d in /proc/[0-9]*; do
		p=${d#/proc/}
		[ "$p" = "$$" ] && continue
		tr '\0' '\n' 2> /dev/null < "$d/environ" | grep -qx -- "$s" || continue
		if grep -qx "PPid:[[:space:]]*$rootppid" "$d/status" 2> /dev/null; then echo "r$p"; else echo "$p"; fi
	done
}
if [ "$mode" = end ]; then
	set -- $(scan | grep '^r' | tr -d r)
	i=0
	while [ $# -gt 0 ]; do
		[ $i -ge "$wait" ] && exit 0
		sleep 1
		i=$((i + 1))
		for p in "$@"; do
			shift
			tr '\0' '\n' 2> /dev/null < "/proc/$p/environ" | grep -qx -- "$s" && set -- "$@" "$p"
		done
	done
fi
i=0
while :; do
	l=$(scan)
	[ "$mode" = end ] && echo "$l" | grep -q '^r' && exit 0
	set -- $(echo "$l" | tr -d r)
	[ $# -eq 0 ] && exit 0
	case $i in
	0) kill -TERM "$@" 2> /dev/null ;;
	5) kill -KILL "$@" 2> /dev/null; exit 0 ;;
	esac
	sleep 1
	i=$((i + 1))
done`

// KillSessionArgs are the docker arguments that end, in the task container
// (by the full ID Agent found), every process of the session with id
// session (sessionScript, mode kill). When the docker exec client of a
// window dies (the window is closed, tmux or the box restarts), dockerd
// leaves what it started running; this is what stands in for the hangup
// the window's death used to be.
func KillSessionArgs(container string, session int64) []string {
	return append([]string{"exec", container}, sessionCmd("kill", session)...)
}

// EndSessionCmd is the command that ends, in the task container, what
// session left running once its Claude has exited, and nothing while it
// runs (sessionScript, mode end). The gate starts it, detached, when
// Claude reports the session's end (SessionEnd), which Claude does before
// it exits.
func EndSessionCmd(session int64) []string { return sessionCmd("end", session) }

func sessionCmd(mode string, session int64) []string {
	return []string{"sh", "-c", sessionScript, "atl-" + mode + "-session", mode, strconv.FormatInt(session, 10)}
}

// ScriptVar marks the processes of one run of a project's setup or
// teardown script in a task container (ScriptArgs), with a value of its
// own per run, for KillScriptArgs to find them.
const ScriptVar = "ATL_SCRIPT"

// scriptKiller is $0 of the script that ends a script run's processes
// (sessionScript, mode kill). ParseActivity does not leave it out as it
// does the session clean-ups: scriptWrapper has the name in its text, and a
// script that runs is activity.
const scriptKiller = "atl-kill-script"

// scriptWrapper runs, in a task container, a project script ($3) with sh
// -c, its processes marked with ScriptVar=$2 and the run dir's atl first on
// PATH (as in a session, see ExecArgv), and exits with its status. When the
// script's shell exits, what it left running ends (sessionScript, $1, mode
// kill); the wrapper and its copier are not marked, so that leaves them
// alone.
//
// docker exec returns only once nothing in the container holds its output
// open, and a process the script started without the mark (env -u) would
// hold it, and atl, for as long as it runs. So the script writes to a FIFO
// of the wrapper's, which cat copies to docker exec's output; once the
// script's shell has exited, cat gets 5 seconds to copy what is left (it
// ends at once when no writer is left), then it is killed, and the wrapper
// exits: then nothing holds docker exec's output. Without a FIFO (no
// mktemp or mkfifo, or no writable /tmp in the image) the script writes
// to docker exec's output itself.
const scriptWrapper = `k=$1 run=$2
d=$(mktemp -d 2> /dev/null) && mkfifo "$d/out" 2> /dev/null || { [ -z "$d" ] || rmdir "$d"; d=; }
if [ -n "$d" ]; then
	cat "$d/out" &
	c=$!
	PATH=` + RunMount + `/bin:$PATH ` + ScriptVar + `=$run sh -c "$3" > "$d/out" 2>&1
else
	PATH=` + RunMount + `/bin:$PATH ` + ScriptVar + `=$run sh -c "$3"
fi
st=$?
sh -c "$k" ` + scriptKiller + ` kill "$run" 0 0 ` + ScriptVar + ` > /dev/null 2>&1
if [ -n "$d" ]; then
	i=0
	while [ $i -lt 50 ] && kill -0 $c 2> /dev/null; do
		sleep 0.1 2> /dev/null || { sleep 1; i=$((i + 9)); }
		i=$((i + 1))
	done
	kill $c 2> /dev/null
	rm -rf "$d"
fi
exit $st`

// ScriptArgs are the docker arguments that run a project's setup or
// teardown script in the task container (by the full ID Ensure returned) as
// user ("uid:gid"), in workdir, with env as its only variables besides the
// container's own (whose DOCKER_HOST is the task's gate): nothing of the
// docker client's environment goes in. run marks the script's processes
// (ScriptVar) for KillScriptArgs; what the script leaves running ends when
// it exits (scriptWrapper). No terminal, no stdin.
func ScriptArgs(container, user, workdir string, env map[string]string, run, script string) []string {
	args := []string{"exec", "-u", user, "-w", workdir}
	keys := make([]string, 0, len(env))
	for k := range env {
		if k != ScriptVar {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-e", k+"="+env[k])
	}
	return append(args, container, "sh", "-c", scriptWrapper, "atl-script", sessionScript, run, script)
}

// KillScriptArgs are the docker arguments that end, in the task container,
// every process of the script run run (ScriptArgs): TERM, then KILL after
// 5 seconds (sessionScript, mode kill). A docker exec client that is
// killed leaves what it started running in the container: this is what
// stops an interrupted script.
func KillScriptArgs(container, run string) []string {
	return []string{"exec", container, "sh", "-c", sessionScript, scriptKiller, "kill", run, "0", "0", ScriptVar}
}

// Runner runs the docker CLI with args and returns its trimmed stdout; a
// failure carries docker's stderr.
type Runner func(ctx context.Context, args ...string) (string, error)

// Docker is the Runner that calls the docker binary on PATH.
func Docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("docker %s: %s", args[0], msg)
	}
	return strings.TrimSpace(string(out)), nil
}

func noSuch(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no such")
}

// AgentContainer is the task container of a sandbox as Docker has it.
type AgentContainer struct {
	ID      string // full ID: what atl addresses it by once checked
	Running bool
	Hash    string // its atelier.spec label (see Spec.Hash)
}

// ErrNotAgent wraps the error of Agent for a container that has the task
// container's name but is not atl's.
var ErrNotAgent = errors.New("not atl's")

// Agent finds the task container of sandbox id by its name and checks that
// atl made it: it carries atelier.agent=<id> and no atelier.task label. A
// task cannot give a container that name through its gate (ReservedName)
// nor the atelier.agent label, but a container takes over the labels of its
// image, which a task can build; so a container of any task carries
// atelier.task, which the gate sets on everything a task creates. A
// container that fails the check is an error wrapping ErrNotAgent, one line
// that names it. One that does not exist is docker's "No such" error
// (IsNoSuch).
func Agent(ctx context.Context, run Runner, id string) (AgentContainer, error) {
	name := ContainerName(id)
	out, err := run(ctx, "inspect", "--type", "container", "--format",
		`{{.Id}} {{.State.Running}} {{json .Config.Labels}}`, name)
	if err != nil {
		return AgentContainer{}, err
	}
	fields := strings.SplitN(out, " ", 3)
	var labels map[string]string
	if len(fields) != 3 || json.Unmarshal([]byte(fields[2]), &labels) != nil || !fullID(fields[0]) {
		return AgentContainer{}, fmt.Errorf("docker inspect %s: unexpected answer", name)
	}
	if _, isTasks := labels[LabelTask]; labels[LabelAgent] != id || isTasks {
		return AgentContainer{}, fmt.Errorf("container %s (%.12s) is %w: it lacks the label %s=%s or has a label %s; "+
			"atl neither uses nor removes it: remove or rename it yourself", name, fields[0], ErrNotAgent, LabelAgent, id, LabelTask)
	}
	return AgentContainer{ID: fields[0], Running: fields[1] == "true", Hash: labels[labelSpec]}, nil
}

// fullID reports whether s is a full Docker object ID: 64 hex digits.
func fullID(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// IsNoSuch reports whether err is docker's "No such ..." (container, image,
// object).
func IsNoSuch(err error) bool { return noSuch(err) }

// Ensure makes the task container exist and run, and returns its full ID.
// A container made from another spec (the task's repos or the atl paths
// changed since) is replaced unless busy reports that sessions are running
// in it; then it is kept and stale is true.
//
// Only atl's own container is used, replaced or started (see Agent): a
// container that merely has the name is an error, and left alone.
//
// fresh, if not nil, runs right before a container is created, when no
// container of the task exists (any old one was removed): what it prepares
// for the container cannot be touched by the task meanwhile. Its error stops
// the creation.
//
// The container returned has a usable $HOME (makeHome): made when the
// container is created, and checked once per atl process when it is
// reused, since it may predate makeHome or its creator may have died first.
// Not in a container kept although it predates a change: its mounts are
// not the spec's.
func Ensure(ctx context.Context, run Runner, spec Spec, busy func() bool, fresh func() error) (container string, stale bool, err error) {
	create := func() (string, error) {
		if fresh != nil {
			if err := fresh(); err != nil {
				return "", err
			}
		}
		out, err := run(ctx, spec.RunArgs()...)
		if err != nil {
			if !strings.Contains(err.Error(), "already in use") {
				return "", err
			}
			// Lost a race with another atl creating it: take theirs, if it
			// is atl's, with its home made, in case that atl has not made it
			// yet or died first; left alone if that fails (it is theirs).
			c, err := Agent(ctx, run, spec.ID)
			if err != nil {
				return "", err
			}
			if !c.Running {
				if _, err := run(ctx, "start", c.ID); err != nil {
					return "", err
				}
			}
			if err := spec.makeHomeOnce(ctx, run, c.ID); err != nil {
				return "", err
			}
			return c.ID, nil
		}
		var id string
		if lines := strings.Fields(out); len(lines) > 0 && fullID(lines[len(lines)-1]) {
			id = lines[len(lines)-1] // docker run -d prints the ID last
		} else {
			c, err := Agent(ctx, run, spec.ID)
			if err != nil {
				return "", err
			}
			id = c.ID
		}
		if err := spec.makeHome(ctx, run, id); err != nil {
			// Removed, so that the next try makes it again.
			if _, rerr := run(ctx, "rm", "-f", id); rerr != nil && !noSuch(rerr) {
				err = errors.Join(err, rerr)
			}
			return "", err
		}
		homesMade.Store(id, true)
		return id, nil
	}
	c, err := Agent(ctx, run, spec.ID)
	switch {
	case noSuch(err):
		container, err = create()
		return container, false, err
	case err != nil:
		return "", false, err
	}
	if c.Hash != spec.Hash() {
		if c.Running && busy != nil && busy() {
			return c.ID, true, nil
		}
		if _, err := run(ctx, "rm", "-f", c.ID); err != nil && !noSuch(err) {
			return "", false, err
		}
		container, err = create()
		return container, false, err
	}
	if !c.Running {
		if _, err = run(ctx, "start", c.ID); err != nil {
			return "", false, err
		}
	}
	if err := spec.makeHomeOnce(ctx, run, c.ID); err != nil {
		return "", false, err
	}
	return c.ID, false, nil
}

// homeScript makes $HOME a directory of the user $1 ("uid:gid"), and hands
// that user the root-owned directories strictly between $HOME and the mount
// points $2... below it (docker makes them, as root, to mount something
// there). It runs as root in the container, and only ever changes the
// container's own file system: nothing when a mount is $HOME or above it;
// never a mount point, nor anything in a mount; never through a symlink
// (it stops at one, and chown -h). It changes nothing that is right already.
const homeScript = `set -f
u=$1 uid=${1%%:*}
shift
owns() { set -- $(ls -dn -- "$1"); [ "$3" = "$uid" ]; }
for o do
	case $HOME in "$o" | "$o"/*) exit 0 ;; esac
done
if [ ! -e "$HOME" ] && [ ! -L "$HOME" ]; then
	mkdir -p -- "$HOME" || exit
fi
if [ ! -L "$HOME" ] && ! owns "$HOME"; then
	chown -h -- "$u" "$HOME" || exit
fi
for m do
	case $m in "$HOME"/*) ;; *) continue ;; esac
	d=$HOME rest=${m#"$HOME"/}
	while :; do
		case $rest in */*) ;; *) break ;; esac
		d=$d/${rest%%/*} rest=${rest#*/}
		if [ -L "$d" ] || [ ! -d "$d" ]; then break; fi
		in=
		for o do
			case $d in "$o" | "$o"/*) in=1 ;; esac
		done
		if [ -n "$in" ]; then break; fi
		if [ -O "$d" ] && ! owns "$d"; then
			chown -h -- "$u" "$d" || exit
		fi
	done
done
`

// makeHome makes the spec's $HOME, in container id, a directory the spec's
// user owns, with the root-owned directories docker made below it for mount
// points handed to that user too (homeScript). $HOME is the home dir of the
// user running atl, which the image has only when built for that path:
// atelier's images put the user's home in /home/<user>, so on a host where
// it is elsewhere (/var/home/<user>, /Users/<user>) it would be missing, or
// a root-owned directory docker made to mount something below it, and what
// writes to ~/.cache, ~/.npm or ~/.config would fail. Without $HOME or a
// user in the spec it does nothing.
func (s Spec) makeHome(ctx context.Context, run Runner, id string) error {
	home := s.Env["HOME"]
	if home == "" || s.User == "" {
		return nil
	}
	args := []string{"exec", "-u", "0", id, "sh", "-c", homeScript, "sh", s.User}
	for _, m := range s.Mounts {
		args = append(args, m.Dst)
	}
	if _, err := run(ctx, args...); err != nil {
		return fmt.Errorf("make the home dir %s in the container: %w", home, err)
	}
	return nil
}

// homesMade holds the IDs of the containers whose home makeHome made in
// this process, so that Ensure checks a container it reuses only once.
var homesMade sync.Map

// makeHomeOnce is makeHome, unless it already ran for container id.
func (s Spec) makeHomeOnce(ctx context.Context, run Runner, id string) error {
	if _, ok := homesMade.Load(id); ok {
		return nil
	}
	if err := s.makeHome(ctx, run, id); err != nil {
		return err
	}
	homesMade.Store(id, true)
	return nil
}

// Cleanup removes the task container of sandbox id (only atl's own, see
// Agent: one that merely has its name is left alone, and reported) and
// everything the task created through its gate: containers (with their
// anonymous volumes), networks and volumes. It keeps going on errors and
// returns them joined.
func Cleanup(ctx context.Context, run Runner, id string) error {
	var errs []error
	if c, err := Agent(ctx, run, id); err == nil {
		if _, err := run(ctx, "rm", "-f", c.ID); err != nil && !noSuch(err) {
			errs = append(errs, err)
		}
	} else if !noSuch(err) {
		errs = append(errs, err)
	}
	filter := "label=" + LabelTask + "=" + id
	for _, kind := range []struct{ list, remove []string }{
		{[]string{"ps", "-aq", "--filter", filter}, []string{"rm", "-f", "-v"}},
		{[]string{"network", "ls", "-q", "--filter", filter}, []string{"network", "rm"}},
		{[]string{"volume", "ls", "-q", "--filter", filter}, []string{"volume", "rm", "-f"}},
	} {
		out, err := run(ctx, kind.list...)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ids := strings.Fields(out); len(ids) > 0 {
			if _, err := run(ctx, append(kind.remove, ids...)...); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// InstallBin copies the atl binary src to <runDir>/bin/atl unless that copy
// is already the same file (size and modification time). The copy replaces
// the old one atomically, so a running container sees the new binary too.
// Next to it go the links that make atl the copy relay's tools in the
// container (wl-copy, wl-paste: see CopyMain). The run dir is atl's: the
// task container has it read-only.
func InstallBin(src, runDir string) error {
	if err := installBin(src, runDir); err != nil {
		return err
	}
	for _, name := range copyTools {
		if err := linkAtl(filepath.Join(runDir, "bin", name)); err != nil {
			return err
		}
	}
	return nil
}

// linkSeq numbers linkAtl's temp links.
var linkSeq atomic.Int64

// linkAtl makes path a symlink to atl in its own directory, replacing
// whatever is there atomically, unless it is one already. Its temp link
// is its own (by process and call): launches in one task may run at once.
func linkAtl(path string) error {
	if target, err := os.Readlink(path); err == nil && target == "atl" {
		return nil
	}
	tmp := fmt.Sprintf("%s.%d.%d.tmp", path, os.Getpid(), linkSeq.Add(1))
	os.Remove(tmp) // left by a crashed process that had this pid
	if err := os.Symlink("atl", tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func installBin(src, runDir string) error {
	dst := filepath.Join(runDir, "bin", "atl")
	sfi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if dfi, err := os.Stat(dst); err == nil && dfi.Size() == sfi.Size() && dfi.ModTime().Equal(sfi.ModTime()) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".atl.tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chtimes(tmp.Name(), sfi.ModTime(), sfi.ModTime()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
