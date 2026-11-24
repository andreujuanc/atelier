package workspace

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"

	"github.com/andreujuanc/atelier/internal/config"
)

// settingKeys are atelier.toml's keys, in the order of the design's table.
var settingKeys = []string{"repos", "git_config", "context", "tmux_socket", "sandbox.image", "sandbox.mounts", "sandbox.context"}

// getKey returns setting key of c; sandbox.mounts joined with ':' (a
// mount never contains one).
func getKey(c *Config, key string) string {
	sb := c.Sandbox
	if sb == nil {
		sb = &SandboxConfig{}
	}
	switch key {
	case "repos":
		return c.Repos
	case "git_config":
		return c.GitConfig
	case "context":
		return c.Context
	case "tmux_socket":
		return c.TmuxSocket
	case "sandbox.image":
		return sb.Image
	case "sandbox.mounts":
		return strings.Join(sb.Mounts, ":")
	case "sandbox.context":
		return sb.Context
	}
	return ""
}

// setKey sets setting key of c to v; sandbox.mounts from a ':'-separated
// list (empty parts dropped).
func setKey(c *Config, key, v string) {
	if c.Sandbox == nil {
		c.Sandbox = &SandboxConfig{}
	}
	switch key {
	case "repos":
		c.Repos = v
	case "git_config":
		c.GitConfig = v
	case "context":
		c.Context = v
	case "tmux_socket":
		c.TmuxSocket = v
	case "sandbox.image":
		c.Sandbox.Image = v
	case "sandbox.mounts":
		c.Sandbox.Mounts = nil
		for _, m := range strings.Split(v, ":") {
			if m = strings.TrimSpace(m); m != "" {
				c.Sandbox.Mounts = append(c.Sandbox.Mounts, m)
			}
		}
	case "sandbox.context":
		c.Sandbox.Context = v
	}
}

// fill sets each setting dst does not have to src's, recording in from
// where it came (source(key)) when from is not nil.
func fill(dst *Config, src Config, from map[string]string, source func(key string) string) {
	for _, k := range settingKeys {
		if getKey(dst, k) != "" {
			continue
		}
		if v := getKey(&src, k); v != "" {
			setKey(dst, k, v)
			if from != nil {
				from[k] = source(k)
			}
		}
	}
}

// envFillKeys are the settings `atl workspace new` takes from the user's
// environment.
var envFillKeys = []string{"repos", "context", "sandbox.image", "sandbox.mounts", "sandbox.context"}

// keyVar is the environment variable of a setting.
func keyVar(key string) string {
	for v, k := range envKeys {
		if k == key {
			return v
		}
	}
	return ""
}

// envConfig is the settings the user's environment gives `atl workspace
// new` ($ATL_REPOS, $ATL_SANDBOX_MOUNTS, $ATL_SANDBOX_IMAGE,
// $ATL_SANDBOX_CONTEXT, $ATL_CONTEXT); values atl set itself for a
// workspace (see AtlSet) are not taken: they are that workspace's.
func envConfig() Config {
	var c Config
	for _, k := range envFillKeys {
		setKey(&c, k, UserEnv(keyVar(k)))
	}
	return c
}

// FillFromEnv sets the settings c does not have from the user's
// environment (see envConfig), so that `atl workspace new` run in an old
// box writes that box's settings.
func FillFromEnv(c *Config) { fill(c, envConfig(), nil, nil) }

// Plan is what Create writes for a new or adopted workspace, as Prepare
// computed it (without writing anything).
type Plan struct {
	Name, Dir string
	// Adopt is true when the directory exists (without atelier.toml): it
	// is adopted, its contents kept.
	Adopt bool
	// Config is the content of atelier.toml.
	Config Config
	// Settings are the settings written, in atelier.toml's order, each
	// with where it came from.
	Settings []PlannedSetting
	// Skipped are the values of the box's devcontainer.json not taken,
	// each with the reason.
	Skipped  []string
	Warnings []string
	Notes    []string // on what Prepare decided

	keepTmux bool
	repos    string // the repos directory (made when inside the workspace)
}

// PlannedSetting is one setting a Plan writes.
type PlannedSetting struct {
	Key   string
	Value string // as shown: a path with "~" for the home directory, a context quoted and shortened
	From  string // "--repos", ".devcontainer/devcontainer.json", "$ATL_REPOS", ...
}

func (s PlannedSetting) String() string { return s.Key + " = " + s.Value + " (from " + s.From + ")" }

// flagSource names the `atl workspace new` flag of a setting.
func flagSource(key string) string {
	switch key {
	case "repos":
		return "--repos"
	case "sandbox.mounts":
		return "--mount"
	case "sandbox.image":
		return "--image"
	}
	return "the command line"
}

// Create makes workspace name under root as Prepare plans it, with flags
// the settings given on the command line, and returns the plan.
func Create(root, name string, flags Config) (*Plan, error) {
	p, err := Prepare(root, name, flags)
	if err != nil {
		return nil, err
	}
	return p, p.Write()
}

// Prepare computes, without writing anything, what Create writes for
// workspace name under root: its atelier.toml and its directories. A
// directory that exists without atelier.toml is adopted (its contents are
// kept); one with atelier.toml is refused.
//
// Each setting comes from, in order: flags (the command line); when
// adopting, the box's .devcontainer/devcontainer.json in the directory
// (see readDevcontainer); the user's environment (see envConfig); else
// the default (not written). Relative and "~" paths in repos and
// sandbox.mounts are made absolute (relative to the current directory; a
// mount starting with "~/" is kept as written); a repos directory inside
// the workspace is written relative to it, and left out when it is the
// default.
//
// A directory that has a database (data/atelier.db) was a devcontainer
// box's workspace. Adopted inside that box (the user's $ATL_WORKSPACE is
// that directory) while $ATL_TMUX_SOCKET is not the user's, it writes
// tmux_socket = "default": the box's atl ran its sessions on tmux's
// default server, and on atl-<name> it would not see them, so it would
// take them for stopped and end them. Anywhere else (on the host) it needs
// the repos, the box's canonical clones, and refuses without them (an
// *AdoptDBError): the defaults would leave the box's repos and mounts
// behind.
func Prepare(root, name string, flags Config) (*Plan, error) {
	if err := CheckName(name); err != nil {
		return nil, err
	}
	dir := filepath.Join(root, name)
	if err := CheckSocketBudget(dir); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(filepath.Join(dir, File)); err == nil {
		return nil, fmt.Errorf("workspace %s already exists (it has %s)", dir, File)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	p := &Plan{Name: name, Dir: dir}
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		p.Adopt = true
	}
	c := flags
	if flags.Sandbox != nil {
		sb := *flags.Sandbox
		sb.Mounts = slices.Clone(sb.Mounts)
		c.Sandbox = &sb
	}
	from := map[string]string{}
	for _, k := range settingKeys {
		if getKey(&c, k) != "" {
			from[k] = flagSource(k)
		}
	}
	if p.Adopt {
		im, found, err := readDevcontainer(dir)
		switch {
		case err != nil:
			p.Warnings = append(p.Warnings, err.Error()+"; nothing is taken from it")
		case found:
			fill(&c, im.Config, from, func(string) string { return DevcontainerFile })
			p.Skipped, p.Warnings = im.Skipped, im.Warnings
		}
	}
	fill(&c, envConfig(), from, func(k string) string { return "$" + keyVar(k) })

	if HasDB(dir) {
		switch {
		case inBoxOf(root, dir):
			p.keepTmux = c.TmuxSocket == "" && UserEnv("ATL_TMUX_SOCKET") == ""
		case strings.TrimSpace(c.Repos) == "":
			return nil, &AdoptDBError{Name: name, Dir: dir}
		}
	}
	home, _ := os.UserHomeDir()
	p.repos = filepath.Join(dir, "repos")
	if c.Repos != "" {
		r, err := filepath.Abs(config.ExpandHome(c.Repos, home))
		if err != nil {
			return nil, fmt.Errorf("repos %q: %w", c.Repos, err)
		}
		p.repos = r
		switch rel, err := filepath.Rel(dir, r); {
		case err == nil && rel == "repos":
			c.Repos = ""
		case err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)):
			c.Repos = rel
		default:
			c.Repos = r
		}
	}
	if c.Sandbox != nil {
		for i, m := range c.Sandbox.Mounts {
			if strings.Contains(m, ":") {
				return nil, fmt.Errorf("mount %q: a path must not contain ':'", m)
			}
			if m == "~" || strings.HasPrefix(m, "~/") {
				continue // the form ATL_SANDBOX_MOUNTS takes
			}
			a, err := filepath.Abs(m)
			if err != nil {
				return nil, fmt.Errorf("mount %q: %w", m, err)
			}
			c.Sandbox.Mounts[i] = a
		}
		if c.Sandbox.empty() {
			c.Sandbox = nil
		}
	}
	if strings.Contains(c.TmuxSocket, "/") {
		return nil, fmt.Errorf("tmux_socket %q (from %s): a socket name, not a path", c.TmuxSocket, from["tmux_socket"])
	}
	if p.keepTmux {
		c.TmuxSocket = "default"
		from["tmux_socket"] = "this directory's box, whose sessions run on tmux's default server"
		p.Notes = append(p.Notes, "tmux_socket = \"default\": "+dir+" already has a database, whose sessions run on tmux's default server; "+
			"remove that line from "+File+" once none runs there (e.g. when moving to atl on the host) to use atl-"+name)
	}
	p.Config = c
	for _, k := range settingKeys {
		if v := getKey(&c, k); v != "" {
			p.Settings = append(p.Settings, PlannedSetting{Key: k, Value: showSetting(k, v, home), From: from[k]})
		}
	}
	return p, nil
}

// showSetting is value v of setting key as a Plan shows it.
func showSetting(key, v, home string) string {
	switch key {
	case "repos", "git_config":
		return abbrevHome(v, home)
	case "sandbox.mounts":
		ms := strings.Split(v, ":")
		for i, m := range ms {
			ms[i] = abbrevHome(m, home)
		}
		return strings.Join(ms, ", ")
	case "context", "sandbox.context":
		const max = 40
		if utf8.RuneCountInString(v) > max {
			return strconv.Quote(string([]rune(v)[:max])) + fmt.Sprintf("… (%d bytes)", len(v))
		}
		return strconv.Quote(v)
	}
	return v
}

// abbrevHome writes p with "~" for the home directory.
func abbrevHome(p, home string) string {
	if home == "" || home == "/" {
		return p
	}
	if p == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(p, home+string(filepath.Separator)); ok {
		return "~" + string(filepath.Separator) + rest
	}
	return p
}

// Write makes the workspace p plans: atelier.toml (refused if one exists
// by now), tasks/, data/, config/, state/ and the repos directory when it
// is inside the workspace.
func (p *Plan) Write() error {
	var buf bytes.Buffer
	buf.WriteString("# atelier workspace " + p.Name + " (see `atl workspace` in atelier's README)\n")
	if err := toml.NewEncoder(&buf).Encode(p.Config); err != nil {
		return err
	}
	if p.keepTmux {
		out := strings.Replace(buf.String(), "tmux_socket = \"default\"\n", tmuxDefaultComment+"tmux_socket = \"default\"\n", 1)
		buf.Reset()
		buf.WriteString(out)
	}
	for _, d := range []string{"tasks", "data", "config", "state"} {
		if err := os.MkdirAll(filepath.Join(p.Dir, d), 0o755); err != nil {
			return err
		}
	}
	if rel, err := filepath.Rel(p.Dir, p.repos); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		if err := os.MkdirAll(p.repos, 0o755); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(filepath.Join(p.Dir, File), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("workspace %s already exists (it has %s)", p.Dir, File)
		}
		return err
	}
	_, err = writeFile(f, buf.Bytes())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		// The file is ours (O_EXCL): a partial one would make a retry
		// fail as an existing workspace that does not load.
		os.Remove(f.Name())
	}
	return err
}

// writeFile writes atelier.toml's content; a var so tests can fail it.
var writeFile = (*os.File).Write

// HasDB reports whether dir has a database (data/atelier.db), as the
// workspace of a devcontainer box does.
func HasDB(dir string) bool { return isFile(filepath.Join(dir, "data", "atelier.db")) }

// inBoxOf reports whether atl runs in the box of the workspace in dir: the
// user's $ATL_WORKSPACE (not one atl set, see AtlSet) is dir.
func inBoxOf(root, dir string) bool {
	v := UserEnv(Var)
	if v == "" {
		return false
	}
	sel, err := byNameOrPath(root, v, ByEnv)
	if err != nil {
		return false
	}
	if filepath.Clean(sel.Dir) == filepath.Clean(dir) {
		return true
	}
	a, err1 := os.Stat(sel.Dir)
	b, err2 := os.Stat(dir)
	return err1 == nil && err2 == nil && os.SameFile(a, b)
}

// AdoptDBError is Create's refusal to adopt a directory with a database
// (a devcontainer box's workspace) without knowing its repos: no --repos,
// no ATL_REPOS in its .devcontainer/devcontainer.json, no $ATL_REPOS.
type AdoptDBError struct{ Name, Dir string }

func (e *AdoptDBError) Error() string {
	return fmt.Sprintf("%s has a database (a devcontainer box's workspace?) and no repos were found (no ATL_REPOS in %s): adopt it with %s, "+
		"giving the box's repos and mounts (see \"Moving from a devcontainer box\" in atelier's docs/workspaces.md)", e.Dir, DevcontainerFile, adoptCommand(e.Dir))
}

// reposKnown reports whether adopting dir finds its repos without a flag:
// in its .devcontainer/devcontainer.json or the user's $ATL_REPOS.
func reposKnown(dir string) bool {
	if UserEnv("ATL_REPOS") != "" {
		return true
	}
	im, found, err := readDevcontainer(dir)
	return found && err == nil && im.Config.Repos != ""
}

// adoptCommand is the `atl workspace new` command line that adopts dir, a
// box's workspace.
func adoptCommand(dir string) string {
	return strings.TrimSuffix(newCommand(dir), "`") + " --repos DIR [--mount PATH]...`"
}

// tmuxDefaultComment precedes the tmux_socket = "default" Create writes
// when it adopts a directory with a database.
const tmuxDefaultComment = `# This directory already had a database when it was adopted, and the atl that
# used it ran its sessions on tmux's default server. Keeping atl there keeps
# them visible: on another server atl would take them for stopped and end them.
# Remove this line once no session runs on the old server (e.g. when moving to
# atl on the host); the default is atl-<name>, a tmux server of its own.
`
