package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/sandbox"
)

// gated serves task "fix" of f through a gate's control socket, as its task
// container sees it, and returns the socket.
func gated(t *testing.T, f *fixture) string { return gatedWith(t, f, nil) }

// gatedWith is gated with setup run on the task server before it serves.
func gatedWith(t *testing.T, f *fixture, setup func(*TaskServer)) string {
	t.Helper()
	if err := os.MkdirAll(f.p.ClaudeDir("fix"), 0o700); err != nil {
		t.Fatal(err)
	}
	ts, err := OpenTaskServer(f.p, f.task.ID, "fix")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ts.Close() })
	if setup != nil {
		setup(ts)
	}
	dir, err := os.MkdirTemp("", "gate") // short: a socket path has at most 107 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "atl.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go (&sandbox.Control{Slug: "fix", TaskID: f.task.ID, Atl: "/bin/false", Task: ts}).Serve(l)
	return sock
}

// inContainer is env as atl in the task container of sock's gate sees it.
func inContainer(sock string, env map[string]string) func(string) string {
	m := map[string]string{sandbox.InEnv: "fix-abc123", "ATL_CONTROL_SOCK": sock}
	for k, v := range env {
		m[k] = v
	}
	return envOf(m)
}

type hookResult struct {
	code           int
	stdout, stderr string
}

var asOf = regexp.MustCompile(`current as of [^)]*\)`)

// hookVia runs `atl hook event` with stdin: through the gate at sock when
// sock is not "", else directly on f's database as atl in a task container
// did when that database was mounted (sandbox.InEnv set).
func hookVia(t *testing.T, f *fixture, sock, event, stdin string, env map[string]string) hookResult {
	t.Helper()
	var out, errb bytes.Buffer
	var code int
	if sock != "" {
		code = run([]string{event}, strings.NewReader(stdin), &out, &errb, inContainer(sock, env))
	} else {
		m := map[string]string{sandbox.InEnv: "fix-abc123"}
		for k, v := range env {
			m[k] = v
		}
		if len(stdin) > maxInput+1 { // what run reads
			stdin = stdin[:maxInput+1]
		}
		code = db{p: f.p}.serve(event, []byte(stdin), nil, nil, &out, &errb, envOf(m))
	}
	return hookResult{code, asOf.ReplaceAllString(out.String(), "current as of T)"), errb.String()}
}

// rows renders every session row (the start time of rows the fixture made
// is an hour before now: "touched" says whether an event moved it).
func rows(t *testing.T, f *fixture) string {
	t.Helper()
	all, err := f.st.ListSessionsByState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, s := range all {
		fmt.Fprintf(&b, "%d task=%d cid=%s state=%s transcript=%s touched=%v\n", s.ID, s.TaskID, s.ClaudeSessionID,
			s.State, s.TranscriptPath, !s.LastEventAt.Equal(now.Add(-time.Hour)))
	}
	return b.String()
}

func hookInput(in Input) string {
	b, _ := json.Marshal(in)
	return string(b)
}

// TestHookThroughGate: every hook event through the gate has the same
// effect on the database, exit code, stdout and stderr as the same event
// handled in the task container with the database at hand (as before).
func TestHookThroughGate(t *testing.T) {
	old := maxInput
	maxInput = 4 << 20
	t.Cleanup(func() { maxInput = old })
	type step struct {
		event string
		in    func(f *fixture) string
		env   map[string]string
	}
	docs := func(f *fixture, name string) string { return filepath.Join(f.p.RepoPath("docs"), name) }
	rw := func(f *fixture) string { return filepath.Join(f.p.WorktreePath("fix", "api"), "main.go") }
	pre := func(target func(f *fixture) string, content string) func(f *fixture) string {
		return func(f *fixture) string {
			ti, _ := json.Marshal(map[string]string{"file_path": target(f), "content": content})
			return hookInput(Input{SessionID: "c1", Cwd: f.p.TaskDir("fix"), HookEventName: EventPreToolUse, ToolName: "Write", ToolInput: ti})
		}
	}
	// A transcript path "CLAUDE/x" is x in the task's Claude config dir.
	simple := func(event string, in Input) func(f *fixture) string {
		return func(f *fixture) string {
			in := in
			in.Cwd, in.HookEventName = f.p.TaskDir("fix"), event
			if rest, ok := strings.CutPrefix(in.TranscriptPath, "CLAUDE/"); ok {
				in.TranscriptPath = filepath.Join(f.p.ClaudeDir("fix"), rest)
			}
			return hookInput(in)
		}
	}
	session := map[string]string{"ATL_SESSION": "1", "ATL_CONTEXT": "TASK CONTAINER rules"}
	big := strings.Repeat("0123456789abcdef", 1<<17) // 2 MiB
	for _, c := range []struct {
		name  string
		codes []int // exit code of each step (both ways)
		steps []step
	}{
		{"a session's life", []int{0, 0, 0, 0, 0, 0, 0, 0}, []step{
			{EventSessionStart, simple(EventSessionStart, Input{SessionID: "c1", Source: "startup", TranscriptPath: "CLAUDE/projects/k/c1.jsonl"}), session},
			{EventUserPromptSubmit, simple(EventUserPromptSubmit, Input{SessionID: "c1"}), session},
			{EventNotification, simple(EventNotification, Input{SessionID: "c1", NotificationType: "permission_prompt"}), session},
			{EventPostToolUse, simple(EventPostToolUse, Input{SessionID: "c1", ToolName: "Bash"}), session},
			{EventStop, simple(EventStop, Input{SessionID: "c1"}), session},
			{EventSessionStart, simple(EventSessionStart, Input{SessionID: "c2", Source: "clear", TranscriptPath: "CLAUDE/projects/k/c2.jsonl"}), session},
			{EventSessionStart, simple(EventSessionStart, Input{SessionID: "c3", Source: "compact"}), session},
			{EventSessionEnd, simple(EventSessionEnd, Input{SessionID: "c3"}), session},
		}},
		{"a session atl did not launch", []int{0, 0}, []step{
			{EventSessionStart, simple(EventSessionStart, Input{SessionID: "fresh", Source: "startup"}), nil},
			{EventUserPromptSubmit, simple(EventUserPromptSubmit, Input{SessionID: "fresh"}), nil},
		}},
		{"ro guard", []int{2, 0, 2, 2, 0}, []step{
			{EventPreToolUse, pre(func(f *fixture) string { return docs(f, "a.md") }, "x"), session},
			{EventPreToolUse, pre(rw, "x"), session},
			{EventPreToolUse, pre(func(f *fixture) string { return filepath.Join(f.p.RepoPath("api"), "x.go") }, "x"), session},
			{EventPreToolUse, pre(func(f *fixture) string { return docs(f, "big.txt") }, big), session},
			{EventPreToolUse, pre(rw, big), session},
		}},
		{"oversized and broken input", []int{2, 2, 2, 2, 0, 0, 0, 0, 0, 0}, []step{
			{EventPreToolUse, pre(rw, big+big), session},                                              // over maxInput: refused
			{EventPreToolUse, pre(func(f *fixture) string { return docs(f, "b") }, big+big), session}, // refused (content first)
			{EventPreToolUse, func(f *fixture) string { // target first: the guard sees it and blocks
				return `{"session_id":"c1","cwd":"` + f.p.TaskDir("fix") + `","tool_name":"Write","tool_input":{"file_path":"` +
					docs(f, "d") + `","content":"` + big + big + `"}}`
			}, session},
			{EventPreToolUse, func(f *fixture) string { // truncated, target first: blocked
				return `{"session_id":"c1","cwd":"` + f.p.TaskDir("fix") + `","tool_input":{"file_path":"` + docs(f, "e") + `","content":"abc`
			}, session},
			{EventPreToolUse, func(f *fixture) string { return pre(func(f *fixture) string { return docs(f, "c") }, "")(f)[:120] }, session},
			{EventPreToolUse, func(*fixture) string { return "]]" }, session},
			{EventStop, func(*fixture) string { return "{not json" }, session},
			{EventStop, func(*fixture) string { return `{"session_id":"c1","x":"` + big + big + `"}` }, session},
			{"SubagentStop", simple("SubagentStop", Input{SessionID: "c1"}), session},
			{"", func(*fixture) string { return "" }, session},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			direct, gate := setup(t), setup(t)
			sock := gated(t, gate)
			for _, f := range []*fixture{direct, gate} {
				if s := f.session(t, model.StateIdle, ""); s.ID != 1 {
					t.Fatalf("session id %d", s.ID)
				}
			}
			for i, s := range c.steps {
				want := hookVia(t, direct, "", s.event, s.in(direct), s.env)
				got := hookVia(t, gate, sock, s.event, s.in(gate), s.env)
				// Paths differ between the two fixtures' temp dirs.
				got.stdout = strings.ReplaceAll(got.stdout, gate.root, direct.root)
				got.stderr = strings.ReplaceAll(got.stderr, gate.root, direct.root)
				if got != want {
					t.Errorf("step %d %s:\n got  %+v\n want %+v", i, s.event, got, want)
				}
				if want.code != c.codes[i] {
					t.Errorf("step %d %s: exit %d, want %d (%s)", i, s.event, want.code, c.codes[i], want.stderr)
				}
				if c.name == "oversized and broken input" && i == 2 && !strings.Contains(want.stderr, "read-only repo docs") {
					t.Errorf("step %d: an oversized write whose target comes first is not blocked by the guard: %s", i, want.stderr)
				}
				r := strings.ReplaceAll(rows(t, gate), gate.p.ClaudeDir("fix"), direct.p.ClaudeDir("fix"))
				if w := rows(t, direct); r != w {
					t.Errorf("step %d %s: rows\n%s\nwant\n%s", i, s.event, r, w)
				}
			}
			// The hook log (errors) is written where the gate runs, as it was.
			logLines := func(f *fixture) []string {
				var out []string
				for _, l := range strings.Split(strings.TrimSpace(readLog(t, f.p)), "\n") {
					if _, rest, ok := strings.Cut(l, " "); ok {
						out = append(out, strings.ReplaceAll(rest, f.root, "ROOT"))
					}
				}
				sort.Strings(out)
				return out
			}
			if g, w := logLines(gate), logLines(direct); strings.Join(g, "\n") != strings.Join(w, "\n") {
				t.Errorf("hook log through the gate:\n%s\nwant:\n%s", strings.Join(g, "\n"), strings.Join(w, "\n"))
			}
		})
	}
}

// TestGateSessionEnd: the gate is told each session a SessionEnd is for
// (it then ends what the session leaves running in the container),
// and nothing for other events or a SessionEnd that matches no session.
func TestGateSessionEnd(t *testing.T) {
	f := setup(t)
	var mu sync.Mutex
	var ended []int64
	sock := gatedWith(t, f, func(ts *TaskServer) {
		ts.OnSessionEnd(func(id int64) {
			mu.Lock()
			ended = append(ended, id)
			mu.Unlock()
		})
	})
	s := f.session(t, model.StateIdle, "")
	other := f.session(t, model.StateIdle, "")
	env := map[string]string{"ATL_SESSION": strconv.FormatInt(s.ID, 10)}
	in := func(event, sid string) string {
		return hookInput(Input{SessionID: sid, Cwd: f.p.TaskDir("fix"), HookEventName: event, Source: "startup"})
	}
	hookVia(t, f, sock, EventSessionStart, in(EventSessionStart, "c1"), env)
	hookVia(t, f, sock, EventStop, in(EventStop, "c1"), env)
	hookVia(t, f, sock, EventSessionEnd, in(EventSessionEnd, "nobody"), nil)
	mu.Lock()
	if len(ended) != 0 {
		t.Errorf("told of ended sessions %v before any ended", ended)
	}
	mu.Unlock()
	hookVia(t, f, sock, EventSessionEnd, in(EventSessionEnd, "c1"), env)
	mu.Lock()
	defer mu.Unlock()
	if len(ended) != 1 || ended[0] != s.ID || ended[0] == other.ID {
		t.Errorf("told of ended sessions %v, want [%d]", ended, s.ID)
	}
	if got, err := f.st.GetSession(context.Background(), s.ID); err != nil || got.State != model.StateEnded {
		t.Errorf("session %d: %+v, %v", s.ID, got, err)
	}
}

// TestHookThroughGateContext: the SessionStart context through the gate is
// the task's, with the sandbox note and the container's ATL_CONTEXT.
func TestHookThroughGateContext(t *testing.T) {
	f := setup(t)
	sock := gated(t, f)
	s := f.session(t, model.StateIdle, "")
	in := hookInput(Input{SessionID: "c1", Cwd: f.p.TaskDir("fix"), Source: "startup"})
	r := hookVia(t, f, sock, EventSessionStart, in, map[string]string{"ATL_SESSION": itoa(s.ID), "ATL_CONTEXT": "TASK CONTAINER rules"})
	ctx := sessionStartContext(t, r.stdout)
	for _, want := range []string{"- Slug: fix", sandboxNote, "TASK CONTAINER rules"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("context lacks %q:\n%s", want, ctx)
		}
	}
	if r.code != 0 || r.stderr != "" || f.get(t, s.ID).ClaudeSessionID != "c1" {
		t.Errorf("result %+v, row %+v", r, f.get(t, s.ID))
	}
	if strings.Contains(ctx, mountsHead) {
		t.Errorf("a mounts line without mounts:\n%s", ctx)
	}

	// The gate's read-only host paths are named, after the sandbox note.
	sock = gatedWith(t, f, func(ts *TaskServer) { ts.SetROMounts([]string{"/home/u/.npmrc-corp", "/etc/corp-ca"}) })
	r = hookVia(t, f, sock, EventSessionStart, in, map[string]string{"ATL_SESSION": itoa(s.ID)})
	if ctx := sessionStartContext(t, r.stdout); !strings.Contains(ctx, sandboxNote+"\n"+mountsHead+"/home/u/.npmrc-corp, /etc/corp-ca\n") {
		t.Errorf("context lacks the mounts line:\n%s", ctx)
	}
}

// snapshot renders everything of task "other" in f: its row, notes and
// sessions.
func snapshot(t *testing.T, f *fixture) string {
	t.Helper()
	ctx := context.Background()
	o, err := f.st.GetTaskBySlug(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	notes, err := f.st.GetNotes(ctx, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := f.st.ListSessions(ctx, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%+v\nnotes=%q\nsessions=%+v", *o, notes, ss)
}

// TestGateScope: nothing a request from the task container carries reaches
// past the gate's own task: another task's session id (ATL_SESSION), its
// Claude session id, its slug (ATL_TASK) or a working directory inside it
// change nothing of that task, and resolve to the gate's task or to nothing.
func TestGateScope(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	sock := gated(t, f)
	o, err := f.st.GetTaskBySlug(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	theirs := &model.Session{TaskID: o.ID, ClaudeSessionID: "o1", State: model.StateIdle, LastEventAt: now.Add(-time.Hour), TranscriptPath: "/t/o1.jsonl"}
	if err := f.st.CreateSession(ctx, theirs); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.AppendNote(ctx, o.ID, model.AuthorUser, "their note"); err != nil {
		t.Fatal(err)
	}
	mine := f.session(t, model.StateIdle, "m1")
	before := snapshot(t, f)
	otherDir := f.p.TaskDir("other")
	asThem := map[string]string{"ATL_SESSION": itoa(theirs.ID), "ATL_TASK": "other"}

	for _, c := range []struct {
		name  string
		event string
		in    Input
		env   map[string]string
	}{
		{"their ATL_SESSION and claude id", EventUserPromptSubmit, Input{SessionID: "o1", Cwd: otherDir}, asThem},
		{"their claude id", EventStop, Input{SessionID: "o1"}, nil},
		{"their claude id, my ATL_SESSION", EventSessionEnd, Input{SessionID: "o1"}, map[string]string{"ATL_SESSION": itoa(mine.ID)}},
		{"their ATL_SESSION, no claude id", EventNotification, Input{NotificationType: "permission_prompt"}, asThem},
		{"their transcript", EventPostToolUse, Input{SessionID: "o1", TranscriptPath: "/t/evil.jsonl"}, asThem},
		{"start as them", EventSessionStart, Input{SessionID: "o1", Source: "resume", Cwd: otherDir}, asThem},
		{"start in their dir", EventSessionStart, Input{SessionID: "new1", Source: "startup", Cwd: otherDir}, map[string]string{"ATL_TASK": "other"}},
		{"compact onto their id", EventSessionStart, Input{SessionID: "o1", Source: "compact"}, map[string]string{"ATL_SESSION": itoa(mine.ID)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := hookVia(t, f, sock, c.event, hookInput(c.in), c.env)
			if r.code != 0 {
				t.Errorf("result %+v", r)
			}
			if got := snapshot(t, f); got != before {
				t.Errorf("task other changed:\n%s\nwas\n%s", got, before)
			}
			if strings.Contains(r.stdout, "Slug: other") {
				t.Errorf("their context came back: %s", r.stdout)
			}
		})
	}
	// No row of the gate's task took their Claude id, and the session
	// started "in their dir" is the gate's task's.
	all, _ := f.st.ListSessions(ctx, f.task.ID)
	for _, s := range all {
		if s.ClaudeSessionID == "o1" {
			t.Errorf("row %+v of task fix has their claude id", s)
		}
	}
	if s, err := f.st.GetSessionByClaudeID(ctx, "new1"); err != nil || s.TaskID != f.task.ID {
		t.Errorf("session started in their dir: %+v, %v", s, err)
	}
	if got, err := f.st.GetSessionByClaudeID(ctx, "o1"); err != nil || got.ID != theirs.ID {
		t.Errorf("their claude id now finds %+v, %v", got, err)
	}
	if log := readLog(t, f.p); !strings.Contains(log, "belongs to another task") {
		t.Errorf("refused claude id not logged:\n%s", log)
	}

	// The guard decides with the gate's task (docs is ro in fix, not in
	// other), whatever ATL_TASK and cwd say.
	ti, _ := json.Marshal(map[string]string{"file_path": filepath.Join(f.p.RepoPath("docs"), "a.md")})
	r := hookVia(t, f, sock, EventPreToolUse, hookInput(Input{SessionID: "o1", Cwd: otherDir, ToolName: "Write", ToolInput: ti}), asThem)
	if r.code != 2 || !strings.Contains(r.stderr, "read-only repo docs") {
		t.Errorf("guard as them: %+v", r)
	}

	// The status line shows the gate's task, not theirs.
	for _, in := range []string{`{"session_id":"o1","cwd":"` + otherDir + `"}`, `{"cwd":"` + otherDir + `"}`} {
		var out, errb bytes.Buffer
		runStatusLine(strings.NewReader(in), &out, &errb, inContainer(sock, map[string]string{"ATL_TASK": "other", "NO_COLOR": "1"}))
		if out.String() != "● Fix\n" {
			t.Errorf("status line for %s = %q (%s)", in, out.String(), errb.String())
		}
	}
	if got := snapshot(t, f); got != before {
		t.Errorf("task other changed:\n%s\nwas\n%s", got, before)
	}

	// A request that tries to pass ATL_TASK on is refused by the gate.
	code, ran, err := sandbox.Ask(sock, sandbox.ControlRequest{Op: sandbox.OpHook, Event: EventStop,
		Env: map[string]string{"ATL_TASK": "other"}}, []byte(`{}`), &bytes.Buffer{}, &bytes.Buffer{}, time.Minute)
	if err != nil || ran || code == 0 {
		t.Errorf("ATL_TASK in a request: exit %d, ran %v, %v", code, ran, err)
	}
}

// TestGateNotes: note and notes through the gate act on the gate's task.
func TestGateNotes(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	sock := gated(t, f)
	before := snapshot(t, f)
	ask := func(req sandbox.ControlRequest, body string) (int, string, string) {
		t.Helper()
		var out, errb bytes.Buffer
		code, ran, err := sandbox.Ask(sock, req, []byte(body), &out, &errb, time.Minute)
		if err != nil || !ran {
			t.Fatalf("%+v: ran %v, %v (%s)", req, ran, err, errb.String())
		}
		return code, out.String(), errb.String()
	}
	notes := func() string {
		s, err := f.st.GetNotes(ctx, f.task.ID)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if code, out, _ := ask(sandbox.ControlRequest{Op: sandbox.OpNote, Author: "assistant"}, "from the container"); code != 0 || out != "note added\n" ||
		!strings.HasSuffix(notes(), "[assistant] from the container") {
		t.Errorf("note: %d %q, notes %q", code, out, notes())
	}
	if code, out, _ := ask(sandbox.ControlRequest{Op: sandbox.OpNotes}, ""); code != 0 || !strings.Contains(out, "[assistant] from the container") {
		t.Errorf("notes: %d %q", code, out)
	}
	if code, out, _ := ask(sandbox.ControlRequest{Op: sandbox.OpNotes, Set: true}, "new\ttext\n"); code != 0 || out != "notes replaced (8 bytes)\n" || notes() != "new\ttext" {
		t.Errorf("notes --set: %d %q, notes %q", code, out, notes())
	}
	if code, _, errs := ask(sandbox.ControlRequest{Op: sandbox.OpNotes, Set: true}, "  \n"); code != 1 || !strings.Contains(errs, "stdin is empty") || notes() != "new\ttext" {
		t.Errorf("notes --set empty: %d %q", code, errs)
	}
	if code, _, errs := ask(sandbox.ControlRequest{Op: sandbox.OpNote, Author: "root"}, "x"); code != 1 || !strings.Contains(errs, "invalid note author") {
		t.Errorf("note --author root: %d %q", code, errs)
	}
	if code, out, _ := ask(sandbox.ControlRequest{Op: sandbox.OpNotes, Clear: true}, ""); code != 0 || out != "notes cleared\n" || notes() != "" {
		t.Errorf("notes --clear: %d %q", code, out)
	}
	if got := snapshot(t, f); got != before {
		t.Errorf("task other changed:\n%s\nwas\n%s", got, before)
	}
}

// TestHookWithoutGate: when the gate cannot be reached the hook fails like
// any hook error (exit 0, the error on stderr, nothing written in the
// container), except that an oversized PreToolUse is refused.
func TestHookWithoutGate(t *testing.T) {
	f := setup(t)
	old := maxInput
	maxInput = 4096
	t.Cleanup(func() { maxInput = old })
	none := filepath.Join(t.TempDir(), "none.sock")
	ro := filepath.Join(f.p.RepoPath("docs"), "a.md")
	ti, _ := json.Marshal(map[string]string{"file_path": ro, "content": strings.Repeat("x", 5000)})
	for _, c := range []struct {
		event, stdin string
		code         int
		stderr       string
	}{
		{EventStop, `{"session_id":"c"}`, 0, "cannot reach the task's gate"},
		{EventSessionStart, `{"session_id":"c"}`, 0, "cannot reach the task's gate"},
		{EventPreToolUse, hookInput(Input{ToolName: "Write", ToolInput: json.RawMessage(`{"file_path":"` + ro + `"}`)}), 0, "cannot reach"},
		{EventPreToolUse, hookInput(Input{ToolName: "Write", ToolInput: ti}), 2, "refusing the Write tool call"},
	} {
		var out, errb bytes.Buffer
		code := run([]string{c.event}, strings.NewReader(c.stdin), &out, &errb, inContainer(none, nil))
		if code != c.code || out.Len() != 0 || !strings.Contains(errb.String(), c.stderr) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", c.event, code, out.String(), errb.String())
		}
	}
	var out, errb bytes.Buffer
	if code := runStatusLine(strings.NewReader(`{}`), &out, &errb, inContainer(none, nil)); code != 0 || out.Len() != 0 {
		t.Errorf("status line: %d %q", code, out.String())
	}
	if _, err := os.Stat(f.p.HookLog()); !os.IsNotExist(err) {
		t.Errorf("hook log written without a gate: %v", err)
	}
}

// TestGateLandings: a symlink that exists only in the task container (here:
// one the gate cannot see, because the client resolves before the gate
// looks) still makes the guard block, through the landings the container
// sends; landings can only add blocks.
func TestGateLandings(t *testing.T) {
	f := setup(t)
	sock := gated(t, f)
	link := filepath.Join(t.TempDir(), "into-docs")
	if err := os.Symlink(f.p.RepoPath("docs"), link); err != nil {
		t.Fatal(err)
	}
	in := hookInput(Input{Cwd: f.p.TaskDir("fix"), ToolName: "Write", ToolInput: json.RawMessage(`{"file_path":"` + link + `/a.md"}`)})
	landings := containerLandings([]byte(in))
	found := false
	for _, l := range landings {
		found = found || l.Path == filepath.Join(f.p.RepoPath("docs"), "a.md")
	}
	if !found {
		t.Fatalf("landings %+v", landings)
	}
	if err := os.Remove(link); err != nil { // gone where the gate looks
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code, ran, err := sandbox.Ask(sock, sandbox.ControlRequest{Op: sandbox.OpHook, Event: EventPreToolUse, Landings: landings},
		[]byte(in), &out, &errb, time.Minute)
	if err != nil || !ran || code != 2 || !strings.Contains(errb.String(), "resolves to "+filepath.Join(f.p.RepoPath("docs"), "a.md")) {
		t.Errorf("exit %d, ran %v, %v, %q", code, ran, err, errb.String())
	}
	// Without the landings the gate alone allows it (the link is gone).
	code, _, _ = sandbox.Ask(sock, sandbox.ControlRequest{Op: sandbox.OpHook, Event: EventPreToolUse}, []byte(in), &out, &errb, time.Minute)
	if code != 0 {
		t.Errorf("without landings: exit %d", code)
	}
	// A landing outside the task's repos blocks nothing.
	code, _, _ = sandbox.Ask(sock, sandbox.ControlRequest{Op: sandbox.OpHook, Event: EventPreToolUse,
		Landings: []sandbox.Landing{{Given: "/x", Path: f.p.TaskDir("fix")}, {Given: "/y", Path: "relative/path"}}}, []byte(in), &out, &errb, time.Minute)
	if code != 0 {
		t.Errorf("harmless landings: exit %d", code)
	}
}

// TestLogErrorOneLine: an event name or session id from the hook input
// (from the task, through a gate) cannot add lines to the hook log.
func TestLogErrorOneLine(t *testing.T) {
	f := setup(t)
	logError(f.p, &bytes.Buffer{}, "Stop\n2026-01-01T00:00:00Z event=x", "a\nb\rc", errors.New("one\ntwo"))
	log := readLog(t, f.p)
	if strings.Count(log, "\n") != 1 || !strings.Contains(log, "session_id=a?b?c") {
		t.Errorf("hook log = %q", log)
	}
}

// TestGateTranscriptPath: the gate records a transcript path only inside
// the task's own Claude config dir, symlinks resolved; any other is
// ignored (the event still counts) and logged.
func TestGateTranscriptPath(t *testing.T) {
	f := setup(t)
	sock := gated(t, f)
	own := f.p.ClaudeDir("fix")
	project := filepath.Join(own, "projects", "k")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	userDir := filepath.Join(f.root, ".claude", "projects", "k")
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(userDir, filepath.Join(own, "projects", "out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.root, ".claude", ".credentials.json"), filepath.Join(project, "creds.jsonl")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, path string
		ok         bool
	}{
		{"own, not written yet", filepath.Join(project, "a.jsonl"), true},
		{"the user's dir", filepath.Join(userDir, "b.jsonl"), false},
		{"another task's dir", filepath.Join(f.p.ClaudeDir("other"), "projects", "k", "c.jsonl"), false},
		{"dot-dot out of it", filepath.Join(own, "..", "x.jsonl"), false},
		{"own dir itself", own, false},
		{"a symlinked dir out of it", filepath.Join(own, "projects", "out", "d.jsonl"), false},
		{"a symlink out of it", filepath.Join(project, "creds.jsonl"), false},
		{"relative", "projects/k/e.jsonl", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := f.session(t, model.StateIdle, "")
			in := hookInput(Input{SessionID: "cid-" + itoa(s.ID), Cwd: f.p.TaskDir("fix"), Source: "startup", TranscriptPath: c.path})
			r := hookVia(t, f, sock, EventSessionStart, in, map[string]string{"ATL_SESSION": itoa(s.ID)})
			got := f.get(t, s.ID)
			if r.code != 0 || got.ClaudeSessionID != "cid-"+itoa(s.ID) {
				t.Fatalf("event not applied: %+v, row %+v", r, got)
			}
			if want := map[bool]string{true: c.path, false: ""}[c.ok]; got.TranscriptPath != want {
				t.Errorf("transcript path recorded %q, want %q", got.TranscriptPath, want)
			}
			if logged := strings.Contains(readLog(t, f.p), "transcript path ignored: "+strconv.Quote(c.path)); logged == c.ok {
				t.Errorf("logged = %v:\n%s", logged, readLog(t, f.p))
			}
		})
	}
}

// TestGateClaudeID: a Claude session id that cannot be one (control
// characters, or a leading "-": resuming runs `claude --resume <id>`,
// where it would be an option) is not recorded; the event still counts,
// and the id is logged as ignored.
func TestGateClaudeID(t *testing.T) {
	f := setup(t)
	sock := gated(t, f)
	start := func(cid string) *model.Session {
		t.Helper()
		s := f.session(t, model.StateEnded, "old")
		in := hookInput(Input{SessionID: cid, Cwd: f.p.TaskDir("fix"), Source: "clear"})
		if r := hookVia(t, f, sock, EventSessionStart, in, map[string]string{"ATL_SESSION": itoa(s.ID)}); r.code != 0 {
			t.Fatalf("result %+v", r)
		}
		got := f.get(t, s.ID)
		if got.State != model.StateIdle {
			t.Fatalf("event not applied: row %+v", got)
		}
		return got
	}
	for _, cid := range []string{"--dangerously-skip-permissions", "-x", "a\x1b]0;pwned\x07b", "a\nb", "a\u009bb"} {
		t.Run(strconv.Quote(cid), func(t *testing.T) {
			if got := start(cid); got.ClaudeSessionID != "old" {
				t.Errorf("claude id recorded: %q", got.ClaudeSessionID)
			}
			if !strings.Contains(readLog(t, f.p), "claude session id ignored") {
				t.Errorf("not logged:\n%s", readLog(t, f.p))
			}
		})
	}
	if got := start("1f0c3a52-7d4e-4b8a-9c1e-2f3a4b5c6d7e"); got.ClaudeSessionID != "1f0c3a52-7d4e-4b8a-9c1e-2f3a4b5c6d7e" {
		t.Errorf("a valid claude id not recorded: %q", got.ClaudeSessionID)
	}
}

// TestGateLatency: a hook round trip through the gate (client and gate in
// one process here) stays in the low milliseconds.
func TestGateLatency(t *testing.T) {
	if raceEnabled {
		t.Skip("race-instrumented code is too slow to measure")
	}
	f := setup(t)
	sock := gated(t, f)
	s := f.session(t, model.StateIdle, "c1")
	env := map[string]string{"ATL_SESSION": itoa(s.ID)}
	for _, c := range []struct{ event, in string }{
		{EventSessionStart, hookInput(Input{SessionID: "c1", Cwd: f.p.TaskDir("fix"), Source: "startup"})},
		{EventPreToolUse, hookInput(Input{SessionID: "c1", Cwd: f.p.TaskDir("fix"), ToolName: "Edit",
			ToolInput: json.RawMessage(`{"file_path":"` + filepath.Join(f.p.WorktreePath("fix", "api"), "x.go") + `"}`)})},
		{EventPostToolUse, hookInput(Input{SessionID: "c1", ToolName: "Bash"})},
	} {
		var walls []time.Duration
		for i := 0; i < 15; i++ {
			start := time.Now()
			if r := hookVia(t, f, sock, c.event, c.in, env); r.code != 0 || r.stderr != "" {
				t.Fatalf("%s: %+v", c.event, r)
			}
			walls = append(walls, time.Since(start))
		}
		sort.Slice(walls, func(i, j int) bool { return walls[i] < walls[j] })
		median := walls[len(walls)/2]
		t.Logf("%s through the gate: min %v, median %v, max %v", c.event, walls[0], median, walls[len(walls)-1])
		if median > 50*time.Millisecond {
			t.Errorf("%s: median %v", c.event, median)
		}
	}
}

// TestTaskServerEdges: a task server opens only for a task that still has
// its id and slug, and notices when the task goes; a note needs text and
// is the user's unless it says otherwise; an unknown op is refused.
func TestTaskServerEdges(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	if _, err := OpenTaskServer(f.p, f.task.ID, "other"); err == nil || !strings.Contains(err.Error(), "is not other") {
		t.Fatalf("open with another task's slug: %v", err)
	}
	if _, err := OpenTaskServer(f.p, 9999, "fix"); err == nil {
		t.Fatal("open for a missing task: no error")
	}
	ts, err := OpenTaskServer(f.p, f.task.ID, "fix")
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	do := func(req sandbox.ControlRequest, body string) (int, string, string) {
		var out, errb bytes.Buffer
		code := ts.Do(req, []byte(body), &out, &errb)
		return code, out.String(), errb.String()
	}
	if code, _, errs := do(sandbox.ControlRequest{Op: "frobnicate"}, ""); code != 1 || !strings.Contains(errs, `unknown operation "frobnicate"`) {
		t.Errorf("unknown op: %d %q", code, errs)
	}
	if code, _, errs := do(sandbox.ControlRequest{Op: sandbox.OpNote}, " \n\x01\t"); code != 1 || !strings.Contains(errs, "note is empty") {
		t.Errorf("empty note: %d %q", code, errs)
	}
	if code, out, _ := do(sandbox.ControlRequest{Op: sandbox.OpNote}, "plain"); code != 0 || out != "note added\n" {
		t.Errorf("note without author: %d %q", code, out)
	}
	if notes, err := f.st.GetNotes(ctx, f.task.ID); err != nil || !strings.HasSuffix(notes, "[user] plain") {
		t.Errorf("notes %q, %v", notes, err)
	}
	if code, _, errs := do(sandbox.ControlRequest{Op: sandbox.OpHook, Event: EventAssistantGuard}, "{}"); code != 2 || errs == "" {
		t.Errorf("assistant guard for a task container: %d %q", code, errs)
	}
	if gone, err := ts.Gone(ctx); err != nil || gone {
		t.Fatalf("Gone = %v, %v", gone, err)
	}
	if err := f.st.DeleteTask(ctx, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if gone, err := ts.Gone(ctx); err != nil || !gone {
		t.Fatalf("Gone after delete = %v, %v", gone, err)
	}
}
