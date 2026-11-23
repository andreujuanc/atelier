package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// A task container has neither git credentials nor atelier's database, so
// atl in it asks the task's gate process, which runs where atl runs. The gate
// listens on a second socket in the run dir for that and takes one request
// per connection, of a fixed shape (ControlRequest):
//
//   - push and fetch: the gate runs `atl push` or `atl fetch` for its own task
//     (named by database id and slug), with at most a repo name and --force;
//   - hook, statusline, note and notes: the gate answers them itself (Task),
//     on its own task's rows only;
//   - copy: the gate puts the body in the user's clipboard (Copy), through
//     the terminal attached to its own task's tmux session (see CopyMain);
//   - atl: only the assistant's gate (Control.Workspace set when it
//     started), never a task's: it runs an allowed atl command line
//     (CheckAssistantArgs) in its workspace, with the body as stdin.
//
// Protocol: the client sends the request as one line of JSON, followed by
// exactly req.Body bytes (the hook's or status line's input, a note's text).
// The gate answers with lines of JSON (controlReply): output as it comes
// (stdout, and for task operations stderr apart), then the exit status. The
// exit status of an operation the gate carried out names that operation; a
// refusal does not.

// InControlSocket is the control socket as a task container sees it.
const InControlSocket = RunMount + "/atl.sock"

// ControlSocketPath is the control socket in runDir.
func ControlSocketPath(runDir string) string { return filepath.Join(runDir, "atl.sock") }

// Operations a task may ask its gate for.
const (
	OpPush       = "push"
	OpFetch      = "fetch"
	OpHook       = "hook"       // `atl hook <event>`
	OpStatusLine = "statusline" // `atl statusline`
	OpNote       = "note"       // `atl note`: append a line to the task's notes
	OpNotes      = "notes"      // `atl notes`: print, replace or clear them
	OpCopy       = "copy"       // wl-copy (atl, see CopyMain): put the body in the user's clipboard
	OpAtl        = "atl"        // the assistant's gate only: run atl with Args, the body as stdin
)

// Landing is a tool target of a PreToolUse hook as the task container
// resolves it: Given (an absolute form of the target) lands at Path.
type Landing struct {
	Given string `json:"given"`
	Path  string `json:"path"`
}

// ControlRequest is what a task container may ask its gate to do. Fields an
// operation does not take must be empty.
type ControlRequest struct {
	Op    string `json:"op"`              // one of the Op* constants
	Repo  string `json:"repo,omitempty"`  // push, fetch: only this repo of the task; "" → all
	Force bool   `json:"force,omitempty"` // push: --force (with lease)

	Event    string            `json:"event,omitempty"`    // hook: the hook event
	Env      map[string]string `json:"env,omitempty"`      // hook, statusline: the caller's values of HookEnv / StatusLineEnv
	ReadErr  string            `json:"read_err,omitempty"` // hook: reading the input failed after Body bytes
	Landings []Landing         `json:"landings,omitempty"` // hook: where PreToolUse targets land in the container
	Author   string            `json:"author,omitempty"`   // note: user or assistant
	Set      bool              `json:"set,omitempty"`      // notes: replace them with the body
	Clear    bool              `json:"clear,omitempty"`    // notes: clear them (with Set: allow an empty body)
	Args     []string          `json:"args,omitempty"`     // atl: the command line, without the binary
	Body     int64             `json:"body,omitempty"`     // bytes that follow the request line
}

// The variables of the caller's environment a request may carry. Nothing else
// of the container's environment reaches the gate.
var (
	HookEnv       = []string{"ATL_SESSION", "ATL_CONTEXT"}
	StatusLineEnv = []string{"NO_COLOR", "COLORFGBG"}
)

// Task answers the task operations (hook, statusline, note, notes) in the
// gate process. It acts on the gate's own task only, whatever the request
// says; see hook.TaskServer.
type Task interface {
	Do(req ControlRequest, body []byte, stdout, stderr io.Writer) int
}

type controlReply struct {
	Output string `json:"output,omitempty"` // stdout (push and fetch: all their output)
	Stderr string `json:"stderr,omitempty"`
	Exit   *int   `json:"exit,omitempty"`
	Op     string `json:"op,omitempty"` // with Exit: the operation that ran ("" for a refusal)
}

// controlTimeout bounds a push or fetch. It is long: a push may wait for the
// user to touch a hardware key.
const controlTimeout = 10 * time.Minute

// MaxRequestLine bounds the request line, newline included (the landings
// of a PreToolUse hook can hold a few long paths).
const MaxRequestLine = 64 << 10

// MaxHookBody bounds a hook's input: 64 MiB (hook.maxInput) plus the one
// byte that tells an oversized input.
const MaxHookBody = 64<<20 + 1

// maxBody bounds the body of each operation.
func maxBody(op string) int64 {
	switch op {
	case OpHook:
		return MaxHookBody
	case OpStatusLine, OpNote, OpNotes, OpAtl:
		return 1 << 20
	case OpCopy:
		return MaxCopy
	}
	return 0
}

// maxAtlOutput bounds what an atl command run for the assistant may print
// (stdout and stderr together); the rest is dropped, and the gate says so.
const maxAtlOutput = 16 << 20

// maxAtlRunning bounds the atl commands running for the assistant at once;
// a request beyond it is refused at once.
const maxAtlRunning = 3

// atlWriteTimeout bounds each reply write of an atl request.
const atlWriteTimeout = 30 * time.Second

func (c *Control) writeTimeout() time.Duration {
	if c.WriteTimeout > 0 {
		return c.WriteTimeout
	}
	return atlWriteTimeout
}

// MaxCopy bounds what a task can put in the user's clipboard at once: 1 MiB,
// far more than a selection on a screen, and little enough for tmux to hold
// (each copy is a paste buffer there too).
const MaxCopy = 1 << 20

// copyTimeout bounds putting one copy in the clipboard (two tmux commands).
const copyTimeout = 10 * time.Second

// bodyTimeout bounds reading a request's body.
const bodyTimeout = 30 * time.Second

// exitRefused is the exit status of a refused request.
const exitRefused = 2

// Control is the gate's side of atl in a task container.
type Control struct {
	Slug    string        // the gate's task: the only one a request can act on
	TaskID  int64         // its database id (push and fetch name the task by both)
	Atl     string        // the atl binary that runs push and fetch
	Task    Task          // answers the task operations; nil: refused
	Log     *log.Logger   // requests and refusals; nil: discarded
	Timeout time.Duration // push and fetch; 0 → controlTimeout
	// Copy puts text in the user's clipboard where atl runs (for the gate:
	// through the terminal attached to the task's own tmux session, see
	// tmux.Client.Copy); nil: copy is refused. The text comes from the task:
	// it only ever goes to Copy, as bytes, and is never logged.
	Copy func(ctx context.Context, text []byte) error
	// Workspace, set only in the assistant's gate, is its workspace dir:
	// atl requests (OpAtl) run `<Atl> -w <Workspace> <command>
	// --for-assistant <rest>` there. Empty
	// (every task's gate): atl requests are refused.
	Workspace string
	// Resolve, needed with Workspace, reads an atl command line (what the
	// gate will exec, -w included) as atl's own command tree will: the
	// CommandPath of the command it runs ("atl notes") and whether that
	// reads stdin (notes --set); an error refuses the request.
	Resolve func(argv []string) (path string, readsStdin bool, err error)
	// WriteTimeout bounds each reply write of an atl request (a client that
	// stops reading); 0 → atlWriteTimeout.
	WriteTimeout time.Duration

	atlOnce  sync.Once
	atlSlots chan struct{} // the atl commands running for the assistant

	bigOnce sync.Once
	big     chan struct{} // slots for bodies over bigBody
	copyMu  sync.Mutex    // one copy at a time: in the order they came, one tmux at a time
}

// bigBody is the body size above which a request waits for one of
// maxBigBodies slots: a task cannot make its gate hold more than a few
// 64 MiB hook inputs at once.
const (
	bigBody      = 1 << 20
	maxBigBodies = 2
)

func (c *Control) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log.Printf(format, args...)
	}
}

// Serve answers requests on l until l is closed.
func (c *Control) Serve(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go c.handle(conn)
	}
}

// validControlRepo reports whether name may be passed on as a repo name: a
// single plain path component that cannot be taken for a flag.
func validControlRepo(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 255 ||
		strings.HasPrefix(name, "-") || strings.ContainsAny(name, `/\`) {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return strings.TrimSpace(name) == name
}

// validEvent reports whether e may be a hook event name: short, printable.
func validEvent(e string) bool {
	if len(e) > 64 || !utf8.ValidString(e) {
		return false
	}
	for _, r := range e {
		if r <= 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// check returns why req is refused, or nil.
func (c *Control) check(req ControlRequest) error {
	notFor := func(what string) error { return fmt.Errorf("%s is not for %s", what, req.Op) }
	remote := req.Op == OpPush || req.Op == OpFetch
	switch req.Op {
	case OpPush, OpFetch, OpHook, OpStatusLine, OpNote, OpNotes, OpCopy, OpAtl:
	default:
		return fmt.Errorf("unknown operation %q (a task may ask for push, fetch, hook, statusline, note, notes or copy)", req.Op)
	}
	switch {
	case len(req.Args) > 0 && req.Op != OpAtl:
		return notFor("a command line")
	case req.Repo != "" && !remote:
		return notFor("a repo")
	case req.Repo != "" && !validControlRepo(req.Repo):
		return fmt.Errorf("invalid repo name %q", req.Repo)
	case req.Force && req.Op != OpPush:
		return errors.New("force is for push only")
	case (req.Event != "" || req.ReadErr != "" || len(req.Landings) > 0) && req.Op != OpHook:
		return notFor("a hook event")
	case req.Event != "" && !validEvent(req.Event):
		return errors.New("invalid hook event")
	case req.Author != "" && req.Op != OpNote:
		return notFor("an author")
	case (req.Set || req.Clear) && req.Op != OpNotes:
		return notFor("set or clear")
	case req.Body < 0 || req.Body > maxBody(req.Op):
		return fmt.Errorf("a body of %d bytes is too large for %s", req.Body, req.Op)
	case req.Op == OpNotes && req.Body > 0 && !req.Set:
		return notFor("a body without set")
	}
	allowed := map[string]bool{}
	switch req.Op {
	case OpHook:
		for _, k := range HookEnv {
			allowed[k] = true
		}
	case OpStatusLine:
		for _, k := range StatusLineEnv {
			allowed[k] = true
		}
	}
	for k := range req.Env {
		if !allowed[k] {
			return fmt.Errorf("variable %q is not for %s", k, req.Op)
		}
	}
	if len(req.Landings) > 64 {
		return errors.New("too many landings")
	}
	return nil
}

// argv is the atl command line (without the binary) for a push or fetch. The
// task is named by id and slug: atl acts only if both still name it.
func (c *Control) argv(req ControlRequest) []string {
	args := []string{req.Op, "--task-id", strconv.FormatInt(c.TaskID, 10), c.Slug}
	if req.Repo != "" {
		args = append(args, "--repo", req.Repo)
	}
	if req.Force {
		args = append(args, "--force")
	}
	return args
}

// replyWriter sends what is written to it to the client as replies.
type replyWriter struct {
	mu  sync.Mutex
	enc *json.Encoder
	// conn and perWrite, when set (the assistant's atl op), bound each
	// write: a client that stops reading cannot hold the gate. After a write
	// fails, the rest is dropped.
	conn     net.Conn
	perWrite time.Duration
	broken   bool
}

func (w *replyWriter) send(r controlReply) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.broken {
		return
	}
	if w.conn != nil && w.perWrite > 0 {
		w.conn.SetWriteDeadline(time.Now().Add(w.perWrite))
	}
	if err := w.enc.Encode(r); err != nil && w.conn != nil {
		w.broken = true
	} // without conn: a client that went away is noticed by the reader
}

func (w *replyWriter) Write(p []byte) (int, error) {
	w.send(controlReply{Output: string(p)})
	return len(p), nil
}

func (w *replyWriter) exit(code int, op string) { w.send(controlReply{Exit: &code, Op: op}) }

// stderrWriter sends what is written to it as stderr replies.
type stderrWriter struct{ w *replyWriter }

func (s stderrWriter) Write(p []byte) (int, error) {
	s.w.send(controlReply{Stderr: string(p)})
	return len(p), nil
}

func (c *Control) handle(conn net.Conn) {
	defer conn.Close()
	out := &replyWriter{enc: json.NewEncoder(conn)}
	refuse := func(reason string) {
		c.logf("deny  atl request: %s", reason)
		fmt.Fprintf(stderrWriter{out}, "atelier gate: %s\n", reason)
		out.exit(exitRefused, "")
	}

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	in := bufio.NewReaderSize(conn, MaxRequestLine)
	line, err := in.ReadSlice('\n')
	if err != nil {
		refuse("unreadable request")
		return
	}
	var req ControlRequest
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		refuse("unreadable request")
		return
	}
	if err := c.check(req); err != nil {
		refuse(err.Error())
		return
	}
	if req.Op == OpCopy {
		c.copy(conn, in, out, req, refuse)
		return
	}
	if req.Op == OpAtl {
		c.atl(conn, in, out, req, refuse)
		return
	}
	if req.Op != OpPush && req.Op != OpFetch {
		c.task(conn, in, out, req, refuse)
		return
	}
	conn.SetReadDeadline(time.Time{})
	if c.TaskID <= 0 || c.Atl == "" {
		refuse(req.Op + " is not served by this gate")
		return
	}
	args := c.argv(req)
	logArgs := strings.Join(args, " ")
	c.logf("run   atl %s", logArgs)
	code := c.run(in, args, nil, out, out)
	c.logf("done  atl %s: exit %d", logArgs, code)
	out.exit(code, req.Op)
}

// run runs c.Atl with args where atl runs, with stdin (nil: none), its output
// to stdout and stderr, and returns its exit status. It stops the command
// (and what it started) after c.Timeout, or when the client hangs up, which
// it notices by reading in to the end. Messages of the gate go to stdout.
func (c *Control) run(in io.Reader, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = controlTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// The client keeps its side open until the end; hanging up (ctrl-c in
	// the task) stops the command.
	go func() {
		io.Copy(io.Discard, in)
		cancel()
	}()

	cmd := exec.CommandContext(ctx, c.Atl, args...)
	cmd.Env = controlEnv()
	cmd.Stdin = stdin
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// Its own process group, so that stopping it also stops git and ssh.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	code := 0
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		code = exit.ExitCode()
		if code < 0 { // killed by a signal
			code = 1
		}
	case err != nil:
		fmt.Fprintf(stdout, "atelier gate: %v\n", err)
		code = 1
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		fmt.Fprintf(stdout, "atelier gate: stopped after %s\n", timeout)
	}
	return code
}

// atl runs, in the assistant's gate only, an atl command line the assistant
// may run in the gate's workspace: `<Atl> -w <Workspace> <command>
// --for-assistant <rest>` (ForAssistantFlag), exec'd like the CLI, with the
// gate's environment (never the requester's), the body as its stdin, stdout and stderr apart
// (at most maxAtlOutput together) and the exit status passed back. A task's
// gate (no Workspace) refuses it. The command line must pass
// CheckAssistantArgs and resolve (Resolve, cobra's own reading of it) to an
// allowed command (AssistantCommandPath); a body only goes with `atl notes
// --set`; at most maxAtlRunning run at once (more are refused, not queued:
// each is a process where atl runs, for up to 10 minutes). The log gets the command and the exit
// status, never stdin nor the other arguments (a note's text).
func (c *Control) atl(conn net.Conn, in *bufio.Reader, out *replyWriter, req ControlRequest, refuse func(string)) {
	if c.Workspace == "" || c.Atl == "" || c.Resolve == nil {
		refuse(req.Op + " is not served by this gate")
		return
	}
	if err := CheckAssistantArgs(req.Args); err != nil {
		refuse(err.Error())
		return
	}
	// The gate's flag right after the command (never a flag, see
	// CheckAssistantArgs), where no "--" can come before it.
	argv := append([]string{"-w", c.Workspace, req.Args[0], "--" + ForAssistantFlag}, req.Args[1:]...)
	// As cobra will run it: both checks must pass.
	path, readsStdin, err := c.Resolve(argv)
	switch {
	case err != nil:
		refuse("atl " + req.Args[0] + ": " + err.Error())
		return
	case !AssistantCommandPath(path):
		refuse(fmt.Sprintf("this command line runs %q, which does not run for the assistant", path))
		return
	case req.Body > 0 && !(path == "atl notes" && readsStdin):
		refuse("standard input is only for atl notes --set")
		return
	}
	c.atlOnce.Do(func() { c.atlSlots = make(chan struct{}, maxAtlRunning) })
	var release func()
	select {
	case c.atlSlots <- struct{}{}:
		var once sync.Once
		release = func() { once.Do(func() { <-c.atlSlots }) }
		defer release()
	default:
		refuse(fmt.Sprintf("%d atl commands of the assistant are running already: try again once one has finished", maxAtlRunning))
		return
	}
	conn.SetReadDeadline(time.Now().Add(bodyTimeout))
	stdin := make([]byte, req.Body)
	if _, err := io.ReadFull(in, stdin); err != nil {
		refuse("request body cut short")
		return
	}
	conn.SetReadDeadline(time.Time{})
	sub := strings.TrimPrefix(path, "atl ")
	lim := &outputLimit{left: maxAtlOutput}
	stdout, stderr := lim.writer(out), lim.writer(stderrWriter{out})
	c.logf("run   atl %s (for the assistant)", sub)
	out.mu.Lock()
	out.conn, out.perWrite = conn, c.writeTimeout()
	out.mu.Unlock()
	code := c.run(in, argv, bytes.NewReader(stdin), stdout, stderr)
	release() // the process is done: its slot is free, whatever the client does now
	if lim.cut() {
		fmt.Fprintf(stderrWriter{out}, "atelier gate: output cut at %d MiB\n", maxAtlOutput>>20)
	}
	c.logf("done  atl %s (for the assistant): exit %d", sub, code)
	out.exit(code, req.Op)
}

// outputLimit lets through the first left bytes written to its writers
// together and drops the rest.
type outputLimit struct {
	mu      sync.Mutex
	left    int
	dropped bool
}

func (l *outputLimit) writer(w io.Writer) io.Writer { return limitedWriter{l, w} }

func (l *outputLimit) cut() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dropped
}

type limitedWriter struct {
	l *outputLimit
	w io.Writer
}

func (lw limitedWriter) Write(p []byte) (int, error) {
	lw.l.mu.Lock()
	n := min(len(p), lw.l.left)
	lw.l.left -= n
	if n < len(p) {
		lw.l.dropped = true
	}
	lw.l.mu.Unlock()
	if n > 0 {
		lw.w.Write(p[:n])
	}
	return len(p), nil // what is dropped counts as written: the command goes on
}

// task reads the body of a task operation and has c.Task answer it.
func (c *Control) task(conn net.Conn, in *bufio.Reader, out *replyWriter, req ControlRequest, refuse func(string)) {
	if c.Task == nil {
		refuse(req.Op + " is not served by this gate")
		return
	}
	if req.Body > bigBody {
		c.bigOnce.Do(func() { c.big = make(chan struct{}, maxBigBodies) })
		select {
		case c.big <- struct{}{}:
			defer func() { <-c.big }()
		case <-time.After(bodyTimeout):
			refuse("too many large requests at once")
			return
		}
	}
	conn.SetReadDeadline(time.Now().Add(bodyTimeout))
	body := make([]byte, req.Body)
	if _, err := io.ReadFull(in, body); err != nil {
		refuse("request body cut short")
		return
	}
	conn.SetReadDeadline(time.Time{})
	code := c.Task.Do(req, body, out, stderrWriter{out})
	out.exit(code, req.Op)
}

// copy reads the body of a copy (at most MaxCopy bytes, which check saw to)
// and has c.Copy put it in the user's clipboard. The log gets the time, the
// task and the size, never the text; an error of Copy is tmux's, which does
// not echo its input.
func (c *Control) copy(conn net.Conn, in *bufio.Reader, out *replyWriter, req ControlRequest, refuse func(string)) {
	if c.Copy == nil {
		refuse(req.Op + " is not served by this gate")
		return
	}
	conn.SetReadDeadline(time.Now().Add(bodyTimeout))
	text := make([]byte, req.Body)
	if _, err := io.ReadFull(in, text); err != nil {
		refuse("request body cut short")
		return
	}
	conn.SetReadDeadline(time.Time{})
	if len(text) == 0 {
		out.exit(0, req.Op) // nothing to copy
		return
	}
	c.copyMu.Lock()
	defer c.copyMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), copyTimeout)
	defer cancel()
	if err := c.Copy(ctx, text); err != nil {
		c.logf("fail  copy of %d bytes for %s: %v", len(text), c.Slug, err)
		fmt.Fprintf(stderrWriter{out}, "atelier gate: copy: %v\n", err)
		out.exit(1, req.Op)
		return
	}
	c.logf("copy  %d bytes for %s to the clipboard", len(text), c.Slug)
	out.exit(0, req.Op)
}

// controlEnv is the environment of a command the gate runs: its own, minus
// what would make atl think it runs in a task container (or the assistant's).
func controlEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, InEnv+"=") && !strings.HasPrefix(kv, AssistantEnv+"=") {
			env = append(env, kv)
		}
	}
	return env
}

// ControlClient sends req to the gate's control socket and copies the
// command's output to out. It returns the command's exit status; an error
// means the gate could not be asked or did not answer to the end.
func ControlClient(socket string, req ControlRequest, out io.Writer) (int, error) {
	code, _, err := Ask(socket, req, nil, out, out, 0)
	return code, err
}

// ErrGate wraps the errors of Ask that mean the gate could not be asked or
// did not answer to the end.
var ErrGate = errors.New("the task's gate")

// Ask sends req with body (req.Body is set to its length) to the gate's
// control socket (InControlSocket if socket is "") and copies the gate's
// stdout and stderr replies to stdout and stderr as they come. It returns
// the exit status and whether the gate carried the operation out (false: it
// refused the request). timeout, if not 0, bounds the whole exchange.
func Ask(socket string, req ControlRequest, body []byte, stdout, stderr io.Writer, timeout time.Duration) (code int, ran bool, err error) {
	if socket == "" {
		socket = InControlSocket
	}
	req.Body = int64(len(body))
	line, err := json.Marshal(req)
	if err != nil {
		return 0, false, err
	}
	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return 0, false, fmt.Errorf("cannot reach %w: %v", ErrGate, err)
	}
	defer conn.Close()
	if timeout > 0 {
		conn.SetDeadline(time.Now().Add(timeout))
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return 0, false, fmt.Errorf("cannot reach %w: %v", ErrGate, err)
	}
	var writeErr error
	if len(body) > 0 {
		// A gate that refuses the request line stops reading: its answer
		// still comes.
		_, writeErr = conn.Write(body)
	}
	dec := json.NewDecoder(conn)
	for {
		var r controlReply
		if err := dec.Decode(&r); err != nil {
			if writeErr != nil {
				return 0, false, fmt.Errorf("%w did not take the request: %v", ErrGate, writeErr)
			}
			return 0, false, fmt.Errorf("%w closed the connection before the command ended", ErrGate)
		}
		if r.Output != "" {
			io.WriteString(stdout, r.Output)
		}
		if r.Stderr != "" {
			io.WriteString(stderr, r.Stderr)
		}
		if r.Exit != nil {
			return *r.Exit, r.Op != "" && r.Op == req.Op, nil
		}
	}
}
