package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/gen"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/store"
	"github.com/andreujuanc/atelier/internal/tmux"
)

type taskEnv struct {
	app  *App
	root string
	sock string
}

// newTaskEnv builds an App on temp repos/tasks/DB dirs with an isolated
// tmux socket (server killed at cleanup) and AtlBin /usr/local/bin/atl.
func newTaskEnv(t *testing.T) *taskEnv {
	t.Helper()
	root := t.TempDir()
	t.Setenv("TMUX", "")
	sock := fmt.Sprintf("atltask-%d-%d", os.Getpid(), sockSeq.Add(1))
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", sock, "kill-server").Run() })
	paths := config.Paths{
		Repos:     filepath.Join(root, "repos"),
		Tasks:     filepath.Join(root, "tasks"),
		DB:        filepath.Join(root, "data", "atelier.db"),
		StateDir:  filepath.Join(root, "state"),
		ConfigDir: filepath.Join(root, "config"),
	}
	st, err := store.Open(paths.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &taskEnv{
		app: &App{Paths: paths, Store: st, Tmux: &tmux.Client{Socket: sock}, Out: &bytes.Buffer{},
			Now: time.Now, AtlBin: "/usr/local/bin/atl", CheckBin: anyBin},
		root: root,
		sock: sock,
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s %v: %v\n%s", dir, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (e *taskEnv) origin(name string) string { return filepath.Join(e.root, "origins", name+".git") }
func (e *taskEnv) seed(name string) string   { return filepath.Join(e.root, "seeds", name) }

// addRepo creates a bare origin for name whose default branch is branch
// (with one commit) and clones it to $ATL_REPOS/name.
func (e *taskEnv) addRepo(t *testing.T, name, branch string) {
	t.Helper()
	origin, seed := e.origin(name), e.seed(name)
	for _, d := range []string{origin, seed, e.app.Paths.Repos} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	git(t, origin, "init", "-q", "--bare", "-b", branch)
	git(t, seed, "init", "-q", "-b", branch)
	git(t, seed, "remote", "add", "origin", origin)
	e.pushOrigin(t, name, "README")
	git(t, e.app.Paths.Repos, "clone", "-q", origin, name)
}

// pushOrigin commits a new file in name's seed clone, pushes it to origin's
// current branch and returns the new sha.
func (e *taskEnv) pushOrigin(t *testing.T, name, file string) string {
	t.Helper()
	seed := e.seed(name)
	if err := os.WriteFile(filepath.Join(seed, file), []byte(file+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", file)
	git(t, seed, "commit", "-q", "-m", "add "+file)
	git(t, seed, "push", "-q", "origin", "HEAD")
	return git(t, seed, "rev-parse", "HEAD")
}

func (e *taskEnv) canonical(name string) string { return e.app.Paths.RepoPath(name) }

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func countRows(t *testing.T, e *taskEnv) int {
	t.Helper()
	tasks, err := e.app.Store.ListTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return len(tasks)
}

func worktreeCount(t *testing.T, repo string) int {
	t.Helper()
	return strings.Count(git(t, repo, "worktree", "list", "--porcelain"), "worktree ")
}

func TestNewTask(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "lib", "trunk")
	e.addRepo(t, "docs", "main")

	// Canonical api: local main ahead of origin, and origin ahead of the
	// canonical clone's last fetch. The task must start at origin's tip.
	api := e.canonical("api")
	if err := os.WriteFile(filepath.Join(api, "local"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, api, "add", "local")
	git(t, api, "commit", "-q", "-m", "local only")
	localMain := git(t, api, "rev-parse", "main")
	tip := e.pushOrigin(t, "api", "new-on-origin")

	task, err := e.app.NewTask(ctx, NewTaskOpts{
		Title: "  Fix Login!  ", Description: "desc",
		Repos: []RepoArg{{"api", model.ModeRW}, {"lib", model.ModeRW}, {"docs", model.ModeRO}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.Slug != "fix-login" || task.Title != "Fix Login!" || task.Branch != "atl/fix-login" ||
		task.PortOffset != 0 || task.Status != model.StatusActive {
		t.Fatalf("task = %+v", task)
	}
	dir := e.app.Paths.TaskDir("fix-login")
	wt := filepath.Join(dir, "api")
	if got := git(t, wt, "rev-parse", "HEAD"); got != tip || got == localMain {
		t.Fatalf("worktree HEAD %s, want origin tip %s (local main %s)", got, tip, localMain)
	}
	if b := git(t, wt, "symbolic-ref", "--short", "HEAD"); b != "atl/fix-login" {
		t.Fatalf("branch %s", b)
	}
	if b := git(t, filepath.Join(dir, "lib"), "symbolic-ref", "--short", "HEAD"); b != "atl/fix-login" {
		t.Fatalf("lib branch %s", b)
	}
	if exists(filepath.Join(dir, "docs")) {
		t.Fatal("ro repo must not get a worktree")
	}
	stored, err := e.app.Store.GetTaskBySlug(ctx, "fix-login")
	if err != nil {
		t.Fatal(err)
	}
	want := []model.TaskRepo{
		{Name: "api", Mode: model.ModeRW, BaseBranch: "main", BaseCommit: tip},
		{Name: "lib", Mode: model.ModeRW, BaseBranch: "trunk", BaseCommit: git(t, e.origin("lib"), "rev-parse", "trunk")},
		{Name: "docs", Mode: model.ModeRO},
	}
	if fmt.Sprint(stored.Repos) != fmt.Sprint(want) {
		t.Fatalf("repos = %+v\nwant %+v", stored.Repos, want)
	}
	md, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"Fix Login!", wt, e.canonical("docs"), "ATL_PORT_OFFSET=0"} {
		if !strings.Contains(string(md), s) {
			t.Errorf("CLAUDE.md lacks %q", s)
		}
	}
	settings, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if err != nil || !strings.Contains(string(settings), gen.HookCommand("/usr/local/bin/atl", "SessionStart")) {
		t.Fatalf("settings.json: %v\n%s", err, settings)
	}

	// Same title again, backlog, no fetch: next slug and port, and the base
	// is origin/main as of the last fetch, not origin's newer tip.
	newer := e.pushOrigin(t, "api", "even-newer")
	t2, err := e.app.NewTask(ctx, NewTaskOpts{Title: "Fix login", Backlog: true, NoFetch: true,
		Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	if t2.Slug != "fix-login-2" || t2.PortOffset != 100 || t2.Status != model.StatusBacklog {
		t.Fatalf("t2 = %+v", t2)
	}
	if got := git(t, e.app.Paths.WorktreePath(t2.Slug, "api"), "rev-parse", "HEAD"); got != tip {
		t.Fatalf("--no-fetch worktree at %s, want %s (not %s)", got, tip, newer)
	}

	// With fetch, the newest origin commit is used.
	t3, err := e.app.NewTask(ctx, NewTaskOpts{Title: "third", Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := git(t, e.app.Paths.WorktreePath(t3.Slug, "api"), "rev-parse", "HEAD"); got != newer || t3.PortOffset != 200 {
		t.Fatalf("fetched worktree at %s, want %s; port %d", got, newer, t3.PortOffset)
	}
}

func TestNewTaskValidation(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	if err := os.MkdirAll(filepath.Join(e.app.Paths.Repos, "plain"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		o    NewTaskOpts
		want string
	}{
		{"no title", NewTaskOpts{Title: "  ", Repos: []RepoArg{{"api", model.ModeRW}}}, "title"},
		{"no repos", NewTaskOpts{Title: "x"}, "no repos"},
		{"escape", NewTaskOpts{Title: "a\x1bb", Repos: []RepoArg{{"api", model.ModeRW}}}, "control characters"},
		{"bad utf-8", NewTaskOpts{Title: "a\xffb", Repos: []RepoArg{{"api", model.ModeRW}}}, "not valid UTF-8"},
		{"missing", NewTaskOpts{Title: "x", Repos: []RepoArg{{"nope", model.ModeRW}}}, "does not exist"},
		{"plain dir", NewTaskOpts{Title: "x", Repos: []RepoArg{{"plain", model.ModeRO}}}, "not a git repository"},
		{"bad name", NewTaskOpts{Title: "x", Repos: []RepoArg{{"../api", model.ModeRW}}}, "invalid repo name"},
		{"dot", NewTaskOpts{Title: "x", Repos: []RepoArg{{".", model.ModeRW}}}, "invalid repo name"},
		{"dotdot", NewTaskOpts{Title: "x", Repos: []RepoArg{{"..", model.ModeRO}}}, "invalid repo name"},
		{".claude", NewTaskOpts{Title: "x", Repos: []RepoArg{{".claude", model.ModeRO}}}, "invalid repo name"},
		{".CLAUDE", NewTaskOpts{Title: "x", Repos: []RepoArg{{".Claude", model.ModeRW}}}, "invalid repo name"},
		{"CLAUDE.md", NewTaskOpts{Title: "x", Repos: []RepoArg{{"CLAUDE.md", model.ModeRO}}}, "invalid repo name"},
		{"claude.md", NewTaskOpts{Title: "x", Repos: []RepoArg{{"claude.MD", model.ModeRW}}}, "invalid repo name"},
		{"bad mode", NewTaskOpts{Title: "x", Repos: []RepoArg{{"api", "rx"}}}, "invalid mode"},
		{"unknown project", NewTaskOpts{Title: "x", Project: "shop"}, "not found"},
	}
	for _, c := range cases {
		if _, err := e.app.NewTask(ctx, c.o); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	if n := countRows(t, e); n != 0 {
		t.Fatalf("%d task rows after failures", n)
	}
}

func TestNewTaskRollback(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "web", "main")
	opts := NewTaskOpts{Title: "Roll back", Repos: []RepoArg{{"api", model.ModeRW}, {"web", model.ModeRW}}}

	// Second rw repo has no origin: fails before anything is created.
	noOrigin := e.canonical("solo")
	if err := os.MkdirAll(noOrigin, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, noOrigin, "init", "-q", "-b", "main")
	git(t, noOrigin, "commit", "-q", "--allow-empty", "-m", "init")
	if _, err := e.app.NewTask(ctx, NewTaskOpts{Title: "Roll back",
		Repos: []RepoArg{{"api", model.ModeRW}, {"solo", model.ModeRW}}}); err == nil || !strings.Contains(err.Error(), "origin") {
		t.Fatalf("no-origin err = %v", err)
	}

	// The second worktree cannot be created (its checkout fails: a blob is
	// unreadable; a branch in the way of atl/roll-back would only change
	// the slug): the worktrees, their branches, the task dir and the row
	// are undone.
	web := e.canonical("web")
	git(t, web, "branch", "keep-me")
	restore := breakCheckout(t, web, "origin/main:README")
	_, err := e.app.NewTask(ctx, opts)
	restore()
	if err == nil || !strings.Contains(err.Error(), "web") {
		t.Fatalf("err = %v", err)
	}
	if n := countRows(t, e); n != 0 {
		t.Fatalf("%d task rows after rollback", n)
	}
	if exists(e.app.Paths.TaskDir("roll-back")) {
		t.Fatal("task dir left behind")
	}
	if n := worktreeCount(t, e.canonical("api")); n != 1 {
		t.Fatalf("api has %d worktrees after rollback", n)
	}
	if out, err := exec.Command("git", "-C", e.canonical("api"), "rev-parse", "--verify", "-q", "refs/heads/atl/roll-back").CombinedOutput(); err == nil {
		t.Fatalf("branch left in api: %s", out)
	}
	if n := worktreeCount(t, web); n != 1 {
		t.Fatalf("web has %d worktrees after rollback", n)
	}
	if out, err := exec.Command("git", "-C", web, "rev-parse", "--verify", "-q", "refs/heads/atl/roll-back").CombinedOutput(); err == nil {
		t.Fatalf("branch left in web: %s", out)
	}
	if git(t, web, "rev-parse", "--verify", "refs/heads/keep-me") == "" {
		t.Fatal("pre-existing branch in web must not be deleted")
	}

	// Once the checkout works the same slug works.
	task, err := e.app.NewTask(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if task.Slug != "roll-back" {
		t.Fatalf("slug %s", task.Slug)
	}
}

// breakCheckout makes a blob of rev unreadable in repo (a loose object), so
// that `git worktree add` fails in its checkout, after it created the
// branch. It returns the undo.
func breakCheckout(t *testing.T, repo, rev string) func() {
	t.Helper()
	oid := git(t, repo, "rev-parse", rev)
	path := filepath.Join(repo, ".git", "objects", oid[:2], oid[2:])
	if err := os.Rename(path, path+".gone"); err != nil {
		t.Fatalf("%s is not a loose object: %v", rev, err)
	}
	return func() {
		if err := os.Rename(path+".gone", path); err != nil {
			t.Fatal(err)
		}
	}
}

// A failed checkout makes `git worktree add` fail after it has created the
// branch: what it made must be rolled back. (Hooks, the old way to make it
// fail late, no longer run.)
func TestNewTaskRollbackHookFailure(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	api := e.canonical("api")
	restore := breakCheckout(t, api, "origin/main:README")
	opts := NewTaskOpts{Title: "Hooked", NoFetch: true, Repos: []RepoArg{{"api", model.ModeRW}}}
	if _, err := e.app.NewTask(ctx, opts); err == nil {
		t.Fatal("NewTask succeeded despite the failed checkout")
	}
	if out, err := exec.Command("git", "-C", api, "rev-parse", "--verify", "-q", "refs/heads/atl/hooked").CombinedOutput(); err == nil {
		t.Fatalf("branch left behind: %s", out)
	}
	if n := worktreeCount(t, api); n != 1 {
		t.Fatalf("%d worktrees after rollback:\n%s", n, git(t, api, "worktree", "list"))
	}
	if n := countRows(t, e); n != 0 {
		t.Fatalf("%d task rows after rollback", n)
	}
	if exists(e.app.Paths.TaskDir("hooked")) {
		t.Fatal("task dir left behind")
	}

	restore()
	task, err := e.app.NewTask(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if task.Slug != "hooked" {
		t.Fatalf("slug %s", task.Slug)
	}
}

// TestNewTaskDirExists: a slug whose task dir path is taken on disk (by a
// directory or anything else) gets the next suffix; what is there is left
// alone.
func TestNewTaskDirExists(t *testing.T) {
	e := newTaskEnv(t)
	e.addRepo(t, "api", "main")
	dir := e.app.Paths.TaskDir("taken")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "keep")
	if err := os.WriteFile(keep, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.app.Paths.TaskDir("taken-2"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	task, err := e.app.NewTask(context.Background(), NewTaskOpts{Title: "Taken", Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	if task.Slug != "taken-3" || task.Branch != "atl/taken-3" {
		t.Fatalf("task = %+v", task)
	}
	if !exists(keep) || exists(filepath.Join(dir, "api")) {
		t.Fatal("existing task dir was touched")
	}
	if fi, err := os.Lstat(e.app.Paths.TaskDir("taken-2")); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("the file in the way was touched: %v", err)
	}
	if !exists(e.app.Paths.WorktreePath("taken-3", "api")) {
		t.Fatal("no worktree")
	}
	if n := countRows(t, e); n != 1 {
		t.Fatalf("%d rows", n)
	}
}

// TestNewTaskBranchExists: a slug whose branch atl/<slug> exists in the
// canonical clone of one of the task's rw repos (locally, or on origin as
// fetched) gets the next suffix; the branch is not touched. A branch in a
// repo the task has ro does not count.
func TestNewTaskBranchExists(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "lib", "main")
	api, lib := e.canonical("api"), e.canonical("lib")
	git(t, lib, "branch", "atl/local")
	localTip := git(t, lib, "rev-parse", "atl/local")
	git(t, e.seed("api"), "push", "-q", "origin", "HEAD:refs/heads/atl/remote")
	git(t, api, "branch", "atl/ro-only")

	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "Local", Repos: []RepoArg{{"api", model.ModeRW}, {"lib", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	if task.Slug != "local-2" {
		t.Fatalf("slug %s, want local-2", task.Slug)
	}
	if got := git(t, lib, "rev-parse", "atl/local"); got != localTip {
		t.Fatalf("atl/local moved: %s", got)
	}
	task, err = e.app.NewTask(ctx, NewTaskOpts{Title: "Remote", Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	if task.Slug != "remote-2" { // the fetch brought origin/atl/remote
		t.Fatalf("slug %s, want remote-2", task.Slug)
	}
	task, err = e.app.NewTask(ctx, NewTaskOpts{Title: "Ro only", NoFetch: true,
		Repos: []RepoArg{{"api", model.ModeRO}, {"lib", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	if task.Slug != "ro-only" {
		t.Fatalf("slug %s, want ro-only", task.Slug)
	}

	// A branch below atl/<slug> (atl/<slug>/x) keeps git from creating
	// atl/<slug>: taken too, locally or on origin.
	git(t, lib, "branch", "atl/under/x")
	git(t, e.seed("api"), "push", "-q", "origin", "HEAD:refs/heads/atl/far/y")
	for _, c := range []struct{ title, repo, want string }{{"Under", "lib", "under-2"}, {"Far", "api", "far-2"}} {
		task, err := e.app.NewTask(ctx, NewTaskOpts{Title: c.title, Repos: []RepoArg{{c.repo, model.ModeRW}}})
		if err != nil {
			t.Fatal(err)
		}
		if task.Slug != c.want {
			t.Fatalf("slug %s, want %s", task.Slug, c.want)
		}
	}
	if git(t, lib, "rev-parse", "--verify", "refs/heads/atl/under/x") == "" {
		t.Fatal("atl/under/x gone")
	}
}

func TestNewTaskTitleWhitespace(t *testing.T) {
	e := newTaskEnv(t)
	e.addRepo(t, "api", "main")
	task, err := e.app.NewTask(context.Background(), NewTaskOpts{Title: " Fix\n  the\t\r\nlogin  ",
		Description: "line one\nline two", NoFetch: true, Repos: []RepoArg{{"api", model.ModeRO}}})
	if err != nil {
		t.Fatal(err)
	}
	if task.Title != "Fix the login" || task.Slug != "fix-the-login" || task.Description != "line one\nline two" {
		t.Fatalf("task = %+v", task)
	}
}

// TestNewTaskDescriptionControl: control characters in a description are
// dropped as in notes (newlines and tabs kept, \r\n made \n), in the
// database and in CLAUDE.md.
func TestNewTaskDescriptionControl(t *testing.T) {
	e := newTaskEnv(t)
	e.addRepo(t, "api", "main")
	task, err := e.app.NewTask(context.Background(), NewTaskOpts{Title: "Fix",
		Description: " \x1b[2Jline\a one\r\n\tdetail\x00 \n", NoFetch: true, Repos: []RepoArg{{"api", model.ModeRO}}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "[2Jline one\n\tdetail"; task.Description != want {
		t.Fatalf("description = %q, want %q", task.Description, want)
	}
	md, err := os.ReadFile(filepath.Join(e.app.Paths.TaskDir(task.Slug), "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(string(md), "\x1b\a\r\x00") || !strings.Contains(string(md), "[2Jline one\n\tdetail") {
		t.Fatalf("CLAUDE.md:\n%q", md)
	}
}

func TestNewTaskProject(t *testing.T) {
	e := newTaskEnv(t)
	e.addRepo(t, "api", "main")
	e.addRepo(t, "lib", "main")
	e.addRepo(t, "docs", "main")
	pf := e.app.Paths.ProjectFile("shop")
	if err := os.MkdirAll(filepath.Dir(pf), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pf, []byte("[[repos]]\nname = \"api\"\n[[repos]]\nname = \"lib\"\nmode = \"rw\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	task, err := e.app.NewTask(context.Background(), NewTaskOpts{Title: "p", Project: "shop",
		Repos: []RepoArg{{"lib", model.ModeRO}, {"docs", model.ModeRO}}})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range task.Repos {
		got = append(got, r.Name+":"+string(r.Mode))
	}
	if strings.Join(got, " ") != "api:rw lib:ro docs:ro" || task.Project != "shop" {
		t.Fatalf("repos %v project %q", got, task.Project)
	}
	if exists(e.app.Paths.WorktreePath("p", "lib")) {
		t.Fatal("lib overridden to ro but got a worktree")
	}
}

func TestGetListSetStatus(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "web", "main")
	e.addRepo(t, "docs", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "view",
		Repos: []RepoArg{{"api", model.ModeRW}, {"web", model.ModeRW}, {"docs", model.ModeRO}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.app.Paths.WorktreePath("view", "api"), "dirty"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(e.app.Paths.WorktreePath("view", "web")); err != nil {
		t.Fatal(err)
	}
	s := &model.Session{TaskID: task.ID, State: model.StateWaiting}
	if err := e.app.Store.CreateSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	v, err := e.app.Get(ctx, "vi")
	if err != nil {
		t.Fatal(err)
	}
	want := []RepoStatus{
		{Name: "api", Mode: model.ModeRW, Path: e.app.Paths.WorktreePath("view", "api"), BaseBranch: "main", Dirty: true},
		{Name: "web", Mode: model.ModeRW, Path: e.app.Paths.WorktreePath("view", "web"), BaseBranch: "main", Err: "missing"},
		{Name: "docs", Mode: model.ModeRO, Path: e.canonical("docs")},
	}
	if fmt.Sprint(v.RepoStatus) != fmt.Sprint(want) || len(v.Sessions) != 1 {
		t.Fatalf("view = %+v", v)
	}

	views, err := e.app.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || len(views[0].Sessions) != 1 || views[0].RepoStatus != nil {
		t.Fatalf("list = %+v", views)
	}
	if err := e.app.SetStatus(ctx, "view", model.StatusReview); err != nil {
		t.Fatal(err)
	}
	if views, _ := e.app.List(ctx, model.StatusActive); len(views) != 0 {
		t.Fatalf("still active: %+v", views)
	}
	if views, _ := e.app.List(ctx, model.StatusReview); len(views) != 1 {
		t.Fatal("not in review")
	}
	if err := e.app.SetStatus(ctx, "nope", model.StatusReview); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown task: %v", err)
	}
}

func TestDone(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "docs", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "finish me",
		Repos: []RepoArg{{"api", model.ModeRW}, {"docs", model.ModeRO}}})
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, st := range []model.SessionState{model.StateIdle, model.StateStopped, model.StateEnded, model.StateWaiting} {
		s := &model.Session{TaskID: task.ID, State: st}
		if err := e.app.Store.CreateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.ID)
	}
	tmuxOK := false
	if _, err := exec.LookPath("tmux"); err == nil {
		out, err := exec.Command("tmux", "-L", e.sock, "-f", os.DevNull, "new-session", "-d", "-s",
			tmux.SessionName(task.Slug), "sleep", "300").CombinedOutput()
		if err != nil {
			t.Fatalf("tmux: %v: %s", err, out)
		}
		tmuxOK = true
	}

	wt := e.app.Paths.WorktreePath(task.Slug, "api")
	if err := os.WriteFile(filepath.Join(wt, "wip"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = e.app.Done(ctx, task.Slug, false)
	if err == nil || !strings.Contains(err.Error(), "api") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("dirty Done err = %v", err)
	}
	if !exists(wt) {
		t.Fatal("refused Done removed the worktree")
	}
	if tmuxOK && !e.app.Tmux.HasSession(ctx, tmux.SessionName(task.Slug)) {
		t.Fatal("refused Done killed tmux")
	}

	if _, err := e.app.Done(ctx, task.Slug, true); err != nil {
		t.Fatal(err)
	}
	if exists(e.app.Paths.TaskDir(task.Slug)) {
		t.Fatal("task dir left")
	}
	if n := worktreeCount(t, e.canonical("api")); n != 1 {
		t.Fatalf("%d worktrees after done", n)
	}
	// No commits of its own → the branch is deleted (only in rw repos).
	assertNoBranch(t, e.canonical("api"), "atl/finish-me")
	if out := e.app.Out.(*bytes.Buffer).String(); !strings.Contains(out, "deleted branch atl/finish-me in api\n") ||
		strings.Contains(out, " in docs") {
		t.Fatalf("output: %q", out)
	}
	if !exists(e.canonical("docs")) {
		t.Fatal("ro repo removed")
	}
	if tmuxOK && e.app.Tmux.HasSession(ctx, tmux.SessionName(task.Slug)) {
		t.Fatal("tmux session still alive")
	}
	for _, id := range ids {
		s, err := e.app.Store.GetSession(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if s.State != model.StateEnded {
			t.Fatalf("session %d is %s", id, s.State)
		}
	}
	got, _ := e.app.Store.GetTask(ctx, task.ID)
	if got.Status != model.StatusDone {
		t.Fatalf("status %s", got.Status)
	}
	if _, err := e.app.Done(ctx, task.Slug, true); err == nil || !strings.Contains(err.Error(), "already done") {
		t.Fatalf("second Done: %v", err)
	}
	if _, err := e.app.Cancel(ctx, task.Slug, false); err == nil {
		t.Fatal("Cancel of a done task succeeded")
	}
}

// A worktree dir deleted by hand does not block Done, and its metadata is
// pruned so the branch (no commits of its own) can be deleted.
func TestDoneMissingWorktree(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "gone", NoFetch: true, Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(e.app.Paths.WorktreePath(task.Slug, "api")); err != nil {
		t.Fatal(err)
	}
	got, err := e.app.Done(ctx, task.Slug, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Slug != task.Slug || got.Status != model.StatusDone {
		t.Fatalf("Done returned %s %s", got.Slug, got.Status)
	}
	api := e.canonical("api")
	if n := worktreeCount(t, api); n != 1 {
		t.Fatalf("%d worktrees after done", n)
	}
	assertNoBranch(t, api, "atl/gone")
}

func assertNoBranch(t *testing.T, repo, branch string) {
	t.Helper()
	if err := exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Run(); err == nil {
		t.Fatalf("branch %s still exists in %s", branch, repo)
	}
}

// commitIn commits a new file in the task's worktree for repo.
func (e *taskEnv) commitIn(t *testing.T, slug, repo, file string) string {
	t.Helper()
	wt := e.app.Paths.WorktreePath(slug, repo)
	if err := os.WriteFile(filepath.Join(wt, file), []byte(file+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, wt, "add", file)
	git(t, wt, "commit", "-q", "-m", "add "+file)
	return git(t, wt, "rev-parse", "HEAD")
}

// Done/Cancel delete the task branch only when every commit on it is
// reachable from another local branch or a remote-tracking ref.
func TestFinishBranchCleanup(t *testing.T) {
	cases := []struct {
		name    string
		cancel  bool
		prepare func(t *testing.T, e *taskEnv, slug string)
		kept    bool
	}{
		{name: "no commits", prepare: func(*testing.T, *taskEnv, string) {}},
		{name: "pushed", prepare: func(t *testing.T, e *taskEnv, slug string) {
			e.commitIn(t, slug, "api", "f")
			wt := e.app.Paths.WorktreePath(slug, "api")
			// Plain push to the URL (no tracking ref), then fetch creates
			// refs/remotes/origin/atl/<slug>.
			git(t, wt, "push", "-q", e.origin("api"), "atl/"+slug)
			git(t, e.canonical("api"), "fetch", "-q", "origin")
			git(t, e.canonical("api"), "rev-parse", "--verify", "refs/remotes/origin/atl/"+slug)
		}},
		{name: "unpushed", kept: true, prepare: func(t *testing.T, e *taskEnv, slug string) {
			e.commitIn(t, slug, "api", "f")
		}},
		{name: "in other branch", prepare: func(t *testing.T, e *taskEnv, slug string) {
			sha := e.commitIn(t, slug, "api", "f")
			git(t, e.canonical("api"), "branch", "other", sha)
		}},
		{name: "alias symref", kept: true, prepare: func(t *testing.T, e *taskEnv, slug string) {
			e.commitIn(t, slug, "api", "f")
			git(t, e.canonical("api"), "symbolic-ref", "refs/heads/alias", "refs/heads/atl/"+slug)
		}},
		{name: "cancel no commits", cancel: true, prepare: func(*testing.T, *taskEnv, string) {}},
		{name: "cancel unpushed", cancel: true, kept: true, prepare: func(t *testing.T, e *taskEnv, slug string) {
			e.commitIn(t, slug, "api", "f")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTaskEnv(t)
			ctx := context.Background()
			e.addRepo(t, "api", "main")
			task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "br", Repos: []RepoArg{{"api", model.ModeRW}}})
			if err != nil {
				t.Fatal(err)
			}
			c.prepare(t, e, task.Slug)
			e.app.Out.(*bytes.Buffer).Reset()
			if c.cancel {
				_, err = e.app.Cancel(ctx, task.Slug, false)
			} else {
				_, err = e.app.Done(ctx, task.Slug, false)
			}
			if err != nil {
				t.Fatal(err)
			}
			out := e.app.Out.(*bytes.Buffer).String()
			if c.kept {
				git(t, e.canonical("api"), "rev-parse", "--verify", "refs/heads/atl/br")
				if !strings.Contains(out, "kept branch atl/br in api (1 unpushed commit(s))") {
					t.Fatalf("output: %q", out)
				}
			} else {
				assertNoBranch(t, e.canonical("api"), "atl/br")
				if !strings.Contains(out, "deleted branch atl/br in api") {
					t.Fatalf("output: %q", out)
				}
			}
		})
	}
}

// A branch that moves between the unique-commit check and the delete is kept
// (compare-and-delete against the tip read before the check).
func TestFinishBranchMovedBeforeDelete(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "race", Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	canon := e.canonical("api")
	var moved string
	beforeBranchDelete = func(repo, branch string) {
		// A new commit on the branch, made from the canonical repo.
		tree := git(t, repo, "rev-parse", "refs/heads/"+branch+"^{tree}")
		moved = git(t, repo, "commit-tree", "-p", "refs/heads/"+branch, "-m", "late", tree)
		git(t, repo, "update-ref", "refs/heads/"+branch, moved)
	}
	t.Cleanup(func() { beforeBranchDelete = nil })
	e.app.Out.(*bytes.Buffer).Reset()
	if _, err := e.app.Done(ctx, task.Slug, false); err != nil {
		t.Fatal(err)
	}
	if got := git(t, canon, "rev-parse", "refs/heads/atl/race"); got != moved {
		t.Fatalf("branch tip %s, want %s", got, moved)
	}
	out := e.app.Out.(*bytes.Buffer).String()
	if !strings.Contains(out, "warning: delete branch atl/race in api:") || strings.Contains(out, "deleted branch") {
		t.Fatalf("output: %q", out)
	}
}

// --force discards a dirty worktree but still protects unpushed commits.
func TestDoneForceKeepsUnpushedBranch(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "web", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "forced",
		Repos: []RepoArg{{"api", model.ModeRW}, {"web", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	e.commitIn(t, task.Slug, "api", "f")
	e.commitIn(t, task.Slug, "api", "g")
	wt := e.app.Paths.WorktreePath(task.Slug, "api")
	if err := os.WriteFile(filepath.Join(wt, "wip"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.Done(ctx, task.Slug, false); err == nil {
		t.Fatal("dirty Done succeeded")
	}
	if _, err := e.app.Done(ctx, task.Slug, true); err != nil {
		t.Fatal(err)
	}
	if exists(wt) || worktreeCount(t, e.canonical("api")) != 1 {
		t.Fatal("worktree not removed")
	}
	git(t, e.canonical("api"), "rev-parse", "--verify", "refs/heads/atl/forced")
	assertNoBranch(t, e.canonical("web"), "atl/forced")
	out := e.app.Out.(*bytes.Buffer).String()
	if !strings.Contains(out, "kept branch atl/forced in api (2 unpushed commit(s))") ||
		!strings.Contains(out, "deleted branch atl/forced in web") {
		t.Fatalf("output: %q", out)
	}
}

func TestCancelLeavesNonEmptyDir(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "drop", Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	dir := e.app.Paths.TaskDir(task.Slug)
	notes := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notes, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Committed work in the worktree is not "dirty".
	wt := e.app.Paths.WorktreePath(task.Slug, "api")
	if err := os.WriteFile(filepath.Join(wt, "feature"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, wt, "add", "feature")
	git(t, wt, "commit", "-q", "-m", "feature")

	if _, err := e.app.Cancel(ctx, task.Slug, false); err != nil {
		t.Fatal(err)
	}
	if !exists(notes) || exists(wt) || exists(filepath.Join(dir, "CLAUDE.md")) || exists(filepath.Join(dir, ".claude")) {
		t.Fatal("unexpected task dir contents after cancel")
	}
	if out := e.app.Out.(*bytes.Buffer).String(); !strings.Contains(out, "left "+dir+" (not empty)") {
		t.Fatalf("warning missing: %q", out)
	}
	got, _ := e.app.Store.GetTask(ctx, task.ID)
	if got.Status != model.StatusCancelled {
		t.Fatalf("status %s", got.Status)
	}
	git(t, e.canonical("api"), "rev-parse", "--verify", "refs/heads/atl/drop") // unpushed commit
	if out := e.app.Out.(*bytes.Buffer).String(); !strings.Contains(out, "kept branch atl/drop in api (1 unpushed commit(s))") {
		t.Fatalf("output: %q", out)
	}
}

func TestPrepareTaskDir(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	task := &model.Task{Slug: "prep", Title: "Prep", Status: model.StatusActive, Branch: "atl/prep",
		Repos: []model.TaskRepo{{Name: "docs", Mode: model.ModeRO}}}
	dir := e.app.Paths.TaskDir("prep")
	if err := e.app.prepareTaskDir(ctx, task); err != nil {
		t.Fatal(err)
	}
	md, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil || !strings.Contains(string(md), e.canonical("docs")) {
		t.Fatalf("CLAUDE.md: %v %s", err, md)
	}
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.app.AtlBin = "/opt/atl"
	if err := e.app.prepareTaskDir(ctx, task); err != nil {
		t.Fatal(err)
	}
	if md, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md")); string(md) != "mine" {
		t.Fatal("existing CLAUDE.md overwritten")
	}
	if s, _ := os.ReadFile(filepath.Join(dir, ".claude", "settings.json")); !strings.Contains(string(s), gen.HookCommand("/opt/atl", "Stop")) {
		t.Fatalf("settings not refreshed: %s", s)
	}
}

// TestPrepareTaskDirSymlinks: what a task plants in its task dir (from its
// container) before its next session launch cannot make prepareTaskDir
// write where atl runs, nor read a file there into the task dir.
func TestPrepareTaskDirSymlinks(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	task := &model.Task{Slug: "prep", Title: "Prep", Status: model.StatusActive, Branch: "atl/prep",
		Repos: []model.TaskRepo{{Name: "docs", Mode: model.ModeRO}}}
	dir := e.app.Paths.TaskDir("prep")
	outside := t.TempDir()
	victim := filepath.Join(outside, "bashrc")
	secret := filepath.Join(outside, "settings.json")
	for p, s := range map[string]string{victim: "# the user's\n", secret: `{"env":{"TOKEN":"SECRET-OF-THE-USER"}}`} {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		filepath.Join(dir, ".claude", "settings.json"):     secret,
		filepath.Join(dir, ".claude", "settings.json.bak"): victim,
		filepath.Join(dir, "CLAUDE.md"):                    victim,
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.app.prepareTaskDir(ctx, task); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "# the user's\n" {
		t.Errorf("a file outside the task dir was written: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".claude", "settings.json")); strings.Contains(string(b), "SECRET-OF-THE-USER") {
		t.Errorf("settings.json got the content of the file its symlink named: %s", b)
	}
	// .claude itself a symlink: the launch fails rather than write there.
	os.RemoveAll(filepath.Join(dir, ".claude"))
	if err := os.Symlink(outside, filepath.Join(dir, ".claude")); err != nil {
		t.Fatal(err)
	}
	if err := e.app.prepareTaskDir(ctx, task); err == nil {
		t.Error("prepareTaskDir through a symlinked .claude succeeded")
	}
	if b, _ := os.ReadFile(secret); !strings.Contains(string(b), "SECRET-OF-THE-USER") || strings.Contains(string(b), "hook") {
		t.Errorf("the other dir's settings.json was rewritten: %s", b)
	}
}

func TestDoctorMissingRepos(t *testing.T) {
	e := newTaskEnv(t)
	checks := e.app.Doctor(context.Background())
	found := map[string]Check{}
	for _, c := range checks {
		found[c.Name] = c
	}
	r := found["repos dir"]
	if !r.OK || !r.Warn || !strings.Contains(r.Detail, "~/repos") {
		t.Fatalf("repos check = %+v", r)
	}
	if c := found["tasks dir"]; !c.OK || !c.Warn {
		t.Fatalf("tasks check = %+v", c)
	}
	if c := found["database"]; !c.OK || c.Warn {
		t.Fatalf("db check = %+v", c)
	}
	if c := found["hook log"]; !c.OK || !exists(e.app.Paths.HookLog()) {
		t.Fatalf("hook log check = %+v", c)
	}
	if err := os.MkdirAll(e.app.Paths.Repos, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range e.app.Doctor(context.Background()) {
		if c.Name == "repos dir" && (c.Warn || !c.OK) {
			t.Fatalf("existing repos dir: %+v", c)
		}
	}
}

// In a workspace the repos dir defaults to <workspace>/repos, not ~/repos:
// the hint for a missing one names that default.
func TestDoctorMissingReposInWorkspace(t *testing.T) {
	e := newTaskEnv(t)
	ws := filepath.Join(e.root, "ws")
	e.app.Paths.Workspace = ws
	for _, c := range e.app.Doctor(context.Background()) {
		if c.Name != "repos dir" {
			continue
		}
		if !c.Warn || strings.Contains(c.Detail, "~/repos") || !strings.Contains(c.Detail, "(default "+filepath.Join(ws, "repos")+")") {
			t.Errorf("repos check = %+v", c)
		}
		return
	}
	t.Fatal("no repos dir check")
}

func TestTmuxVersionCheck(t *testing.T) {
	for out, level := range map[string]string{
		"tmux 3.5a": "ok", "tmux 3.2": "ok", "tmux 4.0": "ok", "tmux next-3.6": "ok",
		"tmux 3.1c": "FAIL", "tmux 2.9": "FAIL", "tmux master": "warn",
	} {
		if got := checkTmuxVersion(out).Level(); got != level {
			t.Errorf("%q: %s, want %s", out, got, level)
		}
	}
}

// Commits only a detached HEAD reaches block Done/Cancel, even with --force,
// before anything is torn down.
func TestFinishRefusesOrphanCommits(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "detached", Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	wt := e.app.Paths.WorktreePath(task.Slug, "api")
	git(t, wt, "checkout", "-q", "--detach")
	e.commitIn(t, task.Slug, "api", "f")
	for _, force := range []bool{false, true} {
		_, err := e.app.Cancel(ctx, task.Slug, force)
		if err == nil || !strings.Contains(err.Error(), "api: HEAD has 1 commit(s) not on any branch") {
			t.Fatalf("force=%v: err = %v", force, err)
		}
		if !exists(wt) {
			t.Fatal("refused Cancel removed the worktree")
		}
	}
	git(t, wt, "branch", "keep")
	if _, err := e.app.Cancel(ctx, task.Slug, false); err != nil {
		t.Fatal(err)
	}
}

// A failed check whether the task branch exists (here: ctx cancelled) is
// not "it did not exist": the worktree is not attempted, and the kept
// branch, with its unpushed commit, is not deleted as if this call had
// made it.
func TestBuildTaskDirKeepsBranchOnFailedProbe(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "kept", Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	kept := e.commitIn(t, task.Slug, "api", "work")
	if _, err := e.app.Done(ctx, task.Slug, false); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	plan := []rwCheckout{{Repo: task.Repos[0], Existing: true}}
	if _, err := e.app.buildTaskDir(cancelled, task, plan, true); err == nil {
		t.Fatal("buildTaskDir worked with a cancelled ctx")
	}
	if got := git(t, e.canonical("api"), "rev-parse", "refs/heads/atl/kept"); got != kept {
		t.Fatalf("kept branch at %s, want %s", got, kept)
	}
	if exists(e.app.Paths.TaskDir(task.Slug)) {
		t.Fatal("task dir left behind")
	}
}

// Doctor lists, per repo, the settings of its own config (its clone's and
// its worktrees') that make atl refuse a git command, naming the worktree
// a worktree-scoped one is in; repos without any pass.
func TestDoctorRepoConfigs(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "docs", "main")
	check := func() map[string]Check {
		t.Helper()
		found := map[string]Check{}
		for _, c := range e.app.checkRepoConfigs(ctx) {
			found[c.Name] = c
		}
		return found
	}
	if c := check()["repo configs"]; !c.OK || c.Warn || !strings.Contains(c.Detail, "2 repos") {
		t.Fatalf("clean repos: %+v", c)
	}
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "cfg", Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	git(t, e.canonical("api"), "config", "filter.x.clean", "cat")
	wt := e.app.Paths.WorktreePath(task.Slug, "api")
	git(t, e.canonical("api"), "config", "extensions.worktreeConfig", "true")
	git(t, wt, "config", "--worktree", "url.https://evil.example/.insteadOf", "https://github.com/")
	found := check()
	c, ok := found["repo api"]
	if !ok || !c.Warn || !strings.Contains(c.Detail, "filter.x.clean = cat (local; blocks atl diff") ||
		!strings.Contains(c.Detail, "(worktree "+wt+"; blocks atl push") {
		t.Fatalf("repo api: %+v (all: %+v)", c, found)
	}
	if _, ok := found["repo docs"]; ok {
		t.Fatalf("docs flagged: %+v", found)
	}
	if _, ok := found["repo configs"]; ok {
		t.Fatalf("a pass besides the warnings: %+v", found)
	}
}
