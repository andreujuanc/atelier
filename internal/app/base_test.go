package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andreujuanc/atelier/internal/model"
)

// pushBranch creates (or moves) branch in name's seed clone at the seed's
// current default-branch tip plus one new commit adding file, pushes it to
// origin and returns its sha. The seed is left on def.
func (e *taskEnv) pushBranch(t *testing.T, name, def, branch, file string) string {
	t.Helper()
	seed := e.seed(name)
	git(t, seed, "checkout", "-q", "-B", branch, def)
	if err := os.WriteFile(filepath.Join(seed, file), []byte(file+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", file)
	git(t, seed, "commit", "-q", "-m", "add "+file)
	git(t, seed, "push", "-q", "origin", branch)
	sha := git(t, seed, "rev-parse", "HEAD")
	git(t, seed, "checkout", "-q", def)
	return sha
}

func originSHA(t *testing.T, e *taskEnv, name, branch string) string {
	t.Helper()
	return git(t, e.origin(name), "rev-parse", branch)
}

func branchExists(repo, branch string) bool {
	return exec.Command("git", "-C", repo, "rev-parse", "--verify", "-q", "refs/heads/"+branch).Run() == nil
}

func TestNewTaskBaseAuto(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "docs", "main")
	// dev is pushed after the clone: only the fetch makes it visible.
	dev := e.pushBranch(t, "api", "main", "dev", "dev-only")
	mainSHA := originSHA(t, e, "api", "main")
	if dev == mainSHA {
		t.Fatal("fixture: dev must differ from main")
	}

	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "auto",
		Repos: []RepoArg{{"api", model.ModeRW}, {"docs", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	if task.Base != model.BaseAuto {
		t.Fatalf("base %q", task.Base)
	}
	wt := e.app.Paths.WorktreePath(task.Slug, "api")
	if got := git(t, wt, "rev-parse", "HEAD"); got != dev {
		t.Fatalf("api worktree at %s, want origin/dev %s", got, dev)
	}
	stored, err := e.app.Store.GetTaskBySlug(ctx, task.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if r := stored.Repos[0]; r.BaseBranch != "dev" || r.BaseCommit != dev {
		t.Fatalf("api repo %+v", r)
	}
	// docs has no dev/develop: falls back to origin's default branch.
	if r := stored.Repos[1]; r.BaseBranch != "main" || r.BaseCommit != originSHA(t, e, "docs", "main") {
		t.Fatalf("docs repo %+v", r)
	}
	md, err := os.ReadFile(filepath.Join(e.app.Paths.TaskDir(task.Slug), "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"branched from `origin/dev`", "branched from `origin/main`"} {
		if !strings.Contains(string(md), s) {
			t.Errorf("CLAUDE.md lacks %q:\n%s", s, md)
		}
	}
	d, err := e.app.Show(ctx, task.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if d.Task.Base != "" || d.Repos[0].BaseBranch != "dev" || d.Repos[1].BaseBranch != "main" {
		t.Fatalf("show: base %q repos %+v", d.Task.Base, d.Repos)
	}
	v, err := e.app.Get(ctx, task.Slug)
	if err != nil || v.RepoStatus[0].BaseBranch != "dev" {
		t.Fatalf("get: %+v, %v", v, err)
	}
}

func TestNewTaskBaseDefaultAndExplicit(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "web", "main")
	e.pushBranch(t, "api", "main", "dev", "dev-only")
	e.pushBranch(t, "web", "main", "develop", "develop-only")
	relAPI := e.pushBranch(t, "api", "main", "release/1.2", "rel")
	relWeb := e.pushBranch(t, "web", "main", "release/1.2", "rel")
	rw := []RepoArg{{"api", model.ModeRW}, {"web", model.ModeRW}}

	// Production patch: origin's default branch, whatever dev branches exist.
	prod, err := e.app.NewTask(ctx, NewTaskOpts{Title: "hotfix", Base: model.BaseDefault, Repos: rw})
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := e.app.Store.GetTaskBySlug(ctx, prod.Slug)
	if stored.Base != model.BaseDefault {
		t.Fatalf("stored base %q", stored.Base)
	}
	for i, name := range []string{"api", "web"} {
		r := stored.Repos[i]
		if r.BaseBranch != "main" || r.BaseCommit != originSHA(t, e, name, "main") {
			t.Fatalf("%s repo %+v", name, r)
		}
		if got := git(t, e.app.Paths.WorktreePath(prod.Slug, name), "rev-parse", "HEAD"); got != r.BaseCommit {
			t.Fatalf("%s worktree at %s", name, got)
		}
	}

	// Explicit branch (with a slash), trimmed.
	rel, err := e.app.NewTask(ctx, NewTaskOpts{Title: "release fix", Base: " release/1.2 ", Repos: rw})
	if err != nil {
		t.Fatal(err)
	}
	stored, _ = e.app.Store.GetTaskBySlug(ctx, rel.Slug)
	if stored.Base != "release/1.2" || stored.Repos[0].BaseBranch != "release/1.2" || stored.Repos[0].BaseCommit != relAPI ||
		stored.Repos[1].BaseCommit != relWeb {
		t.Fatalf("release task %+v", stored)
	}

	// Auto: web has only develop.
	auto, err := e.app.NewTask(ctx, NewTaskOpts{Title: "feature", Repos: rw})
	if err != nil {
		t.Fatal(err)
	}
	stored, _ = e.app.Store.GetTaskBySlug(ctx, auto.Slug)
	if stored.Repos[0].BaseBranch != "dev" || stored.Repos[1].BaseBranch != "develop" {
		t.Fatalf("auto task %+v", stored.Repos)
	}

	// "main" is just a branch name.
	m, err := e.app.NewTask(ctx, NewTaskOpts{Title: "on main", Base: "main", Repos: rw[:1]})
	if err != nil {
		t.Fatal(err)
	}
	if stored, _ = e.app.Store.GetTaskBySlug(ctx, m.Slug); stored.Base != "main" || stored.Repos[0].BaseBranch != "main" {
		t.Fatalf("main task %+v", stored)
	}
}

func TestNewTaskBaseMissingBranch(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "web", "main")
	e.addRepo(t, "docs", "main")
	e.pushBranch(t, "api", "main", "staging", "s")
	before := countRows(t, e)
	_, err := e.app.NewTask(ctx, NewTaskOpts{Title: "staging fix", Base: "staging",
		Repos: []RepoArg{{"api", model.ModeRW}, {"web", model.ModeRW}, {"docs", model.ModeRO}}})
	if err == nil || !strings.Contains(err.Error(), "repo web") || !strings.Contains(err.Error(), `"staging"`) {
		t.Fatalf("err = %v", err)
	}
	if countRows(t, e) != before {
		t.Fatal("task row left behind")
	}
	if exists(e.app.Paths.TaskDir("staging-fix")) {
		t.Fatal("task dir left behind")
	}
	for _, r := range []string{"api", "web"} {
		if n := worktreeCount(t, e.canonical(r)); n != 1 {
			t.Fatalf("%s has %d worktrees", r, n)
		}
		if branchExists(e.canonical(r), "atl/staging-fix") {
			t.Fatalf("branch left in %s", r)
		}
	}

	for _, bad := range []string{":prod", "origin/dev", "-x", "a b", "a..b", "refs/heads/dev"} {
		if _, err := e.app.NewTask(ctx, NewTaskOpts{Title: "bad", Base: bad,
			Repos: []RepoArg{{"api", model.ModeRW}}}); err == nil || !strings.Contains(err.Error(), "invalid base") {
			t.Errorf("base %q: err = %v", bad, err)
		}
	}
	if countRows(t, e) != before {
		t.Fatal("task row after invalid base")
	}
}

func TestAddRepoUsesTaskBase(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	for _, r := range []string{"api", "web", "lib"} {
		e.addRepo(t, r, "main")
	}
	webDev := e.pushBranch(t, "web", "main", "dev", "dev-only")
	e.pushBranch(t, "lib", "main", "dev", "dev-only")
	e.pushBranch(t, "api", "main", "rc", "rc")

	prod, err := e.app.NewTask(ctx, NewTaskOpts{Title: "prod", Base: model.BaseDefault,
		Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.app.AddRepo(ctx, prod.Slug, RepoArg{Name: "web", Mode: model.ModeRW}, false)
	if err != nil {
		t.Fatal(err)
	}
	if r := got.Repos[1]; r.Name != "web" || r.BaseBranch != "main" || r.BaseCommit != originSHA(t, e, "web", "main") {
		t.Fatalf("prod web %+v", r)
	}

	auto, err := e.app.NewTask(ctx, NewTaskOpts{Title: "feat", Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err = e.app.AddRepo(ctx, auto.Slug, RepoArg{Name: "web", Mode: model.ModeRW}, false)
	if err != nil {
		t.Fatal(err)
	}
	if r := got.Repos[1]; r.BaseBranch != "dev" || r.BaseCommit != webDev {
		t.Fatalf("auto web %+v", r)
	}
	if h := git(t, e.app.Paths.WorktreePath(auto.Slug, "web"), "rev-parse", "HEAD"); h != webDev {
		t.Fatalf("auto web worktree at %s", h)
	}
	md, _ := os.ReadFile(filepath.Join(e.app.Paths.TaskDir(auto.Slug), "CLAUDE.md"))
	if !strings.Contains(string(md), "branched from `origin/dev`") {
		t.Fatalf("CLAUDE.md:\n%s", md)
	}

	// Explicit branch missing in the added repo: clear error, nothing added.
	rc, err := e.app.NewTask(ctx, NewTaskOpts{Title: "rc", Base: "rc", Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.AddRepo(ctx, rc.Slug, RepoArg{Name: "lib", Mode: model.ModeRW}, false); err == nil ||
		!strings.Contains(err.Error(), "repo lib") || !strings.Contains(err.Error(), `"rc"`) {
		t.Fatalf("err = %v", err)
	}
	stored, _ := e.app.Store.GetTaskBySlug(ctx, rc.Slug)
	if len(stored.Repos) != 1 || exists(e.app.Paths.WorktreePath(rc.Slug, "lib")) || branchExists(e.canonical("lib"), rc.Branch) {
		t.Fatalf("lib added anyway: %+v", stored.Repos)
	}
	// ro needs no base.
	if _, err := e.app.AddRepo(ctx, rc.Slug, RepoArg{Name: "lib", Mode: model.ModeRO}, false); err != nil {
		t.Fatal(err)
	}
}

func TestPRTargetsBaseBranch(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	gh := installGH(t)
	e.addRepo(t, "api", "main")
	e.pushBranch(t, "api", "main", "dev", "dev-only")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "Feature", Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	api := e.app.Paths.WorktreePath(task.Slug, "api")
	commitIn(t, api, "a.txt", "a\n")
	git(t, api, "push", "-q", "origin", task.Branch)
	res, err := e.app.PR(ctx, task.Slug)
	if err != nil || len(res) != 1 || res[0].Skipped {
		t.Fatalf("res = %+v, %v", res, err)
	}
	args := strings.Join(gh.args(t), "|")
	if !strings.Contains(args, "|--base|dev|") {
		t.Fatalf("gh args %q", args)
	}
}
