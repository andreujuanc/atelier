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

func commitIn(t *testing.T, dir, file, content string) {
	t.Helper()
	p := filepath.Join(dir, file)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", file)
	git(t, dir, "commit", "-q", "-m", "add "+file)
}

func TestDiff(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "lib", "main")
	e.addRepo(t, "docs", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "diff me",
		Repos: []RepoArg{{"api", model.ModeRW}, {"lib", model.ModeRW}, {"docs", model.ModeRO}}})
	if err != nil {
		t.Fatal(err)
	}
	api := e.app.Paths.WorktreePath(task.Slug, "api")
	commitIn(t, api, "committed.txt", "committed-line\n")
	if err := os.WriteFile(filepath.Join(api, "README"), []byte("README\nuncommitted-line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(api, "new dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(api, "new dir", "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(e.app.Paths.WorktreePath(task.Slug, "lib")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := e.app.Diff(ctx, task.Slug, &buf, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	base := task.Repos[0].BaseCommit[:8]
	libBase := task.Repos[1].BaseCommit[:8]
	if !strings.HasPrefix(out, "==> api (atl/diff-me vs "+base+")\n") {
		t.Fatalf("header:\n%s", out)
	}
	for _, s := range []string{"+committed-line", "+uncommitted-line", "?? new dir/x.txt\n",
		"\n\n==> lib (atl/diff-me vs " + libBase + ")\n(worktree missing)\n"} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
	if strings.Contains(out, "docs") || strings.Contains(out, "\x1b[") {
		t.Errorf("unexpected ro repo or colour:\n%s", out)
	}
	// Untracked files come after the diff.
	if strings.Index(out, "?? new dir/x.txt") < strings.Index(out, "+uncommitted-line") {
		t.Errorf("untracked listed before the diff:\n%s", out)
	}

	buf.Reset()
	if err := e.app.Diff(ctx, task.Slug, &buf, true); err != nil || !strings.Contains(buf.String(), "\x1b[") {
		t.Fatalf("colour diff: %v\n%q", err, buf.String())
	}

	ro, err := e.app.NewTask(ctx, NewTaskOpts{Title: "ro only", Repos: []RepoArg{{"docs", model.ModeRO}}})
	if err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if err := e.app.Diff(ctx, ro.Slug, &buf, false); err != nil || buf.String() != "no rw repos\n" {
		t.Fatalf("ro only: %v %q", err, buf.String())
	}
	if err := e.app.Diff(ctx, "nope", &buf, false); err == nil {
		t.Fatal("want error for unknown task")
	}
}

const ghStub = `#!/bin/sh
echo call >> "$GH_STUB_LOG.calls"
pwd > "$GH_STUB_LOG.cwd"
printf '%s\0' "$@" > "$GH_STUB_LOG.args"
case "$GH_STUB_MODE" in
fail)
	echo "GraphQL: Head sha can't be blank (createPullRequest)" >&2
	exit 1 ;;
exists)
	echo 'a pull request for branch "atl/x" into branch "main" already exists:' >&2
	echo "https://github.com/o/r/pull/7" >&2
	exit 1 ;;
*)
	echo "Creating pull request for atl/x into main in o/r" >&2
	echo
	echo "https://github.com/o/r/pull/1"
	echo ;;
esac
`

type ghLog struct{ base string }

// installGH puts a stub gh first on PATH and returns its log location.
func installGH(t *testing.T) ghLog {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(ghStub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	l := ghLog{filepath.Join(t.TempDir(), "gh")}
	t.Setenv("GH_STUB_LOG", l.base)
	t.Setenv("GH_STUB_MODE", "")
	return l
}

func (l ghLog) calls() int {
	b, _ := os.ReadFile(l.base + ".calls")
	return strings.Count(string(b), "call\n")
}

func (l ghLog) args(t *testing.T) []string {
	t.Helper()
	return strings.Split(strings.TrimSuffix(readFile(t, l.base+".args"), "\x00"), "\x00")
}

func (l ghLog) cwd(t *testing.T) string { return strings.TrimSpace(readFile(t, l.base+".cwd")) }

func samePathT(t *testing.T, a, b string) bool {
	t.Helper()
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// originHasBranch reports whether the bare origin of repo has branch.
func (e *taskEnv) originHasBranch(t *testing.T, repo, branch string) bool {
	t.Helper()
	return git(t, e.origin(repo), "branch", "--list", branch) != ""
}

func TestPR(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	gh := installGH(t)
	for _, r := range []string{"api", "lib", "web", "docs"} {
		e.addRepo(t, r, "main")
	}
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "Ship it", Description: "Does things.\nMore.",
		Repos: []RepoArg{{"api", model.ModeRW}, {"lib", model.ModeRW}, {"web", model.ModeRW}, {"docs", model.ModeRO}}})
	if err != nil {
		t.Fatal(err)
	}
	api := e.app.Paths.WorktreePath(task.Slug, "api")
	web := e.app.Paths.WorktreePath(task.Slug, "web")
	commitIn(t, web, "w.txt", "w\n") // committed, not pushed

	// Nothing pushed yet: gh must not run.
	commitIn(t, api, "a.txt", "a\n")
	res, err := e.app.PR(ctx, task.Slug)
	if err != nil {
		t.Fatal(err)
	}
	want := []PRResult{
		{Repo: "api", Skipped: true, Reason: "push atl/ship-it first"},
		{Repo: "lib", Skipped: true, Reason: "no commits"},
		{Repo: "web", Skipped: true, Reason: "push atl/ship-it first"},
	}
	if len(res) != len(want) {
		t.Fatalf("res = %+v", res)
	}
	for i := range want {
		if res[i] != want[i] {
			t.Errorf("res[%d] = %+v, want %+v", i, res[i], want[i])
		}
	}
	if gh.calls() != 0 {
		t.Fatal("gh called for unpushed branches")
	}
	st, _ := e.app.Store.GetTaskBySlug(ctx, task.Slug)
	if st.Status != model.StatusActive {
		t.Fatalf("status %s with no PR created", st.Status)
	}
	if e.originHasBranch(t, "api", "atl/ship-it") || e.originHasBranch(t, "web", "atl/ship-it") {
		t.Fatal("atelier pushed a branch")
	}

	// The user pushes api.
	git(t, api, "push", "-q", "origin", "atl/ship-it")
	res, err = e.app.PR(ctx, task.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if res[0] != (PRResult{Repo: "api", URL: "https://github.com/o/r/pull/1"}) || res[1] != want[1] || res[2] != want[2] {
		t.Fatalf("res = %+v", res)
	}
	if gh.calls() != 1 || !samePathT(t, gh.cwd(t), api) {
		t.Fatalf("gh calls %d cwd %s", gh.calls(), gh.cwd(t))
	}
	wantArgs := []string{"pr", "create", "--head", "atl/ship-it", "--base", "main", "--title", "Ship it",
		"--body", "Does things.\nMore.\n\nCreated by atelier"}
	if got := gh.args(t); strings.Join(got, "|") != strings.Join(wantArgs, "|") {
		t.Fatalf("gh args %q\nwant %q", got, wantArgs)
	}
	st, _ = e.app.Store.GetTaskBySlug(ctx, task.Slug)
	if st.Status != model.StatusReview {
		t.Fatalf("status %s", st.Status)
	}
	if e.originHasBranch(t, "web", "atl/ship-it") {
		t.Fatal("atelier pushed web's branch")
	}
}

func TestPRGhFailureAndExisting(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	gh := installGH(t)
	e.addRepo(t, "api", "trunk")
	e.addRepo(t, "gone", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "fail",
		Repos: []RepoArg{{"api", model.ModeRW}, {"gone", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	api := e.app.Paths.WorktreePath(task.Slug, "api")
	commitIn(t, api, "a.txt", "a\n")
	git(t, api, "push", "-q", "origin", "atl/fail")
	if err := os.RemoveAll(e.app.Paths.WorktreePath(task.Slug, "gone")); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GH_STUB_MODE", "fail")
	res, err := e.app.PR(ctx, task.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 ||
		res[0] != (PRResult{Repo: "api", Skipped: true, Reason: "gh: GraphQL: Head sha can't be blank (createPullRequest)"}) ||
		res[1] != (PRResult{Repo: "gone", Skipped: true, Reason: "worktree missing"}) {
		t.Fatalf("res = %+v", res)
	}
	st, _ := e.app.Store.GetTaskBySlug(ctx, task.Slug)
	if st.Status != model.StatusActive {
		t.Fatalf("status %s after gh failure", st.Status)
	}

	t.Setenv("GH_STUB_MODE", "exists")
	res, err = e.app.PR(ctx, task.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if res[0] != (PRResult{Repo: "api", URL: "https://github.com/o/r/pull/7"}) {
		t.Fatalf("res = %+v", res)
	}
	if got := gh.args(t); got[5] != "trunk" || got[len(got)-1] != "Created by atelier" {
		t.Fatalf("gh args %q", got)
	}
	st, _ = e.app.Store.GetTaskBySlug(ctx, task.Slug)
	if st.Status != model.StatusReview {
		t.Fatalf("status %s", st.Status)
	}
}

func TestPRResultString(t *testing.T) {
	for _, c := range []struct {
		r    PRResult
		want string
	}{
		{PRResult{Repo: "a", URL: "https://x/1"}, "https://x/1"},
		{PRResult{Repo: "a", Skipped: true, Reason: "push atl/s first"}, "skipped: push atl/s first"},
		{PRResult{Repo: "a", Reason: "PR already exists"}, "PR already exists"},
	} {
		if got := c.r.String(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.r, got, c.want)
		}
	}
	if lastURL("a pull request ... already exists") != "" || lastLine("x\nhttps://u\n\n") != "https://u" {
		t.Fatal("helpers")
	}
}
