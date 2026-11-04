package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/store"
	"github.com/andreujuanc/atelier/internal/tmux"
)

var sockSeq atomic.Int64

type env struct {
	app  *App
	out  string // dir the stub claude records into
	sock string
}

// newEnv builds an App on a temp DB and temp paths, an isolated tmux socket
// (server killed at cleanup) and a stub `claude` first on PATH that records
// its argv and env to out/{argv,env}-<ATL_SESSION> and then sleeps.
func newEnv(t *testing.T) *env {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	root := t.TempDir()
	t.Setenv("TMUX", "")
	t.Setenv("HOME", root) // keep the user's tmux.conf out of the test server
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, ".config"))
	t.Setenv("SHELL", "/bin/sh")

	bin := filepath.Join(root, "bin")
	out := filepath.Join(root, "out")
	for _, d := range []string{bin, out} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub := fmt.Sprintf(`#!/bin/sh
f=%q/argv-$ATL_SESSION
: > "$f.tmp"
for a in "$@"; do printf '%%s\n' "$a" >> "$f.tmp"; done
env > %q/env-$ATL_SESSION
pwd > %q/pwd-$ATL_SESSION
mv "$f.tmp" "$f"
exec sleep 300
`, out, out, out)
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	sock := fmt.Sprintf("atlapp-%d-%d", os.Getpid(), sockSeq.Add(1))
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", sock, "kill-server").Run()
		tmp := os.Getenv("TMUX_TMPDIR")
		if tmp == "" {
			tmp = "/tmp"
		}
		_ = os.Remove(filepath.Join(tmp, fmt.Sprintf("tmux-%d", os.Getuid()), sock))
	})

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
	e := &env{
		app: &App{Paths: paths, Store: st, Tmux: &tmux.Client{Socket: sock}, Out: &bytes.Buffer{},
			Now: time.Now, AtlBin: "/usr/bin/atl"},
		out:  out,
		sock: sock,
	}
	e.startServer(t)
	return e
}

// startServer starts the test tmux server with -f /dev/null (neither
// /etc/tmux.conf nor a user config is loaded) and exit-empty off, then sets
// the given global options ("name value" pairs). Tests that kill the server
// and then launch again call it first, so the client never starts a server
// with the system config.
func (e *env) startServer(t *testing.T, opts ...[2]string) {
	t.Helper()
	args := []string{"-L", e.sock, "-f", os.DevNull, "start-server", ";", "set-option", "-g", "exit-empty", "off"}
	for _, o := range opts {
		args = append(args, ";", "set-option", "-g", o[0], o[1])
	}
	if out, err := exec.Command("tmux", args...).CombinedOutput(); err != nil {
		t.Fatalf("start tmux server: %v: %s", err, out)
	}
}

// crash kills the tmux server and starts a fresh, empty one.
func (e *env) crash(t *testing.T) {
	t.Helper()
	e.tmux(t, "kill-server")
	e.startServer(t)
}

func (e *env) task(t *testing.T, slug string, st model.Status, mkdir bool, repos ...model.TaskRepo) *model.Task {
	t.Helper()
	task := &model.Task{Slug: slug, Title: "T " + slug, Status: st, Branch: "atl/" + slug, PortOffset: 100, Repos: repos}
	if err := e.app.Store.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if mkdir {
		if err := os.MkdirAll(e.app.Paths.TaskDir(slug), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return task
}

func (e *env) session(t *testing.T, id int64) *model.Session {
	t.Helper()
	s, err := e.app.Store.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// argv waits for the stub's argv record of session id.
func (e *env) argv(t *testing.T, id int64) []string {
	t.Helper()
	path := filepath.Join(e.out, "argv-"+strconv.FormatInt(id, 10))
	deadline := time.Now().Add(10 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil {
			return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
		}
		if time.Now().After(deadline) {
			t.Fatalf("claude stub never ran for session %d", id)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// crashClaude kills the claude process in window target and waits for its
// pane to be dead (the server must have remain-on-exit on).
func (e *env) crashClaude(t *testing.T, target string) {
	t.Helper()
	pid, err := strconv.Atoi(e.tmux(t, "display-message", "-p", "-t", target, "#{pane_pid}"))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for e.tmux(t, "display-message", "-p", "-t", target, "#{pane_dead}") != "1" {
		if time.Now().After(deadline) {
			t.Fatal("pane never died")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *env) tmux(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("tmux", append([]string{"-L", e.sock}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("tmux %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestClaudeArgv(t *testing.T) {
	for _, c := range []struct {
		ro     []string
		resume string
		want   []string
	}{
		{nil, "", []string{"claude"}},
		{[]string{"/r/a", "/r/b"}, "", []string{"claude", "--add-dir", "/r/a", "--add-dir", "/r/b"}},
		{[]string{"/r/a"}, "cid", []string{"claude", "--resume", "cid", "--add-dir", "/r/a"}},
		{nil, "cid", []string{"claude", "--resume", "cid"}},
	} {
		if got := ClaudeArgv(c.ro, c.resume); !reflect.DeepEqual(got, c.want) {
			t.Errorf("ClaudeArgv(%v, %q) = %v, want %v", c.ro, c.resume, got, c.want)
		}
	}
}

func TestNewSession(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	task := e.task(t, "fix-login", model.StatusActive, true,
		model.TaskRepo{Name: "api", Mode: model.ModeRW},
		model.TaskRepo{Name: "docs", Mode: model.ModeRO},
		model.TaskRepo{Name: "lib", Mode: model.ModeRO})

	s, err := e.app.NewSession(ctx, "fix")
	if err != nil {
		t.Fatal(err)
	}
	got := e.session(t, s.ID)
	if got.TaskID != task.ID || got.State != model.StateIdle || got.Name != fmt.Sprintf("claude-%d", s.ID) ||
		!strings.HasPrefix(got.TmuxTarget, "@") || got.ClaudeSessionID != "" {
		t.Fatalf("session row = %+v", got)
	}
	if !reflect.DeepEqual(got, s) {
		t.Errorf("returned %+v, stored %+v", s, got)
	}
	tags, err := e.app.Tmux.TaggedWindows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tags[strconv.FormatInt(s.ID, 10)] != got.TmuxTarget {
		t.Fatalf("tags = %v, want %d → %s", tags, s.ID, got.TmuxTarget)
	}
	if name := e.tmux(t, "display-message", "-p", "-t", got.TmuxTarget, "#{session_name}:#{window_name}"); name != "atl-fix-login:"+got.Name {
		t.Errorf("window = %q", name)
	}

	wantArgv := []string{"--add-dir", e.app.Paths.RepoPath("docs"), "--add-dir", e.app.Paths.RepoPath("lib")}
	if argv := e.argv(t, s.ID); !reflect.DeepEqual(argv, wantArgv) {
		t.Errorf("claude argv = %q, want %q", argv, wantArgv)
	}
	envb, _ := os.ReadFile(filepath.Join(e.out, fmt.Sprintf("env-%d", s.ID)))
	for _, want := range []string{
		fmt.Sprintf("ATL_SESSION=%d\n", s.ID),
		"ATL_TASK=fix-login\n",
		"ATL_PORT_OFFSET=100\n",
		"ATL_TASK_DIR=" + e.app.Paths.TaskDir("fix-login") + "\n",
	} {
		if !strings.Contains(string(envb), want) {
			t.Errorf("claude env missing %q", want)
		}
	}
	pwd, _ := os.ReadFile(filepath.Join(e.out, fmt.Sprintf("pwd-%d", s.ID)))
	if strings.TrimSpace(string(pwd)) != e.app.Paths.TaskDir("fix-login") {
		t.Errorf("claude ran in %q", pwd)
	}

	// A second session goes into the same tmux session.
	s2, err := e.app.NewSession(ctx, "fix-login")
	if err != nil {
		t.Fatal(err)
	}
	if s2.ID == s.ID {
		t.Fatal("same session id twice")
	}
	if wins := e.tmux(t, "list-windows", "-t", "=atl-fix-login", "-F", "#{window_name}"); wins != fmt.Sprintf("claude-%d\nclaude-%d", s.ID, s2.ID) {
		t.Errorf("windows = %q", wins)
	}
}

func TestNewSessionRefusals(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.task(t, "finished", model.StatusDone, true)
	e.task(t, "dropped", model.StatusCancelled, true)
	e.task(t, "nodir", model.StatusActive, false)
	for _, ref := range []string{"finished", "dropped", "nodir", "missing"} {
		if _, err := e.app.NewSession(ctx, ref); err == nil {
			t.Errorf("NewSession(%s) succeeded", ref)
		}
	}
	all, err := e.app.Store.ListSessionsByState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Errorf("refused NewSession left rows: %+v", all)
	}
	if e.app.Tmux.HasSession(ctx, "atl-finished") {
		t.Error("tmux session created for a done task")
	}
}

func TestNewSessionLaunchFailure(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.task(t, "t", model.StatusActive, true)
	e.app.Tmux = &tmux.Client{Socket: e.sock}
	t.Setenv("PATH", t.TempDir()) // no tmux at all
	if _, err := e.app.NewSession(ctx, "t"); err == nil {
		t.Fatal("NewSession succeeded without tmux")
	}
	all, _ := e.app.Store.ListSessionsByState(ctx)
	if len(all) != 1 || all[0].State != model.StateEnded {
		t.Fatalf("sessions after failed launch = %+v", all)
	}
	// And Recover reports tmux being unavailable once something is live.
	live := &model.Session{TaskID: all[0].TaskID, State: model.StateIdle, TmuxTarget: "@1"}
	if err := e.app.Store.CreateSession(ctx, live); err != nil {
		t.Fatal(err)
	}
	if n, err := e.app.Recover(ctx); err == nil || n != 0 {
		t.Fatalf("Recover without tmux = %d, %v", n, err)
	}
}

func TestRecover(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	task := e.task(t, "t", model.StatusActive, true)

	if n, err := e.app.Recover(ctx); err != nil || n != 0 {
		t.Fatalf("Recover on empty DB = %d, %v", n, err)
	}
	s1, err := e.app.NewSession(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := e.app.NewSession(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	mk := func(st model.SessionState, target string, created time.Time) *model.Session {
		s := &model.Session{TaskID: task.ID, State: st, TmuxTarget: target, CreatedAt: created, LastEventAt: created}
		if err := e.app.Store.CreateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	ended := mk(model.StateEnded, "@99", old)
	waiting := mk(model.StateWaiting, "@98", old)    // window long gone
	launching := mk(model.StateIdle, "", time.Now()) // NewSession mid-launch
	foreign := mk(model.StateRunning, "", old)       // hook row: claude not launched by atl

	e.argv(t, s1.ID)
	e.tmux(t, "kill-window", "-t", e.session(t, s1.ID).TmuxTarget)

	n, err := e.app.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("Recover = %d, want 2", n)
	}
	for _, c := range []struct {
		id   int64
		want model.SessionState
	}{
		{s1.ID, model.StateStopped},
		{s2.ID, model.StateIdle},
		{ended.ID, model.StateEnded},
		{waiting.ID, model.StateStopped},
		{launching.ID, model.StateIdle},
		{foreign.ID, model.StateRunning},
	} {
		if got := e.session(t, c.id); got.State != c.want {
			t.Errorf("session %d: state %s, want %s", c.id, got.State, c.want)
		}
	}
	if got := e.session(t, ended.ID); !got.LastEventAt.Equal(old.Truncate(time.Millisecond)) {
		t.Errorf("ended session touched: last_event_at %v", got.LastEventAt)
	}
	if n, err := e.app.Recover(ctx); err != nil || n != 0 {
		t.Fatalf("second Recover = %d, %v", n, err)
	}

	// The whole server going away stops everything.
	e.tmux(t, "kill-server")
	if n, err := e.app.Recover(ctx); err != nil || n != 1 {
		t.Fatalf("Recover after kill-server = %d, %v", n, err)
	}
	if got := e.session(t, s2.ID); got.State != model.StateStopped {
		t.Errorf("s2 state %s", got.State)
	}
	for _, id := range []int64{launching.ID, foreign.ID} {
		if got := e.session(t, id); !got.State.Live() {
			t.Errorf("session %d without tmux target touched by Recover: %s", id, got.State)
		}
	}
}

func TestRecoverUnreachableServer(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	task := e.task(t, "t", model.StatusActive, true)
	s := &model.Session{TaskID: task.ID, State: model.StateRunning, TmuxTarget: "@1"}
	if err := e.app.Store.CreateSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	if os.Getuid() == 0 {
		t.Skip("root ignores socket permissions")
	}
	// "error connecting to ... (Permission denied)" must not read as "no
	// server" (which would stop every session).
	sockPath := e.tmux(t, "display-message", "-p", "#{socket_path}")
	if err := os.Chmod(sockPath, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(sockPath, 0o660)
	if n, err := e.app.Recover(ctx); err == nil || n != 0 {
		t.Fatalf("Recover with unreachable server = %d, %v; want error", n, err)
	}
	if got := e.session(t, s.ID); got.State != model.StateRunning {
		t.Errorf("state %s after failed Recover", got.State)
	}
}

func TestResume(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	task := e.task(t, "t", model.StatusActive, true, model.TaskRepo{Name: "docs", Mode: model.ModeRO})

	s1, err := e.app.NewSession(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	e.argv(t, s1.ID)
	s1.ClaudeSessionID = "cid-1" // as the SessionStart hook would
	if err := e.app.Store.UpdateSession(ctx, s1); err != nil {
		t.Fatal(err)
	}
	s1 = e.session(t, s1.ID)
	s2, err := e.app.NewSession(ctx, "t") // never got a claude id
	if err != nil {
		t.Fatal(err)
	}
	e.argv(t, s2.ID)
	ended := &model.Session{TaskID: task.ID, ClaudeSessionID: "cid-old", State: model.StateEnded}
	if err := e.app.Store.CreateSession(ctx, ended); err != nil {
		t.Fatal(err)
	}

	// Nothing stopped yet: nothing to do.
	if n, err := e.app.Resume(ctx, "t"); err != nil || n != 0 {
		t.Fatalf("Resume with live sessions = %d, %v", n, err)
	}

	// Simulate a crash.
	if err := e.app.Tmux.KillSession(ctx, "atl-t"); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(e.out, fmt.Sprintf("argv-%d", s1.ID)))
	oldTarget := e.session(t, s1.ID).TmuxTarget

	n, err := e.app.Resume(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("Resume = %d, want 1", n)
	}
	got := e.session(t, s1.ID)
	if got.State != model.StateIdle || got.ClaudeSessionID != "cid-1" || got.TmuxTarget == "" || got.Name != fmt.Sprintf("claude-%d", s1.ID) {
		t.Fatalf("resumed session = %+v (old target %s)", got, oldTarget)
	}
	tags, _ := e.app.Tmux.TaggedWindows(ctx)
	if tags[strconv.FormatInt(s1.ID, 10)] != got.TmuxTarget || len(tags) != 1 {
		t.Errorf("tags = %v", tags)
	}
	want := []string{"--resume", "cid-1", "--add-dir", e.app.Paths.RepoPath("docs")}
	if argv := e.argv(t, s1.ID); !reflect.DeepEqual(argv, want) {
		t.Errorf("resumed argv = %q, want %q", argv, want)
	}
	if st := e.session(t, s2.ID).State; st != model.StateEnded {
		t.Errorf("stopped session without claude id: %s, want ended", st)
	}
	if st := e.session(t, ended.ID).State; st != model.StateEnded {
		t.Errorf("ended session: %s", st)
	}

	// Resuming a closed task with stopped sessions is refused.
	e.crash(t)
	if err := e.app.Store.SetTaskStatus(ctx, task.ID, model.StatusDone); err != nil {
		t.Fatal(err)
	}
	if n, err := e.app.Resume(ctx, "t"); err == nil || n != 0 {
		t.Fatalf("Resume of done task = %d, %v", n, err)
	}
	if st := e.session(t, s1.ID).State; st != model.StateStopped {
		t.Errorf("refused resume changed state to %s", st)
	}
}

func TestOpen(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.task(t, "t", model.StatusActive, true)
	wantArgs := []string{"tmux", "-L", e.sock, "attach-session", "-t", "=atl-t"}

	// No tmux session, no sessions: a new Claude session is started.
	cmd, err := e.app.Open(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cmd.Args, wantArgs) {
		t.Errorf("attach cmd = %v, want %v", cmd.Args, wantArgs)
	}
	if !e.app.Tmux.HasSession(ctx, "atl-t") {
		t.Fatal("Open did not create the tmux session")
	}
	all, _ := e.app.Store.ListSessionsByState(ctx)
	if len(all) != 1 || all[0].State != model.StateIdle {
		t.Fatalf("sessions after first Open = %+v", all)
	}
	s := all[0]
	e.argv(t, s.ID)

	// Session exists: just attach.
	if _, err := e.app.Open(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	if all, _ := e.app.Store.ListSessionsByState(ctx); len(all) != 1 {
		t.Fatalf("second Open created sessions: %+v", all)
	}

	// After a crash, a session with a claude id is resumed instead.
	s.ClaudeSessionID = "cid-9"
	if err := e.app.Store.UpdateSession(ctx, &s); err != nil {
		t.Fatal(err)
	}
	e.crash(t)
	os.Remove(filepath.Join(e.out, fmt.Sprintf("argv-%d", s.ID)))
	if _, err := e.app.Open(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	all, _ = e.app.Store.ListSessionsByState(ctx)
	if len(all) != 1 || all[0].State != model.StateIdle {
		t.Fatalf("sessions after resuming Open = %+v", all)
	}
	if argv := e.argv(t, s.ID); !reflect.DeepEqual(argv, []string{"--resume", "cid-9"}) {
		t.Errorf("argv = %q", argv)
	}

	// After a crash with nothing resumable, a fresh session is started.
	e.crash(t)
	if err := e.app.Store.SetSessionState(ctx, s.ID, model.StateEnded, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.Open(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	live, _ := e.app.Store.ListSessionsByState(ctx, model.StateIdle)
	if len(live) != 1 || live[0].ID == s.ID {
		t.Fatalf("live sessions after fresh Open = %+v", live)
	}

	// Inside tmux the attach command switches clients.
	t.Setenv("TMUX", "/tmp/tmux-1000/default,1,0")
	cmd, err = e.app.Open(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Args[3] != "switch-client" {
		t.Errorf("attach cmd inside tmux = %v", cmd.Args)
	}

	// Done tasks without a tmux session cannot be opened.
	e.task(t, "old", model.StatusDone, true)
	if _, err := e.app.Open(ctx, "old"); err == nil {
		t.Error("Open of done task succeeded")
	}
}

// TestUserTmuxOptions runs the session lifecycle on a server with options a
// user's tmux.conf might set.
func TestUserTmuxOptions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, o := range [][2]string{
		{"base-index", "1"},
		{"remain-on-exit", "on"},
		{"automatic-rename", "on"},
		{"default-command", "/nonexistent/odd-shell --weird"},
	} {
		e.tmux(t, "set-option", "-g", o[0], o[1])
	}
	e.task(t, "t", model.StatusActive, true)

	s1, err := e.app.NewSession(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := e.app.NewSession(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	e.argv(t, s1.ID)
	e.argv(t, s2.ID)
	if got := e.tmux(t, "list-windows", "-t", "=atl-t", "-F", "#{window_index} #{window_name}"); got != fmt.Sprintf("1 claude-%d\n2 claude-%d", s1.ID, s2.ID) {
		t.Errorf("windows = %q", got)
	}
	s1.ClaudeSessionID = "cid-1"
	if err := e.app.Store.UpdateSession(ctx, s1); err != nil {
		t.Fatal(err)
	}

	// Claude exits; remain-on-exit keeps its window with a dead pane.
	e.crashClaude(t, s1.TmuxTarget)
	if n, err := e.app.Recover(ctx); err != nil || n != 1 {
		t.Fatalf("Recover = %d, %v; want 1", n, err)
	}
	if st := e.session(t, s1.ID).State; st != model.StateStopped {
		t.Fatalf("s1 state %s, want stopped", st)
	}
	if st := e.session(t, s2.ID).State; st != model.StateIdle {
		t.Fatalf("s2 state %s, want idle", st)
	}

	os.Remove(filepath.Join(e.out, fmt.Sprintf("argv-%d", s1.ID)))
	if n, err := e.app.Resume(ctx, "t"); err != nil || n != 1 {
		t.Fatalf("Resume = %d, %v; want 1", n, err)
	}
	got := e.session(t, s1.ID)
	if got.State != model.StateIdle || got.TmuxTarget == "" || got.TmuxTarget == s1.TmuxTarget || got.ClaudeSessionID != "cid-1" {
		t.Fatalf("resumed session = %+v (old target %s)", got, s1.TmuxTarget)
	}
	if argv := e.argv(t, s1.ID); !reflect.DeepEqual(argv, []string{"--resume", "cid-1"}) {
		t.Errorf("resumed argv = %q", argv)
	}
	tags, err := e.app.Tmux.TaggedWindows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{strconv.FormatInt(s1.ID, 10): got.TmuxTarget, strconv.FormatInt(s2.ID, 10): s2.TmuxTarget}
	if !reflect.DeepEqual(tags, want) {
		t.Errorf("tags = %v, want %v", tags, want)
	}
	// The dead remain-on-exit window was killed before the relaunch.
	wins := strings.Fields(e.tmux(t, "list-windows", "-a", "-F", "#{window_id}"))
	sort.Strings(wins)
	wantWins := []string{got.TmuxTarget, s2.TmuxTarget}
	sort.Strings(wantWins)
	if !reflect.DeepEqual(wins, wantWins) {
		t.Errorf("windows = %v, want %v (dead window %s killed)", wins, wantWins, s1.TmuxTarget)
	}
	if n, err := e.app.Recover(ctx); err != nil || n != 0 {
		t.Errorf("Recover after resume = %d, %v", n, err)
	}
}

// TestResumeOnlyWindowDead resumes a session whose dead remain-on-exit
// window is the only window of the task's tmux session: the new window is
// launched before the dead one is killed, so the tmux session survives.
func TestResumeOnlyWindowDead(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.tmux(t, "set-option", "-g", "remain-on-exit", "on")
	e.task(t, "t", model.StatusActive, true)
	s, err := e.app.NewSession(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	e.argv(t, s.ID)
	s.ClaudeSessionID = "cid-1"
	if err := e.app.Store.UpdateSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	e.crashClaude(t, s.TmuxTarget)
	tsess := e.tmux(t, "display-message", "-p", "-t", "=atl-t:", "#{session_id}")

	os.Remove(filepath.Join(e.out, fmt.Sprintf("argv-%d", s.ID)))
	if n, err := e.app.Resume(ctx, "t"); err != nil || n != 1 {
		t.Fatalf("Resume = %d, %v; want 1", n, err)
	}
	if argv := e.argv(t, s.ID); !reflect.DeepEqual(argv, []string{"--resume", "cid-1"}) {
		t.Errorf("resumed argv = %q", argv)
	}
	got := e.session(t, s.ID)
	if got.State != model.StateIdle || got.TmuxTarget == "" || got.TmuxTarget == s.TmuxTarget {
		t.Fatalf("resumed session = %+v (old target %s)", got, s.TmuxTarget)
	}
	if now := e.tmux(t, "display-message", "-p", "-t", "=atl-t:", "#{session_id}"); now != tsess {
		t.Errorf("tmux session replaced: %s → %s", tsess, now)
	}
	if wins := e.tmux(t, "list-windows", "-a", "-F", "#{window_id} #{pane_dead}"); wins != got.TmuxTarget+" 0" {
		t.Errorf("windows = %q, want only the live %s", wins, got.TmuxTarget)
	}
}

// TestOpenDeadWindow opens a task whose tmux session survives with only a
// dead (remain-on-exit) claude window: Open relaunches rather than attaching.
func TestOpenDeadWindow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.tmux(t, "set-option", "-g", "remain-on-exit", "on")
	e.task(t, "t", model.StatusActive, true)
	if _, err := e.app.Open(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	all, _ := e.app.Store.ListSessionsByState(ctx)
	if len(all) != 1 {
		t.Fatalf("sessions after first Open = %+v", all)
	}
	s := all[0]
	e.argv(t, s.ID)
	s.ClaudeSessionID = "cid-1"
	if err := e.app.Store.UpdateSession(ctx, &s); err != nil {
		t.Fatal(err)
	}

	// Crashed with a claude id: resumed.
	e.crashClaude(t, s.TmuxTarget)
	os.Remove(filepath.Join(e.out, fmt.Sprintf("argv-%d", s.ID)))
	if _, err := e.app.Open(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	if argv := e.argv(t, s.ID); !reflect.DeepEqual(argv, []string{"--resume", "cid-1"}) {
		t.Errorf("argv after Open = %q, want a resume", argv)
	}
	got := e.session(t, s.ID)
	if got.State != model.StateIdle || got.TmuxTarget == s.TmuxTarget {
		t.Fatalf("session after Open = %+v (old target %s)", got, s.TmuxTarget)
	}
	if wins := e.tmux(t, "list-windows", "-a", "-F", "#{window_id} #{pane_dead}"); wins != got.TmuxTarget+" 0" {
		t.Errorf("windows = %q, want only the live %s", wins, got.TmuxTarget)
	}

	// Crashed without a claude id: a new session is started.
	got.ClaudeSessionID = ""
	if err := e.app.Store.UpdateSession(ctx, got); err != nil {
		t.Fatal(err)
	}
	e.crashClaude(t, got.TmuxTarget)
	if _, err := e.app.Open(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	live, _ := e.app.Store.ListSessionsByState(ctx, model.StateIdle)
	if len(live) != 1 || live[0].ID == s.ID {
		t.Fatalf("live sessions after Open = %+v", live)
	}
	e.argv(t, live[0].ID)
	tags, err := e.app.Tmux.TaggedWindows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{strconv.FormatInt(live[0].ID, 10): live[0].TmuxTarget}; !reflect.DeepEqual(tags, want) {
		t.Errorf("live tags = %v, want %v", tags, want)
	}
}

// TestRecoverStaleSnapshot replays the Recover-vs-Resume interleaving that
// used to launch a session twice: a Recover whose session snapshot predates
// a relaunch (here into a window that reuses the old window id, as happens
// after a tmux server restart) must not mark the relaunched session stopped.
func TestRecoverStaleSnapshot(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.task(t, "t", model.StatusActive, true)
	s, err := e.app.NewSession(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	e.argv(t, s.ID)
	st := e.app.Store
	snap, err := st.ListLaunchedLive(ctx) // the slow Recover's snapshot
	if err != nil || len(snap) != 1 {
		t.Fatalf("ListLaunchedLive = %v, %v", snap, err)
	}
	e.crash(t)
	// A concurrent Resume: its own Recover marks the session stopped, it
	// claims it and records a relaunch in a window with the same id.
	if n, err := e.app.Recover(ctx); err != nil || n != 1 {
		t.Fatalf("Recover = %d, %v", n, err)
	}
	if ok, err := st.ClaimStopped(ctx, s.ID, time.Now()); !ok || err != nil {
		t.Fatalf("ClaimStopped = %v, %v", ok, err)
	}
	if err := st.SetSessionLaunch(ctx, s.ID, snap[0].Target, s.Name); err != nil {
		t.Fatal(err)
	}
	// The slow Recover, whose tmux listing missed the new window's tag.
	if ok, err := st.MarkStoppedIfLaunch(ctx, snap[0], time.Now()); ok || err != nil {
		t.Fatalf("MarkStoppedIfLaunch(stale) = %v, %v; want false", ok, err)
	}
	if got := e.session(t, s.ID); got.State != model.StateIdle {
		t.Fatalf("relaunched session = %+v, want idle", got)
	}
	// A fresh snapshot of the same launch still marks it.
	fresh, err := st.ListLaunchedLive(ctx)
	if err != nil || len(fresh) != 1 {
		t.Fatalf("ListLaunchedLive = %v, %v", fresh, err)
	}
	if ok, err := st.MarkStoppedIfLaunch(ctx, fresh[0], time.Now()); !ok || err != nil {
		t.Fatalf("MarkStoppedIfLaunch(fresh) = %v, %v; want true", ok, err)
	}
}

// TestResumeConcurrent checks that concurrent Resumes launch each stopped
// session exactly once.
func TestResumeConcurrent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.task(t, "t", model.StatusActive, true)
	var ids []int64
	for i := 0; i < 3; i++ {
		s, err := e.app.NewSession(ctx, "t")
		if err != nil {
			t.Fatal(err)
		}
		e.argv(t, s.ID)
		s.ClaudeSessionID = fmt.Sprintf("cid-%d", s.ID)
		if err := e.app.Store.UpdateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.ID)
	}
	e.crash(t)

	const workers = 4
	var total atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := e.app.Resume(ctx, "t")
			total.Add(int64(n))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if total.Load() != int64(len(ids)) {
		t.Errorf("sessions resumed = %d, want %d", total.Load(), len(ids))
	}
	wins := strings.Split(e.tmux(t, "list-windows", "-t", "=atl-t", "-F", "#{window_name}"), "\n")
	if len(wins) != len(ids) {
		t.Errorf("windows = %q, want one per session", wins)
	}
	tags, _ := e.app.Tmux.TaggedWindows(ctx)
	for _, id := range ids {
		got := e.session(t, id)
		if got.State != model.StateIdle || tags[strconv.FormatInt(id, 10)] != got.TmuxTarget {
			t.Errorf("session %d = %+v, tags %v", id, got, tags)
		}
	}
}

// TestResumeLaunchFailure checks that a failed relaunch leaves the session
// stopped (and resumable) with its claude id intact.
func TestResumeLaunchFailure(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.task(t, "t", model.StatusActive, true)
	s, err := e.app.NewSession(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	e.argv(t, s.ID)
	s.ClaudeSessionID = "cid-1"
	if err := e.app.Store.UpdateSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	e.crash(t)

	// A tmux that works except for launching windows.
	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	wrapper := fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do case $a in new-window|new-session) echo boom >&2; exit 1;; esac; done\nexec %q \"$@\"\n", real)
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	if n, err := e.app.Resume(ctx, "t"); err == nil || n != 0 || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Resume with failing launch = %d, %v", n, err)
	}
	got := e.session(t, s.ID)
	if got.State != model.StateStopped || got.ClaudeSessionID != "cid-1" {
		t.Fatalf("session after failed resume = %+v", got)
	}
	// Once tmux works again the session resumes.
	t.Setenv("PATH", strings.TrimPrefix(os.Getenv("PATH"), bin+string(os.PathListSeparator)))
	if n, err := e.app.Resume(ctx, "t"); err != nil || n != 1 {
		t.Fatalf("Resume = %d, %v", n, err)
	}
}
