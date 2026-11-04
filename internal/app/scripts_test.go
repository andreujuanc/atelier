package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andreujuanc/atelier/internal/model"
)

func (e *taskEnv) writeProject(t *testing.T, name, body string) {
	t.Helper()
	pf := e.app.Paths.ProjectFile(name)
	if err := os.MkdirAll(filepath.Dir(pf), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pf, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *taskEnv) outString() string { return e.app.Out.(*bytes.Buffer).String() }

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNewTaskRunsSetup(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "lib", "main")
	e.addRepo(t, "docs", "main")
	// Occupy offset 0 so the project task gets a non-zero one.
	if _, err := e.app.NewTask(ctx, NewTaskOpts{Title: "first", Repos: []RepoArg{{"docs", model.ModeRO}}}); err != nil {
		t.Fatal(err)
	}
	e.writeProject(t, "shop", `
setup = '''printf '%s %s %s' "$ATL_PORT_OFFSET" "$ATL_REPO" "$FOO" > setup.out; echo "hello from $ATL_REPO"; echo "in $ATL_WORKTREE on $ATL_BRANCH task $ATL_TASK dir $ATL_TASK_DIR" > env.out'''
[[repos]]
name = "api"
[[repos]]
name = "lib"
[[repos]]
name = "docs"
mode = "ro"
[env]
FOO = "bar"
ATL_PORT_OFFSET = "9999"
`)
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "with setup", Project: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if task.PortOffset != 100 {
		t.Fatalf("port offset %d", task.PortOffset)
	}
	for _, repo := range []string{"api", "lib"} {
		wt := e.app.Paths.WorktreePath(task.Slug, repo)
		if got := readFile(t, filepath.Join(wt, "setup.out")); got != "100 "+repo+" bar" {
			t.Errorf("%s setup.out = %q", repo, got)
		}
		want := "in " + wt + " on atl/with-setup task with-setup dir " + e.app.Paths.TaskDir(task.Slug) + "\n"
		if got := readFile(t, filepath.Join(wt, "env.out")); got != want {
			t.Errorf("%s env.out = %q, want %q", repo, got, want)
		}
		if !strings.Contains(e.outString(), "setup "+repo+": hello from "+repo+"\n") {
			t.Errorf("output lacks %s progress:\n%s", repo, e.outString())
		}
	}
	if exists(filepath.Join(e.canonical("docs"), "setup.out")) {
		t.Fatal("setup ran in a ro repo")
	}
}

func TestNewTaskSetupFailure(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "lib", "main")
	e.writeProject(t, "shop", `
setup = '[ "$ATL_REPO" = api ] && { echo boom >&2; exit 7; }; touch ok'
[[repos]]
name = "api"
[[repos]]
name = "lib"
`)
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "bad setup", Project: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	out := e.outString()
	if !strings.Contains(out, "warning: setup failed in api: script failed: exit status 7") ||
		!strings.Contains(out, "setup api: boom\n") {
		t.Fatalf("output:\n%s", out)
	}
	if !exists(filepath.Join(e.app.Paths.WorktreePath(task.Slug, "lib"), "ok")) {
		t.Fatal("setup did not continue with lib after api failed")
	}
	if _, err := e.app.Store.GetTaskBySlug(ctx, task.Slug); err != nil {
		t.Fatal("task not kept:", err)
	}
	if !exists(e.app.Paths.WorktreePath(task.Slug, "api")) {
		t.Fatal("worktree removed after setup failure")
	}
}

func TestDoneRunsTeardown(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "lib", "main")
	e.addRepo(t, "web", "main")
	marker := filepath.Join(t.TempDir(), "marker")
	// Reads a file inside the worktree: proves teardown runs before removal.
	e.writeProject(t, "shop", `
teardown = 'printf "%s:%s:%s\n" "$ATL_REPO" "$(cat "$ATL_WORKTREE/README")" "$ATL_PORT_OFFSET" >> "$MARKER"'
[[repos]]
name = "api"
[[repos]]
name = "lib"
[[repos]]
name = "web"
[env]
MARKER = "`+marker+`"
`)
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "td", Project: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	// A worktree deleted by hand is skipped.
	if err := os.RemoveAll(e.app.Paths.WorktreePath(task.Slug, "lib")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.Done(ctx, task.Slug, false); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, marker); got != "api:README:0\nweb:README:0\n" {
		t.Fatalf("marker = %q", got)
	}
	if exists(e.app.Paths.WorktreePath(task.Slug, "api")) {
		t.Fatal("worktree not removed")
	}
	st, _ := e.app.Store.GetTaskBySlug(ctx, task.Slug)
	if st.Status != model.StatusDone {
		t.Fatalf("status %s", st.Status)
	}
}

func TestTeardownFailureAbortsUnlessForce(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.writeProject(t, "shop", "teardown = 'echo nope >&2; exit 4'\n[[repos]]\nname = \"api\"\n")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "td fail", Project: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	wt := e.app.Paths.WorktreePath(task.Slug, "api")
	for _, cancel := range []bool{false, true} {
		finish := e.app.Done
		if cancel {
			finish = e.app.Cancel
		}
		_, err := finish(ctx, task.Slug, false)
		if err == nil || !strings.Contains(err.Error(), "teardown") || !strings.Contains(err.Error(), "exit status 4") {
			t.Fatalf("err = %v", err)
		}
		st, _ := e.app.Store.GetTaskBySlug(ctx, task.Slug)
		if st.Status != model.StatusActive || !exists(wt) {
			t.Fatalf("status %s, worktree exists %v", st.Status, exists(wt))
		}
	}
	got, err := e.app.Done(ctx, task.Slug, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusDone || exists(wt) {
		t.Fatalf("status %s, worktree exists %v", got.Status, exists(wt))
	}
	if !strings.Contains(e.outString(), "warning: teardown failed in api: script failed: exit status 4") {
		t.Fatalf("output:\n%s", e.outString())
	}
}

func TestTeardownProjectGone(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.writeProject(t, "shop", "teardown = 'exit 1'\n[[repos]]\nname = \"api\"\n")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "gone", Project: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(e.app.Paths.ProjectFile("shop")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.Done(ctx, task.Slug, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.outString(), "warning: skipping teardown: project \"shop\" not found") {
		t.Fatalf("output:\n%s", e.outString())
	}
}

func TestLaunchEnvProject(t *testing.T) {
	e := newTaskEnv(t)
	e.writeProject(t, "shop", "[env]\nFOO = \"bar\"\nATL_PORT_OFFSET = \"9999\"\nATL_SESSION = \"x\"\n")
	task := &model.Task{Slug: "s", Project: "shop", PortOffset: 300}
	env := e.app.launchEnv(task, &model.Session{ID: 7})
	want := map[string]string{"FOO": "bar", "ATL_PORT_OFFSET": "300", "ATL_SESSION": "7", "ATL_TASK": "s",
		"ATL_TASK_DIR": e.app.Paths.TaskDir("s")}
	if len(env) != len(want) {
		t.Fatalf("env = %v", env)
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	// Project file gone (or no project): ATL_* only.
	task.Project = "missing"
	if env := e.app.launchEnv(task, &model.Session{ID: 7}); len(env) != 4 || env["ATL_PORT_OFFSET"] != "300" {
		t.Fatalf("env = %v", env)
	}
	if !strings.Contains(e.outString(), `warning: project "missing" not found`) ||
		!strings.Contains(e.outString(), "; launching without project env\n") {
		t.Fatalf("output:\n%s", e.outString())
	}
}

func TestTeardownLeavesChanges(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.writeProject(t, "shop", "teardown = 'echo x > teardown.log'\n[[repos]]\nname = \"api\"\n")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "td dirty", Project: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	wt := e.app.Paths.WorktreePath(task.Slug, "api")
	_, err = e.app.Done(ctx, task.Slug, false)
	if err == nil || !strings.Contains(err.Error(), "worktree has changes after teardown") ||
		!strings.Contains(err.Error(), "--force") {
		t.Fatalf("err = %v", err)
	}
	if !exists(wt) {
		t.Fatal("worktree removed")
	}
	if os.Remove(filepath.Join(wt, "teardown.log")) != nil {
		t.Fatal("teardown.log missing")
	}
	got, err := e.app.Done(ctx, task.Slug, true)
	if err != nil || got.Status != model.StatusDone || exists(wt) {
		t.Fatalf("force: %v, worktree exists %v", err, exists(wt))
	}
}

func TestPrefixWriter(t *testing.T) {
	var buf bytes.Buffer
	p := &prefixWriter{w: &buf, prefix: "setup api: "}
	for _, s := range []string{"one\n", "10%\r20", "%\r", "done\r\n", "\n", "tail"} {
		p.Write([]byte(s))
	}
	p.Flush()
	want := "setup api: one\nsetup api: 10%\nsetup api: 20%\nsetup api: done\nsetup api: tail\n"
	if buf.String() != want {
		t.Fatalf("got %q\nwant %q", buf.String(), want)
	}
}
