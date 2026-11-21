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
// `atl gate <slug> --task-id N`: a gate for task N on $GATE_RUN_DIR.
func TestMain(m *testing.M) {
	if os.Getenv("ATL_SANDBOX_TEST_GATE") == "1" {
		id, _ := strconv.ParseInt(os.Args[len(os.Args)-1], 10, 64)
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
		defer stop()
		g := &Gate{ID: testID, Root: os.Getenv("GATE_ROOT"), Upstream: "/nonexistent", Ident: Ident(id, os.Args[0])}
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
// from the same atl binary, and replaces one that serves another task id or
// runs another binary.
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

	if err := Start(bin, "t1", 1, runDir, logPath); err != nil {
		t.Fatal(err)
	}
	ident1, pid1 := readIdent(t, runDir)
	if ident1 != Ident(1, bin) || pid1 == "" {
		t.Fatalf("ident %q pid %q, want %q", ident1, pid1, Ident(1, bin))
	}
	// Same task, same binary: kept.
	if err := Start(bin, "t1", 1, runDir, logPath); err != nil {
		t.Fatal(err)
	}
	if _, pid := readIdent(t, runDir); pid != pid1 {
		t.Errorf("gate replaced although nothing changed: pid %s → %s", pid1, pid)
	}
	// Another task id: replaced.
	if err := Start(bin, "t1", 2, runDir, logPath); err != nil {
		t.Fatal(err)
	}
	ident2, pid2 := readIdent(t, runDir)
	if ident2 != Ident(2, bin) || pid2 == pid1 || !Alive(runDir) {
		t.Errorf("after another task id: ident %q pid %s (was %s), alive %v", ident2, pid2, pid1, Alive(runDir))
	}
	// A new atl binary: replaced.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(bin, later, later); err != nil {
		t.Fatal(err)
	}
	if err := Start(bin, "t1", 2, runDir, logPath); err != nil {
		t.Fatal(err)
	}
	ident3, pid3 := readIdent(t, runDir)
	if ident3 != Ident(2, bin) || ident3 == ident2 || pid3 == pid2 || !Alive(runDir) {
		t.Errorf("after a new binary: ident %q pid %s (was %q, %s)", ident3, pid3, ident2, pid2)
	}
}
