package tmux

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestUseConfig: for a server of atl's own UseConfig writes ServerConfig
// with the user's file sourced last (rewriting a file that differs,
// leaving an equal one alone) and passes it with -f; for tmux's default
// server, or none named, it does nothing.
func TestUseConfig(t *testing.T) {
	dir := t.TempDir()
	user := filepath.Join(dir, "ws", "tmux.conf")
	for _, sock := range []string{"", DefaultSocket} {
		c := &Client{Socket: sock}
		path := filepath.Join(dir, "none", "tmux.conf")
		if err := c.UseConfig(path, user); err != nil {
			t.Fatal(err)
		}
		if c.Config() != "" || c.OwnServer() {
			t.Errorf("socket %q: config %q, own server %v", sock, c.Config(), c.OwnServer())
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("socket %q: %s written (%v)", sock, path, err)
		}
		if got := c.args("new-session"); !reflect.DeepEqual(got, append(socketArgs(sock), "new-session")) {
			t.Errorf("socket %q: args %q", sock, got)
		}
	}

	c := &Client{Socket: "atl-alpha"}
	path := filepath.Join(dir, "state", "tmux.conf") // its dir is created
	if err := c.UseConfig(path, user); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != ServerConfigWith(user) || !strings.HasPrefix(string(b), ServerConfig) ||
		!strings.HasSuffix(string(b), "\nsource-file -q '"+user+"'\n") {
		t.Fatalf("%s = %q, %v", path, b, err)
	}
	if want := []string{"-f", path, "-L", "atl-alpha", "has-session"}; !reflect.DeepEqual(c.args("has-session"), want) {
		t.Errorf("args %q, want %q", c.args("has-session"), want)
	}
	before, _ := os.Stat(path)
	if err := (&Client{Socket: "atl-alpha"}).UseConfig(path, user); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(path); !os.SameFile(before, after) {
		t.Error("an unchanged config was rewritten")
	}
	if err := os.WriteFile(path, []byte("set -g status on\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (&Client{Socket: "atl-alpha"}).UseConfig(path, user); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != ServerConfigWith(user) {
		t.Errorf("a changed config was not rewritten: %q", b)
	}
	// No user file (no workspace): ServerConfig alone.
	if err := (&Client{Socket: "atl-alpha"}).UseConfig(path, ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != ServerConfig {
		t.Errorf("without a user file: %q", b)
	}

	// Not writable: an error, and no -f.
	c = &Client{Socket: "atl-alpha"}
	if err := c.UseConfig(filepath.Join(path, "x"), user); err == nil || c.Config() != "" {
		t.Errorf("unwritable: %v, config %q", err, c.Config())
	}
}

// TestSourcePath: the user's file is quoted for source-file, which expands
// glob patterns.
func TestSourcePath(t *testing.T) {
	for in, want := range map[string]string{
		"/home/u/ws/tmux.conf":     `'/home/u/ws/tmux.conf'`,
		`/a b/$x "q"/tmux.conf`:    `'/a b/$x "q"/tmux.conf'`,
		`/it's/*?[x]\/tmux.conf`:   `'/it'"'"'s/\*\?\[x]\\/tmux.conf'`,
		"/ws #1/{a,b}/~/tmux.conf": `'/ws #1/{a,b}/~/tmux.conf'`,
	} {
		if got := sourcePath(in); got != want {
			t.Errorf("sourcePath(%q) = %s, want %s", in, got, want)
		}
	}
}

func socketArgs(sock string) []string {
	if sock == "" {
		return nil
	}
	return []string{"-L", sock}
}

// TestServerConfigTakesEffect: a server a Client with UseConfig starts
// (new-session, through NewWindow) has the box's settings, and says it was
// started with the file. Without the user's file it starts all the same;
// with it (in a directory whose name needs quoting) its settings apply.
func TestServerConfigTakesEffect(t *testing.T) {
	c := freshClient(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tmux.conf")
	user := filepath.Join(t.TempDir(), `it's a *[ws]`, "tmux.conf")
	if err := c.UseConfig(path, user); err != nil {
		t.Fatal(err)
	}
	if _, running, err := c.ConfigFiles(ctx); err != nil || running {
		t.Fatalf("before: running %v, %v", running, err)
	}
	if _, err := c.NewWindow(ctx, WindowSpec{Session: "s", Argv: []string{"sleep", "60"}}); err != nil {
		t.Fatal(err)
	}
	for opt, want := range map[string]string{
		"status":           "off",
		"mouse":            "on",
		"escape-time":      "10",
		"default-terminal": "tmux-256color",
	} {
		if got, err := c.run(ctx, "show-options", "-gv", opt); err != nil || got != want {
			t.Errorf("%s = %q (%v), want %q", opt, got, err, want)
		}
	}
	if got, err := c.run(ctx, "show-options", "-sv", "terminal-features"); err != nil || !strings.Contains(got, "*:RGB") {
		t.Errorf("terminal-features = %q (%v), want *:RGB among them", got, err)
	}
	if got, err := c.run(ctx, "list-keys", "-T", "root", "C-q"); err != nil || !strings.Contains(got, "detach-client") {
		t.Errorf("C-q: %q (%v)", got, err)
	}
	list, running, err := c.ConfigFiles(ctx)
	if err != nil || !running || list != path || !ConfigListed(list, path) {
		t.Errorf("config files %q, running %v, %v", list, running, err)
	}

	// The user's file, read last: its settings win.
	if _, err := c.run(ctx, "kill-server"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(user), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(user, []byte("set -g status-left HOOKED\nset -g escape-time 20\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.NewWindow(ctx, WindowSpec{Session: "s", Argv: []string{"sleep", "60"}}); err != nil {
		t.Fatal(err)
	}
	for opt, want := range map[string]string{"status-left": "HOOKED", "escape-time": "20", "status": "off"} {
		if got, err := c.run(ctx, "show-options", "-gv", opt); err != nil || got != want {
			t.Errorf("with the user's file: %s = %q (%v), want %q", opt, got, err, want)
		}
	}
}

// TestConfigListed: a file is found in a #{config_files} list even when
// its path has a comma.
func TestConfigListed(t *testing.T) {
	for _, c := range []struct {
		list, path string
		want       bool
	}{
		{"/s/tmux.conf", "/s/tmux.conf", true},
		{"/a,b/tmux.conf", "/a,b/tmux.conf", true},
		{"/etc/tmux.conf,/a,b/tmux.conf", "/a,b/tmux.conf", true},
		{"/a,b/tmux.conf,/x", "/a,b/tmux.conf", true},
		{"/x,/a,b/tmux.conf,/y", "/a,b/tmux.conf", true},
		{"/etc/tmux.conf", "/s/tmux.conf", false},
		{"/s/tmux.conf.bak", "/s/tmux.conf", false},
		{"/x/s/tmux.conf", "/s/tmux.conf", false},
		{"", "/s/tmux.conf", false},
		{"", "", false},
	} {
		if got := ConfigListed(c.list, c.path); got != c.want {
			t.Errorf("ConfigListed(%q, %q) = %v", c.list, c.path, got)
		}
	}
}

// TestServerConfigMatchesBox: ServerConfig has exactly the settings of the
// box image's /etc/tmux.conf (the Dockerfile's printf lines); the line
// that sources the user's file is atl's only.
func TestServerConfigMatchesBox(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(b), "RUN printf '%s\\n' \\\n")
	box, _, ok2 := strings.Cut(rest, "> /etc/tmux.conf")
	if !ok || !ok2 {
		t.Fatal("no /etc/tmux.conf printf in the Dockerfile")
	}
	var want []string
	for _, l := range strings.Split(box, "\n") {
		l = strings.TrimSuffix(strings.TrimSpace(l), "\\")
		if l = strings.TrimSpace(l); l != "" {
			want = append(want, strings.Trim(l, "'"))
		}
	}
	var got []string
	for _, l := range strings.Split(ServerConfigWith("/ws/tmux.conf"), "\n") {
		if l != "" && !strings.HasPrefix(l, "#") && !strings.HasPrefix(l, "source-file -q ") {
			got = append(got, l)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ServerConfig settings %q, box %q", got, want)
	}
}
