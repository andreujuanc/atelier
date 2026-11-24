package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"text/template"
	"time"
)

// Sessions of the session scripts' tests ("7", "17", "8" in their
// comments). The scripts find a session's processes in all of /proc, so
// another run of these tests at the same time (another checkout's go test
// on the same machine) must not share them: they carry this process's PID.
// s17 ends with s7's digits, which the scripts must not take for s7.
var (
	s7  = int64(os.Getpid())*10 + 7
	s17 = s7 + pow10(len(str(s7)))
	s8  = s7 + 1
)

func str(session int64) string { return strconv.FormatInt(session, 10) }

func pow10(n int) int64 {
	p := int64(1)
	for range n {
		p *= 10
	}
	return p
}

// TestKillSessionArgs: the script docker exec runs in the container ends
// every process whose environment has ATL_SESSION=<the session>, children
// included, and nothing else (not session 17 for session 7). It is run
// here with sh, as docker exec would run it in the container.
func TestKillSessionArgs(t *testing.T) {
	args := KillSessionArgs(oldID, s7)
	if !slices.Equal(args[:4], []string{"exec", oldID, "sh", "-c"}) || args[len(args)-1] != str(s7) {
		t.Fatalf("args = %q", args)
	}
	start := func(session string) *exec.Cmd {
		// A session's process with a child (as Claude runs its tools).
		cmd := exec.Command("sh", "-c", "sleep 300 & sleep 300; wait")
		cmd.Env = append(os.Environ(), "ATL_SESSION="+session)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		return cmd
	}
	mine, other := start(str(s7)), start(str(s17))
	count := func(session string) int {
		n := 0
		dirs, _ := filepath.Glob("/proc/[0-9]*")
		for _, d := range dirs {
			if b, err := os.ReadFile(filepath.Join(d, "environ")); err == nil && slices.Contains(strings.Split(string(b), "\x00"), "ATL_SESSION="+session) {
				if st, err := os.ReadFile(filepath.Join(d, "stat")); err == nil && !strings.Contains(string(st), ") Z ") {
					n++
				}
			}
		}
		return n
	}
	// Each session's sh starts its two sleeps in its own time.
	until(func() bool { return count(str(s7)) >= 3 && count(str(s17)) >= 3 })
	if n := count(str(s7)); n < 3 {
		t.Fatalf("session 7 has %d processes, want its sh and two sleeps", n)
	}
	out, err := exec.Command("sh", append([]string{"-c", args[4]}, args[5:]...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("kill script: %v: %s", err, out)
	}
	if err := mine.Wait(); err == nil {
		t.Error("session 7's process exited normally, want killed")
	}
	until(func() bool { return count(str(s7)) == 0 }) // the kill reaches them in their own time
	if n := count(str(s7)); n != 0 {
		t.Errorf("%d processes of session 7 left", n)
	}
	if n := count(str(s17)); n < 3 || other.ProcessState != nil {
		t.Errorf("session 17 lost processes: %d left", n)
	}
}

// until waits, up to 10 seconds, for cond: what other processes do (start
// children, die of a signal) takes its own time on a loaded machine.
func until(cond func() bool) {
	for deadline := time.Now().Add(10 * time.Second); !cond() && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
}

// aliveProc reports whether process pid runs (is not gone or a zombie).
func aliveProc(pid int) bool {
	st, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	return err == nil && !strings.Contains(string(st), ") Z ")
}

// TestEndSessionCmd: the script that ends what a session left behind once
// its Claude is gone. Here the roots, which docker exec starts in a
// container (parent outside it: PPid 0), are the test's children (the
// script's third argument), and leftovers are processes of the session
// whose parent has exited. While a root of the session runs, nothing of it
// is touched; once its roots are gone, its leftovers end; other sessions
// are never touched; a root that outlives the wait (a /clear) means the
// session goes on.
func TestEndSessionCmd(t *testing.T) {
	cmd := EndSessionCmd(s7)
	if !slices.Equal(cmd[:2], []string{"sh", "-c"}) || !slices.Equal(cmd[3:], []string{"atl-end-session", "end", str(s7)}) {
		t.Fatalf("cmd = %q", cmd[3:])
	}
	env := func(session string) []string { return append(os.Environ(), "ATL_SESSION="+session) }
	root := func(session string) *exec.Cmd { // Claude, say
		c := exec.Command("sleep", "300")
		c.Env = env(session)
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Process.Kill(); c.Wait() })
		return c
	}
	leftover := func(session string) int { // a dev server Claude started in the background
		c := exec.Command("sh", "-c", "sleep 300 > /dev/null 2>&1 & echo $!")
		c.Env = env(session)
		out, err := c.Output()
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
		return pid
	}
	end := func(session string, wait int) *exec.Cmd {
		c := exec.Command("sh", "-c", cmd[2], "atl-end-session", "end", session, strconv.Itoa(os.Getpid()), strconv.Itoa(wait))
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		return c
	}
	claude7, left7 := root(str(s7)), leftover(str(s7))
	claude17, left17 := root(str(s17)), leftover(str(s17))
	claude8, left8 := root(str(s8)), leftover(str(s8))
	// Each of them runs, with its session in its environment, for the scripts to find.
	until(func() bool {
		for pid, session := range map[int]int64{claude7.Process.Pid: s7, left7: s7, claude17.Process.Pid: s17, left17: s17,
			claude8.Process.Pid: s8, left8: s8} {
			env, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
			if err != nil || !aliveProc(pid) || !slices.Contains(strings.Split(string(env), "\x00"), "ATL_SESSION="+str(session)) {
				return false
			}
		}
		return true
	})

	// Session 8 had a /clear: its Claude outlives the wait.
	clear8 := end(str(s8), 2)
	// Session 7 ends: nothing goes while its Claude still runs.
	end7 := end(str(s7), 30)
	time.Sleep(1500 * time.Millisecond)
	if !aliveProc(left7) || claude7.ProcessState != nil {
		t.Fatal("session 7 was touched while its Claude ran")
	}
	claude7.Process.Signal(syscall.SIGTERM) // Claude exits
	claude7.Wait()
	done := make(chan error, 1)
	go func() { done <- end7.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("end script: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the end script did not finish after Claude exited")
	}
	until(func() bool { return !aliveProc(left7) }) // the kill reaches it in its own time
	if aliveProc(left7) {
		t.Error("session 7's background process outlived its Claude")
	}
	if err := clear8.Wait(); err != nil {
		t.Fatalf("end script of session 8: %v", err)
	}
	for name, pid := range map[string]int{"17's Claude": claude17.Process.Pid, "17's leftover": left17,
		"8's Claude": claude8.Process.Pid, "8's leftover": left8} {
		if !aliveProc(pid) {
			t.Errorf("session %s was ended", name)
		}
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv(InEnv, "")
	t.Setenv("ATL_SANDBOX_IMAGE", "")
	// On by default: unset, empty and docker.
	t.Setenv("ATL_SANDBOX", "") // restores the old value after the test
	os.Unsetenv("ATL_SANDBOX")
	if c, err := FromEnv(); err != nil || c == nil || c.Image != DefaultImage {
		t.Errorf("unset: %+v, %v; want on", c, err)
	}
	for _, on := range []string{"", "  ", "docker", " Docker "} {
		t.Setenv("ATL_SANDBOX", on)
		if c, err := FromEnv(); err != nil || c == nil || c.Image != DefaultImage {
			t.Errorf("ATL_SANDBOX=%q: %+v, %v; want on", on, c, err)
		}
	}
	for _, off := range []string{"off", " OFF "} {
		t.Setenv("ATL_SANDBOX", off)
		if c, err := FromEnv(); c != nil || err != nil {
			t.Errorf("ATL_SANDBOX=%q: %v, %v; want off", off, c, err)
		}
	}
	t.Setenv("ATL_SANDBOX", "")
	t.Setenv("ATL_SANDBOX_IMAGE", "dev:1")
	if c, _ := FromEnv(); c == nil || c.Image != "dev:1" {
		t.Errorf("image: %+v", c)
	}
	// A typo, or an old spelling of off, picks no mode: the error names
	// the two values there are.
	for _, bad := range []string{"dokcer", "0", "none", "on"} {
		t.Setenv("ATL_SANDBOX", bad)
		if c, err := FromEnv(); err == nil || c != nil || !strings.Contains(err.Error(), "want docker") ||
			!strings.Contains(err.Error(), "or off") {
			t.Errorf("ATL_SANDBOX=%q: %+v, %v; want an error", bad, c, err)
		}
	}
	// In a task container there is nothing to sandbox.
	t.Setenv("ATL_SANDBOX", "docker")
	t.Setenv(InEnv, "x-123456")
	if c, err := FromEnv(); c != nil || err != nil {
		t.Errorf("inside: %v, %v", c, err)
	}
}

// TestDefaultImage: the default task image is the Dockerfile's task target,
// not the box image, and its build commands build that target for the user.
func TestDefaultImage(t *testing.T) {
	if DefaultImage != "atelier-task" {
		t.Errorf("DefaultImage = %q", DefaultImage)
	}
	for _, want := range []string{"docker build --target task -t atelier-task ", "--build-arg USERNAME=$(whoami)",
		"--build-arg UID=$(id -u)", "--build-arg GID=$(id -g)"} {
		if !strings.Contains(BuildDefaultImage, want) {
			t.Errorf("BuildDefaultImage lacks %q: %s", want, BuildDefaultImage)
		}
	}
	// Both work from any directory: they name the checkout, not ".".
	if !strings.HasSuffix(BuildDefaultImage, " <atelier checkout>") || strings.Contains(BuildDefaultImage, "\n") {
		t.Errorf("BuildDefaultImage: %q", BuildDefaultImage)
	}
	if BuildImages != "<atelier checkout>/build-images" {
		t.Errorf("BuildImages: %q", BuildImages)
	}
	// The script is there, executable, and builds that image by default.
	path := filepath.Join("..", "..", "build-images")
	script, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode()&0o111 != 0o111 {
		t.Errorf("build-images is not executable: %v, %v", fi, err)
	}
	if !strings.Contains(string(script), "task=${ATL_SANDBOX_IMAGE:-"+DefaultImage+"}") {
		t.Errorf("build-images does not build %s by default", DefaultImage)
	}
	run := func(_ context.Context, args ...string) (string, error) {
		if args[len(args)-1] == "gone" {
			return "", errors.New("docker image: Error response from daemon: No such image: gone:latest")
		}
		if args[len(args)-1] == "broken" {
			return "", errors.New("docker image: Cannot connect to the Docker daemon")
		}
		return "sha256:x", nil
	}
	for image, want := range map[string]bool{"here": false, "gone": true} {
		if missing, err := ImageMissing(context.Background(), run, image); missing != want || err != nil {
			t.Errorf("ImageMissing(%s) = %v, %v", image, missing, err)
		}
	}
	if missing, err := ImageMissing(context.Background(), run, "broken"); missing || err == nil {
		t.Errorf("ImageMissing without a daemon = %v, %v", missing, err)
	}
}

func TestIDDiffersPerWorkspace(t *testing.T) {
	a, b := ID("fix-login", "/ws/a/tasks/fix-login"), ID("fix-login", "/ws/b/tasks/fix-login")
	if a == b || !strings.HasPrefix(a, "fix-login-") || ContainerName(a) != "atl-"+a {
		t.Errorf("ids %q %q", a, b)
	}
	// Every task container's name is reserved (see Gate).
	for _, slug := range []string{"fix-login", "x", "a.b_c-1"} {
		if name := ContainerName(ID(slug, "/ws/tasks/"+slug)); !ReservedName(name) || !ReservedName("/"+strings.ToUpper(name)) {
			t.Errorf("%s is not reserved", name)
		}
	}
}

func testSpec() Spec {
	return Spec{ID: "t-abc123", Image: "img", User: "1000:1000", Workdir: "/tasks/t",
		Mounts: []Mount{{Src: "/tasks/t", Dst: "/tasks/t"}, {Src: "/run/x", Dst: RunMount, RO: true}},
		Env:    map[string]string{"B": "2", "A": "1"}}
}

func TestSpecRunArgs(t *testing.T) {
	s := testSpec()
	want := []string{"run", "-d", "--init", "--name", "atl-t-abc123", "--label", "atelier.agent=t-abc123",
		"--label", "atelier.spec=" + s.Hash(), "--user", "1000:1000", "--workdir", "/tasks/t",
		"--mount", "type=bind,src=/tasks/t,dst=/tasks/t", "--mount", "type=bind,src=/run/x,dst=/run/atl,readonly",
		"--env", "A=1", "--env", "B=2", "img", "sleep", "infinity"}
	if got := s.RunArgs(); !slices.Equal(got, want) {
		t.Errorf("RunArgs:\n got %q\nwant %q", got, want)
	}
	other := testSpec()
	other.Mounts = append(other.Mounts, Mount{Src: "/repos/docs", Dst: "/repos/docs", RO: true})
	if other.Hash() == s.Hash() {
		t.Error("the hash does not change with the mounts")
	}
}

func TestExecArgv(t *testing.T) {
	got := ExecArgv(oldID, "/tasks/t", []string{"ATL_TASK", "TERM"}, []string{"claude", "--resume", "x"})
	want := []string{"docker", "exec", "-it", "--detach-keys", "ctrl-^,ctrl-^,ctrl-^", "-w", "/tasks/t", "-e", "ATL_TASK", "-e", "COLORFGBG", "-e", "COLORTERM",
		"-e", "NO_COLOR", "-e", "TERM", oldID, "sh", "-c", `PATH=/run/atl/bin:$PATH WAYLAND_DISPLAY=atl-copy-relay exec "$@"`, "atl-exec",
		"claude", "--resume", "x"}
	if !slices.Equal(got, want) {
		t.Errorf("ExecArgv:\n got %q\nwant %q", got, want)
	}
}

// windowEnv runs WindowArgv(attach, cmd) with sh, as tmux would, with a
// fake tmux on PATH that logs its arguments to dir/tmux.log and answers
// the contents of dir/attached (exit 1 if there is no such file).
func windowEnv(t *testing.T, attach bool, cmd string) (dir string, run *exec.Cmd) {
	t.Helper()
	dir = t.TempDir()
	fake := "#!/bin/sh\necho \"$*\" >> " + dir + "/tmux.log\nexec cat " + dir + "/attached 2> /dev/null\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	argv := WindowArgv(attach, []string{"sh", "-c", cmd})
	run = exec.Command(argv[0], argv[1:]...)
	run.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TMUX_PANE=%7", "OUT="+dir)
	return dir, run
}

func waitForFile(t *testing.T, path string, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestWindowArgv(t *testing.T) {
	for _, c := range []struct {
		attach bool
		mode   string
	}{{true, "attach"}, {false, "detached"}} {
		got := WindowArgv(c.attach, []string{"docker", "exec", "-it", "x"})
		if !slices.Equal(got[:2], []string{"sh", "-c"}) || !slices.Equal(got[3:], []string{"atl-window", c.mode, "docker", "exec", "-it", "x"}) {
			t.Errorf("WindowArgv(%v) = %q", c.attach, got)
		}
	}
}

// TestWindowScriptWaitsForAttach: with an attach to follow, docker exec
// starts only once the window's tmux session has a client; then at once.
func TestWindowScriptWaitsForAttach(t *testing.T) {
	dir, run := windowEnv(t, true, `touch "$OUT/started"; echo "$@" > "$OUT/args"`)
	if err := os.WriteFile(filepath.Join(dir, "attached"), []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- run.Wait() }()
	if waitForFile(t, filepath.Join(dir, "started"), 700*time.Millisecond) {
		t.Fatal("started before a client attached")
	}
	if err := os.WriteFile(filepath.Join(dir, "attached"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitForFile(t, filepath.Join(dir, "started"), 2*time.Second) {
		t.Fatal("did not start once a client attached")
	}
	if err := <-done; err != nil {
		t.Errorf("exit: %v", err)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "tmux.log"))
	if first, _, _ := strings.Cut(string(log), "\n"); first != "display-message -p -t %7 #{session_attached}" {
		t.Errorf("tmux called with: %q", log)
	}
}

// TestWindowScriptStartsAtOnce: without an attach to follow, or when tmux
// cannot answer, docker exec starts at once; its exit status is the
// window's.
func TestWindowScriptStartsAtOnce(t *testing.T) {
	for _, c := range []struct {
		name   string
		attach bool
		answer bool // the fake tmux answers "0" (else it fails)
	}{{"detached", false, true}, {"attach, tmux fails", true, false}} {
		t.Run(c.name, func(t *testing.T) {
			dir, run := windowEnv(t, c.attach, `exit 3`)
			if c.answer {
				if err := os.WriteFile(filepath.Join(dir, "attached"), []byte("0\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			start := time.Now()
			err := run.Run()
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ee.ExitCode() != 3 {
				t.Errorf("exit: %v", err)
			}
			if d := time.Since(start); d > 700*time.Millisecond {
				t.Errorf("took %v", d)
			}
			log, _ := os.ReadFile(filepath.Join(dir, "tmux.log"))
			if !c.attach && len(log) > 0 {
				t.Errorf("tmux called without an attach to wait for: %q", log)
			}
		})
	}
}

// TestWindowScriptResendsSize: docker exec gets SIGWINCH after its start,
// so that it sends the window's size once more.
func TestWindowScriptResendsSize(t *testing.T) {
	dir, run := windowEnv(t, false, `trap 'echo winch >> "$OUT/winch"' WINCH; i=0; while [ $i -lt 45 ]; do sleep 0.1; i=$((i + 1)); done`)
	if err := run.Run(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "winch"))
	if n := strings.Count(string(b), "winch"); n != 2 {
		t.Errorf("SIGWINCH %d times in 4.5 s, want 2 (at 1 and 3 s)", n)
	}
}

// script is a Runner answering from a list of (prefix → output, error).
type script struct {
	calls   []string
	answers map[string]struct {
		out string
		err error
	}
}

func (s *script) run(_ context.Context, args ...string) (string, error) {
	call := strings.Join(args, " ")
	s.calls = append(s.calls, call)
	for prefix, a := range s.answers {
		if strings.HasPrefix(call, prefix) {
			return a.out, a.err
		}
	}
	return "", nil
}

func (s *script) answer(prefix, out string, err error) {
	if s.answers == nil {
		s.answers = map[string]struct {
			out string
			err error
		}{}
	}
	s.answers[prefix] = struct {
		out string
		err error
	}{out, err}
}

// agentDaemon is a Runner for Ensure: a daemon with at most the one
// container named atl-<id>.
type agentDaemon struct {
	t      *testing.T
	c      *container // nil: none
	calls  []string
	runErr error      // what docker run fails with
	winner *container // the container another atl made when run fails with a name conflict
	// noHome: $HOME is not a directory the container's user can write to
	// (docker exec as that user fails), until made as root; rootErr is what
	// docker exec as root fails with.
	noHome  bool
	rootErr error
	execs   [][]string // the docker exec calls
}

const (
	oldID = "0000000000000000000000000000000000000000000000000000000000000001"
	newID = "0000000000000000000000000000000000000000000000000000000000000002"
)

func (d *agentDaemon) run(_ context.Context, args ...string) (string, error) {
	d.calls = append(d.calls, strings.Join(args, " "))
	switch args[0] {
	case "inspect":
		if d.c == nil {
			return "", errors.New("docker inspect: Error response from daemon: No such container: atl-t-abc123")
		}
		return d.c.inspect(d.t, args), nil
	case "run":
		if d.runErr != nil {
			d.c = d.winner
			return "", d.runErr
		}
		d.c = &container{Id: newID}
		d.c.State.Running = true
		d.c.Config.Labels = map[string]string{}
		for i, a := range args {
			if a == "--label" {
				k, v, _ := strings.Cut(args[i+1], "=")
				d.c.Config.Labels[k] = v
			}
		}
		return newID, nil
	case "rm":
		if d.c != nil && args[len(args)-1] == d.c.Id {
			d.c = nil
		}
	case "start":
		if d.c != nil && args[len(args)-1] == d.c.Id {
			d.c.State.Running = true
		}
	case "exec":
		d.execs = append(d.execs, args)
		if args[1] != "-u" {
			if d.noHome {
				return "", errors.New("docker exec: exit status 1")
			}
		} else if d.rootErr != nil {
			return "", d.rootErr
		} else {
			d.noHome = false
		}
	}
	return "", nil
}

func (d *agentDaemon) verbs() string {
	var v []string
	for _, c := range d.calls {
		first, _, _ := strings.Cut(c, " ")
		v = append(v, first)
	}
	return strings.Join(v, " ")
}

// atlsContainer is a container atl made for spec, with spec label hash.
func atlsContainer(spec Spec, running bool, hash string) *container {
	c := &container{Id: oldID}
	c.State.Running = running
	c.Config.Labels = map[string]string{LabelAgent: spec.ID, labelSpec: hash}
	return c
}

func TestEnsure(t *testing.T) {
	spec := testSpec()
	ctx := context.Background()
	busy := func() bool { return true }
	idle := func() bool { return false }

	for _, tc := range []struct {
		name      string
		c         *container
		busy      func() bool
		want      string
		wantID    string
		wantStale bool
	}{
		{"missing: create", nil, idle, "inspect run", newID, false},
		{"running, same spec: nothing", atlsContainer(spec, true, spec.Hash()), busy, "inspect", oldID, false},
		{"stopped, same spec: start", atlsContainer(spec, false, spec.Hash()), idle, "inspect start", oldID, false},
		{"other spec, idle: replace", atlsContainer(spec, true, "old"), idle, "inspect rm run", newID, false},
		{"other spec, sessions running: keep", atlsContainer(spec, true, "old"), busy, "inspect", oldID, true},
		{"other spec but stopped: replace", atlsContainer(spec, false, "old"), busy, "inspect rm run", newID, false},
	} {
		d := &agentDaemon{t: t, c: tc.c}
		// fresh runs right before each docker run, after any docker rm.
		fresh := func() error {
			d.calls = append(d.calls, "fresh")
			return nil
		}
		id, stale, err := Ensure(ctx, d.run, spec, tc.busy, fresh)
		want := strings.ReplaceAll(tc.want, "run", "fresh run")
		if err != nil || stale != tc.wantStale || d.verbs() != want || id != tc.wantID {
			t.Errorf("%s: calls %q stale %v id %.4s… err %v; want %q stale %v id %.4s…", tc.name, d.verbs(), stale, id, err, want, tc.wantStale, tc.wantID)
		}
		// By ID only, once checked.
		for _, c := range d.calls {
			if strings.HasPrefix(c, "rm ") && !strings.HasSuffix(c, " "+oldID) || strings.HasPrefix(c, "start ") && !strings.HasSuffix(c, " "+oldID) {
				t.Errorf("%s: %q", tc.name, c)
			}
		}
	}

	// Two atl at once: the loser of the name race uses the winner's
	// container, if it is atl's.
	conflict := errors.New(`docker run: Conflict. The container name "/atl-t-abc123" is already in use`)
	d := &agentDaemon{t: t, runErr: conflict, winner: atlsContainer(spec, false, spec.Hash())}
	if id, _, err := Ensure(ctx, d.run, spec, idle, nil); err != nil || d.verbs() != "inspect run inspect start" || id != oldID {
		t.Errorf("name race: %q, %q, %v", d.verbs(), id, err)
	}
	d = &agentDaemon{t: t, runErr: conflict, winner: &container{Id: oldID}}
	if id, _, err := Ensure(ctx, d.run, spec, idle, nil); !errors.Is(err, ErrNotAgent) || d.verbs() != "inspect run inspect" || id != "" {
		t.Errorf("name race won by a container not atl's: %q, %q, %v", d.verbs(), id, err)
	}
	// Any other failure is reported.
	d = &agentDaemon{t: t, runErr: errors.New("docker run: pull access denied for img")}
	if _, _, err := Ensure(ctx, d.run, spec, idle, nil); err == nil {
		t.Error("a failed docker run was not reported")
	}
	// A failed fresh stops the creation.
	d = &agentDaemon{t: t}
	if _, _, err := Ensure(ctx, d.run, spec, idle, func() error { return errors.New("no") }); err == nil || d.verbs() != "inspect" {
		t.Errorf("failed fresh: %q, %v", d.verbs(), err)
	}
}

// TestEnsureMakesHome: $HOME is the home dir of the user running atl,
// which the image has only when built for that path (atelier's images put
// it in /home/<user>; a host may have it in /var/home/<user>). A new
// container whose $HOME is not a directory its user can write to gets one,
// made as root and handed to the user; one where it is gets nothing. If
// that fails, the container is removed, so that the next try makes it again.
func TestEnsureMakesHome(t *testing.T) {
	spec := testSpec()
	spec.Env["HOME"] = "/var/home/u"
	ctx := context.Background()
	idle := func() bool { return false }

	d := &agentDaemon{t: t}
	if id, _, err := Ensure(ctx, d.run, spec, idle, nil); err != nil || d.verbs() != "inspect run exec" || id != newID {
		t.Fatalf("home usable: %q, %.4s…, %v", d.verbs(), id, err)
	}
	if got := d.execs[0][:2]; !slices.Equal(got, []string{"exec", newID}) {
		t.Errorf("the check runs %q, want it as the container's user, in the new container", got)
	}

	d = &agentDaemon{t: t, noHome: true}
	if id, _, err := Ensure(ctx, d.run, spec, idle, nil); err != nil || d.verbs() != "inspect run exec exec" || id != newID || d.noHome {
		t.Fatalf("home missing: %q, %.4s…, %v, still missing %v", d.verbs(), id, err, d.noHome)
	}
	root := d.execs[1]
	if !slices.Equal(root[:4], []string{"exec", "-u", "0", newID}) || root[len(root)-1] != spec.User {
		t.Fatalf("made with %q, want as root in the new container, for %s", root, spec.User)
	}
	// The scripts do what they say, with a real sh: the check fails on a
	// missing home, the root script makes it (here owned by the user
	// running the test, root being out of reach), and the check passes.
	home := filepath.Join(t.TempDir(), "var", "home", "u")
	sh := func(argv []string) error {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		return nil
	}
	check := d.execs[0][2:]
	if err := sh(check); err == nil {
		t.Error("the check passes on a missing home")
	}
	owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	if err := sh(append(slices.Clone(root[4:len(root)-1]), owner)); err != nil {
		t.Fatalf("the root script: %v", err)
	}
	if err := sh(check); err != nil {
		t.Errorf("the check fails on the home made: %v", err)
	}

	d = &agentDaemon{t: t, noHome: true, rootErr: errors.New("docker exec: chown: not permitted")}
	if _, _, err := Ensure(ctx, d.run, spec, idle, nil); err == nil || !strings.Contains(err.Error(), "/var/home/u") ||
		d.verbs() != "inspect run exec exec rm" || d.c != nil {
		t.Errorf("home not made: %q, %v; want an error that names it, and the container removed", d.verbs(), err)
	}

	// Without $HOME in the spec, nothing to check.
	d = &agentDaemon{t: t, noHome: true}
	if _, _, err := Ensure(ctx, d.run, testSpec(), idle, nil); err != nil || d.verbs() != "inspect run" {
		t.Errorf("no $HOME: %q, %v", d.verbs(), err)
	}
}

// container is a fake daemon's only container, for Runners that answer
// docker inspect --format as Docker does.
type container struct {
	Id     string
	State  struct{ Running bool }
	Config struct{ Labels map[string]string }
}

// inspectFormat answers `docker inspect ... --format F <name>` for c.
func (c *container) inspect(t *testing.T, args []string) string {
	t.Helper()
	f := ""
	for i, a := range args {
		if a == "--format" || a == "-f" {
			f = args[i+1]
		}
	}
	tmpl, err := template.New("f").Funcs(template.FuncMap{"json": func(v any) string {
		b, _ := json.Marshal(v)
		return string(b)
	}}).Parse(f)
	if err != nil {
		t.Fatalf("format %q: %v", f, err)
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, c); err != nil {
		t.Fatalf("format %q: %v", f, err)
	}
	return b.String()
}

// TestEnsureAdoptsOnlyItsOwn: a container that merely has the task
// container's name is not adopted, replaced or removed: only one that
// carries atelier.agent=<id> and no atelier.task label is atl's (a task
// cannot create one through its gate, but a container takes its image's
// labels). Ensure fails with one line that names it.
func TestEnsureAdoptsOnlyItsOwn(t *testing.T) {
	spec := testSpec()
	const cid = "9999999999999999999999999999999999999999999999999999999999999999"
	for name, labels := range map[string]map[string]string{
		"no labels":            {},
		"another task's agent": {LabelAgent: "t-other1", labelSpec: spec.Hash()},
		"a task's container":   {LabelAgent: spec.ID, LabelTask: "t-other1", labelSpec: spec.Hash()},
		"a task's, other spec": {LabelAgent: spec.ID, LabelTask: "t-other1", labelSpec: "old"},
	} {
		c := &container{Id: cid}
		c.State.Running = true
		c.Config.Labels = labels
		var calls []string
		run := func(_ context.Context, args ...string) (string, error) {
			calls = append(calls, args[0])
			if args[0] == "inspect" {
				return c.inspect(t, args), nil
			}
			return "", nil
		}
		got, _, err := Ensure(context.Background(), run, spec, func() bool { return false }, nil)
		if err == nil || got != "" {
			t.Errorf("%s: adopted (%q, %v)", name, got, err)
			continue
		}
		if msg := err.Error(); !strings.Contains(msg, "atl-"+spec.ID) || strings.Contains(msg, "\n") {
			t.Errorf("%s: error %q; want one line naming the container", name, msg)
		}
		if !slices.Equal(calls, []string{"inspect"}) {
			t.Errorf("%s: docker calls %q; want the inspect only (nothing removed or started)", name, calls)
		}
	}
	// Its own: adopted by ID, started if stopped.
	c := &container{Id: cid}
	c.Config.Labels = map[string]string{LabelAgent: spec.ID, labelSpec: spec.Hash()}
	var calls []string
	run := func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "inspect" {
			return c.inspect(t, args), nil
		}
		return "", nil
	}
	got, stale, err := Ensure(context.Background(), run, spec, nil, nil)
	if err != nil || stale || got != cid || calls[len(calls)-1] != "start "+cid {
		t.Errorf("own container: %q %v %v, calls %q", got, stale, err, calls)
	}
}

func TestCleanup(t *testing.T) {
	inspect := "inspect --type container --format {{.Id}} {{.State.Running}} {{json .Config.Labels}} atl-t-abc123"
	rest := []string{
		"ps -aq --filter label=atelier.task=t-abc123",
		"rm -f -v c1 c2",
		"network ls -q --filter label=atelier.task=t-abc123",
		"volume ls -q --filter label=atelier.task=t-abc123",
		"volume rm -f v1",
	}
	for _, tc := range []struct {
		name, out string
		err       error
		want      []string
		wantErr   bool
	}{
		{"atl's container", oldID + ` true {"atelier.agent":"t-abc123","atelier.spec":"x"}`, nil,
			append([]string{inspect, "rm -f " + oldID}, rest...), false},
		{"none", "", errors.New("docker inspect: Error: No such container: atl-t-abc123"), append([]string{inspect}, rest...), false},
		// One that merely has the name is not atl's to remove: left, and reported.
		{"another's of that name", oldID + ` true {"atelier.agent":"t-abc123","atelier.task":"t-other1"}`, nil,
			append([]string{inspect}, rest...), true},
	} {
		s := &script{}
		s.answer("inspect", tc.out, tc.err)
		s.answer("ps", "c1\nc2", nil)
		s.answer("network ls", "", nil)
		s.answer("volume ls", "v1", nil)
		err := Cleanup(context.Background(), s.run, "t-abc123")
		if (err != nil) != tc.wantErr || tc.wantErr && !errors.Is(err, ErrNotAgent) {
			t.Errorf("%s: Cleanup: %v", tc.name, err)
		}
		if !slices.Equal(s.calls, tc.want) {
			t.Errorf("%s: calls:\n got %q\nwant %q", tc.name, s.calls, tc.want)
		}
	}
	// A failure does not stop the rest, and is reported.
	s := &script{}
	s.answer("inspect", "", errors.New("docker inspect: Error: No such container: atl-t-abc123"))
	s.answer("ps", "", errors.New("docker ps: daemon down"))
	s.answer("volume ls", "v1", nil)
	if err := Cleanup(context.Background(), s.run, "t-abc123"); err == nil || !slices.Contains(s.calls, "volume rm -f v1") {
		t.Errorf("after a failure: err %v, calls %q", err, s.calls)
	}
}

func TestInstallBin(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "atl")
	if err := os.WriteFile(src, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(dir, "run")
	dst := filepath.Join(run, "bin", "atl")
	if err := InstallBin(src, run); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dst)
	if err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("installed copy: %v %v", fi, err)
	}
	// Unchanged: the copy is left alone (same inode).
	if err := InstallBin(src, run); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.Stat(dst); !os.SameFile(fi, again) {
		t.Error("an unchanged binary was copied again")
	}
	// Changed: replaced.
	if err := os.WriteFile(src, []byte("version two"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := InstallBin(src, run); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "version two" {
		t.Errorf("copy = %q", b)
	}
	// The copy relay's tools are links to it, also put back when something
	// else is in their place.
	links := func() {
		t.Helper()
		for _, name := range []string{"wl-copy", "wl-paste"} {
			path := filepath.Join(run, "bin", name)
			if target, err := os.Readlink(path); err != nil || target != "atl" {
				t.Errorf("%s: link to %q, %v; want a link to atl", name, target, err)
			}
			if b, _ := os.ReadFile(path); string(b) != "version two" {
				t.Errorf("%s reads %q, want the atl copy", name, b)
			}
		}
	}
	links()
	if err := os.Remove(filepath.Join(run, "bin", "wl-copy")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "bin", "wl-copy"), []byte("other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := InstallBin(src, run); err != nil {
		t.Fatal(err)
	}
	links()
	if entries, _ := os.ReadDir(filepath.Join(run, "bin")); len(entries) != 3 {
		t.Errorf("bin has %d entries, want atl and its two links", len(entries))
	}
}

// Launches in one task, from several atl processes, install the binary at
// once: each gets the copy and its links, and no temp file is left.
func TestInstallBinConcurrent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "atl")
	if err := os.WriteFile(src, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 50; round++ {
		run := filepath.Join(dir, fmt.Sprintf("run-%d", round))
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := InstallBin(src, run); err != nil {
					errs <- err
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("round %d: %v", round, err)
		}
		entries, _ := os.ReadDir(filepath.Join(run, "bin"))
		if len(entries) != 3 {
			t.Fatalf("round %d: bin has %v, want atl and its two links", round, entries)
		}
	}
}

// TestBuildImagesScript: build-images builds the box, then the task image,
// with its arguments passed on; ATL_BUILD_BOX=0 builds only the task image,
// and another value is refused. A fake docker records the calls.
func TestBuildImagesScript(t *testing.T) {
	script, err := filepath.Abs(filepath.Join("..", "..", "build-images"))
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	fake := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(box string) ([]string, string, error) {
		os.Remove(log)
		cmd := exec.Command("sh", script, "--no-cache")
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "ATL_BOX_IMAGE=", "ATL_SANDBOX_IMAGE=", "ATL_BUILD_BOX="+box)
		out, err := cmd.CombinedOutput()
		b, _ := os.ReadFile(log)
		var calls []string
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if l != "" {
				calls = append(calls, l)
			}
		}
		return calls, string(out), err
	}
	calls, out, err := run("")
	if err != nil || len(calls) != 2 || !strings.HasPrefix(calls[0], "build -t atelier ") || !strings.HasPrefix(calls[1], "build --target task -t atelier-task ") ||
		!strings.Contains(calls[0], " --no-cache ") || !strings.Contains(calls[1], " --no-cache ") || !strings.Contains(out, "built atelier (box) and atelier-task (task image)") {
		t.Errorf("default: %q %q %v", calls, out, err)
	}
	calls, out, err = run("0")
	if err != nil || len(calls) != 1 || !strings.HasPrefix(calls[0], "build --target task -t atelier-task ") ||
		strings.TrimSpace(out) != "built atelier-task (task image)" {
		t.Errorf("ATL_BUILD_BOX=0: %q %q %v", calls, out, err)
	}
	calls, out, err = run("no")
	if err == nil || len(calls) != 0 || !strings.Contains(out, "ATL_BUILD_BOX must be 0 or 1") {
		t.Errorf("ATL_BUILD_BOX=no: %q %q %v", calls, out, err)
	}
}
