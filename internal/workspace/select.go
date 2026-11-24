package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/andreujuanc/atelier/internal/config"
)

// How a workspace was selected.
const (
	ByFlag = "--workspace"
	ByEnv  = "$" + Var
	ByCwd  = "the current directory"
)

// Selection is the workspace directory picked by Select, not yet loaded.
type Selection struct {
	Name string
	Dir  string // absolute
	By   string // ByFlag, ByEnv or ByCwd
}

// NoneError is returned by Open when nothing selects a workspace.
type NoneError struct {
	Root  string
	Names []string // the existing workspaces
}

func (e *NoneError) Error() string {
	s := fmt.Sprintf("no workspace selected: pass --workspace <name>, or cd $%s/<name> (%s)", RootVar, e.Root)
	if len(e.Names) == 0 {
		return s + "; there are none yet: create one with `atl workspace new <name>`"
	}
	return s + "; workspaces: " + strings.Join(e.Names, ", ")
}

// Select picks the workspace directory: flag (the --workspace value), else
// $ATL_WORKSPACE, else the current directory when it is a workspace
// directory under the root or inside one. It returns nil when nothing
// selects one. It does not check that the directory is a workspace.
func Select(flag string) (*Selection, error) {
	root, err := Root()
	if err != nil {
		return nil, err
	}
	if flag != "" {
		return byNameOrPath(root, flag, ByFlag)
	}
	if v := os.Getenv(Var); v != "" {
		return byNameOrPath(root, v, ByEnv)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil
	}
	name := underRoot(root, cwd)
	if name == "" {
		return nil, nil
	}
	return &Selection{Name: name, Dir: filepath.Join(root, name), By: ByCwd}, nil
}

// EnvDir returns the directory $ATL_WORKSPACE selects, resolved as Select
// resolves it (a name is a directory under the root, anything else a path);
// "" when the variable is unset. It does not check the directory.
func EnvDir() (string, error) {
	v := os.Getenv(Var)
	if v == "" {
		return "", nil
	}
	root, err := Root()
	if err != nil {
		return "", err
	}
	s, err := byNameOrPath(root, v, ByEnv)
	if err != nil {
		return "", err
	}
	return s.Dir, nil
}

// Paths is config.Resolve with $ATL_WORKSPACE read as the CLI reads it
// (EnvDir): for commands that run without selecting a workspace (the
// hooks, the status line), whose $ATL_WORKSPACE may be a name.
func Paths() (config.Paths, error) {
	dir, err := EnvDir()
	if err != nil {
		return config.Paths{}, err
	}
	return config.ResolveIn(dir)
}

// byNameOrPath resolves v, a workspace name or a path (anything with a '/'
// or a leading "~"), to a directory.
func byNameOrPath(root, v, by string) (*Selection, error) {
	if !strings.ContainsRune(v, '/') && !strings.HasPrefix(v, "~") && v != "." && v != ".." {
		if err := CheckName(v); err != nil {
			return nil, fmt.Errorf("%s: %w", by, err)
		}
		return &Selection{Name: v, Dir: filepath.Join(root, v), By: by}, nil
	}
	home, _ := os.UserHomeDir()
	p, err := filepath.Abs(config.ExpandHome(v, home))
	if err != nil {
		return nil, fmt.Errorf("%s (%q): %w", by, v, err)
	}
	return &Selection{Name: filepath.Base(p), Dir: p, By: by}, nil
}

// underRoot returns the name of the workspace directory under root that
// dir is or is inside of; "" if none. It compares dir as given and with
// symlinks resolved against root as given and resolved, and then against
// each entry of the root with symlinks resolved (a workspace that is a
// symlink to elsewhere).
func underRoot(root, dir string) string {
	dirs, roots := []string{dir}, []string{root}
	if d, err := filepath.EvalSymlinks(dir); err == nil {
		dirs = append(dirs, d)
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		roots = append(roots, r)
	}
	for _, r := range roots {
		for _, d := range dirs {
			rel, ok := inside(r, d)
			if !ok || rel == "." {
				continue
			}
			if name, _, _ := strings.Cut(rel, string(filepath.Separator)); CheckName(name) == nil {
				return name
			}
		}
	}
	entries, _ := List(root)
	for _, e := range entries {
		beta, err := filepath.EvalSymlinks(e.Dir)
		if err != nil {
			continue
		}
		for _, d := range dirs {
			if _, ok := inside(beta, d); ok {
				return e.Name
			}
		}
	}
	return ""
}

// inside reports whether d is dir or inside it, and d relative to dir.
func inside(dir, d string) (string, bool) {
	rel, err := filepath.Rel(dir, d)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// Open selects (see Select) and loads the workspace; a *NoneError when
// nothing selects one.
func Open(flag string) (*Workspace, string, error) {
	sel, err := Select(flag)
	if err != nil {
		return nil, "", err
	}
	if sel == nil {
		root, err := Root()
		if err != nil {
			return nil, "", err
		}
		return nil, "", &NoneError{Root: root, Names: Names(root)}
	}
	ws, err := Load(sel.Dir)
	if err != nil {
		if sel.By == ByEnv {
			err = fmt.Errorf("%s: %w", ByEnv, err)
		}
		return nil, "", err
	}
	return ws, sel.By, nil
}

// Applied is a workspace applied to the environment.
type Applied struct {
	*Workspace
	By       string    // how it was selected
	Settings []Setting // the values in effect, with their sources
	// UserVar is true when $ATL_WORKSPACE was the user's when it was
	// applied (set, and not by atl: see AtlSet), as in a devcontainer box.
	UserVar bool

	restore map[string]*string // previous values of what Apply changed (nil: unset)
}

var (
	snapMu    sync.Mutex
	snapTaken bool
	snapshot  []string
)

// OriginalEnv returns atl's environment as it was before the first Apply
// (os.Environ() if nothing was applied), for re-executing atl in another
// workspace.
func OriginalEnv() []string {
	snapMu.Lock()
	defer snapMu.Unlock()
	if !snapTaken {
		snapshot, snapTaken = os.Environ(), true
	}
	return append([]string(nil), snapshot...)
}

// ResetOriginalEnv forgets the snapshot OriginalEnv returns, so that the
// next OriginalEnv (or Apply) takes it anew. For tests only: they run atl
// many times in one process.
func ResetOriginalEnv() {
	snapMu.Lock()
	defer snapMu.Unlock()
	snapshot, snapTaken = nil, false
}

// Apply sets ws's settings in the environment: $ATL_WORKSPACE to ws.Dir,
// and each setting's variable unless the user set it. A variable atl set
// itself (see AtlSet), for this workspace or another, is not the user's:
// it is set anew from ws. What it sets it records, with a hash of each
// value, in $ATL_WORKSPACE_VARS, $ATL_WORKSPACE too (so that an atl it
// starts can tell the value from one the user set, see UserVar).
func Apply(ws *Workspace, by string) (*Applied, error) {
	OriginalEnv() // take the snapshot first
	a := &Applied{Workspace: ws, By: by, UserVar: UserEnv(Var) != "", restore: map[string]*string{}}
	var rec []string
	for _, s := range ws.Settings {
		if cur := os.Getenv(s.Var); cur != "" && !AtlSet(s.Var) {
			a.Settings = append(a.Settings, Setting{Key: s.Key, Var: s.Var, Value: cur, Source: FromEnv})
			continue
		}
		if err := a.set(s.Var, s.Value); err != nil {
			return a, err
		}
		if s.Value != "" {
			rec = append(rec, s.Var+"="+valueHash(s.Value))
		}
		a.Settings = append(a.Settings, s)
	}
	if err := a.set(Var, ws.Dir); err != nil {
		return a, err
	}
	rec = append(rec, Var+"="+valueHash(ws.Dir))
	if err := a.set(VarsVar, strings.Join(rec, ",")); err != nil {
		return a, err
	}
	return a, nil
}

// valueHash is the short hash of an environment value in $ATL_WORKSPACE_VARS.
func valueHash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])[:12]
}

// AtlSet reports whether environment variable k has the value atl set
// itself when it applied a workspace (recorded in $ATL_WORKSPACE_VARS as
// NAME=<hash of the value>, comma-separated). A value the user set since,
// or one never recorded, is the user's.
func AtlSet(k string) bool {
	v := os.Getenv(k)
	if v == "" {
		return false
	}
	for _, e := range strings.Split(os.Getenv(VarsVar), ",") {
		if name, h, ok := strings.Cut(e, "="); ok && name == k {
			return h == valueHash(v)
		}
	}
	return false
}

// UserEnv returns $k unless atl set it (see AtlSet): the user's value, or "".
func UserEnv(k string) string {
	if AtlSet(k) {
		return ""
	}
	return os.Getenv(k)
}

// set sets (value "" unsets) env var k, remembering its old value for Undo.
func (a *Applied) set(k, value string) error {
	if _, done := a.restore[k]; !done {
		if old, ok := os.LookupEnv(k); ok {
			a.restore[k] = &old
		} else {
			a.restore[k] = nil
		}
	}
	if value == "" {
		return os.Unsetenv(k)
	}
	if err := os.Setenv(k, value); err != nil {
		return fmt.Errorf("set $%s from %s: %w", k, File, err)
	}
	return nil
}

// Undo restores the variables Apply changed.
func (a *Applied) Undo() {
	if a == nil {
		return
	}
	for k, v := range a.restore {
		if v == nil {
			os.Unsetenv(k)
		} else {
			os.Setenv(k, *v)
		}
	}
	a.restore = map[string]*string{}
}
