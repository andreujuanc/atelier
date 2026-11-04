package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
)

func TestReadTranscriptInfo(t *testing.T) {
	info, err := readTranscriptInfo(filepath.Join("testdata", "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	want := transcriptInfo{Title: "Login fix", FirstPrompt: "Fix the login bug", LastPrompt: "Ship it"}
	if info != want {
		t.Fatalf("info = %+v, want %+v", info, want)
	}

	dir := t.TempDir()
	write := func(name string, lines ...string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	user := func(text string) string {
		b, _ := json.Marshal(text)
		return `{"type":"user","message":{"role":"user","content":` + string(b) + `}}`
	}

	// A custom title (/rename) beats generated ones, even newer ones; the
	// newest of each kind wins. Text is sanitised to one line.
	p := write("titles.jsonl",
		`{"type":"summary","summary":"old summary","leafUuid":"x"}`,
		`{"type":"ai-title","aiTitle":"first ai title","sessionId":"s"}`,
		user("first\nprompt\u001b[2J here"),
		`{"type":"custom-title","customTitle":"my name","sessionId":"s"}`,
		`{"type":"custom-title","customTitle":"my\tnewer\u202ename","sessionId":"s"}`,
		`{"type":"custom-title","customTitle":"   ","sessionId":"s"}`,
		`{"type":"ai-title","aiTitle":"later ai title","sessionId":"s"}`,
		user("[Request interrupted by user]"),
		user("last one"),
		user("<command-name>/clear</command-name>"),
	)
	info, err = readTranscriptInfo(p)
	if err != nil {
		t.Fatal(err)
	}
	want = transcriptInfo{Title: "my newer name", FirstPrompt: "first prompt [2J here", LastPrompt: "last one"}
	if info != want {
		t.Fatalf("info = %+v, want %+v", info, want)
	}

	// Generated titles only: the newest wins.
	p = write("ai.jsonl", `{"type":"summary","summary":"a summary"}`, `{"type":"ai-title","aiTitle":"the ai title"}`, user("hi"))
	if info, _ := readTranscriptInfo(p); info.Title != "the ai title" || info.FirstPrompt != "hi" || info.LastPrompt != "hi" {
		t.Fatalf("ai info = %+v", info)
	}
	// Nothing usable.
	p = write("empty.jsonl", "not json", `{"type":"assistant","message":{"content":"x"}}`)
	if info, err := readTranscriptInfo(p); err != nil || info != (transcriptInfo{}) {
		t.Fatalf("empty info = %+v, %v", info, err)
	}
	// Long texts are capped.
	p = write("long.jsonl", `{"type":"ai-title","aiTitle":"`+strings.Repeat("t", 500)+`"}`, user(strings.Repeat("p", 500)))
	if info, _ := readTranscriptInfo(p); len([]rune(info.Title)) != maxTitleRunes+1 || len([]rune(info.LastPrompt)) != maxPromptRunes+1 {
		t.Fatalf("caps: %d %d", len([]rune(info.Title)), len([]rune(info.LastPrompt)))
	}

	// A large file is read at its head and tail only.
	var lines []string
	lines = append(lines, user("the very first prompt"))
	filler := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + strings.Repeat("x", 1000) + `"}]}}`
	for i := 0; i < 2500; i++ {
		lines = append(lines, filler)
		if i == 1250 {
			lines = append(lines, user("lost in the middle"), `{"type":"custom-title","customTitle":"middle title"}`)
		}
	}
	lines = append(lines, `{"type":"ai-title","aiTitle":"tail title"}`, user("the last prompt"))
	p = write("big.jsonl", lines...)
	if fi, _ := os.Stat(p); fi.Size() <= 2*transcriptWindow {
		t.Fatalf("big transcript only %d bytes", fi.Size())
	}
	info, err = readTranscriptInfo(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := (transcriptInfo{Title: "tail title", FirstPrompt: "the very first prompt", LastPrompt: "the last prompt"}); info != want {
		t.Fatalf("big info = %+v, want %+v", info, want)
	}

	if _, err := readTranscriptInfo(filepath.Join(dir, "missing.jsonl")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing transcript: %v", err)
	}
}

// projectDir is Claude Code's transcript dir for dir ($HOME/.claude/projects/<key>).
func projectDir(dir string) string {
	return filepath.Join(os.Getenv("HOME"), ".claude", "projects", claudeProjectKey(dir))
}

// transcript writes a small transcript with an AI title and prompts in
// Claude's project dir for task t's dir (where `claude --resume` run there
// finds it) and returns its path.
func (e *env) transcript(t *testing.T, name, title string, prompts ...string) string {
	t.Helper()
	return e.transcriptIn(t, projectDir(e.app.Paths.TaskDir("t")), name, title, prompts...)
}

// transcriptIn is transcript, written in dir.
func (e *env) transcriptIn(t *testing.T, dir, name, title string, prompts ...string) string {
	t.Helper()
	var lines []string
	if title != "" {
		lines = append(lines, fmt.Sprintf(`{"type":"ai-title","aiTitle":%q}`, title))
	}
	for _, p := range prompts {
		b, _ := json.Marshal(p)
		lines = append(lines, `{"type":"user","message":{"role":"user","content":`+string(b)+`}}`)
	}
	path := filepath.Join(dir, name+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// pastSession inserts a session row as the hooks would have left it.
func (e *env) pastSession(t *testing.T, taskID int64, st model.SessionState, cid, transcript string, last time.Time) *model.Session {
	t.Helper()
	s := &model.Session{TaskID: taskID, State: st, ClaudeSessionID: cid, TranscriptPath: transcript,
		TmuxTarget: "@999", CreatedAt: last, LastEventAt: last}
	if err := e.app.Store.CreateSession(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	s.Name = windowName(s.ID)
	if err := e.app.Store.UpdateSession(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	return s
}

// activeWindow returns the current window id of the tmux session.
func (e *env) activeWindow(t *testing.T, session string) string {
	t.Helper()
	for _, l := range strings.Split(e.tmux(t, "list-windows", "-t", "="+session, "-F", "#{window_active} #{window_id}"), "\n") {
		if id, ok := strings.CutPrefix(l, "1 "); ok {
			return id
		}
	}
	return ""
}

func choiceIDs(cs []SessionChoice) []int64 {
	var out []int64
	for _, c := range cs {
		out = append(out, c.Session.ID)
	}
	return out
}

func TestOpenChoices(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	task := e.task(t, "t", model.StatusActive, true)
	other := e.task(t, "o", model.StatusActive, true)
	base := time.Now().Add(-time.Hour)
	proj := projectDir(e.app.Paths.TaskDir("t"))

	live, err := e.app.NewSession(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	e.argv(t, live.ID)
	if err := e.app.Store.SetTranscriptPath(ctx, live.ID, e.transcript(t, "live", "", "make it work", "and test it")); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Store.SetClaudeSession(ctx, live.ID, "cid-live", base.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// A claude atl did not launch (hook-created row, no window): not listed.
	hookRow := &model.Session{TaskID: task.ID, State: model.StateRunning, ClaudeSessionID: "cid-hook"}
	if err := e.app.Store.CreateSession(ctx, hookRow); err != nil {
		t.Fatal(err)
	}
	ended := e.pastSession(t, task.ID, model.StateEnded, "cid-e", e.transcript(t, "ended", "Refactor store", "first", "last words"), base.Add(10*time.Minute))
	stopped := e.pastSession(t, task.ID, model.StateStopped, "cid-s", "", base.Add(20*time.Minute)) // no path recorded: kept
	older := e.pastSession(t, task.ID, model.StateEnded, "cid-old", e.transcript(t, "older", ""), base)
	e.pastSession(t, task.ID, model.StateEnded, "", e.transcript(t, "noid", "x", "y"), base.Add(30*time.Minute))      // no claude id
	e.pastSession(t, task.ID, model.StateEnded, "cid-gone", filepath.Join(proj, "gone.jsonl"), base.Add(time.Minute)) // transcript gone
	e.pastSession(t, other.ID, model.StateEnded, "cid-other", "", base.Add(40*time.Minute))                           // another task

	choices, err := e.app.OpenChoices(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := choiceIDs(choices), []int64{live.ID, stopped.ID, ended.ID, older.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("choices = %v, want %v (%+v)", got, want, choices)
	}
	c := choices[0]
	if !c.Live || c.Window != e.session(t, live.ID).TmuxTarget || c.Title != "make it work" || c.LastPrompt != "and test it" {
		t.Errorf("live choice = %+v", c)
	}
	if c := choices[1]; c.Live || c.Window != "" || c.Title != windowName(stopped.ID) || c.LastPrompt != "" {
		t.Errorf("stopped choice = %+v", c)
	}
	if c := choices[2]; c.Title != "Refactor store" || c.LastPrompt != "last words" {
		t.Errorf("ended choice = %+v", c)
	}
	if c := choices[3]; c.Title != windowName(older.ID) {
		t.Errorf("older choice = %+v", c)
	}

	// Its window gone, the live session becomes stopped (Recover) and joins
	// the past ones, newest first.
	e.crash(t)
	choices, err = e.app.OpenChoices(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := choiceIDs(choices), []int64{live.ID, stopped.ID, ended.ID, older.ID}; !reflect.DeepEqual(got, want) || choices[0].Live {
		t.Fatalf("after crash choices = %v, want %v, all past (%+v)", got, want, choices)
	}

	// Closed tasks offer no past sessions.
	if err := e.app.Store.SetTaskStatus(ctx, task.ID, model.StatusDone); err != nil {
		t.Fatal(err)
	}
	if choices, err := e.app.OpenChoices(ctx, "t"); err != nil || len(choices) != 0 {
		t.Fatalf("closed task choices = %+v, %v", choices, err)
	}
}

func TestOpenOrChoose(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.task(t, "t", model.StatusActive, true)
	wantArgs := []string{"tmux", "-L", e.sock, "attach-session", "-t", "=atl-t"}

	// No sessions: a new one is started and attached to.
	cmd, choices, err := e.app.OpenOrChoose(ctx, "t")
	if err != nil || choices != nil || cmd == nil || !reflect.DeepEqual(cmd.Args, wantArgs) {
		t.Fatalf("OpenOrChoose without sessions = %v, %+v, %v", cmd, choices, err)
	}
	all, _ := e.app.Store.ListSessionsByState(ctx)
	if len(all) != 1 || all[0].State != model.StateIdle {
		t.Fatalf("sessions = %+v", all)
	}
	s := all[0]
	e.argv(t, s.ID)

	// A live session: offered, nothing launched.
	cmd, choices, err = e.app.OpenOrChoose(ctx, "t")
	if err != nil || cmd != nil || !reflect.DeepEqual(choiceIDs(choices), []int64{s.ID}) || !choices[0].Live {
		t.Fatalf("OpenOrChoose with a live session = %v, %+v, %v", cmd, choices, err)
	}
	// The user exited claude (ended, window gone): offered for resuming, not
	// silently replaced by a blank session.
	if err := e.app.Store.SetClaudeSession(ctx, s.ID, "cid-1", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Store.SetSessionState(ctx, s.ID, model.StateEnded, time.Time{}); err != nil {
		t.Fatal(err)
	}
	e.crash(t)
	cmd, choices, err = e.app.OpenOrChoose(ctx, "t")
	if err != nil || cmd != nil || !reflect.DeepEqual(choiceIDs(choices), []int64{s.ID}) || choices[0].Live {
		t.Fatalf("OpenOrChoose with an ended session = %v, %+v, %v", cmd, choices, err)
	}
	if all, _ := e.app.Store.ListSessionsByState(ctx); len(all) != 1 || e.app.Tmux.HasSession(ctx, "atl-t") {
		t.Fatalf("OpenOrChoose launched something: %+v", all)
	}
}

func TestOpenSessionResumesEnded(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	task := e.task(t, "t", model.StatusActive, true, model.TaskRepo{Name: "docs", Mode: model.ModeRO})
	wantArgs := []string{"tmux", "-L", e.sock, "attach-session", "-t", "=atl-t"}
	ended := e.pastSession(t, task.ID, model.StateEnded, "cid-e", e.transcript(t, "e", "", "hi"), time.Now().Add(-time.Hour))
	stopped := e.pastSession(t, task.ID, model.StateStopped, "cid-s", "", time.Now().Add(-2*time.Hour))

	// Another live window in the task, so selection matters.
	other, err := e.app.NewSession(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	e.argv(t, other.ID)

	cmd, err := e.app.OpenSession(ctx, "t", ended.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cmd.Args, wantArgs) {
		t.Errorf("attach cmd = %v", cmd.Args)
	}
	want := []string{"--resume", "cid-e", "--add-dir", e.app.Paths.RepoPath("docs")}
	if argv := e.argv(t, ended.ID); !reflect.DeepEqual(argv, want) {
		t.Errorf("argv = %q, want %q", argv, want)
	}
	got := e.session(t, ended.ID)
	if got.State != model.StateIdle || got.ClaudeSessionID != "cid-e" || got.TmuxTarget == "" || got.TmuxTarget == "@999" || got.Name != windowName(ended.ID) {
		t.Fatalf("resumed session = %+v", got)
	}
	tags, _ := e.app.Tmux.TaggedWindows(ctx)
	if tags[strconv.FormatInt(ended.ID, 10)] != got.TmuxTarget {
		t.Errorf("tags = %v", tags)
	}
	if w := e.activeWindow(t, "atl-t"); w != got.TmuxTarget {
		t.Errorf("active window = %s, want the resumed %s", w, got.TmuxTarget)
	}
	// Other stopped sessions are left alone.
	if st := e.session(t, stopped.ID).State; st != model.StateStopped {
		t.Errorf("other stopped session: %s", st)
	}

	// A live session: its window is selected, nothing is launched.
	o := e.session(t, other.ID)
	if _, err := e.app.OpenSession(ctx, "t", other.ID); err != nil {
		t.Fatal(err)
	}
	if w := e.activeWindow(t, "atl-t"); w != o.TmuxTarget {
		t.Errorf("active window = %s, want the live %s", w, o.TmuxTarget)
	}
	if n := len(strings.Split(e.tmux(t, "list-windows", "-t", "=atl-t"), "\n")); n != 2 {
		t.Errorf("windows = %d, want 2", n)
	}

	// OpenNew: a new session, selected.
	cmd, err = e.app.OpenNew(ctx, "t")
	if err != nil || !reflect.DeepEqual(cmd.Args, wantArgs) {
		t.Fatalf("OpenNew = %v, %v", cmd, err)
	}
	all, _ := e.app.Store.ListSessions(ctx, task.ID)
	newest := all[len(all)-1]
	e.argv(t, newest.ID)
	if newest.ID <= other.ID || newest.State != model.StateIdle || e.activeWindow(t, "atl-t") != newest.TmuxTarget {
		t.Errorf("OpenNew session %+v, active %s", newest, e.activeWindow(t, "atl-t"))
	}
	if argv := e.argv(t, newest.ID); len(argv) != 2 || argv[0] != "--add-dir" {
		t.Errorf("OpenNew argv = %q", argv)
	}
}

func TestOpenSessionRefusals(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	task := e.task(t, "t", model.StatusActive, true)
	other := e.task(t, "other", model.StatusActive, true)
	now := time.Now()
	foreign := e.pastSession(t, other.ID, model.StateEnded, "cid-x", "", now)
	noID := e.pastSession(t, task.ID, model.StateEnded, "", "", now)
	gone := e.pastSession(t, task.ID, model.StateEnded, "cid-g", filepath.Join(projectDir(e.app.Paths.TaskDir("t")), "gone.jsonl"), now)
	hookRow := &model.Session{TaskID: task.ID, State: model.StateRunning, ClaudeSessionID: "cid-h"}
	if err := e.app.Store.CreateSession(ctx, hookRow); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		id   int64
		want string
	}{
		{foreign.ID, fmt.Sprintf("session %d belongs to task other, not t", foreign.ID)},
		{9999, "task t has no session 9999"},
		{noID.ID, "has no Claude session id"},
		{gone.ID, "no longer exists"},
		{hookRow.ID, "is running but has no live atelier tmux window"},
	} {
		if _, err := e.app.OpenSession(ctx, "t", c.id); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("OpenSession(%d) = %v, want %q", c.id, err, c.want)
		}
	}
	if st := e.session(t, foreign.ID).State; st != model.StateEnded {
		t.Errorf("foreign session touched: %s", st)
	}
	if e.app.Tmux.HasSession(ctx, "atl-t") || e.app.Tmux.HasSession(ctx, "atl-other") {
		t.Error("a refused OpenSession launched a window")
	}

	// Closed task: nothing to resume, nor a new session.
	closedT := e.task(t, "c", model.StatusDone, true)
	past := e.pastSession(t, closedT.ID, model.StateEnded, "cid-c", "", now)
	if _, err := e.app.OpenSession(ctx, "c", past.ID); err == nil || !strings.Contains(err.Error(), "is done") {
		t.Errorf("OpenSession on done task = %v", err)
	}
	if st := e.session(t, past.ID).State; st != model.StateEnded {
		t.Errorf("refused resume changed state to %s", st)
	}
	if _, err := e.app.OpenNew(ctx, "c"); err == nil {
		t.Error("OpenNew on done task succeeded")
	}
}

// TestOpenSessionConcurrent races OpenSession calls for one stopped session
// against Resume (the r key): exactly one window is launched for it.
func TestOpenSessionConcurrent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	task := e.task(t, "t", model.StatusActive, true)
	for _, from := range []model.SessionState{model.StateStopped, model.StateEnded} {
		s := e.pastSession(t, task.ID, from, "cid-"+string(from), "", time.Now())
		var wg sync.WaitGroup
		errs := make(chan error, 6)
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if i%3 == 2 {
					_, err := e.app.Resume(ctx, "t")
					errs <- err
					return
				}
				_, err := e.app.OpenSession(ctx, "t", s.ID)
				errs <- err
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			// A loser that read the session before the winner's claim is told
			// so, one that read it while its window was starting too; one
			// that read it after just attaches.
			if err != nil && !strings.Contains(err.Error(), "resumed or changed meanwhile") &&
				!strings.Contains(err.Error(), "still starting") {
				t.Errorf("%s: %v", from, err)
			}
		}
		e.argv(t, s.ID)
		got := e.session(t, s.ID)
		var tagged []string
		for _, l := range strings.Split(e.tmux(t, "list-windows", "-a", "-F", "#{@atl_session}"), "\n") {
			if l == strconv.FormatInt(s.ID, 10) {
				tagged = append(tagged, l)
			}
		}
		if got.State != model.StateIdle || len(tagged) != 1 {
			t.Fatalf("%s: session %+v, %d windows tagged with it", from, got, len(tagged))
		}
	}
}

// TestOpenSessionLaunchFailure: a failed resume puts an ended session back
// to ended (and a stopped one to stopped), claude id intact.
func TestOpenSessionLaunchFailure(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	task := e.task(t, "t", model.StatusActive, true)
	ended := e.pastSession(t, task.ID, model.StateEnded, "cid-e", "", time.Now())
	stopped := e.pastSession(t, task.ID, model.StateStopped, "cid-s", "", time.Now())
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
	for _, s := range []*model.Session{ended, stopped} {
		if _, err := e.app.OpenSession(ctx, "t", s.ID); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("OpenSession with failing launch = %v", err)
		}
		if got := e.session(t, s.ID); got.State != s.State || got.ClaudeSessionID != s.ClaudeSessionID {
			t.Errorf("after failed resume: %+v, want state %s", got, s.State)
		}
	}
}

// TestOpenLiveWindowOfPastRow: a session whose row is ended or stopped while
// its claude still runs in a live tagged window (SessionEnd then SessionStart
// around /clear) counts as live: it is listed as live, and opening it (the
// picker, `atl open --session`) or Resume attaches to that window instead of
// relaunching claude and killing the running one.
func TestOpenLiveWindowOfPastRow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.task(t, "t", model.StatusActive, true)
	s, err := e.app.NewSession(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	e.argv(t, s.ID)
	if err := e.app.Store.SetClaudeSession(ctx, s.ID, "cid-1", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Store.SetTranscriptPath(ctx, s.ID, e.transcript(t, "c", "Live title", "hi")); err != nil {
		t.Fatal(err)
	}
	win := e.session(t, s.ID).TmuxTarget
	pid := e.tmux(t, "display-message", "-p", "-t", win, "#{pane_pid}")
	tag := strconv.FormatInt(s.ID, 10)
	argvFile := filepath.Join(e.out, "argv-"+tag)
	if err := os.Remove(argvFile); err != nil {
		t.Fatal(err)
	}
	// untouched checks that the original claude still runs, alone, in its
	// window and was never relaunched.
	untouched := func(what string) {
		t.Helper()
		tags, err := e.app.Tmux.TaggedWindows(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if tags[tag] != win {
			t.Errorf("%s: tagged window = %q, want the running %s", what, tags[tag], win)
		}
		if got := e.tmux(t, "display-message", "-p", "-t", win, "#{pane_pid} #{pane_dead}"); got != pid+" 0" {
			t.Errorf("%s: pane = %q, want %q", what, got, pid+" 0")
		}
		if n := len(strings.Split(e.tmux(t, "list-windows", "-t", "=atl-t"), "\n")); n != 1 {
			t.Errorf("%s: %d windows, want 1", what, n)
		}
		if _, err := os.Stat(argvFile); err == nil {
			t.Errorf("%s: claude was relaunched", what)
		}
	}

	for _, st := range []model.SessionState{model.StateEnded, model.StateStopped} {
		if err := e.app.Store.SetSessionState(ctx, s.ID, st, time.Time{}); err != nil {
			t.Fatal(err)
		}
		choices, err := e.app.OpenChoices(ctx, "t")
		if err != nil {
			t.Fatal(err)
		}
		if len(choices) != 1 || !choices[0].Live || choices[0].Window != win || choices[0].Title != "Live title" {
			t.Fatalf("%s row with a live window: choices = %+v", st, choices)
		}
		cmd, choices, err := e.app.OpenOrChoose(ctx, "t")
		if err != nil || cmd != nil || len(choices) != 1 || !choices[0].Live {
			t.Fatalf("%s: OpenOrChoose = %v, %+v, %v", st, cmd, choices, err)
		}
		if _, err := e.app.OpenSession(ctx, "t", s.ID); err != nil {
			t.Fatalf("%s: OpenSession: %v", st, err)
		}
		time.Sleep(200 * time.Millisecond) // a relaunched stub would have written its argv by now
		untouched(string(st) + " OpenSession")
		if w := e.activeWindow(t, "atl-t"); w != win {
			t.Errorf("%s: active window = %s, want %s", st, w, win)
		}
		if got := e.session(t, s.ID).State; got != st {
			t.Errorf("%s: OpenSession changed the state to %s", st, got)
		}
	}

	// Resume (the r key) claims the stopped row but adopts its live window.
	if _, err := e.app.Resume(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	untouched("Resume")
	if got := e.session(t, s.ID); got.State != model.StateIdle || got.TmuxTarget != win {
		t.Errorf("after Resume: %+v, want idle in %s", got, win)
	}
}

// TestOpenResumableOnlyFromTaskDir: past sessions are offered (and resumed
// by --session) only if `claude --resume` run in the task dir can find them.
func TestOpenResumableOnlyFromTaskDir(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	task := e.task(t, "t", model.StatusActive, true)
	now := time.Now()

	// A claude run in a task subdir (or a nested claude -p): its transcript
	// is in another project dir.
	sub := filepath.Join(e.app.Paths.TaskDir("t"), "api")
	elsewhere := e.pastSession(t, task.ID, model.StateEnded, "cid-sub", e.transcriptIn(t, projectDir(sub), "sub", "Sub title", "x"), now)
	// A hook-created row with no transcript path and no atl launch.
	hookRow := &model.Session{TaskID: task.ID, State: model.StateEnded, ClaudeSessionID: "cid-hook", CreatedAt: now, LastEventAt: now}
	if err := e.app.Store.CreateSession(ctx, hookRow); err != nil {
		t.Fatal(err)
	}
	// No transcript path, but atl launched it (named window): offered.
	launched := e.pastSession(t, task.ID, model.StateStopped, "cid-l", "", now.Add(-time.Minute))
	// A transcript in the task dir's project dir: offered.
	here := e.pastSession(t, task.ID, model.StateEnded, "cid-here", e.transcript(t, "here", "Here", "y"), now.Add(-2*time.Minute))

	choices, err := e.app.OpenChoices(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := choiceIDs(choices), []int64{launched.ID, here.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("choices = %v, want %v (%+v)", got, want, choices)
	}

	for _, c := range []struct {
		id   int64
		want string
	}{
		{elsewhere.ID, "is not in Claude's project dir for the task dir"},
		{hookRow.ID, "was not started by atl and has no recorded transcript"},
	} {
		if _, err := e.app.OpenSession(ctx, "t", c.id); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("OpenSession(%d) = %v, want %q", c.id, err, c.want)
		}
		if st := e.session(t, c.id).State; st != model.StateEnded {
			t.Errorf("refused session %d changed to %s", c.id, st)
		}
	}
	if e.app.Tmux.HasSession(ctx, "atl-t") {
		t.Fatal("a refused OpenSession launched a window")
	}

	if _, err := e.app.OpenSession(ctx, "t", launched.ID); err != nil {
		t.Fatal(err)
	}
	if argv := e.argv(t, launched.ID); len(argv) < 2 || argv[0] != "--resume" || argv[1] != "cid-l" {
		t.Errorf("argv = %q", argv)
	}
}
