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

// originRefs lists name's origin branches as "ref sha" lines.
func originRefs(t *testing.T, e *taskEnv, name string) string {
	t.Helper()
	return git(t, e.origin(name), "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads")
}

func TestPush(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	for _, name := range []string{"api", "lib", "docs"} {
		e.addRepo(t, name, "main")
	}
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "push me", NoFetch: true,
		Repos: []RepoArg{{"api", model.ModeRW}, {"lib", model.ModeRW}, {"docs", model.ModeRO}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"api", "lib"} {
		wt := e.app.Paths.WorktreePath(task.Slug, name)
		if err := os.WriteFile(filepath.Join(wt, "work"), []byte("work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, wt, "add", "work")
		git(t, wt, "commit", "-q", "-m", "work")
	}
	// main has a new commit in the canonical clone, which must stay there.
	if err := os.WriteFile(filepath.Join(e.canonical("api"), "local"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, e.canonical("api"), "add", "local")
	git(t, e.canonical("api"), "commit", "-q", "-m", "local main")
	// A hook the task planted would leave a marker.
	marker := filepath.Join(e.root, "hook-ran")
	hook := filepath.Join(e.canonical("api"), ".git", "hooks", "pre-push")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := map[string]string{}
	for _, name := range []string{"api", "lib", "docs"} {
		before[name] = originRefs(t, e, name)
	}
	head := func(name string) string {
		return git(t, e.app.Paths.WorktreePath(task.Slug, name), "rev-parse", "HEAD")
	}

	// --repo: that repo only.
	var out bytes.Buffer
	res, err := e.app.Push(ctx, task.Slug, PushOpts{Repo: "api"}, &out)
	if err != nil || len(res) != 1 || res[0].Repo != "api" || res[0].Err != nil {
		t.Fatalf("push --repo api: %+v, %v\n%s", res, err, out.String())
	}
	if got, want := originRefs(t, e, "api"), "refs/heads/atl/push-me "+head("api")+"\n"+before["api"]; got != want {
		t.Errorf("api origin:\n%s\nwant:\n%s", got, want)
	}
	if got := originRefs(t, e, "lib"); got != before["lib"] {
		t.Errorf("lib was pushed with --repo api:\n%s", got)
	}
	if exists(marker) {
		t.Error("the pre-push hook planted in the repo ran")
	}

	// All: every rw repo; the ro repo is not pushed.
	out.Reset()
	res, err = e.app.Push(ctx, "push-me", PushOpts{}, &out)
	if err != nil || len(res) != 2 || res[0].Repo != "api" || res[1].Repo != "lib" || res[0].Err != nil || res[1].Err != nil {
		t.Fatalf("push: %+v, %v\n%s", res, err, out.String())
	}
	if got, want := originRefs(t, e, "lib"), "refs/heads/atl/push-me "+head("lib")+"\n"+before["lib"]; got != want {
		t.Errorf("lib origin:\n%s\nwant:\n%s", got, want)
	}
	if got := originRefs(t, e, "docs"); got != before["docs"] {
		t.Errorf("the ro repo was pushed:\n%s", got)
	}
	if got := git(t, e.origin("api"), "rev-parse", "main"); !strings.HasSuffix(before["api"], got) {
		t.Errorf("api main on origin moved to %s", got)
	}

	// A rewritten branch fails without --force and goes with it.
	wt := e.app.Paths.WorktreePath(task.Slug, "api")
	git(t, wt, "commit", "-q", "--amend", "-m", "work, amended")
	res, err = e.app.Push(ctx, task.Slug, PushOpts{Repo: "api"}, &out)
	if err != nil || len(res) != 1 || res[0].Err == nil {
		t.Errorf("push of a rewritten branch without --force: %+v, %v", res, err)
	}
	res, err = e.app.Push(ctx, task.Slug, PushOpts{Repo: "api", Force: true}, &out)
	if err != nil || len(res) != 1 || res[0].Err != nil {
		t.Fatalf("push --force: %+v, %v\n%s", res, err, out.String())
	}
	if got := git(t, e.origin("api"), "rev-parse", "atl/push-me"); got != head("api") {
		t.Errorf("origin atl/push-me = %s after --force, want %s", got, head("api"))
	}

	// Refusals.
	for _, o := range []PushOpts{{Repo: "docs"}, {Repo: "nope"}} {
		if res, err := e.app.Push(ctx, task.Slug, o, &out); err == nil {
			t.Errorf("push %+v: %+v, want an error", o, res)
		}
	}
	if exists(marker) {
		t.Error("the pre-push hook planted in the repo ran")
	}
}

func TestFetch(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "docs", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "fetch me", NoFetch: true,
		Repos: []RepoArg{{"api", model.ModeRW}, {"docs", model.ModeRO}}})
	if err != nil {
		t.Fatal(err)
	}
	apiTip := e.pushOrigin(t, "api", "new-api")
	docsTip := e.pushOrigin(t, "docs", "new-docs")

	res, err := e.app.Fetch(ctx, task.Slug, "docs", nil)
	if err != nil || len(res) != 1 || res[0].Repo != "docs" || res[0].Err != nil {
		t.Fatalf("fetch --repo docs: %+v, %v", res, err)
	}
	if got := git(t, e.canonical("docs"), "rev-parse", "origin/main"); got != docsTip {
		t.Errorf("docs origin/main = %s, want %s", got, docsTip)
	}
	if got := git(t, e.canonical("api"), "rev-parse", "origin/main"); got == apiTip {
		t.Error("api was fetched with --repo docs")
	}

	res, err = e.app.Fetch(ctx, task.Slug, "", nil)
	if err != nil || len(res) != 2 || res[0].Err != nil || res[1].Err != nil {
		t.Fatalf("fetch: %+v, %v", res, err)
	}
	if got := git(t, e.app.Paths.WorktreePath(task.Slug, "api"), "rev-parse", "origin/main"); got != apiTip {
		t.Errorf("api origin/main = %s, want %s", got, apiTip)
	}
}
