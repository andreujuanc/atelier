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
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/andreujuanc/atelier/internal/gen"
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

	// config, if set, is passed as -f on every command: tmux reads it (and
	// neither /etc/tmux.conf nor the user's) only when that command starts
	// the server (new-session, attach-session, start-server), and ignores
	// it otherwise. UseConfig sets it to the file atl generates; tests set
	// it to /dev/null.
	config string
}

// ServerConfig is the tmux configuration atl starts a server of its own
// with (see UseConfig, which adds the user's file, ServerConfigWith): what
// the box image's /etc/tmux.conf has. tmux out of the way: no status bar,
// mouse scrolling, Ctrl-q detaches (back to atl), and a terminal with 256
// colours and RGB. The copy relay (Copy) needs nothing here: load-buffer -w
// sends OSC 52 whatever set-clipboard says (checked with tmux 3.5a).
const ServerConfig = `# Written by atl: tmux reads this file when atl starts this workspace's
# tmux server. atl rewrites it when it differs; changes here do not last.
set -g status off
set -g mouse on
set -g escape-time 10
bind -n C-q detach-client
set -g default-terminal "tmux-256color"
set -as terminal-features ",*:RGB"
`

// ServerConfigWith is ServerConfig followed by `source-file -q user`: the
// user's own settings for atl's server (the workspace's tmux.conf), read
// last, so they win; -q: a missing file is no error (checked with tmux
// 3.5a). With user "" it is ServerConfig.
func ServerConfigWith(user string) string {
	if user == "" {
		return ServerConfig
	}
	return ServerConfig + "# Yours, if it exists (not rewritten by atl):\nsource-file -q " + sourcePath(user) + "\n"
}

// sourcePath quotes path, a file name, for source-file in a tmux config:
// source-file expands glob patterns (glob(3)), so *, ?, [ and \ are
// escaped with a backslash, and the result is single-quoted (no escapes or
// variables in there), a ' in it as '"'"'.
func sourcePath(path string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range path {
		switch r {
		case '*', '?', '[', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\'':
			b.WriteString(`'"'"'`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// DefaultSocket is the socket name of tmux's default server (`tmux -L
// default` is the server plain `tmux` reaches): the user's own.
const DefaultSocket = "default"

// OwnServer reports whether c names a server of atl's own: a socket name
// other than DefaultSocket. With no name tmux picks the server ($TMUX's,
// else the default one), which is the user's.
func (c *Client) OwnServer() bool { return c.Socket != "" && c.Socket != DefaultSocket }

// UseConfig makes c start its server with ServerConfigWith(user): it writes
// that to path (atomically, and only when the file's content differs; the
// parent directory is created) and passes path with -f from then on. A
// server that already runs keeps the options it has (see ConfigFiles). For
// a server that is not atl's own (OwnServer) it does nothing: if atl
// started it, -f would replace the user's tmux.conf (in the box:
// /etc/tmux.conf, which applies there). When the file cannot be written c
// is left without -f.
func (c *Client) UseConfig(path, user string) error {
	if !c.OwnServer() {
		return nil
	}
	conf := ServerConfigWith(user)
	if b, err := os.ReadFile(path); err != nil || string(b) != conf {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("tmux: write its config: %w", err)
		}
		if err := gen.WriteAtomic(path, []byte(conf)); err != nil {
			return fmt.Errorf("tmux: write its config: %w", err)
		}
	}
	c.config = path
	return nil
}

// Config is the file c passes with -f ("" for none).
func (c *Client) Config() string { return c.config }

// ConfigFiles returns the config files the running server was started with,
// as tmux's #{config_files} lists them, comma-separated (those given with
// -f, whether or not they existed, else the default ones it found; see
// ConfigListed), and whether a server runs.
func (c *Client) ConfigFiles(ctx context.Context) (list string, running bool, err error) {
	out, err := c.run(ctx, "display-message", "-p", "#{config_files}")
	if err != nil {
		if noServer(err) || serverExited(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return out, true, nil
}

// ConfigListed reports whether path is one of the files in list, a
// #{config_files} value. tmux joins the files with commas without quoting
// them, so a path with a comma cannot be split out of it: path counts when
// it is the whole list or a comma-delimited part of it.
func ConfigListed(list, path string) bool {
	if path == "" {
		return false
	}
	return list == path || strings.HasPrefix(list, path+",") || strings.HasSuffix(list, ","+path) ||
		strings.Contains(list, ","+path+",")
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
	// GlobalEnv is set in the server's global environment, which the window
	// inherits, in the same tmux invocation that creates the window (and
	// the server, if none runs yet: a server takes the environment of the
	// process that starts it, which need not have these values): the values
	// reach tmux on its standard input, never on a command line (see
	// globalEnvScript).
	GlobalEnv map[string]string
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
	return c.runIn(ctx, "", args...)
}

// runIn is run with stdin as tmux's standard input.
func (c *Client) runIn(ctx context.Context, stdin string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "tmux", c.args(args...)...)
	cmd.Env = c.serverEnv()
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
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

// sessionVars are what a tmux server started from inside a session (a
// Claude window, a task container's gate) must not take into its global
// environment, which every window it opens later inherits.
var sessionVars = []string{"CLAUDECODE", "ATL_SESSION", "ATL_CONTROL_SOCK", "ATL_TASK", "ATL_TASK_DIR", "ATL_PORT_OFFSET"}

// serverEnv is the environment of the tmux commands c runs (a command that
// starts the server gives it this one): atl's, without sessionVars, and,
// with a socket name, without TMUX and TMUX_PANE (another server's).
func (c *Client) serverEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		drop := c.Socket != "" && (k == "TMUX" || k == "TMUX_PANE")
		for _, v := range sessionVars {
			drop = drop || k == v
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
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

// serverExited reports whether err is tmux's "server exited unexpectedly":
// the client reached a server that was on its way out (e.g. just after
// kill-server, or as its last session closed), so there is none now.
func serverExited(err error) bool {
	var te *Error
	return errors.As(err, &te) && strings.Contains(te.Stderr, "server exited unexpectedly")
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

// globalEnvScript is the tmux commands that set env in the server's global
// environment, for `source-file -`: on tmux's standard input, so that no
// value appears on a command line, nor in an error (see redact). A value
// may not contain a single quote or a control character. "" for no env.
func globalEnvScript(env map[string]string) (string, error) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var script strings.Builder
	for _, k := range keys {
		v := env[k]
		if k == "" || strings.ContainsFunc(k+v, func(r rune) bool { return r == '\'' || r < 0x20 || r == 0x7f }) {
			return "", fmt.Errorf("tmux: $%s has a character atl cannot pass to tmux (a quote or a control character)", k)
		}
		fmt.Fprintf(&script, "set-environment -g '%s' '%s'\n", k, v)
	}
	return script.String(), nil
}

// redact removes env's values from a tmux error.
func redact(err error, env map[string]string) error {
	var te *Error
	if !errors.As(err, &te) {
		return err
	}
	msg := te.Stderr
	for _, v := range env {
		if v != "" {
			msg = strings.ReplaceAll(msg, v, "…")
		}
	}
	return &Error{Args: te.Args, Stderr: msg, Err: te.Err}
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
	script, err := globalEnvScript(spec.GlobalEnv)
	if err != nil {
		return "", err
	}
	build := func(exists bool) []string {
		var args []string
		if script != "" {
			// One invocation: if it starts the server, the variables are
			// set in it before the window is created.
			args = []string{"source-file", "-", ";"}
		}
		if exists {
			args = append(args, "new-window", "-t", "="+spec.Session+":")
		} else {
			args = append(args, "new-session", "-d", "-s", spec.Session)
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
	out, err := c.runIn(ctx, script, build(exists)...)
	if serverExited(err) {
		// Reached a server on its way out (just killed, or its last
		// session just closed), which ran nothing: once more, with the
		// server that is there now, or a new one.
		exists = c.HasSession(ctx, spec.Session)
		out, err = c.runIn(ctx, script, build(exists)...)
	}
	var te *Error
	if !exists && err != nil && out == "" && errors.As(err, &te) && strings.Contains(te.Stderr, "duplicate session") {
		// Lost a race with another atl creating the same session.
		out, err = c.runIn(ctx, script, build(true)...)
	}
	if err != nil {
		return "", fmt.Errorf("tmux: launch window in session %s: %w", spec.Session, redact(err, spec.GlobalEnv))
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

// SetWindowOption sets option opt of window (an id, "@N") to value.
func (c *Client) SetWindowOption(ctx context.Context, window, opt, value string) error {
	if !strings.HasPrefix(window, "@") {
		return fmt.Errorf("tmux: set window option: bad window id %q", window)
	}
	if _, err := c.run(ctx, "set-option", "-w", "-t", window, opt, value); err != nil {
		return fmt.Errorf("tmux: set %s of window %s: %w", opt, window, err)
	}
	return nil
}

// SetSessionOption sets option opt of the named session (exact name) to
// value.
func (c *Client) SetSessionOption(ctx context.Context, session, opt, value string) error {
	if _, err := c.run(ctx, "set-option", "-t", "="+session+":", opt, value); err != nil {
		return fmt.Errorf("tmux: set %s of session %s: %w", opt, session, err)
	}
	return nil
}

// CapturePane returns the text of the first pane of window (an id, "@N"),
// its scrollback included, with lines tmux wrapped joined (capture-pane -J),
// and the pane's width in columns. The text stays in this process: it is on
// no command line and in no error.
func (c *Client) CapturePane(ctx context.Context, window string) (text string, width int, err error) {
	if !strings.HasPrefix(window, "@") {
		return "", 0, fmt.Errorf("tmux: capture pane: bad window id %q", window)
	}
	w, err := c.run(ctx, "display-message", "-p", "-t", window, "#{pane_width}")
	if err != nil {
		return "", 0, fmt.Errorf("tmux: capture pane of window %s: %w", window, err)
	}
	width, _ = strconv.Atoi(strings.TrimSpace(w))
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "tmux", c.args("capture-pane", "-p", "-J", "-S", "-", "-E", "-", "-t", window)...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		return "", 0, fmt.Errorf("tmux: capture pane of window %s: %s", window, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), width, nil
}

// SetBuffer puts text in a new automatic paste buffer, the one tmux's
// paste-buffer (prefix ]) pastes by default.
func (c *Client) SetBuffer(ctx context.Context, text string) error {
	if _, err := c.run(ctx, "set-buffer", "--", text); err != nil {
		return fmt.Errorf("tmux: set paste buffer: %w", err)
	}
	return nil
}

// ErrNoClient is returned by Copy when no terminal is attached to the
// session.
var ErrNoClient = errors.New("no terminal is attached to the session")

// Copy puts text in a new automatic paste buffer and in the clipboard of the
// terminal attached to the named session (exact name), the one active last
// if there are several: load-buffer -w, which sends it to that terminal with
// the xterm escape sequence (OSC 52) if tmux takes the terminal for one that
// supports it (its terminfo's Ms, or the clipboard terminal feature; tmux
// gives xterm* that by default), whatever set-clipboard says (tmux 3.5a
// sends it with set-clipboard off too, checked). This is what
// Claude Code does itself when it runs in tmux. Without an attached
// terminal it returns ErrNoClient (wrapped) and copies nothing: whoever
// copies does so where the user is looking.
//
// The text reaches tmux on its standard input, never on a command line, and
// is in no error.
func (c *Client) Copy(ctx context.Context, session string, text []byte) error {
	if len(text) == 0 {
		return errors.New("tmux: copy: nothing to copy")
	}
	out, err := c.run(ctx, "list-clients", "-t", "="+session, "-F", "#{client_activity} #{client_name}")
	if err != nil {
		if noSession(err) {
			return fmt.Errorf("tmux: copy: session %s: %w", session, ErrNoClient)
		}
		return fmt.Errorf("tmux: copy: %w", err)
	}
	client, last := "", int64(-1)
	for _, line := range strings.Split(out, "\n") {
		act, name, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || name == "" {
			continue
		}
		if n, err := strconv.ParseInt(act, 10, 64); err == nil && n > last {
			client, last = name, n
		}
	}
	if client == "" {
		return fmt.Errorf("tmux: copy: session %s: %w", session, ErrNoClient)
	}
	if _, err := c.runIn(ctx, string(text), "load-buffer", "-w", "-t", client, "-"); err != nil {
		return fmt.Errorf("tmux: copy to the terminal of session %s: %w", session, err)
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
// session: switch-client when already inside a client of this server
// ($TMUX names its socket), attach-session otherwise. From inside another
// server (another workspace's, or the user's own tmux) that is a nested
// client, so TMUX is removed from its environment (tmux refuses to nest
// while it is set). Stdio is left for the caller to wire.
func (c *Client) AttachCmd(name string) *exec.Cmd {
	if os.Getenv("TMUX") == "" {
		return exec.Command("tmux", c.args("attach-session", "-t", "="+name)...)
	}
	if c.inside() {
		return exec.Command("tmux", c.args("switch-client", "-t", "="+name)...)
	}
	cmd := exec.Command("tmux", c.args("attach-session", "-t", "="+name)...)
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); k != "TMUX" && k != "TMUX_PANE" {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	return cmd
}

// inside reports whether $TMUX (set) is a client of c's server: always with
// no socket name (tmux then picks $TMUX's server), else when $TMUX's
// socket (its first comma-separated field) is the file SocketPath names.
func (c *Client) inside() bool {
	if c.Socket == "" {
		return true
	}
	cur, _, _ := strings.Cut(os.Getenv("TMUX"), ",")
	a, err := os.Stat(cur)
	if err != nil {
		return false
	}
	b, err := os.Stat(c.SocketPath())
	return err == nil && os.SameFile(a, b)
}

// SocketPath is the path of the socket `tmux -L Socket` uses:
// $TMUX_TMPDIR (else /tmp)/tmux-<uid>/<Socket>. "" with no socket name.
func (c *Client) SocketPath() string {
	if c.Socket == "" {
		return ""
	}
	base := os.Getenv("TMUX_TMPDIR")
	if base == "" {
		base = "/tmp"
	}
	return filepath.Join(base, fmt.Sprintf("tmux-%d", os.Getuid()), c.Socket)
}
