package tmux

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestCopy: Copy puts the text in a paste buffer and sends it to the
// terminal attached to the session as OSC 52. The terminal is a pane of a
// second tmux server ("outer", set-clipboard on, as a terminal that takes
// OSC 52), which turns the sequence it receives into a buffer of its own.
func TestCopy(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	text := "echo 'héllo' \\\n  --a-long-flag=\"x y\"\n\ttabbed\n"

	// No session, a session without a terminal, nothing to copy.
	if err := c.Copy(ctx, "atl-t", []byte(text)); !errors.Is(err, ErrNoClient) {
		t.Errorf("Copy without the session: %v, want ErrNoClient", err)
	}
	if _, err := c.NewWindow(ctx, WindowSpec{Session: "atl-t", Name: "claude", Argv: []string{"sleep", "300"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Copy(ctx, "atl-t", []byte(text)); !errors.Is(err, ErrNoClient) {
		t.Errorf("Copy without a terminal: %v, want ErrNoClient", err)
	}
	if err := c.Copy(ctx, "atl-t", nil); err == nil {
		t.Error("Copy of nothing succeeded")
	}
	if out, _ := c.run(ctx, "list-buffers", "-F", "#{buffer_name}"); out != "" {
		t.Fatalf("buffers after refused copies: %q", out)
	}

	// The tmux server under test takes any terminal for one that does OSC
	// 52 (the outer server's panes have its default-terminal).
	if _, err := c.run(ctx, "set-option", "-as", "terminal-features", ",*:clipboard"); err != nil {
		t.Fatal(err)
	}
	outer := testClient(t)
	if _, err := outer.run(ctx, "set-option", "-g", "set-clipboard", "on"); err != nil {
		t.Fatal(err)
	}
	attach := []string{"env", "-u", "TMUX", "tmux", "-L", c.Socket, "attach-session", "-t", "=atl-t"}
	if _, err := outer.NewWindow(ctx, WindowSpec{Session: "terminal", Argv: attach}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, err := c.run(ctx, "list-clients", "-t", "=atl-t", "-F", "#{client_name}")
		if err == nil && out != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no terminal attached to atl-t: %q, %v", out, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Another session's terminal does not matter (there is none here, and
	// a session named like a prefix is not the session).
	if err := c.Copy(ctx, "atl", []byte(text)); !errors.Is(err, ErrNoClient) {
		t.Errorf("Copy to a session name prefix: %v, want ErrNoClient", err)
	}

	if err := c.Copy(ctx, "atl-t", []byte(text)); err != nil {
		t.Fatal(err)
	}
	if out, err := c.runRaw(ctx, "show-buffer"); err != nil || out != text {
		t.Errorf("paste buffer = %q, %v; want %q", out, err, text)
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		out, err := outer.runRaw(ctx, "show-buffer")
		if err == nil && out == text {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the terminal's clipboard (outer buffer) = %q, %v; want %q", out, err, text)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// runRaw is run without trimming the output.
func (c *Client) runRaw(ctx context.Context, args ...string) (string, error) {
	var out strings.Builder
	cmd := exec.CommandContext(ctx, "tmux", c.args(args...)...)
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	return out.String(), err
}
