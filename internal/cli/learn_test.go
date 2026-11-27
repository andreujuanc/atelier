package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andreujuanc/atelier/internal/learn"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/store"
)

func TestLearnDryRun(t *testing.T) {
	root := setup(t)
	for _, title := range []string{"Fix", "Docs"} {
		if code, _, errs := atl(t, "new", title, "--repo", "api", "--no-fetch"); code != 0 {
			t.Fatalf("new %s: %d %s", title, code, errs)
		}
	}
	for _, args := range [][]string{{"note", "fix", "no prose"}, {"note", "fix", "ran the tests", "--author", "agent"}} {
		if code, _, errs := atl(t, args...); code != 0 {
			t.Fatalf("%v: %d %s", args, code, errs)
		}
	}
	dir := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", "learn")
	transcript, resumed := filepath.Join(dir, "s1.jsonl"), filepath.Join(dir, "s2.jsonl")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"type":"user","uuid":"u1","sessionId":"s1","origin":{"kind":"human"},"timestamp":"2026-05-01T10:00:00Z","message":{"role":"user","content":"make it <short>:\npanic: boom\nat main.go:1"}}`,
		`{"type":"assistant","uuid":"u2","sessionId":"s1","timestamp":"2026-05-01T10:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"Here is a long answer."}]}}`,
		`{"type":"user","uuid":"u3","sessionId":"s1","isMeta":true,"origin":{"kind":"peer"},"timestamp":"2026-05-01T10:00:02Z","message":{"role":"user","content":"Another Claude session sent a message:\n<agent-message from=\"a\">report</agent-message>"}}`,
		`{"type":"user","uuid":"u4","sessionId":"s1","origin":{"kind":"human"},"timestamp":"2026-05-01T10:00:03Z","message":{"role":"user","content":"no, shorter"}}`,
	}
	more := `{"type":"user","uuid":"u5","sessionId":"s2","origin":{"kind":"human"},"timestamp":"2026-05-01T11:00:00Z","message":{"role":"user","content":"thanks"}}`
	history := `{"display":"make it <short>:\n[Pasted text #1 +2 lines]","pastedContents":{"1":{"id":1,"type":"text","content":"panic: boom\nat main.go:1"}},"timestamp":1777629600000,"project":"/p","sessionId":"s1"}`
	for path, content := range map[string]string{
		transcript: strings.Join(lines, "\n"),
		resumed:    strings.Join(append(lines, more), "\n"), // a resume that copied the conversation
		filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "history.jsonl"): history,
	} {
		if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(testDB())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	fix, err := st.GetTaskBySlug(ctx, "fix")
	if err != nil {
		t.Fatal(err)
	}
	docs, err := st.GetTaskBySlug(ctx, "docs")
	if err != nil {
		t.Fatal(err)
	}
	sessions := []*model.Session{
		{TaskID: fix.ID, State: model.StateEnded, TranscriptPath: transcript},
		{TaskID: fix.ID, State: model.StateEnded},                                                          // no transcript
		{TaskID: docs.ID, State: model.StateEnded, TranscriptPath: filepath.Join(root, "elsewhere.jsonl")}, // not read
	}
	for _, s := range sessions {
		if err := st.CreateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	// The first session went on in another file (resume), and the
	// second reported the first file too: each file is read once.
	if err := st.SetTranscriptPath(ctx, sessions[0].ID, resumed); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSessionTranscript(ctx, sessions[1].ID, transcript); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTaskStatus(ctx, docs.ID, model.StatusDone); err != nil { // closed tasks count
		t.Fatal(err)
	}
	st.Close()

	if code, _, errs := atl(t, "learn", "--json"); code != 2 || !strings.Contains(errs, "--json goes with --dry-run") {
		t.Errorf("learn --json: %d %s", code, errs)
	}
	if code, _, _ := atl(t, "learn", "--dry-run", "extra"); code != 2 {
		t.Errorf("learn with an argument: %d", code)
	}
	if code, _, _ := atl(t, "learn", "--dry-run", "--task", "nope"); code != 1 {
		t.Errorf("learn --task nope: %d", code)
	}

	code, out, errs := atl(t, "learn", "--dry-run")
	if code != 0 {
		t.Fatalf("learn --dry-run: %d %s", code, errs)
	}
	if !strings.Contains(errs, "atl: warning: task docs, session ") || !strings.Contains(errs, "not read") {
		t.Errorf("no warning for the transcript outside the Claude dirs: %q", errs)
	}
	rows := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(rows) != 4 || strings.Join(strings.Fields(rows[0]), " ") != "TASK PROMPTS DROPPED NOTES BYTES" {
		t.Fatalf("output:\n%s", out)
	}
	if f := strings.Fields(rows[1]); f[0] != "fix" || f[1] != "3" || f[2] != "0" || strings.Join(f[3:7], " ") != "1 user, 1 agent" {
		t.Errorf("fix: %q", rows[1])
	}
	if f := strings.Fields(rows[2]); f[0] != "docs" || f[1] != "0" || f[2] != "0" || f[3] != "-" {
		t.Errorf("docs: %q", rows[2])
	}
	if !strings.HasPrefix(rows[3], "total (2 tasks)") || !strings.Contains(rows[3], "1 user, 1 agent") {
		t.Errorf("total: %q", rows[3])
	}

	code, out, _ = atl(t, "learn", "--dry-run", "--task", "fix", "--json")
	if code != 0 {
		t.Fatalf("--json: %d", code)
	}
	var exs []learn.Extract
	if err := json.Unmarshal([]byte(out), &exs); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(exs) != 1 || exs[0].Task.Slug != "fix" || exs[0].Task.Status != "active" || strings.Join(exs[0].Task.Repos, ",") != "api" {
		t.Fatalf("extracts: %+v", exs)
	}
	ex := exs[0]
	if len(ex.Prompts) != 3 || ex.Prompts[0].Text != "make it <short>:\n[pasted]" || ex.Prompts[1].Text != "no, shorter" || ex.Prompts[2].Text != "thanks" ||
		ex.Prompts[1].ReplyBefore != "Here is a long answer." || ex.Prompts[0].Session == 0 {
		t.Errorf("prompts: %+v", ex.Prompts)
	}
	if len(ex.Notes) != 2 || ex.Notes[0].Author != "user" || !strings.HasSuffix(ex.Notes[0].Text, "[user] no prose") ||
		ex.Notes[1].Author != "agent" || len(ex.InputHash) != 64 {
		t.Errorf("notes/hash: %+v %q", ex.Notes, ex.InputHash)
	}
	if _, out2, _ := atl(t, "learn", "--dry-run", "--task", "fix", "--json"); out2 != out {
		t.Error("a second run printed another extract")
	}
	// A rename is not new input.
	if code, _, errs := atl(t, "rename", "fix", "Fix", "it", "all"); code != 0 {
		t.Fatalf("rename: %d %s", code, errs)
	}
	var renamed []learn.Extract
	_, out2, _ := atl(t, "learn", "--dry-run", "--task", "fix", "--json")
	if err := json.Unmarshal([]byte(out2), &renamed); err != nil || renamed[0].Task.Title != "Fix it all" || renamed[0].InputHash != ex.InputHash {
		t.Errorf("after a rename: %v %+v", err, renamed)
	}

	// [learn] in atelier.toml applies.
	toml := filepath.Join(os.Getenv("ATL_WORKSPACE"), "atelier.toml")
	if err := os.WriteFile(toml, []byte("[learn]\nreply_snippet = 5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, out, _ = atl(t, "learn", "--dry-run", "--task", "fix", "--json")
	if err := json.Unmarshal([]byte(out), &exs); err != nil || exs[0].Prompts[1].ReplyBefore != "swer." {
		t.Errorf("reply_snippet = 5: %v %+v", err, exs)
	}
	if err := os.WriteFile(toml, []byte("[learn]\nreply_snippets = 5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := atl(t, "learn", "--dry-run"); code != 1 || !strings.Contains(errs, "unknown keys: learn.reply_snippets") {
		t.Errorf("unknown [learn] key: %d %s", code, errs)
	}
}

// TestLearnRun: atl learn without --dry-run runs the pre-clean of each
// task (here with a stub claude, ATL_SANDBOX=off), one row each,
// then Analyse and the status changes; a task that failed makes it exit 1.
func TestLearnRun(t *testing.T) {
	root := setup(t)
	for _, title := range []string{"Fix", "Docs"} {
		if code, _, errs := atl(t, "new", title, "--repo", "api", "--no-fetch"); code != 0 {
			t.Fatalf("new %s: %d %s", title, code, errs)
		}
	}
	if code, _, errs := atl(t, "note", "fix", "write no prose"); code != 0 {
		t.Fatalf("note: %d %s", code, errs)
	}
	bin := filepath.Join(root, "stubbin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	answer := filepath.Join(root, "answer.json")
	calls := filepath.Join(root, "calls")
	analyse := filepath.Join(root, "analyse.json")
	stub := "#!/bin/sh\ncat > /dev/null\necho call >> " + calls + "\ncase \"$*\" in *new_learnings*) cat " + analyse + "; exit 0 ;; esac\ncat " + answer + "\ngrep -q is_error..true " + answer + " && exit 1\nexit 0\n"
	if err := os.WriteFile(analyse, []byte(`{"type":"result","is_error":false,"total_cost_usd":0.01,"structured_output":{"assignments":[{"index":0,"new":"np"}],"new_learnings":[{"label":"np","text":"Write no prose"}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	good := `{"type":"result","is_error":false,"total_cost_usd":0.002,"structured_output":{"occurrences":[` +
		`{"source":{"type":"note","index":0},"quote":"write no prose","text":"No prose","strength":"strong","polarity":"reinforce"},` +
		`{"source":{"type":"note","index":0},"quote":"never add any emojis","text":"No emojis","strength":"mild","polarity":"reinforce"}]}}`
	if err := os.WriteFile(answer, []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	row := func(out, slug string) string {
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Fields(l); len(f) > 0 && f[0] == slug {
				return strings.Join(f, " ")
			}
		}
		return ""
	}

	code, out, errs := atl(t, "learn")
	if code != 0 {
		t.Fatalf("learn: %d %s", code, errs)
	}
	if r := row(out, "fix"); r != "fix ran 1 $0.0020 dropped 1 quote not found" {
		t.Errorf("fix: %q\n%s", r, out)
	}
	if r := row(out, "docs"); r != "docs empty 0 -" {
		t.Errorf("docs: %q", r)
	}
	if r := row(out, "total"); r != "total (2 tasks) 1 ran, 1 empty 1 $0.0020" ||
		!strings.HasSuffix(out, "analyse: 1 new occurrence(s): 1 assigned (1 to 1 new learning(s)); 1 call(s), $0.0100\n") {
		t.Errorf("output:\n%s", out)
	}
	if !strings.Contains(errs, "pre-clean of 1 task(s) with haiku (effort low)") || !strings.Contains(errs, "analyse of 1 new occurrence(s) with haiku (effort medium)") {
		t.Errorf("stderr: %q", errs)
	}
	if code, out, _ := atl(t, "learnings", "--all"); code != 0 || !strings.Contains(out, "candidate") || !strings.Contains(out, "Write no prose") {
		t.Errorf("learnings: %d\n%s", code, out)
	}

	code, out, _ = atl(t, "learn", "--task", "fix")
	if r := row(out, "fix"); code != 0 || r != "fix cached 1 -" || !strings.HasSuffix(out, "analyse: no new occurrences\n") {
		t.Errorf("again: %d %q\n%s", code, r, out)
	}
	if b, _ := os.ReadFile(calls); strings.Count(string(b), "call") != 2 {
		t.Errorf("calls: %q", b)
	}

	// The same preference in docs too: proposed.
	if code, _, errs := atl(t, "note", "docs", "write no prose"); code != 0 {
		t.Fatalf("note: %d %s", code, errs)
	}
	if err := os.WriteFile(analyse, []byte(`{"type":"result","is_error":false,"total_cost_usd":0.01,"structured_output":{"assignments":[{"index":0,"learning_id":1}],"new_learnings":[]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs = atl(t, "learn")
	if code != 0 || !strings.Contains(out, "analyse: 1 new occurrence(s): 1 assigned; 1 call(s), $0.0100\n") ||
		!strings.HasSuffix(out, "learnings: 1 new proposal(s): atl learnings\n") {
		t.Errorf("proposal: %d\n%s\n%s", code, out, errs)
	}

	// A failed pre-clean: the task says why, atl learn exits 1; the
	// others are analysed all the same.
	if code, _, errs := atl(t, "note", "fix", "no emojis"); code != 0 {
		t.Fatalf("note: %d %s", code, errs)
	}
	if err := os.WriteFile(answer, []byte(`{"type":"result","is_error":true,"total_cost_usd":0,"result":"Not logged in"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs = atl(t, "learn")
	if code != 1 || row(out, "fix") != "fix failed - $0.0000 claude: Not logged in" ||
		!strings.Contains(errs, "the pre-clean of 1 task(s) failed") || !strings.Contains(out, "analyse: no new occurrences") {
		t.Errorf("failed: %d\n%s\n%s", code, out, errs)
	}

	// A failed Analyse: exit 1, the pre-cleans cached.
	if code, _, errs := atl(t, "note", "docs", "never add any emojis"); code != 0 {
		t.Fatalf("note: %d %s", code, errs)
	}
	// (docs: "never add any emojis" is its notes line 1)
	if err := os.WriteFile(answer, []byte(strings.Replace(good, `"index":0},"quote":"never`, `"index":1},"quote":"never`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(analyse, []byte(`{"type":"result","is_error":false,"total_cost_usd":0.01,"structured_output":{"assignments":[{"index":0,"learning_id":7}],"new_learnings":[]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs = atl(t, "learn")
	if code != 1 || !strings.Contains(errs, "analyse failed: the answer does not hold up, nothing of it is stored: index 0: no learning 7") ||
		!strings.Contains(errs, "the pre-cleans are cached") || row(out, "docs") != "docs ran 2 $0.0020" {
		t.Errorf("failed analyse: %d\n%s\n%s", code, out, errs)
	}

	// --retry-skipped: not with --dry-run; says how many it sends again.
	if code, _, errs := atl(t, "learn", "--retry-skipped", "--dry-run"); code != 2 || !strings.Contains(errs, "--retry-skipped does not go with --dry-run") {
		t.Errorf("--retry-skipped --dry-run: %d %s", code, errs)
	}
	if _, _, errs := atl(t, "learn", "--retry-skipped"); !strings.Contains(errs, "learn: 0 skipped occurrence(s) analysed again") {
		t.Errorf("--retry-skipped: %s", errs)
	}
}
