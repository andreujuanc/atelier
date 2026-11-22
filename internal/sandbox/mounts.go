package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/andreujuanc/atelier/internal/config"
)

// MountsVar names the workspace setting that lists host files and
// directories every task gets read-only (see ROMounts): at the same path in
// its own container, and for bind mounts, read-only only, in the containers
// it creates through its gate.
const MountsVar = "ATL_SANDBOX_MOUNTS"

// guard is a path that a path of MountsVar may not be, contain or lie
// inside, and what it is.
type guard struct{ path, what string }

// guards returns what the sandbox keeps from tasks, which a path of
// MountsVar would hand to every task.
func guards(p config.Paths) []guard {
	var gs []guard
	add := func(path, what string) {
		if path == "" {
			return
		}
		if abs, err := filepath.Abs(path); err == nil {
			gs = append(gs, guard{abs, what})
		}
	}
	if p.Workspace != "" {
		add(p.Workspace, "the workspace dir ($ATL_WORKSPACE)")
	} else {
		add(filepath.Dir(p.DB), "atelier's data dir (its database)")
		add(p.StateDir, "atelier's state dir (hook and gate logs)")
		add(p.ConfigDir, "atelier's config dir (projects)")
	}
	add(p.RunRoot(), "atelier's run dir (gate sockets, task Claude dirs, origin pins, the stored Claude token)")
	add(p.Repos, "the repos dir ($ATL_REPOS)")
	add(p.Tasks, "the tasks dir")
	add(config.ClaudeConfigDir(), "your Claude Code config dir")
	for _, s := range []string{UpstreamSocket(), "/var/run/docker.sock", "/run/docker.sock"} {
		add(s, "the Docker socket")
	}
	add(os.Getenv("SSH_AUTH_SOCK"), "the SSH agent's socket ($SSH_AUTH_SOCK)")
	add(RunMount, "where a task container has its run dir")
	for _, k := range []string{"/proc", "/sys", "/dev"} {
		add(k, "a kernel file system")
	}
	return gs
}

// within reports whether path lies inside dir (not dir itself).
func within(path, dir string) bool {
	return dir != "/" && strings.HasPrefix(path, dir+"/")
}

// checkMount resolves the symlinks of path, which must be absolute and exist,
// and checks the result against gs; it returns the result. The error is one
// line that names path and the rule it broke.
func checkMount(path string, gs []guard) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s is not an absolute path", path)
	}
	real, err := filepath.EvalSymlinks(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%s does not exist here (it must exist here and at the same path on the host)", path)
	}
	if err != nil {
		return "", fmt.Errorf("%s: %v", path, err)
	}
	name := path
	if real != filepath.Clean(path) {
		name += " (" + real + ")"
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("%s: %v", name, err)
	}
	if !fi.Mode().IsRegular() && !fi.IsDir() {
		return "", fmt.Errorf("%s is neither a file nor a directory", name)
	}
	if strings.ContainsAny(real, `,"`) || strings.ContainsFunc(real, unicode.IsControl) {
		return "", fmt.Errorf("%s has a comma, a quote or a control character, which docker's --mount cannot take", name)
	}
	if real == "/" {
		return "", fmt.Errorf("%s is the root directory: every task would get the whole host", name)
	}
	for _, g := range gs {
		forms := []string{filepath.Clean(g.path)}
		if r, err := resolveExisting(g.path); err == nil && r != forms[0] {
			forms = append(forms, r)
		}
		for _, gp := range forms {
			rel := ""
			switch {
			case real == gp:
				rel = "is"
			case within(real, gp):
				rel = "lies inside"
			case within(gp, real):
				rel = "contains"
			default:
				continue
			}
			return "", fmt.Errorf("%s %s %s, %s, which the sandbox keeps from tasks", name, rel, g.what, gp)
		}
	}
	return real, nil
}

// ROMounts reads $ATL_SANDBOX_MOUNTS: a ":"-separated list of absolute paths
// of files or directories (like $PATH: empty entries are ignored; a leading
// "~" is the home directory, as in config.Resolve). Each is resolved once
// (symlinks followed) and the result is what every task container mounts,
// read-only, at the same path, and what its gate lets it bind-mount,
// read-only only. A path must exist here, and so (atl cannot check it) at
// the same path on the host, where the Docker daemon resolves it. It may not
// be /, nor be, contain or lie inside what the sandbox keeps from tasks
// (see guards). Nil when the variable is unset or empty; the error is one
// line that names the variable, the path and the rule it broke.
func ROMounts(p config.Paths) ([]string, error) {
	paths, errs := CheckROMounts(p)
	if len(errs) > 0 {
		return nil, errs[0]
	}
	return paths, nil
}

// CheckROMounts is ROMounts with every path checked: the resolved paths
// that pass (without duplicates) and an error for each that does not.
func CheckROMounts(p config.Paths) (paths []string, errs []error) {
	raw := os.Getenv(MountsVar)
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	home, _ := os.UserHomeDir()
	gs := guards(p)
	seen := map[string]bool{}
	for _, entry := range strings.Split(raw, ":") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if home != "" {
			entry = config.ExpandHome(entry, home)
		}
		real, err := checkMount(entry, gs)
		if err != nil {
			errs = append(errs, fmt.Errorf("$%s: %w", MountsVar, err))
			continue
		}
		if !seen[real] {
			seen[real] = true
			paths = append(paths, real)
		}
	}
	return paths, errs
}

// CheckROMount checks path, one of the paths ROMounts returned (resolved),
// again: the gate does so for each it is given.
func CheckROMount(p config.Paths, path string) error {
	real, err := checkMount(path, guards(p))
	if err != nil {
		return err
	}
	if real != path {
		return fmt.Errorf("%s resolves to %s", path, real)
	}
	return nil
}
