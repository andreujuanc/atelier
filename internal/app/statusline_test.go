package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/andreujuanc/atelier/internal/gen"
	"github.com/andreujuanc/atelier/internal/model"
)

// TestRelaunchAddsStatusLine: a task whose settings.json predates the
// status line (hooks only, written by an older atl) gets atelier's
// statusLine on its next launch (resume or new session); the hooks stay
// and a user's own statusLine is never replaced. A global statusLine of the
// user's makes the next launch drop atelier's entry.
func TestRelaunchAddsStatusLine(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	writeOld := func(slug, extra string) string {
		t.Helper()
		path := filepath.Join(e.app.Paths.TaskDir(slug), ".claude", "settings.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		old := `{` + extra + `"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "'/usr/bin/atl' hook Stop"}]},
			{"hooks": [{"type": "command", "command": "notify-send done"}]}]}}`
		if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	read := func(path string) map[string]any {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%v: %s", err, b)
		}
		return m
	}
	countCmd := func(m map[string]any, event, cmd string) int {
		n := 0
		groups, _ := m["hooks"].(map[string]any)[event].([]any)
		for _, g := range groups {
			for _, h := range g.(map[string]any)["hooks"].([]any) {
				if h.(map[string]any)["command"] == cmd {
					n++
				}
			}
		}
		return n
	}

	// Resume of a stopped session.
	task := e.task(t, "old", model.StatusActive, true)
	path := writeOld("old", "")
	stopped := &model.Session{TaskID: task.ID, ClaudeSessionID: "cid-1", State: model.StateStopped}
	if err := e.app.Store.CreateSession(ctx, stopped); err != nil {
		t.Fatal(err)
	}
	if n, err := e.app.Resume(ctx, "old"); err != nil || n != 1 {
		t.Fatalf("Resume = %d, %v", n, err)
	}
	e.argv(t, stopped.ID)
	m := read(path)
	sl, _ := m["statusLine"].(map[string]any)
	if sl == nil || sl["type"] != "command" || sl["command"] != gen.StatusLineCommand("/usr/bin/atl") {
		t.Errorf("statusLine after resume = %#v", m["statusLine"])
	}
	if countCmd(m, "Stop", gen.HookCommand("/usr/bin/atl", "Stop")) != 1 || countCmd(m, "Stop", "notify-send done") != 1 ||
		countCmd(m, "SessionStart", gen.HookCommand("/usr/bin/atl", "SessionStart")) != 1 {
		t.Errorf("hooks not intact after resume: %v", m["hooks"])
	}

	// A new session with a moved atl binary updates atelier's entry.
	e.app.AtlBin = "/opt/atl/atl"
	s, err := e.app.NewSession(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	e.argv(t, s.ID)
	if sl, _ := read(path)["statusLine"].(map[string]any); sl["command"] != gen.StatusLineCommand("/opt/atl/atl") {
		t.Errorf("statusLine not updated to the new binary: %#v", sl)
	}

	// A user's own statusLine is kept on launch.
	e.task(t, "mine", model.StatusActive, true)
	path = writeOld("mine", `"statusLine": {"type": "command", "command": "~/bin/my-status"}, `)
	s, err = e.app.NewSession(ctx, "mine")
	if err != nil {
		t.Fatal(err)
	}
	e.argv(t, s.ID)
	m = read(path)
	if sl, _ := m["statusLine"].(map[string]any); sl["command"] != "~/bin/my-status" {
		t.Errorf("user statusLine replaced: %#v", m["statusLine"])
	}
	if countCmd(m, "Stop", gen.HookCommand("/opt/atl/atl", "Stop")) != 1 {
		t.Errorf("hooks not refreshed: %v", m["hooks"])
	}

	// With a global statusLine of the user's, the next launch removes
	// atelier's entry (project settings would hide the user's) and keeps
	// the hooks.
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"statusLine": {"type": "command", "command": "~/s.sh"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(e.app.Paths.TaskDir("old"), ".claude", "settings.json")
	s, err = e.app.NewSession(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	e.argv(t, s.ID)
	m = read(path)
	if _, ok := m["statusLine"]; ok {
		t.Errorf("atelier's statusLine kept despite a global one: %#v", m["statusLine"])
	}
	if countCmd(m, "Stop", gen.HookCommand("/opt/atl/atl", "Stop")) != 1 {
		t.Errorf("hooks not refreshed: %v", m["hooks"])
	}
}
