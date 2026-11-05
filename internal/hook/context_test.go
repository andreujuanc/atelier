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

// ctxTask adds task "ctx" (api rw from dev, docs ro, offset 300) whose
// notes text has n lines, line i (1-based) dated i days after 2026-04-01,
// alternating authors; it returns the task.
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
	if _, err := f.st.SetNotes(ctx, task.ID, noteLines(1, n), ""); err != nil {
		t.Fatal(err)
	}
	return task
}

// noteLines returns notes lines from..to (see ctxTask), newline-separated.
func noteLines(from, to int) string {
	var lines []string
	for i := from; i <= to; i++ {
		author := model.AuthorUser
		if i%2 == 0 {
			author = model.AuthorAssistant
		}
		lines = append(lines, model.NoteLine(time.Date(2026, 4, 1+i, 12, 0, 0, 0, time.Local), author, fmt.Sprintf("note %d", i)))
	}
	return strings.Join(lines, "\n")
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
			"\nNotes:\n- 2026-04-02 [user] note 1\n- 2026-04-03 [assistant] note 2\n- 2026-04-04 [user] note 3\n" +
				"- 2026-04-05 [assistant] note 4\n- 2026-04-06 [user] note 5\n- 2026-04-07 [assistant] note 6\n" +
				"- 2026-04-08 [user] note 7\n\n",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("context lacks %q:\n%s", want, got)
			}
		}
		if strings.Contains(got, "truncated") {
			t.Errorf("short notes truncated:\n%s", got)
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
		// Ended: a real /clear or /resume fires SessionEnd first.
		s := &model.Session{TaskID: task.ID, ClaudeSessionID: "old", State: model.StateEnded, LastEventAt: now}
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

	t.Run("ATL_CONTEXT appended from the hook env", func(t *testing.T) {
		in := Input{SessionID: "c-env", Cwd: dir, Source: "startup"}
		_, plain, _ := runHook(t, EventSessionStart, in, nil)
		code, out, errOut := runHook(t, EventSessionStart, in, map[string]string{"ATL_CONTEXT": "  No runtimes here.\r\nRun tests via docker.\n"})
		if code != 0 || errOut != "" {
			t.Fatalf("exit=%d stderr=%q", code, errOut)
		}
		got := sessionStartContext(t, out)
		want := "\nATL_PORT_OFFSET=300\n\nEnvironment notes (ATL_CONTEXT):\nNo runtimes here.\nRun tests via docker.\n\nNotes:\n"
		if !strings.Contains(got, want) {
			t.Errorf("context lacks %q:\n%s", want, got)
		}
		_, blank, _ := runHook(t, EventSessionStart, in, map[string]string{"ATL_CONTEXT": " \t\n "})
		body := func(s string) string { return strings.SplitN(sessionStartContext(t, s), "\n", 2)[1] } // drop the timestamped heading
		if body(blank) != body(plain) || strings.Contains(body(plain), "Environment notes") {
			t.Errorf("blank ATL_CONTEXT changed the context:\n%s\nvs\n%s", body(blank), body(plain))
		}
		code, out, errOut = runHook(t, EventSessionStart, Input{SessionID: "x-env", Cwd: f.root, Source: "startup"},
			map[string]string{"ATL_CONTEXT": "hello"})
		if code != 0 || out != "" || errOut != "" {
			t.Errorf("outside a task: exit=%d stdout=%q stderr=%q", code, out, errOut)
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
		if err := writeSessionContext(context.Background(), &out, st, f.p, task.ID, "", now); err == nil || out.Len() != 0 {
			t.Errorf("err=%v stdout=%q", err, out.String())
		}
		if err := writeSessionContext(context.Background(), &out, f.st, f.p, 9999, "", now); err == nil || out.Len() != 0 {
			t.Errorf("missing task: err=%v stdout=%q", err, out.String())
		}
	})
}

func TestRenderContextLimits(t *testing.T) {
	p := config.Paths{Tasks: "/t", Repos: "/r"}
	base := model.Task{Slug: "s", Title: "T", Status: model.StatusActive, Branch: "atl/s",
		Repos: []model.TaskRepo{{Name: "api", Mode: model.ModeRW, BaseBranch: "main"}}}
	footer := "\nUse `atl note s \"...\"` to leave a status note for later sessions.\n"
	const marker = "… (earlier notes truncated; `atl notes s` for all)\n"
	// notesOf returns the notes section of a context ("" if none).
	notesOf := func(t *testing.T, got string) string {
		t.Helper()
		i := strings.Index(got, "\nNotes:\n")
		if i < 0 {
			return ""
		}
		rest := got[i+len("\nNotes:\n"):]
		if !strings.HasSuffix(rest, "\n"+footer) {
			t.Fatalf("notes section not followed by the footer:\n%q", rest)
		}
		return strings.TrimSuffix(rest, "\n"+footer)
	}
	manyRepos := func(n int) model.Task {
		task := base
		for i := 0; i < n; i++ {
			task.Repos = append(task.Repos, model.TaskRepo{Name: fmt.Sprintf("repo-with-a-long-name-%02d", i), Mode: model.ModeRO})
		}
		return task
	}

	t.Run("sanitised, newlines and indentation kept", func(t *testing.T) {
		task := base
		task.Title = "evil\x1b[2J\u202etitle\r\nnext"
		notes := "\n\nGoal: fix it\r\n  - sub\titem\x00\x07 \u200b\x1b[31mred\rlast  \n\n"
		got := renderContext(task, notes, p, "", now)
		if !strings.Contains(got, "- Title: evil [2J title next\n") {
			t.Errorf("title not sanitised:\n%q", got)
		}
		if n := notesOf(t, got); n != "Goal: fix it\n  - sub item     [31mred\nlast" {
			t.Errorf("notes section %q", n)
		}
		for _, r := range got {
			if r != '\n' && (r < 0x20 || r == 0x7f || r == '\u202e' || r == '\u200b') {
				t.Fatalf("control/format char %U in context", r)
			}
		}
	})

	t.Run("blank notes: no section", func(t *testing.T) {
		for _, n := range []string{"", " \n\t\n", "\x1b"} {
			if got := renderContext(base, n, p, "", now); strings.Contains(got, "Notes") || !strings.HasSuffix(got, footer) {
				t.Errorf("%q:\n%s", n, got)
			}
		}
	})

	t.Run("fits exactly, one more byte truncates", func(t *testing.T) {
		room := contextMaxLen - len(renderContext(base, "", p, "", now)) - len("\nNotes:\n") - 1
		exact := strings.Repeat("a", room)
		got := renderContext(base, exact, p, "", now)
		if len(got) != contextMaxLen || notesOf(t, got) != exact {
			t.Fatalf("exact fit: len %d", len(got))
		}
		got = renderContext(base, "b\n"+exact, p, "", now)
		if len(got) > contextMaxLen || notesOf(t, got) != marker+exact[len(marker):] {
			t.Errorf("one line over: len %d:\n%s", len(got), notesOf(t, got))
		}
	})

	t.Run("keeps the end, cut at a line start", func(t *testing.T) {
		task := manyRepos(12)
		var lines []string
		for i := 0; i < 60; i++ {
			lines = append(lines, fmt.Sprintf("- 2026-03-%02d [user] N%02d %s", i%28+1, i, strings.Repeat("x", 40)))
		}
		got := renderContext(task, strings.Join(lines, "\n"), p, "", now)
		if len(got) > contextMaxLen || !utf8.ValidString(got) {
			t.Fatalf("len %d > %d", len(got), contextMaxLen)
		}
		if !strings.HasSuffix(got, footer) || !strings.Contains(got, "repo-with-a-long-name-11 (ro, /r/repo-with-a-long-name-11") ||
			!strings.Contains(got, "ATL_PORT_OFFSET=0") {
			t.Errorf("fixed part or footer lost:\n%s", got)
		}
		n := notesOf(t, got)
		if !strings.HasPrefix(n, marker+"- 2026-03-") || !strings.HasSuffix(n, "N59 "+strings.Repeat("x", 40)) || strings.Contains(n, "N00 ") {
			t.Errorf("not the end of the notes from a line start:\n%s", n)
		}
		// every kept line is whole, and nearly all the room is used
		for _, l := range strings.Split(strings.TrimPrefix(n, marker), "\n") {
			if len(l) != len(lines[0]) {
				t.Errorf("partial line %q", l)
			}
		}
		if len(got) < contextMaxLen-len(lines[0])-1 {
			t.Errorf("too much room left: len %d", len(got))
		}
	})

	t.Run("one huge line: cut on a rune boundary", func(t *testing.T) {
		notes := "first line\n" + strings.Repeat("é", 3000) + "Z"
		got := renderContext(base, notes, p, "", now)
		n := notesOf(t, got)
		if len(got) > contextMaxLen || !utf8.ValidString(got) || !strings.HasPrefix(n, marker+"é") || !strings.HasSuffix(n, "éZ") ||
			strings.Contains(n, "first line") {
			t.Errorf("len %d:\n%s", len(got), n)
		}
		if len(got) < contextMaxLen-1 {
			t.Errorf("rune cut too short: len %d", len(got))
		}
	})

	t.Run("huge fixed part still fits", func(t *testing.T) {
		task := base
		for i := 0; i < 80; i++ {
			task.Repos = append(task.Repos, model.TaskRepo{Name: fmt.Sprintf("repo-%02d", i), Mode: model.ModeRO})
		}
		got := renderContext(task, "- 2026-03-01 [user] hi", p, "", now)
		if len(got) > contextMaxLen || !strings.HasSuffix(got, footer) || strings.Contains(got, "Notes") {
			t.Errorf("len %d:\n%s", len(got), got)
		}
	})

	t.Run("too little room for notes: section dropped", func(t *testing.T) {
		task := manyRepos(0)
		for len(renderContext(task, "", p, "", now)) < contextMaxLen-len("\nNotes:\n")-len(marker)-contextMinNotes+5 {
			task.Repos = append(task.Repos, model.TaskRepo{Name: fmt.Sprintf("r%03d", len(task.Repos)), Mode: model.ModeRO})
		}
		got := renderContext(task, strings.Repeat("long notes line\n", 50), p, "", now)
		if len(got) > contextMaxLen || !strings.HasSuffix(got, footer) || strings.Contains(got, "Notes") {
			t.Errorf("len %d:\n%s", len(got), got)
		}
	})
}

func TestRenderContextEnv(t *testing.T) {
	p := config.Paths{Tasks: "/t", Repos: "/r"}
	base := model.Task{Slug: "s", Title: "T", Status: model.StatusActive, Branch: "atl/s", PortOffset: 100,
		Repos: []model.TaskRepo{{Name: "api", Mode: model.ModeRW, BaseBranch: "main"}}}
	notes := "- 2026-03-01 [user] hi"
	footer := "\nUse `atl note s \"...\"` to leave a status note for later sessions.\n"
	const head = "\nEnvironment notes (ATL_CONTEXT):\n"
	section := func(t *testing.T, got string) string {
		t.Helper()
		i := strings.Index(got, head)
		if i < 0 {
			t.Fatalf("no ATL_CONTEXT section:\n%s", got)
		}
		rest := got[i+len(head):]
		j := strings.Index(rest, "\n\n")
		if j < 0 {
			t.Fatalf("section not terminated:\n%q", rest)
		}
		return rest[:j]
	}

	t.Run("placed after ATL_PORT_OFFSET, before notes", func(t *testing.T) {
		got := renderContext(base, notes, p, "Use docker for tests.", now)
		want := "\nATL_PORT_OFFSET=100\n" + head + "Use docker for tests.\n\nNotes:\n- 2026-03-01 [user] hi\n" + footer
		if !strings.HasSuffix(got, want) {
			t.Errorf("got:\n%s", got)
		}
		got = renderContext(base, "", p, "x", now)
		if !strings.HasSuffix(got, "\nATL_PORT_OFFSET=100\n"+head+"x\n"+footer) {
			t.Errorf("no notes:\n%s", got)
		}
	})

	t.Run("unset or blank: unchanged", func(t *testing.T) {
		plain := renderContext(base, notes, p, "", now)
		if strings.Contains(plain, "Environment notes") {
			t.Fatalf("section without ATL_CONTEXT:\n%s", plain)
		}
		for _, v := range []string{" ", "\t\n\r\n  ", "\x1b\u202e\u200b"} {
			if got := renderContext(base, notes, p, v, now); got != plain {
				t.Errorf("%q changed the output:\n%s", v, got)
			}
		}
	})

	t.Run("sanitised, newlines kept", func(t *testing.T) {
		got := renderContext(base, notes, p, "\n  line one\r\nline\ttwo\x1b[31m\rthree \u202eevil\u200b\x00x\xff \n\n", now)
		if s := section(t, got); s != "line one\nline two [31m\nthree  evil  x" {
			t.Errorf("section %q", s)
		}
		for _, r := range got {
			if r != '\n' && (r < 0x20 || r == 0x7f || r == '\u202e' || r == '\u200b' || r == utf8.RuneError) {
				t.Fatalf("control/format char %U in context", r)
			}
		}
	})

	t.Run("capped at 1000 bytes", func(t *testing.T) {
		exact := strings.Repeat("a", contextEnvLen)
		if s := section(t, renderContext(base, "", p, exact, now)); s != exact {
			t.Errorf("%d-byte text cut to %d", len(exact), len(s))
		}
		got := renderContext(base, "", p, "b"+strings.Repeat("é", 1000), now)
		s := section(t, got)
		if len(s) > contextEnvLen || !utf8.ValidString(s) || !strings.HasSuffix(s, "é…") || !strings.HasPrefix(s, "bé") {
			t.Errorf("len %d, valid %v, tail %q", len(s), utf8.ValidString(s), s[len(s)-8:])
		}
		if len(s) < contextEnvLen-len("é") {
			t.Errorf("cut too short: %d", len(s))
		}
	})

	t.Run("overall cap: notes dropped first", func(t *testing.T) {
		task := base
		for i := 0; i < 8; i++ {
			task.Repos = append(task.Repos, model.TaskRepo{Name: fmt.Sprintf("repo-with-a-long-name-%02d", i), Mode: model.ModeRO})
		}
		var lines []string
		for i := 0; i < 5; i++ {
			lines = append(lines, fmt.Sprintf("- 2026-03-%02d [user] N%d ", 16+i, i)+strings.Repeat("x", 400))
		}
		many := strings.Join(lines, "\n")
		env := strings.Repeat("env text ", 200)
		got := renderContext(task, many, p, env, now)
		if len(got) > contextMaxLen || !utf8.ValidString(got) || !strings.HasSuffix(got, footer) {
			t.Fatalf("len %d > %d or footer lost:\n%s", len(got), contextMaxLen, got)
		}
		if s := section(t, got); len(s) > contextEnvLen || !strings.HasSuffix(s, "…") || !strings.HasPrefix(s, "env text env") {
			t.Errorf("section cut wrongly (len %d): %q", len(s), s)
		}
		// the little room left keeps the end of the newest line, after the marker
		if strings.Contains(got, "N0 ") || !strings.Contains(got, "\nNotes:\n… (earlier notes truncated; `atl notes s` for all)\nxxx") ||
			!strings.HasSuffix(got, "xxx\n"+footer) || len(got) < contextMaxLen-1 {
			t.Errorf("notes not cut from the start, newest kept:\n%s", got)
		}

		// A fixed part too big for notes: notes go, the cap and footer hold.
		for i := 8; i < 30; i++ {
			task.Repos = append(task.Repos, model.TaskRepo{Name: fmt.Sprintf("repo-with-a-long-name-%02d", i), Mode: model.ModeRO})
		}
		got = renderContext(task, many, p, env, now)
		if len(got) > contextMaxLen || !utf8.ValidString(got) || !strings.HasSuffix(got, footer) || strings.Contains(got, "Notes") {
			t.Errorf("pathological: len %d:\n%s", len(got), got)
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

func TestCheckEnvContext(t *testing.T) {
	for _, tc := range []struct {
		raw string
		ok  bool
	}{
		{"", true},
		{strings.Repeat("a", contextEnvLen), true},
		{" \n" + strings.Repeat("a", contextEnvLen) + "\r\n", true}, // trimmed like the hook does
		{strings.Repeat("a", contextEnvLen+1), false},
		{strings.Repeat("é", contextEnvLen/2+1), false}, // bytes, not characters
	} {
		if err := checkEnvContext(tc.raw); (err == nil) != tc.ok {
			t.Errorf("len %d: err %v, want ok=%v", len(tc.raw), err, tc.ok)
		}
	}
	if err := checkEnvContext(strings.Repeat("a", 1043)); err == nil || err.Error() != "$ATL_CONTEXT is 1043 bytes, max 1000" {
		t.Errorf("message: %v", err)
	}
}
