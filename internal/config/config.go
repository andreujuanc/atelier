// Package config resolves atelier's filesystem paths from the environment.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Paths holds every filesystem location atelier uses. All fields are absolute.
type Paths struct {
	Repos     string // $ATL_REPOS            default ~/repos
	Tasks     string // $ATL_TASKS            default ~/tasks
	DB        string // $XDG_DATA_HOME/atelier/atelier.db   default ~/.local/share/atelier/atelier.db
	StateDir  string // $XDG_STATE_HOME/atelier             default ~/.local/state/atelier
	ConfigDir string // $XDG_CONFIG_HOME/atelier            default ~/.config/atelier
}

// Resolve reads the environment and returns the resolved paths. A leading
// "~" is expanded to the home directory. Relative ATL_* paths are made
// absolute; relative XDG_* values are ignored in favour of the default. It
// never creates directories.
func Resolve() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve home directory: %w", err)
	}
	get := func(env, def string) (string, error) {
		v := expandHome(os.Getenv(env), home)
		// Per the XDG Base Directory spec, relative XDG_* values are invalid
		// and must be ignored.
		if v == "" || (strings.HasPrefix(env, "XDG_") && !filepath.IsAbs(v)) {
			v = def
		}
		abs, err := filepath.Abs(v)
		if err != nil {
			return "", fmt.Errorf("resolve $%s (%q): %w", env, v, err)
		}
		return abs, nil
	}
	var p Paths
	var data, state, cfg string
	for _, f := range []struct {
		dst      *string
		env, def string
	}{
		{&p.Repos, "ATL_REPOS", filepath.Join(home, "repos")},
		{&p.Tasks, "ATL_TASKS", filepath.Join(home, "tasks")},
		{&data, "XDG_DATA_HOME", filepath.Join(home, ".local", "share")},
		{&state, "XDG_STATE_HOME", filepath.Join(home, ".local", "state")},
		{&cfg, "XDG_CONFIG_HOME", filepath.Join(home, ".config")},
	} {
		if *f.dst, err = get(f.env, f.def); err != nil {
			return Paths{}, err
		}
	}
	p.DB = filepath.Join(data, "atelier", "atelier.db")
	p.StateDir = filepath.Join(state, "atelier")
	p.ConfigDir = filepath.Join(cfg, "atelier")
	return p, nil
}

func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// TaskDir returns Tasks/slug.
func (p Paths) TaskDir(slug string) string { return filepath.Join(p.Tasks, slug) }

// RepoPath returns Repos/name.
func (p Paths) RepoPath(name string) string { return filepath.Join(p.Repos, name) }

// WorktreePath returns Tasks/slug/repo.
func (p Paths) WorktreePath(slug, repo string) string { return filepath.Join(p.Tasks, slug, repo) }

// HookLog returns StateDir/hook.log.
func (p Paths) HookLog() string { return filepath.Join(p.StateDir, "hook.log") }

// ProjectFile returns ConfigDir/projects/<name>.toml.
func (p Paths) ProjectFile(name string) string {
	return filepath.Join(p.ConfigDir, "projects", name+".toml")
}

// AssistantDir returns the atelier assistant's working directory: the
// "assistant" directory next to the database ($XDG_DATA_HOME/atelier/assistant).
func (p Paths) AssistantDir() string { return filepath.Join(filepath.Dir(p.DB), "assistant") }
