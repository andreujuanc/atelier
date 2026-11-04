package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
)

func TestShow(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	e.addRepo(t, "api", "main")
	e.addRepo(t, "lib", "main")
	e.addRepo(t, "docs", "main")
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "show me",
		Repos: []RepoArg{{"api", model.ModeRW}, {"lib", model.ModeRW}, {"docs", model.ModeRO}}})
	if err != nil {
		t.Fatal(err)
	}
	api := e.app.Paths.WorktreePath(task.Slug, "api")
	commitIn(t, api, "a.txt", "a\n")
	commitIn(t, api, "b.txt", "b\n")
	if err := os.WriteFile(filepath.Join(api, "untracked"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(e.app.Paths.WorktreePath(task.Slug, "lib")); err != nil {
		t.Fatal(err)
	}
	sess := &model.Session{TaskID: task.ID, State: model.StateIdle, TranscriptPath: "/t/x.jsonl"}
	if err := e.app.Store.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.AppendNote(ctx, task.Slug, "first", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.AppendNote(ctx, task.Slug, "second", model.AuthorAssistant); err != nil {
		t.Fatal(err)
	}
	today := time.Now().Format("2006-01-02")
	wantNotes := "- " + today + " [user] first\n- " + today + " [assistant] second"

	d, err := e.app.Show(ctx, task.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if d.Task.ID != task.ID || d.TaskDir != e.app.Paths.TaskDir(task.Slug) || len(d.Repos) != 3 {
		t.Fatalf("detail = %+v", d)
	}
	ra, rl, rd := d.Repos[0], d.Repos[1], d.Repos[2]
	if ra.Name != "api" || ra.Path != api || !ra.Dirty || ra.Ahead != 2 || ra.Unpushed != 2 || ra.Pushed || ra.Err != "" {
		t.Fatalf("api = %+v", ra)
	}
	if ra.LastCommit == nil || ra.LastCommit.SHA != git(t, api, "rev-parse", "HEAD") ||
		ra.LastCommit.Subject != "add b.txt" || time.Since(ra.LastCommit.Date) > time.Hour {
		t.Fatalf("api last commit = %+v", ra.LastCommit)
	}
	if rl.Name != "lib" || rl.Err != "missing" || rl.LastCommit != nil {
		t.Fatalf("lib = %+v", rl)
	}
	if rd.Name != "docs" || rd.Mode != model.ModeRO || rd.Path != e.canonical("docs") || rd.Dirty ||
		rd.Ahead != 0 || rd.Unpushed != 0 || rd.Pushed || rd.Err != "" || rd.LastCommit == nil || rd.LastCommit.Subject != "add README" {
		t.Fatalf("docs = %+v", rd)
	}
	if len(d.Sessions) != 1 || d.Sessions[0].TranscriptPath != "/t/x.jsonl" {
		t.Fatalf("sessions = %+v", d.Sessions)
	}
	if d.Notes != wantNotes {
		t.Fatalf("notes = %q, want %q", d.Notes, wantNotes)
	}

	// Pushed: the remote-tracking ref appears after a push to the bare
	// origin (from a separate clone) and a fetch; no network on Show.
	other := filepath.Join(e.root, "other-api")
	git(t, e.root, "clone", "-q", e.origin("api"), other)
	git(t, other, "fetch", "-q", api, task.Branch+":"+task.Branch)
	git(t, other, "push", "-q", "origin", task.Branch)
	if d, _ = e.app.Show(ctx, task.Slug); d.Repos[0].Pushed {
		t.Fatal("pushed before fetch")
	}
	git(t, e.canonical("api"), "fetch", "-q", "origin")
	if d, _ = e.app.Show(ctx, task.Slug); !d.Repos[0].Pushed || d.Repos[0].Unpushed != 0 || d.Repos[0].Ahead != 2 {
		t.Fatalf("not pushed after fetch: %+v", d.Repos[0])
	}
	// Unpushed counts commits made after the push.
	commitIn(t, api, "c.txt", "c\n")
	if d, _ = e.app.Show(ctx, task.Slug); !d.Repos[0].Pushed || d.Repos[0].Unpushed != 1 || d.Repos[0].Ahead != 3 ||
		d.Repos[0].Err != "" {
		t.Fatalf("after a new commit: %+v", d.Repos[0])
	}

	// Get carries the notes text (TUI details pane).
	v, err := e.app.Get(ctx, task.Slug)
	if err != nil || v.Notes != wantNotes {
		t.Fatalf("Get notes = %+v, %v", v, err)
	}

	if _, err := e.app.Show(ctx, "nope"); err == nil {
		t.Fatal("Show of unknown task succeeded")
	}
}

func TestNotesApp(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	task := &model.Task{Slug: "closed", Title: "Closed", Status: model.StatusDone, Branch: "atl/closed"}
	if err := e.app.Store.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	today := time.Now().Format("2006-01-02")
	line, err := e.app.AppendNote(ctx, "clo", "  note on a closed task \n", "")
	if err != nil || line != "- "+today+" [user] note on a closed task" {
		t.Fatalf("AppendNote = %q, %v", line, err)
	}
	for _, c := range []struct{ body, author string }{{"   ", "user"}, {"\x1b", "user"}, {"x", "bot"}} {
		if _, err := e.app.AppendNote(ctx, "closed", c.body, c.author); err == nil {
			t.Errorf("AppendNote(%q, %q) succeeded", c.body, c.author)
		}
	}
	if _, err := e.app.AppendNote(ctx, "missing", "x", "user"); err == nil {
		t.Error("AppendNote on unknown task succeeded")
	}
	tk, text, err := e.app.Notes(ctx, "closed")
	if err != nil || tk.ID != task.ID || text != line {
		t.Fatalf("Notes = %+v %q, %v", tk, text, err)
	}
	// SetNotes is a compare-and-set; ReplaceNotes is not
	if _, err := e.app.SetNotes(ctx, "closed", "mine", "something else"); !errors.Is(err, model.ErrNotesConflict) {
		t.Fatalf("SetNotes with a stale base = %v", err)
	}
	if saved, err := e.app.SetNotes(ctx, "closed", "mine\n", line); err != nil || saved != "mine" {
		t.Fatalf("SetNotes = %q, %v", saved, err)
	}
	if saved, err := e.app.ReplaceNotes(ctx, "closed", "theirs"); err != nil || saved != "theirs" {
		t.Fatalf("ReplaceNotes = %q, %v", saved, err)
	}
	if _, err := e.app.SetNotes(ctx, "missing", "x", ""); err == nil {
		t.Error("SetNotes on unknown task succeeded")
	}
	v, err := e.app.Get(ctx, "closed")
	if err != nil || v.Notes != "theirs" {
		t.Fatalf("Get = %+v, %v", v, err)
	}
}

func TestParseTranscript(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	turns, err := ParseTranscript(f)
	if err != nil {
		t.Fatal(err)
	}
	ts := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	bash := "[tool: Bash] " + firstChars("grep -rn login src/ | head -n 50 && echo this is a very long command that goes on and on past eighty characters", 80)
	if !strings.HasSuffix(bash, "…") || len([]rune(bash)) != len("[tool: Bash] ")+81 {
		t.Fatalf("bash summary %q", bash)
	}
	shell := firstChars("go test ./... -run TestParseTranscript -count=1 && echo the shell command is long enough to be truncated at eighty", 80)
	if !strings.HasSuffix(shell, "…") {
		t.Fatalf("shell summary %q", shell)
	}
	want := []Turn{
		{"user", "Fix the login bug", ts("2026-05-01T10:00:01Z")},
		{"assistant", "Let me look at the code.\n" + bash + "\n[tool: Edit] /w/src/login.go\nFixed the nil check in login.go.", ts("2026-05-01T10:00:02Z")},
		{"user", "Thanks.\nNow add a test.\nAnd run it.", time.Time{}},
		{"assistant", "Test added and passing.\n[tool: TodoWrite]", ts("2026-05-01T10:01:04Z")},
		{"system", "[conversation compacted]", ts("2026-05-01T10:01:06.5Z")},
		{"user", "[command: /clear]\n[command]\n[shell: " + shell + "]\n[background task notification]", ts("2026-05-01T10:01:06.6Z")},
		{"assistant", "Noted.", ts("2026-05-01T10:01:06.99Z")},
		{"user", "Ship it", ts("2026-05-01T10:01:07Z")},
	}
	if len(turns) != len(want) {
		t.Fatalf("got %d turns: %+v", len(turns), turns)
	}
	for i := range want {
		if turns[i].Role != want[i].Role || turns[i].Text != want[i].Text || !turns[i].Time.Equal(want[i].Time) {
			t.Errorf("turn %d = %+v\nwant %+v", i, turns[i], want[i])
		}
	}
}

func TestParseTranscriptCapsTurns(t *testing.T) {
	long := strings.Repeat("é", maxTurnRunes+5)
	in := `{"type":"user","message":{"content":"` + long + `"}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"` + strings.Repeat("a", maxTurnRunes-2) + `"}]}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"bcd"}]}}` + "\n" +
		`{"type":"user","message":{"content":"` + strings.Repeat("x", maxTurnRunes) + `"}}` + "\n"
	turns, err := ParseTranscript(strings.NewReader(in))
	if err != nil || len(turns) != 3 {
		t.Fatalf("turns = %d, %v", len(turns), err)
	}
	if want := strings.Repeat("é", maxTurnRunes) + "… (5 more chars)"; turns[0].Text != want {
		t.Errorf("user turn = %q", turns[0].Text)
	}
	// The cap applies to the collapsed turn ("a…a" + "\n" + "bcd").
	if want := strings.Repeat("a", maxTurnRunes-2) + "\nb… (2 more chars)"; turns[1].Text != want {
		t.Errorf("assistant turn = %q", turns[1].Text)
	}
	if turns[2].Text != strings.Repeat("x", maxTurnRunes) {
		t.Error("turn of exactly the cap was truncated")
	}
}

func TestTranscript(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	task := &model.Task{Slug: "tr", Title: "Tr", Status: model.StatusActive, Branch: "atl/tr"}
	if err := e.app.Store.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.Transcript(ctx, "tr", 0, 0); err == nil || !strings.Contains(err.Error(), "no session with a recorded transcript") {
		t.Fatalf("no sessions: %v", err)
	}
	fixture, err := filepath.Abs(filepath.Join("testdata", "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	short := filepath.Join(e.root, "short.jsonl")
	if err := os.WriteFile(short, []byte(`{"type":"user","message":{"content":"hi"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	mk := func(path string, off time.Duration) *model.Session {
		s := &model.Session{TaskID: task.ID, State: model.StateIdle, TranscriptPath: path, CreatedAt: at.Add(off)}
		if err := e.app.Store.CreateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	s1 := mk(fixture, 0)
	s2 := mk(short, time.Minute)
	none := mk("", 2*time.Minute) // most recent, but no transcript: skipped by default
	gone := mk("/nonexistent/x.jsonl", -time.Minute)

	turns, err := e.app.Transcript(ctx, "tr", 0, 0)
	if err != nil || len(turns) != 1 || turns[0].Text != "hi" {
		t.Fatalf("default = %+v, %v", turns, err)
	}
	turns, err = e.app.Transcript(ctx, "tr", s1.ID, 2)
	if err != nil || len(turns) != 2 || turns[0].Role != "assistant" || turns[1].Text != "Ship it" {
		t.Fatalf("tail 2 = %+v, %v", turns, err)
	}
	if turns, err = e.app.Transcript(ctx, "tr", s1.ID, 0); err != nil || len(turns) != 8 {
		t.Fatalf("all = %d, %v", len(turns), err)
	}
	if turns, err = e.app.Transcript(ctx, "tr", s1.ID, 99); err != nil || len(turns) != 8 {
		t.Fatalf("tail 99 = %d, %v", len(turns), err)
	}
	_ = s2
	if _, err := e.app.Transcript(ctx, "tr", none.ID, 0); err == nil || !strings.Contains(err.Error(), "no recorded transcript") {
		t.Fatalf("session without path: %v", err)
	}
	if _, err := e.app.Transcript(ctx, "tr", gone.ID, 0); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := e.app.Transcript(ctx, "tr", 12345, 0); err == nil || !strings.Contains(err.Error(), "has no session 12345") {
		t.Fatalf("unknown session: %v", err)
	}
}
