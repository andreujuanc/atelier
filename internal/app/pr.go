package app

import (
	"context"
	"errors"
)

// PRResult is the outcome of PR for one rw repo.
type PRResult struct {
	Repo, URL string
	Skipped   bool
	Reason    string
}

// PR opens a pull request (gh pr create) for each rw repo of the task that
// has commits and a pushed branch; if any was created the task moves to
// review.
// TODO(M7): implement.
func (a *App) PR(ctx context.Context, ref string) ([]PRResult, error) {
	return nil, errors.New("not implemented yet")
}
