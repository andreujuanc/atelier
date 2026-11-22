package sandbox

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
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
}
