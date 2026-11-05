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
	ws := filepath.Join(t.TempDir(), "ed")
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
	cases := []struct{ got, want string }{
		{p.TaskDir("fix-bug"), "/t/fix-bug"},
		{p.RepoPath("api"), "/r/api"},
		{p.WorktreePath("fix-bug", "api"), "/t/fix-bug/api"},
		{p.HookLog(), "/s/hook.log"},
		{p.ProjectFile("acme"), "/c/projects/acme.toml"},
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
