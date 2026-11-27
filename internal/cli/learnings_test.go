package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/store"
)

// TestLearnings: atl learnings lists, shows and changes the learnings;
// accept and reject only from the statuses that allow them.
func TestLearnings(t *testing.T) {
	setup(t)
	for _, title := range []string{"Fix", "Docs"} {
		if code, _, errs := atl(t, "new", title, "--repo", "api", "--no-fetch"); code != 0 {
			t.Fatalf("new %s: %d %s", title, code, errs)
		}
	}
	if code, out, _ := atl(t, "learnings"); code != 0 || !strings.Contains(out, "no proposed or accepted learnings") {
		t.Errorf("none: %d %q", code, out)
	}
	if code, out, _ := atl(t, "learnings", "ls", "--all"); code != 0 || !strings.Contains(out, "no learnings yet") {
		t.Errorf("none at all: %d %q", code, out)
	}

	st, err := store.Open(testDB())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	fix, _ := st.GetTaskBySlug(ctx, "fix")
	docs, _ := st.GetTaskBySlug(ctx, "docs")
	now := time.Now()
	occ := func(key string, task int64, minutesAgo int, quote string) store.LearningOccurrence {
		return store.LearningOccurrence{Key: key, TaskID: task, Session: 1, At: now.Add(-time.Duration(minutesAgo) * time.Minute),
			Strength: "strong", Polarity: "reinforce", Quote: quote, Text: "Be brief"}
	}
	if _, err := st.AssignOccurrences(ctx, []string{"Keep answers short", "Ask before pushing"}, []store.Assignment{
		{Occ: occ("k1", fix.ID, 3, "no, way shorter answers please"), NewIndex: 0},
		{Occ: occ("k2", docs.ID, 1, "stop writing essays here"), NewIndex: 0},
		{Occ: occ("k3", docs.ID, 2, "ask me before you push"), NewIndex: 1},
	}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetLearningStatus(ctx, 1, []string{"candidate"}, "proposed", false, func([]store.Learning, []store.LearningOccurrence) string { return "" }); err != nil {
		t.Fatal(err)
	}
	st.Close()

	code, out, errs := atl(t, "learnings")
	if code != 0 {
		t.Fatalf("ls: %d %s", code, errs)
	}
	rows := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(rows) != 2 || strings.Join(strings.Fields(rows[0]), " ") != "ID STATUS SCORE TREND TASKS LAST FLAGS TEXT" {
		t.Fatalf("ls:\n%s", out)
	}
	if f := strings.Fields(rows[1]); len(f) != 10 || f[0] != "1" || f[1] != "proposed" || f[2] != "4.0" || f[3] != "↑" || f[4] != "2" ||
		f[5] != now.Add(-time.Minute).Format("2006-01-02") || f[6] != "-" || strings.Join(f[7:], " ") != "Keep answers short" {
		t.Errorf("row: %q", rows[1])
	}
	_, out, _ = atl(t, "learnings", "ls", "--all")
	if rows := strings.Split(strings.TrimSuffix(out, "\n"), "\n"); len(rows) != 3 || !strings.HasPrefix(rows[2], "2 ") || !strings.Contains(rows[2], "candidate") {
		t.Errorf("--all:\n%s", out)
	}
	var ls []map[string]any
	_, out, _ = atl(t, "learnings", "--all", "--json")
	if err := json.Unmarshal([]byte(out), &ls); err != nil || len(ls) != 2 || ls[0]["id"] != float64(1) || ls[0]["score"] != float64(4) ||
		ls[0]["tasks"] != float64(2) || ls[1]["status"] != "candidate" || ls[0]["still_corrected"] != false {
		t.Errorf("--json: %v\n%s", err, out)
	}

	code, out, _ = atl(t, "learnings", "show", "1")
	if code != 0 || !strings.HasPrefix(out, "learning 1: Keep answers short\nstatus proposed, score 4.0 (reinforce 4.0, contradict 0.0), trend ↑ +4.0, 2 task(s), last ") {
		t.Errorf("show:\n%s", out)
	}
	// Newest first; a quote that shares no word with the text is marked.
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if n := len(lines); n < 5 || strings.Join(strings.Fields(lines[n-4]), " ") != "DATE TASK STRENGTH POLARITY ? QUOTE" ||
		strings.Join(strings.Fields(lines[n-3])[1:], " ") != "docs strong reinforce ? stop writing essays here" ||
		strings.Join(strings.Fields(lines[n-2])[1:], " ") != "fix strong reinforce no, way shorter answers please" ||
		lines[n-1] != "?: 1 quote(s) share no word with the text: read them before you accept" {
		t.Errorf("show, newest first:\n%s", out)
	}
	var shown map[string]any
	_, out, _ = atl(t, "learnings", "show", "1", "--json")
	if err := json.Unmarshal([]byte(out), &shown); err != nil || len(shown["occurrences"].([]any)) != 2 ||
		shown["occurrences"].([]any)[0].(map[string]any)["task"] != "docs" || shown["occurrences"].([]any)[0].(map[string]any)["unrelated"] != true ||
		shown["occurrences"].([]any)[1].(map[string]any)["unrelated"] != false {
		t.Errorf("show --json: %v\n%s", err, out)
	}
	for _, c := range []struct {
		args []string
		code int
		err  string
	}{
		{[]string{"learnings", "show", "9"}, 1, "learning 9: not found"},
		{[]string{"learnings", "show", "x"}, 2, `"x" is not a learning id`},
		{[]string{"learnings", "show"}, 2, ""},
		{[]string{"learnings", "accept", "0"}, 2, "not a learning id"},
		{[]string{"learnings", "accept", "9"}, 1, "learning 9: not found"},
		{[]string{"learnings", "rm", "9"}, 1, "learning 9: not found"},
		{[]string{"learnings", "extra"}, 2, ""},
		{[]string{"learnings", "accept", "2", "--for-assistant"}, 1, "not for the assistant"},
	} {
		if code, _, errs := atl(t, c.args...); code != c.code || !strings.Contains(errs, c.err) {
			t.Errorf("%v: %d %s", c.args, code, errs)
		}
	}

	// Decide: accept from proposed or candidate, reject from any but rejected.
	steps := []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"accept", "2"}, 0, "learning 2 accepted"},
		{[]string{"accept", "2"}, 1, "learning 2 is accepted: only a proposed or candidate learning can be accepted"},
		{[]string{"reject", "2"}, 0, "learning 2 rejected"},
		{[]string{"reject", "2"}, 1, "learning 2 is rejected already"},
		{[]string{"accept", "2"}, 1, "learning 2 is rejected: only"},
		{[]string{"accept", "1"}, 0, "learning 1 accepted"},
	}
	for _, s := range steps {
		code, out, errs := atl(t, append([]string{"learnings"}, s.args...)...)
		if code != s.code || !strings.Contains(out+errs, s.msg) {
			t.Errorf("%v: %d %q %q", s.args, code, out, errs)
		}
	}
	_, out, _ = atl(t, "learnings")
	if !strings.Contains(out, "accepted") || strings.Contains(out, "Ask before pushing") {
		t.Errorf("after decisions:\n%s", out)
	}

	// Edit: one line from stdin, checked.
	edit := func(stdin string, args ...string) (int, string, string) {
		var o, e bytes.Buffer
		code := runIn(context.Background(), append([]string{"learnings", "edit"}, args...), strings.NewReader(stdin), &o, &e)
		return code, o.String(), e.String()
	}
	if code, out, errs := edit("  Keep it short\n", "1"); code != 0 || out != "learning 1 edited\n" {
		t.Errorf("edit: %d %q %s", code, out, errs)
	}
	if code, _, errs := edit("ASK before pushing", "1"); code != 1 || !strings.Contains(errs, "learning 2: another learning says that") {
		t.Errorf("edit to another's text: %d %s", code, errs)
	}
	// While atl learn runs (its lock is held), no change.
	lock, err := os.OpenFile(filepath.Join(os.Getenv("ATL_WORKSPACE"), "state", "learn.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"learnings", "accept", "1"}, {"learnings", "reject", "1"}, {"learnings", "rm", "1"}} {
		if code, _, errs := atl(t, args...); code != 1 || !strings.Contains(errs, "atl learn is running in this workspace") {
			t.Errorf("%v while atl learn runs: %d %s", args, code, errs)
		}
	}
	if code, _, errs := edit("Keep it shorter", "1"); code != 1 || !strings.Contains(errs, "atl learn is running") {
		t.Errorf("edit while atl learn runs: %d %s", code, errs)
	}
	lock.Close()
	for _, bad := range []string{"", "two\nlines", strings.Repeat("x", 201), strings.Repeat("x", 1000)} {
		if code, _, errs := edit(bad, "1"); code != 1 || errs == "" {
			t.Errorf("edit %q: %d %s", bad, code, errs)
		}
	}
	var o, e bytes.Buffer
	if code := runIn(context.Background(), []string{"learnings", "edit", "9"}, failReader{t}, &o, &e); code != 1 || !strings.Contains(e.String(), "not found") {
		t.Errorf("edit of no learning: %d %s", code, e.String())
	}
	_, out, _ = atl(t, "learnings", "show", "1", "--json")
	if err := json.Unmarshal([]byte(out), &shown); err != nil || shown["text"] != "Keep it short" || shown["edited"] != true || shown["decided_at"] == nil {
		t.Errorf("after edit: %v %s", err, out)
	}

	// rm: the learning and its occurrences.
	if code, out, _ := atl(t, "learnings", "rm", "1"); code != 0 || out != "learning 1 deleted (2 occurrence(s))\n" {
		t.Errorf("rm: %d %q", code, out)
	}
	if code, _, _ := atl(t, "learnings", "show", "1"); code != 1 {
		t.Errorf("show after rm: %d", code)
	}
}
