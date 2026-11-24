package config

import (
	"os"
	"path/filepath"
	"testing"
)

func setEnv(t *testing.T, home string, kv map[string]string) {
	t.Helper()
	t.Setenv("HOME", home)
	for _, k := range []string{"ATL_WORKSPACE", "ATL_REPOS", "ATL_TASKS", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME"} {
		t.Setenv(k, kv[k])
	}
}

func TestResolveDefaults(t *testing.T) {
	home := t.TempDir()
	setEnv(t, home, nil)
	p, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{
		Repos:     filepath.Join(home, "repos"),
		Tasks:     filepath.Join(home, "tasks"),
		DB:        filepath.Join(home, ".local/share/atelier/atelier.db"),
		StateDir:  filepath.Join(home, ".local/state/atelier"),
		ConfigDir: filepath.Join(home, ".config/atelier"),
	}
	if p != want {
		t.Fatalf("got %+v\nwant %+v", p, want)
	}
	// Resolve must not create anything.
	entries, _ := os.ReadDir(home)
	if len(entries) != 0 {
		t.Fatalf("Resolve created entries in home: %v", entries)
	}
}

func TestResolveOverrides(t *testing.T) {
	home := t.TempDir()
	other := t.TempDir()
	cwd, _ := os.Getwd()
	setEnv(t, home, map[string]string{
		"ATL_REPOS":       "~/code",
		"ATL_TASKS":       "rel/tasks",
		"XDG_DATA_HOME":   "~",
		"XDG_STATE_HOME":  "rel/state",
		"XDG_CONFIG_HOME": filepath.Join(other, "cfg") + "/",
	})
	p, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{
		Repos:     filepath.Join(home, "code"),
		Tasks:     filepath.Join(cwd, "rel/tasks"), // relative ATL_* made absolute
		DB:        filepath.Join(home, "atelier/atelier.db"),
		StateDir:  filepath.Join(home, ".local/state/atelier"), // relative XDG ignored
		ConfigDir: filepath.Join(other, "cfg/atelier"),
	}
	if p != want {
		t.Fatalf("got %+v\nwant %+v", p, want)
	}
}

func TestResolveWorkspace(t *testing.T) {
	home := t.TempDir()
	ws := filepath.Join(t.TempDir(), "beta")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	setEnv(t, home, map[string]string{
		"ATL_WORKSPACE":   ws + "/",
		"ATL_REPOS":       "~/code",
		"XDG_DATA_HOME":   filepath.Join(home, "xdg-data"), // ignored with a workspace
		"XDG_STATE_HOME":  filepath.Join(home, "xdg-state"),
		"XDG_CONFIG_HOME": filepath.Join(home, "xdg-config"),
	})
	p, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{
		Workspace: ws,
		Repos:     filepath.Join(home, "code"),
		Tasks:     filepath.Join(ws, "tasks"),
		DB:        filepath.Join(ws, "data/atelier.db"),
		StateDir:  filepath.Join(ws, "state"),
		ConfigDir: filepath.Join(ws, "config"),
	}
	if p != want {
		t.Fatalf("got %+v\nwant %+v", p, want)
	}
	if p.AssistantDir() != filepath.Join(ws, "data/assistant") {
		t.Fatalf("assistant dir %q", p.AssistantDir())
	}
	// Resolve must not create anything in the workspace.
	if entries, _ := os.ReadDir(ws); len(entries) != 0 {
		t.Fatalf("Resolve created entries in the workspace: %v", entries)
	}

	// ATL_TASKS still overrides the workspace's tasks dir.
	t.Setenv("ATL_TASKS", filepath.Join(home, "elsewhere"))
	if p, err = Resolve(); err != nil || p.Tasks != filepath.Join(home, "elsewhere") {
		t.Fatalf("ATL_TASKS override: %q, %v", p.Tasks, err)
	}
}

func TestResolveWorkspaceMissing(t *testing.T) {
	home := t.TempDir()
	file := filepath.Join(home, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, ws := range []string{filepath.Join(home, "nope"), file} {
		setEnv(t, home, map[string]string{"ATL_WORKSPACE": ws})
		if _, err := Resolve(); err == nil {
			t.Errorf("ATL_WORKSPACE=%s: want an error", ws)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "nope")); err == nil {
		t.Error("Resolve created the missing workspace")
	}
}

func TestHelpers(t *testing.T) {
	p := Paths{Repos: "/r", Tasks: "/t", StateDir: "/s", ConfigDir: "/c"}
	w := Paths{Workspace: "/w", Tasks: "/w/tasks", StateDir: "/w/state"}
	cases := []struct{ got, want string }{
		{p.TaskDir("fix-bug"), "/t/fix-bug"},
		{p.RepoPath("api"), "/r/api"},
		{p.WorktreePath("fix-bug", "api"), "/t/fix-bug/api"},
		{p.HookLog(), "/s/hook.log"},
		{p.ProjectFile("acme"), "/c/projects/acme.toml"},
		{filepath.Dir(p.RunDir("fix-bug")), "/s-run"},
		{p.OriginPin("api"), "/s-run/origins/api.json"},
		{filepath.Dir(w.RunDir("fix-bug")), "/w/run"},
		{w.OriginPin("api"), "/w/run/origins/api.json"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

func TestResolveIgnoresRelativeXDG(t *testing.T) {
	home := t.TempDir()
	setEnv(t, home, map[string]string{
		"XDG_DATA_HOME":   "data",
		"XDG_STATE_HOME":  "./state",
		"XDG_CONFIG_HOME": "~cfg",
	})
	p, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local/share/atelier/atelier.db"); p.DB != want {
		t.Errorf("DB = %q, want %q", p.DB, want)
	}
	if want := filepath.Join(home, ".local/state/atelier"); p.StateDir != want {
		t.Errorf("StateDir = %q, want %q", p.StateDir, want)
	}
	if want := filepath.Join(home, ".config/atelier"); p.ConfigDir != want {
		t.Errorf("ConfigDir = %q, want %q", p.ConfigDir, want)
	}
}

func TestAssistantDir(t *testing.T) {
	p := Paths{DB: "/data/atelier/atelier.db"}
	if got := p.AssistantDir(); got != "/data/atelier/assistant" {
		t.Fatalf("AssistantDir = %q", got)
	}
}

// TestRunLayout: everything next to the run dirs (task Claude dirs, the
// token, the assistant's dirs) lies under RunRoot, outside StateDir and the
// task dirs that containers see; each task's run dir and Claude dir are its
// own (named by a hash of its task dir), and no task's can be the
// assistant's.
func TestRunLayout(t *testing.T) {
	for _, p := range []Paths{
		{Tasks: "/t", StateDir: "/s", DB: "/d/atelier.db"},
		{Workspace: "/w", Tasks: "/w/tasks", StateDir: "/w/state", DB: "/w/data/atelier.db"},
	} {
		root := p.RunRoot()
		under := func(path string) bool {
			rel, err := filepath.Rel(root, path)
			return err == nil && rel != "." && filepath.IsLocal(rel)
		}
		for _, d := range []string{p.RunDir("a"), p.ClaudeDir("a"), p.ClaudeToken(), p.AssistantRunDir(), p.AssistantClaudeDir(), p.OriginPin("api")} {
			if !under(d) {
				t.Errorf("%s is not under the run root %s", d, root)
			}
			for _, seen := range []string{p.StateDir, p.Tasks, filepath.Dir(p.DB)} {
				if rel, err := filepath.Rel(seen, d); err == nil && filepath.IsLocal(rel) {
					t.Errorf("%s is inside %s", d, seen)
				}
			}
		}
		if p.RunDir("a") == p.RunDir("b") || p.ClaudeDir("a") == p.ClaudeDir("b") {
			t.Error("two tasks share a run dir or Claude dir")
		}
		other := p
		other.Tasks += "2"
		if other.ClaudeDir("a") == p.ClaudeDir("a") {
			t.Error("the same slug in another tasks dir shares the Claude dir")
		}
		if filepath.Dir(p.ClaudeDir("a")) != filepath.Dir(p.AssistantClaudeDir()) || p.ClaudeDir("assistant") == p.AssistantClaudeDir() ||
			p.RunDir("assistant") == p.AssistantRunDir() {
			t.Error("a task's dirs can be the assistant's")
		}
	}
	if got := (Paths{StateDir: "/s"}).UserTmuxConf(); got != "" {
		t.Errorf("UserTmuxConf without a workspace = %q", got)
	}
	if got := (Paths{Workspace: "/w"}).UserTmuxConf(); got != "/w/tmux.conf" {
		t.Errorf("UserTmuxConf = %q", got)
	}
}

func TestClaudeConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if got := ClaudeConfigDir(); got != filepath.Join(home, ".claude") {
		t.Errorf("ClaudeConfigDir = %q", got)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/cc")
	if got := ClaudeUserSettings(); got != "/cc/settings.json" {
		t.Errorf("ClaudeUserSettings = %q", got)
	}
}
