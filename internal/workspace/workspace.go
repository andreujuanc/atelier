// Package workspace finds, loads, creates and applies atelier workspaces: a
// directory $ATL_WORKSPACES/<name>/ with an atelier.toml in it (see
// docs/WORKSPACES.md). Applying one sets environment variables in atl's own
// process; the rest of atl, and every process it starts, reads those.
package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/sandbox"
)

// Environment variables and the marker file.
const (
	RootVar = "ATL_WORKSPACES" // the workspaces root
	Var     = "ATL_WORKSPACE"  // the workspace atl runs in (a name or an absolute path)
	// VarsVar records the variables atl set itself when it applied a
	// workspace (not the user's), as NAME=<hash of the value>,
	// comma-separated (see AtlSet).
	VarsVar = "ATL_WORKSPACE_VARS"
	File    = "atelier.toml"
)

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// CheckName returns an error unless name is a valid workspace name.
func CheckName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("invalid workspace name %q (lowercase letters, digits, '-' and '_', starting with a letter or digit, at most 32)", name)
	}
	return nil
}

// Root returns the workspaces root: $ATL_WORKSPACES, else
// ~/atelier/workspaces ("~" expanded, made absolute). It need not exist.
func Root() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	v := config.ExpandHome(os.Getenv(RootVar), home)
	if v == "" {
		v = filepath.Join(home, "atelier", "workspaces")
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		return "", fmt.Errorf("resolve $%s (%q): %w", RootVar, v, err)
	}
	return abs, nil
}

// Entry is a directory under the root.
type Entry struct {
	Name string
	Dir  string
	// Workspace is true when Dir has an atelier.toml.
	Workspace bool
}

// List returns the directories under root whose names are valid workspace
// names, sorted by name, marking those with an atelier.toml. A missing root
// yields none.
func List(root string) ([]Entry, error) {
	des, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, de := range des {
		if CheckName(de.Name()) != nil {
			continue
		}
		dir := filepath.Join(root, de.Name())
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		e := Entry{Name: de.Name(), Dir: dir}
		if fi, err := os.Stat(filepath.Join(dir, File)); err == nil && fi.Mode().IsRegular() {
			e.Workspace = true
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Names returns the names of the workspaces (with atelier.toml) under root.
func Names(root string) []string {
	es, _ := List(root)
	var names []string
	for _, e := range es {
		if e.Workspace {
			names = append(names, e.Name)
		}
	}
	return names
}

// Config is atelier.toml. Every key is optional.
type Config struct {
	Repos      string         `toml:"repos,omitempty"`
	GitConfig  string         `toml:"git_config,omitempty"`
	Context    string         `toml:"context,omitempty"`
	TmuxSocket string         `toml:"tmux_socket,omitempty"`
	Sandbox    *SandboxConfig `toml:"sandbox,omitempty"`
}

// SandboxConfig is atelier.toml's [sandbox] table.
type SandboxConfig struct {
	Image   string   `toml:"image,omitempty"`
	Mounts  []string `toml:"mounts,omitempty"`
	Context string   `toml:"context,omitempty"`
}

func (s *SandboxConfig) empty() bool {
	return s == nil || (s.Image == "" && len(s.Mounts) == 0 && s.Context == "")
}

// ReadConfig reads dir/atelier.toml strictly: unknown keys are an error.
func ReadConfig(dir string) (Config, error) {
	path := filepath.Join(dir, File)
	var c Config
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	md, err := toml.Decode(string(data), &c)
	if err != nil {
		return c, fmt.Errorf("parse %s: %w", path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, len(und))
		for i, k := range und {
			keys[i] = k.String()
		}
		return c, fmt.Errorf("%s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	if strings.Contains(c.TmuxSocket, "/") {
		return c, fmt.Errorf("%s: tmux_socket %q: a socket name, not a path", path, c.TmuxSocket)
	}
	if c.Sandbox != nil {
		for _, m := range c.Sandbox.Mounts {
			if m == "" || strings.Contains(m, ":") {
				return c, fmt.Errorf("%s: sandbox.mounts: %q: empty or contains ':'", path, m)
			}
		}
	}
	return c, nil
}

// Source says where a setting's value came from.
type Source string

const (
	FromEnv     Source = "env"
	FromFile    Source = File
	FromDefault Source = "default"
)

// Setting is one value a workspace sets in the environment.
type Setting struct {
	Key    string // the atelier.toml key ("repos", "sandbox.image", ...)
	Var    string // the environment variable
	Value  string // "" when unset
	Source Source // "" when unset
}

// Workspace is a loaded workspace.
type Workspace struct {
	Name   string
	Dir    string // absolute
	Config Config
	// Settings are the values from atelier.toml and the defaults, in the
	// order of the design's table, before the environment is consulted.
	Settings []Setting
}

// NotWorkspaceError is returned by Load for a directory without
// atelier.toml.
type NotWorkspaceError struct{ Name, Dir string }

func (e *NotWorkspaceError) Error() string {
	if root, err := Root(); err == nil && HasDB(e.Dir) && !inBoxOf(root, e.Dir) && !reposKnown(e.Dir) {
		return fmt.Sprintf("%s is not a workspace (no %s) and has a database (a devcontainer box's workspace?): adopt it with %s, "+
			"giving the box's repos and mounts (see \"Moving from a devcontainer box\" in atelier's README)", e.Dir, File, adoptCommand(e.Dir))
	}
	return fmt.Sprintf("%s is not a workspace (no %s): adopt it with %s", e.Dir, File, newCommand(e.Dir))
}

// newCommand is the `atl workspace new` command line that makes dir.
func newCommand(dir string) string {
	name := filepath.Base(dir)
	if root, err := Root(); err == nil && filepath.Dir(dir) != root {
		return "`" + RootVar + "=" + filepath.Dir(dir) + " atl workspace new " + name + "`"
	}
	return "`atl workspace new " + name + "`"
}

// Load reads the workspace in dir (absolute); its name is dir's base name.
func Load(dir string) (*Workspace, error) {
	name := filepath.Base(dir)
	fi, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("workspace %s does not exist: create it with %s", dir, newCommand(dir))
	}
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("workspace %s is not a directory", dir)
	}
	c, err := ReadConfig(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, &NotWorkspaceError{Name: name, Dir: dir}
	}
	if err != nil {
		return nil, err
	}
	home, _ := os.UserHomeDir()
	path := func(p string) string {
		p = config.ExpandHome(p, home)
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		return filepath.Clean(p)
	}
	ws := &Workspace{Name: name, Dir: dir, Config: c}
	add := func(key, env, value string, src Source) {
		if value == "" {
			src = ""
		}
		ws.Settings = append(ws.Settings, Setting{Key: key, Var: env, Value: value, Source: src})
	}
	if c.Repos != "" {
		add("repos", "ATL_REPOS", path(c.Repos), FromFile)
	} else {
		add("repos", "ATL_REPOS", filepath.Join(dir, "repos"), FromDefault)
	}
	switch def := filepath.Join(dir, "gitconfig"); {
	case c.GitConfig != "":
		add("git_config", "GIT_CONFIG_GLOBAL", path(c.GitConfig), FromFile)
	case isFile(def):
		add("git_config", "GIT_CONFIG_GLOBAL", def, FromDefault)
	default:
		add("git_config", "GIT_CONFIG_GLOBAL", "", "")
	}
	add("context", "ATL_CONTEXT", c.Context, FromFile)
	if c.TmuxSocket != "" {
		add("tmux_socket", "ATL_TMUX_SOCKET", c.TmuxSocket, FromFile)
	} else {
		add("tmux_socket", "ATL_TMUX_SOCKET", defaultSocket(dir), FromDefault)
	}
	sb := c.Sandbox
	if sb == nil {
		sb = &SandboxConfig{}
	}
	add("sandbox.image", "ATL_SANDBOX_IMAGE", sb.Image, FromFile)
	add("sandbox.mounts", sandbox.MountsVar, strings.Join(sb.Mounts, ":"), FromFile)
	add("sandbox.context", "ATL_SANDBOX_CONTEXT", sb.Context, FromFile)
	return ws, nil
}

// defaultSocket is the default tmux_socket of the workspace in dir:
// atl-<name>, and a hash of dir after it when dir is not directly under the
// root (so that it does not take the server of the root's <name>).
func defaultSocket(dir string) string {
	name := filepath.Base(dir)
	if root, err := Root(); err == nil {
		if filepath.Dir(dir) == root {
			return "atl-" + name
		}
		r, err1 := filepath.EvalSymlinks(root)
		d, err2 := filepath.EvalSymlinks(filepath.Dir(dir))
		if err1 == nil && err2 == nil && r == d {
			return "atl-" + name
		}
	}
	return "atl-" + name + "-" + valueHash(dir)[:6]
}

// settingVars are the variables of the settings, in Settings order.
var settingVars = []string{"ATL_REPOS", "GIT_CONFIG_GLOBAL", "ATL_CONTEXT", "ATL_TMUX_SOCKET", "ATL_SANDBOX_IMAGE", sandbox.MountsVar, "ATL_SANDBOX_CONTEXT"}

// WindowEnv is what atl gives every tmux window it creates (tmux -e), so
// that the window has this process's workspace even on a tmux server
// started with another environment: $ATL_WORKSPACE, $ATL_WORKSPACE_VARS
// and every setting's variable, "" when unset; but GIT_CONFIG_GLOBAL only
// when set (set and empty, git would read no global config at all).
func WindowEnv() map[string]string {
	env := map[string]string{Var: os.Getenv(Var), VarsVar: os.Getenv(VarsVar)}
	for _, k := range settingVars {
		v := os.Getenv(k)
		if v == "" && k == "GIT_CONFIG_GLOBAL" {
			continue
		}
		env[k] = v
	}
	return env
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// CheckSocketBudget returns an error when the gate socket of a task in
// workspace dir (<dir>/run/<12 chars>/docker.sock) would not fit in a unix
// socket path.
func CheckSocketBudget(dir string) error {
	sock := sandbox.SocketPath(config.Paths{Workspace: dir}.RunDir("x"))
	if len(sock) > sandbox.MaxSocketPath {
		return fmt.Errorf("workspace path %s is too long: a task's gate socket there (%s) would be %d bytes, more than the %d a unix socket path can have; use a shorter name or $%s",
			dir, sock, len(sock), sandbox.MaxSocketPath, RootVar)
	}
	return nil
}

// lastFile, in the root, holds the name of the workspace whose TUI was
// opened last (the selector preselects it).
const lastFile = ".last"

// Last returns the name recorded by SaveLast in root, "" if none or it is
// not a valid name.
func Last(root string) string {
	data, err := os.ReadFile(filepath.Join(root, lastFile))
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(string(data))
	if CheckName(name) != nil {
		return ""
	}
	return name
}

// SaveLast records name as the last workspace opened in root (an existing
// directory: it is not created). Errors are the caller's to ignore.
func SaveLast(root, name string) error {
	if err := CheckName(name); err != nil {
		return err
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return fmt.Errorf("workspaces root %s is not a directory", root)
	}
	tmp, err := os.CreateTemp(root, lastFile+".*")
	if err != nil {
		return err
	}
	_, err = tmp.WriteString(name + "\n")
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(root, lastFile))
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}
