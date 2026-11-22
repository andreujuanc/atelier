package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andreujuanc/atelier/internal/tmux"
)

// TestTmuxConfig: atl writes the config its tmux server starts with into
// the workspace's state dir, and doctor says so; with tmux_socket =
// "default" (the user's server) it writes none.
func TestTmuxConfig(t *testing.T) {
	setup(t)
	conf := filepath.Join(os.Getenv("ATL_WORKSPACE"), "state", "tmux.conf")
	_, out, _ := atl(t, "doctor")
	if b, err := os.ReadFile(conf); err != nil || string(b) != tmux.ServerConfigWith(filepath.Join(os.Getenv("ATL_WORKSPACE"), "tmux.conf")) {
		t.Fatalf("%s: %q, %v", conf, b, err)
	}
	if !strings.Contains(out, "tmux server") || !strings.Contains(out, "-L "+os.Getenv("ATL_TMUX_SOCKET")+", not running: atl starts it with "+conf) {
		t.Errorf("doctor:\n%s", out)
	}

	if err := os.Remove(conf); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATL_TMUX_SOCKET", "default")
	_, out, _ = atl(t, "doctor")
	if _, err := os.Stat(conf); !os.IsNotExist(err) {
		t.Errorf("default server: %s written (%v)", conf, err)
	}
	if !strings.Contains(out, "tmux's default server") {
		t.Errorf("doctor, default server:\n%s", out)
	}
}
