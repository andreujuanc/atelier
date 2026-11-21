// Package sandbox runs a task's Claude sessions in the task's own Docker
// container instead of next to atl, and gives that container a gated Docker
// socket (see Gate) in place of the host's.
//
// It is switched on per workspace with $ATL_SANDBOX=docker. The image comes
// from the task's project (`image = "..."`), else $ATL_SANDBOX_IMAGE, else
// DefaultImage; it needs `claude` and a shell, and should have the same user
// as the one running atl (the atelier image does).
package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultImage is the task image when neither the project nor
// $ATL_SANDBOX_IMAGE names one: the image atl itself ships in.
const DefaultImage = "atelier"

// Where a task container finds what atl gives it.
const (
	RunMount = "/run/atl"                // the task's run dir, read-only
	InSocket = RunMount + "/docker.sock" // the gate
	InBin    = RunMount + "/bin/atl"     // the atl binary that created the container
	// InEnv is set (to the sandbox ID) in every task container. atl running
	// in there opens no database: hooks, the status line, note, notes, push
	// and fetch ask the task's gate (see Control), everything else is refused.
	InEnv = "ATL_SANDBOX_ID"
)

const labelSpec = "atelier.spec"

// Config is the workspace's sandbox setting; nil means tasks run next to atl.
type Config struct {
	Image string // default task image
}

// FromEnv reads $ATL_SANDBOX and $ATL_SANDBOX_IMAGE. It returns nil when the
// sandbox is off (unset, "", "0", "off", "none"), and also inside a task
// container, where there is nothing to launch.
func FromEnv() (*Config, error) {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv("ATL_SANDBOX"))); v {
	case "", "0", "off", "none":
		return nil, nil
	case "docker":
	default:
		return nil, fmt.Errorf("$ATL_SANDBOX=%q: want docker or off", v)
	}
	if Inside() {
		return nil, nil
	}
	image := strings.TrimSpace(os.Getenv("ATL_SANDBOX_IMAGE"))
	if image == "" {
		image = DefaultImage
	}
	return &Config{Image: image}, nil
}

// Inside reports whether this process runs in a task container.
func Inside() bool { return os.Getenv(InEnv) != "" }

func shortHash(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:n]
}

// ID names a task's sandbox on the Docker daemon: the slug plus a short hash
// of the task dir, so equal slugs in two workspaces on one daemon differ.
func ID(slug, taskDir string) string { return slug + "-" + shortHash(taskDir, 6) }

// ContainerName is the name of the task container of sandbox id.
func ContainerName(id string) string { return "atl-" + id }

// Mount is a bind mount of a host path into the task container.
type Mount struct {
	Src, Dst string
	RO       bool
}

// Spec describes a task container.
type Spec struct {
	ID, Image string
	User      string // "uid:gid"
	Workdir   string
	Mounts    []Mount
	Env       map[string]string
}

// Hash identifies the spec; it is stored on the container (label
// atelier.spec) to notice that the container predates a change.
func (s Spec) Hash() string { return shortHash(strings.Join(s.runArgs(""), "\x00"), 16) }

// RunArgs is the docker command line that creates the container. It only
// sleeps: sessions are started in it with docker exec.
func (s Spec) RunArgs() []string { return s.runArgs(s.Hash()) }

func (s Spec) runArgs(hash string) []string {
	args := []string{"run", "-d", "--init", "--name", ContainerName(s.ID),
		"--label", LabelAgent + "=" + s.ID}
	if hash != "" {
		args = append(args, "--label", labelSpec+"="+hash)
	}
	if s.User != "" {
		args = append(args, "--user", s.User)
	}
	if s.Workdir != "" {
		args = append(args, "--workdir", s.Workdir)
	}
	for _, m := range s.Mounts {
		v := "type=bind,src=" + m.Src + ",dst=" + m.Dst
		if m.RO {
			v += ",readonly"
		}
		args = append(args, "--mount", v)
	}
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--env", k+"="+s.Env[k])
	}
	return append(args, s.Image, "sleep", "infinity")
}

// ExecArgv is the command for a tmux window that runs argv in the task
// container of sandbox id, in workdir. The variables named in envKeys are
// passed on from the window's environment. The atl binary of the run dir
// comes first on PATH, so `atl` in the container is the atl that made it.
//
// docker's default detach sequence starts with ctrl-p, which it then holds
// back until the next key; an unlikely sequence keeps ctrl-p usable.
func ExecArgv(id, workdir string, envKeys, argv []string) []string {
	out := []string{"docker", "exec", "-it", "--detach-keys", "ctrl-^,ctrl-^,ctrl-^", "-w", workdir}
	keys := append([]string{"TERM", "COLORTERM", "COLORFGBG", "NO_COLOR"}, envKeys...)
	sort.Strings(keys)
	for i, k := range keys {
		if i == 0 || k != keys[i-1] {
			out = append(out, "-e", k)
		}
	}
	out = append(out, ContainerName(id), "sh", "-c", `PATH=`+filepath.Dir(InBin)+`:$PATH exec "$@"`, "atl-exec")
	return append(out, argv...)
}

// Runner runs the docker CLI with args and returns its trimmed stdout; a
// failure carries docker's stderr.
type Runner func(ctx context.Context, args ...string) (string, error)

// Docker is the Runner that calls the docker binary on PATH.
func Docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("docker %s: %s", args[0], msg)
	}
	return strings.TrimSpace(string(out)), nil
}

func noSuch(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no such")
}

// Ensure makes the task container exist and run. A container made from
// another spec (the task's repos or the atl paths changed since) is replaced
// unless busy reports that sessions are running in it; then it is kept and
// stale is true.
func Ensure(ctx context.Context, run Runner, spec Spec, busy func() bool) (stale bool, err error) {
	name := ContainerName(spec.ID)
	out, err := run(ctx, "inspect", "--type", "container", "--format",
		`{{.State.Running}} {{index .Config.Labels "`+labelSpec+`"}}`, name)
	switch {
	case noSuch(err):
		if _, err = run(ctx, spec.RunArgs()...); err != nil && strings.Contains(err.Error(), "already in use") {
			// Lost a race with another atl creating it: take theirs.
			_, err = run(ctx, "start", name)
		}
		return false, err
	case err != nil:
		return false, err
	}
	state, hash, _ := strings.Cut(out, " ")
	if hash != spec.Hash() {
		if state == "true" && busy != nil && busy() {
			return true, nil
		}
		if _, err := run(ctx, "rm", "-f", name); err != nil && !noSuch(err) {
			return false, err
		}
		_, err = run(ctx, spec.RunArgs()...)
		return false, err
	}
	if state != "true" {
		_, err = run(ctx, "start", name)
	}
	return false, err
}

// Cleanup removes the task container of sandbox id and everything the task
// created through its gate: containers (with their anonymous volumes),
// networks and volumes. It keeps going on errors and returns them joined.
func Cleanup(ctx context.Context, run Runner, id string) error {
	var errs []error
	if _, err := run(ctx, "rm", "-f", ContainerName(id)); err != nil && !noSuch(err) {
		errs = append(errs, err)
	}
	filter := "label=" + LabelTask + "=" + id
	for _, kind := range []struct{ list, remove []string }{
		{[]string{"ps", "-aq", "--filter", filter}, []string{"rm", "-f", "-v"}},
		{[]string{"network", "ls", "-q", "--filter", filter}, []string{"network", "rm"}},
		{[]string{"volume", "ls", "-q", "--filter", filter}, []string{"volume", "rm", "-f"}},
	} {
		out, err := run(ctx, kind.list...)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ids := strings.Fields(out); len(ids) > 0 {
			if _, err := run(ctx, append(kind.remove, ids...)...); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// InstallBin copies the atl binary src to <runDir>/bin/atl unless that copy
// is already the same file (size and modification time). The copy replaces
// the old one atomically, so a running container sees the new binary too.
func InstallBin(src, runDir string) error {
	dst := filepath.Join(runDir, "bin", "atl")
	sfi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if dfi, err := os.Stat(dst); err == nil && dfi.Size() == sfi.Size() && dfi.ModTime().Equal(sfi.ModTime()) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".atl.tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chtimes(tmp.Name(), sfi.ModTime(), sfi.ModTime()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
