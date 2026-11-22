package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/andreujuanc/atelier/internal/workspace"
)

// TestWindowEnv: in a workspace, every tmux window atl creates gets the
// workspace's variables (empty when unset), the window's own env winning.
func TestWindowEnv(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "alpha")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATL_WORKSPACE", dir)
	t.Setenv("ATL_REPOS", "/r")
	t.Setenv("ATL_CONTEXT", "")
	t.Setenv("GIT_CONFIG_GLOBAL", "")
	a := &App{}
	if got := a.windowEnv(map[string]string{"X": "1"}); len(got) != 1 {
		t.Errorf("without a workspace: %v", got)
	}
	a.Workspace = &workspace.Applied{}
	got := a.windowEnv(map[string]string{"ATL_TASK": "t", "ATL_REPOS": "/project"})
	for k, want := range map[string]string{"ATL_WORKSPACE": dir, "ATL_REPOS": "/project", "ATL_CONTEXT": "", "ATL_TASK": "t", "ATL_TMUX_SOCKET": ""} {
		if v, ok := got[k]; !ok || v != want {
			t.Errorf("%s = %q (%v), want %q", k, v, ok, want)
		}
	}
	if _, ok := got["GIT_CONFIG_GLOBAL"]; ok {
		t.Error("an empty GIT_CONFIG_GLOBAL would hide every global git config")
	}
}
