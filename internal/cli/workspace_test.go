package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andreujuanc/atelier/internal/tmux"
	"github.com/andreujuanc/atelier/internal/workspace"
)

// TestWorkspaces: atl workspace new/list/open, selecting a workspace by
// --workspace, $ATL_WORKSPACE and the current directory, and what atl says
// when none is selected.
func TestWorkspaces(t *testing.T) {
	root := setup(t)
	wsRoot := os.Getenv("ATL_WORKSPACES")
	t.Setenv("ATL_WORKSPACE", "")

	// No workspaces yet.
	if code, _, errs := atl(t, "ls"); code != exitErr || !strings.Contains(errs, "none yet") ||
		!strings.Contains(errs, "atl workspace new <name>") {
		t.Errorf("ls, no workspaces: %d %q", code, errs)
	}
	if code, out, errs := atl(t, "workspace", "list"); code != 0 || out != "" || !strings.Contains(errs, "no workspaces in "+wsRoot) {
		t.Errorf("list, none: %d %q %q", code, out, errs)
	}
	if code, out, _ := atl(t, "workspace", "list", "--json"); code != 0 || strings.TrimSpace(out) != "[]" {
		t.Errorf("list --json, none: %d %q", code, out)
	}

	// new: atelier.toml and the dirs; settings not given come from the
	// environment ($ATL_REPOS here).
	code, out, errs := atl(t, "workspace", "new", "a", "--image", "img-a", "--mount", "~/.npmrc")
	if code != 0 || out != "a\t"+filepath.Join(wsRoot, "a")+"\n" {
		t.Fatalf("new a: %d %q %q", code, out, errs)
	}
	toml, _ := os.ReadFile(filepath.Join(wsRoot, "a", "atelier.toml"))
	for _, want := range []string{`repos = "` + filepath.Join(root, "repos") + `"`, `image = "img-a"`, `mounts = ["~/.npmrc"]`} {
		if !strings.Contains(string(toml), want) {
			t.Errorf("atelier.toml lacks %s:\n%s", want, toml)
		}
	}
	for _, d := range []string{"tasks", "data", "config", "state"} {
		if _, err := os.Stat(filepath.Join(wsRoot, "a", d)); err != nil {
			t.Error(err)
		}
	}
	if code, _, errs := atl(t, "workspace", "new", "b"); code != 0 {
		t.Fatalf("new b: %d %s", code, errs)
	}
	if code, _, errs := atl(t, "workspace", "new", "a"); code != exitErr || !strings.Contains(errs, "already exists") {
		t.Errorf("new a again: %d %q", code, errs)
	}
	if code, _, _ := atl(t, "workspace", "new", "Bad"); code != exitUsage {
		t.Errorf("new Bad: %d", code)
	}
	// A directory without atelier.toml: listed, and selecting it says how
	// to adopt it.
	if err := os.MkdirAll(filepath.Join(wsRoot, "old", "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := atl(t, "-w", "old", "ls"); code != exitErr || !strings.Contains(errs, "atl workspace new old") {
		t.Errorf("ls in old: %d %q", code, errs)
	}

	// None selected: one line naming the ways and the workspaces.
	code, out, errs = atl(t, "ls")
	if code != exitErr || out != "" || strings.Count(strings.TrimSpace(errs), "\n") != 0 ||
		!strings.Contains(errs, "--workspace <name>") || !strings.Contains(errs, "cd $ATL_WORKSPACES/<name>") ||
		!strings.Contains(errs, "workspaces: a, b") {
		t.Errorf("ls, none selected: %d %q %q", code, out, errs)
	}
	// doctor reports it and still checks the tools.
	code, out, _ = atl(t, "doctor")
	if code != exitErr || doctorLevel(out, "workspace") != "FAIL" || doctorLevel(out, "git") == "" || doctorLevel(out, "database") != "" {
		t.Errorf("doctor, none selected: %d\n%s", code, out)
	}
	// help needs none; neither does atl alone off a terminal (the help).
	if code, out, _ := atl(t); code != 0 || !strings.Contains(out, "--workspace") || !strings.Contains(out, "workspace") {
		t.Errorf("atl: %d\n%s", code, out)
	}

	// --workspace, and the settings' sources in doctor.
	if code, _, errs := atl(t, "-w", "b", "new", "in b", "--repo", "api", "--no-fetch"); code != 0 {
		t.Fatalf("new in b: %d %s", code, errs)
	}
	_, out, _ = atl(t, "--workspace", "a", "doctor")
	for name, want := range map[string]string{
		"workspace":     "a: " + filepath.Join(wsRoot, "a") + " (from --workspace)",
		"sandbox.image": "$ATL_SANDBOX_IMAGE = img-a (atelier.toml)",
		"tmux_socket":   "$ATL_TMUX_SOCKET = " + os.Getenv("ATL_TMUX_SOCKET") + " (env)",
		"git_config":    "(env)",
		"database":      filepath.Join(wsRoot, "a", "data", "atelier.db"),
	} {
		if !strings.Contains(out, want) || doctorLevel(out, name) != "ok" {
			t.Errorf("doctor -w a: %s lacks %q:\n%s", name, want, out)
		}
	}
	if v := os.Getenv("ATL_SANDBOX_IMAGE"); v != "" {
		t.Errorf("$ATL_SANDBOX_IMAGE = %q after the run", v)
	}
	if _, out, _ := atl(t, "-w", "a", "ls"); strings.Contains(out, "in-b") {
		t.Errorf("a has b's task:\n%s", out)
	}
	// $ATL_WORKSPACE: a name or a path.
	t.Setenv("ATL_WORKSPACE", "b")
	if _, out, _ := atl(t, "ls"); !strings.Contains(out, "in-b") {
		t.Errorf("ls with $ATL_WORKSPACE=b:\n%s", out)
	}
	t.Setenv("ATL_WORKSPACE", filepath.Join(wsRoot, "a"))
	if _, out, _ := atl(t, "ls"); strings.Contains(out, "in-b") {
		t.Errorf("ls with $ATL_WORKSPACE a path:\n%s", out)
	}
	// The flag wins over it.
	if _, out, _ := atl(t, "-w", "b", "ls"); !strings.Contains(out, "in-b") {
		t.Errorf("ls -w b with $ATL_WORKSPACE=a:\n%s", out)
	}
	t.Setenv("ATL_WORKSPACE", "")
	// The current directory, inside the workspace.
	t.Chdir(filepath.Join(wsRoot, "b", "data"))
	if code, out, errs := atl(t, "ls"); code != 0 || !strings.Contains(out, "in-b") {
		t.Errorf("ls in b's dir: %d %q %q", code, out, errs)
	}

	// list: the current one marked, open tasks counted.
	code, out, _ = atl(t, "workspace", "list")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if code != 0 || len(lines) != 3 || !strings.HasPrefix(lines[0], "  a ") || !strings.Contains(lines[0], "0 open") ||
		!strings.HasPrefix(lines[1], "* b ") || !strings.Contains(lines[1], "1 open") || !strings.HasSuffix(lines[1], filepath.Join(wsRoot, "b")) ||
		!strings.HasPrefix(lines[2], "  old ") || !strings.Contains(lines[2], "not a workspace (no atelier.toml)") {
		t.Errorf("list:\n%s", out)
	}
	code, out, _ = atl(t, "workspace", "list", "--json")
	var list []jsonWorkspace
	if err := json.Unmarshal([]byte(out), &list); code != 0 || err != nil || len(list) != 3 {
		t.Fatalf("list --json: %d %v\n%s", code, err, out)
	}
	if w := list[1]; w.Name != "b" || !w.Current || !w.Workspace || w.OpenTasks != 1 || w.Waiting != 0 || w.Path != filepath.Join(wsRoot, "b") {
		t.Errorf("b: %+v", w)
	}
	if w := list[2]; w.Name != "old" || w.Workspace || w.Current {
		t.Errorf("old: %+v", w)
	}

	// open: the workspace must exist; the TUI needs a terminal.
	if code, _, errs := atl(t, "workspace", "open", "nope"); code != exitErr || !strings.Contains(errs, "atl workspace new nope") {
		t.Errorf("open nope: %d %q", code, errs)
	}
	if code, _, errs := atl(t, "workspace", "open", "a"); code != exitErr || !strings.Contains(errs, "needs a terminal") {
		t.Errorf("open a: %d %q", code, errs)
	}
	if code, _, _ := atl(t, "-w", "b", "workspace", "open", "a"); code != exitUsage {
		t.Errorf("open a -w b: %d", code)
	}
}

// wsEnv is setup without the ATL_* values it exports, so that the
// workspace's own (atelier.toml, defaults) apply; tmux servers it names go
// under the test's dir. It returns setup's root and the workspaces root.
func wsEnv(t *testing.T) (string, string) {
	root := setup(t)
	for _, k := range []string{"ATL_WORKSPACE", "ATL_WORKSPACE_VARS", "ATL_TMUX_SOCKET", "ATL_REPOS", "ATL_TASKS",
		"ATL_SANDBOX_IMAGE", "ATL_SANDBOX_MOUNTS", "ATL_SANDBOX_CONTEXT", "ATL_CONTEXT"} {
		t.Setenv(k, "")
	}
	t.Setenv("TMUX_TMPDIR", filepath.Join(root, "t"))
	if err := os.MkdirAll(filepath.Join(root, "t"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	return root, os.Getenv("ATL_WORKSPACES")
}

// doctorDetail returns the detail of doctor's check name.
func doctorDetail(out, name string) string {
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[1] == name {
			return strings.Join(f[2:], " ")
		}
	}
	return ""
}

// TestWorkspaceSettings: the settings come from atelier.toml and the
// defaults (a tmux server per workspace); what atl set for one workspace
// is not taken for the user's by an atl it starts for another, follows
// atelier.toml, and a value the user set wins; atl workspace new takes
// only the user's values.
func TestWorkspaceSettings(t *testing.T) {
	root, wsRoot := wsEnv(t)
	if code, _, errs := atl(t, "workspace", "new", "a"); code != 0 {
		t.Fatalf("new a: %d %s", code, errs)
	}
	if code, _, errs := atl(t, "workspace", "new", "b"); code != 0 {
		t.Fatalf("new b: %d %s", code, errs)
	}
	a, b := filepath.Join(wsRoot, "a"), filepath.Join(wsRoot, "b")
	if toml, _ := os.ReadFile(filepath.Join(a, "atelier.toml")); strings.Contains(string(toml), "repos") {
		t.Errorf("atelier.toml has repos with none given:\n%s", toml)
	}
	writeTOML := func(dir, s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "atelier.toml"), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeTOML(b, "repos = \""+filepath.Join(root, "repos")+"\"\ncontext = \"ctx-b\"\n[sandbox]\nmounts = [\"~/.npmrc\"]\n")

	_, out, _ := atl(t, "-w", "a", "doctor")
	for name, want := range map[string]string{
		"tmux_socket": "$ATL_TMUX_SOCKET = atl-a (default)",
		"repos":       "$ATL_REPOS = " + filepath.Join(a, "repos") + " (default)",
		"context":     "$ATL_CONTEXT unset",
	} {
		if want != "" && doctorDetail(out, name) != want {
			t.Errorf("doctor -w a: %s = %q, want %q\n%s", name, doctorDetail(out, name), want, out)
		}
	}
	if !strings.Contains(out, filepath.Join(a, "tasks")) {
		t.Errorf("doctor -w a: tasks dir not a's:\n%s", out)
	}
	_, out, _ = atl(t, "-w", "b", "doctor")
	if d := doctorDetail(out, "context"); d != "$ATL_CONTEXT = 5 bytes (atelier.toml)" {
		t.Errorf("doctor -w b: context %q", d)
	}
	if d := doctorDetail(out, "repos"); d != "$ATL_REPOS = "+filepath.Join(root, "repos")+" (atelier.toml)" {
		t.Errorf("doctor -w b: repos %q", d)
	}

	// The tmux server is the workspace's: a session goes to atl-b.
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n[ \"$1\" = --version ] && exec echo stub\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if code, _, errs := atl(t, "-w", "b", "new", "x", "--repo", "api", "--no-fetch"); code != 0 {
		t.Fatalf("new x: %d %s", code, errs)
	}
	if code, _, errs := atl(t, "-w", "b", "session", "x"); code != 0 {
		t.Fatalf("session x: %d %s", code, errs)
	}
	sb := &tmux.Client{Socket: "atl-b"}
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", "atl-b", "kill-server").Run() })
	if !sb.HasSession(context.Background(), tmux.SessionName("x")) {
		t.Error("no session atl-x on tmux server atl-b")
	}

	// An atl started from b's (what atl b sets in its environment, and
	// gives everything it starts).
	wb, err := workspace.Load(b)
	if err != nil {
		t.Fatal(err)
	}
	ap, err := workspace.Apply(wb, workspace.ByFlag)
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Undo()
	// ... for a: b's values are not the user's.
	_, out, _ = atl(t, "-w", "a", "doctor")
	for name, want := range map[string]string{
		"tmux_socket":    "$ATL_TMUX_SOCKET = atl-a (default)",
		"repos":          "$ATL_REPOS = " + filepath.Join(a, "repos") + " (default)",
		"context":        "$ATL_CONTEXT unset",
		"sandbox.mounts": "$ATL_SANDBOX_MOUNTS unset",
	} {
		if d := doctorDetail(out, name); d != want {
			t.Errorf("-w a from b: %s = %q, want %q", name, d, want)
		}
	}
	// ... nor are they for atl workspace new.
	if code, _, errs := atl(t, "workspace", "new", "c"); code != 0 {
		t.Fatalf("new c: %d %s", code, errs)
	}
	if toml, _ := os.ReadFile(filepath.Join(wsRoot, "c", "atelier.toml")); strings.Contains(string(toml), "repos") ||
		strings.Contains(string(toml), "ctx-b") || strings.Contains(string(toml), "npmrc") {
		t.Errorf("new c from b took b's values:\n%s", toml)
	}
	// ... for b itself, after atelier.toml changed: the file's new values.
	writeTOML(b, "context = \"changed\"\n")
	_, out, _ = atl(t, "doctor")
	if d := doctorDetail(out, "context"); d != "$ATL_CONTEXT = 7 bytes (atelier.toml)" {
		t.Errorf("b after an edit: context %q", d)
	}
	if d := doctorDetail(out, "repos"); d != "$ATL_REPOS = "+filepath.Join(b, "repos")+" (default)" {
		t.Errorf("b after an edit: repos %q", d)
	}
	// A value the user set wins, also when switching.
	t.Setenv("ATL_TMUX_SOCKET", "mine")
	_, out, _ = atl(t, "-w", "a", "doctor")
	if d := doctorDetail(out, "tmux_socket"); d != "$ATL_TMUX_SOCKET = mine (env)" {
		t.Errorf("user's socket: %q", d)
	}
	if os.Getenv("ATL_TMUX_SOCKET") != "mine" {
		t.Error("the user's value was changed")
	}
	t.Setenv("ATL_TMUX_SOCKET", "atl-b")
	// $ATL_WORKSPACE_VARS left over without $ATL_WORKSPACE: still atl's.
	t.Setenv("ATL_WORKSPACE", "")
	_, out, _ = atl(t, "-w", "a", "doctor")
	if d := doctorDetail(out, "tmux_socket"); d != "$ATL_TMUX_SOCKET = atl-a (default)" {
		t.Errorf("leftover vars: %q", d)
	}
}

// TestWorkspaceAdoptTmux: adopting a directory that has a database while
// $ATL_TMUX_SOCKET is unset keeps its sessions on tmux's default server.
func TestWorkspaceAdoptTmux(t *testing.T) {
	_, wsRoot := wsEnv(t)
	old := filepath.Join(wsRoot, "old")
	if err := os.MkdirAll(filepath.Join(old, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "data", "atelier.db"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := atl(t, "workspace", "new", "old")
	if code != 0 || !strings.Contains(errs, `atl: note: tmux_socket = "default"`) {
		t.Fatalf("new old: %d %q", code, errs)
	}
	if toml, _ := os.ReadFile(filepath.Join(old, "atelier.toml")); !strings.Contains(string(toml), "\ntmux_socket = \"default\"\n") ||
		!strings.Contains(string(toml), "# Remove this line") {
		t.Errorf("atelier.toml:\n%s", toml)
	}
	_, out, _ := atl(t, "-w", "old", "doctor")
	if d := doctorDetail(out, "tmux_socket"); d != "$ATL_TMUX_SOCKET = default (atelier.toml)" {
		t.Errorf("tmux_socket %q", d)
	}
	// A new workspace (no database) gets its own server.
	if code, _, errs := atl(t, "workspace", "new", "fresh"); code != 0 || errs != "" {
		t.Errorf("new fresh: %d %q", code, errs)
	}
	if toml, _ := os.ReadFile(filepath.Join(wsRoot, "fresh", "atelier.toml")); strings.Contains(string(toml), "tmux_socket") {
		t.Errorf("fresh atelier.toml:\n%s", toml)
	}
}

// TestNoWorkspaceToken: atl token needs a workspace (its token file is
// the workspace's) and says so; it writes nothing.
func TestNoWorkspaceToken(t *testing.T) {
	root, _ := wsEnv(t)
	for _, args := range [][]string{{"token", "--paste"}, {"token"}} {
		code, out, errs := atl(t, args...)
		if code != exitErr || out != "" || !strings.Contains(errs, "no workspace selected") {
			t.Errorf("%v: %d %q %q", args, code, out, errs)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "-run")); !os.IsNotExist(err) {
		t.Errorf("token wrote in the current directory: %v", err)
	}
	code, out, _ := atl(t, "doctor")
	if code != exitErr || doctorLevel(out, "workspace") != "FAIL" || doctorLevel(out, "tmux") == "" {
		t.Errorf("doctor: %d\n%s", code, out)
	}
}

// TestWorkspaceSelectorRouting: off a terminal, atl with no workspace
// selected still shows the help, and atl workspace open without a name
// (the selector) says it needs a terminal, whatever $ATL_WORKSPACE says.
func TestWorkspaceSelectorRouting(t *testing.T) {
	_, wsRoot := wsEnv(t)
	if code, _, errs := atl(t, "workspace", "new", "a"); code != 0 {
		t.Fatalf("new a: %d %s", code, errs)
	}
	if code, out, errs := atl(t); code != 0 || !strings.Contains(out, "Usage:") || errs != "" {
		t.Errorf("atl, none selected: %d %q %q", code, out, errs)
	}
	for _, env := range []string{"", "a"} {
		t.Setenv("ATL_WORKSPACE", env)
		code, out, errs := atl(t, "workspace", "open")
		if code != exitErr || out != "" || !strings.Contains(errs, "shows the workspace selector, which needs a terminal") ||
			!strings.Contains(errs, "atl workspace open <name>") {
			t.Errorf("open, $ATL_WORKSPACE=%q: %d %q %q", env, code, out, errs)
		}
	}
	t.Setenv("ATL_WORKSPACE", "")
	// With --workspace and no name: that workspace (here: no terminal).
	if code, _, errs := atl(t, "-w", "a", "workspace", "open"); code != exitErr || !strings.Contains(errs, "atl workspace open needs a terminal") {
		t.Errorf("-w a workspace open: %d %q", code, errs)
	}
	if code, _, _ := atl(t, "workspace", "open", "a", "b"); code != exitUsage {
		t.Errorf("open a b: %d", code)
	}
	if _, err := os.Stat(filepath.Join(wsRoot, ".last")); !os.IsNotExist(err) {
		t.Errorf(".last written without a TUI: %v", err)
	}
}

// TestReexec: opening the selector's choice re-executes atl as
// `atl --workspace <name>` in the environment atl started with, not the
// one a workspace was applied to.
func TestReexec(t *testing.T) {
	_, wsRoot := wsEnv(t)
	if code, _, errs := atl(t, "workspace", "new", "a"); code != 0 {
		t.Fatalf("new a: %d %s", code, errs)
	}
	var gotBin string
	var gotArgv, gotEnv []string
	old := execProcess
	t.Cleanup(func() { execProcess = old })
	execProcess = func(bin string, argv, env []string) error {
		gotBin, gotArgv, gotEnv = bin, argv, env
		return nil
	}
	t.Setenv("ATL_BIN", "/opt/atl")

	ws, err := workspace.Load(filepath.Join(wsRoot, "a"))
	if err != nil {
		t.Fatal(err)
	}
	ap, err := workspace.Apply(ws, workspace.ByFlag)
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Undo()
	c := &cli{}
	if err := c.openChosen("beta"); err != nil {
		t.Fatal(err)
	}
	if gotBin != "/opt/atl" || strings.Join(gotArgv, " ") != "/opt/atl --workspace beta" {
		t.Errorf("exec %q %q", gotBin, gotArgv)
	}
	for _, kv := range gotEnv {
		if kv == "ATL_WORKSPACE="+ws.Dir {
			t.Errorf("the applied workspace leaked into the environment: %s", kv)
		}
	}
	if len(gotEnv) == 0 {
		t.Error("empty environment")
	}

	execProcess = func(string, []string, []string) error { return os.ErrPermission }
	if err := c.reexec("workspace", "open"); err == nil || !strings.Contains(err.Error(), "run /opt/atl workspace open") {
		t.Errorf("failed exec: %v", err)
	}
}
