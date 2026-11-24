package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var sockSeq atomic.Int64

// testClient returns a client on a fresh, isolated tmux socket whose server
// (already started, without any tmux.conf and with exit-empty off) is killed
// at cleanup. It skips if tmux is not installed.
func testClient(t *testing.T) *Client {
	t.Helper()
	c := freshClient(t)
	c.config = os.DevNull
	startServer(t, c, nil)
	return c
}

// freshClient returns a client on a fresh, isolated tmux socket, with no
// server yet and no config set (a server it starts is killed at cleanup).
// It skips if tmux is not installed.
func freshClient(t *testing.T) *Client {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	t.Setenv("TMUX", "")
	// Keep the user's tmux.conf out of the test server.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("SHELL", "/bin/sh")
	sock := fmt.Sprintf("atlt-%d-%d", os.Getpid(), sockSeq.Add(1))
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", sock, "kill-server").Run()
		tmp := os.Getenv("TMUX_TMPDIR")
		if tmp == "" {
			tmp = "/tmp"
		}
		_ = os.Remove(filepath.Join(tmp, fmt.Sprintf("tmux-%d", os.Getuid()), sock))
	})
	return &Client{Socket: sock}
}

// startServer starts c's server with -f /dev/null (so neither /etc/tmux.conf
// nor the user's config is loaded), exit-empty off, and the given extra
// global options ("name value" pairs, e.g. {"base-index", "1"}).
func startServer(t *testing.T, c *Client, opts [][2]string) {
	t.Helper()
	args := []string{"-L", c.Socket, "-f", os.DevNull, "start-server", ";", "set-option", "-g", "exit-empty", "off"}
	for _, o := range opts {
		args = append(args, ";", "set-option", "-g", o[0], o[1])
	}
	if out, err := exec.Command("tmux", args...).CombinedOutput(); err != nil {
		t.Fatalf("start tmux server: %v: %s", err, out)
	}
}

// waitPanes waits until window id's #{pane_dead} list equals want.
func waitPanes(t *testing.T, c *Client, id, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, err := c.run(context.Background(), "list-panes", "-t", id, "-F", "#{pane_dead}")
		if err != nil {
			t.Fatal(err)
		}
		if out == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("panes of %s dead = %q, want %q", id, out, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitDead waits until the (single-pane) window id's pane is dead; the
// window itself must remain (remain-on-exit).
func waitDead(t *testing.T, c *Client, id string) { t.Helper(); waitPanes(t, c, id, "1") }

func waitFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(path)
		if err == nil && len(b) > 0 && strings.HasSuffix(string(b), "\n") {
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSessionName(t *testing.T) {
	if got := SessionName("fix-login"); got != "atl-fix-login" {
		t.Fatalf("SessionName = %q", got)
	}
}

// TestGlobalEnv: a window gets the variables of WindowSpec.GlobalEnv from
// the server's global environment, whether a server runs already or the
// launch starts it (the first session after a reboot), and they are not in
// this process's environment (a stored token is not); values a tmux
// command cannot carry are refused without being repeated.
func TestGlobalEnv(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	secret := "sk-ant-oat01-Abc_def-123$HOME~#{x}"
	os.Unsetenv("ATL_T_TOKEN")
	dir := t.TempDir()
	launch := func(name string) string {
		t.Helper()
		out := filepath.Join(dir, name)
		_, err := c.NewWindow(ctx, WindowSpec{
			Session: "atl-x", Name: name, GlobalEnv: map[string]string{"ATL_T_TOKEN": secret},
			Argv: []string{"sh", "-c", fmt.Sprintf(`printf 'token=%%s\n' "${ATL_T_TOKEN:-<unset>}" > %s.tmp && mv %s.tmp %s; sleep 30`, out, out, out)},
		})
		if err != nil {
			t.Fatal(err)
		}
		return waitFile(t, out)
	}
	if got := launch("running"); got != "token="+secret+"\n" {
		t.Errorf("with a server running: window saw %q", got)
	}
	if out, err := c.run(ctx, "show-environment", "-g", "ATL_T_TOKEN"); err != nil || out != "ATL_T_TOKEN="+secret {
		t.Errorf("global env = %q, %v", out, err)
	}
	if _, err := c.run(ctx, "kill-server"); err != nil {
		t.Fatal(err)
	}
	if got := launch("fresh"); got != "token="+secret+"\n" {
		t.Errorf("first window of a new server: window saw %q", got)
	}
	for _, bad := range []string{"a'b", "a\nb", "a\x1bb"} {
		_, err := c.NewWindow(ctx, WindowSpec{Session: "atl-x", GlobalEnv: map[string]string{"ATL_T_TOKEN": bad}, Argv: []string{"true"}})
		if err == nil || strings.Contains(err.Error(), bad) {
			t.Errorf("value %q: %v", bad, err)
		}
	}
}

func TestNewWindowAndTags(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	dir := t.TempDir()

	if c.HasSession(ctx, "atl-x") {
		t.Fatal("HasSession true before creation")
	}
	envFile := filepath.Join(dir, "env")
	pwdFile := filepath.Join(dir, "pwd")
	script := fmt.Sprintf("pwd > %s; env > %s.tmp && mv %s.tmp %s; exec sleep 300", pwdFile, envFile, envFile, envFile)
	id1, err := c.NewWindow(ctx, WindowSpec{
		Session: "atl-x", Name: "claude-1", Dir: dir,
		Env:  map[string]string{"ATL_SESSION": "1", "ATL_TASK": "x", "WITH_SPACE": "a b"},
		Argv: []string{"sh", "-c", script},
		Tag:  "1",
	})
	if err != nil {
		t.Fatalf("NewWindow 1: %v", err)
	}
	if !strings.HasPrefix(id1, "@") {
		t.Fatalf("window id %q", id1)
	}
	if !c.HasSession(ctx, "atl-x") {
		t.Fatal("session not created")
	}
	// Exact match: a prefix must not match.
	if c.HasSession(ctx, "atl-") {
		t.Fatal("HasSession matched a prefix")
	}

	env := waitFile(t, envFile)
	for _, want := range []string{"ATL_SESSION=1\n", "ATL_TASK=x\n", "WITH_SPACE=a b\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("window env missing %q", want)
		}
	}
	realDir, _ := filepath.EvalSymlinks(dir)
	if got := strings.TrimSpace(waitFile(t, pwdFile)); got != dir && got != realDir {
		t.Errorf("window dir = %q, want %q", got, dir)
	}

	// Second window in the same session. A single-element argv is a program
	// path (tmux hands it to the shell, so it must survive quoting).
	prog := filepath.Join(dir, "my prog's.sh")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\nexec sleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	id2, err := c.NewWindow(ctx, WindowSpec{Session: "atl-x", Name: "claude-2", Dir: dir, Argv: []string{prog}, Tag: "2"})
	if err != nil {
		t.Fatalf("NewWindow 2: %v", err)
	}
	if id2 == id1 {
		t.Fatalf("same window id %q twice", id2)
	}
	out, err := c.run(ctx, "list-windows", "-t", "=atl-x", "-F", "#{window_name}")
	if err != nil {
		t.Fatal(err)
	}
	if out != "claude-1\nclaude-2" {
		t.Errorf("windows in session = %q", out)
	}

	// An untagged window is ignored.
	if _, err := c.run(ctx, "new-window", "-t", "=atl-x:", "-d", "--", "sleep", "300"); err != nil {
		t.Fatal(err)
	}
	tags, err := c.TaggedWindows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"1": id1, "2": id2}; !reflect.DeepEqual(tags, want) {
		t.Fatalf("TaggedWindows = %v, want %v", tags, want)
	}

	// A command that exits immediately may vanish before it is tagged: the
	// id comes back with an explanatory error.
	if id, err := c.NewWindow(ctx, WindowSpec{Session: "atl-x", Name: "quick", Argv: []string{"true"}, Tag: "3"}); err != nil {
		if id == "" || !strings.Contains(err.Error(), "exited before") {
			t.Fatalf("NewWindow quick: id %q, err %v", id, err)
		}
	}

	if err := c.KillSession(ctx, "atl-x"); err != nil {
		t.Fatal(err)
	}
	if c.HasSession(ctx, "atl-x") {
		t.Fatal("session still exists after kill")
	}
	if err := c.KillSession(ctx, "atl-x"); err != nil {
		t.Fatalf("second KillSession: %v", err)
	}
}

func TestKillSessionLeavesOthers(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	for _, s := range []string{"atl-a", "atl-ab"} {
		if _, err := c.NewWindow(ctx, WindowSpec{Session: s, Argv: []string{"sleep", "300"}, Tag: s}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.KillSession(ctx, "atl-missing"); err != nil {
		t.Fatalf("KillSession missing: %v", err)
	}
	if err := c.KillSession(ctx, "atl-a"); err != nil {
		t.Fatal(err)
	}
	if c.HasSession(ctx, "atl-a") || !c.HasSession(ctx, "atl-ab") {
		t.Fatal("KillSession killed the wrong session")
	}
	tags, err := c.TaggedWindows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tags["atl-ab"]; !ok || len(tags) != 1 {
		t.Fatalf("TaggedWindows = %v", tags)
	}
}

func TestNoServer(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	// A running server without sessions: nothing tagged, nothing to kill.
	if tags, err := c.TaggedWindows(ctx); err != nil || len(tags) != 0 {
		t.Fatalf("TaggedWindows on empty server = %v, %v", tags, err)
	}
	if err := c.KillSession(ctx, "atl-x"); err != nil {
		t.Fatalf("KillSession on empty server: %v", err)
	}
	if _, err := c.run(ctx, "kill-server"); err != nil {
		t.Fatal(err)
	}
	tags, err := c.TaggedWindows(ctx)
	if err != nil || tags == nil || len(tags) != 0 {
		t.Fatalf("TaggedWindows = %v, %v; want empty, nil", tags, err)
	}
	if err := c.KillSession(ctx, "atl-x"); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	if c.HasSession(ctx, "atl-x") {
		t.Fatal("HasSession true without server")
	}
}

func TestTaggedWindowsDeadPanes(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	if _, err := c.run(ctx, "set-option", "-g", "remain-on-exit", "on"); err != nil {
		t.Fatal(err)
	}
	short := []string{"sh", "-c", "sleep 0.5"}
	live, err := c.NewWindow(ctx, WindowSpec{Session: "atl-x", Argv: []string{"sleep", "300"}, Tag: "live"})
	if err != nil {
		t.Fatal(err)
	}
	dead, err := c.NewWindow(ctx, WindowSpec{Session: "atl-x", Argv: short, Tag: "dead"})
	if err != nil {
		t.Fatal(err)
	}
	// A window with one dead and one live pane is still alive.
	mixed, err := c.NewWindow(ctx, WindowSpec{Session: "atl-x", Argv: short, Tag: "mixed"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.run(ctx, "split-window", "-d", "-t", mixed, "--", "sleep", "300"); err != nil {
		t.Fatal(err)
	}
	waitDead(t, c, dead)
	waitPanes(t, c, mixed, "1\n0") // first pane dead, split pane alive
	tags, err := c.TaggedWindows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"live": live, "mixed": mixed}; !reflect.DeepEqual(tags, want) {
		t.Fatalf("TaggedWindows = %v, want %v", tags, want)
	}
}

func TestNoServerErrors(t *testing.T) {
	mk := func(stderr string) error { return &Error{Args: []string{"list-panes"}, Stderr: stderr} }
	for stderr, want := range map[string]bool{
		"no server running on /tmp/tmux-1000/x":                            true,
		"error connecting to /tmp/tmux-1000/x (No such file or directory)": true,
		"error connecting to /tmp/tmux-1000/x (Connection refused)":        true,
		"error connecting to /tmp/tmux-1000/x (Permission denied)":         false,
		"server version is too old for client":                             false,
		"":                                                                 false,
	} {
		if got := noServer(mk(stderr)); got != want {
			t.Errorf("noServer(%q) = %v, want %v", stderr, got, want)
		}
	}
	if noServer(errors.New("no server running")) {
		t.Error("noServer accepted a non-tmux error")
	}
	// A server we may not talk to is an error, not an empty listing.
	if os.Getuid() == 0 {
		return // root ignores socket permissions
	}
	c := testClient(t)
	sockPath, err := c.run(context.Background(), "display-message", "-p", "#{socket_path}")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sockPath, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(sockPath, 0o660)
	if tags, err := c.TaggedWindows(context.Background()); err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("TaggedWindows on an unreachable socket = %v, %v; want permission error", tags, err)
	}
}

func TestNewWindowCancelledBeforeStart(t *testing.T) {
	c := testClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.NewWindow(ctx, WindowSpec{Session: "atl-x", Argv: []string{"sleep", "300"}, Tag: "1"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("NewWindow with cancelled ctx: %v", err)
	}
	if c.HasSession(context.Background(), "atl-x") {
		t.Fatal("window launched with a cancelled ctx")
	}
}

func TestNewWindowBadDir(t *testing.T) {
	c := testClient(t)
	_, err := c.NewWindow(context.Background(), WindowSpec{Session: "atl-x", Argv: []string{"sleep", "300"}, Tag: "1", Dir: "/nonexistent/dir"})
	// tmux falls back to another directory for a missing -c on some versions;
	// either way no untagged window may be left behind.
	if err == nil {
		return
	}
	tags, _ := c.TaggedWindows(context.Background())
	if len(tags) != 0 {
		t.Fatalf("windows left after failure: %v", tags)
	}
}

func TestAttachCmd(t *testing.T) {
	t.Setenv("TMUX", "")
	c := &Client{Socket: "s1"}
	if got, want := c.AttachCmd("atl-x").Args, []string{"tmux", "-L", "s1", "attach-session", "-t", "=atl-x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("outside tmux: %v, want %v", got, want)
	}
	// Inside another server (another workspace's, or the user's own): a
	// nested attach-session, without TMUX.
	t.Setenv("TMUX", "/nonexistent/tmux-1000/default,123,0")
	t.Setenv("TMUX_PANE", "%1")
	cmd := c.AttachCmd("atl-x")
	if got, want := cmd.Args, []string{"tmux", "-L", "s1", "attach-session", "-t", "=atl-x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("inside another tmux server: %v, want %v", got, want)
	}
	if len(cmd.Env) == 0 {
		t.Error("nested attach without its environment")
	}
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
			t.Errorf("nested attach env has %s", kv)
		}
	}
	// No socket name: tmux picks $TMUX's server.
	if got, want := (&Client{}).AttachCmd("atl-x").Args, []string{"tmux", "switch-client", "-t", "=atl-x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("default socket: %v, want %v", got, want)
	}
	if cmd := c.AttachCmd("atl-x"); cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
		t.Error("AttachCmd must not wire stdio")
	}
}

// TestAttachCmdSameServer: inside a client of the client's own server
// ($TMUX names its socket), switch-client; inside another, attach-session.
func TestAttachCmdSameServer(t *testing.T) {
	c := testClient(t)
	sock := c.SocketPath()
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("SocketPath %s: %v", sock, err)
	}
	t.Setenv("TMUX", sock+",123,0")
	cmd := c.AttachCmd("atl-x")
	if got, want := cmd.Args, c.args("switch-client", "-t", "=atl-x"); !reflect.DeepEqual(got[1:], want) {
		t.Errorf("inside this server: %v, want %v", got, want)
	}
	if cmd.Env != nil {
		t.Error("switch-client with an environment of its own")
	}
	other := testClient(t)
	t.Setenv("TMUX", other.SocketPath()+",1,0")
	if got := c.AttachCmd("atl-x").Args; !reflect.DeepEqual(got[1:], c.args("attach-session", "-t", "=atl-x")) {
		t.Errorf("inside another server: %v", got)
	}
}

// TestAttachCmdStartsServerWithoutSessionVars: attach-session starts the
// server when none runs (which stays if its config, e.g. the user's, makes
// a session); like every server atl starts, it does not take the session's
// variables into its global environment.
func TestAttachCmdStartsServerWithoutSessionVars(t *testing.T) {
	c := freshClient(t)
	c.config = filepath.Join(t.TempDir(), "tmux.conf")
	if err := os.WriteFile(c.config, []byte("new-session -d -s keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATL_SESSION", "7")
	t.Setenv("CLAUDECODE", "1")
	_ = c.AttachCmd("keep").Run() // no terminal: fails once the server runs
	env, err := c.run(context.Background(), "show-environment", "-g")
	if err != nil {
		t.Fatalf("no server after attach-session: %v", err)
	}
	for _, line := range strings.Split(env, "\n") {
		if strings.HasPrefix(line, "ATL_SESSION=") || strings.HasPrefix(line, "CLAUDECODE=") {
			t.Errorf("server started by attach-session has %s", line)
		}
	}
}

func TestNewClientFromEnv(t *testing.T) {
	t.Setenv("ATL_TMUX_SOCKET", "abc")
	if New().Socket != "abc" {
		t.Fatal("New ignores ATL_TMUX_SOCKET")
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"claude":      "claude",
		"/usr/bin/x":  "/usr/bin/x",
		"a b":         "'a b'",
		"it's":        `'it'\''s'`,
		"":            "''",
		"$(rm -rf /)": "'$(rm -rf /)'",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKillTaggedWindows(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	if n, err := c.KillTaggedWindows(ctx, "7", ""); n != 0 || err != nil {
		t.Fatalf("KillTaggedWindows on empty server = %d, %v", n, err)
	}
	if _, err := c.run(ctx, "set-option", "-g", "remain-on-exit", "on"); err != nil {
		t.Fatal(err)
	}
	keep, err := c.NewWindow(ctx, WindowSpec{Session: "atl-x", Argv: []string{"sleep", "300"}, Tag: "70"})
	if err != nil {
		t.Fatal(err)
	}
	dead, err := c.NewWindow(ctx, WindowSpec{Session: "atl-x", Argv: []string{"sh", "-c", "sleep 0.3"}, Tag: "7"})
	if err != nil {
		t.Fatal(err)
	}
	live, err := c.NewWindow(ctx, WindowSpec{Session: "atl-y", Argv: []string{"sleep", "300"}, Tag: "7"})
	if err != nil {
		t.Fatal(err)
	}
	waitDead(t, c, dead)
	// except spares one window carrying the tag.
	n, err := c.KillTaggedWindows(ctx, "7", live)
	if err != nil || n != 1 {
		t.Fatalf("KillTaggedWindows(except %s) = %d, %v; want 1", live, n, err)
	}
	out, err := c.run(ctx, "list-windows", "-a", "-F", "#{window_id}")
	if err != nil {
		t.Fatal(err)
	}
	if want := keep + "\n" + live; out != want {
		t.Fatalf("windows left = %q, want %q (killed %s)", out, want, dead)
	}
	n, err = c.KillTaggedWindows(ctx, "7", "")
	if err != nil || n != 1 {
		t.Fatalf("KillTaggedWindows = %d, %v; want 1", n, err)
	}
	out, err = c.run(ctx, "list-windows", "-a", "-F", "#{window_id}")
	if err != nil {
		t.Fatal(err)
	}
	if out != keep {
		t.Fatalf("windows left = %q, want only %s (killed %s)", out, keep, live)
	}
	if _, err := c.KillTaggedWindows(ctx, "", ""); err == nil {
		t.Fatal("KillTaggedWindows(\"\") succeeded")
	}
}

// TestCapturePaneAfterExit: with remain-on-exit set on the window, what a
// command printed can be read after it exited, scrollback included, with
// lines tmux wrapped joined and lines the program broke itself kept apart.
func TestCapturePaneAfterExit(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	script := `read -r _; i=0; while [ $i -lt 40 ]; do echo "line $i"; i=$((i+1)); done; ` +
		`printf '%s\n' "$(printf 'x%.0s' $(seq 1 100))"; printf 'ab\ncd\n'`
	id, err := c.NewWindow(ctx, WindowSpec{Session: "atl-s", Name: "w", Argv: []string{"sh", "-c", script}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetWindowOption(ctx, id, "remain-on-exit", "on"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.run(ctx, "send-keys", "-t", id, "Enter"); err != nil {
		t.Fatal(err)
	}
	waitDead(t, c, id)
	text, width, err := c.CapturePane(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if width != 80 {
		t.Errorf("width = %d", width)
	}
	for _, want := range []string{"\nline 0\n", "\nline 39\n", "\n" + strings.Repeat("x", 100) + "\n", "\nab\ncd\n"} {
		if !strings.Contains("\n"+text, want) {
			t.Errorf("capture lacks %q:\n%s", want, text)
		}
	}
	if err := c.SetSessionOption(ctx, "atl-s", "detach-on-destroy", "off"); err != nil {
		t.Error(err)
	}
	if v, err := c.run(ctx, "show-options", "-v", "-t", "=atl-s:", "detach-on-destroy"); err != nil || v != "off" {
		t.Errorf("detach-on-destroy = %q, %v", v, err)
	}
	if _, _, err := c.CapturePane(ctx, "w"); err == nil {
		t.Error("CapturePane accepted a non-id target")
	}
}

func TestSelectWindow(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	a, err := c.NewWindow(ctx, WindowSpec{Session: "atl-s", Name: "a", Argv: []string{"sleep", "300"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.NewWindow(ctx, WindowSpec{Session: "atl-s", Name: "b", Argv: []string{"sleep", "300"}}); err != nil {
		t.Fatal(err)
	}
	cur := func() string {
		out, err := c.run(ctx, "list-windows", "-t", "=atl-s", "-F", "#{window_active} #{window_name}")
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range strings.Split(out, "\n") {
			if name, ok := strings.CutPrefix(l, "1 "); ok {
				return name
			}
		}
		return ""
	}
	if got := cur(); got != "b" {
		t.Fatalf("current window = %q, want the newest (b)", got)
	}
	if err := c.SelectWindow(ctx, a); err != nil {
		t.Fatal(err)
	}
	if got := cur(); got != "a" {
		t.Fatalf("current window after SelectWindow = %q, want a", got)
	}
	if err := c.SelectWindow(ctx, "a"); err == nil {
		t.Error("SelectWindow accepted a non-id target")
	}
	if err := c.SelectWindow(ctx, "@9999"); err == nil {
		t.Error("SelectWindow of a missing window succeeded")
	}
}

// TestServerEnv: a tmux server atl starts does not take a session's
// variables, nor, on a named socket, another server's TMUX.
func TestServerEnv(t *testing.T) {
	t.Setenv("ATL_SESSION", "3")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("ATL_TASK", "x")
	t.Setenv("TMUX", "/tmp/tmux-1/default,1,0")
	t.Setenv("ATL_WORKSPACE", "/w")
	has := func(env []string, k string) bool {
		for _, kv := range env {
			if strings.HasPrefix(kv, k+"=") {
				return true
			}
		}
		return false
	}
	env := (&Client{Socket: "s"}).serverEnv()
	for _, k := range []string{"ATL_SESSION", "CLAUDECODE", "ATL_TASK", "TMUX"} {
		if has(env, k) {
			t.Errorf("server env has %s", k)
		}
	}
	if !has(env, "ATL_WORKSPACE") {
		t.Error("server env lacks ATL_WORKSPACE")
	}
	if !has((&Client{}).serverEnv(), "TMUX") {
		t.Error("without a socket name TMUX picks the server; it must stay")
	}
}

// TestArgsEndingInSemicolon: tmux splits its command line into commands at
// every argument that ends in ";", so such an argument (a word of a prompt,
// a value) would lose its ";" and what follows it would run as tmux
// commands. Each one reaches its command as it is.
func TestArgsEndingInSemicolon(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	args := []string{"do it;", `a\;`, ";", "kill-server"}
	script := `for a; do printf '[%s]' "$a"; done > ` + out + `.tmp; echo >> ` + out + `.tmp; mv ` + out + `.tmp ` + out + `; exec sleep 300`
	_, err := c.NewWindow(ctx, WindowSpec{
		Session: "atl-x", Name: "w;", Dir: dir,
		Env:       map[string]string{"E": "v;"},
		GlobalEnv: map[string]string{"G": "g"},
		Argv:      append([]string{"sh", "-c", script, "sh"}, args...),
	})
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	if got, want := strings.TrimSpace(waitFile(t, out)), `[do it;][a\;][;][kill-server]`; got != want {
		t.Errorf("argv = %s, want %s", got, want)
	}
	if !c.HasSession(ctx, "atl-x") {
		t.Fatal("session gone: part of an argument ran as a tmux command")
	}
	if names, err := c.run(ctx, "list-windows", "-t", "=atl-x", "-F", "#{window_name}"); err != nil || names != "w;" {
		t.Errorf("window name = %q, %v", names, err)
	}
	if e, err := c.run(ctx, "show-environment", "-t", "=atl-x", "E"); err == nil && e != "" && e != "E=v;" {
		t.Errorf("session env = %q", e)
	}
	for _, text := range []string{"text;", `text\;`} {
		if err := c.SetBuffer(ctx, text); err != nil {
			t.Fatal(err)
		}
		if got, err := c.run(ctx, "show-buffer"); err != nil || got != text {
			t.Errorf("show-buffer = %q, %v; want %q", got, err, text)
		}
	}
}

// TestNewWindowFormatChars: tmux expands formats (#{...}, #(command)) in a
// new window's directory, name and session name; atl's are taken as they
// are.
func TestNewWindowFormatChars(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "a#{session_name}##b")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pwdFile := filepath.Join(filepath.Dir(dir), "pwd")
	script := fmt.Sprintf("pwd > %s.tmp && mv %s.tmp %s; exec sleep 300", pwdFile, pwdFile, pwdFile)
	for _, session := range []string{"atl-#{host}", "atl-#{host}"} { // new-session, then new-window
		if _, err := c.NewWindow(ctx, WindowSpec{Session: session, Name: "w#{window_id}", Dir: dir, Argv: []string{"sh", "-c", script}}); err != nil {
			t.Fatalf("NewWindow: %v", err)
		}
		realDir, _ := filepath.EvalSymlinks(dir)
		if got := strings.TrimSpace(waitFile(t, pwdFile)); got != dir && got != realDir {
			t.Errorf("window dir = %q, want %q", got, dir)
		}
		os.Remove(pwdFile)
	}
	if !c.HasSession(ctx, "atl-#{host}") {
		t.Error("session atl-#{host} missing")
	}
	if names, err := c.run(ctx, "list-windows", "-t", "=atl-#{host}", "-F", "#{window_name}"); err != nil || names != "w#{window_id}\nw#{window_id}" {
		t.Errorf("window names = %q, %v", names, err)
	}
}
