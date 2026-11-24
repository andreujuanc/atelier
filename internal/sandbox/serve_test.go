package sandbox

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain lets TestStartReplacesStaleGate run the test binary as
// `atl gate <slug> [--ro-mount P]... --task-id N`: a gate for task N on
// $GATE_RUN_DIR.
func TestMain(m *testing.M) {
	if os.Getenv("ATL_SANDBOX_TEST_GATE") == "1" {
		id, _ := strconv.ParseInt(os.Args[len(os.Args)-1], 10, 64)
		var roMounts []string
		for i, a := range os.Args {
			if a == "--ro-mount" && i+1 < len(os.Args) {
				roMounts = append(roMounts, os.Args[i+1])
			}
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
		defer stop()
		g := &Gate{ID: testID, Root: os.Getenv("GATE_ROOT"), Upstream: "/nonexistent", Ident: Ident(id, os.Args[0], roMounts), ROMounts: roMounts}
		if err := Serve(ctx, g, &Control{Slug: "t1", TaskID: id, Atl: "/bin/false"}, os.Getenv("GATE_RUN_DIR")); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	// The tests do not see the atl task they may run in (go test in a task
	// container has ATL_SANDBOX_ID, ATL_SESSION, ...); the gate above gets
	// what TestStartReplacesStaleGate sets.
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "ATL_") {
			os.Unsetenv(k)
		}
	}
	os.Exit(m.Run())
}

func readIdent(t *testing.T, runDir string) (string, string) {
	t.Helper()
	ident, _ := os.ReadFile(identPath(runDir))
	pid, _ := os.ReadFile(pidPath(runDir))
	return strings.TrimSpace(string(ident)), strings.TrimSpace(string(pid))
}

// TestStartReplacesStaleGate: Start keeps a gate that serves the same task
// from the same atl binary with the same read-only mounts, and replaces one
// that serves another task id, runs another binary or has other read-only
// mounts.
func TestStartReplacesStaleGate(t *testing.T) {
	dir, err := os.MkdirTemp("", "gate") // short: socket paths
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	runDir := filepath.Join(dir, "run")
	t.Setenv("ATL_SANDBOX_TEST_GATE", "1")
	t.Setenv("GATE_ROOT", t.TempDir())
	t.Setenv("GATE_RUN_DIR", runDir)
	// The "atl binary": a copy of the test binary, so it can change.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "atl")
	if err := os.WriteFile(bin, data, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "gate.log")
	t.Cleanup(func() { Stop(runDir); awaitUnlocked(runDir, 15*time.Second) })

	if err := Start(bin, "t1", 1, runDir, logPath, nil); err != nil {
		t.Fatal(err)
	}
	ident1, pid1 := readIdent(t, runDir)
	if ident1 != Ident(1, bin, nil) || pid1 == "" {
		t.Fatalf("ident %q pid %q, want %q", ident1, pid1, Ident(1, bin, nil))
	}
	// Same task, same binary: kept.
	if err := Start(bin, "t1", 1, runDir, logPath, nil); err != nil {
		t.Fatal(err)
	}
	if _, pid := readIdent(t, runDir); pid != pid1 {
		t.Errorf("gate replaced although nothing changed: pid %s → %s", pid1, pid)
	}
	// Another task id: replaced.
	if err := Start(bin, "t1", 2, runDir, logPath, nil); err != nil {
		t.Fatal(err)
	}
	ident2, pid2 := readIdent(t, runDir)
	if ident2 != Ident(2, bin, nil) || pid2 == pid1 || !Alive(runDir) {
		t.Errorf("after another task id: ident %q pid %s (was %s), alive %v", ident2, pid2, pid1, Alive(runDir))
	}
	// A new atl binary: replaced.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(bin, later, later); err != nil {
		t.Fatal(err)
	}
	if err := Start(bin, "t1", 2, runDir, logPath, nil); err != nil {
		t.Fatal(err)
	}
	ident3, pid3 := readIdent(t, runDir)
	if ident3 != Ident(2, bin, nil) || ident3 == ident2 || pid3 == pid2 || !Alive(runDir) {
		t.Errorf("after a new binary: ident %q pid %s (was %q, %s)", ident3, pid3, ident2, pid2)
	}
	// Read-only mounts listed: replaced, and the new gate has them (its
	// arguments); the same list again: kept; another list: replaced; none
	// again: replaced.
	pid := pid3
	for i, step := range []struct {
		mounts  []string
		replace bool
	}{
		{[]string{"/srv/a", "/srv/b"}, true},
		{[]string{"/srv/a", "/srv/b"}, false},
		{[]string{"/srv/a"}, true},
		{nil, true},
	} {
		if err := Start(bin, "t1", 2, runDir, logPath, step.mounts); err != nil {
			t.Fatal(err)
		}
		ident, now := readIdent(t, runDir)
		if ident != Ident(2, bin, step.mounts) || (now != pid) != step.replace || !Alive(runDir) {
			t.Errorf("step %d, mounts %q: ident %q pid %s (was %s), want replaced %v", i, step.mounts, ident, now, pid, step.replace)
		}
		cmdline, _ := os.ReadFile("/proc/" + now + "/cmdline")
		args := strings.Join(strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")[1:], " ")
		want := "gate t1 --task-id 2"
		if len(step.mounts) > 0 {
			want = "gate t1 --ro-mount " + strings.Join(step.mounts, " --ro-mount ") + " --task-id 2"
		}
		if args != want {
			t.Errorf("step %d: gate runs with %q, want %q", i, args, want)
		}
		pid = now
	}
	if Ident(2, bin, []string{"/a", "/b"}) == Ident(2, bin, []string{"/a/b"}) || Ident(2, bin, nil) != Ident(2, bin, []string{}) {
		t.Error("Ident does not tell the lists apart")
	}
	// StopAndWait returns once the gate has stopped serving.
	if err := StopAndWait(runDir, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	if locked(runDir) || Alive(runDir) {
		t.Errorf("gate %s still serves after StopAndWait", pid)
	}
	if err := StopAndWait(filepath.Join(dir, "none"), time.Second); err != nil {
		t.Errorf("StopAndWait without a gate: %v", err)
	}
}

// TestServeRunDirIsPrivate: the run dir is the owner's only (an older atl
// made it 0755), so no other user of the host reaches the gate's sockets
// in it; the Docker socket itself stays open to any UID, as the containers
// a task bind-mounts it into (testcontainers, localstack) may run as
// another one, and the dir is not on their path to it.
func TestServeRunDirIsPrivate(t *testing.T) {
	_, upstream := newDaemon(t)
	runDir := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, &Gate{ID: testID, Root: t.TempDir(), Upstream: upstream}, &Control{Slug: "t1", TaskID: 1, Atl: "/bin/false"}, runDir)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !Alive(runDir) {
		if time.Now().After(deadline) {
			t.Fatal("gate never came up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for path, want := range map[string]os.FileMode{runDir: os.ModeDir | 0o700, SocketPath(runDir): os.ModeSocket | 0o666} {
		if fi, err := os.Stat(path); err != nil {
			t.Error(err)
		} else if fi.Mode() != want {
			t.Errorf("%s: mode %v, want %v", path, fi.Mode(), want)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestStopStalePidFile: a gate killed with SIGKILL (or a reboot) leaves its
// pid file. Stop must not signal the process that pid names now, even an
// `atl gate` (another task's gate, after the pid was reused): no gate holds
// the run dir's lock.
func TestStopStalePidFile(t *testing.T) {
	runDir := t.TempDir()
	// Another gate: a process whose command line has a "gate" argument.
	other := exec.Command("/bin/sh", "-c", "sleep 30", "atl", "gate", "other")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- other.Wait() }()
	t.Cleanup(func() { other.Process.Kill(); <-exited })
	if err := os.WriteFile(pidPath(runDir), []byte(strconv.Itoa(other.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, lockFile := range []bool{false, true} { // the dead gate's lock file is there, or not
		if lockFile {
			if err := os.WriteFile(lockPath(runDir), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		Stop(runDir)
		select {
		case err := <-exited:
			exited <- err
			t.Fatalf("Stop (lock file %v) signalled the process a stale pid file names: %v", lockFile, err)
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// openFDs counts this process's open file descriptors.
func openFDs(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("no /proc/self/fd")
	}
	return len(ents)
}

// settles waits until f() is at most want, and returns its last value.
func settles(f func() int, want int) int {
	n := f()
	for deadline := time.Now().Add(5 * time.Second); n > want && time.Now().Before(deadline); n = f() {
		time.Sleep(20 * time.Millisecond)
	}
	return n
}

// steady returns f() once it has not changed for 100ms (or after 5s).
func steady(f func() int) int {
	n := f()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		time.Sleep(100 * time.Millisecond)
		m := f()
		if m == n {
			break
		}
		n = m
	}
	return n
}

// TestGateLeaksNothing: a gate's requests (forwarded, refused, attach
// streams whose client or container hangs up, control requests) leave no
// goroutine or file descriptor behind once they are over, and none is left
// once Serve has returned.
func TestGateLeaksNothing(t *testing.T) {
	d, upstream := newDaemon(t)
	runDir := filepath.Join(t.TempDir(), "run")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := &Gate{ID: testID, Root: t.TempDir(), Upstream: upstream}
	baseG, baseFD := runtime.NumGoroutine(), openFDs(t)
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, g, &Control{Slug: "t1", TaskID: 1, Atl: "/bin/false"}, runDir)
	}()
	for deadline := time.Now().Add(5 * time.Second); !Alive(runDir); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("gate never came up")
		}
	}
	round := func() {
		for _, req := range []string{
			"GET /v1.45/containers/json HTTP/1.1\r\nHost: d\r\nConnection: close\r\n\r\n",
			"GET /v1.45/containers/mine/json HTTP/1.1\r\nHost: d\r\nConnection: close\r\n\r\n",
			"GET /v1.45/containers/host/json HTTP/1.1\r\nHost: d\r\nConnection: close\r\n\r\n",
			"POST /v1.45/swarm/init HTTP/1.1\r\nHost: d\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
			"GET /v1.45/containers/json HTTP/1.1\r\nHost: d\r\n\r\n", // keep-alive, then hung up
		} {
			conn, err := net.Dial("unix", SocketPath(runDir))
			if err != nil {
				t.Fatal(err)
			}
			io.WriteString(conn, req)
			if resp, err := http.ReadResponse(bufio.NewReader(conn), nil); err != nil {
				t.Fatal(err)
			} else {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			conn.Close()
		}
		// Attach streams: one the client hangs up, one the container does.
		for i := 0; i < 2; i++ {
			conn, err := net.Dial("unix", SocketPath(runDir))
			if err != nil {
				t.Fatal(err)
			}
			io.WriteString(conn, "POST /v1.45/containers/mine/attach?stream=1&stdout=1 HTTP/1.1\r\nHost: d\r\n"+
				"Connection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			if resp, err := http.ReadResponse(bufio.NewReader(conn), nil); err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
				t.Fatalf("attach: %v %v", resp, err)
			}
			d.mu.Lock()
			container := d.attached[len(d.attached)-1]
			d.mu.Unlock()
			if i == 0 {
				// The gate hangs up on the container too.
				conn.Close()
				container.SetReadDeadline(time.Now().Add(5 * time.Second))
				if _, err := io.Copy(io.Discard, container); err != nil {
					t.Fatalf("the container's end of an attach its client left: %v", err)
				}
				container.Close()
				continue
			}
			container.Close()
			io.Copy(io.Discard, conn)
			conn.Close()
		}
		// The control socket.
		if conn, err := net.Dial("unix", ControlSocketPath(runDir)); err == nil {
			io.WriteString(conn, "{}\n")
			io.Copy(io.Discard, conn)
			conn.Close()
		} else {
			t.Fatal(err)
		}
	}
	round()
	afterOne, fdOne := steady(runtime.NumGoroutine), steady(func() int { return openFDs(t) }) // what one round leaves to its end
	for i := 0; i < 20; i++ {
		round()
	}
	if n := settles(runtime.NumGoroutine, afterOne); n > afterOne {
		t.Errorf("goroutines grow with the requests: %d after one round, %d after 21", afterOne, n)
	}
	if n := settles(func() int { return openFDs(t) }, fdOne); n > fdOne {
		t.Errorf("open fds grow with the requests: %d after one round, %d after 21", fdOne, n)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Kept for the next request in a gate process, which exits now.
	if g.client != nil {
		g.client.CloseIdleConnections()
	}
	if n := settles(runtime.NumGoroutine, baseG); n > baseG {
		buf := make([]byte, 1<<20)
		t.Errorf("%d goroutines after Serve returned, %d before:\n%s", n, baseG, buf[:runtime.Stack(buf, true)])
	}
	if n := settles(func() int { return openFDs(t) }, baseFD); n > baseFD {
		t.Errorf("%d open fds after Serve returned, %d before", n, baseFD)
	}
}
