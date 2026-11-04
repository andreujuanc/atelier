package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/store"
)

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestShowNotesTranscript(t *testing.T) {
	root := setup(t)
	if code, _, errs := atl(t, "new", "Fix", "--repo", "api", "--no-fetch", "-m", "the description"); code != 0 {
		t.Fatalf("new: %d %s", code, errs)
	}
	wt := filepath.Join(root, "tasks", "fix", "api")
	git(t, wt, "commit", "-q", "--allow-empty", "-m", "work in progress")

	// notes
	today := time.Now().Format("2006-01-02")
	if code, out, errs := atl(t, "notes", "fix"); code != 0 || out != "" {
		t.Fatalf("notes of a new task: %d %q %s", code, out, errs)
	}
	if code, out, errs := atl(t, "note", "fix", "first", "note"); code != 0 || out != "note added\n" {
		t.Fatalf("note: %d %q %s", code, out, errs)
	}
	if code, _, errs := atl(t, "note", "fix", "from the", "assistant", "--author", "assistant"); code != 0 {
		t.Fatalf("note assistant: %d %s", code, errs)
	}
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"note", "fix"}, 2},
		{[]string{"note", "fix", "x", "--author", "bot"}, 2},
		{[]string{"note", "fix", "  "}, 2},
		{[]string{"note", "nope", "x"}, 1},
		{[]string{"notes"}, 2},
		{[]string{"notes", "nope"}, 1},
		{[]string{"notes", "fix", "extra"}, 2},
		{[]string{"notes", "fix", "--set", "extra"}, 2},
		{[]string{"notes", "nope", "--set"}, 1},
		{[]string{"show", "fix", "--json", "--notes"}, 2},
		{[]string{"show"}, 2},
		{[]string{"show", "nope"}, 1},
		{[]string{"transcript", "fix", "--tail", "-1"}, 2},
		{[]string{"transcript", "fix"}, 1}, // no session with a transcript yet
		{[]string{"transcript", "nope"}, 1},
	} {
		if code, _, errs := atl(t, c.args...); code != c.code {
			t.Errorf("%v: exit %d, want %d (%s)", c.args, code, c.code, errs)
		}
	}
	wantNotes := "- " + today + " [user] first note\n- " + today + " [assistant] from the assistant\n"
	code, out, _ := atl(t, "notes", "fix")
	if code != 0 || out != wantNotes {
		t.Fatalf("notes: %d %q, want %q", code, out, wantNotes)
	}
	// the read-only spelling the assistant may run without asking
	if code, out, _ := atl(t, "show", "fix", "--notes"); code != 0 || out != wantNotes {
		t.Fatalf("show --notes: %d %q", code, out)
	}

	// a session with a transcript
	transcript := filepath.Join(root, "t.jsonl")
	long := strings.Repeat("word ", 50)
	lines := []string{
		`{"type":"user","timestamp":"2026-05-01T10:00:00Z","message":{"role":"user","content":"Fix it\u001b[31m please"}}`,
		`{"type":"assistant","timestamp":"2026-05-01T10:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"` + long + `"}]}}`,
		`{"type":"user","timestamp":"2026-05-01T10:00:02Z","message":{"role":"user","content":"thanks"}}`,
	}
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(os.Getenv("XDG_DATA_HOME"), "atelier", "atelier.db"))
	if err != nil {
		t.Fatal(err)
	}
	task, err := st.GetTaskBySlug(context.Background(), "fix")
	if err != nil {
		t.Fatal(err)
	}
	sess := &model.Session{TaskID: task.ID, State: model.StateIdle, Name: "claude-1", ClaudeSessionID: "cid", TranscriptPath: transcript}
	if err := st.CreateSession(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	st.Close()

	code, out, errs := atl(t, "transcript", "fix")
	if code != 0 {
		t.Fatalf("transcript: %d %s", code, errs)
	}
	blocks := strings.Split(strings.TrimSuffix(out, "\n"), "\n\n")
	if len(blocks) != 3 || !strings.HasPrefix(blocks[0], "[user 2026-") || !strings.HasSuffix(blocks[0], "] Fix it[31m please") ||
		!strings.HasPrefix(blocks[1], "[assistant ") || !strings.HasSuffix(blocks[2], "] thanks") {
		t.Fatalf("transcript:\n%s", out)
	}
	if strings.Contains(out, "\x1b") {
		t.Fatal("escape sequence printed")
	}
	al := strings.Split(blocks[1], "\n")
	if len(al) < 3 {
		t.Fatalf("assistant block not wrapped:\n%s", blocks[1])
	}
	for _, l := range strings.Split(out, "\n") {
		if len([]rune(l)) > wrapWidth {
			t.Fatalf("line longer than %d: %q", wrapWidth, l)
		}
	}
	if code, out, _ := atl(t, "transcript", "fix", "--tail", "1", "--session", "1"); code != 0 || !strings.HasPrefix(out, "[user ") || !strings.HasSuffix(out, "] thanks\n") {
		t.Fatalf("tail 1: %d %q", code, out)
	}
	if code, _, errs := atl(t, "transcript", "fix", "--session", "99"); code != 1 || !strings.Contains(errs, "no session 99") {
		t.Fatalf("unknown session: %d %s", code, errs)
	}

	// show (text)
	code, out, errs = atl(t, "show", "fix")
	if code != 0 {
		t.Fatalf("show: %d %s", code, errs)
	}
	for _, s := range []string{"Task\n", "  slug         fix\n", "  base         auto\n", "the description",
		"\nRepos\n  api (rw)  " + wt + "\n    base origin/main\n",
		"clean, 1 commit(s) ahead of base, not pushed", "work in progress", "\nSessions\n  1 claude-1  idle",
		"transcript  " + transcript, "    title       Fix it [31m please\n",
		"\nNotes\n  - " + today + " [user] first note\n  - " + today + " [assistant] from the assistant\n"} {
		if !strings.Contains(out, s) {
			t.Errorf("show lacks %q:\n%s", s, out)
		}
	}

	// show --json: stable shape
	code, out, errs = atl(t, "show", "fix", "--json")
	if code != 0 {
		t.Fatalf("show --json: %d %s", code, errs)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatal(err)
	}
	wantTop := []string{"base", "branch", "created_at", "description", "id", "notes", "port_offset", "project", "repos",
		"sessions", "slug", "status", "task_dir", "title", "updated_at"}
	if got := keys(raw); !reflect.DeepEqual(got, wantTop) {
		t.Fatalf("top keys %v", got)
	}
	repo := raw["repos"].([]any)[0].(map[string]any)
	if got := keys(repo); !reflect.DeepEqual(got, []string{"ahead", "base_branch", "dirty", "error", "last_commit", "mode", "name", "path", "pushed", "unpushed"}) {
		t.Fatalf("repo keys %v", got)
	}
	if raw["base"] != "" || repo["base_branch"] != "main" {
		t.Fatalf("base %v base_branch %v", raw["base"], repo["base_branch"])
	}
	if repo["ahead"].(float64) != 1 || repo["pushed"] != false || repo["unpushed"].(float64) != 1 || repo["dirty"] != false || repo["path"] != wt {
		t.Fatalf("repo %v", repo)
	}
	lc := repo["last_commit"].(map[string]any)
	if got := keys(lc); !reflect.DeepEqual(got, []string{"date", "sha", "subject"}) || lc["subject"] != "work in progress" {
		t.Fatalf("last_commit %v", lc)
	}
	s := raw["sessions"].([]any)[0].(map[string]any)
	if got := keys(s); !reflect.DeepEqual(got, []string{"claude_session_id", "created_at", "id", "last_event_at", "name",
		"state", "title", "tmux_target", "transcript_path"}) || s["transcript_path"] != transcript || s["title"] != "Fix it [31m please" {
		t.Fatalf("session %v", s)
	}
	if notes, ok := raw["notes"].(string); !ok || notes+"\n" != wantNotes {
		t.Fatalf("notes %#v", raw["notes"])
	}

	// closed tasks still take notes
	if code, _, errs := atl(t, "cancel", "fix", "--force"); code != 0 {
		t.Fatalf("cancel: %d %s", code, errs)
	}
	if code, _, errs := atl(t, "note", "fix", "post-mortem"); code != 0 {
		t.Fatalf("note on closed: %d %s", code, errs)
	}
	if code, out, _ := atl(t, "show", "fix"); code != 0 || !strings.Contains(out, "missing (worktree removed)") ||
		!strings.Contains(out, "  - "+today+" [user] post-mortem\n") {
		t.Fatalf("show closed: %d\n%s", code, out)
	}
}

func TestNotesSet(t *testing.T) {
	setup(t)
	if code, _, errs := atl(t, "new", "Fix", "--repo", "api", "--no-fetch"); code != 0 {
		t.Fatalf("new: %d %s", code, errs)
	}
	set := func(in string, args ...string) (int, string, string) {
		t.Helper()
		var out, errb bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		code := runIn(ctx, append([]string{"notes"}, args...), strings.NewReader(in), &out, &errb)
		return code, out.String(), errb.String()
	}
	notes := func() string {
		t.Helper()
		code, out, errs := atl(t, "notes", "fix")
		if code != 0 {
			t.Fatalf("notes: %d %s", code, errs)
		}
		return out
	}
	// --set replaces with stdin: newlines, tabs and indentation kept, CRLF
	// and control characters cleaned, trailing blank lines dropped
	text := "Goal: fix login\r\n\n  - step\tone\x1b[31m\n  - step two\n\n\n"
	if code, out, errs := set(text, "fix", "--set"); code != 0 || out != "notes replaced (46 bytes)\n" {
		t.Fatalf("--set: %d %q %s", code, out, errs)
	}
	if got := notes(); got != "Goal: fix login\n\n  - step\tone[31m\n  - step two\n" {
		t.Fatalf("after --set: %q", got)
	}
	// note appends a line after it
	if code, _, errs := atl(t, "note", "fix", "--author", "assistant", "agent", "line"); code != 0 {
		t.Fatalf("note: %d %s", code, errs)
	}
	if got := notes(); !strings.HasSuffix(got, "  - step two\n- "+time.Now().Format("2006-01-02")+" [assistant] agent line\n") {
		t.Fatalf("after note: %q", got)
	}
	// --set round-trips what atl notes prints
	before := notes()
	if code, _, errs := set(before, "fix", "--set"); code != 0 || notes() != before {
		t.Fatalf("round trip: %d %s %q", code, errs, notes())
	}
	// plain atl notes never reads stdin or clears
	if code, out, _ := set("", "fix"); code != 0 || out != before {
		t.Fatalf("notes without --set: %d %q", code, out)
	}
	// too large: refused, nothing changes
	if code, _, errs := set(strings.Repeat("x", model.MaxNotesLen+1), "fix", "--set"); code != 1 ||
		!strings.Contains(errs, "notes too large") || !strings.Contains(errs, "64 KiB") || notes() != before {
		t.Fatalf("too large: %d %s", code, errs)
	}
	if code, _, errs := set(strings.Repeat("x", 4*model.MaxNotesLen+10), "fix", "--set"); code != 1 || !strings.Contains(errs, "notes too large") {
		t.Fatalf("huge stdin: %d %s", code, errs)
	}
	// empty stdin with --set clears
	if code, out, errs := set("", "fix", "--set"); code != 0 || out != "notes cleared\n" || notes() != "" {
		t.Fatalf("clear: %d %q %s", code, out, errs)
	}
	// closed tasks too
	if code, _, errs := atl(t, "done", "fix", "--force"); code != 0 {
		t.Fatalf("done: %d %s", code, errs)
	}
	if code, _, errs := set("after done\n", "fix", "--set"); code != 0 || notes() != "after done\n" {
		t.Fatalf("--set on a closed task: %d %s %q", code, errs, notes())
	}
	if code, out, _ := atl(t, "show", "fix", "--json"); code != 0 || !strings.Contains(out, `"notes": "after done"`) {
		t.Fatalf("show --json: %d %s", code, out)
	}
	if code, _, _ := set("", "fix", "--set"); code != 0 {
		t.Fatal("clear")
	}
	// empty notes are "" in JSON, never null
	if code, out, _ := atl(t, "show", "fix", "--json"); code != 0 || !strings.Contains(out, `"notes": ""`) {
		t.Fatalf("show --json: %d %s", code, out)
	}
	if code, out, _ := atl(t, "show", "fix"); code != 0 || !strings.HasSuffix(out, "\nNotes\n  (none)\n") {
		t.Fatalf("show: %d %s", code, out)
	}
}

func TestWriteNotes(t *testing.T) {
	var b bytes.Buffer
	writeNotes(&b, "  ", "Goal\n\n  - "+strings.Repeat("word ", 30)+"\n\tx\x1b")
	want := "  Goal\n\n    - " + strings.TrimSpace(strings.Repeat("word ", 18)) + "\n      " +
		strings.TrimSpace(strings.Repeat("word ", 12)) + "\n   x\n"
	if b.String() != want {
		t.Fatalf("got\n%q\nwant\n%q", b.String(), want)
	}
}

func TestWrapLine(t *testing.T) {
	if got := wrapLine("", 30); !reflect.DeepEqual(got, []string{""}) {
		t.Fatalf("empty: %q", got)
	}
	got := wrapLine("aaa bbb "+strings.Repeat("x", 45)+" ccc", 20)
	want := []string{"aaa bbb", strings.Repeat("x", 20), strings.Repeat("x", 20), "xxxxx ccc"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wrap = %q", got)
	}
}

func TestPushedText(t *testing.T) {
	for _, c := range []struct {
		pushed   bool
		unpushed int
		want     string
	}{
		{false, 3, "not pushed"},
		{true, 0, "pushed (origin/atl/x)"},
		{true, 2, "pushed (origin/atl/x, 2 unpushed)"},
	} {
		if got := pushedText(c.pushed, c.unpushed, "atl/x"); got != c.want {
			t.Errorf("pushedText(%v, %d) = %q, want %q", c.pushed, c.unpushed, got, c.want)
		}
	}
}
