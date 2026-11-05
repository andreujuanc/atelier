package hook

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andreujuanc/atelier/internal/config"
)

// assistantSetup points the environment at a temp root whose database path
// is a directory (so any attempt to open the store fails) and creates the
// assistant dir. It returns the paths and the root.
func assistantSetup(t *testing.T) (config.Paths, string) {
	t.Helper()
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	for k, v := range map[string]string{
		"HOME":            root,
		"ATL_WORKSPACE":   "", // never the developer's real workspace
		"ATL_REPOS":       filepath.Join(root, "repos"),
		"ATL_TASKS":       filepath.Join(root, "tasks"),
		"XDG_DATA_HOME":   filepath.Join(root, "data"),
		"XDG_STATE_HOME":  filepath.Join(root, "state"),
		"XDG_CONFIG_HOME": filepath.Join(root, "config"),
		"ATL_SESSION":     "",
		"ATL_TASK":        "",
	} {
		t.Setenv(k, v)
	}
	p, err := config.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	// The DB path is a non-empty directory: opening the store would fail.
	for _, d := range []string{filepath.Join(p.DB, "blocker"), p.AssistantDir(), p.WorktreePath("fix", "api")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return p, root
}

func assistantInput(tool, cwd, key, path string) string {
	ti := map[string]any{}
	if key != "" {
		ti[key] = path
	}
	b, _ := json.Marshal(map[string]any{"session_id": "a1", "cwd": cwd, "hook_event_name": "PreToolUse",
		"tool_name": tool, "tool_input": ti})
	return string(b)
}

func TestAssistantGuard(t *testing.T) {
	p, root := assistantSetup(t)
	dir := p.AssistantDir()
	outside := p.WorktreePath("fix", "api")
	if err := os.Symlink(outside, filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(outside, "deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(deep, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "nowhere.md"), filepath.Join(dir, "dangling.md")); err != nil {
		t.Fatal(err)
	}

	const notAllowed = "is not allowed — the assistant only manages tasks"
	for _, c := range []struct {
		name, stdin string
		want        int
		wantErr     string
	}{
		{"abs inside", assistantInput("Write", "/", "file_path", filepath.Join(dir, "notes.md")), 0, ""},
		{"new subdir inside", assistantInput("Write", "/", "file_path", filepath.Join(dir, "scratch", "a", "b.md")), 0, ""},
		{"relative inside", assistantInput("Edit", dir, "file_path", "notes.md"), 0, ""},
		{"via symlinked alias of the dir", assistantInput("Write", "/", "file_path", filepath.Join(alias, "notes.md")), 0, ""},
		{"multiedit inside", assistantInput("MultiEdit", dir, "file_path", "./x/../notes.md"), 0, ""},
		{"empty tool name inside", assistantInput("", "/", "file_path", filepath.Join(dir, "n.md")), 0, ""},
		{"other tool ignored", assistantInput("TodoWrite", "/", "", ""), 0, ""},

		{"abs outside", assistantInput("Write", dir, "file_path", filepath.Join(outside, "main.go")), 2,
			"atelier assistant: editing " + filepath.Join(outside, "main.go") + " " + notAllowed},
		{"relative escape", assistantInput("Edit", dir, "file_path", "../atelier.db/x"), 2, "editing ../atelier.db/x " + notAllowed},
		{"notebook outside", assistantInput("NotebookEdit", dir, "notebook_path", filepath.Join(outside, "n.ipynb")), 2, notAllowed},
		{"symlink inside pointing out", assistantInput("Write", "/", "file_path", filepath.Join(dir, "out", "main.go")), 2, notAllowed},
		{"symlink then dotdot (kernel walk)", assistantInput("Write", "/", "file_path", filepath.Join(dir, "link")+"/../x.go"), 2, notAllowed},
		{"dangling symlink pointing out", assistantInput("Write", "/", "file_path", filepath.Join(dir, "dangling.md")), 2, notAllowed},
		{"tilde outside", assistantInput("Write", dir, "file_path", "~/notes.md"), 2, "editing ~/notes.md " + notAllowed},
		{"own CLAUDE.md", assistantInput("Edit", dir, "file_path", "CLAUDE.md"), 2, notAllowed},
		{"own CLAUDE.local.md", assistantInput("Write", dir, "file_path", "CLAUDE.local.md"), 2, notAllowed},
		{"own CLAUDE.local.md other case", assistantInput("Write", dir, "file_path", "claude.LOCAL.md"), 2, notAllowed},
		{"own .mcp.json", assistantInput("Write", "/", "file_path", filepath.Join(dir, ".mcp.json")), 2, notAllowed},
		{"own .mcp.json other case", assistantInput("Edit", dir, "file_path", ".MCP.json"), 2, notAllowed},
		{"mcp-ish name allowed", assistantInput("Write", dir, "file_path", "notes/.mcp.json.md"), 0, ""},
		{"own settings", assistantInput("Write", "/", "file_path", filepath.Join(dir, ".claude", "settings.json")), 2, notAllowed},
		{"own settings via relative", assistantInput("Write", dir, "file_path", "x/../.claude/settings.local.json"), 2, notAllowed},
		{"own claude.md, other case", assistantInput("Write", dir, "file_path", "claude.md"), 2, notAllowed},
		{"own CLAUDE.MD, other case", assistantInput("Edit", "/", "file_path", filepath.Join(dir, "Claude.MD")), 2, notAllowed},
		{"own settings, other case", assistantInput("Write", "/", "file_path", filepath.Join(dir, ".Claude", "settings.json")), 2, notAllowed},
		{"own settings dir, upper case", assistantInput("Write", dir, "file_path", ".CLAUDE/hooks.json"), 2, notAllowed},
		{"claude-ish name allowed", assistantInput("Write", dir, "file_path", ".claude-notes/x.md"), 0, ""},
		{"relative without cwd", assistantInput("Write", "", "file_path", "notes.md"), 2, notAllowed},

		{"malformed JSON", `{"tool_name":"Write","tool_input":{"file_path":"` + filepath.Join(dir, "a") + `"`, 2, "not allowed"},
		{"not an object", `[]`, 2, "not allowed"},
		{"empty input", ``, 2, "not allowed"},
		{"no target", assistantInput("Write", dir, "content", "x"), 2, "not allowed (no target path)"},
		{"non-string target", `{"tool_name":"Write","cwd":"/","tool_input":{"file_path":7}}`, 2, "not allowed"},
		{"bad tool_input", `{"tool_name":"Write","cwd":"/","tool_input":"x"}`, 2, "not allowed"},
		{"bad string beside good path", `{"tool_name":"NotebookEdit","cwd":"/","tool_input":{"file_path":"` +
			filepath.Join(dir, "a") + `","notebook_path":[]}}`, 2, "not allowed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var outb, errb bytes.Buffer
			code := run([]string{EventAssistantGuard}, strings.NewReader(c.stdin), &outb, &errb, envOf(nil))
			if outb.Len() != 0 {
				t.Errorf("unexpected stdout %q", outb.String())
			}
			if code != c.want || !strings.Contains(errb.String(), c.wantErr) {
				t.Errorf("exit=%d stderr=%q; want %d with %q", code, errb.String(), c.want, c.wantErr)
			}
			if c.want == 0 && errb.Len() != 0 {
				t.Errorf("unexpected stderr %q", errb.String())
			}
		})
	}

	t.Run("tilde inside when home is the dir", func(t *testing.T) {
		t.Setenv("HOME", dir)
		var outb, errb bytes.Buffer
		code := run([]string{EventAssistantGuard}, strings.NewReader(assistantInput("Write", dir, "file_path", "~/n.md")), &outb, &errb, envOf(nil))
		if code != 0 {
			t.Errorf("exit=%d stderr=%q", code, errb.String())
		}
	})

	t.Run("oversized input refused", func(t *testing.T) {
		old := maxInput
		maxInput = 1024
		t.Cleanup(func() { maxInput = old })
		big := `{"tool_name":"Write","cwd":"/","tool_input":{"file_path":"` + filepath.Join(dir, "a") +
			`","content":"` + strings.Repeat("x", 4096) + `"}}`
		var outb, errb bytes.Buffer
		if code := run([]string{EventAssistantGuard}, strings.NewReader(big), &outb, &errb, envOf(nil)); code != 2 || outb.Len() != 0 {
			t.Errorf("exit=%d stderr=%q", code, errb.String())
		}
	})

	// The store was never opened: the DB path (a directory) is untouched
	// and nothing was logged for allowed calls.
	ents, err := os.ReadDir(p.DB)
	if err != nil || len(ents) != 1 || ents[0].Name() != "blocker" {
		t.Errorf("DB dir touched: %v %v", ents, err)
	}
	if strings.Contains(readLog(t, p), "open") {
		t.Errorf("store opened: %s", readLog(t, p))
	}
	if log := readLog(t, p); !strings.Contains(log, "event=AssistantGuard") || !strings.Contains(log, "decode hook input") {
		t.Errorf("errors not logged: %q", log)
	}
}

func TestAssistantGuardProcess(t *testing.T) {
	p, _ := assistantSetup(t)
	dir := p.AssistantDir()
	exit, out, errs, _ := runMain(t, EventAssistantGuard, assistantInput("Write", dir, "file_path", "notes.md"))
	if exit != 0 || out != "" || errs != "" {
		t.Errorf("inside: exit=%d stdout=%q stderr=%q", exit, out, errs)
	}
	target := filepath.Join(p.Tasks, "fix", "api", "main.go")
	exit, out, errs, _ = runMain(t, EventAssistantGuard, assistantInput("Write", dir, "file_path", target))
	if exit != 2 || out != "" || !strings.Contains(errs, "atelier assistant: editing "+target+" is not allowed") {
		t.Errorf("outside: exit=%d stdout=%q stderr=%q", exit, out, errs)
	}
	exit, _, errs, _ = runMain(t, EventAssistantGuard, "{nope")
	if exit != 2 || !strings.Contains(errs, "not allowed") {
		t.Errorf("malformed: exit=%d stderr=%q", exit, errs)
	}
}
