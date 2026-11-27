package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/sandbox"
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
	// (transcripts are read from a Claude config dir: the user's here)
	transcript := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", "x", "t.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("word ", 50)
	lines := []string{
		`{"type":"user","timestamp":"2026-05-01T10:00:00Z","message":{"role":"user","content":"Fix it\u001b[31m please"}}`,
		`{"type":"assistant","timestamp":"2026-05-01T10:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"` + long + `"}]}}`,
		`{"type":"user","timestamp":"2026-05-01T10:00:02Z","message":{"role":"user","content":"thanks"}}`,
	}
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(testDB())
	if err != nil {
		t.Fatal(err)
	}
	task, err := st.GetTaskBySlug(context.Background(), "fix")
	if err != nil {
		t.Fatal(err)
	}
	sess := &model.Session{TaskID: task.ID, State: model.StateIdle, ClaudeSessionID: "cid", TranscriptPath: transcript}
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
	// empty stdin with --set is an error (an empty pipe by mistake must
	// not wipe the notes), also when it is only blanks
	for _, in := range []string{"", " \n\t\r\n"} {
		if code, out, errs := set(in, "fix", "--set"); code != 1 || out != "" ||
			!strings.Contains(errs, "stdin is empty: nothing to set (use --clear to clear the notes)") || notes() != before {
			t.Fatalf("empty --set %q: %d %q %s", in, code, out, errs)
		}
	}
	// a bad ref fails before stdin is read
	if code, _, errs := set("", "nope", "--set"); code != 1 || !strings.Contains(errs, "nope") {
		t.Fatalf("bad ref: %d %s", code, errs)
	}
	var out, errb bytes.Buffer
	if code := runIn(context.Background(), []string{"notes", "nope", "--set"}, failReader{t}, &out, &errb); code != 1 {
		t.Fatalf("bad ref with stdin: %d %s", code, errb.String())
	}
	if code := runIn(context.Background(), []string{"notes", "nope", "--clear"}, failReader{t}, &out, &errb); code != 1 {
		t.Fatalf("bad ref --clear: %d %s", code, errb.String())
	}
	// --clear clears without reading stdin
	var clearOut bytes.Buffer
	if code := runIn(context.Background(), []string{"notes", "fix", "--clear"}, failReader{t}, &clearOut, &errb); code != 0 ||
		clearOut.String() != "notes cleared\n" || notes() != "" {
		t.Fatalf("--clear: %d %q %s", code, clearOut.String(), errb.String())
	}
	// --set --clear: stdin may be empty (clears) or not (replaces)
	if code, out, errs := set("kept\n", "fix", "--set", "--clear"); code != 0 || out != "notes replaced (4 bytes)\n" || notes() != "kept\n" {
		t.Fatalf("--set --clear with text: %d %q %s", code, out, errs)
	}
	if code, out, errs := set("", "fix", "--set", "--clear"); code != 0 || out != "notes cleared\n" || notes() != "" {
		t.Fatalf("--set --clear empty: %d %q %s", code, out, errs)
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
	if code, _, _ := set("", "fix", "--clear"); code != 0 {
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

// failReader fails the test when read: the command must not read stdin.
type failReader struct{ t *testing.T }

func (r failReader) Read([]byte) (int, error) {
	r.t.Helper()
	r.t.Error("stdin was read")
	return 0, io.EOF
}

// TestNotesSetDevNull checks that /dev/null as stdin gets no "reading from
// stdin" hint (it is a character device, but no terminal).
func TestNotesSetDevNull(t *testing.T) {
	setup(t)
	if code, _, errs := atl(t, "new", "Fix", "--repo", "api", "--no-fetch"); code != 0 {
		t.Fatalf("new: %d %s", code, errs)
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if isTerminal(null) {
		t.Fatal("isTerminal(/dev/null)")
	}
	var out, errb bytes.Buffer
	code := runIn(context.Background(), []string{"notes", "fix", "--set"}, null, &out, &errb)
	if code != 1 || strings.Contains(errb.String(), "ctrl-d") || !strings.Contains(errb.String(), "use --clear") {
		t.Fatalf("--set < /dev/null: %d %q", code, errb.String())
	}
}

func TestIsTerminal(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, v := range []any{f, &bytes.Buffer{}, strings.NewReader(""), nil} {
		if isTerminal(v) {
			t.Errorf("isTerminal(%T) = true", v)
		}
	}
	for _, p := range notTTYDevices {
		d, err := os.Open(p)
		if err != nil {
			continue
		}
		if isTerminal(d) {
			t.Errorf("isTerminal(%s) = true", p)
		}
		d.Close()
	}
	// a real terminal, when the test has one
	if tty, err := os.Open("/dev/tty"); err == nil {
		defer tty.Close()
		if !isTerminal(tty) {
			t.Error("isTerminal(/dev/tty) = false")
		}
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
	// Wide (CJK) characters take two columns: wrapped by display width.
	got = wrapLine("漢字 "+strings.Repeat("字", 25)+" 漢", 20)
	want = []string{"漢字", strings.Repeat("字", 10), strings.Repeat("字", 10), "字字字字字 漢"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wide wrap = %q", got)
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

// TestForAssistant: as the assistant's gate runs them (--for-assistant,
// which it always adds), atl status only moves an open task between open
// statuses: closing a task, or reopening a closed one (which removes its
// Docker objects), is refused and nothing changes, as is atl pr on a
// closed task (it would set review); atl notes --set and
// --clear first append the notes they replace, dated, to a file under the
// state dir, so the user's text is never lost.
func TestForAssistant(t *testing.T) {
	setup(t)
	for _, title := range []string{"alpha", "beta"} {
		if code, _, errs := atl(t, "new", title, "--repo", "api", "--no-fetch"); code != 0 {
			t.Fatalf("new: %d %s", code, errs)
		}
	}
	status := func(slug string) string {
		t.Helper()
		_, out, _ := atl(t, "ls", "--status", "all", "--json")
		var tasks []map[string]any
		if err := json.Unmarshal([]byte(out), &tasks); err != nil {
			t.Fatal(err)
		}
		for _, tk := range tasks {
			if tk["slug"] == slug {
				return tk["status"].(string)
			}
		}
		return ""
	}
	if code, _, errs := atl(t, "status", "--for-assistant", "alpha", "review"); code != 0 || status("alpha") != "review" {
		t.Fatalf("open to open: %d %s", code, errs)
	}
	for _, st := range []string{"done", "cancelled"} {
		if code, _, errs := atl(t, "status", "--for-assistant", "alpha", st); code != 1 || !strings.Contains(errs, "closes the task") ||
			status("alpha") != "review" {
			t.Errorf("to %s: %d %s", st, code, errs)
		}
	}
	if code, _, errs := atl(t, "status", "alpha", "done"); code != 0 {
		t.Fatalf("user closes: %d %s", code, errs)
	}
	if code, _, errs := atl(t, "status", "--for-assistant", "alpha", "active"); code != 1 || !strings.Contains(errs, "reopening it") ||
		status("alpha") != "done" {
		t.Errorf("reopen: %d %s (status %s)", code, errs, status("alpha"))
	}
	if code, _, errs := atl(t, "pr", "--for-assistant", "alpha"); code != 1 || !strings.Contains(errs, "open tasks only") {
		t.Errorf("pr on a closed task: %d %s", code, errs)
	}

	notes := func(in string, args ...string) {
		t.Helper()
		var out, errb bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if code := runIn(ctx, append([]string{"notes", "beta"}, args...), strings.NewReader(in), &out, &errb); code != 0 {
			t.Fatalf("notes %q: %d %s", args, code, errb.String())
		}
	}
	history := filepath.Join(os.Getenv("ATL_WORKSPACE"), "state", "notes-history", "beta.md")
	notes("the user's plan\n", "--set")
	if _, err := os.Stat(history); !os.IsNotExist(err) {
		t.Errorf("history kept for the user's own --set: %v", err)
	}
	notes("the assistant's text\n", "--for-assistant", "--set")
	notes("", "--clear", "--for-assistant")
	if _, out, _ := atl(t, "notes", "beta"); out != "" {
		t.Errorf("notes after --clear: %q", out)
	}
	h, err := os.ReadFile(history)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(h); strings.Count(s, "\n## ")+strings.Count(s[:3], "## ") != 2 ||
		!strings.Contains(s, "the user's plan\n") || !strings.Contains(s, "the assistant's text\n") ||
		strings.Index(s, "the user's plan") > strings.Index(s, "the assistant's text") {
		t.Errorf("history:\n%s", s)
	}
	// a note run for the assistant is the assistant's, whatever --author says
	for _, au := range []string{"user", "agent", "assistant"} {
		if code, _, errs := atl(t, "note", "--for-assistant", "beta", "by "+au, "--author", au); code != 0 {
			t.Fatalf("note --author %s: %d %s", au, code, errs)
		}
	}
	if _, out, _ := atl(t, "notes", "beta"); strings.Count(out, "[assistant] by ") != 3 || strings.Contains(out, "[user]") || strings.Contains(out, "[agent]") {
		t.Errorf("notes after the assistant's notes: %q", out)
	}
}

// TestShowSanitizesSessionFields: a session's Claude id and transcript path
// come from hook input, which a sandboxed task's container sends (through
// its gate); `atl show` must not print their control characters.
func TestShowSanitizesSessionFields(t *testing.T) {
	setup(t)
	if code, _, errs := atl(t, "new", "Fix", "--repo", "api", "--no-fetch"); code != 0 {
		t.Fatalf("new: %d %s", code, errs)
	}
	st, err := store.Open(testDB())
	if err != nil {
		t.Fatal(err)
	}
	task, err := st.GetTaskBySlug(context.Background(), "fix")
	if err != nil {
		t.Fatal(err)
	}
	sess := &model.Session{TaskID: task.ID, State: model.StateIdle, Name: "claude-1",
		ClaudeSessionID: "cid\x1b]0;pwned\x07", TranscriptPath: "/tmp/t\x1b[2J.jsonl"}
	if err := st.CreateSession(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	st.Close()
	code, out, errs := atl(t, "show", "fix")
	if code != 0 || strings.ContainsAny(out, "\x1b\x07") ||
		!strings.Contains(out, "claude id   cid]0;pwned\n") || !strings.Contains(out, "transcript  /tmp/t[2J.jsonl\n") {
		t.Fatalf("show: %d %q %s", code, out, errs)
	}
}

// TestNoteHostWriter: with the sandbox off, a task session's (or script's)
// notes are an agent's and the assistant's its own, whatever --author says,
// so neither counts as the user's words (atl learn); the user's own atl
// note, with none of their variables, is the user's.
func TestNoteHostWriter(t *testing.T) {
	setup(t)
	if code, _, errs := atl(t, "new", "Fix", "--repo", "api", "--no-fetch"); code != 0 {
		t.Fatalf("new: %d %s", code, errs)
	}
	note := func(text string, env map[string]string) {
		t.Helper()
		for _, k := range []string{"ATL_SESSION", "ATL_TASK", sandbox.AssistantEnv} {
			t.Setenv(k, env[k])
		}
		if code, _, errs := atl(t, "note", "fix", "--author", "user", text); code != 0 {
			t.Fatalf("note %q: %d %s", text, code, errs)
		}
	}
	note("from a session", map[string]string{"ATL_SESSION": "1", "ATL_TASK": "fix"})
	note("from a script", map[string]string{"ATL_TASK": "fix"})
	note("from the assistant", map[string]string{sandbox.AssistantEnv: "1"})
	note("from the user", nil)

	// atl notes --set from a session: the lines it adds are the agent's.
	_, before, _ := atl(t, "notes", "fix")
	t.Setenv("ATL_SESSION", "1")
	var out, errb bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if code := runIn(ctx, []string{"notes", "fix", "--set"}, strings.NewReader(before+"set by a session\n"), &out, &errb); code != 0 {
		t.Fatalf("notes --set: %d %s", code, errb.String())
	}
	t.Setenv("ATL_SESSION", "")

	st, err := store.Open(testDB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	task, err := st.GetTaskBySlug(context.Background(), "fix")
	if err != nil {
		t.Fatal(err)
	}
	lines, err := st.NotesBlame(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"[agent] from a session":         model.AuthorAgent,
		"[agent] from a script":          model.AuthorAgent,
		"[assistant] from the assistant": model.AuthorAssistant,
		"[user] from the user":           model.AuthorUser,
		"set by a session":               model.AuthorAgent,
	}
	seen := 0
	for _, l := range lines {
		for suffix, author := range want {
			if strings.HasSuffix(l.Text, suffix) {
				seen++
				if l.Author != author {
					t.Errorf("%q: author %s, want %s", l.Text, l.Author, author)
				}
			}
		}
	}
	if seen != len(want) {
		t.Errorf("found %d of %d lines in %+v", seen, len(want), lines)
	}
}
