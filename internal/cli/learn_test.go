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

	if code, _, errs := atl(t, "learn"); code != 2 || !strings.Contains(errs, "only --dry-run is implemented yet") {
		t.Errorf("learn: %d %s", code, errs)
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
