package hook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/store"
)

const maxSymlinks = 40

// guard is the PreToolUse ro guard: it blocks (exit 2) Edit/Write tool calls
// whose target path is inside the canonical clone ($ATL_REPOS/<name>) of any
// of the task's repos: an ro repo, or an rw one, whose work belongs in its
// worktree (a session launched while the repo was ro still has the clone as
// an --add-dir after it is upgraded to rw). It fails open (exit 0, error for
// the log) on anything it cannot resolve.
func guard(ctx context.Context, in Input, env func(string) string, st *store.Store, p config.Paths) (int, string, error) {
	targets, decodeErr := guardTargets(in.ToolInput)
	if len(targets) == 0 {
		if decodeErr != nil {
			return 0, "", decodeErr
		}
		return 0, "", fmt.Errorf("ro guard: no file_path or notebook_path in %s tool_input", orDash(in.ToolName))
	}

	task, err := guardTask(ctx, in, env, st, p)
	if err != nil {
		return 0, "", fmt.Errorf("ro guard: resolve task: %w", err)
	}
	if task == nil {
		return 0, "", errors.New("ro guard: cannot resolve task (no ATL_TASK, session or task cwd); allowing")
	}
	if len(task.Repos) == 0 {
		return 0, "", nil
	}

	type candidate struct{ given, landing string }
	var candidates []candidate
	var resolveErr error
	for _, target := range targets {
		forms, err := targetForms(target, in.Cwd)
		if err != nil {
			resolveErr = err
		}
		for _, raw := range forms {
			// raw keeps ".." unapplied: the kernel resolves "<dir>/link/../x" by
			// following link first, so a lexical Clean can land somewhere else.
			abs := filepath.Clean(raw)
			candidates = append(candidates, candidate{abs, abs})
			for _, path := range []string{abs, raw} {
				r, err := resolvePath(path)
				if err != nil {
					resolveErr = fmt.Errorf("ro guard: resolve %s: %w", path, err)
					continue
				}
				candidates = append(candidates, candidate{path, r})
			}
		}
	}
	if resolveErr == nil {
		resolveErr = decodeErr
	}

	for _, repo := range task.Repos {
		name := repo.Name
		root := p.RepoPath(name)
		for _, r := range []string{root, realPath(root)} {
			for _, c := range candidates {
				if _, ok := within(r, c.landing); ok {
					what := c.given
					if c.landing != c.given {
						what += " (resolves to " + c.landing + ")"
					}
					if repo.Mode == model.ModeRO {
						return 2, fmt.Sprintf("atelier: %s is inside read-only repo %s; edit it in a rw repo instead", what, name), nil
					}
					return 2, fmt.Sprintf("atelier: %s is inside the original clone of %s; edit the task's worktree %s instead",
						what, name, p.WorktreePath(task.Slug, name)), nil
				}
			}
		}
	}
	return 0, "", resolveErr
}

// guardTargets returns the non-empty string values of the exact keys
// file_path and notebook_path in tool_input (duplicate keys: the last one
// wins, as in JSON.parse). A key with a non-string value is skipped and
// reported in err, so the other key is still checked.
func guardTargets(toolInput json.RawMessage) (targets []string, err error) {
	if len(toolInput) == 0 {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(toolInput, &m); err != nil {
		return nil, fmt.Errorf("ro guard: decode tool_input: %w", err)
	}
	for _, key := range []string{"file_path", "notebook_path"} {
		v, ok := m[key]
		if !ok {
			continue
		}
		var s string
		if uerr := json.Unmarshal(v, &s); uerr != nil {
			err = fmt.Errorf("ro guard: decode tool_input.%s: %w", key, uerr)
			continue
		}
		if s != "" {
			targets = append(targets, s)
		}
	}
	return targets, err
}

// targetForms returns the absolute, uncleaned paths a tool target may
// refer to: the target itself (joined to cwd if relative) and, for a
// leading "~" or "~/", its expansion against the home directory (Claude
// Code expands ~ for its file tools). err reports a form that could not be
// built; the others are still returned.
func targetForms(target, cwd string) (forms []string, err error) {
	if filepath.IsAbs(target) {
		forms = append(forms, target)
	} else if filepath.IsAbs(cwd) {
		forms = append(forms, cwd+string(filepath.Separator)+target)
	} else {
		err = fmt.Errorf("ro guard: relative path %q without absolute cwd", target)
	}
	if target == "~" || strings.HasPrefix(target, "~/") {
		home, herr := os.UserHomeDir()
		switch {
		case herr != nil:
			err = fmt.Errorf("ro guard: expand %q: %w", target, herr)
		case !filepath.IsAbs(home):
			err = fmt.Errorf("ro guard: expand %q: home %q is not absolute", target, home)
		default:
			forms = append(forms, home+string(filepath.Separator)+target[1:])
		}
	}
	return forms, err
}

// guardTask resolves the task for the ro guard: ATL_TASK env → the
// session's task → task dir from cwd. nil, nil if none matches.
func guardTask(ctx context.Context, in Input, env func(string) string, st *store.Store, p config.Paths) (*model.Task, error) {
	if slug := env("ATL_TASK"); slug != "" {
		task, err := st.GetTaskBySlug(ctx, slug)
		if err == nil || !errors.Is(err, store.ErrNotFound) {
			return task, err
		}
	}
	sess, err := findSession(ctx, EventPreToolUse, in, env, st)
	if err != nil {
		return nil, err
	}
	if sess != nil {
		task, err := st.GetTask(ctx, sess.TaskID)
		if err == nil || !errors.Is(err, store.ErrNotFound) {
			return task, err
		}
	}
	return findTask(ctx, in, func(string) string { return "" }, st, p)
}

// resolvePath returns where a write to the absolute path would land,
// resolving it the way the kernel (plus a mkdir -p of missing parents)
// does: components are walked left to right; an existing symlink is
// replaced by its target (absolute targets restart at /) before later
// components are applied; ".." goes to the parent of the directory
// resolved so far; components that do not exist are appended as is. A
// dangling symlink is followed through its target, so a write through it
// is attributed to where it would land.
func resolvePath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%q is not absolute", path)
	}
	const sep = string(filepath.Separator)
	pending := strings.Split(path, sep)
	cur := sep
	links := 0
	for len(pending) > 0 {
		c := pending[0]
		pending = pending[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, c)
		fi, err := os.Lstat(next)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			cur = next // a real entry, or one that does not exist (yet)
			continue
		}
		if links++; links > maxSymlinks {
			return "", errors.New("too many levels of symbolic links")
		}
		link, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(link) {
			cur = sep
		}
		pending = append(strings.Split(link, sep), pending...)
	}
	return cur, nil
}
