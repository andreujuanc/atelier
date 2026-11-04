package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/store"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cli-test-git")
	if err != nil {
		panic(err)
	}
	cfg := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(cfg, nil, 0o644); err != nil {
		panic(err)
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL":   cfg,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME":     "Test",
		"GIT_AUTHOR_EMAIL":    "test@example.com",
		"GIT_COMMITTER_NAME":  "Test",
		"GIT_COMMITTER_EMAIL": "test@example.com",
	} {
		os.Setenv(k, v)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// setup points every atelier path at a temp dir and creates repo "api"
// (bare origin with default branch main, canonical clone in $ATL_REPOS).
func setup(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	sock := fmt.Sprintf("atlcli-%d", os.Getpid())
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", sock, "kill-server").Run() })
	for k, v := range map[string]string{
		"HOME":            root,
		"TMUX":            "",
		"ATL_REPOS":       filepath.Join(root, "repos"),
		"ATL_TASKS":       filepath.Join(root, "tasks"),
		"XDG_DATA_HOME":   filepath.Join(root, "data"),
		"XDG_STATE_HOME":  filepath.Join(root, "state"),
		"XDG_CONFIG_HOME": filepath.Join(root, "config"),
		"ATL_BIN":         "/usr/local/bin/atl",
		"ATL_TMUX_SOCKET": sock,
	} {
		t.Setenv(k, v)
	}
	origin, seed := filepath.Join(root, "origin.git"), filepath.Join(root, "seed")
	for _, d := range []string{origin, seed, filepath.Join(root, "repos")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	git(t, origin, "init", "-q", "--bare", "-b", "main")
	git(t, seed, "init", "-q", "-b", "main")
	git(t, seed, "commit", "-q", "--allow-empty", "-m", "init")
	git(t, seed, "push", "-q", origin, "main")
	git(t, filepath.Join(root, "repos"), "clone", "-q", origin, "api")
	return root
}

func atl(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	code := run(ctx, args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestFlow(t *testing.T) {
	root := setup(t)

	code, out, errs := atl(t, "new", "Fix", "the", "login", "--repo", "api", "-m", "why", "--no-fetch")
	if code != 0 {
		t.Fatalf("new: %d %s", code, errs)
	}
	dir := filepath.Join(root, "tasks", "fix-the-login")
	if out != "fix-the-login\t"+dir+"\n" {
		t.Fatalf("new output %q", out)
	}
	// Interspersed flags after the title.
	if code, _, errs := atl(t, "new", "second", "--backlog", "--repo=api:rw"); code != 0 {
		t.Fatalf("new 2: %d %s", code, errs)
	}

	code, out, _ = atl(t, "ls")
	if code != 0 {
		t.Fatal(code)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "ID") || !strings.Contains(lines[0], "SESSIONS") ||
		!strings.Contains(out, "fix-the-login") || !strings.Contains(out, "Fix the login") {
		t.Fatalf("ls:\n%s", out)
	}
	if f := strings.Fields(lines[1]); f[1] != "second" || f[2] != "backlog" || f[3] != "-" || f[4] != "-" {
		t.Fatalf("ls row %q", lines[1])
	}

	code, out, _ = atl(t, "ls", "--json", "--status", "active")
	if code != 0 {
		t.Fatal(code)
	}
	var tasks []map[string]any
	if err := json.Unmarshal([]byte(out), &tasks); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks %v", tasks)
	}
	tk := tasks[0]
	if tk["slug"] != "fix-the-login" || tk["description"] != "why" || tk["branch"] != "atl/fix-the-login" ||
		tk["task_dir"] != dir || tk["port_offset"] != float64(0) || tk["status"] != "active" {
		t.Fatalf("task json %v", tk)
	}
	repos := tk["repos"].([]any)
	if r := repos[0].(map[string]any); r["name"] != "api" || r["mode"] != "rw" || r["path"] != filepath.Join(dir, "api") {
		t.Fatalf("repo json %v", r)
	}
	if _, err := time.Parse(time.RFC3339, tk["created_at"].(string)); err != nil {
		t.Fatal(err)
	}
	if s, ok := tk["sessions"].([]any); !ok || len(s) != 0 {
		t.Fatalf("sessions %v", tk["sessions"])
	}

	if code, _, errs := atl(t, "status", "fix-the", "review"); code != 0 {
		t.Fatalf("status: %d %s", code, errs)
	}
	if code, _, errs := atl(t, "status", "fix-the-login", "bogus"); code != 2 || !strings.Contains(errs, "atl: ") {
		t.Fatalf("bad status: %d %s", code, errs)
	}
	if code, _, _ := atl(t, "status", "fix-the-login"); code != 2 {
		t.Fatalf("missing arg: %d", code)
	}
	if code, _, _ := atl(t, "ls", "--status", "nope"); code != 2 {
		t.Fatalf("bad --status: %d", code)
	}
	if code, _, _ := atl(t, "new", "x", "--repo", "api:rx"); code != 2 {
		t.Fatalf("bad --repo mode: %d", code)
	}
	if code, _, _ := atl(t, "frobnicate"); code != 2 {
		t.Fatalf("unknown command: %d", code)
	}
	if code, _, errs := atl(t, "done", "nope"); code != 1 || !strings.HasPrefix(errs, "atl: ") {
		t.Fatalf("done unknown: %d %s", code, errs)
	}

	if code, out, errs := atl(t, "done", "fix-the-login"); code != 0 || out != "fix-the-login\tdone\n" {
		t.Fatalf("done: %d %q %s", code, out, errs)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("task dir still there: %v", err)
	}
	if code, _, errs := atl(t, "done", "fix-the-login"); code != 1 || !strings.Contains(errs, "already done") {
		t.Fatalf("done twice: %d %s", code, errs)
	}

	_, out, _ = atl(t, "ls")
	if strings.Contains(out, "fix-the-login") {
		t.Fatalf("done task in default ls:\n%s", out)
	}
	_, out, _ = atl(t, "ls", "--status", "all")
	if !strings.Contains(out, "fix-the-login") || !strings.Contains(out, "second") {
		t.Fatalf("ls --status all:\n%s", out)
	}
	_, out, _ = atl(t, "ls", "--status", "done,cancelled", "--json")
	if err := json.Unmarshal([]byte(out), &tasks); err != nil || len(tasks) != 1 || tasks[0]["status"] != "done" {
		t.Fatalf("done list: %v %s", err, out)
	}

	if code, out, _ := atl(t, "cancel", "second"); code != 0 || out != "second\tcancelled\n" {
		t.Fatalf("cancel: %d %q", code, out)
	}
	_, out, _ = atl(t, "ls", "--json")
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty ls --json: %q", out)
	}
}

func TestRootAndDoctor(t *testing.T) {
	setup(t)
	code, out, _ := atl(t)
	if code != 0 || !strings.Contains(out, "Usage:") {
		t.Fatalf("root: %d %s", code, out)
	}
	code, out, _ = atl(t, "doctor")
	if !strings.Contains(out, "repos dir") || !strings.Contains(out, "database") {
		t.Fatalf("doctor: %d %s", code, out)
	}
	if strings.Contains(out, "FAIL") != (code == 1) {
		t.Fatalf("doctor exit %d with output\n%s", code, out)
	}
}

func TestSessionsSummary(t *testing.T) {
	ss := func(states ...model.SessionState) []model.Session {
		var out []model.Session
		for _, st := range states {
			out = append(out, model.Session{State: st})
		}
		return out
	}
	for want, in := range map[string][]model.Session{
		"-":                        ss(model.StateEnded),
		"1":                        ss(model.StateIdle, model.StateEnded),
		"2 (1 waiting)":            ss(model.StateRunning, model.StateWaiting),
		"3 (1 waiting, 1 stopped)": ss(model.StateIdle, model.StateWaiting, model.StateStopped),
	} {
		if got := sessionsSummary(in); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if got := sessionsSummary(nil); got != "-" {
		t.Fatal(got)
	}
}

func TestPRExitCode(t *testing.T) {
	root := setup(t)
	bin := t.TempDir()
	stub := "#!/bin/sh\necho 'boom' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	if code, _, errs := atl(t, "new", "ship", "--repo", "api", "--no-fetch"); code != 0 {
		t.Fatalf("new: %d %s", code, errs)
	}
	wt := filepath.Join(root, "tasks", "ship", "api")

	// Skips are not failures.
	if code, out, errs := atl(t, "pr", "ship"); code != 0 || out != "api\tskipped: no commits\n" {
		t.Fatalf("pr (no commits): %d %q %s", code, out, errs)
	}
	git(t, wt, "commit", "-q", "--allow-empty", "-m", "work")
	if code, out, errs := atl(t, "pr", "ship"); code != 0 || out != "api\tskipped: push atl/ship first\n" {
		t.Fatalf("pr (unpushed): %d %q %s", code, out, errs)
	}

	// gh failing is.
	git(t, wt, "push", "-q", "origin", "atl/ship")
	if code, out, errs := atl(t, "pr", "ship"); code != 1 || out != "api\tskipped: gh: boom\n" || errs != "" {
		t.Fatalf("pr (gh fails): %d %q %q", code, out, errs)
	}
}

// TestStartupRecover checks that commands run Recover once at start: a live
// session whose tmux window is gone (here: no server at all) is reported
// stopped by `atl ls`, without a warning.
func TestStartupRecover(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	setup(t)
	if code, _, errs := atl(t, "new", "rec", "--repo", "api", "--no-fetch"); code != 0 {
		t.Fatalf("new: %d %s", code, errs)
	}
	st, err := store.Open(filepath.Join(os.Getenv("XDG_DATA_HOME"), "atelier", "atelier.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	tk, err := st.ResolveTask(ctx, "rec")
	if err != nil {
		t.Fatal(err)
	}
	s := &model.Session{TaskID: tk.ID, ClaudeSessionID: "cid", TmuxTarget: "@5", Name: "claude-x", State: model.StateRunning}
	if err := st.CreateSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	code, out, errs := atl(t, "ls", "--json")
	if code != 0 || errs != "" {
		t.Fatalf("ls: %d %q", code, errs)
	}
	var tasks []struct {
		Sessions []struct {
			State string `json:"state"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(out), &tasks); err != nil || len(tasks) != 1 || len(tasks[0].Sessions) != 1 {
		t.Fatalf("ls json: %v %s", err, out)
	}
	if got := tasks[0].Sessions[0].State; got != "stopped" {
		t.Fatalf("session state after startup recover = %s, want stopped", got)
	}
}
