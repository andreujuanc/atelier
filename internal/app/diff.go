package app

import (
	"context"
	"errors"
	"io"
)

// Diff writes, for each rw repo of the task, a header and `git diff
// <base_commit>` to w (with ANSI colour if color).
// TODO(M7): implement.
func (a *App) Diff(ctx context.Context, ref string, w io.Writer, color bool) error {
	return errors.New("not implemented yet")
}
