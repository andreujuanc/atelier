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
	c := &Client{Socket: sock, config: os.DevNull}
	startServer(t, c, nil)
	return c
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
	t.Setenv("TMUX", "/tmp/tmux-1000/default,123,0")
	if got, want := c.AttachCmd("atl-x").Args, []string{"tmux", "-L", "s1", "switch-client", "-t", "=atl-x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("inside tmux: %v, want %v", got, want)
	}
	if got, want := (&Client{}).AttachCmd("atl-x").Args, []string{"tmux", "switch-client", "-t", "=atl-x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("default socket: %v, want %v", got, want)
	}
	if cmd := c.AttachCmd("atl-x"); cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
		t.Error("AttachCmd must not wire stdio")
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
