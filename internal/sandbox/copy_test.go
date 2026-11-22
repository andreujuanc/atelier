package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCopyTool(t *testing.T) {
	for name, want := range map[string]bool{
		"wl-copy": true, "wl-paste": true,
		"atl": false, "xclip": false, "wl-copy.tmp": false, "": false,
	} {
		if got := CopyTool(name); got != want {
			t.Errorf("CopyTool(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestParseWlCopy(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want wlCopyArgs
	}{
		// What Claude Code passes.
		{nil, wlCopyArgs{}},
		{[]string{"--primary"}, wlCopyArgs{primary: true}},
		// What a script may pass.
		{[]string{"-p"}, wlCopyArgs{primary: true}},
		{[]string{"-n"}, wlCopyArgs{trim: true}},
		{[]string{"--trim-newline", "hello", "world"}, wlCopyArgs{trim: true, text: []string{"hello", "world"}}},
		{[]string{"-pn"}, wlCopyArgs{primary: true, trim: true}},
		{[]string{"--", "-n", "--primary"}, wlCopyArgs{text: []string{"-n", "--primary"}}},
		{[]string{"-"}, wlCopyArgs{text: []string{"-"}}},
		{[]string{"--help"}, wlCopyArgs{help: true}},
	} {
		got, err := parseWlCopy(tc.args)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseWlCopy(%q) = %+v, %v; want %+v", tc.args, got, err, tc.want)
		}
	}
	for _, args := range [][]string{
		{"--type", "image/png"}, {"-t", "text/plain"}, {"--clear"}, {"-c"}, {"--paste-once"},
		{"--foreground"}, {"--seat", "x"}, {"-px"}, {"--primary=yes"},
	} {
		if got, err := parseWlCopy(args); err == nil {
			t.Errorf("parseWlCopy(%q) = %+v, want an error", args, got)
		}
	}
}

// copyGate serves a Control with a Copy that records what it gets.
type copyGate struct {
	sock string
	mu   sync.Mutex
	got  [][]byte
	err  error // what Copy returns
	log  syncBuffer
}

func (g *copyGate) fail(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.err = err
}

// syncBuffer is a bytes.Buffer for a writer in another goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newCopyGate(t *testing.T, serveCopy bool) *copyGate {
	t.Helper()
	g := &copyGate{sock: filepath.Join(t.TempDir(), "atl.sock")}
	l, err := net.Listen("unix", g.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	ctl := &Control{Slug: "t1", TaskID: 7, Atl: "/bin/false", Log: log.New(&g.log, "", 0)}
	if serveCopy {
		ctl.Copy = func(ctx context.Context, text []byte) error {
			g.mu.Lock()
			defer g.mu.Unlock()
			if _, ok := ctx.Deadline(); !ok {
				t.Error("Copy without a deadline")
			}
			g.got = append(g.got, bytes.Clone(text))
			return g.err
		}
	}
	go ctl.Serve(l)
	return g
}

// copies returns what Copy got since the last call.
func (g *copyGate) copies() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, b := range g.got {
		out = append(out, string(b))
	}
	g.got = nil
	return out
}

// noRead is a standard input that must not be read.
type noRead struct{ t *testing.T }

func (r noRead) Read([]byte) (int, error) {
	r.t.Error("standard input was read")
	return 0, io.EOF
}

func TestCopyMain(t *testing.T) {
	g := newCopyGate(t, true)
	env := func(k string) string {
		return map[string]string{InEnv: "t1-abc123", "ATL_CONTROL_SOCK": g.sock}[k]
	}
	big := strings.Repeat("x", MaxCopy)
	for _, tc := range []struct {
		name  string
		args  []string
		stdin io.Reader
		code  int
		want  []string // what reaches the gate's Copy
	}{
		// Claude Code: the clipboard, then the primary selection, which is
		// not copied again.
		{"wl-copy", nil, strings.NewReader("echo 'a long\n  script'\n"), 0, []string{"echo 'a long\n  script'\n"}},
		{"wl-copy", []string{"--primary"}, strings.NewReader("echo 'a long\n  script'\n"), 0, nil},
		{"wl-copy", []string{"-n"}, strings.NewReader("line\n"), 0, []string{"line"}},
		{"wl-copy", []string{"two", "words"}, noRead{t}, 0, []string{"two words"}},
		{"wl-copy", nil, strings.NewReader(big), 0, []string{big}},
		{"wl-copy", nil, strings.NewReader(""), 0, nil},
		{"wl-copy", []string{"--help"}, noRead{t}, 0, nil},
		// Over the cap: nothing reaches the gate.
		{"wl-copy", nil, strings.NewReader(big + "y"), 1, nil},
		{"wl-copy", []string{big + "y"}, noRead{t}, 1, nil},
		{"wl-copy", []string{"--type", "text/plain"}, noRead{t}, 2, nil},
		// Reading is refused, whatever it asks for: it reads nothing (and
		// CopyMain has no standard output to write to).
		{"wl-paste", nil, noRead{t}, 1, nil},
		{"wl-paste", []string{"--primary", "--no-newline"}, noRead{t}, 1, nil},
		{"wl-paste", []string{"--list-types"}, noRead{t}, 1, nil},
	} {
		var errb strings.Builder
		code := CopyMain(tc.name, tc.args, tc.stdin, &errb, env)
		got := g.copies()
		if code != tc.code || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s %.40q: exit %d, copied %.60q (stderr %q); want exit %d, %.60q", tc.name, tc.args, code, got, errb.String(), tc.code, tc.want)
		}
		if tc.code != 0 && errb.Len() == 0 {
			t.Errorf("%s %.40q: exit %d without a word on stderr", tc.name, tc.args, code)
		}
	}

	// The gate's failure comes back, with its exit status.
	g.fail(errors.New("tmux: copy: session atl-t1: no terminal is attached to the session"))
	var errb strings.Builder
	if code := CopyMain("wl-copy", nil, strings.NewReader("x"), &errb, env); code != 1 || !strings.Contains(errb.String(), "no terminal is attached") {
		t.Errorf("failing copy: exit %d, stderr %q", code, errb.String())
	}
	g.copies()

	// Outside a task container it does nothing.
	errb.Reset()
	outside := func(k string) string {
		if k == InEnv {
			return ""
		}
		return env(k)
	}
	if code := CopyMain("wl-copy", nil, noRead{t}, &errb, outside); code != 1 || len(g.copies()) != 0 {
		t.Errorf("outside a task container: exit %d, stderr %q", code, errb.String())
	}

	// Without a gate, or with one that does not serve copy.
	errb.Reset()
	none := func(k string) string {
		if k == "ATL_CONTROL_SOCK" {
			return filepath.Join(t.TempDir(), "none.sock")
		}
		return env(k)
	}
	if code := CopyMain("wl-copy", nil, strings.NewReader("x"), &errb, none); code != 1 || !strings.Contains(errb.String(), "gate") {
		t.Errorf("no gate: exit %d, stderr %q", code, errb.String())
	}
	g2 := newCopyGate(t, false)
	errb.Reset()
	other := func(k string) string {
		if k == "ATL_CONTROL_SOCK" {
			return g2.sock
		}
		return env(k)
	}
	if code := CopyMain("wl-copy", nil, strings.NewReader("x"), &errb, other); code != exitRefused || !strings.Contains(errb.String(), "not served") {
		t.Errorf("gate without Copy: exit %d, stderr %q", code, errb.String())
	}
}

// TestControlCopy: a copy reaches Copy with its body and nothing else; the
// log has its size, never its text; a request with anything but a body, or
// a body over MaxCopy, is refused before Copy sees it.
func TestControlCopy(t *testing.T) {
	g := newCopyGate(t, true)
	secret := "export TOKEN=s3cr3t-in-the-selection"
	var out, errb strings.Builder
	code, ran, err := Ask(g.sock, ControlRequest{Op: OpCopy}, []byte(secret), &out, &errb, 30*time.Second)
	if err != nil || !ran || code != 0 || out.Len() != 0 {
		t.Fatalf("copy: exit %d, ran %v, %v, stdout %q, stderr %q", code, ran, err, out.String(), errb.String())
	}
	if got := g.copies(); !reflect.DeepEqual(got, []string{secret}) {
		t.Errorf("Copy got %q", got)
	}
	if !strings.Contains(g.log.String(), fmt.Sprintf("copy  %d bytes for t1", len(secret))) || strings.Contains(g.log.String(), "s3cr3t") {
		t.Errorf("gate log %q: want the size, never the text", g.log.String())
	}

	// Copy's error: exit 1, the operation ran, the log names the error and
	// the size only.
	g.fail(errors.New("tmux: copy: no terminal is attached to the session"))
	errb.Reset()
	if code, ran, err := Ask(g.sock, ControlRequest{Op: OpCopy}, []byte(secret), io.Discard, &errb, 30*time.Second); err != nil || !ran || code != 1 ||
		!strings.Contains(errb.String(), "no terminal") {
		t.Errorf("failing copy: exit %d, ran %v, %v, stderr %q", code, ran, err, errb.String())
	}
	g.fail(nil)
	g.copies()
	if strings.Contains(g.log.String(), "s3cr3t") || !strings.Contains(g.log.String(), fmt.Sprintf("fail  copy of %d bytes", len(secret))) {
		t.Errorf("gate log %q", g.log.String())
	}

	// Nothing to copy: done, without Copy.
	if code, ran, err := Ask(g.sock, ControlRequest{Op: OpCopy}, nil, io.Discard, io.Discard, 30*time.Second); err != nil || !ran || code != 0 || len(g.copies()) != 0 {
		t.Errorf("empty copy: exit %d, ran %v, %v", code, ran, err)
	}

	for _, req := range []ControlRequest{
		{Op: OpCopy, Repo: "api"},
		{Op: OpCopy, Force: true},
		{Op: OpCopy, Event: "Stop"},
		{Op: OpCopy, Env: map[string]string{"ATL_SESSION": "1"}},
		{Op: OpCopy, Env: map[string]string{"DISPLAY": ":0"}},
		{Op: OpCopy, Author: "user"},
		{Op: OpCopy, Set: true},
		{Op: OpCopy, Landings: []Landing{{Given: "/a", Path: "/a"}}},
	} {
		errb.Reset()
		code, ran, err := Ask(g.sock, req, []byte("x"), io.Discard, &errb, 30*time.Second)
		if err != nil || ran || code != exitRefused || strings.Contains(errb.String(), "not served") {
			t.Errorf("%+v: exit %d, ran %v, %v, %q; want a refusal by check", req, code, ran, err, errb.String())
		}
	}
	errb.Reset()
	code, ran, err = Ask(g.sock, ControlRequest{Op: OpCopy}, make([]byte, MaxCopy+1), io.Discard, &errb, 30*time.Second)
	if err != nil || ran || code != exitRefused || !strings.Contains(errb.String(), "too large") {
		t.Errorf("oversized copy: exit %d, ran %v, %v, %q", code, ran, err, errb.String())
	}
	if got := g.copies(); len(got) != 0 {
		t.Errorf("refused requests reached Copy: %d", len(got))
	}
}
