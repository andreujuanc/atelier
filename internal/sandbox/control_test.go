package sandbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// controlLab serves a Control for task "t1" on a temp socket. Its atl is a
// shell script that records its argv (one per line) and the
// $ATL_SANDBOX_ID it saw, prints a line to stdout and one to stderr, and
// exits with the status in the file "exit" if there is one.
type controlLab struct {
	dir, sock string
}

func newControlLab(t *testing.T) *controlLab {
	t.Helper()
	dir := t.TempDir()
	atl := filepath.Join(dir, "atl")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + filepath.Join(dir, "argv") + "\n" +
		"printf '%s' \"${" + InEnv + "-unset}\" > " + filepath.Join(dir, "env") + "\n" +
		"echo \"to stdout: $1\"\n" +
		"echo \"to stderr: $2\" >&2\n" +
		"[ -f " + filepath.Join(dir, "exit") + " ] && exit $(cat " + filepath.Join(dir, "exit") + ")\n" +
		"exit 0\n"
	if err := os.WriteFile(atl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "atl.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go (&Control{Slug: "t1", TaskID: 7, Atl: atl}).Serve(l)
	return &controlLab{dir: dir, sock: sock}
}

// argv returns what the stand-in atl was run with, and removes the record.
func (l *controlLab) argv(t *testing.T) (string, bool) {
	t.Helper()
	path := filepath.Join(l.dir, "argv")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(path)
	return strings.Join(strings.Fields(string(data)), " "), true
}

func (l *controlLab) ask(t *testing.T, req ControlRequest) (int, string) {
	t.Helper()
	var out strings.Builder
	code, err := ControlClient(l.sock, req, &out)
	if err != nil {
		t.Fatalf("%+v: %v", req, err)
	}
	return code, out.String()
}

func TestControlRunsPushAndFetchForItsTask(t *testing.T) {
	lab := newControlLab(t)
	t.Setenv(InEnv, "should-not-reach-atl")
	for _, tc := range []struct {
		req  ControlRequest
		argv string
	}{
		// The task is named by id and exact slug.
		{ControlRequest{Op: OpPush}, "push --task-id 7 t1"},
		{ControlRequest{Op: OpPush, Repo: "api"}, "push --task-id 7 t1 --repo api"},
		{ControlRequest{Op: OpPush, Force: true}, "push --task-id 7 t1 --force"},
		{ControlRequest{Op: OpPush, Repo: "my.repo_2", Force: true}, "push --task-id 7 t1 --repo my.repo_2 --force"},
		{ControlRequest{Op: OpFetch}, "fetch --task-id 7 t1"},
		{ControlRequest{Op: OpFetch, Repo: "docs"}, "fetch --task-id 7 t1 --repo docs"},
	} {
		code, out := lab.ask(t, tc.req)
		if code != 0 {
			t.Errorf("%+v: exit %d, output %q", tc.req, code, out)
		}
		if got, ran := lab.argv(t); !ran || got != tc.argv {
			t.Errorf("%+v: atl ran with %q (ran: %v), want %q", tc.req, got, ran, tc.argv)
		}
		for _, want := range []string{"to stdout: " + tc.req.Op + "\n", "to stderr: --task-id\n"} {
			if !strings.Contains(out, want) {
				t.Errorf("%+v: output %q lacks %q", tc.req, out, want)
			}
		}
	}
	if env, _ := os.ReadFile(filepath.Join(lab.dir, "env")); string(env) != "unset" {
		t.Errorf("atl saw %s=%q; it runs where atl runs, not in a task container", InEnv, env)
	}

	// A failure's exit status comes back.
	if err := os.WriteFile(filepath.Join(lab.dir, "exit"), []byte("3"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := lab.ask(t, ControlRequest{Op: OpPush}); code != 3 || !strings.Contains(out, "to stdout: push") {
		t.Errorf("failing push: exit %d, output %q; want 3 and the output", code, out)
	}
}

func TestControlRefusals(t *testing.T) {
	lab := newControlLab(t)
	for _, req := range []ControlRequest{
		{Op: ""},
		{Op: "gc"},
		{Op: "push t2"},
		{Op: "PUSH"},
		{Op: OpPush, Repo: "--upload-pack=x"},
		{Op: OpPush, Repo: "-f"},
		{Op: OpPush, Repo: "a/b"},
		{Op: OpPush, Repo: `a\b`},
		{Op: OpPush, Repo: ".."},
		{Op: OpPush, Repo: "."},
		{Op: OpFetch, Repo: "../other"},
		{Op: OpFetch, Repo: "a\nb"},
		{Op: OpFetch, Repo: " api"},
		{Op: OpFetch, Force: true},
		// Fields of other operations.
		{Op: OpPush, Event: "Stop"},
		{Op: OpFetch, Author: "user"},
		{Op: OpPush, Env: map[string]string{"ATL_SESSION": "1"}},
		{Op: OpHook, Repo: "api"},
		{Op: OpHook, Event: "Stop", Env: map[string]string{"ATL_TASK": "other"}},
		{Op: OpHook, Event: "Stop", Env: map[string]string{"NO_COLOR": "1"}},
		{Op: OpHook, Event: "Stop now"},
		{Op: OpStatusLine, Env: map[string]string{"ATL_SESSION": "1"}},
		{Op: OpNote, Set: true},
		{Op: OpNotes, Author: "user"},
		{Op: OpStatusLine, Landings: []Landing{{Given: "/a", Path: "/a"}}},
	} {
		code, out := lab.ask(t, req)
		if code != exitRefused || !strings.Contains(out, "atelier gate:") {
			t.Errorf("%+v: exit %d, output %q; want a refusal", req, code, out)
		}
		if got, ran := lab.argv(t); ran {
			t.Errorf("%+v: atl ran with %q", req, got)
		}
	}

	// Requests that are not a ControlRequest: another task's slug, unknown
	// fields, not JSON.
	for _, line := range []string{
		`{"op":"push","slug":"t2"}`,
		`{"op":"push","args":["--all"]}`,
		`{"op":"push"} {"op":"fetch"}`,
		`push t1`,
		``,
	} {
		conn, err := net.Dial("unix", lab.sock)
		if err != nil {
			t.Fatal(err)
		}
		conn.Write([]byte(line + "\n"))
		dec := json.NewDecoder(conn)
		var out string
		code := -1
		for {
			var r controlReply
			if dec.Decode(&r) != nil {
				break
			}
			out += r.Output + r.Stderr
			if r.Exit != nil {
				code = *r.Exit
				break
			}
		}
		conn.Close()
		if code != exitRefused || !strings.Contains(out, "unreadable request") {
			t.Errorf("%q: exit %d, output %q; want a refusal", line, code, out)
		}
		if got, ran := lab.argv(t); ran {
			t.Errorf("%q: atl ran with %q", line, got)
		}
	}
}

// fakeTask records the task operations it is asked for.
type fakeTask struct {
	mu   sync.Mutex
	reqs []ControlRequest
	body [][]byte
}

func (f *fakeTask) Do(req ControlRequest, body []byte, stdout, stderr io.Writer) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	f.body = append(f.body, body)
	fmt.Fprintf(stdout, "out %s %d\n", req.Op, len(body))
	fmt.Fprintf(stderr, "err %s\n", req.Event)
	return 5
}

// TestControlTaskOps: task operations reach Task with their body, stdout
// and stderr stay apart, and the exit status names the operation; a gate
// without Task refuses them, and a refusal names none.
func TestControlTaskOps(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "atl.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	task := &fakeTask{}
	go (&Control{Slug: "t1", TaskID: 7, Atl: "/bin/false", Task: task}).Serve(l)

	big := bytes.Repeat([]byte("0123456789abcdef"), 1<<20) // 16 MiB, far past the request line
	for _, c := range []struct {
		req  ControlRequest
		body []byte
	}{
		{ControlRequest{Op: OpHook, Event: "PreToolUse", Env: map[string]string{"ATL_SESSION": "3"},
			Landings: []Landing{{Given: "/t/x", Path: "/r/x"}}}, big},
		{ControlRequest{Op: OpHook, Event: "Stop"}, nil},
		{ControlRequest{Op: OpStatusLine, Env: map[string]string{"NO_COLOR": "1"}}, []byte(`{}`)},
		{ControlRequest{Op: OpNote, Author: "assistant"}, []byte("hello")},
		{ControlRequest{Op: OpNotes, Set: true, Clear: true}, nil},
	} {
		var out, errb strings.Builder
		code, ran, err := Ask(sock, c.req, c.body, &out, &errb, 30*time.Second)
		if err != nil || !ran || code != 5 {
			t.Errorf("%s: exit %d, ran %v, %v", c.req.Op, code, ran, err)
		}
		if want := fmt.Sprintf("out %s %d\n", c.req.Op, len(c.body)); out.String() != want {
			t.Errorf("%s: stdout %q, want %q", c.req.Op, out.String(), want)
		}
		if want := "err " + c.req.Event + "\n"; errb.String() != want {
			t.Errorf("%s: stderr %q, want %q", c.req.Op, errb.String(), want)
		}
		got := task.reqs[len(task.reqs)-1]
		if got.Op != c.req.Op || got.Event != c.req.Event || got.Author != c.req.Author || got.Body != int64(len(c.body)) {
			t.Errorf("%s: Task got %+v", c.req.Op, got)
		}
		if !bytes.Equal(task.body[len(task.body)-1], c.body) && len(c.body) > 0 {
			t.Errorf("%s: Task got a body of %d bytes, want %d", c.req.Op, len(task.body[len(task.body)-1]), len(c.body))
		}
	}

	// A body cut short, or over the limit, is refused before Task sees it.
	n := len(task.reqs)
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte(`{"op":"hook","event":"Stop","body":100}` + "\nshort"))
	conn.(*net.UnixConn).CloseWrite()
	reply, _ := io.ReadAll(conn)
	conn.Close()
	if !strings.Contains(string(reply), "cut short") || len(task.reqs) != n {
		t.Errorf("short body: %s (Task called %d times)", reply, len(task.reqs)-n)
	}
	var errb strings.Builder
	code, ran, err := Ask(sock, ControlRequest{Op: OpNote}, make([]byte, 1<<20+1), io.Discard, &errb, 30*time.Second)
	if err != nil || ran || code != exitRefused || !strings.Contains(errb.String(), "too large") || len(task.reqs) != n {
		t.Errorf("oversized note: exit %d, ran %v, %v, %q", code, ran, err, errb.String())
	}

	// Without Task, task operations are refused; push still runs.
	sock2 := filepath.Join(dir, "atl2.sock")
	l2, err := net.Listen("unix", sock2)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	go (&Control{Slug: "t1", TaskID: 7, Atl: "/bin/false"}).Serve(l2)
	if code, ran, err := Ask(sock2, ControlRequest{Op: OpHook, Event: "Stop"}, nil, io.Discard, io.Discard, 0); err != nil || ran || code != exitRefused {
		t.Errorf("hook without Task: exit %d, ran %v, %v", code, ran, err)
	}
	if code, ran, err := Ask(sock2, ControlRequest{Op: OpFetch}, nil, io.Discard, io.Discard, 0); err != nil || !ran || code != 1 {
		t.Errorf("fetch: exit %d, ran %v, %v; want /bin/false's 1", code, ran, err)
	}
}

func TestControlClientNeedsAGate(t *testing.T) {
	if _, err := ControlClient(filepath.Join(t.TempDir(), "none.sock"), ControlRequest{Op: OpPush}, os.Stderr); err == nil {
		t.Error("no error without a gate")
	}
}
