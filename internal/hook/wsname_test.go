package hook

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/store"
)

// TestWorkspaceByName: $ATL_WORKSPACE may be a workspace name, as for every
// atl command (docs/workspaces.md); the hook and the status line resolve it
// under $ATL_WORKSPACES like the CLI, not as a path relative to the current
// directory, so the ro guard still blocks an edit into an ro repo.
func TestWorkspaceByName(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	wsDir := filepath.Join(root, "ws", "work")
	for k, v := range map[string]string{
		"HOME":           root,
		"ATL_WORKSPACES": filepath.Join(root, "ws"),
		"ATL_WORKSPACE":  wsDir,
		"ATL_REPOS":      filepath.Join(root, "repos"),
		"ATL_TASKS":      "",
	} {
		t.Setenv(k, v)
	}
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := config.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{p.RepoPath("docs"), p.WorktreePath("fix", "api")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(p.DB)
	if err != nil {
		t.Fatal(err)
	}
	task := &model.Task{Slug: "fix", Title: "Fix", Status: model.StatusActive, Branch: "atl/fix",
		Repos: []model.TaskRepo{{Name: "api", Mode: model.ModeRW}, {Name: "docs", Mode: model.ModeRO}}}
	if err := st.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	st.Close()

	t.Setenv("ATL_WORKSPACE", "work") // a name, run from the task dir
	t.Chdir(p.TaskDir("fix"))
	env := envOf(map[string]string{"ATL_TASK": "fix"})
	in := `{"session_id":"s","cwd":"` + p.TaskDir("fix") + `","tool_name":"Edit","tool_input":{"file_path":"` +
		filepath.Join(p.RepoPath("docs"), "a.md") + `"}}`
	var out, errb bytes.Buffer
	if code := run([]string{EventPreToolUse}, strings.NewReader(in), &out, &errb, env); code != 2 ||
		!strings.Contains(errb.String(), "inside read-only repo docs") {
		t.Errorf("PreToolUse: exit=%d stderr=%q; want 2, blocked", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	sl := envOf(map[string]string{"ATL_TASK": "fix", "NO_COLOR": "1"})
	if code := runStatusLine(strings.NewReader(`{"session_id":"s"}`), &out, &errb, sl); code != 0 ||
		strings.TrimSpace(out.String()) != "● Fix" {
		t.Errorf("statusline: exit=%d out=%q stderr=%q; want ● Fix", code, out.String(), errb.String())
	}
}
