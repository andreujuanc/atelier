package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

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
	if code, out, errs := atl(t, "note", "fix", "first", "note"); code != 0 || out != "note 1 added\n" {
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
	code, out, _ := atl(t, "notes", "fix")
	if code != 0 || !strings.HasPrefix(out, "[") || !strings.Contains(out, " assistant] from the assistant\n\n[") ||
		!strings.HasSuffix(out, " user] first note\n") {
		t.Fatalf("notes: %d %q", code, out)
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
	for _, s := range []string{"Task\n", "  slug         fix\n", "the description", "\nRepos\n  api (rw)  " + wt + "\n",
		"clean, 1 commit(s) ahead of base, not pushed", "work in progress", "\nSessions\n  1 claude-1  idle",
		"transcript  " + transcript, "\nNotes\n", "assistant] from the assistant"} {
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
	wantTop := []string{"branch", "created_at", "description", "id", "notes", "port_offset", "project", "repos",
		"sessions", "slug", "status", "task_dir", "title", "updated_at"}
	if got := keys(raw); !reflect.DeepEqual(got, wantTop) {
		t.Fatalf("top keys %v", got)
	}
	repo := raw["repos"].([]any)[0].(map[string]any)
	if got := keys(repo); !reflect.DeepEqual(got, []string{"ahead", "dirty", "error", "last_commit", "mode", "name", "path", "pushed", "unpushed"}) {
		t.Fatalf("repo keys %v", got)
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
		"state", "tmux_target", "transcript_path"}) || s["transcript_path"] != transcript {
		t.Fatalf("session %v", s)
	}
	notes := raw["notes"].([]any)
	n0 := notes[0].(map[string]any)
	if got := keys(n0); len(notes) != 2 || !reflect.DeepEqual(got, []string{"author", "body", "created_at", "id"}) ||
		n0["author"] != "assistant" || n0["body"] != "from the assistant" {
		t.Fatalf("notes %v", notes)
	}

	// closed tasks still take notes
	if code, _, errs := atl(t, "cancel", "fix", "--force"); code != 0 {
		t.Fatalf("cancel: %d %s", code, errs)
	}
	if code, _, errs := atl(t, "note", "fix", "post-mortem"); code != 0 {
		t.Fatalf("note on closed: %d %s", code, errs)
	}
	if code, out, _ := atl(t, "show", "fix"); code != 0 || !strings.Contains(out, "missing (worktree removed)") ||
		!strings.Contains(out, "user] post-mortem") {
		t.Fatalf("show closed: %d\n%s", code, out)
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
