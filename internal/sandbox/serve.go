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

// maxSocketPath is the longest unix socket path Linux accepts (sun_path
// holds 108 bytes including the final NUL).
const maxSocketPath = 107

// Serve runs g on the socket in runDir until ctx ends, the task directory
// (g.Root) disappears or the socket is removed or replaced. It returns nil
// at once if another gate already serves runDir.
func Serve(ctx context.Context, g *Gate, runDir string) error {
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return err
	}
	sock := socketPath(runDir)
	if len(sock) > maxSocketPath {
		return fmt.Errorf("gate socket path %s is longer than %d bytes: use a shorter $ATL_WORKSPACE", sock, maxSocketPath)
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
	os.Remove(sock) // stale, from a gate that died
	l, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer os.Remove(sock)
	if err := os.Chmod(sock, 0o666); err != nil { // the dir decides who gets to it
		return err
	}
	if err := os.WriteFile(pidPath(runDir), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		return err
	}
	defer os.Remove(pidPath(runDir))
	mine, err := os.Stat(sock)
	if err != nil {
		return err
	}
	if g.Log != nil {
		g.Log.Printf("gate %s on %s, mounts allowed under %s", g.ID, sock, g.Root)
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

// Start makes sure a gate serves runDir: if none answers, it starts
// `<atlBin> gate <slug>` as a detached process (it outlives this atl, like
// the tmux server) logging to logPath, and waits until it answers.
func Start(atlBin, slug, runDir, logPath string) error {
	if Alive(runDir) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(atlBin, "gate", slug)
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
			return fmt.Errorf("gate for %s exited (%v); see %s", slug, err, logPath)
		case <-deadline:
			return fmt.Errorf("gate for %s did not come up; see %s", slug, logPath)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Stop ends the gate serving runDir, if any: it signals the process named in
// the pid file (only if that process is an `atl gate`) and removes the
// socket, which a gate that missed the signal notices by itself.
func Stop(runDir string) {
	if data, err := os.ReadFile(pidPath(runDir)); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 1 {
			if cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil &&
				strings.Contains(string(cmdline), "\x00gate\x00") {
				_ = syscall.Kill(pid, syscall.SIGTERM)
			}
		}
	}
	os.Remove(socketPath(runDir))
}

// NewLogger returns the logger a gate process writes to (its stderr, which
// Start points at the gate log).
func NewLogger() *log.Logger { return log.New(os.Stderr, "", log.LstdFlags) }
