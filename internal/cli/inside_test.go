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
	if !strings.HasSuffix(notes(boxed.ID), "[assistant] from inside") {
		t.Errorf("boxed notes = %q", notes(boxed.ID))
	}
	if code, out, errs := atl(t, "notes", "boxed"); code != 0 || !strings.HasSuffix(out, "[assistant] from inside\n") {
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
