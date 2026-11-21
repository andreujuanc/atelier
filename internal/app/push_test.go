package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andreujuanc/atelier/internal/gitx"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/sandbox"
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

// decoy makes an empty bare repo for name's origin to be pointed at instead.
func (e *taskEnv) decoy(t *testing.T, name string) string {
	t.Helper()
	decoy := filepath.Join(e.root, "decoys", name+".git")
	git(t, e.root, "init", "-q", "--bare", decoy)
	return decoy
}

func readPin(t *testing.T, e *taskEnv, name string) gitx.Origin {
	t.Helper()
	var o gitx.Origin
	if err := json.Unmarshal([]byte(readFile(t, e.app.Paths.OriginPin(name))), &o); err != nil {
		t.Fatal(err)
	}
	return o
}

// TestPushGoesToThePinnedOrigin: the first push pins origin's URL; once the
// repo's config points origin elsewhere, pushes still go to the pinned URL,
// with a warning.
func TestPushGoesToThePinnedOrigin(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "pin me", NoFetch: true, Repos: []RepoArg{{"api", model.ModeRW}}})
	if err != nil {
		t.Fatal(err)
	}
	// Pinned when the task's worktree was made (for git-lfs; see
	// gitx.AddWorktree), with the sandbox off too.
	pin := e.app.Paths.OriginPin("api")
	if !exists(pin) {
		t.Fatal("not pinned by atl new")
	}
	wt := e.app.Paths.WorktreePath(task.Slug, "api")
	commit := func(msg string) string {
		if err := os.WriteFile(filepath.Join(wt, "work"), []byte(msg+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, wt, "add", "work")
		git(t, wt, "commit", "-q", "-m", msg)
		return git(t, wt, "rev-parse", "HEAD")
	}
	commit("one")
	var out bytes.Buffer
	if res, err := e.app.Push(ctx, task.Slug, PushOpts{}, &out); err != nil || res[0].Err != nil {
		t.Fatalf("push: %+v, %v\n%s", res, err, out.String())
	}
	if got, want := readPin(t, e, "api"), (gitx.Origin{URLs: []string{e.origin("api")}}); !got.Equal(want) {
		t.Errorf("pin = %+v, want %+v", got, want)
	}
	if strings.Contains(out.String(), "warning") {
		t.Errorf("warning with an unchanged origin: %q", out.String())
	}
	pinned := readFile(t, pin)

	decoy := e.decoy(t, "api")
	git(t, e.canonical("api"), "remote", "set-url", "origin", decoy)
	head := commit("two")
	out.Reset()
	if res, err := e.app.Push(ctx, task.Slug, PushOpts{}, &out); err != nil || res[0].Err != nil {
		t.Fatalf("push after set-url: %+v, %v\n%s", res, err, out.String())
	}
	if got := git(t, e.origin("api"), "rev-parse", "atl/pin-me"); got != head {
		t.Errorf("origin atl/pin-me = %s, want %s", got, head)
	}
	if got := git(t, decoy, "for-each-ref"); got != "" {
		t.Errorf("the decoy got refs: %s", got)
	}
	if got := git(t, wt, "rev-parse", "refs/remotes/origin/atl/pin-me"); got != head {
		t.Errorf("refs/remotes/origin/atl/pin-me = %s, want %s", got, head)
	}
	warning := "warning: repo api: its config now has origin " + decoy + "; using the pinned " + e.origin("api") + " (run `atl repos pin api` to accept the config's)\n"
	if !strings.Contains(out.String(), warning) {
		t.Errorf("output lacks the warning %q:\n%s", warning, out.String())
	}
	if got := readFile(t, pin); got != pinned {
		t.Errorf("the pin changed:\n%s", got)
	}

	// --force still works after a rewrite, with its lease, and is needed.
	git(t, wt, "commit", "-q", "--amend", "-m", "two, amended")
	if res, err := e.app.Push(ctx, task.Slug, PushOpts{}, &out); err != nil || res[0].Err == nil {
		t.Errorf("push of a rewritten branch without --force: %+v, %v", res, err)
	}
	if res, err := e.app.Push(ctx, task.Slug, PushOpts{Force: true}, &out); err != nil || res[0].Err != nil {
		t.Fatalf("push --force: %+v, %v\n%s", res, err, out.String())
	}
	if got, want := git(t, e.origin("api"), "rev-parse", "atl/pin-me"), git(t, wt, "rev-parse", "HEAD"); got != want {
		t.Errorf("origin atl/pin-me = %s after --force, want %s", got, want)
	}

	// A rewrite rule in the repo's config is refused, naming it.
	git(t, e.canonical("api"), "config", "url."+decoy+".pushInsteadOf", e.origin("api"))
	res, err := e.app.Push(ctx, task.Slug, PushOpts{Force: true}, &out)
	if err != nil || res[0].Err == nil || !strings.Contains(res[0].Err.Error(), "url."+decoy+".pushinsteadof") {
		t.Errorf("push with a pushInsteadOf rule in the repo: %+v, %v", res, err)
	}
	if got := git(t, decoy, "for-each-ref"); got != "" {
		t.Errorf("the decoy got refs: %s", got)
	}

	// atl repos pin accepts the config's URL.
	git(t, e.canonical("api"), "config", "--remove-section", "url."+decoy)
	old, had, now, err := e.app.RepinOrigin(ctx, "api")
	if err != nil || !had || !old.Equal(gitx.Origin{URLs: []string{e.origin("api")}}) || !now.Equal(gitx.Origin{URLs: []string{decoy}}) {
		t.Fatalf("RepinOrigin = %+v, %v, %+v, %v", old, had, now, err)
	}
	out.Reset()
	if res, err := e.app.Push(ctx, task.Slug, PushOpts{}, &out); err != nil || res[0].Err != nil {
		t.Fatalf("push after the repin: %+v, %v\n%s", res, err, out.String())
	}
	if got := git(t, decoy, "rev-parse", "atl/pin-me"); got != git(t, wt, "rev-parse", "HEAD") {
		t.Errorf("decoy atl/pin-me = %s after the repin", got)
	}
	if strings.Contains(out.String(), "warning") {
		t.Errorf("warning after the repin: %q", out.String())
	}
	// So does deleting the pin.
	git(t, e.canonical("api"), "remote", "set-url", "origin", e.origin("api"))
	if err := os.Remove(pin); err != nil {
		t.Fatal(err)
	}
	if res, err := e.app.Push(ctx, task.Slug, PushOpts{}, &out); err != nil || res[0].Err != nil {
		t.Fatalf("push after deleting the pin: %+v, %v\n%s", res, err, out.String())
	}
	if got := readPin(t, e, "api"); !got.Equal(gitx.Origin{URLs: []string{e.origin("api")}}) {
		t.Errorf("pin after deleting it = %+v", got)
	}
	// A damaged pin is replaced by a repin; inside a task container there
	// is none.
	if err := os.WriteFile(pin, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, err := e.app.Push(ctx, task.Slug, PushOpts{}, &out); err != nil || res[0].Err == nil ||
		!strings.Contains(res[0].Err.Error(), "atl repos pin api") {
		t.Errorf("push with a damaged pin: %+v, %v", res, err)
	}
	if _, _, _, err := e.app.RepinOrigin(ctx, "api"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(sandbox.InEnv, "x")
	if _, _, _, err := e.app.RepinOrigin(ctx, "api"); err == nil {
		t.Error("RepinOrigin from inside a task container succeeded")
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

	res, err := e.app.Fetch(ctx, task.Slug, FetchOpts{Repo: "docs"}, nil)
	if err != nil || len(res) != 1 || res[0].Repo != "docs" || res[0].Err != nil {
		t.Fatalf("fetch --repo docs: %+v, %v", res, err)
	}
	if got := git(t, e.canonical("docs"), "rev-parse", "origin/main"); got != docsTip {
		t.Errorf("docs origin/main = %s, want %s", got, docsTip)
	}
	if got := git(t, e.canonical("api"), "rev-parse", "origin/main"); got == apiTip {
		t.Error("api was fetched with --repo docs")
	}

	res, err = e.app.Fetch(ctx, task.Slug, FetchOpts{}, nil)
	if err != nil || len(res) != 2 || res[0].Err != nil || res[1].Err != nil {
		t.Fatalf("fetch: %+v, %v", res, err)
	}
	if got := git(t, e.app.Paths.WorktreePath(task.Slug, "api"), "rev-parse", "origin/main"); got != apiTip {
		t.Errorf("api origin/main = %s, want %s", got, apiTip)
	}

	// Both repos are pinned now; a fetch after origin was pointed at a
	// decoy (one commit ahead) still comes from the pinned origin.
	for _, name := range []string{"api", "docs"} {
		if got := readPin(t, e, name); !got.Equal(gitx.Origin{URLs: []string{e.origin(name)}}) {
			t.Errorf("%s pin = %+v", name, got)
		}
	}
	decoy := e.decoy(t, "docs")
	work := filepath.Join(e.root, "decoy-work")
	git(t, e.root, "clone", "-q", e.origin("docs"), work)
	if err := os.WriteFile(filepath.Join(work, "decoy"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "decoy")
	git(t, work, "commit", "-q", "-m", "decoy")
	git(t, work, "push", "-q", decoy, "HEAD:refs/heads/main")
	git(t, e.canonical("docs"), "remote", "set-url", "origin", decoy)
	docsTip = e.pushOrigin(t, "docs", "newer-docs")
	var out bytes.Buffer
	res, err = e.app.Fetch(ctx, task.Slug, FetchOpts{Repo: "docs"}, &out)
	if err != nil || len(res) != 1 || res[0].Err != nil {
		t.Fatalf("fetch after set-url: %+v, %v\n%s", res, err, out.String())
	}
	if got := git(t, e.canonical("docs"), "rev-parse", "origin/main"); got != docsTip {
		t.Errorf("docs origin/main = %s after set-url, want %s", got, docsTip)
	}
	if !strings.Contains(out.String(), "warning: repo docs: its config now has origin "+decoy) {
		t.Errorf("no warning:\n%s", out.String())
	}
	git(t, e.canonical("docs"), "config", "url."+decoy+".insteadOf", e.origin("docs"))
	res, err = e.app.Fetch(ctx, task.Slug, FetchOpts{Repo: "docs"}, &out)
	if err != nil || len(res) != 1 || res[0].Err == nil || !strings.Contains(res[0].Err.Error(), "url."+decoy+".insteadof") {
		t.Errorf("fetch with an insteadOf rule in the repo: %+v, %v", res, err)
	}
}

// TestNewTaskFetchesThePinnedOrigin: atl's own fetch before branching comes
// from the pinned origin too, with the sandbox on or off.
func TestNewTaskFetchesThePinnedOrigin(t *testing.T) {
	for _, sandboxed := range []bool{true, false} {
		e := newTaskEnv(t)
		ctx := context.Background()
		if sandboxed {
			e.app.Sandbox = &sandbox.Config{Image: "unused"}
		}
		e.addRepo(t, "api", "main")
		if _, err := e.app.NewTask(ctx, NewTaskOpts{Title: "first", Repos: []RepoArg{{"api", model.ModeRW}}}); err != nil {
			t.Fatal(err)
		}
		if got := readPin(t, e, "api"); !got.Equal(gitx.Origin{URLs: []string{e.origin("api")}}) {
			t.Errorf("pin = %+v", got)
		}
		git(t, e.canonical("api"), "remote", "set-url", "origin", e.decoy(t, "api"))
		tip := e.pushOrigin(t, "api", "new")
		task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "second", Repos: []RepoArg{{"api", model.ModeRW}}})
		if err != nil {
			t.Fatal(err)
		}
		if got := task.Repos[0].BaseCommit; got != tip {
			t.Errorf("sandbox %v: second task based on %s, want origin's %s", sandboxed, got, tip)
		}
		if out := e.outString(); !strings.Contains(out, "warning: repo api: its config now has origin") {
			t.Errorf("sandbox %v: no warning:\n%s", sandboxed, out)
		}
	}
}
