package cli

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/hook"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/sandbox"
	"github.com/andreujuanc/atelier/internal/store"
)

// TestInsideTaskContainer: in a task container, note and notes go to the
// task's gate, every command that would need atelier's database (or tmux,
// or the Docker daemon) is refused with one line, and atl never creates or
// opens its data, state or config dir there.
func TestInsideTaskContainer(t *testing.T) {
	root := setup(t)
	// Where atl runs: task "boxed" (and "other") in a database the
	// container does not have, served by boxed's gate.
	box := t.TempDir()
	p := config.Paths{Repos: filepath.Join(box, "repos"), Tasks: filepath.Join(box, "tasks"),
		DB: filepath.Join(box, "data", "atelier.db"), StateDir: filepath.Join(box, "state"), ConfigDir: filepath.Join(box, "config")}
	st, err := store.Open(p.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	var boxed, other model.Task
	for _, tk := range []*model.Task{&boxed, &other} {
		slug := "boxed"
		if tk == &other {
			slug = "other"
		}
		*tk = model.Task{Slug: slug, Title: slug, Status: model.StatusActive, Branch: "atl/" + slug}
		if err := st.CreateTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AppendNote(ctx, other.ID, model.AuthorUser, "theirs"); err != nil {
		t.Fatal(err)
	}
	ts, err := hook.OpenTaskServer(p, boxed.ID, "boxed")
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	dir, err := os.MkdirTemp("", "gate")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "atl.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go (&sandbox.Control{Slug: "boxed", TaskID: boxed.ID, Atl: "/bin/false", Task: ts}).Serve(l)
	t.Setenv(sandbox.InEnv, "boxed-abc123")
	t.Setenv("ATL_TASK", "boxed")
	t.Setenv("ATL_CONTROL_SOCK", sock)

	notes := func(id int64) string {
		s, err := st.GetNotes(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if code, out, errs := atl(t, "note", "boxed", "from", "inside", "--author", "assistant"); code != 0 || out != "note added\n" {
		t.Errorf("note: %d %q %s", code, out, errs)
	}
	if !strings.HasSuffix(notes(boxed.ID), "[agent] from inside") { // the gate writes as the agent
		t.Errorf("boxed notes = %q", notes(boxed.ID))
	}
	if code, out, errs := atl(t, "notes", "boxed"); code != 0 || !strings.HasSuffix(out, "[agent] from inside\n") {
		t.Errorf("notes: %d %q %s", code, out, errs)
	}
	var out, errb bytes.Buffer
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if code := runIn(cctx, []string{"notes", "boxed", "--set"}, strings.NewReader("all new\n"), &out, &errb); code != 0 ||
		out.String() != "notes replaced (7 bytes)\n" || notes(boxed.ID) != "all new" {
		t.Errorf("notes --set: %d %q %s; notes %q", code, out.String(), errb.String(), notes(boxed.ID))
	}
	if code, _, errs := atl(t, "notes", "boxed", "--set"); code != 1 || !strings.Contains(errs, "stdin is empty") || notes(boxed.ID) != "all new" {
		t.Errorf("notes --set with empty stdin: %d %s", code, errs)
	}
	if code, out, errs := atl(t, "notes", "boxed", "--clear"); code != 0 || out != "notes cleared\n" || notes(boxed.ID) != "" {
		t.Errorf("notes --clear: %d %q %s", code, out, errs)
	}
	// Only the container's own task.
	for _, args := range [][]string{{"note", "other", "x"}, {"notes", "other"}, {"notes", "other", "--clear"}, {"note", "1", "x"}} {
		if code, _, errs := atl(t, args...); code != 2 || !strings.Contains(errs, "this task (boxed) only") {
			t.Errorf("%q: %d %s", args, code, errs)
		}
	}
	if notes(other.ID) != "- "+time.Now().Format("2006-01-02")+" [user] theirs" {
		t.Errorf("other's notes = %q", notes(other.ID))
	}

	// Everything else is refused with one line.
	for _, args := range [][]string{
		{}, {"ls"}, {"show", "boxed"}, {"new", "x"}, {"add", "boxed", "api"}, {"session", "boxed"}, {"open", "boxed"},
		{"resume", "boxed"}, {"status", "boxed", "done"}, {"rename", "boxed", "y"}, {"diff", "boxed"}, {"pr", "boxed"},
		{"done", "boxed"}, {"cancel", "boxed"}, {"doctor"}, {"transcript", "boxed"}, {"repos"}, {"assistant"},
		{"gate", "boxed", "--task-id", "1"}, {"token"}, {"token", "--paste"},
	} {
		code, out, errs := atl(t, args...)
		if code != 1 || out != "" || !strings.Contains(errs, "does not work inside a task container") ||
			strings.Count(strings.TrimSpace(errs), "\n") != 0 {
			t.Errorf("atl %q: %d %q %q", args, code, out, errs)
		}
	}
	if code, out, _ := atl(t, "help", "note"); code != 0 || !strings.Contains(out, "notes") {
		t.Errorf("help: %d %q", code, out)
	}

	for _, d := range []string{"data", "state", "config"} {
		if _, err := os.Stat(filepath.Join(root, d)); !os.IsNotExist(err) {
			t.Errorf("atl touched its %s dir in a task container: %v", d, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".local")); !os.IsNotExist(err) {
		t.Errorf("atl created ~/.local in a task container: %v", err)
	}

	// Without a gate: an error that says so, still no database.
	t.Setenv("ATL_CONTROL_SOCK", filepath.Join(dir, "none.sock"))
	if code, _, errs := atl(t, "note", "boxed", "x"); code != 1 || !strings.Contains(errs, "gate") {
		t.Errorf("note without a gate: %d %s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(root, "data")); !os.IsNotExist(err) {
		t.Errorf("atl touched its data dir in a task container: %v", err)
	}
}

// TestInsideAssistantContainer: in the assistant's container atl sends its
// command line, as it is, to the assistant's gate, which runs atl in its
// workspace where atl runs; stdin goes along for notes --set only, output
// and exit status come back, and what the gate refuses (atl open, -w,
// --force) fails with its reason. hook and statusline stay local.
func TestInsideAssistantContainer(t *testing.T) {
	root := setup(t)
	dir, err := os.MkdirTemp("", "gate")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	stand := filepath.Join(dir, "atl")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + filepath.Join(dir, "argv") + "\ncat > " + filepath.Join(dir, "stdin") +
		"\necho \"ran $3\"\necho warn >&2\nexit 4\n"
	if err := os.WriteFile(stand, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "atl.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go (&sandbox.Control{Slug: "atl_assistant", Atl: stand, Workspace: "/home/you/atelier/workspaces/personal", Resolve: resolveAtl}).Serve(l)
	t.Setenv(sandbox.InEnv, "_assistant-abc123")
	t.Setenv(sandbox.AssistantEnv, "1")
	t.Setenv("ATL_CONTROL_SOCK", sock)
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		os.Remove(filepath.Join(dir, name))
		return string(b)
	}

	var out, errb bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if code := runIn(ctx, []string{"notes", "alpha", "--set"}, strings.NewReader("new notes\n"), &out, &errb); code != 4 ||
		out.String() != "ran notes\n" || errb.String() != "warn\n" {
		t.Errorf("notes --set: %d %q %q", code, out.String(), errb.String())
	}
	if got := read("argv"); got != "-w\n/home/you/atelier/workspaces/personal\nnotes\n--for-assistant\nalpha\n--set\n" {
		t.Errorf("argv %q", got)
	}
	if got := read("stdin"); got != "new notes\n" {
		t.Errorf("stdin %q", got)
	}
	out.Reset()
	errb.Reset()
	// Other commands send no stdin (it might never end).
	if code := runIn(ctx, []string{"ls", "--json"}, strings.NewReader("not for ls"), &out, &errb); code != 4 || out.String() != "ran ls\n" {
		t.Errorf("ls: %d %q %q", code, out.String(), errb.String())
	}
	if got := read("stdin"); got != "" {
		t.Errorf("stdin of ls %q", got)
	}
	read("argv")
	// As the gate reads the line: notes --set --help only prints help, and
	// reads no stdin, so none is sent (the gate would refuse it).
	out.Reset()
	errb.Reset()
	if code := runIn(ctx, []string{"notes", "alpha", "--set", "--help"}, strings.NewReader("not read"), &out, &errb); code != 4 || out.String() != "ran notes\n" {
		t.Errorf("notes --set --help: %d %q %q", code, out.String(), errb.String())
	}
	if got := read("stdin"); got != "" {
		t.Errorf("stdin of notes --set --help %q", got)
	}
	read("argv")

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"open", "alpha"}, "select the task and press Enter"},
		{[]string{"-w", "beta", "ls"}, "--workspace"},
		{[]string{"done", "alpha", "--force"}, "--force"},
		{[]string{"cancel", "alpha", "-f"}, "--force"},
		{[]string{"token"}, "does not run for the assistant"},
		{[]string{}, "TUI"},
	} {
		code, out, errs := atl(t, c.args...)
		if code != 2 || out != "" || !strings.Contains(errs, c.want) {
			t.Errorf("%q: %d %q %q", c.args, code, out, errs)
		}
		if got := read("argv"); got != "" {
			t.Errorf("%q: atl ran with %q", c.args, got)
		}
	}
	for _, d := range []string{"data", "state", "config"} {
		if _, err := os.Stat(filepath.Join(root, d)); !os.IsNotExist(err) {
			t.Errorf("atl touched its %s dir in the assistant's container: %v", d, err)
		}
	}

	// Without its gate: one line that says so.
	t.Setenv("ATL_CONTROL_SOCK", filepath.Join(dir, "none.sock"))
	if code, _, errs := atl(t, "ls"); code != 1 || !strings.Contains(errs, "gate") || strings.Count(strings.TrimSpace(errs), "\n") != 0 {
		t.Errorf("without a gate: %d %q", code, errs)
	}
}

// TestResolveAtl: the assistant's gate reads a command line as cobra will
// run it. A leading help flag takes the next argument as its value, so
// `--help --help=false <cmd>` runs <cmd>: resolved, such lines name the
// command that would run, which is off the assistant's list (the string
// check refuses them as well, for their leading flag).
func TestResolveAtl(t *testing.T) {
	ws := []string{"-w", "/home/you/atelier/workspaces/personal"}
	for _, c := range []struct {
		args  []string
		path  string
		stdin bool
		ok    bool // the gate runs it
	}{
		{[]string{"ls", "--json"}, "atl ls", false, true},
		{[]string{"notes", "alpha", "--set"}, "atl notes", true, true},
		{[]string{"notes", "--set=true", "alpha"}, "atl notes", true, true},
		{[]string{"notes", "alpha", "--set=false"}, "atl notes", false, true},
		{[]string{"notes", "alpha", "--set", "--help"}, "atl notes", false, true},
		{[]string{"notes", "alpha"}, "atl notes", false, true},
		{[]string{"help", "token"}, "atl help", false, true},
		{[]string{"repos"}, "atl repos", false, true},
		{[]string{"repos", "pin", "api"}, "atl repos pin", false, false},
		{[]string{"--help", "--help=false", "token", "--paste"}, "atl token", false, false},
		{[]string{"--help", "--help=false", "workspace", "new", "x", "--image", "evil"}, "atl workspace new", false, false},
		{[]string{"-h", "-h=false", "push", "alpha"}, "atl push", false, false},
		{[]string{"--help=true", "--help=false", "open", "alpha"}, "atl open", false, false},
		{[]string{"--help", "--help=false", "gate", "--assistant"}, "atl gate", false, false},
		{[]string{"--help", "x", "--help", "--help=false", "repos", "pin", "api"}, "atl repos pin", false, false},
	} {
		path, stdin, err := resolveAtl(append(append([]string{}, ws...), c.args...))
		if err != nil || path != c.path || stdin != c.stdin {
			t.Errorf("%q: %q %v %v, want %q %v", c.args, path, stdin, err, c.path, c.stdin)
		}
		allowed := sandbox.AssistantCommandPath(path) && sandbox.CheckAssistantArgs(c.args) == nil
		if allowed != c.ok {
			t.Errorf("%q: allowed %v", c.args, allowed)
		}
	}
	if _, _, err := resolveAtl(append(append([]string{}, ws...), "nosuch")); err == nil {
		t.Error("an unknown command resolved")
	}
}
