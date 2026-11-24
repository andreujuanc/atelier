package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/andreujuanc/atelier/internal/model"
)

// gitInit makes path an empty git repository with one commit.
func gitInit(t *testing.T, path string) {
	t.Helper()
	for _, args := range [][]string{{"init", "-q", "-b", "main", path},
		{"-C", path, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

// With the sandbox on and a live session, AddRepo refuses; AddRepos refuses
// while things run (changing nothing), and otherwise closes the session,
// replaces the container, adds the repo and resumes the same conversation.
func TestAddReposRestartsSandboxedSessions(t *testing.T) {
	e, fd := sandboxEnv(t)
	ctx := context.Background()
	p := e.app.Paths
	if err := os.MkdirAll(filepath.Join(p.RepoPath("api"), ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitInit(t, p.RepoPath("docs"))
	gitInit(t, p.RepoPath("web"))
	task := e.task(t, "boxed", model.StatusActive, true, model.TaskRepo{Name: "api", Mode: model.ModeRW})
	s, err := e.app.NewSession(ctx, "boxed")
	if err != nil {
		t.Fatal(err)
	}
	e.argv(t, s.ID)
	s = e.session(t, s.ID)
	s.ClaudeSessionID = "cid-1" // as the SessionStart hook would
	if err := e.app.Store.UpdateSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	oldTarget := e.session(t, s.ID).TmuxTarget
	repos := func() []string {
		t.Helper()
		got, err := e.app.Store.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, r := range got.Repos {
			names = append(names, r.Name)
		}
		return names
	}

	if _, err := e.app.AddRepo(ctx, "boxed", RepoArg{"docs", model.ModeRO}, true); !errors.Is(err, ErrRestartNeeded) {
		t.Fatalf("AddRepo with a live sandboxed session: %v", err)
	}

	sid := fmt.Sprint(s.ID)
	idle := "1\t0\t\t/sbin/docker-init -- sleep infinity\n7\t1\t\tsleep infinity\n8\t0\t" + sid + "\tclaude --resume x\n" +
		"9\t8\t" + sid + "\tnode /mcp/server.js\n"
	fd.ps = idle + "20\t8\t" + sid + "\t/bin/bash -c source snap.sh && eval 'npm run dev'\n21\t20\t" + sid + "\tnpm run dev\n" +
		"30\t1\t\tpython3 -m http.server\n40\t0\t\tbash\n"
	if err := e.app.Store.SetSessionState(ctx, s.ID, model.StateRunning, e.app.now()); err != nil {
		t.Fatal(err)
	}
	_, err = e.app.AddRepos(ctx, "boxed", []RepoArg{{"docs", model.ModeRO}}, AddOpts{NoFetch: true})
	var busy *BusyError
	if !errors.As(err, &busy) {
		t.Fatalf("AddRepos while things run: %v", err)
	}
	name := fmt.Sprintf("claude-%d", s.ID)
	want := []string{"session " + name + ": Claude is working", "session " + name + " runs npm run dev",
		"process 30: python3 -m http.server", "process 40: bash"}
	if !slices.Equal(busy.Running, want) {
		t.Fatalf("running = %q, want %q", busy.Running, want)
	}
	if got := repos(); !slices.Equal(got, []string{"api"}) || len(fd.find("rm")) != 0 || e.session(t, s.ID).TmuxTarget != oldTarget {
		t.Fatalf("a refused AddRepos changed things: repos %v, rm %v", got, fd.find("rm"))
	}

	// Idle, but a background subagent still works (in Claude's own process,
	// so only its Stop hook tells): busy.
	if err := e.app.Store.SetIdle(ctx, s.ID, "subagent: Review the diff", e.app.now()); err != nil {
		t.Fatal(err)
	}
	fd.ps = idle
	_, err = e.app.AddRepos(ctx, "boxed", []RepoArg{{"docs", model.ModeRO}}, AddOpts{NoFetch: true})
	if !errors.As(err, &busy) || !slices.Equal(busy.Running, []string{"session " + name + " works in the background: subagent: Review the diff"}) {
		t.Fatalf("AddRepos with a background subagent: %v", err)
	}
	if running, err := e.app.AddCheck(ctx, "boxed", []RepoArg{{"docs", model.ModeRO}}); err != nil || len(running) != 1 {
		t.Fatalf("AddCheck with a background subagent: %q, %v", running, err)
	}

	// Idle: restarted around the add.
	if err := e.app.Store.SetIdle(ctx, s.ID, "", e.app.now()); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(e.out, "argv-"+sid))
	res, err := e.app.AddRepos(ctx, "boxed", []RepoArg{{"docs", model.ModeRO}}, AddOpts{NoFetch: true})
	if err != nil || len(res.Added) != 1 || len(res.Failed) != 0 || res.Restarted != 1 {
		t.Fatalf("AddRepos idle = %+v, %v", res, err)
	}
	if got := repos(); !slices.Equal(got, []string{"api", "docs"}) {
		t.Fatalf("repos %v", got)
	}
	if len(fd.find("rm")) != 1 {
		t.Fatalf("container not replaced: %v", fd.changes())
	}
	if argv := e.argv(t, s.ID); !slices.Equal(argv, []string{"--resume=cid-1", "--add-dir", p.RepoPath("docs")}) {
		t.Fatalf("resumed argv %q", argv)
	}
	if got := e.session(t, s.ID); got.TmuxTarget == oldTarget || !got.State.Live() {
		t.Fatalf("session not resumed: %+v", got)
	}
	if !strings.Contains(e.app.Out.(*bytes.Buffer).String(), "restarted 1 session(s) of boxed") {
		t.Fatalf("output:\n%s", e.app.Out.(*bytes.Buffer).String())
	}

	// Forced: things run, the restart goes on.
	fd.ps = idle + "40\t0\t\tbash\n"
	os.Remove(filepath.Join(e.out, "argv-"+sid))
	res, err = e.app.AddRepos(ctx, "boxed", []RepoArg{{"web", model.ModeRO}}, AddOpts{NoFetch: true, Force: true})
	if err != nil || len(res.Added) != 1 || res.Restarted != 1 {
		t.Fatalf("AddRepos forced = %+v, %v", res, err)
	}
	if argv := e.argv(t, s.ID); !slices.Contains(argv, p.RepoPath("web")) {
		t.Fatalf("resumed argv %q lacks web", argv)
	}

	// Repos that cannot be added (missing, already there, twice) are found
	// before the sessions are touched: no restart for nothing.
	fd.ps = idle
	rms := len(fd.find("rm"))
	target := e.session(t, s.ID).TmuxTarget
	res, err = e.app.AddRepos(ctx, "boxed", []RepoArg{{"nope", model.ModeRO}, {"docs", model.ModeRO}}, AddOpts{NoFetch: true, Force: true})
	if err != nil || len(res.Added) != 0 || len(res.Failed) != 2 || res.Restarted != 0 {
		t.Fatalf("AddRepos failing = %+v, %v", res, err)
	}
	if len(fd.find("rm")) != rms || e.session(t, s.ID).TmuxTarget != target {
		t.Fatal("sessions restarted for repos that could not be added")
	}

	// An interrupt once the sessions are closed does not stop the rest:
	// the repo is added and the session resumed.
	gitInit(t, p.RepoPath("lib"))
	cctx, cancel := context.WithCancel(ctx)
	fd.onRm = cancel
	os.Remove(filepath.Join(e.out, "argv-"+sid))
	res, err = e.app.AddRepos(cctx, "boxed", []RepoArg{{"lib", model.ModeRO}}, AddOpts{NoFetch: true})
	fd.onRm = nil
	if err != nil || len(res.Added) != 1 || res.Restarted != 1 {
		t.Fatalf("AddRepos interrupted = %+v, %v", res, err)
	}
	if argv := e.argv(t, s.ID); !slices.Contains(argv, p.RepoPath("lib")) {
		t.Fatalf("resumed argv %q lacks lib", argv)
	}

	// A live session whose Claude is not in the container, or that has not
	// reported its Claude id: what it runs, or how to resume it, is unknown.
	fd.ps = "1\t0\t\t/sbin/docker-init -- sleep infinity\n7\t1\t\tsleep infinity\n"
	gitInit(t, p.RepoPath("more"))
	_, err = e.app.AddRepos(ctx, "boxed", []RepoArg{{"more", model.ModeRO}}, AddOpts{NoFetch: true})
	if !errors.As(err, &busy) || len(busy.Running) != 1 || !strings.Contains(busy.Running[0], "not found in the container") {
		t.Fatalf("Claude missing from the container: %v", err)
	}
	fd.ps = idle
	fresh, err := e.app.NewSession(ctx, "boxed") // no SessionStart yet
	if err != nil {
		t.Fatal(err)
	}
	fd.ps = idle + "50\t0\t" + fmt.Sprint(fresh.ID) + "\tclaude\n"
	_, err = e.app.AddRepos(ctx, "boxed", []RepoArg{{"more", model.ModeRO}}, AddOpts{NoFetch: true})
	if !errors.As(err, &busy) || !slices.Contains(busy.Running, fmt.Sprintf("session claude-%d: has not reported its Claude session yet, so it could not be resumed", fresh.ID)) {
		t.Fatalf("session without a Claude id: %v", err)
	}
}

// Without the sandbox, a live session sees a rw worktree added under it:
// no restart. A ro repo reaches Claude only at launch: restart.
func TestAddReposWithoutSandbox(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.app.Paths
	gitInit(t, p.RepoPath("docs"))
	e.task(t, "plain", model.StatusActive, true)
	s, err := e.app.NewSession(ctx, "plain")
	if err != nil {
		t.Fatal(err)
	}
	e.argv(t, s.ID)
	s = e.session(t, s.ID)
	s.ClaudeSessionID = "cid-2"
	if err := e.app.Store.UpdateSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	if need, err := e.app.addNeedsRestart(ctx, s2task(t, e, "plain"), []RepoArg{{"docs", model.ModeRW}}); err != nil || need {
		t.Fatalf("rw without sandbox needs restart: %v %v", need, err)
	}
	if err := e.app.Store.SetSessionState(ctx, s.ID, model.StateRunning, e.app.now()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.AddRepos(ctx, "plain", []RepoArg{{"docs", model.ModeRO}}, AddOpts{NoFetch: true}); !errors.As(err, new(*BusyError)) {
		t.Fatalf("ro while Claude works: %v", err)
	}
	if err := e.app.Store.SetSessionState(ctx, s.ID, model.StateIdle, e.app.now()); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(e.out, fmt.Sprintf("argv-%d", s.ID)))
	res, err := e.app.AddRepos(ctx, "plain", []RepoArg{{"docs", model.ModeRO}}, AddOpts{NoFetch: true})
	if err != nil || res.Restarted != 1 {
		t.Fatalf("ro idle = %+v, %v", res, err)
	}
	if argv := e.argv(t, s.ID); !slices.Equal(argv, []string{"--resume=cid-2", "--add-dir", p.RepoPath("docs")}) {
		t.Fatalf("resumed argv %q", argv)
	}
}

func s2task(t *testing.T, e *env, slug string) *model.Task {
	t.Helper()
	task, err := e.app.Store.ResolveTask(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// A forced restart ends a live session that has not reported its Claude id
// (it cannot be resumed) with a warning, instead of relaunching it; the
// repo is still added.
func TestAddReposForcedEndsUnreportedSession(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	gitInit(t, e.app.Paths.RepoPath("docs"))
	e.task(t, "plain", model.StatusActive, true)
	s, err := e.app.NewSession(ctx, "plain") // no SessionStart: no Claude id
	if err != nil {
		t.Fatal(err)
	}
	e.argv(t, s.ID)
	if _, err := e.app.AddRepos(ctx, "plain", []RepoArg{{"docs", model.ModeRO}}, AddOpts{NoFetch: true}); !errors.As(err, new(*BusyError)) {
		t.Fatalf("unforced add with an unreported session: %v", err)
	}
	os.Remove(filepath.Join(e.out, fmt.Sprintf("argv-%d", s.ID)))
	res, err := e.app.AddRepos(ctx, "plain", []RepoArg{{"docs", model.ModeRO}}, AddOpts{NoFetch: true, Force: true})
	if err != nil || len(res.Added) != 1 || res.Restarted != 0 {
		t.Fatalf("forced add = %+v, %v", res, err)
	}
	if got := e.session(t, s.ID); got.State != model.StateEnded {
		t.Fatalf("unreported session is %s, want ended", got.State)
	}
	if out := e.app.Out.(*bytes.Buffer).String(); !strings.Contains(out, "had not reported its Claude session: ended, not resumed") {
		t.Fatalf("no warning:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(e.out, fmt.Sprintf("argv-%d", s.ID))); err == nil {
		t.Fatal("the unreported session was launched again")
	}
	if live, err := e.app.liveSessions(ctx, s2task(t, e, "plain")); err != nil || len(live) != 0 {
		t.Fatalf("live sessions after the restart: %v, %v", live, err)
	}
}
