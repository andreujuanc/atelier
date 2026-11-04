// Package tmux is a thin, socket-aware wrapper around the tmux binary. Every
// command runs as `tmux [-L socket] ...` via os/exec (never a shell).
//
// Windows created by atelier carry the window user option @atl_session (the
// sessions.id they belong to). Window ids are reused after a server restart,
// so liveness checks use the tag, never the id alone.
//
// Requires tmux >= 3.2 (new-session -e).
package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// TagOption is the window user option holding the atelier session id.
const TagOption = "@atl_session"

// launchTimeout bounds NewWindow's launch-and-tag sequence, which does not
// honour cancellation of the caller's context.
const launchTimeout = 10 * time.Second

// Client runs tmux commands against one server.
//
// Atelier assumes a single tmux server holds all of its sessions. With Socket
// empty (ATL_TMUX_SOCKET unset) no -L/-S flag is passed, so tmux picks the
// server itself: the one named by $TMUX when atl runs inside tmux, otherwise
// the default socket. Recover, Open and attach all rely on reaching the same
// server every time, so users who run tmux on a non-default socket (tmux -L
// or -S) should set ATL_TMUX_SOCKET to that socket name.
type Client struct {
	Socket string // "" → tmux's own choice ($TMUX's server, else default); else `tmux -L Socket`

	config string // if set, passed as -f (tests: keep tmux.conf out of a server they start)
}

// New returns a client for the server named by $ATL_TMUX_SOCKET. When it is
// unset, tmux uses $TMUX's server if set, else the default socket; see Client.
func New() *Client { return &Client{Socket: os.Getenv("ATL_TMUX_SOCKET")} }

// SessionName returns the tmux session name for a task slug.
func SessionName(slug string) string { return "atl-" + slug }

// WindowSpec describes a window to launch.
type WindowSpec struct {
	Session string            // tmux session name; created detached if missing
	Name    string            // window name
	Dir     string            // start dir
	Env     map[string]string // passed with -e
	Argv    []string          // command
	Tag     string            // stored as window user option @atl_session; "" → untagged
}

// Error is returned when a tmux invocation fails.
type Error struct {
	Args   []string // arguments passed to tmux (excluding "-L socket")
	Stderr string   // trimmed stderr
	Err    error    // underlying error; the context's error if it was cancelled
}

func (e *Error) Error() string {
	sub := ""
	if len(e.Args) > 0 {
		sub = e.Args[0]
	}
	if e.Stderr != "" {
		return fmt.Sprintf("tmux %s: %s", sub, e.Stderr)
	}
	return fmt.Sprintf("tmux %s: %v", sub, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

func (c *Client) args(args ...string) []string {
	var pre []string
	if c.config != "" {
		pre = append(pre, "-f", c.config)
	}
	if c.Socket != "" {
		pre = append(pre, "-L", c.Socket)
	}
	return append(pre, args...)
}

// run executes tmux and returns trimmed stdout.
func (c *Client) run(ctx context.Context, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "tmux", c.args(args...)...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// A freshly started server daemonizes; never hang on inherited pipes.
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			err = cerr
		}
		return strings.TrimSpace(stdout.String()), &Error{Args: args, Stderr: strings.TrimSpace(stderr.String()), Err: err}
	}
	return strings.TrimSpace(stdout.String()), nil
}

// noServer reports whether err means no tmux server is running on the
// socket: "no server running on ..." or "error connecting to ... (No such
// file or directory | Connection refused)". Any other failure to connect
// (e.g. "Permission denied") is a real error: treating it as "no server"
// would make Recover stop every live session.
func noServer(err error) bool {
	var te *Error
	if !errors.As(err, &te) {
		return false
	}
	s := te.Stderr
	return strings.Contains(s, "no server running") ||
		strings.Contains(s, "No such file or directory") ||
		strings.Contains(s, "Connection refused")
}

// noSessions reports whether err means there is no server or a server
// without any session (tmux says "no current target" for -a listings and
// session targets then).
func noSessions(err error) bool {
	var te *Error
	if !errors.As(err, &te) {
		return false
	}
	return noServer(err) ||
		strings.Contains(te.Stderr, "no sessions") ||
		strings.Contains(te.Stderr, "no current target")
}

// noSession reports whether err means the target session does not exist.
func noSession(err error) bool {
	var te *Error
	if !errors.As(err, &te) {
		return false
	}
	return noSessions(err) ||
		strings.Contains(te.Stderr, "can't find session") ||
		strings.Contains(te.Stderr, "session not found")
}

// gone reports whether err means the target window (or the whole server)
// no longer exists.
func gone(err error) bool {
	var te *Error
	if !errors.As(err, &te) {
		return false
	}
	return noServer(err) || strings.Contains(te.Stderr, "can't find window") || strings.Contains(te.Stderr, "no such window")
}

// HasSession reports whether a session with exactly this name exists.
func (c *Client) HasSession(ctx context.Context, name string) bool {
	_, err := c.run(ctx, "has-session", "-t", "="+name)
	return err == nil
}

// NewWindow launches spec.Argv in a new window (creating the session detached
// if it is missing), tags it with @atl_session=spec.Tag and returns its window
// id ("@N"). With an empty Tag the window is left untagged (TaggedWindows,
// KillTaggedWindows and so Recover ignore it).
//
// The tag is set by a second tmux command. If the window's command exits
// immediately the window can be gone by then; the id is returned together
// with an error saying so. If tagging fails for another reason the untagged
// window is killed (best effort) and its id is returned with the error.
//
// Once started, the launch-and-tag sequence ignores cancellation of ctx (it
// runs under its own launchTimeout instead): cancelling between the two
// commands would leave a running window atelier could never track.
func (c *Client) NewWindow(ctx context.Context, spec WindowSpec) (string, error) {
	if len(spec.Argv) == 0 {
		return "", errors.New("tmux: new window: empty command")
	}
	if spec.Session == "" {
		return "", errors.New("tmux: new window: empty session name")
	}
	build := func(exists bool) []string {
		var args []string
		if exists {
			args = []string{"new-window", "-t", "=" + spec.Session + ":"}
		} else {
			args = []string{"new-session", "-d", "-s", spec.Session}
		}
		if spec.Name != "" {
			args = append(args, "-n", spec.Name)
		}
		if spec.Dir != "" {
			args = append(args, "-c", spec.Dir)
		}
		keys := make([]string, 0, len(spec.Env))
		for k := range spec.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "-e", k+"="+spec.Env[k])
		}
		args = append(args, "-P", "-F", "#{window_id}", "--")
		return append(args, commandArgs(spec.Argv)...)
	}

	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("tmux: launch window in session %s: %w", spec.Session, err)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), launchTimeout)
	defer cancel()

	exists := c.HasSession(ctx, spec.Session)
	out, err := c.run(ctx, build(exists)...)
	var te *Error
	if !exists && err != nil && out == "" && errors.As(err, &te) && strings.Contains(te.Stderr, "duplicate session") {
		// Lost a race with another atl creating the same session.
		out, err = c.run(ctx, build(true)...)
	}
	if err != nil {
		return "", fmt.Errorf("tmux: launch window in session %s: %w", spec.Session, err)
	}
	id := strings.TrimSpace(out)
	if !strings.HasPrefix(id, "@") || strings.ContainsAny(id, " \n") {
		return "", fmt.Errorf("tmux: launch window in session %s: unexpected output %q", spec.Session, out)
	}
	if spec.Tag == "" {
		return id, nil
	}
	if _, err := c.run(ctx, "set-option", "-w", "-t", id, TagOption, spec.Tag); err != nil {
		if gone(err) {
			return id, fmt.Errorf("tmux: window %s in session %s exited before it could be tagged (did its command fail to start?): %w",
				id, spec.Session, err)
		}
		// Still there but untagged: atelier could never track it.
		_, _ = c.run(ctx, "kill-window", "-t", id)
		return id, fmt.Errorf("tmux: tag window %s in session %s: %w", id, spec.Session, err)
	}
	return id, nil
}

// commandArgs adapts argv for tmux: with several arguments tmux executes them
// directly, but a single argument is handed to the shell, so it is quoted.
func commandArgs(argv []string) []string {
	if len(argv) == 1 {
		return []string{shellQuote(argv[0])}
	}
	return argv
}

func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./=:@%+,") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// TaggedWindows returns tag → window id for every tagged window, in every
// session, that has at least one live pane. Windows whose panes are all dead
// (kept around by remain-on-exit) count as gone. No server (or no sessions)
// yields an empty map and nil.
func (c *Client) TaggedWindows(ctx context.Context) (map[string]string, error) {
	out, err := c.run(ctx, "list-panes", "-a", "-F", "#{window_id} #{pane_dead} #{"+TagOption+"}")
	m := map[string]string{}
	if err != nil {
		if noSessions(err) {
			return m, nil
		}
		return nil, err
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(strings.TrimSpace(line), " ", 3)
		if len(f) != 3 || f[0] == "" || f[2] == "" || f[1] == "1" {
			continue
		}
		m[f[2]] = f[0]
	}
	return m, nil
}

// KillTaggedWindows kills every window, in every session, tagged with exactly
// @atl_session=tag — live or dead (remain-on-exit) — except the window whose
// id is except (if non-empty), and returns how many it killed. No server, no
// sessions, or a window that disappears meanwhile is not an error.
func (c *Client) KillTaggedWindows(ctx context.Context, tag, except string) (int, error) {
	if tag == "" {
		return 0, errors.New("tmux: kill tagged windows: empty tag")
	}
	out, err := c.run(ctx, "list-windows", "-a", "-F", "#{window_id} #{"+TagOption+"}")
	if err != nil {
		if noSessions(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	var errs []error
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(strings.TrimSpace(line), " ", 2)
		if len(f) != 2 || f[1] != tag || !strings.HasPrefix(f[0], "@") || f[0] == except {
			continue
		}
		if _, err := c.run(ctx, "kill-window", "-t", f[0]); err != nil {
			if !gone(err) {
				errs = append(errs, err)
			}
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// WindowAlive reports whether window (matched by exact name) of the named
// session (exact name) exists and its first pane is alive (not dead). A
// missing server, session or window is (false, nil).
func (c *Client) WindowAlive(ctx context.Context, session, window string) (bool, error) {
	out, err := c.run(ctx, "list-panes", "-t", "="+session+":="+window, "-F", "#{pane_dead}")
	if err != nil {
		if noSession(err) || gone(err) {
			return false, nil
		}
		return false, err
	}
	first, _, _ := strings.Cut(out, "\n")
	return strings.TrimSpace(first) == "0", nil
}

// SelectWindow makes window (an id, "@N") the current window of its
// session, so attaching to the session shows it.
func (c *Client) SelectWindow(ctx context.Context, window string) error {
	if !strings.HasPrefix(window, "@") {
		return fmt.Errorf("tmux: select window: bad window id %q", window)
	}
	if _, err := c.run(ctx, "select-window", "-t", window); err != nil {
		return fmt.Errorf("tmux: select window %s: %w", window, err)
	}
	return nil
}

// SetBuffer puts text in a new automatic paste buffer, the one tmux's
// paste-buffer (prefix ]) pastes by default.
func (c *Client) SetBuffer(ctx context.Context, text string) error {
	if _, err := c.run(ctx, "set-buffer", "--", text); err != nil {
		return fmt.Errorf("tmux: set paste buffer: %w", err)
	}
	return nil
}

// PrefixKey returns the server's prefix key for display, e.g. "Ctrl-b";
// "Ctrl-b" if it cannot be read.
func (c *Client) PrefixKey(ctx context.Context) string {
	k, err := c.run(ctx, "show-options", "-gv", "prefix")
	if err != nil || k == "" || k == "None" {
		return "Ctrl-b"
	}
	if rest, ok := strings.CutPrefix(k, "C-"); ok {
		return "Ctrl-" + rest
	}
	return k
}

// KillSession kills the named session. A missing session or server is not an
// error.
func (c *Client) KillSession(ctx context.Context, name string) error {
	_, err := c.run(ctx, "kill-session", "-t", "="+name)
	if err != nil && !noSession(err) {
		return err
	}
	return nil
}

// AttachCmd returns the command that attaches the terminal to the named
// session: switch-client when already inside tmux ($TMUX set), attach-session
// otherwise. Stdio is left for the caller to wire.
func (c *Client) AttachCmd(name string) *exec.Cmd {
	verb := "attach-session"
	if os.Getenv("TMUX") != "" {
		verb = "switch-client"
	}
	return exec.Command("tmux", c.args(verb, "-t", "="+name)...)
}
