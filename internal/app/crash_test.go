package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/store"
	"github.com/andreujuanc/atelier/internal/tmux"
)

// killPoint says where crashRun's child atl dies: when it runs Cmd (git or
// tmux) with arguments ("$*") matching the shell case pattern Args, the
// command's wrapper SIGKILLs it, before it runs the real command or, if
// After, after.
type killPoint struct {
	Cmd, Args string
	After     bool
}

// newWindowCall matches the tmux call that makes a session's window.
const newWindowCall = `*" new-window "*|*" new-session "*`

// crashRun runs op (see TestCrashHelper) in a child process on a's paths,
// tmux socket and atl binary, and SIGKILLs that process at kill. It fails
// unless the child died of SIGKILL: the kill point was reached.
func crashRun(t *testing.T, a *App, kill killPoint, op ...string) {
	t.Helper()
	real, err := exec.LookPath(kill.Cmd)
	if err != nil {
		t.Skipf("%s not installed", kill.Cmd)
	}
	bin := t.TempDir()
	after := ""
	if kill.After {
		after = fmt.Sprintf("%q \"$@\"; ", real)
	}
	wrapper := fmt.Sprintf(`#!/bin/sh
case "$*" in
%s) %skill -9 $PPID; exit 1;;
esac
exec %q "$@"
`, kill.Args, after, real)
	if err := os.WriteFile(filepath.Join(bin, kill.Cmd), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"CRASHTEST_ROOT="+filepath.Dir(a.Paths.Repos),
		"CRASHTEST_SOCK="+a.Tmux.Socket,
		"CRASHTEST_ATLBIN="+a.AtlBin,
		"CRASHTEST_OP="+strings.Join(op, "\n"))
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("child %v was not killed at %+v: %v\n%s", op, kill, err, out)
	}
}

// TestCrashHelper is crashRun's child, not a test: it runs CRASHTEST_OP
// (newline-separated: an operation and its arguments) on the App crashRun
// describes, and should be killed on the way.
func TestCrashHelper(t *testing.T) {
	opv := os.Getenv("CRASHTEST_OP")
	if opv == "" {
		t.Skip("crashRun's child only")
	}
	root := os.Getenv("CRASHTEST_ROOT")
	paths := config.Paths{
		Repos:     filepath.Join(root, "repos"),
		Tasks:     filepath.Join(root, "tasks"),
		DB:        filepath.Join(root, "data", "atelier.db"),
		StateDir:  filepath.Join(root, "state"),
		ConfigDir: filepath.Join(root, "config"),
	}
	st, err := store.Open(paths.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := &App{Paths: paths, Store: st, Tmux: &tmux.Client{Socket: os.Getenv("CRASHTEST_SOCK")}, Out: os.Stderr,
		Now: time.Now, AtlBin: os.Getenv("CRASHTEST_ATLBIN"), CheckBin: anyBin}
	ctx := context.Background()
	op := strings.Split(opv, "\n")
	switch op[0] {
	case "new-session":
		_, err = a.NewSession(ctx, op[1])
	case "resume":
		_, err = a.Resume(ctx, op[1])
	default:
		t.Fatalf("unknown op %q", op[0])
	}
	t.Fatalf("%v returned (%v) instead of being killed", op, err)
}

// TestRecoverAfterKilledResume: atl killed while it resumes a session (here
// at the tmux call that makes its window, before and after it) leaves the
// session claimed (idle, no window recorded). Recover must make it stopped
// again once no launch is under way, so that it can be resumed: otherwise it
// stays idle, which neither Resume nor the open picker ever takes.
func TestRecoverAfterKilledResume(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprintf("after=%v", after), func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			e.task(t, "t", model.StatusActive, true)
			s, err := e.app.NewSession(ctx, "t")
			if err != nil {
				t.Fatal(err)
			}
			if err := e.app.Store.SetClaudeSession(ctx, s.ID, "cid-1", time.Now()); err != nil {
				t.Fatal(err)
			}
			e.argv(t, s.ID)
			e.tmux(t, "kill-window", "-t", s.TmuxTarget)
			if n, err := e.app.Recover(ctx); err != nil || n != 1 {
				t.Fatalf("Recover = %d, %v", n, err)
			}

			crashRun(t, e.app, killPoint{Cmd: "tmux", Args: newWindowCall, After: after}, "resume", "t")
			if got := e.session(t, s.ID); got.State != model.StateIdle || got.TmuxTarget != "" {
				t.Fatalf("after the kill: %+v", got)
			}
			if after {
				// The window the killed atl made runs on: the session
				// is live, and stays so while it does.
				if _, err := e.app.Recover(ctx); err != nil {
					t.Fatal(err)
				}
				if got := e.session(t, s.ID); got.State != model.StateIdle {
					t.Fatalf("live window's session: %s", got.State)
				}
				e.tmux(t, "kill-window", "-t", "claude-"+strconv.FormatInt(s.ID, 10))
			}
			// A launch under way (the launch lock held) is left alone.
			unlock, err := e.app.launchLock(ctx, s.TaskID, syscall.LOCK_SH)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.app.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if got := e.session(t, s.ID); got.State != model.StateIdle {
				t.Fatalf("Recover during a launch: %s", got.State)
			}
			unlock()

			if n, err := e.app.Recover(ctx); err != nil || n != 1 {
				t.Fatalf("Recover = %d, %v", n, err)
			}
			if got := e.session(t, s.ID); got.State != model.StateStopped || got.ClaudeSessionID != "cid-1" {
				t.Fatalf("after Recover: %+v", got)
			}
			if n, err := e.app.Resume(ctx, "t"); err != nil || n != 1 {
				t.Fatalf("Resume = %d, %v", n, err)
			}
			if argv := e.argv(t, s.ID); !strings.Contains(strings.Join(argv, " "), "--resume=cid-1") {
				t.Fatalf("argv %q", argv)
			}
		})
	}
}

// TestRecoverAfterKilledNewSession: atl killed while it starts a new session
// leaves a live row with no window recorded. Recover makes it stopped once no
// launch is under way and its window (if the killed atl made one) is gone;
// a row for a Claude atl did not launch (the hooks made it) is left alone.
func TestRecoverAfterKilledNewSession(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprintf("after=%v", after), func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			task := e.task(t, "t", model.StatusActive, true)
			foreign := &model.Session{TaskID: task.ID, ClaudeSessionID: "by-hand", State: model.StateIdle}
			if err := e.app.Store.CreateSession(ctx, foreign); err != nil {
				t.Fatal(err)
			}
			crashRun(t, e.app, killPoint{Cmd: "tmux", Args: newWindowCall, After: after}, "new-session", "t")
			sessions, err := e.app.Store.ListSessions(ctx, task.ID)
			if err != nil || len(sessions) != 2 {
				t.Fatalf("sessions %+v, %v", sessions, err)
			}
			s := sessions[1]
			if s.State != model.StateIdle || s.TmuxTarget != "" {
				t.Fatalf("after the kill: %+v", s)
			}
			if after {
				e.argv(t, s.ID)
				if _, err := e.app.Recover(ctx); err != nil {
					t.Fatal(err)
				}
				if got := e.session(t, s.ID); got.State != model.StateIdle {
					t.Fatalf("live window's session: %s", got.State)
				}
				e.tmux(t, "kill-window", "-t", "claude-"+strconv.FormatInt(s.ID, 10))
			}
			if n, err := e.app.Recover(ctx); err != nil || n != 1 {
				t.Fatalf("Recover = %d, %v", n, err)
			}
			if got := e.session(t, s.ID); got.State != model.StateStopped {
				t.Fatalf("after Recover: %+v", got)
			}
			if got := e.session(t, foreign.ID); got.State != model.StateIdle {
				t.Fatalf("hook row: %+v", got)
			}
		})
	}
}
