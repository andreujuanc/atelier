package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/store"
)

// TestMain lets tests re-exec the test binary as `atl hook <args>`.
func TestMain(m *testing.M) {
	if os.Getenv("ATL_HOOK_TEST_MAIN") == "1" {
		os.Exit(Main(strings.Fields(os.Getenv("ATL_HOOK_TEST_ARGS"))))
	}
	os.Exit(m.Run())
}

var now = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

type fixture struct {
	root string
	p    config.Paths
	st   *store.Store
	task *model.Task // slug "fix": api rw, docs ro
}

func setup(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	for k, v := range map[string]string{
		"HOME":            root,
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
	for _, d := range []string{
		p.RepoPath("api"), p.RepoPath("docs"), p.RepoPath("docs2"),
		p.WorktreePath("fix", "api"), p.TaskDir("other"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(p.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	task := &model.Task{Slug: "fix", Title: "Fix", Status: model.StatusActive, Branch: "atl/fix",
		Repos: []model.TaskRepo{{Name: "api", Mode: model.ModeRW}, {Name: "docs", Mode: model.ModeRO}}}
	if err := st.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	other := &model.Task{Slug: "other", Title: "Other", Status: model.StatusActive, Branch: "atl/other", PortOffset: 100,
		Repos: []model.TaskRepo{{Name: "api", Mode: model.ModeRW}}}
	if err := st.CreateTask(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	return &fixture{root: root, p: p, st: st, task: task}
}

func (f *fixture) session(t *testing.T, st model.SessionState, cid string) *model.Session {
	t.Helper()
	s := &model.Session{TaskID: f.task.ID, ClaudeSessionID: cid, TmuxTarget: "@3", Name: "claude-x", State: st,
		LastEventAt: now.Add(-time.Hour)}
	if err := f.st.CreateSession(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *fixture) get(t *testing.T, id int64) *model.Session {
	t.Helper()
	s, err := f.st.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestTransitions(t *testing.T) {
	cases := []struct {
		name    string
		from    model.SessionState
		event   string
		in      Input
		want    model.SessionState
		touched bool
	}{
		{"start from idle (/clear)", model.StateIdle, EventSessionStart, Input{SessionID: "new", Source: "clear"}, model.StateIdle, true},
		{"start resumes stopped", model.StateStopped, EventSessionStart, Input{SessionID: "new", Source: "resume"}, model.StateIdle, true},
		{"start resumes ended", model.StateEnded, EventSessionStart, Input{SessionID: "new", Source: "resume"}, model.StateIdle, true},
		{"compact keeps running", model.StateRunning, EventSessionStart, Input{SessionID: "new", Source: "compact"}, model.StateRunning, true},
		{"compact keeps waiting", model.StateWaiting, EventSessionStart, Input{SessionID: "new", Source: "compact"}, model.StateWaiting, true},
		{"compact keeps idle", model.StateIdle, EventSessionStart, Input{SessionID: "new", Source: "compact"}, model.StateIdle, true},
		{"start from running (/clear)", model.StateRunning, EventSessionStart, Input{SessionID: "new", Source: "clear"}, model.StateIdle, true},
		{"prompt", model.StateIdle, EventUserPromptSubmit, Input{}, model.StateRunning, true},
		{"prompt from waiting", model.StateWaiting, EventUserPromptSubmit, Input{}, model.StateRunning, true},
		{"permission typed", model.StateRunning, EventNotification, Input{NotificationType: "permission_prompt", Message: "Claude needs your permission to use Bash"}, model.StateWaiting, true},
		{"permission untyped", model.StateRunning, EventNotification, Input{Message: "Claude needs your PERMISSION to use Bash"}, model.StateWaiting, true},
		{"idle notification typed", model.StateIdle, EventNotification, Input{NotificationType: "idle_prompt", Message: "Claude is waiting for your input"}, model.StateIdle, true},
		{"idle notification untyped", model.StateRunning, EventNotification, Input{Message: "Claude is waiting for your input"}, model.StateRunning, true},
		{"other typed notification mentioning permission", model.StateRunning, EventNotification, Input{NotificationType: "auth_success", Message: "permission granted"}, model.StateRunning, true},
		{"posttool clears waiting", model.StateWaiting, EventPostToolUse, Input{ToolName: "Bash"}, model.StateRunning, true},
		{"posttool leaves running", model.StateRunning, EventPostToolUse, Input{ToolName: "Bash"}, model.StateRunning, true},
		{"posttool leaves idle", model.StateIdle, EventPostToolUse, Input{ToolName: "Bash"}, model.StateIdle, true},
		{"posttool leaves stopped", model.StateStopped, EventPostToolUse, Input{}, model.StateStopped, true},
		{"stop", model.StateRunning, EventStop, Input{}, model.StateIdle, true},
		{"session end", model.StateIdle, EventSessionEnd, Input{Source: "exit"}, model.StateEnded, true},
		{"pretooluse no state change", model.StateRunning, EventPreToolUse, Input{ToolName: "Edit", ToolInput: json.RawMessage(`{"file_path":"x"}`)}, model.StateRunning, false},
		{"unknown event", model.StateRunning, "SubagentStop", Input{}, model.StateRunning, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			s := f.session(t, c.from, "old")
			c.in.Cwd = f.p.TaskDir("fix")
			env := envOf(map[string]string{"ATL_SESSION": itoa(s.ID), "ATL_TASK": "fix"})
			exit, msg, err := Handle(context.Background(), c.event, c.in, env, f.st, f.p, now)
			if exit != 0 || msg != "" || err != nil {
				t.Fatalf("Handle = %d, %q, %v", exit, msg, err)
			}
			g := f.get(t, s.ID)
			if g.State != c.want {
				t.Errorf("state = %s, want %s", g.State, c.want)
			}
			if got := g.LastEventAt.Equal(now); got != c.touched {
				t.Errorf("last_event_at = %v (touched=%v, want %v)", g.LastEventAt, got, c.touched)
			}
			wantCID := "old"
			if c.event == EventSessionStart {
				wantCID = "new"
			}
			if g.ClaudeSessionID != wantCID || g.TmuxTarget != "@3" || g.Name != "claude-x" {
				t.Errorf("row = %+v", g)
			}
		})
	}
}

func itoa(i int64) string { b, _ := json.Marshal(i); return string(b) }

func countSessions(t *testing.T, f *fixture) int {
	t.Helper()
	all, err := f.st.ListSessionsByState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return len(all)
}

func TestSessionResolution(t *testing.T) {
	ctx := context.Background()

	t.Run("ATL_SESSION of another task falls back to claude id", func(t *testing.T) {
		f := setup(t)
		s := f.session(t, model.StateIdle, "c1")
		other := f.session(t, model.StateIdle, "c2")
		env := envOf(map[string]string{"ATL_SESSION": itoa(s.ID), "ATL_TASK": "other"})
		if _, _, err := Handle(ctx, EventUserPromptSubmit, Input{SessionID: "c2"}, env, f.st, f.p, now); err != nil {
			t.Fatal(err)
		}
		if f.get(t, s.ID).State != model.StateIdle || f.get(t, other.ID).State != model.StateRunning {
			t.Errorf("wrong row updated: %+v / %+v", f.get(t, s.ID), f.get(t, other.ID))
		}
	})

	t.Run("ATL_SESSION missing row and bad value fall back", func(t *testing.T) {
		f := setup(t)
		s := f.session(t, model.StateIdle, "c1")
		for _, v := range []string{"9999", "abc"} {
			env := envOf(map[string]string{"ATL_SESSION": v})
			if _, _, err := Handle(ctx, EventStop, Input{SessionID: "nope"}, env, f.st, f.p, now); err != nil {
				t.Fatal(err)
			}
		}
		if g := f.get(t, s.ID); g.LastEventAt.Equal(now) {
			t.Errorf("unrelated row touched: %+v", g)
		}
	})

	t.Run("claude id", func(t *testing.T) {
		f := setup(t)
		s := f.session(t, model.StateIdle, "c1")
		if _, _, err := Handle(ctx, EventUserPromptSubmit, Input{SessionID: "c1", Cwd: "/elsewhere"}, envOf(nil), f.st, f.p, now); err != nil {
			t.Fatal(err)
		}
		if g := f.get(t, s.ID); g.State != model.StateRunning || !g.LastEventAt.Equal(now) {
			t.Errorf("row = %+v", g)
		}
	})

	t.Run("cwd-derived task creates row on SessionStart", func(t *testing.T) {
		f := setup(t)
		in := Input{SessionID: "fresh", Cwd: filepath.Join(f.p.WorktreePath("fix", "api"), "sub", "dir")}
		if _, _, err := Handle(ctx, EventSessionStart, in, envOf(nil), f.st, f.p, now); err != nil {
			t.Fatal(err)
		}
		s, err := f.st.GetSessionByClaudeID(ctx, "fresh")
		if err != nil {
			t.Fatal(err)
		}
		if s.TaskID != f.task.ID || s.State != model.StateIdle || s.TmuxTarget != "" || !s.LastEventAt.Equal(now) || !s.CreatedAt.Equal(now) {
			t.Errorf("created row = %+v", s)
		}
		// A second SessionStart with the same claude id attaches, not duplicates.
		if _, _, err := Handle(ctx, EventSessionStart, in, envOf(nil), f.st, f.p, now); err != nil {
			t.Fatal(err)
		}
		if n := countSessions(t, f); n != 1 {
			t.Errorf("sessions = %d, want 1", n)
		}
	})

	t.Run("cwd via symlinked tasks dir", func(t *testing.T) {
		f := setup(t)
		link := filepath.Join(f.root, "tlink")
		if err := os.Symlink(f.p.Tasks, link); err != nil {
			t.Fatal(err)
		}
		in := Input{SessionID: "sym", Cwd: filepath.Join(link, "fix")}
		if _, _, err := Handle(ctx, EventSessionStart, in, envOf(nil), f.st, f.p, now); err != nil {
			t.Fatal(err)
		}
		if s, err := f.st.GetSessionByClaudeID(ctx, "sym"); err != nil || s.TaskID != f.task.ID {
			t.Errorf("session = %+v, %v", s, err)
		}
	})

	t.Run("ATL_TASK creates row on SessionStart", func(t *testing.T) {
		f := setup(t)
		in := Input{SessionID: "env", Cwd: "/somewhere/else"}
		if _, _, err := Handle(ctx, EventSessionStart, in, envOf(map[string]string{"ATL_TASK": "other"}), f.st, f.p, now); err != nil {
			t.Fatal(err)
		}
		s, err := f.st.GetSessionByClaudeID(ctx, "env")
		if err != nil {
			t.Fatal(err)
		}
		if task, _ := f.st.GetTaskBySlug(ctx, "other"); s.TaskID != task.ID {
			t.Errorf("row in task %d, want other", s.TaskID)
		}
	})

	t.Run("nested claude does not hijack the parent row", func(t *testing.T) {
		for _, source := range []string{"startup", ""} {
			f := setup(t)
			parent := f.session(t, model.StateRunning, "parent")
			env := envOf(map[string]string{"ATL_SESSION": itoa(parent.ID), "ATL_TASK": "fix"})
			cwd := f.p.WorktreePath("fix", "api")
			step := func(event string, in Input) {
				t.Helper()
				in.Cwd = cwd
				if exit, _, err := Handle(ctx, event, in, env, f.st, f.p, now); exit != 0 || err != nil {
					t.Fatalf("%s: %d, %v", event, exit, err)
				}
			}
			parentUntouched := func(what string) {
				t.Helper()
				if g := f.get(t, parent.ID); g.ClaudeSessionID != "parent" || g.State != model.StateRunning ||
					g.LastEventAt.Equal(now) || g.TmuxTarget != "@3" {
					t.Errorf("source %q, %s changed the parent row: %+v", source, what, g)
				}
			}
			child := func() *model.Session {
				t.Helper()
				c, err := f.st.GetSessionByClaudeID(ctx, "child")
				if err != nil {
					t.Fatalf("child row: %v", err)
				}
				return c
			}

			step(EventSessionStart, Input{SessionID: "child", Source: source})
			parentUntouched("child SessionStart")
			if c := child(); c.ID == parent.ID || c.TaskID != f.task.ID || c.TmuxTarget != "" || c.State != model.StateIdle {
				t.Errorf("child row = %+v", c)
			}
			step(EventUserPromptSubmit, Input{SessionID: "child"})
			step(EventNotification, Input{SessionID: "child", NotificationType: "permission_prompt"})
			if c := child(); c.State != model.StateWaiting {
				t.Errorf("child state = %s, want waiting", c.State)
			}
			step(EventPostToolUse, Input{SessionID: "child"})
			step(EventStop, Input{SessionID: "child"})
			parentUntouched("child Stop")
			if c := child(); c.State != model.StateIdle {
				t.Errorf("child state = %s, want idle", c.State)
			}
			step(EventSessionEnd, Input{SessionID: "child"})
			parentUntouched("child SessionEnd")
			if c := child(); c.State != model.StateEnded {
				t.Errorf("child state = %s, want ended", c.State)
			}
			if n := countSessions(t, f); n != 2 {
				t.Errorf("sessions = %d, want 2", n)
			}

			// /clear in the parent still moves the parent row to the new id.
			step(EventSessionStart, Input{SessionID: "parent2", Source: "clear"})
			if g := f.get(t, parent.ID); g.ClaudeSessionID != "parent2" || g.State != model.StateIdle || !g.LastEventAt.Equal(now) {
				t.Errorf("parent after /clear = %+v", g)
			}
			step(EventUserPromptSubmit, Input{SessionID: "parent2"})
			if g := f.get(t, parent.ID); g.State != model.StateRunning {
				t.Errorf("parent after prompt = %+v", g)
			}
			if c := child(); c.State != model.StateEnded {
				t.Errorf("parent event changed the child: %+v", c)
			}
		}
	})

	t.Run("nested event for unknown claude id is ignored", func(t *testing.T) {
		f := setup(t)
		parent := f.session(t, model.StateRunning, "parent")
		env := envOf(map[string]string{"ATL_SESSION": itoa(parent.ID)})
		for _, ev := range []string{EventStop, EventSessionEnd, EventUserPromptSubmit} {
			if _, _, err := Handle(ctx, ev, Input{SessionID: "stranger"}, env, f.st, f.p, now); err != nil {
				t.Fatal(err)
			}
		}
		if g := f.get(t, parent.ID); g.State != model.StateRunning || g.LastEventAt.Equal(now) {
			t.Errorf("parent row changed: %+v", g)
		}
	})

	t.Run("launched session without claude id", func(t *testing.T) {
		f := setup(t)
		s := f.session(t, model.StateIdle, "")
		env := envOf(map[string]string{"ATL_SESSION": itoa(s.ID), "ATL_TASK": "fix"})
		// A non-SessionStart event with no row for its id falls back to ATL_SESSION.
		if _, _, err := Handle(ctx, EventUserPromptSubmit, Input{SessionID: "c1"}, env, f.st, f.p, now); err != nil {
			t.Fatal(err)
		}
		if g := f.get(t, s.ID); g.State != model.StateRunning || g.ClaudeSessionID != "" {
			t.Errorf("row = %+v", g)
		}
		// The first SessionStart (startup) records the id on the launched row.
		if _, _, err := Handle(ctx, EventSessionStart, Input{SessionID: "c1", Source: "startup"}, env, f.st, f.p, now); err != nil {
			t.Fatal(err)
		}
		if g := f.get(t, s.ID); g.ClaudeSessionID != "c1" || g.State != model.StateIdle {
			t.Errorf("row = %+v", g)
		}
		if n := countSessions(t, f); n != 1 {
			t.Errorf("sessions = %d, want 1", n)
		}
	})

	t.Run("ATL_SESSION without claude id prefers the row matching the id", func(t *testing.T) {
		f := setup(t)
		s := f.session(t, model.StateIdle, "")
		other := f.session(t, model.StateIdle, "c9")
		env := envOf(map[string]string{"ATL_SESSION": itoa(s.ID)})
		if _, _, err := Handle(ctx, EventUserPromptSubmit, Input{SessionID: "c9"}, env, f.st, f.p, now); err != nil {
			t.Fatal(err)
		}
		if f.get(t, s.ID).State != model.StateIdle || f.get(t, other.ID).State != model.StateRunning {
			t.Errorf("wrong row updated: %+v / %+v", f.get(t, s.ID), f.get(t, other.ID))
		}
	})

	t.Run("no row created outside tasks or for other events", func(t *testing.T) {
		f := setup(t)
		for _, c := range []struct {
			event string
			in    Input
		}{
			{EventSessionStart, Input{SessionID: "a", Cwd: f.p.RepoPath("api")}},
			{EventSessionStart, Input{SessionID: "b", Cwd: f.p.Tasks}},
			{EventSessionStart, Input{SessionID: "c", Cwd: f.p.TaskDir("unknown-task")}},
			{EventSessionStart, Input{SessionID: "d", Cwd: f.p.Tasks + "x/fix"}},
			{EventUserPromptSubmit, Input{SessionID: "e", Cwd: f.p.TaskDir("fix")}},
			{EventStop, Input{SessionID: "f", Cwd: f.p.TaskDir("fix")}},
		} {
			exit, _, err := Handle(ctx, c.event, c.in, envOf(nil), f.st, f.p, now)
			if exit != 0 || err != nil {
				t.Errorf("%s %+v: %d, %v", c.event, c.in, exit, err)
			}
		}
		if n := countSessions(t, f); n != 0 {
			t.Errorf("sessions = %d, want 0", n)
		}
	})
}

func toolInput(key, path string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{key: path, "content": "x"})
	return b
}

func TestROGuard(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	docs := f.p.RepoPath("docs")
	taskDir := f.p.TaskDir("fix")
	if err := os.Symlink(docs, filepath.Join(taskDir, "docs-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(docs, "new.md"), filepath.Join(taskDir, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(docs, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		"sublink":  filepath.Join(docs, "sub"),
		"rel-link": "sublink/../y.md", // relative target whose ".." applies after sublink
		"loop1":    "loop2",
		"loop2":    "loop1",
	} {
		if err := os.Symlink(target, filepath.Join(taskDir, link)); err != nil {
			t.Fatal(err)
		}
	}
	// <taskDir>/tilde/~ is a literal "~" directory linking into ro: a
	// "~/a.md" target from that cwd must be checked unexpanded too.
	if err := os.MkdirAll(filepath.Join(taskDir, "tilde"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(docs, filepath.Join(taskDir, "tilde", "~")); err != nil {
		t.Fatal(err)
	}
	taskEnv := envOf(map[string]string{"ATL_TASK": "fix"})
	sep := string(filepath.Separator)

	cases := []struct {
		name    string
		key     string
		path    string
		cwd     string
		env     func(string) string
		block   bool
		wantErr bool
	}{
		{"absolute inside ro", "file_path", filepath.Join(docs, "sub", "a.md"), taskDir, taskEnv, true, false},
		{"ro root itself", "file_path", docs, taskDir, taskEnv, true, false},
		{"non-existent deep path inside ro", "file_path", filepath.Join(docs, "x", "y", "z.go"), taskDir, taskEnv, true, false},
		{"unclean path into ro", "file_path", filepath.Join(f.p.Repos, "api", "..", "docs", "a.md"), taskDir, taskEnv, true, false},
		{"relative via cwd", "file_path", "../../repos/docs/a.md", taskDir, taskEnv, true, false},
		{"symlink into ro", "file_path", filepath.Join(taskDir, "docs-link", "sub", "a.md"), taskDir, taskEnv, true, false},
		{"dangling symlink into ro", "file_path", filepath.Join(taskDir, "dangling"), taskDir, taskEnv, true, false},
		{"dotdot after symlink into ro subdir", "file_path", taskDir + sep + "sublink" + sep + ".." + sep + "x.md", taskDir, taskEnv, true, false},
		{"relative dotdot after symlink", "file_path", "sublink/../x.md", taskDir, taskEnv, true, false},
		{"relative dotdot after symlink, deep", "file_path", "sublink/../new/dir/x.md", taskDir, taskEnv, true, false},
		{"symlink target with dotdot", "file_path", filepath.Join(taskDir, "rel-link"), taskDir, taskEnv, true, false},
		{"missing dir then dotdot then symlink", "file_path", taskDir + sep + "nope" + sep + ".." + sep + "sublink" + sep + "a.md", taskDir, taskEnv, true, false},
		{"dotdot within rw allowed", "file_path", taskDir + sep + "api" + sep + ".." + sep + "api" + sep + "main.go", taskDir, taskEnv, false, false},
		{"symlink loop fails open", "file_path", filepath.Join(taskDir, "loop1", "x"), taskDir, taskEnv, false, true},
		{"notebook_path inside ro", "notebook_path", filepath.Join(docs, "nb.ipynb"), taskDir, taskEnv, true, false},
		{"rw worktree allowed", "file_path", filepath.Join(f.p.WorktreePath("fix", "api"), "main.go"), taskDir, taskEnv, false, false},
		{"relative rw allowed", "file_path", "api/main.go", taskDir, taskEnv, false, false},
		{"rw canonical not ro", "file_path", filepath.Join(f.p.RepoPath("api"), "main.go"), taskDir, taskEnv, false, false},
		{"sibling prefix allowed", "file_path", filepath.Join(f.p.RepoPath("docs2"), "a.md"), taskDir, taskEnv, false, false},
		{"task from cwd", "file_path", filepath.Join(docs, "a.md"), filepath.Join(taskDir, "api"), envOf(nil), true, false},
		{"unknown task fails open", "file_path", filepath.Join(docs, "a.md"), "/tmp", envOf(map[string]string{"ATL_TASK": "nope"}), false, true},
		{"task without ro repos", "file_path", filepath.Join(docs, "a.md"), taskDir, envOf(map[string]string{"ATL_TASK": "other"}), false, false},
		{"missing path fails open", "command", "ls", taskDir, taskEnv, false, true},
		{"tilde expands to home", "file_path", "~/repos/docs/a.md", taskDir, taskEnv, true, false},
		{"tilde without cwd still expanded", "file_path", "~/repos/docs", "", taskEnv, true, false},
		{"bare tilde is home, allowed", "file_path", "~", taskDir, taskEnv, false, false},
		{"tilde literal under cwd", "file_path", "~/a.md", filepath.Join(taskDir, "tilde"), taskEnv, true, false},
		{"tilde into rw allowed", "file_path", "~/repos/api/main.go", taskDir, taskEnv, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := Input{SessionID: "s", Cwd: c.cwd, ToolName: "Write", ToolInput: toolInput(c.key, c.path)}
			exit, msg, err := Handle(ctx, EventPreToolUse, in, c.env, f.st, f.p, now)
			if c.block {
				if exit != 2 || !strings.Contains(msg, "is inside read-only repo docs; edit it in a rw repo instead") ||
					!strings.HasPrefix(msg, "atelier: /") || err != nil {
					t.Errorf("want block, got %d %q %v", exit, msg, err)
				}
			} else if exit != 0 || msg != "" {
				t.Errorf("want allow, got %d %q %v", exit, msg, err)
			}
			if (err != nil) != c.wantErr {
				t.Errorf("err = %v, wantErr %v", err, c.wantErr)
			}
		})
	}

	t.Run("every target key checked", func(t *testing.T) {
		ro := filepath.Join(docs, "a.md")
		rw := filepath.Join(f.p.WorktreePath("fix", "api"), "x")
		q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
		for _, c := range []struct {
			input string
			block bool
		}{
			{`{"file_path":` + q(ro) + `,"File_Path":` + q(rw) + `}`, true},
			{`{"File_Path":` + q(ro) + `,"file_path":` + q(rw) + `}`, false},
			{`{"notebook_path":` + q(ro) + `,"file_path":` + q(rw) + `}`, true},
			{`{"file_path":` + q(rw) + `,"notebook_path":` + q(ro) + `}`, true},
			{`{"file_path":` + q(ro) + `,"file_path":` + q(rw) + `}`, false},
			{`{"file_path":` + q(rw) + `,"file_path":` + q(ro) + `}`, true},
			{`{"file_path":7,"notebook_path":` + q(ro) + `}`, true},
		} {
			in := Input{Cwd: taskDir, ToolName: "Write", ToolInput: json.RawMessage(c.input)}
			exit, msg, err := Handle(ctx, EventPreToolUse, in, taskEnv, f.st, f.p, now)
			if c.block && (exit != 2 || !strings.Contains(msg, "inside read-only repo docs")) {
				t.Errorf("%s: want block, got %d %q %v", c.input, exit, msg, err)
			}
			if !c.block && exit != 0 {
				t.Errorf("%s: want allow, got %d %q %v", c.input, exit, msg, err)
			}
		}
	})

	t.Run("task via session row", func(t *testing.T) {
		s := f.session(t, model.StateRunning, "guard-cid")
		for _, env := range []func(string) string{envOf(map[string]string{"ATL_SESSION": itoa(s.ID)}), envOf(nil)} {
			in := Input{SessionID: "guard-cid", Cwd: "/tmp", ToolInput: toolInput("file_path", filepath.Join(docs, "a.md"))}
			if exit, _, err := Handle(ctx, EventPreToolUse, in, env, f.st, f.p, now); exit != 2 || err != nil {
				t.Errorf("exit = %d, %v", exit, err)
			}
		}
		if g := f.get(t, s.ID); g.State != model.StateRunning || g.LastEventAt.Equal(now) {
			t.Errorf("PreToolUse changed the row: %+v", g)
		}
	})

	t.Run("symlinked repos root", func(t *testing.T) {
		// Paths.Repos itself reached through a symlink: compare against the real root too.
		g := setup(t)
		real := filepath.Join(g.root, "real-repos")
		if err := os.Rename(g.p.Repos, real); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, g.p.Repos); err != nil {
			t.Fatal(err)
		}
		in := Input{Cwd: g.p.TaskDir("fix"), ToolInput: toolInput("file_path", filepath.Join(real, "docs", "a.md"))}
		if exit, _, err := Handle(ctx, EventPreToolUse, in, envOf(map[string]string{"ATL_TASK": "fix"}), g.st, g.p, now); exit != 2 {
			t.Errorf("exit = %d, %v", exit, err)
		}
	})
}

// TestUnreadableInput covers PreToolUse input that is oversized, truncated
// or malformed: the guard still runs on a recoverable path, and oversized
// input it cannot clear is refused.
func TestUnreadableInput(t *testing.T) {
	f := setup(t)
	old := maxInput
	maxInput = 4096
	t.Cleanup(func() { maxInput = old })
	docs := filepath.Join(f.p.RepoPath("docs"), "a.md")
	rw := filepath.Join(f.p.WorktreePath("fix", "api"), "main.go")
	big := strings.Repeat("x", 10000)
	head := func(path string) string {
		return `{"session_id":"s","cwd":"` + f.p.TaskDir("fix") + `","hook_event_name":"PreToolUse","tool_name":"Write","tool_input":{"file_path":"` + path + `","content":"`
	}
	env := envOf(map[string]string{"ATL_TASK": "fix"})
	for _, c := range []struct {
		name, event, stdin, wantErr string
		want                        int
	}{
		{"oversized write into ro", EventPreToolUse, head(docs) + big + `"}}`, "inside read-only repo docs", 2},
		{"oversized write into rw refused", EventPreToolUse, head(rw) + big + `"}}`, "exceeds 4096 bytes", 2},
		{"oversized without recoverable path refused", EventPreToolUse,
			`{"session_id":"s","tool_name":"Write","tool_input":{"content":"` + big + `","file_path":"` + docs + `"}}`, "exceeds 4096 bytes", 2},
		{"truncated write into ro", EventPreToolUse, head(docs) + "abc", "inside read-only repo docs", 2},
		{"truncated notebook into ro", EventPreToolUse,
			`{"cwd":"/","tool_input":{"notebook_path":"` + docs + `","new_source":"x`, "inside read-only repo docs", 2},
		{"truncated write into rw allowed", EventPreToolUse, head(rw) + "abc", "", 0},
		{"truncated duplicate key, last ro", EventPreToolUse,
			`{"cwd":"/","tool_input":{"file_path":"` + rw + `","file_path":"` + docs + `","content":"x`, "inside read-only repo docs", 2},
		{"truncated duplicate key, last rw", EventPreToolUse,
			`{"cwd":"/","tool_input":{"file_path":"` + docs + `","file_path":"` + rw + `","content":"x`, "", 0},
		{"truncated notebook ro beside rw file_path", EventPreToolUse,
			`{"cwd":"/","tool_input":{"notebook_path":"` + docs + `","file_path":"` + rw + `","new_source":"x`, "inside read-only repo docs", 2},
		{"truncated later tool_input wins", EventPreToolUse,
			`{"cwd":"/","tool_input":{"file_path":"` + docs + `"},"tool_input":{"file_path":"` + rw + `","content":"x`, "", 0},
		{"malformed without path fails open", EventPreToolUse, `{"tool_input":`, "", 0},
		{"oversized Stop ignored", EventStop, `{"session_id":"s","x":"` + big + `"}`, "", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			var errb bytes.Buffer
			code := run([]string{c.event}, strings.NewReader(c.stdin), &errb, env)
			if code != c.want || !strings.Contains(errb.String(), c.wantErr) {
				t.Errorf("exit=%d stderr=%q; want %d with %q", code, errb.String(), c.want, c.wantErr)
			}
			if c.want == 0 && errb.Len() != 0 {
				t.Errorf("unexpected stderr %q", errb.String())
			}
		})
	}
}

// runMain re-execs the test binary as `atl hook <args>`.
func runMain(t *testing.T, args, stdin string) (exit int, stdout, stderr string, wall time.Duration) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "ATL_HOOK_TEST_MAIN=1", "ATL_HOOK_TEST_ARGS="+args)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	start := time.Now()
	err := cmd.Run()
	wall = time.Since(start)
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return exit, out.String(), errb.String(), wall
}

func readLog(t *testing.T, p config.Paths) string {
	t.Helper()
	b, err := os.ReadFile(p.HookLog())
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func TestMainProcess(t *testing.T) {
	f := setup(t)
	s := f.session(t, model.StateIdle, "c-main")
	f.st.Close() // the child opens its own handle

	t.Run("malformed JSON is logged, exit 0", func(t *testing.T) {
		exit, out, _, _ := runMain(t, "Stop", "{not json")
		if exit != 0 || out != "" {
			t.Errorf("exit=%d stdout=%q", exit, out)
		}
		log := readLog(t, f.p)
		if !strings.Contains(log, "event=Stop") || !strings.Contains(log, "decode hook input") {
			t.Errorf("hook.log = %q", log)
		}
		line := strings.SplitN(log, " ", 2)[0]
		if _, err := time.Parse(time.RFC3339, line); err != nil {
			t.Errorf("log line does not start with RFC3339 timestamp: %q", log)
		}
	})

	t.Run("malformed JSON on PreToolUse fails open", func(t *testing.T) {
		if exit, out, _, _ := runMain(t, "PreToolUse", "]]"); exit != 0 || out != "" {
			t.Errorf("exit=%d stdout=%q", exit, out)
		}
	})

	t.Run("missing event is logged", func(t *testing.T) {
		if exit, out, _, _ := runMain(t, "", "{}"); exit != 0 || out != "" {
			t.Errorf("exit=%d stdout=%q", exit, out)
		}
		if !strings.Contains(readLog(t, f.p), "missing event") {
			t.Errorf("hook.log = %q", readLog(t, f.p))
		}
	})

	t.Run("state update, no stdout", func(t *testing.T) {
		exit, out, errOut, _ := runMain(t, "UserPromptSubmit", `{"session_id":"c-main","cwd":"/x","hook_event_name":"UserPromptSubmit","prompt":"hi"}`)
		if exit != 0 || out != "" || errOut != "" {
			t.Errorf("exit=%d stdout=%q stderr=%q", exit, out, errOut)
		}
		st, err := store.Open(f.p.DB)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if g, _ := st.GetSession(context.Background(), s.ID); g.State != model.StateRunning {
			t.Errorf("state = %s", g.State)
		}
	})

	t.Run("ro block prints to stderr, exit 2", func(t *testing.T) {
		in, _ := json.Marshal(Input{SessionID: "c-main", Cwd: f.p.TaskDir("fix"), HookEventName: "PreToolUse",
			ToolName: "Edit", ToolInput: toolInput("file_path", filepath.Join(f.p.RepoPath("docs"), "README.md"))})
		exit, out, errOut, _ := runMain(t, "PreToolUse", string(in))
		if exit != 2 || out != "" || !strings.Contains(errOut, "inside read-only repo docs") {
			t.Errorf("exit=%d stdout=%q stderr=%q", exit, out, errOut)
		}
	})

	t.Run("large write into ro is blocked", func(t *testing.T) {
		b, _ := json.Marshal(map[string]string{"file_path": filepath.Join(f.p.RepoPath("docs"), "big.txt"),
			"content": strings.Repeat("0123456789abcdef", 1<<17)}) // 2 MiB
		in, _ := json.Marshal(Input{SessionID: "c-main", Cwd: f.p.TaskDir("fix"), HookEventName: "PreToolUse",
			ToolName: "Write", ToolInput: b})
		exit, out, errOut, _ := runMain(t, "PreToolUse", string(in))
		if exit != 2 || out != "" || !strings.Contains(errOut, "inside read-only repo docs") {
			t.Errorf("exit=%d stdout=%q stderr=%q", exit, out, errOut)
		}
	})

	t.Run("latency", func(t *testing.T) {
		if raceEnabled {
			t.Skip("race-instrumented binaries start too slowly to measure hook latency")
		}
		in := `{"session_id":"c-main","cwd":"` + f.p.TaskDir("fix") + `","hook_event_name":"SessionStart","source":"startup","transcript_path":"/tmp/t.jsonl"}`
		runMain(t, "SessionStart", in) // warm the page cache
		var walls []time.Duration
		for i := 0; i < 7; i++ {
			exit, out, errOut, wall := runMain(t, "SessionStart", in)
			if exit != 0 || out != "" || errOut != "" {
				t.Fatalf("exit=%d stdout=%q stderr=%q", exit, out, errOut)
			}
			walls = append(walls, wall)
		}
		sort.Slice(walls, func(i, j int) bool { return walls[i] < walls[j] })
		median := walls[len(walls)/2]
		t.Logf("SessionStart hook wall time (re-exec of test binary): min %v, median %v, max %v", walls[0], median, walls[len(walls)-1])
		if median > 200*time.Millisecond {
			t.Errorf("median hook latency %v exceeds 200ms", median)
		}
		if strings.Contains(readLog(t, f.p), "SessionStart") {
			t.Errorf("unexpected errors logged: %s", readLog(t, f.p))
		}
	})
}

// TestROGuardRepoAddedLater: the guard reads the task's repos from the DB
// on every call, so an ro repo added to a running task (atl add) is
// protected immediately, with no session restart.
func TestROGuardRepoAddedLater(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	target := filepath.Join(f.p.RepoPath("docs2"), "a.md")
	in := Input{SessionID: "s", Cwd: f.p.TaskDir("fix"), ToolName: "Edit", ToolInput: toolInput("file_path", target)}
	env := envOf(map[string]string{"ATL_TASK": "fix"})
	if exit, msg, err := Handle(ctx, EventPreToolUse, in, env, f.st, f.p, now); exit != 0 || err != nil {
		t.Fatalf("before add: %d %q %v", exit, msg, err)
	}
	if err := f.st.AddTaskRepo(ctx, f.task.ID, model.TaskRepo{Name: "docs2", Mode: model.ModeRO}); err != nil {
		t.Fatal(err)
	}
	exit, msg, err := Handle(ctx, EventPreToolUse, in, env, f.st, f.p, now)
	if exit != 2 || err != nil || !strings.Contains(msg, "inside read-only repo docs2") {
		t.Fatalf("after add: %d %q %v", exit, msg, err)
	}
}

// TestTranscriptPath: SessionStart (every source) stores transcript_path;
// other events fill it only while empty and never overwrite it.
func TestTranscriptPath(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	s := f.session(t, model.StateIdle, "")
	env := envOf(map[string]string{"ATL_SESSION": itoa(s.ID), "ATL_TASK": "fix"})
	cwd := f.p.TaskDir("fix")
	step := func(event string, in Input, want string) {
		t.Helper()
		in.Cwd = cwd
		if _, _, err := Handle(ctx, event, in, env, f.st, f.p, now); err != nil {
			t.Fatalf("%s: %v", event, err)
		}
		if got := f.get(t, s.ID).TranscriptPath; got != want {
			t.Fatalf("after %s %+v: transcript = %q, want %q", event, in, got, want)
		}
	}
	step(EventUserPromptSubmit, Input{}, "")                                               // no path: nothing
	step(EventUserPromptSubmit, Input{TranscriptPath: "/t/early.jsonl"}, "/t/early.jsonl") // fills empty
	step(EventStop, Input{TranscriptPath: "/t/other.jsonl"}, "/t/early.jsonl")             // never overwrites
	step(EventSessionStart, Input{SessionID: "c1", Source: "startup", TranscriptPath: "/t/c1.jsonl"}, "/t/c1.jsonl")
	step(EventNotification, Input{SessionID: "c1", Message: "x", TranscriptPath: "/t/zz.jsonl"}, "/t/c1.jsonl")
	step(EventPostToolUse, Input{SessionID: "c1", TranscriptPath: "/t/zz.jsonl"}, "/t/c1.jsonl")
	step(EventSessionEnd, Input{SessionID: "c1", TranscriptPath: "/t/zz.jsonl"}, "/t/c1.jsonl")
	step(EventSessionStart, Input{SessionID: "c2", Source: "resume", TranscriptPath: "/t/c2.jsonl"}, "/t/c2.jsonl")
	step(EventSessionStart, Input{SessionID: "c3", Source: "compact", TranscriptPath: "/t/c3.jsonl"}, "/t/c3.jsonl")
	step(EventSessionStart, Input{SessionID: "c4", Source: "clear"}, "/t/c3.jsonl") // empty path keeps the old one
	if g := f.get(t, s.ID); g.ClaudeSessionID != "c4" || g.State != model.StateIdle {
		t.Fatalf("row = %+v", g)
	}

	// A session row created by SessionStart (claude not launched by atl)
	// gets the path at creation.
	env2 := envOf(map[string]string{"ATL_TASK": "fix"})
	in := Input{SessionID: "fresh", Source: "startup", Cwd: cwd, TranscriptPath: "/t/fresh.jsonl"}
	if _, _, err := Handle(ctx, EventSessionStart, in, env2, f.st, f.p, now); err != nil {
		t.Fatal(err)
	}
	fresh, err := f.st.GetSessionByClaudeID(ctx, "fresh")
	if err != nil || fresh.TranscriptPath != "/t/fresh.jsonl" {
		t.Fatalf("fresh = %+v, %v", fresh, err)
	}
}
