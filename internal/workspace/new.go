package workspace

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/sandbox"
)

// FillFromEnv sets the settings c does not have from the environment
// ($ATL_REPOS, $ATL_SANDBOX_MOUNTS, $ATL_SANDBOX_IMAGE,
// $ATL_SANDBOX_CONTEXT, $ATL_CONTEXT), so that `atl workspace new` run in
// an old box writes that box's settings. Values atl set itself for a
// workspace (see AtlSet) are not taken: they are that workspace's.
func FillFromEnv(c *Config) {
	if c.Repos == "" {
		c.Repos = UserEnv("ATL_REPOS")
	}
	if c.Context == "" {
		c.Context = UserEnv("ATL_CONTEXT")
	}
	if c.Sandbox == nil {
		c.Sandbox = &SandboxConfig{}
	}
	sb := c.Sandbox
	if len(sb.Mounts) == 0 {
		for _, m := range strings.Split(UserEnv(sandbox.MountsVar), ":") {
			if m = strings.TrimSpace(m); m != "" {
				sb.Mounts = append(sb.Mounts, m)
			}
		}
	}
	if sb.Image == "" {
		sb.Image = UserEnv("ATL_SANDBOX_IMAGE")
	}
	if sb.Context == "" {
		sb.Context = UserEnv("ATL_SANDBOX_CONTEXT")
	}
}

// Create makes workspace name under root with c as its atelier.toml, and
// tasks/, data/, config/, state/ and the repos directory when it is inside
// the workspace. A directory that exists without atelier.toml is adopted
// (its contents are kept); one with atelier.toml is refused. Relative and
// "~" paths in c.Repos and c.Sandbox.Mounts are made absolute (relative to
// the current directory); a repos directory inside the workspace is written
// relative to it, and left out when it is the default.
//
// Adopting a directory that has a database while $ATL_TMUX_SOCKET is not
// the user's, it writes tmux_socket = "default": the atl that used it ran
// its sessions on tmux's default server, and on atl-<name> it would not
// see them, so it would take them for stopped and end them.
//
// It returns the workspace directory and notes on what it decided.
func Create(root, name string, c Config) (string, []string, error) {
	if err := CheckName(name); err != nil {
		return "", nil, err
	}
	dir := filepath.Join(root, name)
	if err := CheckSocketBudget(dir); err != nil {
		return "", nil, err
	}
	if _, err := os.Lstat(filepath.Join(dir, File)); err == nil {
		return "", nil, fmt.Errorf("workspace %s already exists (it has %s)", dir, File)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", nil, err
	}
	home, _ := os.UserHomeDir()
	repos := filepath.Join(dir, "repos")
	if c.Repos != "" {
		p, err := filepath.Abs(config.ExpandHome(c.Repos, home))
		if err != nil {
			return "", nil, fmt.Errorf("repos %q: %w", c.Repos, err)
		}
		repos = p
		switch rel, err := filepath.Rel(dir, p); {
		case err == nil && rel == "repos":
			c.Repos = ""
		case err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)):
			c.Repos = rel
		default:
			c.Repos = p
		}
	}
	if c.Sandbox != nil {
		for i, m := range c.Sandbox.Mounts {
			if strings.Contains(m, ":") {
				return "", nil, fmt.Errorf("mount %q: a path must not contain ':'", m)
			}
			if m == "~" || strings.HasPrefix(m, "~/") {
				continue // the form ATL_SANDBOX_MOUNTS takes
			}
			p, err := filepath.Abs(m)
			if err != nil {
				return "", nil, fmt.Errorf("mount %q: %w", m, err)
			}
			c.Sandbox.Mounts[i] = p
		}
		if c.Sandbox.empty() {
			c.Sandbox = nil
		}
	}
	var notes []string
	keepTmux := c.TmuxSocket == "" && UserEnv("ATL_TMUX_SOCKET") == "" && isFile(filepath.Join(dir, "data", "atelier.db"))
	if keepTmux {
		c.TmuxSocket = "default"
		notes = append(notes, "tmux_socket = \"default\": "+dir+" already has a database, whose sessions run on tmux's default server; "+
			"remove that line from "+File+" once none runs there (e.g. when moving to atl on the host) to use atl-"+name)
	}
	var buf bytes.Buffer
	buf.WriteString("# atelier workspace " + name + " (see `atl workspace` in atelier's README)\n")
	if err := toml.NewEncoder(&buf).Encode(c); err != nil {
		return "", nil, err
	}
	if keepTmux {
		out := strings.Replace(buf.String(), "tmux_socket = \"default\"\n", tmuxDefaultComment+"tmux_socket = \"default\"\n", 1)
		buf.Reset()
		buf.WriteString(out)
	}
	dirs := []string{"tasks", "data", "config", "state"}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return "", nil, err
		}
	}
	if rel, err := filepath.Rel(dir, repos); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		if err := os.MkdirAll(repos, 0o755); err != nil {
			return "", nil, err
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, File), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", nil, fmt.Errorf("workspace %s already exists (it has %s)", dir, File)
		}
		return "", nil, err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return "", nil, err
	}
	return dir, notes, f.Close()
}

// tmuxDefaultComment precedes the tmux_socket = "default" Create writes
// when it adopts a directory with a database.
const tmuxDefaultComment = `# This directory already had a database when it was adopted, and the atl that
# used it ran its sessions on tmux's default server. Keeping atl there keeps
# them visible: on another server atl would take them for stopped and end them.
# Remove this line once no session runs on the old server (e.g. when moving to
# atl on the host); the default is atl-<name>, a tmux server of its own.
`
