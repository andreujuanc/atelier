// Package config resolves atelier's filesystem paths from the environment.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Paths holds every filesystem location atelier uses. All fields are absolute.
//
// With $ATL_WORKSPACE set, everything atelier owns lives in that one
// directory (Tasks default, DB, StateDir, ConfigDir) and XDG_* is ignored.
type Paths struct {
	Workspace string // $ATL_WORKSPACE        "" when unset
	Repos     string // $ATL_REPOS            default ~/repos
	Tasks     string // $ATL_TASKS            default $ATL_WORKSPACE/tasks, else ~/tasks
	DB        string // $ATL_WORKSPACE/data/atelier.db, else $XDG_DATA_HOME/atelier/atelier.db (~/.local/share/atelier/atelier.db)
	StateDir  string // $ATL_WORKSPACE/state,           else $XDG_STATE_HOME/atelier (~/.local/state/atelier)
	ConfigDir string // $ATL_WORKSPACE/config,          else $XDG_CONFIG_HOME/atelier (~/.config/atelier)
}

// Resolve reads the environment and returns the resolved paths. A leading
// "~" is expanded to the home directory. Relative ATL_* paths are made
// absolute; relative XDG_* values are ignored in favour of the default. It
// never creates directories. $ATL_WORKSPACE, when set, must be an existing
// directory: a typo or a missing mount is an error, never a fresh empty
// database somewhere else.
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
	if os.Getenv("ATL_WORKSPACE") != "" {
		if p.Workspace, err = get("ATL_WORKSPACE", ""); err != nil {
			return Paths{}, err
		}
		fi, err := os.Stat(p.Workspace)
		if err != nil {
			return Paths{}, fmt.Errorf("$ATL_WORKSPACE (%s): %w", p.Workspace, err)
		}
		if !fi.IsDir() {
			return Paths{}, fmt.Errorf("$ATL_WORKSPACE (%s) is not a directory", p.Workspace)
		}
	}
	tasksDef := filepath.Join(home, "tasks")
	if p.Workspace != "" {
		tasksDef = filepath.Join(p.Workspace, "tasks")
	}
	vars := []struct {
		dst      *string
		env, def string
	}{
		{&p.Repos, "ATL_REPOS", filepath.Join(home, "repos")},
		{&p.Tasks, "ATL_TASKS", tasksDef},
	}
	var data, state, cfg string
	if p.Workspace == "" {
		vars = append(vars, []struct {
			dst      *string
			env, def string
		}{
			{&data, "XDG_DATA_HOME", filepath.Join(home, ".local", "share")},
			{&state, "XDG_STATE_HOME", filepath.Join(home, ".local", "state")},
			{&cfg, "XDG_CONFIG_HOME", filepath.Join(home, ".config")},
		}...)
	}
	for _, f := range vars {
		if *f.dst, err = get(f.env, f.def); err != nil {
			return Paths{}, err
		}
	}
	if p.Workspace != "" {
		p.DB = filepath.Join(p.Workspace, "data", "atelier.db")
		p.StateDir = filepath.Join(p.Workspace, "state")
		p.ConfigDir = filepath.Join(p.Workspace, "config")
		return p, nil
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

// RunDir returns the directory of task slug's sandbox runtime files (the
// gate's socket, the atl binary for the task container): a directory named
// by a short hash of the task dir under $ATL_WORKSPACE/run, else under
// <StateDir>-run. The name is short and fixed-length because the socket path
// must fit in 107 bytes. It is outside StateDir and the data dir, which a
// task container gets to see, so one task never sees another's socket.
func (p Paths) RunDir(slug string) string {
	return filepath.Join(p.runRoot(), p.taskHash(slug))
}

func (p Paths) taskHash(slug string) string {
	sum := sha256.Sum256([]byte(p.TaskDir(slug)))
	return hex.EncodeToString(sum[:])[:12]
}

// ClaudeDir returns the Claude Code config dir of task slug's container
// (its $CLAUDE_CONFIG_DIR: state file, settings, transcripts): claude/<hash>
// next to the run dirs, the hash as in RunDir. A task container mounts its
// own, never the parent, so no task reaches another's dir or the user's.
func (p Paths) ClaudeDir(slug string) string {
	return filepath.Join(p.runRoot(), "claude", p.taskHash(slug))
}

func (p Paths) runRoot() string {
	if p.Workspace != "" {
		return filepath.Join(p.Workspace, "run")
	}
	return p.StateDir + "-run"
}

// OriginPin returns the file in which atl pins the origin URLs of repo:
// origins/<repo>.json next to the run dirs. A task container gets only its
// own run dir, never their parent, so no task can read or write it.
func (p Paths) OriginPin(repo string) string {
	return filepath.Join(p.runRoot(), "origins", repo+".json")
}

// HookLog returns StateDir/hook.log.
func (p Paths) HookLog() string { return filepath.Join(p.StateDir, "hook.log") }

// RefusalLog returns StateDir/refused.log, where atl records the git
// commands it refused to run because of a repo's config.
func (p Paths) RefusalLog() string { return filepath.Join(p.StateDir, "refused.log") }

// ProjectFile returns ConfigDir/projects/<name>.toml.
func (p Paths) ProjectFile(name string) string {
	return filepath.Join(p.ConfigDir, "projects", name+".toml")
}

// AssistantDir returns the atelier assistant's working directory: the
// "assistant" directory next to the database ($XDG_DATA_HOME/atelier/assistant).
func (p Paths) AssistantDir() string { return filepath.Join(filepath.Dir(p.DB), "assistant") }

// ClaudeConfigDir returns Claude Code's config dir: $CLAUDE_CONFIG_DIR,
// else ~/.claude ("" if neither is known).
func ClaudeConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// ClaudeUserSettings returns the path of Claude Code's user (global)
// settings file, <ClaudeConfigDir>/settings.json ("" if the config dir is
// unknown).
func ClaudeUserSettings() string {
	d := ClaudeConfigDir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, "settings.json")
}
