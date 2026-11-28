package sandbox

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
)

func TestCheckAssistantArgs(t *testing.T) {
	for _, args := range [][]string{
		{"ls"}, {"ls", "--status", "all", "--json"}, {"show", "alpha", "--json"}, {"show", "alpha", "--notes"},
		{"transcript", "alpha", "--tail", "30"}, {"repos"}, {"repos", "--help"}, {"diff", "alpha"},
		{"new", "Fix", "login", "--project", "shop", "--repo", "api:rw", "-m", "why"}, {"new", "x", "-p", "shop"},
		{"add", "alpha", "docs:ro"}, {"status", "alpha", "review"}, {"session", "alpha"}, {"resume", "alpha"},
		{"pr", "alpha"}, {"note", "alpha", "waiting", "on", "review", "--author", "assistant"},
		{"note", "alpha", "x", "--author=assistant"}, {"note", "--author", "assistant", "alpha", "x"},
		{"notes", "alpha"}, {"notes", "alpha", "--set"}, {"notes", "alpha", "--clear"}, {"rename", "alpha", "New", "title"},
		{"doctor"}, {"help"}, {"help", "open"}, {"help", "done"}, {"ls", "-h"}, {"ls", "--help"}, {"status", "alpha", "backlog"},
		// Learnings: read only.
		{"learnings"}, {"learnings", "--json"}, {"learnings", "--all", "--json"}, {"learnings", "ls"}, {"learnings", "ls", "--all", "--json"},
		{"learnings", "--json", "ls"}, {"learnings", "show", "3"}, {"learnings", "show", "3", "--json"}, {"learnings", "show", "--json", "3"},
		{"learnings", "--help"}, {"help", "learnings"},
	} {
		if err := CheckAssistantArgs(args); err != nil {
			t.Errorf("%q refused: %v", args, err)
		}
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, "TUI"},
		{[]string{"open", "alpha"}, "select the task and press Enter"},
		// Closing a task removes its worktrees: the user's.
		{[]string{"done", "alpha"}, "removes the task's worktrees"},
		{[]string{"cancel", "alpha"}, "removes the task's worktrees"},
		{[]string{"done", "--help"}, "removes the task's worktrees"},
		// So is closing it with atl status; reopening it is refused by atl
		// itself (the gate's --for-assistant).
		{[]string{"status", "alpha", "done"}, "closing a task is the user's"},
		{[]string{"status", "alpha", "cancelled"}, "closing a task is the user's"},
		{[]string{"status", "alpha", "active", "--for-assistant=false"}, "the gate sets it"},
		{[]string{"ls", "--for-assistant"}, "the gate sets it"},
		{[]string{"assistant"}, "does not run for the assistant"},
		{[]string{"token"}, "does not run for the assistant"},
		{[]string{"token", "--paste"}, "does not run for the assistant"},
		{[]string{"learn", "--dry-run"}, "does not run for the assistant"},
		// Deciding on learnings is the user's: in every spelling.
		{[]string{"learnings", "accept", "1"}, "changes a learning"},
		{[]string{"learnings", "reject", "1"}, "changes a learning"},
		{[]string{"learnings", "edit", "1"}, "changes a learning"},
		{[]string{"learnings", "rm", "1"}, "changes a learning"},
		{[]string{"learnings", "", "accept", "1"}, "changes a learning"},
		{[]string{"learnings", "--json", "accept", "1"}, "changes a learning"},
		{[]string{"learnings", "--all=true", "reject", "1"}, "changes a learning"},
		{[]string{"learnings", "accept", "--help=false", "1"}, "changes a learning"},
		{[]string{"learnings", "--help", "--help=false", "rm", "1"}, "changes a learning"},
		{[]string{"learnings", "-h", "edit", "1"}, "changes a learning"},
		{[]string{"learnings", "-h", "-h=false", "edit", "1"}, "is not accepted"}, // refused as -f
		{[]string{"learnings", "--", "accept", "1"}, "changes a learning"},
		{[]string{"learnings", "ls", "accept", "1"}, "changes a learning"},
		{[]string{"learnings", "show", "rm"}, "changes a learning"},
		{[]string{"learnings", "accept"}, "changes a learning"},
		{[]string{"learnings", "accept", "--help"}, "changes a learning"},
		{[]string{"learnings", ""}, "does not run for the assistant"},
		{[]string{"learnings", "1"}, "does not run for the assistant"},
		{[]string{"learnings", "ACCEPT", "1"}, "does not run for the assistant"},
		{[]string{"learnings", "decide", "1"}, "does not run for the assistant"},
		{[]string{"learnings accept", "1"}, "does not run for the assistant"},
		{[]string{"learnings", "show", "1", "-w", "beta"}, "--workspace"},
		{[]string{"--help", "--help=false", "learnings", "accept", "1"}, "not accepted"},
		{[]string{"gate", "--assistant"}, "does not run for the assistant"},
		{[]string{"hook", "Stop"}, "does not run for the assistant"},
		{[]string{"workspace", "new", "x"}, "does not run for the assistant"},
		{[]string{"statusline"}, "does not run for the assistant"},
		{[]string{"push", "alpha"}, "does not run for the assistant"},
		{[]string{"fetch", "alpha"}, "does not run for the assistant"},
		{[]string{"completion", "bash"}, "does not run for the assistant"},
		{[]string{"LS"}, "does not run for the assistant"},
		{[]string{"repos", "pin", "api"}, "re-pinning"},
		{[]string{"--json", "ls"}, "before the command"},
		// A leading flag: cobra takes the next argument as its value and
		// runs what follows.
		{[]string{"--help"}, "not accepted"},
		{[]string{"-h"}, "not accepted"},
		{[]string{"--help", "--help=false", "token", "--paste"}, "not accepted"},
		{[]string{"--help", "--help=false", "repos", "pin", "api"}, "not accepted"},
		{[]string{"-h", "-h=false", "push", "alpha"}, "not accepted"},
		{[]string{"--help=true", "--help=false", "open", "alpha"}, "not accepted"},
		{[]string{"--json", "--help", "--help=false", "gate", "--assistant"}, "not accepted"},
		// The workspace is the gate's.
		{[]string{"-w", "beta", "ls"}, "--workspace"},
		{[]string{"ls", "-w", "beta"}, "--workspace"},
		{[]string{"ls", "-wbeta"}, "--workspace"},
		{[]string{"ls", "--workspace", "beta"}, "--workspace"},
		{[]string{"ls", "--workspace=beta"}, "--workspace"},
		{[]string{"--workspace=/home/you/atelier/workspaces/beta", "ls"}, "--workspace"},
		{[]string{"note", "alpha", "x", "--", "-w", "beta"}, "--workspace"},
		{[]string{"new", "x", "-m", "--", "-w", "beta"}, "--workspace"},
		{[]string{"done", "alpha", "-hw", "beta"}, "--workspace"},
		// --force discards uncommitted work.
		{[]string{"done", "alpha", "--force"}, "--force"},
		{[]string{"done", "--force", "alpha"}, "--force"},
		{[]string{"done", "alpha", "--force=true"}, "--force"},
		{[]string{"done", "alpha", "--force=false"}, "--force"},
		{[]string{"done", "alpha", "-f"}, "--force"},
		{[]string{"cancel", "alpha", "-f"}, "--force"},
		{[]string{"cancel", "alpha", "-hf"}, "--force"},
		{[]string{"cancel", "-f", "alpha"}, "--force"},
		{[]string{"cancel", "alpha", "--force"}, "--force"},
		{[]string{"note", "alpha", "a\x00b"}, "not valid text"},
		// The assistant's notes are its own.
		{[]string{"note", "alpha", "x", "--author", "user"}, "--author user"},
		{[]string{"note", "alpha", "x", "--author=user"}, "--author user"},
		{[]string{"note", "--author", "user", "alpha", "x"}, "--author user"},
		{[]string{"note", "alpha", "x", "--author", "assistant", "--author", "user"}, "--author user"},
		{[]string{"note", "alpha", "x", "--author=Assistant"}, "--author Assistant"},
		{[]string{"note", "alpha", "x", "--author"}, "is not accepted"},
		{[]string{"note", "alpha", "x"}, "needs --author assistant"},
		{[]string{"note", "alpha", "--author", "x"}, "--author x"},
	} {
		err := CheckAssistantArgs(c.args)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v, want a refusal with %q", c.args, err, c.want)
		}
	}
	if err := CheckAssistantArgs(make([]string, MaxAssistantArgs+1)); err == nil {
		t.Error("too many arguments accepted")
	}
}

// TestAssistantID: the assistant's sandbox has a name of atl's own (a task
// cannot create or rename a container to it, see ReservedName), which no
// task's can have: slugs never start with "_".
func TestAssistantID(t *testing.T) {
	id := AssistantID("/home/you/atelier/workspaces/personal/data/assistant")
	if !strings.HasPrefix(id, "_assistant-") || len(id) != len("_assistant-")+6 {
		t.Errorf("AssistantID = %q", id)
	}
	if id == AssistantID("/home/you/atelier/workspaces/work/data/assistant") {
		t.Error("two workspaces' assistants share a sandbox ID")
	}
	if !ReservedName(ContainerName(id)) || !ReservedName("/"+ContainerName(id)) {
		t.Errorf("%s is not reserved for atl", ContainerName(id))
	}
	for _, title := range []string{"_assistant", "_assistant-abcdef", "__", " _x", "-_assistant", "assistant", "_"} {
		if slug := model.Slugify(title); strings.HasPrefix(slug, "_") || ID(slug, "/t/"+slug) == id {
			t.Errorf("title %q: slug %q", title, slug)
		}
	}
}

// assistantLab serves the assistant's Control (Workspace set) on a temp
// socket, with a stand-in atl that records its argv, its stdin and the
// variables it saw, prints to stdout and stderr and exits with the status
// in the file "exit"; while the file "hold" exists it marks itself running
// (a file running.<pid>) and waits for the file "release". resolve is the
// Control's Resolve.
func assistantLab(t *testing.T, workspace string, resolve func([]string) (string, bool, error)) (dir, sock string, logs *bytes.Buffer) {
	t.Helper()
	dir = t.TempDir()
	atl := filepath.Join(dir, "atl")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + filepath.Join(dir, "argv") + "\n" +
		"cat > " + filepath.Join(dir, "stdin") + "\n" +
		"printf '%s %s' \"${" + InEnv + "-unset}\" \"${" + AssistantEnv + "-unset}\" > " + filepath.Join(dir, "env") + "\n" +
		"if [ -f " + filepath.Join(dir, "hold") + " ]; then : > " + filepath.Join(dir, "running") + ".$$; while [ ! -f " + filepath.Join(dir, "release") + " ]; do sleep 0.05; done; fi\n" +
		"[ -f " + filepath.Join(dir, "big") + " ] && head -c 4194304 /dev/zero | tr '\\0' x\n" +
		"echo out\n" +
		"echo err >&2\n" +
		"[ -f " + filepath.Join(dir, "exit") + " ] && exit $(cat " + filepath.Join(dir, "exit") + ")\n" +
		"exit 0\n"
	if err := os.WriteFile(atl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sockDir, err := os.MkdirTemp("", "gate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock = filepath.Join(sockDir, "atl.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	logs = &bytes.Buffer{}
	go (&Control{Slug: "atl_assistant", Atl: atl, Workspace: workspace, Log: log.New(logs, "", 0), Resolve: resolve, WriteTimeout: 300 * time.Millisecond}).Serve(l)
	return dir, sock, logs
}

// TestControlAtlForTheAssistant: the assistant's gate runs an allowed atl
// command line in its own workspace, by exec'ing atl with -w, with the
// gate's environment, the body as stdin, stdout and stderr apart and the
// exit status passed back; it refuses the others without running anything,
// and logs the command and status but never stdin.
func TestControlAtlForTheAssistant(t *testing.T) {
	dir, sock, logs := assistantLab(t, "/home/you/atelier/workspaces/personal", fakeResolve)
	t.Setenv(InEnv, "should-not-reach-atl")
	t.Setenv(AssistantEnv, "1")
	ask := func(args []string, stdin string) (int, bool, string, string) {
		t.Helper()
		var out, errb bytes.Buffer
		code, ran, err := Ask(sock, ControlRequest{Op: OpAtl, Args: args}, []byte(stdin), &out, &errb, 0)
		if err != nil {
			t.Fatalf("%q: %v", args, err)
		}
		return code, ran, out.String(), errb.String()
	}
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		os.Remove(filepath.Join(dir, name))
		return string(b)
	}

	code, ran, out, errs := ask([]string{"notes", "alpha", "--set"}, "the new notes\nsecret-ish text\n")
	if code != 0 || !ran || out != "out\n" || errs != "err\n" {
		t.Errorf("notes --set: %d %v %q %q", code, ran, out, errs)
	}
	if got := read("argv"); got != "-w\n/home/you/atelier/workspaces/personal\nnotes\n--for-assistant\nalpha\n--set\n" {
		t.Errorf("argv %q", got)
	}
	if got := read("stdin"); got != "the new notes\nsecret-ish text\n" {
		t.Errorf("stdin %q", got)
	}
	if got := read("env"); got != "unset unset" {
		t.Errorf("atl saw %s and %s: %q; it runs where atl runs", InEnv, AssistantEnv, got)
	}

	// The exit status comes back.
	if err := os.WriteFile(filepath.Join(dir, "exit"), []byte("3"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, ran, out, _ := ask([]string{"pr", "alpha"}, ""); code != 3 || !ran || out != "out\n" {
		t.Errorf("failing pr: %d %v %q", code, ran, out)
	}
	if got := read("stdin"); got != "" {
		t.Errorf("stdin without a body: %q", got)
	}
	read("argv")
	os.Remove(filepath.Join(dir, "exit"))

	// Refused: nothing runs, exit 2, the reason on stderr.
	for _, args := range [][]string{{"open", "alpha"}, {"token"}, {"ls", "-w", "beta"}, {"ls", "--workspace=beta"},
		{"done", "alpha", "--force"}, {"cancel", "alpha", "-f"}, {"gate", "--assistant"}, {}} {
		code, ran, out, errs := ask(args, "")
		if code != 2 || ran || out != "" || !strings.HasPrefix(errs, "atelier gate: ") {
			t.Errorf("%q: %d %v %q %q", args, code, ran, out, errs)
		}
		if got, err := os.ReadFile(filepath.Join(dir, "argv")); err == nil {
			t.Errorf("%q: atl ran with %q", args, got)
		}
	}
	if _, _, _, errs := ask([]string{"open", "alpha"}, ""); !strings.Contains(errs, "select the task and press Enter, or run `atl open <task>` in a terminal") {
		t.Errorf("open: %q", errs)
	}

	l := logs.String()
	for _, want := range []string{"run   atl notes (for the assistant)", "done  atl notes (for the assistant): exit 0",
		"done  atl pr (for the assistant): exit 3", "deny  atl request: --force"} {
		if !strings.Contains(l, want) {
			t.Errorf("log lacks %q:\n%s", want, l)
		}
	}
	if strings.Contains(l, "secret-ish") || strings.Contains(l, "alpha") {
		t.Errorf("log has stdin or arguments:\n%s", l)
	}
}

// TestTaskGateRefusesAtl: a task's gate (no Workspace) never runs an atl
// command line, whatever the request says.
func TestTaskGateRefusesAtl(t *testing.T) {
	lab := newControlLab(t)
	for _, args := range [][]string{{"ls"}, {"notes", "t1"}, {"--help"}} {
		code, out := lab.ask(t, ControlRequest{Op: OpAtl, Args: args})
		if code != exitRefused || !strings.Contains(out, "atl is not served by this gate") {
			t.Errorf("%q: exit %d, output %q", args, code, out)
		}
		if got, ran := lab.argv(t); ran {
			t.Errorf("%q: atl ran with %q", args, got)
		}
	}
}

// TestControlAtlOutputBound: what an atl command prints for the assistant
// stops at the bound, and the gate says it cut it.
func TestControlAtlOutputBound(t *testing.T) {
	var out, errb bytes.Buffer
	lim := &outputLimit{left: 10}
	w1, w2 := lim.writer(&out), lim.writer(&errb)
	w1.Write([]byte("123456"))
	w2.Write([]byte("abcdef"))
	if n, err := w1.Write([]byte("more")); n != 4 || err != nil {
		t.Errorf("write past the bound: %d %v", n, err)
	}
	if out.String() != "123456" || errb.String() != "abcd" || !lim.cut() {
		t.Errorf("out %q, err %q, cut %v", out.String(), errb.String(), lim.cut())
	}
}

// TestNoDockerGate: the assistant's gate refuses every Docker request.
func TestNoDockerGate(t *testing.T) {
	var logs bytes.Buffer
	g := &Gate{ID: AssistantID("/a"), NoDocker: true, Upstream: "/nonexistent.sock", Log: log.New(&logs, "", 0)}
	for _, path := range []string{"/_ping", "/v1.43/version", "/containers/json", "/images/json"} {
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "no Docker access") {
			t.Errorf("GET %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
}

// fakeResolve stands in for atl's command tree (cli.resolveAtl, tested
// there): after "-w <dir>", the first argument is the command; --set reads
// stdin.
func fakeResolve(argv []string) (string, bool, error) {
	if len(argv) < 3 || argv[0] != "-w" {
		return "", false, errors.New("no -w")
	}
	return "atl " + argv[2], slices.Contains(argv, "--set"), nil
}

// TestControlAtlResolved: the gate also checks the command cobra resolves
// the line to, and refuses one off the list even when the string check
// passed (as a spelling it misreads would); a resolver error refuses too,
// and without a resolver nothing runs.
func TestControlAtlResolved(t *testing.T) {
	for _, resolve := range []func([]string) (string, bool, error){
		func([]string) (string, bool, error) { return "atl token", false, nil },
		func([]string) (string, bool, error) { return "atl repos pin", false, nil },
		func([]string) (string, bool, error) { return "atl learnings accept", false, nil },
		func([]string) (string, bool, error) { return "atl learnings rm", false, nil },
		func([]string) (string, bool, error) { return "atl", false, nil },
		func([]string) (string, bool, error) { return "", false, errors.New(`unknown command "x" for "atl"`) },
		nil,
	} {
		dir, sock, _ := assistantLab(t, "/home/you/atelier/workspaces/personal", resolve)
		var out, errb bytes.Buffer
		code, ran, err := Ask(sock, ControlRequest{Op: OpAtl, Args: []string{"ls"}}, nil, &out, &errb, 0)
		if err != nil || code != 2 || ran || !strings.HasPrefix(errb.String(), "atelier gate: ") {
			t.Errorf("%d %v %v %q", code, ran, err, errb.String())
		}
		if _, err := os.Stat(filepath.Join(dir, "argv")); err == nil {
			t.Error("atl ran")
		}
	}
}

// TestControlAtlBody: a body (stdin) goes only with atl notes --set; with
// any other command the request is refused before anything runs.
func TestControlAtlBody(t *testing.T) {
	dir, sock, _ := assistantLab(t, "/home/you/atelier/workspaces/personal", fakeResolve)
	for _, args := range [][]string{{"ls"}, {"notes", "alpha"}, {"notes", "alpha", "--clear"}, {"note", "alpha", "x", "--author", "assistant"}, {"pr", "alpha"}} {
		var out, errb bytes.Buffer
		code, ran, err := Ask(sock, ControlRequest{Op: OpAtl, Args: args}, []byte("text"), &out, &errb, 0)
		if err != nil || code != 2 || ran || !strings.Contains(errb.String(), "standard input is only for atl notes --set") {
			t.Errorf("%q: %d %v %v %q", args, code, ran, err, errb.String())
		}
		if _, err := os.Stat(filepath.Join(dir, "argv")); err == nil {
			t.Errorf("%q: atl ran", args)
		}
	}
}

// TestControlAtlConcurrency: at most maxAtlRunning atl commands run for the
// assistant at once; one more is refused at once, and once one has
// finished the next runs.
func TestControlAtlConcurrency(t *testing.T) {
	dir, sock, _ := assistantLab(t, "/home/you/atelier/workspaces/personal", fakeResolve)
	if err := os.WriteFile(filepath.Join(dir, "hold"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	codes := make(chan int, maxAtlRunning)
	for range maxAtlRunning {
		go func() {
			code, _, _ := Ask(sock, ControlRequest{Op: OpAtl, Args: []string{"ls"}}, nil, io.Discard, io.Discard, 0)
			codes <- code
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		m, _ := filepath.Glob(filepath.Join(dir, "running.*"))
		if len(m) == maxAtlRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d running, want %d", len(m), maxAtlRunning)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var errb bytes.Buffer
	code, ran, err := Ask(sock, ControlRequest{Op: OpAtl, Args: []string{"show", "alpha"}}, nil, io.Discard, &errb, 5*time.Second)
	if err != nil || code != 2 || ran || !strings.Contains(errb.String(), "running already") {
		t.Errorf("one more: %d %v %v %q", code, ran, err, errb.String())
	}
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for range maxAtlRunning {
		if code := <-codes; code != 0 {
			t.Errorf("held command: exit %d", code)
		}
	}
	if code, ran, err := Ask(sock, ControlRequest{Op: OpAtl, Args: []string{"ls"}}, nil, io.Discard, io.Discard, 0); err != nil || code != 0 || !ran {
		t.Errorf("after: %d %v %v", code, ran, err)
	}
}

// TestControlAtlClientNotReading: a client that sends a request and never
// reads the replies does not keep its run slot: each reply write has a
// deadline, after which the rest is dropped, and the slot is free once the
// atl process has exited, before the exit status is written.
func TestControlAtlClientNotReading(t *testing.T) {
	dir, sock, _ := assistantLab(t, "/home/you/atelier/workspaces/personal", fakeResolve)
	for _, name := range []string{"big", "hold"} { // 4 MiB of output, once all are running
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for range maxAtlRunning {
		conn, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		if _, err := conn.Write([]byte(`{"op":"atl","args":["ls"]}` + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	wait := time.Now().Add(10 * time.Second)
	for m, _ := filepath.Glob(filepath.Join(dir, "running.*")); len(m) < maxAtlRunning; m, _ = filepath.Glob(filepath.Join(dir, "running.*")) {
		if time.Now().After(wait) {
			t.Fatalf("%d running", len(m))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		var errb bytes.Buffer
		code, ran, err := Ask(sock, ControlRequest{Op: OpAtl, Args: []string{"ls"}}, nil, io.Discard, &errb, 10*time.Second)
		if err == nil && ran && code == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("slots still held: %d %v %v %q", code, ran, err, errb.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestAssistantCommandPath: the commands cobra may resolve a line to that
// the assistant's gate runs; of learnings only the bare command, ls and
// show.
func TestAssistantCommandPath(t *testing.T) {
	for _, p := range []string{"atl ls", "atl notes", "atl help", "atl learnings", "atl learnings ls", "atl learnings show"} {
		if !AssistantCommandPath(p) {
			t.Errorf("%q refused", p)
		}
	}
	for _, p := range []string{"atl", "atl token", "atl repos pin", "atl learn", "atl learnings accept", "atl learnings reject",
		"atl learnings edit", "atl learnings rm", "atl learnings show x", "atl ls ls", "ls", "atl  ls", "atl workspace new"} {
		if AssistantCommandPath(p) {
			t.Errorf("%q allowed", p)
		}
	}
}
