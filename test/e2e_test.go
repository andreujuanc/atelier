// Package e2e runs the end-to-end script test/e2e.sh as a Go test.
package e2e

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	// atl's packages, imported only so that Go's test cache invalidates this
	// test's result when they change (the script builds atl from source).
	_ "github.com/andreujuanc/atelier/internal/cli"
	_ "github.com/andreujuanc/atelier/internal/hook"
)

// inputs are files the script depends on outside atl's imported packages.
// Reading them makes Go's test cache track their contents.
var inputs = []string{"e2e.sh", "stubs/claude", "stubs/gh", "../cmd/atl/main.go"}

// TestE2E runs e2e.sh (skipped under -short or when a tool it needs is
// missing) and fails with its output if it exits non-zero.
func TestE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test in -short mode")
	}
	for _, tool := range []string{"bash", "git", "tmux", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	_, jqErr := exec.LookPath("jq")
	_, pyErr := exec.LookPath("python3")
	if jqErr != nil && pyErr != nil {
		t.Skip("need jq or python3")
	}
	for _, f := range inputs {
		if _, err := os.ReadFile(f); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "e2e.sh")
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("e2e.sh timed out:\n%s", out)
	}
	if err != nil {
		t.Fatalf("e2e.sh: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}
