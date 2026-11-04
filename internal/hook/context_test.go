package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/store"
)

// sessionStartContext checks that out is exactly one SessionStart hook
// JSON object (plus a newline) and returns its additionalContext.
func sessionStartContext(t *testing.T, out string) string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	var o struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := dec.Decode(&o); err != nil {
		t.Fatalf("stdout is not the SessionStart JSON: %v: %q", err, out)
	}
	if _, err := dec.Token(); err != io.EOF {
		t.Fatalf("stdout has more than one JSON value: %q", out)
	}
	if !strings.HasSuffix(out, "}\n") || strings.Count(out, "\n") != 1 {
		t.Fatalf("stdout is not one line: %q", out)
	}
	if o.HookSpecificOutput.HookEventName != EventSessionStart || o.HookSpecificOutput.AdditionalContext == "" {
		t.Fatalf("unexpected output %+v", o)
	}
	return o.HookSpecificOutput.AdditionalContext
}

// ctxTask adds task "ctx" (api rw from dev, docs ro, offset 300) with n
// notes, note i (1-based) created i days after 2026-04-01, alternating
// authors; it returns the task.
func ctxTask(t *testing.T, f *fixture, n int) *model.Task {
	t.Helper()
	ctx := context.Background()
	task := &model.Task{Slug: "ctx", Title: "Context\ntask", Status: model.StatusReview, Branch: "atl/ctx", PortOffset: 300,
		Repos: []model.TaskRepo{{Name: "api", Mode: model.ModeRW, BaseBranch: "dev", BaseCommit: "abc"},
			{Name: "docs", Mode: model.ModeRO}}}
	if err := f.st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.p.WorktreePath("ctx", "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		author := model.AuthorUser
		if i%2 == 0 {
			author = model.AuthorAssistant
		}
		if err := f.st.AddNote(ctx, &model.Note{TaskID: task.ID, Body: fmt.Sprintf("note %d", i), Author: author,
			CreatedAt: time.Date(2026, 4, 1+i, 9, 0, 0, 0, time.UTC)}); err != nil {
			t.Fatal(err)
		}
	}
	return task
}

func runHook(t *testing.T, event string, in Input, env map[string]string) (code int, stdout, stderr string) {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code = run([]string{event}, bytes.NewReader(b), &out, &errb, envOf(env))
	return code, out.String(), errb.String()
}

func TestSessionStartContext(t *testing.T) {
	f := setup(t)
	task := ctxTask(t, f, 7)
	dir := f.p.TaskDir("ctx")

	t.Run("new row from cwd", func(t *testing.T) {
		before := countSessions(t, f)
		start := time.Now().Add(-time.Second)
		code, out, errOut := runHook(t, EventSessionStart, Input{SessionID: "c1", Cwd: filepath.Join(dir, "api"), Source: "startup"}, nil)
		if code != 0 || errOut != "" {
			t.Fatalf("exit=%d stderr=%q", code, errOut)
		}
		if countSessions(t, f) != before+1 {
			t.Errorf("session row not created before the context was printed")
		}
		got := sessionStartContext(t, out)
		for _, want := range []string{
			"- Title: Context task\n",
			"- Slug: ctx\n",
			"- Status: review\n",
			"- Branch: atl/ctx\n",
			"- Task dir: " + dir + "\n",
			"Repos:\n- api (rw, worktree " + f.p.WorktreePath("ctx", "api") + ", branched from origin/dev)\n" +
				"- docs (ro, " + f.p.RepoPath("docs") + ", READ-ONLY: do not modify by any means)\n",
			"\nATL_PORT_OFFSET=300\n",
			"\nNotes (newest first):\n- 2026-04-08 [user] note 7\n- 2026-04-07 [assistant] note 6\n- 2026-04-06 [user] note 5\n" +
				"- 2026-04-05 [assistant] note 4\n- 2026-04-04 [user] note 3\n\n",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("context lacks %q:\n%s", want, got)
			}
		}
		if strings.Contains(got, "note 2") || strings.Contains(got, "note 1\n") {
			t.Errorf("more than 5 notes:\n%s", got)
		}
		if !strings.HasSuffix(got, "\nUse `atl note ctx \"...\"` to leave a status note for later sessions.\n") {
			t.Errorf("bad final line:\n%s", got)
		}
		const prefix = "# atelier task context (auto-generated, current as of "
		first := strings.SplitN(got, "\n", 2)[0]
		if !strings.HasPrefix(first, prefix) || !strings.HasSuffix(first, ")") {
			t.Fatalf("bad heading %q", first)
		}
		ts, err := time.Parse(time.RFC3339, strings.TrimSuffix(strings.TrimPrefix(first, prefix), ")"))
		if err != nil || ts.Before(start.Truncate(time.Second)) || ts.After(time.Now().Add(time.Second)) {
			t.Errorf("heading time %q: %v", first, err)
		}
	})

	t.Run("ATL_SESSION row on clear, resume and compact", func(t *testing.T) {
		s := &model.Session{TaskID: task.ID, ClaudeSessionID: "old", State: model.StateRunning, LastEventAt: now}
		if err := f.st.CreateSession(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		env := map[string]string{"ATL_SESSION": itoa(s.ID), "ATL_TASK": "ctx"}
		for i, src := range []string{"clear", "resume", "compact"} {
			cid := fmt.Sprintf("new-%d", i)
			code, out, errOut := runHook(t, EventSessionStart, Input{SessionID: cid, Cwd: "/elsewhere", Source: src}, env)
			if code != 0 || errOut != "" {
				t.Fatalf("%s: exit=%d stderr=%q", src, code, errOut)
			}
			if got := sessionStartContext(t, out); !strings.Contains(got, "- Slug: ctx\n") {
				t.Errorf("%s: context %q", src, got)
			}
			if g := f.get(t, s.ID); g.ClaudeSessionID != cid {
				t.Errorf("%s: claude id %q, want %q", src, g.ClaudeSessionID, cid)
			}
		}
	})

	t.Run("nested claude gets its own row and context", func(t *testing.T) {
		parent := &model.Session{TaskID: task.ID, ClaudeSessionID: "parent", State: model.StateRunning, LastEventAt: now}
		if err := f.st.CreateSession(context.Background(), parent); err != nil {
			t.Fatal(err)
		}
		env := map[string]string{"ATL_SESSION": itoa(parent.ID), "ATL_TASK": "ctx"}
		code, out, errOut := runHook(t, EventSessionStart, Input{SessionID: "nested", Cwd: "/tmp", Source: "startup"}, env)
		if code != 0 || errOut != "" {
			t.Fatalf("exit=%d stderr=%q", code, errOut)
		}
		if got := sessionStartContext(t, out); !strings.Contains(got, "- Slug: ctx\n") {
			t.Errorf("context %q", got)
		}
		if g := f.get(t, parent.ID); g.ClaudeSessionID != "parent" {
			t.Errorf("parent row touched: %+v", g)
		}
		if _, err := f.st.GetSessionByClaudeID(context.Background(), "nested"); err != nil {
			t.Errorf("nested row: %v", err)
		}
	})

	t.Run("no notes: section omitted", func(t *testing.T) {
		code, out, _ := runHook(t, EventSessionStart, Input{SessionID: "c-fix", Cwd: f.p.TaskDir("fix"), Source: "startup"}, nil)
		got := sessionStartContext(t, out)
		if code != 0 || strings.Contains(got, "Notes") || !strings.Contains(got, "- api (rw, worktree "+f.p.WorktreePath("fix", "api")+")\n") {
			t.Errorf("context:\n%s", got)
		}
	})

	t.Run("no output outside tasks", func(t *testing.T) {
		before := countSessions(t, f)
		for _, in := range []Input{
			{SessionID: "x1", Cwd: f.root, Source: "startup"},
			{SessionID: "x2", Cwd: f.p.TaskDir("missing"), Source: "startup"},
			{SessionID: "x3", Source: "startup"},
		} {
			code, out, errOut := runHook(t, EventSessionStart, in, nil)
			if code != 0 || out != "" || errOut != "" {
				t.Errorf("%+v: exit=%d stdout=%q stderr=%q", in, code, out, errOut)
			}
		}
		if countSessions(t, f) != before {
			t.Errorf("rows created outside tasks")
		}
	})

	t.Run("no output for other events", func(t *testing.T) {
		in := Input{SessionID: "c1", Cwd: dir, ToolName: "Edit", ToolInput: toolInput("file_path", filepath.Join(dir, "api", "x")),
			NotificationType: "permission_prompt"}
		for _, ev := range []string{EventUserPromptSubmit, EventNotification, EventPostToolUse, EventStop, EventSessionEnd,
			EventPreToolUse, "SubagentStop"} {
			code, out, errOut := runHook(t, ev, in, map[string]string{"ATL_TASK": "ctx"})
			if code != 0 || out != "" || errOut != "" {
				t.Errorf("%s: exit=%d stdout=%q stderr=%q", ev, code, out, errOut)
			}
		}
	})

	t.Run("no output when the DB fails", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", filepath.Join(f.root, "broken"))
		p, err := config.Resolve()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(p.DB, "x"), 0o755); err != nil { // the DB path is a directory
			t.Fatal(err)
		}
		code, out, errOut := runHook(t, EventSessionStart, Input{SessionID: "db", Cwd: dir, Source: "startup"}, nil)
		if code != 0 || out != "" || errOut != "" {
			t.Errorf("exit=%d stdout=%q stderr=%q", code, out, errOut)
		}
		if log := readLog(t, p); !strings.Contains(log, "event=SessionStart session_id=db") {
			t.Errorf("hook.log = %q", log)
		}
	})

	t.Run("context build error writes nothing", func(t *testing.T) {
		st, err := store.Open(f.p.DB)
		if err != nil {
			t.Fatal(err)
		}
		st.Close()
		var out bytes.Buffer
		if err := writeSessionContext(context.Background(), &out, st, f.p, task.ID, now); err == nil || out.Len() != 0 {
			t.Errorf("err=%v stdout=%q", err, out.String())
		}
		if err := writeSessionContext(context.Background(), &out, f.st, f.p, 9999, now); err == nil || out.Len() != 0 {
			t.Errorf("missing task: err=%v stdout=%q", err, out.String())
		}
	})
}

func TestRenderContextLimits(t *testing.T) {
	p := config.Paths{Tasks: "/t", Repos: "/r"}
	day := func(d int) time.Time { return time.Date(2026, 3, d, 0, 0, 0, 0, time.UTC) }
	base := model.Task{Slug: "s", Title: "T", Status: model.StatusActive, Branch: "atl/s",
		Repos: []model.TaskRepo{{Name: "api", Mode: model.ModeRW, BaseBranch: "main"}}}
	footer := "\nUse `atl note s \"...\"` to leave a status note for later sessions.\n"

	t.Run("sanitised one-line notes capped at 300 chars", func(t *testing.T) {
		task := base
		task.Title = "evil\x1b[2J\u202etitle\r\nnext"
		notes := []model.Note{
			{Body: "line one\nline\ttwo\x00\x07 \u200b\x1b[31mred", Author: "user", CreatedAt: day(2)},
			{Body: strings.Repeat("é", 1000), Author: "assistant", CreatedAt: day(1)},
		}
		got := renderContext(task, notes, p, now)
		if !strings.Contains(got, "- Title: evil [2J title next\n") {
			t.Errorf("title not sanitised:\n%q", got)
		}
		if !strings.Contains(got, "- 2026-03-02 [user] line one line two [31mred\n") {
			t.Errorf("note not sanitised:\n%q", got)
		}
		for _, r := range got {
			if r != '\n' && (r < 0x20 || r == 0x7f || r == '\u202e' || r == '\u200b') {
				t.Fatalf("control/format char %U in context", r)
			}
		}
		long := "- 2026-03-01 [assistant] " + strings.Repeat("é", 299) + "…\n"
		if !strings.Contains(got, long) {
			t.Errorf("long note not cut to 300 chars:\n%s", got)
		}
	})

	t.Run("notes truncated first at the cap", func(t *testing.T) {
		task := base
		for i := 0; i < 12; i++ {
			task.Repos = append(task.Repos, model.TaskRepo{Name: fmt.Sprintf("repo-with-a-long-name-%02d", i), Mode: model.ModeRO})
		}
		var notes []model.Note
		for i := 0; i < 5; i++ {
			notes = append(notes, model.Note{Body: fmt.Sprintf("N%d ", i) + strings.Repeat("x", 400), Author: "user", CreatedAt: day(20 - i)})
		}
		got := renderContext(task, notes, p, now)
		if len(got) > contextMaxLen || !utf8.ValidString(got) {
			t.Fatalf("len %d > %d", len(got), contextMaxLen)
		}
		if !strings.HasSuffix(got, footer) || !strings.Contains(got, "repo-with-a-long-name-11 (ro, /r/repo-with-a-long-name-11") ||
			!strings.Contains(got, "ATL_PORT_OFFSET=0") {
			t.Errorf("fixed part or footer lost:\n%s", got)
		}
		if !strings.Contains(got, "Notes (newest first):\n- 2026-03-20 [user] N0 ") || strings.Contains(got, "N4 ") {
			t.Errorf("notes not truncated oldest-first:\n%s", got)
		}
	})

	t.Run("huge fixed part still fits", func(t *testing.T) {
		task := base
		for i := 0; i < 80; i++ {
			task.Repos = append(task.Repos, model.TaskRepo{Name: fmt.Sprintf("repo-%02d", i), Mode: model.ModeRO})
		}
		got := renderContext(task, []model.Note{{Body: "hi", Author: "user", CreatedAt: day(1)}}, p, now)
		if len(got) > contextMaxLen || !strings.HasSuffix(got, footer) || strings.Contains(got, "Notes") {
			t.Errorf("len %d:\n%s", len(got), got)
		}
	})
}

// TestSessionStartContextLatency measures the SessionStart path that also
// prints the task context (task with notes), as a separate process.
func TestSessionStartContextLatency(t *testing.T) {
	if raceEnabled {
		t.Skip("race-instrumented binaries start too slowly to measure hook latency")
	}
	f := setup(t)
	ctxTask(t, f, 20)
	f.st.Close()
	in := `{"session_id":"c-lat","cwd":"` + f.p.TaskDir("ctx") + `","hook_event_name":"SessionStart","source":"startup"}`
	runMain(t, "SessionStart", in) // warm the page cache
	var walls []time.Duration
	for i := 0; i < 7; i++ {
		exit, out, errOut, wall := runMain(t, "SessionStart", in)
		if exit != 0 || errOut != "" || !strings.Contains(sessionStartContext(t, out), "note 20") {
			t.Fatalf("exit=%d stdout=%q stderr=%q", exit, out, errOut)
		}
		walls = append(walls, wall)
	}
	sort.Slice(walls, func(i, j int) bool { return walls[i] < walls[j] })
	median := walls[len(walls)/2]
	t.Logf("SessionStart+context hook wall time (re-exec of test binary): min %v, median %v, max %v", walls[0], median, walls[len(walls)-1])
	if median > 200*time.Millisecond {
		t.Errorf("median hook latency %v exceeds 200ms", median)
	}
	if log := readLog(t, f.p); log != "" {
		t.Errorf("unexpected errors logged: %s", log)
	}
}
