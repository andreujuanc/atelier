package sandbox

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	go (&Control{Slug: "t1", Atl: atl}).Serve(l)
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
		{ControlRequest{Op: OpPush}, "push t1"},
		{ControlRequest{Op: OpPush, Repo: "api"}, "push t1 --repo api"},
		{ControlRequest{Op: OpPush, Force: true}, "push t1 --force"},
		{ControlRequest{Op: OpPush, Repo: "my.repo_2", Force: true}, "push t1 --repo my.repo_2 --force"},
		{ControlRequest{Op: OpFetch}, "fetch t1"},
		{ControlRequest{Op: OpFetch, Repo: "docs"}, "fetch t1 --repo docs"},
	} {
		code, out := lab.ask(t, tc.req)
		if code != 0 {
			t.Errorf("%+v: exit %d, output %q", tc.req, code, out)
		}
		if got, ran := lab.argv(t); !ran || got != tc.argv {
			t.Errorf("%+v: atl ran with %q (ran: %v), want %q", tc.req, got, ran, tc.argv)
		}
		for _, want := range []string{"to stdout: " + tc.req.Op + "\n", "to stderr: t1\n"} {
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
			out += r.Output
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

func TestControlClientNeedsAGate(t *testing.T) {
	if _, err := ControlClient(filepath.Join(t.TempDir(), "none.sock"), ControlRequest{Op: OpPush}, os.Stderr); err == nil {
		t.Error("no error without a gate")
	}
}
