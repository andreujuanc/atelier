package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Files of a task's run dir. The dir is mounted read-only into the task
// container at RunMount, so the task can use the socket but not replace it.
func socketPath(runDir string) string { return filepath.Join(runDir, "docker.sock") }
func pidPath(runDir string) string    { return filepath.Join(runDir, "gate.pid") }
func lockPath(runDir string) string   { return filepath.Join(runDir, "gate.lock") }
func identPath(runDir string) string  { return filepath.Join(runDir, "gate.ident") }

// Ident names what a gate serves: the task (database id), the atl binary
// it runs (size and modification time of bin) and the paths it lets the
// task mount read-only (Gate.ROMounts, by a hash). A gate that answers with
// another ident is replaced by Start: it serves a task that was deleted
// meanwhile, runs code older than the atl in the task container, or has
// another list of $ATL_SANDBOX_MOUNTS.
func Ident(taskID int64, bin string, roMounts []string) string {
	s := fmt.Sprintf("task %d", taskID)
	if fi, err := os.Stat(bin); err == nil {
		s += fmt.Sprintf(", atl %d bytes %d", fi.Size(), fi.ModTime().UnixNano())
	}
	if len(roMounts) > 0 {
		s += ", read-only mounts " + shortHash(strings.Join(roMounts, "\x00"), 16)
	}
	return s
}

// SocketPath is the gate's socket in runDir.
func SocketPath(runDir string) string { return socketPath(runDir) }

// UpstreamSocket is the host daemon's socket: $DOCKER_HOST if it names a
// unix socket, else /var/run/docker.sock.
func UpstreamSocket() string {
	if h := os.Getenv("DOCKER_HOST"); strings.HasPrefix(h, "unix://") {
		return strings.TrimPrefix(h, "unix://")
	}
	return "/var/run/docker.sock"
}

// MaxSocketPath is the longest unix socket path Linux accepts (sun_path
// holds 108 bytes including the final NUL).
const MaxSocketPath = 107

// MakeRunDir creates runDir, or makes an existing one (an older atl made
// them 0755), the owner's only: the gate's sockets in it answer whoever
// connects. The task container runs as the owner, and the containers a
// task mounts the Docker socket into get the socket file itself, not
// through the dir.
func MakeRunDir(runDir string) error {
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return err
	}
	return os.Chmod(runDir, 0o700)
}

// Serve runs g on the socket in runDir, and ctl (if not nil) on the control
// socket there, until ctx ends, the task directory (g.Root) disappears or the
// gate's socket is removed or replaced. It returns nil at once if another
// gate already serves runDir.
func Serve(ctx context.Context, g *Gate, ctl *Control, runDir string) error {
	if err := MakeRunDir(runDir); err != nil {
		return err
	}
	sock := socketPath(runDir)
	if len(sock) > MaxSocketPath {
		return fmt.Errorf("gate socket path %s is longer than %d bytes: use a shorter $ATL_WORKSPACE", sock, MaxSocketPath)
	}
	lock, err := os.OpenFile(lockPath(runDir), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil // another gate has it
		}
		return err
	}
	if err := os.WriteFile(pidPath(runDir), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		return err
	}
	defer os.Remove(pidPath(runDir))
	if err := os.WriteFile(identPath(runDir), []byte(g.Ident+"\n"), 0o644); err != nil {
		return err
	}
	defer os.Remove(identPath(runDir))
	// The control socket first: once the Docker socket answers (Alive), both do.
	if ctl != nil {
		ctlSock := ControlSocketPath(runDir)
		os.Remove(ctlSock) // stale, from a gate that died
		cl, err := net.Listen("unix", ctlSock)
		if err != nil {
			return err
		}
		defer os.Remove(ctlSock)
		defer cl.Close()
		if err := os.Chmod(ctlSock, 0o666); err != nil {
			return err
		}
		go ctl.Serve(cl)
	}
	os.Remove(sock) // stale, from a gate that died
	l, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer os.Remove(sock)
	if err := os.Chmod(sock, 0o666); err != nil { // the dir decides who gets to it
		return err
	}
	mine, err := os.Stat(sock)
	if err != nil {
		return err
	}
	switch {
	case g.Log == nil:
	case g.NoDocker:
		g.Log.Printf("gate %s on %s: no Docker access (every Docker request is refused)", g.ID, sock)
	default:
		g.Log.Printf("gate %s on %s, mounts allowed under %s", g.ID, sock, g.Root)
		if len(g.ROMounts) > 0 {
			g.Log.Printf("gate %s: and, read-only only, from $%s: %s", g.ID, MountsVar, strings.Join(g.ROMounts, ", "))
		}
	}

	srv := &http.Server{Handler: g}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
			case <-tick.C:
				_, rootErr := os.Stat(g.Root)
				now, sockErr := os.Stat(sock)
				if !errors.Is(rootErr, os.ErrNotExist) && sockErr == nil && os.SameFile(mine, now) {
					continue
				}
			}
			srv.Close()
			return
		}
	}()
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Alive reports whether a gate answers on the socket in runDir.
func Alive(runDir string) bool {
	conn, err := net.DialTimeout("unix", socketPath(runDir), time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Start makes sure a gate for task taskID serves runDir: if none answers, it
// starts `<atlBin> gate <slug> [--ro-mount <path>]... --task-id <taskID>`
// as a detached process (it outlives this atl, like the tmux server) logging
// to logPath, and waits until it answers. roMounts are the paths of
// $ATL_SANDBOX_MOUNTS (see ROMounts). A gate that answers for another task,
// from another atl binary or with other roMounts (see Ident) is stopped and
// replaced first.
func Start(atlBin, slug string, taskID int64, runDir, logPath string, roMounts []string) error {
	args := []string{"gate", slug}
	for _, m := range roMounts {
		args = append(args, "--ro-mount", m)
	}
	args = append(args, "--task-id", strconv.FormatInt(taskID, 10))
	return start(atlBin, args, Ident(taskID, atlBin, roMounts), slug, runDir, logPath)
}

// AssistantIdent is the Ident of the assistant's gate for the workspace dir
// workspace, running the atl binary bin.
func AssistantIdent(workspace, bin string) string {
	s := "assistant of " + workspace
	if fi, err := os.Stat(bin); err == nil {
		s += fmt.Sprintf(", atl %d bytes %d", fi.Size(), fi.ModTime().UnixNano())
	}
	return s
}

// StartAssistant makes sure the assistant's gate for workspace (its dir)
// serves runDir, as Start does for a task: `<atlBin> gate --assistant`, run
// with this process's environment, which has the workspace applied. A gate
// that answers for another workspace or from another atl binary
// (AssistantIdent) is replaced.
func StartAssistant(atlBin, workspace, runDir, logPath string) error {
	return start(atlBin, []string{"gate", "--assistant"}, AssistantIdent(workspace, atlBin), "the assistant", runDir, logPath)
}

// start starts `<atlBin> <args>`, a gate with ident whose log is logPath,
// unless one with that ident answers on runDir; one with another ident is
// stopped first. what names the gate's charge in errors.
func start(atlBin string, args []string, ident, what, runDir, logPath string) error {
	if Alive(runDir) {
		cur, _ := os.ReadFile(identPath(runDir))
		if strings.TrimSpace(string(cur)) == ident {
			return nil
		}
		// One that missed the signal notices its socket is gone within 10s.
		if err := StopAndWait(runDir, 15*time.Second); err != nil {
			return fmt.Errorf("replace the gate for %s: %w", what, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(atlBin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start gate: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.After(5 * time.Second)
	for {
		if Alive(runDir) {
			return nil
		}
		select {
		case err := <-exited:
			if Alive(runDir) { // lost a race with another atl starting it
				return nil
			}
			return fmt.Errorf("gate for %s exited (%v); see %s", what, err, logPath)
		case <-deadline:
			return fmt.Errorf("gate for %s did not come up; see %s", what, logPath)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// awaitUnlocked waits until no gate holds runDir's lock (a stopped gate
// holds it until it has removed its files).
func awaitUnlocked(runDir string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		f, err := os.OpenFile(lockPath(runDir), os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		f.Close() // releases the lock too
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("the old gate did not stop")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Stop ends the gate serving runDir, if any: it signals the process named in
// the pid file (only if a gate holds runDir's lock, so the file is not one a
// killed gate left, and only if that process is an `atl gate`) and removes
// the socket, which a gate that missed the signal notices by itself.
func Stop(runDir string) {
	if data, err := os.ReadFile(pidPath(runDir)); err == nil && locked(runDir) {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 1 {
			if cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil &&
				strings.Contains(string(cmdline), "\x00gate\x00") {
				_ = syscall.Kill(pid, syscall.SIGTERM)
			}
		}
	}
	os.Remove(socketPath(runDir))
}

// StopAndWait is Stop, then waits up to wait until the gate has stopped
// serving (it holds the run dir's lock until then), so that it takes no
// request any more. Without a gate holding the lock there is nothing to
// wait for (and Stop signalled nothing).
func StopAndWait(runDir string, wait time.Duration) error {
	Stop(runDir)
	if !locked(runDir) {
		return nil
	}
	return awaitUnlocked(runDir, wait)
}

// locked reports whether a gate holds runDir's lock, as Serve does while it
// serves (and a gate that died does not): false only if the lock is
// missing or free.
func locked(runDir string) bool {
	f, err := os.OpenFile(lockPath(runDir), os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	defer f.Close() // releases the lock if it was free
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil
}

// NewLogger returns the logger a gate process writes to (its stderr, which
// Start points at the gate log).
func NewLogger() *log.Logger { return log.New(os.Stderr, "", log.LstdFlags) }
