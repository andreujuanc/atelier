package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/sandbox"
	"github.com/andreujuanc/atelier/internal/tmux"
)

// anyBin is the test Apps' CheckBin: their stand-in atl binaries are
// scripts, which no task container could run.
func anyBin(string) error { return nil }

// TestSandboxRefusesDynamicAtl: a sandboxed session does not start (Docker
// is asked for its version only) with an atl binary a task container
// cannot run; the error is one line that says how to build one.
func TestSandboxRefusesDynamicAtl(t *testing.T) {
	e, fd := sandboxEnv(t)
	e.app.CheckBin = nil // the real check: AtlBin is a shell script
	e.task(t, "boxed", model.StatusActive, true)
	_, err := e.app.NewSession(context.Background(), "boxed")
	if err == nil {
		t.Fatal("a session started with a script for atl")
	}
	msg := err.Error()
	if !strings.Contains(msg, e.app.AtlBin+" is not a Linux (ELF) binary") || !strings.Contains(msg, "must be a static Linux binary") ||
		!strings.Contains(msg, sandbox.BuildStatic) || strings.Contains(msg, "\n") {
		t.Errorf("error %q", msg)
	}
	for _, c := range fd.calls {
		if c[0] != "version" {
			t.Errorf("docker %q was run", c)
		}
	}
	if _, err := os.Stat(e.app.Paths.RunDir("boxed")); !os.IsNotExist(err) {
		t.Errorf("run dir made (%v)", err)
	}

	// The checker's verdict is what counts.
	e.app.CheckBin = func(path string) error { return errors.New(path + " is dynamically linked") }
	if _, err := e.app.NewSession(context.Background(), "boxed"); err == nil || !strings.Contains(err.Error(), "is dynamically linked") {
		t.Errorf("injected checker: %v", err)
	}
}

func TestDoctorAtlBinary(t *testing.T) {
	e := newEnv(t)
	check := func() Check {
		for _, c := range e.app.checkSandboxFor(t) {
			if c.Name == "atl binary" {
				return c
			}
		}
		t.Fatal("no atl binary check")
		return Check{}
	}
	if c := check(); !c.OK || !strings.Contains(c.Detail, e.app.AtlBin+": a static Linux binary") {
		t.Errorf("anyBin: %+v", c)
	}
	e.app.CheckBin = nil // /usr/bin/atl, which is not there
	if c := check(); c.OK || !strings.Contains(c.Detail, "does not exist") || !strings.Contains(c.Detail, sandbox.BuildStatic) {
		t.Errorf("missing binary: %+v", c)
	}
}

// checkSandboxFor runs doctor's sandbox checks with the sandbox on and no
// Docker daemon (only the checks before the daemon's matter here).
func (a *App) checkSandboxFor(t *testing.T) []Check {
	t.Helper()
	t.Setenv("ATL_SANDBOX", "")
	t.Setenv(sandbox.InEnv, "")
	a.Docker = (&fakeDocker{down: true}).run
	return a.checkSandbox(context.Background())
}

// TestDoctorTmuxServer: which config atl's tmux server runs with.
func TestDoctorTmuxServer(t *testing.T) {
	e := newEnv(t) // its server runs, started with -f /dev/null
	ctx := context.Background()
	one := func() Check {
		checks := e.app.checkTmuxServer(ctx)
		if len(checks) != 1 {
			t.Fatalf("checks %+v", checks)
		}
		return checks[0]
	}
	c := e.app.Tmux
	if err := c.UseConfig(e.app.Paths.TmuxConf(), e.app.Paths.UserTmuxConf()); err != nil {
		t.Fatal(err)
	}
	if ch := one(); !ch.Warn || !strings.Contains(ch.Detail, "-L "+c.Socket+" runs without "+e.app.Paths.TmuxConf()) {
		t.Errorf("running without the file: %+v", ch)
	}

	e.tmux(t, "kill-server")
	if ch := one(); ch.Warn || !ch.OK || !strings.Contains(ch.Detail, "not running: atl starts it with "+e.app.Paths.TmuxConf()) {
		t.Errorf("not running: %+v", ch)
	}
	if _, err := c.NewWindow(ctx, tmux.WindowSpec{Session: "s", Argv: []string{"sleep", "60"}}); err != nil {
		t.Fatal(err)
	}
	if ch := one(); ch.Warn || !ch.OK || !strings.Contains(ch.Detail, "running with "+e.app.Paths.TmuxConf()) {
		t.Errorf("running with the file: %+v", ch)
	}

	e.app.Tmux = &tmux.Client{Socket: tmux.DefaultSocket}
	if ch := one(); ch.Warn || !ch.OK || !strings.Contains(ch.Detail, "default server") {
		t.Errorf("default server: %+v", ch)
	}
}

// TestNewTmux: the App's tmux client starts its server with the generated
// config; with the default server it writes nothing.
func TestNewTmux(t *testing.T) {
	root := t.TempDir()
	paths := pathsUnder(root)
	paths.Workspace = root // its tmux.conf is sourced last
	t.Setenv("ATL_TMUX_SOCKET", "atl-x")
	var out bytes.Buffer
	if c := NewTmux(paths, &out); c.Config() != paths.TmuxConf() || out.Len() != 0 {
		t.Errorf("config %q, out %q", c.Config(), out.String())
	}
	if b, err := os.ReadFile(paths.TmuxConf()); err != nil || string(b) != tmux.ServerConfigWith(filepath.Join(root, "tmux.conf")) {
		t.Errorf("%s: %q, %v", paths.TmuxConf(), b, err)
	}

	t.Setenv("ATL_TMUX_SOCKET", "default")
	other := pathsUnder(filepath.Join(root, "other"))
	if c := NewTmux(other, &out); c.Config() != "" {
		t.Errorf("default server: config %q", c.Config())
	}
	if _, err := os.Stat(other.TmuxConf()); !os.IsNotExist(err) {
		t.Errorf("default server: %s written (%v)", other.TmuxConf(), err)
	}

	// Unwritable: a warning, and no config.
	t.Setenv("ATL_TMUX_SOCKET", "atl-x")
	blocked := pathsUnder(root)
	blocked.StateDir = filepath.Join(paths.TmuxConf(), "state")
	out.Reset()
	if c := NewTmux(blocked, &out); c.Config() != "" || !strings.HasPrefix(out.String(), "warning: tmux: write its config") {
		t.Errorf("unwritable: config %q, out %q", c.Config(), out.String())
	}
}

func pathsUnder(root string) config.Paths {
	return config.Paths{StateDir: filepath.Join(root, "state")}
}
