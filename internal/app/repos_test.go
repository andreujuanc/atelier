package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/andreujuanc/atelier/internal/hook"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/store"
)

func TestAvailableRepos(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()

	// Missing Repos dir: empty, no error.
	got, err := e.app.AvailableRepos(ctx)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("missing dir: %v, %v", got, err)
	}

	e.addRepo(t, "zeta", "main")
	e.addRepo(t, "api", "main")
	repos := e.app.Paths.Repos
	// A plain dir, a file, a dir nested inside... and a symlink to a repo
	// living outside $ATL_REPOS, plus a dangling symlink.
	if err := os.MkdirAll(filepath.Join(repos, "plain", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repos, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(e.root, "elsewhere", "linked")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, outside, "init", "-q", "-b", "main")
	if err := os.Symlink(outside, filepath.Join(repos, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(e.root, "nowhere"), filepath.Join(repos, "dangling")); err != nil {
		t.Fatal(err)
	}
	// A reserved name is never offered even if it is a repo.
	reserved := filepath.Join(repos, ".claude")
	if err := os.MkdirAll(reserved, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, reserved, "init", "-q")

	got, err = e.app.AvailableRepos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"api", "linked", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// guardBlocks runs the PreToolUse hook for a Write to path from the task dir.
func guardBlocks(t *testing.T, e *taskEnv, slug, path string) bool {
	t.Helper()
	in := hook.Input{SessionID: "s", Cwd: e.app.Paths.TaskDir(slug), ToolName: "Write",
		ToolInput: json.RawMessage(`{"file_path":` + strconvQuote(path) + `}`)}
	env := func(k string) string {
		if k == "ATL_TASK" {
			return slug
		}
		return ""
	}
	exit, _, err := hook.Handle(context.Background(), hook.EventPreToolUse, in, env, e.app.Store, e.app.Paths, time.Now())
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	return exit == 2
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestAddRepoRW(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "lib", "trunk")
	e.writeProject(t, "shop", `
setup = 'echo "$ATL_REPO" >> "$MARKER"'
[[repos]]
name = "api"
[env]
MARKER = "`+filepath.Join(e.root, "marker")+`"
`)
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "Add things", Project: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(e.root, "marker")); got != "api\n" {
		t.Fatalf("marker after new: %q", got)
	}

	// --no-fetch: branches from the last fetched origin/trunk, not the new tip.
	old := git(t, e.canonical("lib"), "rev-parse", "refs/remotes/origin/trunk")
	tip := e.pushOrigin(t, "lib", "newer")
	e.app.Out.(interface{ Reset() }).Reset()
	got, err := e.app.AddRepo(ctx, task.Slug, RepoArg{Name: "lib", Mode: model.ModeRW}, true)
	if err != nil {
		t.Fatal(err)
	}
	wt := e.app.Paths.WorktreePath(task.Slug, "lib")
	if h := git(t, wt, "rev-parse", "HEAD"); h != old || h == tip {
		t.Fatalf("no-fetch HEAD %s, want %s", h, old)
	}
	if b := git(t, wt, "symbolic-ref", "--short", "HEAD"); b != task.Branch {
		t.Fatalf("branch %s", b)
	}
	want := []model.TaskRepo{task.Repos[0], {Name: "lib", Mode: model.ModeRW, BaseBranch: "trunk", BaseCommit: old}}
	if !reflect.DeepEqual(got.Repos, want) {
		t.Fatalf("returned repos %+v", got.Repos)
	}
	stored, _ := e.app.Store.GetTask(ctx, task.ID)
	if !reflect.DeepEqual(stored.Repos, want) {
		t.Fatalf("stored repos %+v", stored.Repos)
	}
	md := readFile(t, filepath.Join(e.app.Paths.TaskDir(task.Slug), "CLAUDE.md"))
	if !strings.Contains(md, "**lib** — `"+wt+"` — **rw**") {
		t.Fatalf("CLAUDE.md lacks lib:\n%s", md)
	}
	// setup ran only for the new repo
	if got := readFile(t, filepath.Join(e.root, "marker")); got != "api\nlib\n" {
		t.Fatalf("marker after add: %q", got)
	}
	out := e.outString()
	if strings.Contains(out, "fetching") || !strings.Contains(out, "worktree at "+wt+"; running sessions can already see it") {
		t.Fatalf("output:\n%s", out)
	}

	// With fetch: a second task gets origin's new tip.
	t2, err := e.app.NewTask(ctx, NewTaskOpts{Title: "Second", Repos: []RepoArg{{"api", model.ModeRO}}})
	if err != nil {
		t.Fatal(err)
	}
	e.app.Out.(interface{ Reset() }).Reset()
	if _, err := e.app.AddRepo(ctx, t2.Slug, RepoArg{Name: "lib"}, false); err != nil {
		t.Fatal(err)
	}
	if h := git(t, e.app.Paths.WorktreePath(t2.Slug, "lib"), "rev-parse", "HEAD"); h != tip {
		t.Fatalf("fetched HEAD %s, want %s", h, tip)
	}
	if !strings.Contains(e.outString(), "fetching lib") {
		t.Fatalf("no fetch progress:\n%s", e.outString())
	}
}

func TestAddRepoRO(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "docs", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "Read docs", NoFetch: true, Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(e.canonical("docs"), "README")
	if guardBlocks(t, e, task.Slug, target) {
		t.Fatal("guard blocks before docs is added")
	}
	before := worktreeCount(t, e.canonical("docs"))
	got, err := e.app.AddRepo(ctx, task.Slug, RepoArg{Name: "docs", Mode: model.ModeRO}, false)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(got.Repos); n != 2 || got.Repos[1] != (model.TaskRepo{Name: "docs", Mode: model.ModeRO}) {
		t.Fatalf("repos %+v", got.Repos)
	}
	if exists(e.app.Paths.WorktreePath(task.Slug, "docs")) || worktreeCount(t, e.canonical("docs")) != before {
		t.Fatal("ro repo got a worktree")
	}
	if out, err := exec.Command("git", "-C", e.canonical("docs"), "rev-parse", "--verify", "-q", "refs/heads/"+task.Branch).CombinedOutput(); err == nil {
		t.Fatalf("ro repo got a branch: %s", out)
	}
	md := readFile(t, filepath.Join(e.app.Paths.TaskDir(task.Slug), "CLAUDE.md"))
	if !strings.Contains(md, "## READ-ONLY repositories") || !strings.Contains(md, "`"+e.canonical("docs")+"` (docs)") {
		t.Fatalf("CLAUDE.md lacks read-only docs:\n%s", md)
	}
	if strings.Contains(e.outString(), "fetching docs") ||
		!strings.Contains(e.outString(), "running Claude sessions need a new session (s) to see docs") {
		t.Fatalf("output:\n%s", e.outString())
	}
	// The guard reads the DB live: writes into docs are blocked right away.
	if !guardBlocks(t, e, task.Slug, target) {
		t.Fatal("guard does not block writes into the newly added ro repo")
	}
	if guardBlocks(t, e, task.Slug, filepath.Join(e.app.Paths.WorktreePath(task.Slug, "api"), "x")) {
		t.Fatal("guard blocks the rw worktree")
	}
}

func TestAddRepoRefusals(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "web", "main")
	if err := os.MkdirAll(filepath.Join(e.app.Paths.Repos, "plain"), 0o755); err != nil {
		t.Fatal(err)
	}
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "Busy", NoFetch: true, Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := e.app.NewTask(ctx, NewTaskOpts{Title: "Closed", NoFetch: true, Repos: []RepoArg{{"api", model.ModeRO}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.Done(ctx, closed.Slug, false); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, ref string
		r         RepoArg
		want      string
	}{
		{"duplicate rw", task.Slug, RepoArg{"api", model.ModeRW}, "already has repo api"},
		{"duplicate other mode", task.Slug, RepoArg{"api", model.ModeRO}, "already has repo api"},
		{"closed", closed.Slug, RepoArg{"web", model.ModeRO}, "is done"},
		{"unknown task", "nope", RepoArg{"web", model.ModeRO}, "not found"},
		{"missing", task.Slug, RepoArg{"nope", model.ModeRW}, "does not exist"},
		{"plain dir", task.Slug, RepoArg{"plain", model.ModeRO}, "not a git repository"},
		{"path", task.Slug, RepoArg{"../web", model.ModeRW}, "invalid repo name"},
		{"dotdot", task.Slug, RepoArg{"..", model.ModeRO}, "invalid repo name"},
		{"empty", task.Slug, RepoArg{" ", model.ModeRO}, "invalid repo name"},
		{"reserved", task.Slug, RepoArg{".Claude", model.ModeRO}, "reserved"},
		{"reserved md", task.Slug, RepoArg{"claude.md", model.ModeRW}, "reserved"},
		{"bad mode", task.Slug, RepoArg{"web", "rx"}, "invalid mode"},
	}
	for _, c := range cases {
		if _, err := e.app.AddRepo(ctx, c.ref, c.r, true); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	got, _ := e.app.Store.GetTask(ctx, task.ID)
	if len(got.Repos) != 1 {
		t.Fatalf("repos after refusals: %+v", got.Repos)
	}
	if n := worktreeCount(t, e.canonical("web")); n != 1 {
		t.Fatalf("web has %d worktrees", n)
	}
}

func noBranch(t *testing.T, repo, branch string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", "-q", "refs/heads/"+branch).CombinedOutput(); err == nil {
		t.Fatalf("branch %s left in %s: %s", branch, repo, out)
	}
}

func TestAddRepoRollback(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "web", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "Roll", NoFetch: true, Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	web := e.canonical("web")
	wt := e.app.Paths.WorktreePath(task.Slug, "web")
	check := func(what string) {
		t.Helper()
		if exists(wt) || worktreeCount(t, web) != 1 {
			t.Fatalf("%s: worktree left behind", what)
		}
		if g, _ := e.app.Store.GetTask(ctx, task.ID); len(g.Repos) != 1 {
			t.Fatalf("%s: repo stored: %+v", what, g.Repos)
		}
	}

	// A failing post-checkout hook: git creates branch + worktree, then fails.
	hookPath := filepath.Join(web, ".git", "hooks", "post-checkout")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.AddRepo(ctx, task.Slug, RepoArg{"web", model.ModeRW}, true); err == nil {
		t.Fatal("AddRepo succeeded despite failing hook")
	}
	check("hook")
	noBranch(t, web, task.Branch)
	if err := os.Remove(hookPath); err != nil {
		t.Fatal(err)
	}

	// The branch already exists: AddWorktree fails, the branch is kept.
	git(t, web, "branch", task.Branch)
	if _, err := e.app.AddRepo(ctx, task.Slug, RepoArg{"web", model.ModeRW}, true); err == nil || !strings.Contains(err.Error(), "web") {
		t.Fatalf("existing branch: err = %v", err)
	}
	check("existing branch")
	git(t, web, "rev-parse", "--verify", "refs/heads/"+task.Branch)
	git(t, web, "branch", "-D", task.Branch)

	// The insert fails after the worktree was created: both are undone.
	db, err := sql.Open("sqlite", e.app.Paths.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER no_web BEFORE INSERT ON task_repos WHEN NEW.name = 'web'
		BEGIN SELECT RAISE(ABORT, 'insert refused by test'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.AddRepo(ctx, task.Slug, RepoArg{"web", model.ModeRW}, true); err == nil || !strings.Contains(err.Error(), "refused by test") {
		t.Fatalf("insert failure: err = %v", err)
	}
	check("insert failure")
	noBranch(t, web, task.Branch)
	if _, err := db.Exec(`DROP TRIGGER no_web`); err != nil {
		t.Fatal(err)
	}

	// Everything clean: it works now.
	if _, err := e.app.AddRepo(ctx, task.Slug, RepoArg{"web", model.ModeRW}, true); err != nil {
		t.Fatal(err)
	}
	if b := git(t, wt, "symbolic-ref", "--short", "HEAD"); b != task.Branch {
		t.Fatalf("branch %s", b)
	}
}

// ExactRepos: the project's repos are not merged in (the TUI picker may
// have deselected some), but the project still applies (setup, env).
func TestNewTaskExactRepos(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "lib", "main")
	e.writeProject(t, "shop", "setup = 'touch setup.ran'\n[[repos]]\nname = \"api\"\n[[repos]]\nname = \"lib\"\n")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "exact", Project: "shop", NoFetch: true, ExactRepos: true,
		Repos: []RepoArg{{"lib", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(task.Repos) != 1 || task.Repos[0].Name != "lib" || task.Project != "shop" {
		t.Fatalf("task %+v", task)
	}
	if !exists(filepath.Join(e.app.Paths.WorktreePath(task.Slug, "lib"), "setup.ran")) {
		t.Fatal("project setup did not run")
	}
	if _, err := e.app.NewTask(ctx, NewTaskOpts{Title: "x", Project: "nope", ExactRepos: true,
		Repos: []RepoArg{{"lib", model.ModeRO}}}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown project with ExactRepos: %v", err)
	}
}

// Concurrent AddRepo of one repo to one task, from separate App/Store
// instances (as separate processes would): exactly one wins, and the
// losers neither roll back its worktree nor its branch.
func TestAddRepoConcurrent(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "web", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "Race", NoFetch: true, Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	const n = 6
	apps := make([]*App, n)
	for i := range apps {
		st, err := store.Open(e.app.Paths.DB)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		apps[i] = &App{Paths: e.app.Paths, Store: st, Tmux: e.app.Tmux, Out: &bytes.Buffer{}, Now: time.Now, AtlBin: e.app.AtlBin}
	}
	start := make(chan struct{})
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range apps {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = apps[i].AddRepo(ctx, task.Slug, RepoArg{"web", model.ModeRW}, true)
		}(i)
	}
	close(start)
	wg.Wait()
	ok := 0
	for i, err := range errs {
		switch {
		case err == nil:
			ok++
		case !strings.Contains(err.Error(), "already has repo web"):
			t.Errorf("app %d: err = %v, want duplicate", i, err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d AddRepo calls succeeded, want 1: %v", ok, errs)
	}
	web := e.canonical("web")
	wt := e.app.Paths.WorktreePath(task.Slug, "web")
	if b := git(t, wt, "symbolic-ref", "--short", "HEAD"); b != task.Branch {
		t.Fatalf("worktree branch %s", b)
	}
	git(t, web, "rev-parse", "--verify", "refs/heads/"+task.Branch)
	if c := worktreeCount(t, web); c != 2 {
		t.Fatalf("web has %d worktrees, want 2", c)
	}
	got, _ := e.app.Store.GetTask(ctx, task.ID)
	if len(got.Repos) != 2 || got.Repos[1].Name != "web" {
		t.Fatalf("repos %+v", got.Repos)
	}
	if fi, err := os.Stat(filepath.Join(e.app.Paths.StateDir, "locks", fmt.Sprintf("task-%d.lock", task.ID))); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("lock file: %v %v", fi, err)
	}
}

// A lock held elsewhere makes AddRepo wait; a cancelled context gives up.
func TestAddRepoLockWait(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "Locked", NoFetch: true, Repos: []RepoArg{{"api", model.ModeRO}}})
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := e.app.lockTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := e.app.AddRepo(cctx, task.Slug, RepoArg{"api", model.ModeRW}, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	unlock()
	if _, err := e.app.AddRepo(ctx, task.Slug, RepoArg{"api", model.ModeRW}, true); err == nil || !strings.Contains(err.Error(), "already has repo api") {
		t.Fatalf("after unlock: %v", err)
	}
}

// The task is closed while the worktree is being created: the insert is
// refused and the worktree and branch this call created are undone.
func TestAddRepoTaskClosedMeanwhile(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "web", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "Closing", NoFetch: true, Repos: []RepoArg{{"api", model.ModeRO}}})
	if err != nil {
		t.Fatal(err)
	}
	web := e.canonical("web")
	sig := filepath.Join(e.root, "sig")
	if err := os.MkdirAll(sig, 0o755); err != nil {
		t.Fatal(err)
	}
	hookPath := filepath.Join(web, ".git", "hooks", "post-checkout")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ntouch '" + sig + "/started'\nwhile [ ! -f '" + sig + "/go' ]; do sleep 0.02; done\n"
	if err := os.WriteFile(hookPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.app.AddRepo(ctx, task.Slug, RepoArg{"web", model.ModeRW}, true)
		done <- err
	}()
	deadline := time.Now().Add(20 * time.Second)
	for !exists(filepath.Join(sig, "started")) {
		if time.Now().After(deadline) {
			t.Fatal("hook never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := e.app.Store.SetTaskStatus(ctx, task.ID, model.StatusDone); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sig, "go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	err = <-done
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("err = %v, want task closed", err)
	}
	if exists(e.app.Paths.WorktreePath(task.Slug, "web")) || worktreeCount(t, web) != 1 {
		t.Fatal("worktree left behind")
	}
	noBranch(t, web, task.Branch)
	if g, _ := e.app.Store.GetTask(ctx, task.ID); len(g.Repos) != 1 {
		t.Fatalf("repos %+v", g.Repos)
	}
}
