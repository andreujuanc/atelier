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
	"strings"
	"sync"
	"syscall"
	"time"
)

// A task container has no git credentials, so `atl push` and `atl fetch` in
// it ask the task's gate process, which runs them where atl runs (with the
// SSH agent and git config of that place). The gate listens on a second
// socket in the run dir for that; it takes one request per connection, of a
// fixed shape (ControlRequest), and only ever runs `atl push <slug>` or
// `atl fetch <slug>` for its own task, with at most a repo name and --force.
//
// Protocol: the client sends the request as one line of JSON. The gate
// answers with lines of JSON (controlReply): the command's combined output
// as it comes, then its exit status.

// InControlSocket is the control socket as a task container sees it.
const InControlSocket = RunMount + "/atl.sock"

// ControlSocketPath is the control socket in runDir.
func ControlSocketPath(runDir string) string { return filepath.Join(runDir, "atl.sock") }

// Operations a task may ask its gate for.
const (
	OpPush  = "push"
	OpFetch = "fetch"
)

// ControlRequest is what a task container may ask its gate to do.
type ControlRequest struct {
	Op    string `json:"op"`              // OpPush or OpFetch
	Repo  string `json:"repo,omitempty"`  // only this repo of the task; "" → all
	Force bool   `json:"force,omitempty"` // push only: --force (with lease)
}

type controlReply struct {
	Output string `json:"output,omitempty"`
	Exit   *int   `json:"exit,omitempty"`
}

// controlTimeout bounds one request. It is long: a push may wait for the
// user to touch a hardware key.
const controlTimeout = 10 * time.Minute

// maxControlRequest bounds the request line.
const maxControlRequest = 4 << 10

// exitRefused is the exit status of a refused request.
const exitRefused = 2

// Control is the gate's side of `atl push` and `atl fetch` in a task
// container.
type Control struct {
	Slug    string        // the gate's task: the only one a request can act on
	Atl     string        // the atl binary that runs the request
	Log     *log.Logger   // requests and refusals; nil: discarded
	Timeout time.Duration // 0 → controlTimeout
}

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

// argv is the atl command line (without the binary) for req, or an error
// saying why req is refused.
func (c *Control) argv(req ControlRequest) ([]string, error) {
	if req.Op != OpPush && req.Op != OpFetch {
		return nil, fmt.Errorf("unknown operation %q (a task may ask for push or fetch)", req.Op)
	}
	if req.Repo != "" && !validControlRepo(req.Repo) {
		return nil, fmt.Errorf("invalid repo name %q", req.Repo)
	}
	if req.Force && req.Op != OpPush {
		return nil, errors.New("force is for push only")
	}
	args := []string{req.Op, c.Slug}
	if req.Repo != "" {
		args = append(args, "--repo", req.Repo)
	}
	if req.Force {
		args = append(args, "--force")
	}
	return args, nil
}

// replyWriter sends what is written to it to the client as output replies.
type replyWriter struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (w *replyWriter) send(r controlReply) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.enc.Encode(r) // a client that went away is noticed by the reader
}

func (w *replyWriter) Write(p []byte) (int, error) {
	w.send(controlReply{Output: string(p)})
	return len(p), nil
}

func (w *replyWriter) exit(code int) { w.send(controlReply{Exit: &code}) }

func (c *Control) handle(conn net.Conn) {
	defer conn.Close()
	out := &replyWriter{enc: json.NewEncoder(conn)}
	refuse := func(reason string) {
		c.logf("deny  atl request: %s", reason)
		fmt.Fprintf(out, "atelier gate: %s\n", reason)
		out.exit(exitRefused)
	}

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	in := bufio.NewReaderSize(conn, maxControlRequest)
	line, err := in.ReadSlice('\n')
	if err != nil {
		refuse("unreadable request")
		return
	}
	conn.SetReadDeadline(time.Time{})
	var req ControlRequest
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		refuse("unreadable request")
		return
	}
	args, err := c.argv(req)
	if err != nil {
		refuse(err.Error())
		return
	}

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
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = out, out
	// Its own process group, so that stopping it also stops git and ssh.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	c.logf("run   atl %s", strings.Join(args, " "))
	err = cmd.Run()
	code := 0
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		code = exit.ExitCode()
		if code < 0 { // killed by a signal
			code = 1
		}
	case err != nil:
		fmt.Fprintf(out, "atelier gate: %v\n", err)
		code = 1
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		fmt.Fprintf(out, "atelier gate: stopped after %s\n", timeout)
	}
	c.logf("done  atl %s: exit %d", strings.Join(args, " "), code)
	out.exit(code)
}

// controlEnv is the environment of a command the gate runs: its own, minus
// what would make atl think it runs in a task container.
func controlEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, InEnv+"=") {
			env = append(env, kv)
		}
	}
	return env
}

// ControlClient sends req to the gate's control socket and copies the
// command's output to out. It returns the command's exit status; an error
// means the gate could not be asked or did not answer to the end.
func ControlClient(socket string, req ControlRequest, out io.Writer) (int, error) {
	if socket == "" {
		socket = InControlSocket
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return 0, fmt.Errorf("cannot reach the task's gate: %w", err)
	}
	defer conn.Close()
	line, err := json.Marshal(req)
	if err != nil {
		return 0, err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return 0, fmt.Errorf("cannot reach the task's gate: %w", err)
	}
	dec := json.NewDecoder(conn)
	for {
		var r controlReply
		if err := dec.Decode(&r); err != nil {
			return 0, errors.New("the task's gate closed the connection before the command ended")
		}
		if r.Output != "" {
			io.WriteString(out, r.Output)
		}
		if r.Exit != nil {
			return *r.Exit, nil
		}
	}
}
