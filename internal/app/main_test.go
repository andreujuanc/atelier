package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain isolates git from the user's global/system config and supplies
// an identity so commits work without any global setup. It first clears
// every ATL_* variable: run inside an atelier task (or its container), the
// outer task's ATL_TASK, ATL_SANDBOX_ID, ... would change what atl does.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "app-test-git")
	if err != nil {
		panic(err)
	}
	cfg := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(cfg, nil, 0o644); err != nil {
		panic(err)
	}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "ATL_") {
			os.Unsetenv(k)
		}
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL":   cfg,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME":     "Test",
		"GIT_AUTHOR_EMAIL":    "test@example.com",
		"GIT_COMMITTER_NAME":  "Test",
		"GIT_COMMITTER_EMAIL": "test@example.com",
		// No global Claude Code settings (a global statusLine would keep
		// atelier's out of task settings, see gen.WriteSettings).
		"CLAUDE_CONFIG_DIR": filepath.Join(dir, "claude"),
		// The sandbox is on by default. These tests build Apps without it
		// (sessions run next to atl) and doctor must say the same, as with
		// ATL_SANDBOX=off; the sandbox tests set what they need.
		"ATL_SANDBOX": "off",
		// No other workspaces (the developer's real ones would shift the
		// port offsets); the cross-workspace tests set their own root.
		"ATL_WORKSPACES": filepath.Join(dir, "workspaces"),
	} {
		os.Setenv(k, v)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
