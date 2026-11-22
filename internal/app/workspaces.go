package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/andreujuanc/atelier/internal/gitx"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/store"
	"github.com/andreujuanc/atelier/internal/workspace"
)

// readOnlyBusy is how long a read of another workspace's database waits for
// its writers.
const readOnlyBusy = 2 * time.Second

// openTasks returns the open tasks (backlog, active, review) in database
// db, read read-only.
func openTasks(ctx context.Context, db string) ([]model.Task, error) {
	st, err := store.OpenReadOnly(db, readOnlyBusy)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	return st.ListTasks(ctx, model.StatusBacklog, model.StatusActive, model.StatusReview)
}

// wsTask is an open task of another workspace.
type wsTask struct {
	Workspace  string // the workspace's name
	Slug       string
	PortOffset int
}

// otherWorkspaces is what otherWorkspaceTasks found.
type otherWorkspaces struct {
	Tasks    []wsTask
	Read     int      // databases read
	Warnings []string // one per database (or root) that could not be read
}

// otherWorkspaceTasks returns the open tasks of every other workspace under
// the workspaces root ($ATL_WORKSPACES), each database read read-only. Every
// directory under the root with a valid workspace name and a
// data/atelier.db counts, with or without atelier.toml (an old box not yet
// adopted still binds host ports); one without a database has no tasks.
// This workspace is left out: the directory that resolves to
// a.Paths.Workspace, and a database that is a.Paths.DB. A database that
// cannot be read (locked past the busy timeout, not SQLite, another schema
// version) is skipped with a warning.
func (a *App) otherWorkspaceTasks(ctx context.Context) otherWorkspaces {
	var o otherWorkspaces
	root, err := workspace.Root()
	if err != nil {
		o.Warnings = append(o.Warnings, err.Error()+"; the port offsets of the other workspaces are not avoided")
		return o
	}
	entries, err := workspace.List(root)
	if err != nil {
		o.Warnings = append(o.Warnings, fmt.Sprintf("list workspaces in %s: %v; the port offsets of the other workspaces are not avoided", root, err))
		return o
	}
	self := ""
	if a.Paths.Workspace != "" {
		self = resolvedPath(a.Paths.Workspace)
	}
	selfDB, _ := os.Stat(a.Paths.DB)
	for _, e := range entries {
		if self != "" && resolvedPath(e.Dir) == self {
			continue
		}
		db := filepath.Join(e.Dir, "data", "atelier.db")
		fi, err := os.Stat(db)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err == nil && selfDB != nil && os.SameFile(fi, selfDB) {
			continue
		}
		var tasks []model.Task
		if err == nil {
			tasks, err = openTasks(ctx, db)
		}
		if err != nil {
			o.Warnings = append(o.Warnings, fmt.Sprintf("workspace %s: %v; the port offsets of its tasks are not avoided", e.Name, err))
			continue
		}
		o.Read++
		for _, t := range tasks {
			o.Tasks = append(o.Tasks, wsTask{Workspace: e.Name, Slug: t.Slug, PortOffset: t.PortOffset})
		}
	}
	return o
}

// resolvedPath is p with symlinks resolved, or cleaned when that fails.
func resolvedPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// workspaceName names this workspace in messages.
func (a *App) workspaceName() string {
	switch {
	case a.Workspace != nil && a.Workspace.Workspace != nil && a.Workspace.Name != "":
		return a.Workspace.Name
	case a.Paths.Workspace != "":
		return filepath.Base(a.Paths.Workspace)
	}
	return "this workspace"
}

// slugTaken reports, for a new task with repos, whether slug is taken
// outside the database: something is at its task dir, or branch
// atl/<slug>, or one below it (atl/<slug>/x, which keeps git from creating
// atl/<slug>), exists in the canonical clone of one of its rw repos,
// locally or on origin as last fetched.
func (a *App) slugTaken(ctx context.Context, repos []model.TaskRepo) func(string) (bool, error) {
	return func(slug string) (bool, error) {
		if _, err := os.Lstat(a.Paths.TaskDir(slug)); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("task dir: %w", err)
		}
		for _, r := range repos {
			if r.Mode != model.ModeRW {
				continue
			}
			for _, ref := range []string{"refs/heads/atl/" + slug, "refs/remotes/origin/atl/" + slug} {
				ok, err := gitx.RefOrBelowExists(ctx, a.Paths.RepoPath(r.Name), ref)
				if err != nil {
					return false, fmt.Errorf("repo %s: %w", r.Name, err)
				}
				if ok {
					return true, nil
				}
			}
		}
		return false, nil
	}
}

// checkPortOffsets warns about open tasks of this workspace that share a
// port offset (a reopened task keeps its old one), about each open task of
// this workspace whose port offset an open task of another workspace has
// too (see otherWorkspaceTasks), and about other workspaces it could not
// read. Run where $ATL_WORKSPACE is the user's (a devcontainer box, which
// sees only its own workspace) with no other workspace read, it says that
// the other workspaces' offsets were not checked.
func (a *App) checkPortOffsets(ctx context.Context) Check {
	const name = "port offsets"
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	mine, err := openTasks(ctx, a.Paths.DB)
	if err != nil {
		return warn(name, fmt.Sprintf("cannot read this workspace's tasks: %v", err))
	}
	others := a.otherWorkspaceTasks(ctx)
	byOffset := map[int][]wsTask{}
	for _, o := range others.Tasks {
		byOffset[o.PortOffset] = append(byOffset[o.PortOffset], o)
	}
	sort.Slice(mine, func(i, j int) bool {
		if mine[i].PortOffset != mine[j].PortOffset {
			return mine[i].PortOffset < mine[j].PortOffset
		}
		return mine[i].Slug < mine[j].Slug
	})
	self := a.workspaceName()
	var own, clashes []string
	for i := 0; i < len(mine); {
		j := i + 1
		for j < len(mine) && mine[j].PortOffset == mine[i].PortOffset {
			j++
		}
		if j-i > 1 {
			var slugs []string
			for _, t := range mine[i:j] {
				slugs = append(slugs, self+"/"+t.Slug)
			}
			own = append(own, fmt.Sprintf("%s (+%d)", strings.Join(slugs, " and "), mine[i].PortOffset))
		}
		i = j
	}
	for _, t := range mine {
		for _, o := range byOffset[t.PortOffset] {
			clashes = append(clashes, fmt.Sprintf("%s/%s and %s/%s (+%d)", self, t.Slug, o.Workspace, o.Slug, t.PortOffset))
		}
	}
	var parts []string
	if len(own) > 0 {
		parts = append(parts, "open tasks of this workspace have the same port offset (a reopened task keeps its old one), so their ports collide while both run: "+
			strings.Join(own, "; ")+"; close one of each (a new task gets a free offset)")
	}
	if len(clashes) > 0 {
		parts = append(parts, "open tasks of two workspaces have the same port offset, so their ports collide while both run: "+
			strings.Join(clashes, "; ")+"; close one of each pair (new tasks avoid both offsets)")
	}
	parts = append(parts, others.Warnings...)
	if len(parts) > 0 {
		return warn(name, strings.Join(parts, "; "))
	}
	detail := fmt.Sprintf("no two open tasks share an offset, in this workspace or with one of another workspace (%d other workspaces read)", others.Read)
	if others.Read == 0 && a.Workspace != nil && a.Workspace.UserVar {
		detail += "; $" + workspace.Var + " comes from the environment (a devcontainer box?), where the other workspaces of this host " +
			"may not be under $" + workspace.RootVar + ": their offsets are not checked, nor avoided by new tasks"
	}
	return pass(name, detail)
}
