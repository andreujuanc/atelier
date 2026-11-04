package app

import (
	"context"
	"errors"
	"os"
	"sync"

	"github.com/andreujuanc/atelier/internal/gitx"
	"github.com/andreujuanc/atelier/internal/model"
)

// dirtyWorkers bounds the concurrent `git status` runs of DirtyTasks.
const dirtyWorkers = 4

// DirtyTasks reports, for open tasks (backlog, active, review), which have
// uncommitted changes in any rw worktree: the result maps task ID → true
// for dirty tasks only. Missing worktrees and per-worktree git errors are
// ignored (not dirty); only a failure to list tasks is returned.
func (a *App) DirtyTasks(ctx context.Context) (map[int64]bool, error) {
	tasks, err := a.Store.ListTasks(ctx, model.StatusBacklog, model.StatusActive, model.StatusReview)
	if err != nil {
		return nil, err
	}
	type job struct {
		id   int64
		path string
	}
	var jobs []job
	for i := range tasks {
		t := &tasks[i]
		for _, r := range t.Repos {
			if r.Mode == model.ModeRW {
				jobs = append(jobs, job{t.ID, a.RepoPathFor(t, r)})
			}
		}
	}
	out := map[int64]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	ch := make(chan job)
	n := dirtyWorkers
	if len(jobs) < n {
		n = len(jobs)
	}
	for w := 0; w < n; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				mu.Lock()
				done := out[j.id]
				mu.Unlock()
				if done || ctx.Err() != nil {
					continue
				}
				if _, err := os.Stat(j.path); errors.Is(err, os.ErrNotExist) {
					continue
				}
				if dirty, err := gitx.IsDirty(ctx, j.path); err == nil && dirty {
					mu.Lock()
					out[j.id] = true
					mu.Unlock()
				}
			}
		}()
	}
	for _, j := range jobs {
		ch <- j
	}
	close(ch)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
