package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/andreujuanc/atelier/internal/gitx"
	"github.com/andreujuanc/atelier/internal/model"
)

// Diff writes, for each rw repo of the task in order, a header
// `==> <repo> (<branch> vs <base_commit[:8]>)`, `git diff <base_commit>`
// (committed and uncommitted changes to tracked files, with ANSI colour if
// color) and the untracked files as `?? <path>` lines. A missing worktree
// is reported under its header. Write errors (e.g. EPIPE when the pager
// quits) are returned as is; git errors are returned after all repos have
// been shown.
func (a *App) Diff(ctx context.Context, ref string, w io.Writer, color bool) error {
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return err
	}
	var errs []error
	n := 0
	for _, r := range t.Repos {
		if r.Mode != model.ModeRW {
			continue
		}
		if n > 0 {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
		n++
		if _, err := fmt.Fprintf(w, "==> %s (%s vs %s)\n", r.Name, t.Branch, shortSHA(r.BaseCommit)); err != nil {
			return err
		}
		wt := a.Paths.WorktreePath(t.Slug, r.Name)
		if _, err := os.Stat(wt); errors.Is(err, os.ErrNotExist) {
			if _, err := fmt.Fprintln(w, "(worktree missing)"); err != nil {
				return err
			}
			continue
		}
		if err := gitx.Diff(ctx, wt, r.BaseCommit, color, w); err != nil {
			errs = append(errs, fmt.Errorf("repo %s: %w", r.Name, err))
			continue
		}
		untracked, err := gitx.Untracked(ctx, wt)
		if err != nil {
			errs = append(errs, fmt.Errorf("repo %s: %w", r.Name, err))
			continue
		}
		for _, p := range untracked {
			if _, err := fmt.Fprintf(w, "?? %s\n", p); err != nil {
				return err
			}
		}
	}
	if n == 0 {
		if _, err := fmt.Fprintln(w, "no rw repos"); err != nil {
			return err
		}
	}
	return errors.Join(errs...)
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
