package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/learn"
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
	if _, err := f.st.SetNotes(ctx, task.ID, model.AuthorUser, noteLines(1, n), ""); err != nil {
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
		} {
			if !strings.Contains(got, want) {
				t.Errorf("context lacks %q:\n%s", want, got)
			}
		}
		if strings.Contains(got, "truncated") {
			t.Errorf("short notes truncated:\n%s", got)
		}
		if !strings.HasSuffix(got, "\nATL_PORT_OFFSET=300\n"+cheatSheet("ctx", false)+
			"\nNotes:\n- 2026-04-02 [user] note 1\n- 2026-04-03 [assistant] note 2\n- 2026-04-04 [user] note 3\n"+
			"- 2026-04-05 [assistant] note 4\n- 2026-04-06 [user] note 5\n- 2026-04-07 [assistant] note 6\n"+
			"- 2026-04-08 [user] note 7\n") {
			t.Errorf("bad end (the host's cheat sheet, then the notes, last):\n%s", got)
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
		want := "\nATL_PORT_OFFSET=300\n\nEnvironment notes (ATL_CONTEXT):\nNo runtimes here.\nRun tests via docker.\n" +
			cheatSheet("ctx", false) + "\nNotes:\n"
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
		if err := writeSessionContext(context.Background(), &out, st, f.p, task.ID, "", false, nil, now); err == nil || out.Len() != 0 {
			t.Errorf("err=%v stdout=%q", err, out.String())
		}
		if err := writeSessionContext(context.Background(), &out, f.st, f.p, 9999, "", false, nil, now); err == nil || out.Len() != 0 {
			t.Errorf("missing task: err=%v stdout=%q", err, out.String())
		}
	})
}

// TestRenderContextNotes: the notes come sanitised, last (after the cheat
// sheet), and get the room the rest leaves under contextMaxChars: whole
// when they fit, else their newest end after a marker.
func TestRenderContextNotes(t *testing.T) {
	p := config.Paths{Tasks: "/t", Repos: "/r"}
	base := model.Task{Slug: "s", Title: "T", Status: model.StatusActive, Branch: "atl/s",
		Repos: []model.TaskRepo{{Name: "api", Mode: model.ModeRW, BaseBranch: "main"}}}
	cheat := cheatSheet("s", false)
	const marker = "… (earlier notes truncated; `atl notes s` for all)\n"
	// notesOf returns the notes section of a context ("" if none).
	notesOf := func(t *testing.T, got string) string {
		t.Helper()
		i := strings.Index(got, cheat+"\nNotes:\n")
		if i < 0 {
			if strings.Contains(got, "\nNotes:\n") {
				t.Fatalf("notes section not right after the cheat sheet:\n%s", got)
			}
			return ""
		}
		rest := got[i+len(cheat+"\nNotes:\n"):]
		if !strings.HasSuffix(rest, "\n") {
			t.Fatalf("notes section not last, ending with a newline:\n%q", rest)
		}
		return strings.TrimSuffix(rest, "\n")
	}
	// datedLines returns n notes lines, each with its index and pad.
	datedLines := func(n int, pad string) []string {
		var lines []string
		for i := 0; i < n; i++ {
			lines = append(lines, fmt.Sprintf("- 2026-03-%02d [user] N%04d %s", i%28+1, i, pad))
		}
		return lines
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
			if got := renderContext(base, n, p, "", now); strings.Contains(got, "Notes") || !strings.HasSuffix(got, "\nATL_PORT_OFFSET=0\n"+cheat) {
				t.Errorf("%q:\n%s", n, got)
			}
		}
	})

	t.Run("fits exactly, one more char truncates", func(t *testing.T) {
		room := contextMaxChars - chars(renderContext(base, "", p, "", now)) - chars("\nNotes:\n") - 1
		exact := strings.Repeat("é", room) // 2 bytes, 1 char
		got := renderContext(base, exact, p, "", now)
		if chars(got) != contextMaxChars || notesOf(t, got) != exact {
			t.Fatalf("exact fit: %d chars", chars(got))
		}
		got = renderContext(base, "b\n"+exact, p, "", now)
		if chars(got) > contextMaxChars || notesOf(t, got) != marker+string([]rune(exact)[chars(marker):]) {
			t.Errorf("one line over: %d chars:\n%s", chars(got), notesOf(t, got))
		}
	})

	t.Run("huge notes: newest kept from a line start, oldest dropped", func(t *testing.T) {
		lines := datedLines(1000, strings.Repeat("x", 60)) // ~ 64 KiB
		got := renderContext(base, strings.Join(lines, "\n"), p, "", now)
		n := notesOf(t, got)
		if chars(got) > contextMaxChars || !strings.HasPrefix(n, marker+"- 2026-03-") || !strings.HasSuffix(n, lines[999]) ||
			strings.Contains(n, "N0000 ") || !strings.HasSuffix(got, cheat+"\nNotes:\n"+n+"\n") {
			t.Fatalf("%d chars:\n%s", chars(got), n)
		}
		for _, l := range strings.Split(strings.TrimPrefix(n, marker), "\n") {
			if len(l) != len(lines[0]) {
				t.Errorf("partial line %q", l)
			}
		}
		if chars(got) < contextMaxChars-len(lines[0])-1 {
			t.Errorf("too much room left: %d chars", chars(got))
		}
	})

	t.Run("huge multibyte notes: counted in chars, cut on a rune boundary", func(t *testing.T) {
		// é: 2 bytes, 1 char; 😀: 4 bytes, 2 chars (UTF-16).
		for _, r := range []string{"é", "😀"} {
			notes := "first line\n" + strings.Repeat(r, 40000) + "Z"
			got := renderContext(base, notes, p, "", now)
			n := notesOf(t, got)
			if chars(got) > contextMaxChars || chars(got) < contextMaxChars-2 || !utf8.ValidString(got) ||
				!strings.HasPrefix(n, marker+r) || !strings.HasSuffix(n, r+"Z") || strings.Contains(n, "first line") {
				t.Errorf("%s: %d chars, %d bytes", r, chars(got), len(got))
			}
		}
	})

	t.Run("max everything: within the cap, every section whole", func(t *testing.T) {
		task := base
		task.Slug = strings.Repeat("s", model.MaxSlugLen)
		task.Title = strings.Repeat("😀", model.MaxTitleLen)
		task.Repos = append(task.Repos, model.TaskRepo{Name: "docs", Mode: model.ModeRO})
		var mounts []string
		for i := 0; i < 30; i++ {
			mounts = append(mounts, fmt.Sprintf("/home/someone/shared/readonly-thing-%02d", i))
		}
		env := strings.Repeat("ü", contextEnvLen)
		for _, sandboxed := range []bool{false, true} {
			for _, notes := range []string{strings.Join(datedLines(1000, strings.Repeat("x", 60)), "\n"), strings.Repeat("😀", 30000)} {
				got := renderStartContext(task, notes, p, env, maxSnapshot(), sandboxed, mounts, now)
				if chars(got) > contextMaxChars || !strings.Contains(got, "- Title: "+task.Title+"\n") ||
					!strings.Contains(got, prefsSection(maxSnapshot())) || !strings.Contains(got, "\nEnvironment notes (ATL_CONTEXT):\n"+envBlock(env)+"\n") ||
					!strings.Contains(got, cheatSheet(task.Slug, sandboxed)) || strings.Contains(got, "…\n\nATL_PORT_OFFSET") ||
					(sandboxed && !strings.Contains(got, sandboxNote)) {
					t.Errorf("sandboxed %v: %d chars", sandboxed, chars(got))
				}
			}
		}
	})

	t.Run("many repos: the task block is cut, the rest whole", func(t *testing.T) {
		task := base
		for i := 0; i < 500; i++ {
			task.Repos = append(task.Repos, model.TaskRepo{Name: fmt.Sprintf("repo-with-a-long-name-%03d", i), Mode: model.ModeRO})
		}
		task.PortOffset = 700
		for _, sandboxed := range []bool{false, true} {
			got := renderStartContext(task, "- 2026-03-01 [user] hi", p, "env", maxSnapshot(), sandboxed, []string{"/x"}, now)
			if chars(got) > contextMaxChars || !strings.Contains(got, "READ-ONLY: do not modify by any means)\n…\n\nATL_PORT_OFFSET=700\n") ||
				strings.Contains(got, "repo-with-a-long-name-499") || !strings.Contains(got, prefsSection(maxSnapshot())) ||
				!strings.Contains(got, "\nEnvironment notes (ATL_CONTEXT):\nenv\n") || !strings.Contains(got, cheatSheet("s", sandboxed)) ||
				(sandboxed && !strings.Contains(got, sandboxNote+"\n"+mountsHead+"/x\n")) {
				t.Errorf("sandboxed %v: %d chars:\n%s", sandboxed, chars(got), got)
			}
		}
	})

	t.Run("large task block kept whole", func(t *testing.T) {
		task := base
		task.Title = strings.Repeat("t", model.MaxTitleLen)
		for i := 0; i < 80; i++ {
			task.Repos = append(task.Repos, model.TaskRepo{Name: fmt.Sprintf("repo-%02d", i), Mode: model.ModeRO})
		}
		got := renderContext(task, "- 2026-03-01 [user] hi", p, "", now)
		if !strings.Contains(got, "- Title: "+task.Title+"\n") || !strings.Contains(got, "- repo-79 (ro, /r/repo-79,") ||
			notesOf(t, got) != "- 2026-03-01 [user] hi" || strings.Contains(got, "…") {
			t.Errorf("len %d:\n%s", len(got), got)
		}
	})
}

// maxSnapshot is the largest injection snapshot.
func maxSnapshot() string {
	big := Preferences([]string{strings.Repeat("z", learn.MaxLearningText)}, learn.MaxInjectBudget)
	for len(big)+learn.MaxLearningText+3 < learn.MaxInjectBudget {
		big += "\n- " + strings.Repeat("y", learn.MaxLearningText)
	}
	return big
}

// TestContextFixedFits: at their largest, the sections other than the task
// block and the notes leave the task block room for a big one (longest
// title and slug, 12 repos, long workspace paths), so only a pathological
// one is ever cut; with 4 repos, the notes still get some room.
func TestContextFixedFits(t *testing.T) {
	p := config.Paths{Tasks: "/home/someone-with-a-long-name/atelier/workspaces/a-workspace/tasks",
		Repos: "/home/someone-with-a-long-name/atelier/workspaces/a-workspace/repos"}
	slug := strings.Repeat("s", model.MaxSlugLen)
	task := model.Task{Slug: slug, Title: strings.Repeat("😀", model.MaxTitleLen), Status: model.StatusActive, PortOffset: 1300}
	for i := 0; i < 6; i++ {
		task.Repos = append(task.Repos, model.TaskRepo{Name: fmt.Sprintf("repo-%d", i), Mode: model.ModeRW, BaseBranch: "release/2026-11"},
			model.TaskRepo{Name: fmt.Sprintf("docs-%d", i), Mode: model.ModeRO})
	}
	var mounts []string
	for i := 0; i < 30; i++ {
		mounts = append(mounts, fmt.Sprintf("/home/someone/shared/readonly-thing-%02d", i))
	}
	got := renderStartContext(task, "", p, strings.Repeat("e", contextEnvLen), maxSnapshot(), true, mounts, now)
	if strings.Contains(got, "…\n\nATL_PORT_OFFSET") || chars(got) > contextMaxChars {
		t.Errorf("worst case without notes, 12 repos: %d chars of %d:\n%s", chars(got), contextMaxChars, got)
	}
	t.Logf("worst case without notes, 12 repos: %d chars", chars(got))
	task.Repos = task.Repos[:4]
	got = renderStartContext(task, "", p, strings.Repeat("e", contextEnvLen), maxSnapshot(), true, mounts, now)
	if room := contextMaxChars - chars(got) - len("\nNotes:\n") - 1; room < 1000 {
		t.Errorf("worst case without notes, 4 repos: %d chars, notes room %d", chars(got), room)
	}
	t.Logf("worst case without notes, 4 repos: %d chars", chars(got))
	usual := model.Task{Slug: "fix-login", Title: "Fix the login redirect", Status: model.StatusActive, PortOffset: 300,
		Repos: []model.TaskRepo{{Name: "api", Mode: model.ModeRW, BaseBranch: "main"}, {Name: "docs", Mode: model.ModeRO}}}
	for _, sandboxed := range []bool{false, true} {
		c := renderStartContext(usual, "", p, "", Preferences([]string{strings.Repeat("p", 150)}, 800), sandboxed, nil, now)
		t.Logf("usual context, sandboxed %v: %d chars, notes room %d", sandboxed, chars(c), contextMaxChars-chars(c)-len("\nNotes:\n")-1)
	}
}

func TestRenderContextEnv(t *testing.T) {
	p := config.Paths{Tasks: "/t", Repos: "/r"}
	base := model.Task{Slug: "s", Title: "T", Status: model.StatusActive, Branch: "atl/s", PortOffset: 100,
		Repos: []model.TaskRepo{{Name: "api", Mode: model.ModeRW, BaseBranch: "main"}}}
	notes := "- 2026-03-01 [user] hi"
	cheat := cheatSheet("s", false)
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

	t.Run("placed after ATL_PORT_OFFSET, before the cheat sheet and notes", func(t *testing.T) {
		got := renderContext(base, notes, p, "Use docker for tests.", now)
		want := "\nATL_PORT_OFFSET=100\n" + head + "Use docker for tests.\n" + cheat + "\nNotes:\n- 2026-03-01 [user] hi\n"
		if !strings.HasSuffix(got, want) {
			t.Errorf("got:\n%s", got)
		}
		got = renderContext(base, "", p, "x", now)
		if !strings.HasSuffix(got, "\nATL_PORT_OFFSET=100\n"+head+"x\n"+cheat) {
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
}

// TestRenderSandboxContext: a sandboxed session is told to use a repo's own
// container setup; a session on the host is not.
func TestRenderSandboxContext(t *testing.T) {
	p := config.Paths{Tasks: "/home/someone/atelier/workspaces/alpha/tasks", Repos: "/home/someone/atelier/workspaces/alpha/repos"}
	task := model.Task{Slug: "shop-checkout-fix", Title: "Fix the checkout total rounding", Status: model.StatusActive,
		Branch: "atl/shop-checkout-fix", PortOffset: 300,
		Repos: []model.TaskRepo{{Name: "shop-api", Mode: model.ModeRW, BaseBranch: "main"},
			{Name: "shop-web", Mode: model.ModeRW, BaseBranch: "main"}, {Name: "shop-docs", Mode: model.ModeRO}}}
	now := time.Date(2026, 4, 2, 10, 0, 0, 0, time.UTC)

	got := renderSandboxContext(task, "", p, "", true, nil, now)
	if !strings.Contains(got, "\nATL_PORT_OFFSET=300\n\n"+sandboxNote+"\n") || !strings.Contains(got, "run a repo in its own container setup, if any (`.devcontainer`") ||
		!strings.Contains(got, "network/volume names, build tags start with $COMPOSE_PROJECT_NAME") || !strings.Contains(got, "`-p $COMPOSE_PROJECT_NAME-<repo>`") {
		t.Errorf("sandbox note missing or misplaced:\n%s", got)
	}
	if plain := renderSandboxContext(task, "", p, "", false, []string{"/x"}, now); strings.Contains(plain, "Sandbox:") || strings.Contains(plain, mountsHead) {
		t.Errorf("note without the sandbox:\n%s", plain)
	}
}

// TestRenderSandboxMounts: a sandboxed session is told, in one line after
// the sandbox note, which host paths it has read-only and may bind-mount
// read-only; a long list is cut to mountsLineMax, saying how many more.
func TestRenderSandboxMounts(t *testing.T) {
	p := config.Paths{Tasks: "/home/someone/atelier/workspaces/alpha/tasks", Repos: "/home/someone/atelier/workspaces/alpha/repos"}
	task := model.Task{Slug: "shop-checkout-fix", Title: "Fix the checkout total rounding", Status: model.StatusActive,
		Branch: "atl/shop-checkout-fix", PortOffset: 300,
		Repos: []model.TaskRepo{{Name: "shop-api", Mode: model.ModeRW, BaseBranch: "main"},
			{Name: "shop-web", Mode: model.ModeRW, BaseBranch: "main"}, {Name: "shop-docs", Mode: model.ModeRO}}}
	now := time.Date(2026, 4, 2, 10, 0, 0, 0, time.UTC)
	normal := []string{"/home/someone/.work.npmrc", "/home/someone/certs/corp-ca.pem"}
	var notes strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&notes, "note %02d: something the last session found out\n", i)
	}

	// A normal list, with a usual ATL_SANDBOX_CONTEXT and notes: the whole
	// list, right after the note, and the whole notes.
	env := strings.Repeat("e", 200)
	got := renderSandboxContext(task, notes.String(), p, env, true, normal, now)
	want := "\n" + sandboxNote + "\n" + mountsHead + normal[0] + ", " + normal[1] + "\n\nEnvironment notes"
	if !strings.Contains(got, want) || !strings.Contains(got, "\nNotes:\nnote 00") || !strings.Contains(got, "note 39") {
		t.Errorf("normal list (len %d):\n%s", len(got), got)
	}
	if strings.Count(got, mountsHead) != 1 {
		t.Errorf("not one line:\n%s", got)
	}

	// A long list is cut to mountsLineMax, saying how many more.
	var long []string
	for i := 0; i < 20; i++ {
		long = append(long, fmt.Sprintf("/home/someone/shared/readonly-thing-%02d", i))
	}
	got = renderSandboxContext(task, notes.String(), p, "", true, long, now)
	line := ""
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, mountsHead) {
			line = l
		}
	}
	if len(line)+1 > mountsLineMax || !strings.Contains(line, long[0]+", "+long[1]) || !regexp.MustCompile(` and \d+ more$`).MatchString(line) {
		t.Errorf("long list: %q", line)
	}
	shown := strings.Count(line, "/home/someone/shared/")
	if !strings.HasSuffix(line, fmt.Sprintf(" and %d more", len(long)-shown)) {
		t.Errorf("long list: %d shown, line %q", shown, line)
	}

	if l := mountsLine(normal, len(mountsHead)+len("2, not listed here\n")); l != mountsHead+"2, not listed here\n" {
		t.Errorf("only the count fits: %q", l)
	}
	if l := mountsLine(normal, len(mountsHead)+5); l != "" {
		t.Errorf("nothing fits: %q", l)
	}
	if l := mountsLine([]string{"/a\nInjected: x"}, mountsLineMax); strings.Count(l, "\n") != 1 {
		t.Errorf("a path's newline survived: %q", l)
	}
}

// TestCheatSheet: the start context lists, after a blank line and right
// before the notes, the atl commands the session may run: the complete
// list in a sandbox, the useful ones and those to leave to the user on the
// host.
func TestCheatSheet(t *testing.T) {
	p := config.Paths{Tasks: "/t", Repos: "/r"}
	task := model.Task{Slug: "shop-checkout-fix", Title: "Fix", Status: model.StatusActive, Branch: "atl/shop-checkout-fix",
		Repos: []model.TaskRepo{{Name: "api", Mode: model.ModeRW, BaseBranch: "main"}}}
	common := "- `atl note shop-checkout-fix \"...\"` — append a dated status note for later sessions; authorship is recorded automatically (don't pass --author)\n" +
		"- `atl notes shop-checkout-fix` — print the notes; `--set` replaces them from stdin, `--clear` empties them\n" +
		"- `atl push [--repo R] [--force]` — push this task's branch (--force = with-lease, after a rebase)\n" +
		"- `atl fetch [--repo R]` — git fetch origin\n"
	sandboxed := "\n\natl in this session (complete list, no need for `atl help`):\n" + common +
		"Every other atl command is refused here; ask the user instead.\n"
	host := "\n\natl in this session (no need for `atl help`):\n" + common +
		"- `atl show shop-checkout-fix` / `atl diff shop-checkout-fix` / `atl transcript shop-checkout-fix [--session ID]` — inspect the task\n" +
		"- Only when the user asks (they end or change the task and its sessions): `atl pr shop-checkout-fix`, " +
		"`atl done shop-checkout-fix`, `atl cancel shop-checkout-fix`, `atl status shop-checkout-fix <backlog|active|review|done|cancelled>`, " +
		"`atl add shop-checkout-fix <repo>[:rw|ro]...`, `atl rename shop-checkout-fix <new title...>`\n"
	for _, notes := range []string{"", "- 2026-04-01 [user] hi"} {
		section := ""
		if notes != "" {
			section = "\nNotes:\n" + notes + "\n"
		}
		if got := renderSandboxContext(task, notes, p, "", true, nil, time.Time{}); !strings.HasSuffix(got, sandboxed+section) {
			t.Errorf("sandboxed, notes %q:\n%s", notes, got)
		}
		if got := renderSandboxContext(task, notes, p, "", false, nil, time.Time{}); !strings.HasSuffix(got, host+section) || strings.Contains(got, "refused here") {
			t.Errorf("host, notes %q:\n%s", notes, got)
		}
	}

	// contextCheatLen is the longest sheet; a longer slug than atl makes
	// is left out.
	if n := strings.Count(cheatHost, slugPlaceholder); n != cheatHostSlugs {
		t.Errorf("cheatHost names the slug %d times, cheatHostSlugs is %d", n, cheatHostSlugs)
	}
	longest := strings.Repeat("s", model.MaxSlugLen)
	if s, h := cheatSheet(longest, true), cheatSheet(longest, false); len(s) > contextCheatLen || len(h) != contextCheatLen {
		t.Errorf("sheets %d and %d bytes, contextCheatLen %d", len(s), len(h), contextCheatLen)
	}
	for _, sb := range []bool{true, false} {
		if s := cheatSheet(longest+"s", sb); len(s) > contextCheatLen || strings.Contains(s, longest) || !strings.Contains(s, "`atl note <slug> ") {
			t.Errorf("over-long slug, sandboxed %v:\n%s", sb, s)
		}
	}
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
	checkLatency(t, "SessionStart+context hook", func() time.Duration {
		exit, out, errOut, wall := runMain(t, "SessionStart", in)
		if exit != 0 || errOut != "" || !strings.Contains(sessionStartContext(t, out), "note 20") {
			t.Fatalf("exit=%d stdout=%q stderr=%q", exit, out, errOut)
		}
		return wall
	})
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
