package hook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/learn"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/sandbox"
	"github.com/andreujuanc/atelier/internal/store"
)

// TestPreferences: the snapshot is the head and one line per text, in the
// order given, whole lines within the budget, the last saying how many
// more when cut; nothing without texts or room.
func TestPreferences(t *testing.T) {
	head := prefsHead + "\n"
	if got := Preferences(nil, 800); got != "" {
		t.Errorf("no texts: %q", got)
	}
	if got := Preferences([]string{"Be brief", "Ask before pushing"}, 800); got != head+"- Be brief\n- Ask before pushing" {
		t.Errorf("all fit: %q", got)
	}
	// Sanitised as a context line: control and format characters become
	// spaces, runs of whitespace one; a text left empty is skipped.
	if got := Preferences([]string{"a\tb\x1b[31m \u202ec\u200b", " \n "}, 800); got != head+"- a b [31m c" {
		t.Errorf("sanitised: %q", got)
	}

	texts := []string{"first one", "second one", "third, a longer one", "fourth: " + strings.Repeat("x", 60), "5th"}
	full := Preferences(texts, 800)
	if got := Preferences(texts, len(full)); got != full {
		t.Errorf("exactly the budget: %q", got)
	}
	more := func(n int) string { return "- … (" + string(rune('0'+n)) + " more: atl learnings)" }
	three := head + "- first one\n- second one\n- third, a longer one\n"
	for _, c := range []struct {
		budget int
		want   string
	}{
		// Whole lines in order: the short 5th does not jump the queue.
		{len(full) - 1, three + more(2)},
		{len(three + more(2)), three + more(2)},
		// The "more" line counts: a line that fits only without it goes.
		{len(three+more(2)) - 1, head + "- first one\n- second one\n" + more(3)},
		{len(head + more(5)), head + more(5)},
		{len(head+more(5)) - 1, ""},
		{0, ""},
	} {
		got := Preferences(texts, c.budget)
		if got != c.want || len(got) > c.budget {
			t.Errorf("budget %d: %q (%d bytes), want %q", c.budget, got, len(got), c.want)
		}
	}
}

// TestPrefsBlock: the hook passes the snapshot on sanitised, within
// contextPrefsLen at a line end, whatever is stored.
func TestPrefsBlock(t *testing.T) {
	if got := prefsSection(""); got != "" {
		t.Errorf("empty: %q", got)
	}
	if got := prefsSection("\n" + prefsHead + "\n- a\x1b\u202eb\r\n- c \n\n"); got != "\n"+prefsHead+"\n- a  b\n- c\n" {
		t.Errorf("sanitised: %q", got)
	}
	line := "- " + strings.Repeat("x", 97) + "\n" // 100 bytes
	long := prefsHead + "\n" + strings.Repeat(line, contextPrefsLen/100+5)
	got := prefsBlock(long)
	if len(got) > contextPrefsLen || !strings.HasPrefix(got, prefsHead+"\n") || !strings.HasSuffix(got, strings.TrimSuffix(line, "\n")) ||
		len(got) < contextPrefsLen-len(line) {
		t.Errorf("cut to %d bytes: ...%q", len(got), got[max(0, len(got)-120):])
	}
	if got := prefsBlock(strings.Repeat("y", contextPrefsLen+1)); got != "" {
		t.Errorf("one line over the cap: %d bytes", len(got))
	}
	if contextPrefsLen != learn.MaxInjectBudget {
		t.Error("limits out of step with learn.MaxInjectBudget")
	}
}

// TestRenderStartContextPrefs: the "User preferences" section comes right
// after the task block (sandbox note and mounts line included), before
// the environment notes and the notes, and takes room only from the
// notes (which come last): what comes before them is what it is without
// it, the whole within contextMaxChars.
func TestRenderStartContextPrefs(t *testing.T) {
	p := config.Paths{Tasks: "/t", Repos: "/r"}
	task := model.Task{Slug: "s", Title: "T", Status: model.StatusActive, Branch: "atl/s", PortOffset: 300,
		Repos: []model.TaskRepo{{Name: "api", Mode: model.ModeRW, BaseBranch: "main"}}}
	now := time.Date(2026, 4, 2, 10, 0, 0, 0, time.UTC)
	snapshot := Preferences([]string{"Keep answers short", "Ask before pushing"}, 800)
	section := "\n" + snapshot + "\n"
	big := Preferences([]string{strings.Repeat("z", learn.MaxLearningText)}, learn.MaxInjectBudget)
	for i := 0; len(big)+learn.MaxLearningText+3 < learn.MaxInjectBudget; i++ {
		big += "\n- " + strings.Repeat(string(rune('a'+i%26)), learn.MaxLearningText)
	}
	notes := strings.Repeat("a long enough notes line\n", 200)
	// beforeNotes is s without its notes section.
	beforeNotes := func(s string) string {
		if i := strings.Index(s, "\nNotes:\n"); i >= 0 {
			return s[:i]
		}
		return s
	}
	for _, c := range []struct {
		name      string
		notes     string
		env       string
		sandboxed bool
		mounts    []string
		snap      string
	}{
		{"plain", "- 2026-04-01 [user] hi", "", false, nil, snapshot},
		{"long notes", notes, "", false, nil, snapshot},
		{"env and long notes", notes, strings.Repeat("e", contextEnvLen), false, nil, snapshot},
		{"sandboxed with mounts", notes, "rules", true, []string{"/home/u/.npmrc", "/etc/ca"}, snapshot},
		{"largest snapshot", notes, strings.Repeat("e", contextEnvLen), true, []string{"/x"}, big},
	} {
		without := renderSandboxContext(task, c.notes, p, c.env, c.sandboxed, c.mounts, now)
		got := renderStartContext(task, c.notes, p, c.env, c.snap, c.sandboxed, c.mounts, now)
		sec := "\n" + c.snap + "\n"
		i := strings.Index(got, sec)
		if i < 0 || beforeNotes(got[:i]+got[i+len(sec):]) != beforeNotes(without) {
			t.Errorf("%s: section missing, or the rest changed:\n%s", c.name, got)
			continue
		}
		before, after := got[:i], got[i+len(sec):]
		if !strings.Contains(before, "\nATL_PORT_OFFSET=300\n") || (c.sandboxed && !strings.HasSuffix(before, "\n/etc/ca\n") && !strings.Contains(before, sandboxNote)) {
			t.Errorf("%s: not after the task block:\n%s", c.name, got)
		}
		if strings.Contains(before, "Notes:") || strings.Contains(before, "Environment notes") ||
			(c.env != "" && !strings.HasPrefix(after, "\nEnvironment notes")) || (c.env == "" && !strings.HasPrefix(after, "\natl in this session")) {
			t.Errorf("%s: not before the environment notes, the cheat sheet and the notes:\n%s", c.name, got)
		}
		if chars(got) > contextMaxChars || chars(without) > contextMaxChars ||
			(chars(without)+chars(sec) <= contextMaxChars && got != without[:i]+sec+without[i:]) {
			t.Errorf("%s: %d chars, %d without the section", c.name, chars(got), chars(without))
		}
	}
	if got := renderStartContext(task, notes, p, "", snapshot, true, []string{"/x"}, now); !strings.Contains(got, "/x\n"+section) {
		t.Errorf("not right after the mounts line:\n%s", got)
	}
	if got := renderStartContext(task, notes, p, "", "", false, nil, now); got != renderContext(task, notes, p, "", now) {
		t.Error("an empty snapshot changed the context")
	}
	// A big task block (longest title, many repos) and long environment
	// notes: the section comes after the whole block, the notes keep their
	// end.
	many := task
	many.Title = strings.Repeat("t", model.MaxTitleLen)
	for i := 0; i < 60; i++ {
		many.Repos = append(many.Repos, model.TaskRepo{Name: fmt.Sprintf("repo-%02d", i), Mode: model.ModeRO})
	}
	env := strings.Repeat("env line\n", 110)
	without := renderSandboxContext(many, notes, p, env, false, nil, now)
	got := renderStartContext(many, notes, p, env, snapshot, false, nil, now)
	i := strings.Index(got, section)
	if i < 0 || !strings.HasSuffix(got[:i], "READ-ONLY: do not modify by any means)\n\nATL_PORT_OFFSET=300\n") ||
		!strings.HasPrefix(got[i+len(section):], "\nEnvironment notes") || beforeNotes(got[:i]+got[i+len(section):]) != beforeNotes(without) ||
		!strings.Contains(got, many.Title) || !strings.Contains(got, cheatSheet("s", false)+"\nNotes:\n"+notesMarker("s")) ||
		!strings.HasSuffix(got, "line\na long enough notes line\n") || chars(got) > contextMaxChars || chars(got) < contextMaxChars-30 {
		t.Errorf("big task block: %d chars:\n%s", chars(got), got)
	}
}

// TestStartContextPrefsError: a snapshot that cannot be read costs the
// session only the section: the task context comes, the error is logged.
func TestStartContextPrefsError(t *testing.T) {
	f := setup(t)
	accept(t, f, "Keep answers short", "Proposed")
	old := loadInjection
	t.Cleanup(func() { loadInjection = old })
	loadInjection = func(context.Context, *store.Store) (string, error) { return "", errors.New("no snapshot table") }
	code, out, errs := runHook(t, EventSessionStart, Input{SessionID: "c1", Cwd: f.p.TaskDir("fix"), Source: "startup"}, nil)
	got := sessionStartContext(t, out)
	if code != 0 || errs != "" || !strings.Contains(got, "- Slug: fix\n") || strings.Contains(got, prefsHead) {
		t.Errorf("%d %q:\n%s", code, errs, got)
	}
	if log := readLog(t, f.p); !strings.Contains(log, "event=SessionStart") || !strings.Contains(log, "user preferences: no snapshot table") {
		t.Errorf("hook.log = %q", log)
	}
}

// accept stores learnings texts in f, accepts them all but the last
// (left proposed), and returns the snapshot their acceptance rendered.
func accept(t *testing.T, f *fixture, texts ...string) string {
	t.Helper()
	ctx := context.Background()
	render := func(acc []store.Learning, _ []store.LearningOccurrence) string {
		var ts []string
		for _, l := range acc {
			ts = append(ts, l.Text)
		}
		return Preferences(ts, 800)
	}
	created, err := f.st.AssignOccurrences(ctx, texts, nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range created {
		if err := f.st.SetLearningStatus(ctx, id, []string{"candidate"}, "proposed", false, render); err != nil {
			t.Fatal(err)
		}
		if i < len(created)-1 {
			if err := f.st.SetLearningStatus(ctx, id, []string{"proposed"}, "accepted", true, render); err != nil {
				t.Fatal(err)
			}
		}
	}
	snap, err := f.st.Injection(ctx)
	if err != nil || snap == "" {
		t.Fatalf("snapshot %q %v", snap, err)
	}
	return snap
}

// TestStartContextPrefs: task sessions, next to atl and through their
// gate, and the assistant, next to atl and through its gate, get the
// accepted learnings; a proposed one is never in it.
func TestStartContextPrefs(t *testing.T) {
	f := setup(t)
	snap := accept(t, f, "Keep answers short", "Ask before pushing", "Only proposed")
	want := "\n" + prefsHead + "\n- Keep answers short\n- Ask before pushing\n"
	if strings.Contains(snap, "Only proposed") {
		t.Fatalf("a proposed learning in the snapshot: %q", snap)
	}

	code, out, errs := runHook(t, EventSessionStart, Input{SessionID: "c1", Cwd: f.p.TaskDir("fix"), Source: "startup"}, nil)
	if got := sessionStartContext(t, out); code != 0 || errs != "" || !strings.Contains(got, "ATL_PORT_OFFSET=0\n"+want+"\natl in this session") {
		t.Errorf("task session: %d %q\n%s", code, errs, got)
	}

	sock := gated(t, f)
	s := f.session(t, model.StateIdle, "")
	r := hookVia(t, f, sock, EventSessionStart, hookInput(Input{SessionID: "c2", Cwd: f.p.TaskDir("fix"), Source: "startup"}),
		map[string]string{"ATL_SESSION": itoa(s.ID)})
	if got := sessionStartContext(t, r.stdout); r.code != 0 || !strings.Contains(got, sandboxNote+"\n"+want) {
		t.Errorf("sandboxed task session: %+v\n%s", r, got)
	}

	// The assistant: the section alone.
	code, out, errs = runHook(t, EventAssistantStart, Input{SessionID: "a1", Cwd: f.p.AssistantDir(), Source: "startup"}, nil)
	if got := sessionStartContext(t, out); code != 0 || errs != "" || got != strings.TrimPrefix(want, "\n") {
		t.Errorf("assistant: %d %q %q", code, errs, got)
	}
	d, err := os.MkdirTemp("", "gate")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(d)
	asock := filepath.Join(d, "atl.sock")
	l, err := net.Listen("unix", asock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go (&sandbox.Control{Slug: "atl_assistant", Atl: "/bin/false", Task: NewAssistantServer(f.p), Workspace: f.root}).Serve(l)
	var o, e bytes.Buffer
	box := envOf(map[string]string{sandbox.InEnv: "_assistant-abc123", sandbox.AssistantEnv: "1", "ATL_CONTROL_SOCK": asock})
	if code := run([]string{EventAssistantStart}, strings.NewReader(`{"session_id":"a2"}`), &o, &e, box); code != 0 ||
		sessionStartContext(t, o.String()) != strings.TrimPrefix(want, "\n") {
		t.Errorf("assistant through its gate: %d %q %q", code, o.String(), e.String())
	}
	// A task's gate does not answer it.
	o.Reset()
	if code := run([]string{EventAssistantStart}, strings.NewReader(`{}`), &o, &e, inContainer(sock, nil)); code != 0 || o.Len() != 0 {
		t.Errorf("AssistantStart through a task's gate: %d %q", code, o.String())
	}

	// Nothing accepted: no section, and nothing at all for the assistant.
	ctx := context.Background()
	none := func([]store.Learning, []store.LearningOccurrence) string { return "" }
	if err := f.st.SetLearningStatus(ctx, 1, []string{"accepted"}, "rejected", true, none); err != nil {
		t.Fatal(err)
	}
	_, out, _ = runHook(t, EventSessionStart, Input{SessionID: "c1", Cwd: f.p.TaskDir("fix"), Source: "startup"}, nil)
	if got := sessionStartContext(t, out); strings.Contains(got, prefsHead) {
		t.Errorf("a section with nothing accepted:\n%s", got)
	}
	if code, out, errs := runHook(t, EventAssistantStart, Input{SessionID: "a1"}, nil); code != 0 || out != "" || errs != "" {
		t.Errorf("assistant with nothing accepted: %d %q %q", code, out, errs)
	}
}
