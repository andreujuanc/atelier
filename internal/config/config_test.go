package config

import (
	"os"
	"path/filepath"
	"testing"
)

func setEnv(t *testing.T, home string, kv map[string]string) {
	t.Helper()
	t.Setenv("HOME", home)
	for _, k := range []string{"ATL_REPOS", "ATL_TASKS", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME"} {
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
