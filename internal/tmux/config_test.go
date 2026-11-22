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
// (rewriting a file that differs, leaving an equal one alone) and passes
// it with -f; for tmux's default server, or none named, it does nothing.
func TestUseConfig(t *testing.T) {
	dir := t.TempDir()
	for _, sock := range []string{"", DefaultSocket} {
		c := &Client{Socket: sock}
		path := filepath.Join(dir, "none", "tmux.conf")
		if err := c.UseConfig(path); err != nil {
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
	if err := c.UseConfig(path); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != ServerConfig {
		t.Fatalf("%s = %q, %v", path, b, err)
	}
	if want := []string{"-f", path, "-L", "atl-alpha", "has-session"}; !reflect.DeepEqual(c.args("has-session"), want) {
		t.Errorf("args %q, want %q", c.args("has-session"), want)
	}
	before, _ := os.Stat(path)
	if err := (&Client{Socket: "atl-alpha"}).UseConfig(path); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(path); !os.SameFile(before, after) {
		t.Error("an unchanged config was rewritten")
	}
	if err := os.WriteFile(path, []byte("set -g status on\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (&Client{Socket: "atl-alpha"}).UseConfig(path); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != ServerConfig {
		t.Errorf("a changed config was not rewritten: %q", b)
	}

	// Not writable: an error, and no -f.
	c = &Client{Socket: "atl-alpha"}
	if err := c.UseConfig(filepath.Join(path, "x")); err == nil || c.Config() != "" {
		t.Errorf("unwritable: %v, config %q", err, c.Config())
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
// started with the file.
func TestServerConfigTakesEffect(t *testing.T) {
	c := freshClient(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tmux.conf")
	if err := c.UseConfig(path); err != nil {
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
	files, running, err := c.ConfigFiles(ctx)
	if err != nil || !running || !reflect.DeepEqual(files, []string{path}) {
		t.Errorf("config files %q, running %v, %v", files, running, err)
	}
}

// TestServerConfigMatchesBox: ServerConfig has exactly the settings of the
// box image's /etc/tmux.conf (the Dockerfile's printf lines).
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
	for _, l := range strings.Split(ServerConfig, "\n") {
		if l != "" && !strings.HasPrefix(l, "#") {
			got = append(got, l)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ServerConfig settings %q, box %q", got, want)
	}
}
