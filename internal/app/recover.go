package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/andreujuanc/atelier/internal/model"
)

// Recover marks live sessions (running, waiting, idle) whose tagged tmux
// window no longer exists (or only has dead panes) as stopped, and returns
// how many were marked. If tmux cannot be queried (e.g. not installed) it
// returns 0 and an error the caller may treat as a warning.
//
// Live sessions with an empty tmux target are skipped entirely: they are
// either Claude processes atl did not launch (rows created by hooks) or
// sessions whose window is being launched right now (NewSession, Resume).
// Rows are marked with Store.MarkStoppedIfLive, so a session that ended
// concurrently stays ended.
func (a *App) Recover(ctx context.Context) (int, error) {
	// Sessions are listed before windows: a session that became live after
	// the listing cannot be misjudged by a stale window snapshot.
	live, err := a.Store.ListSessionsByState(ctx, model.StateRunning, model.StateWaiting, model.StateIdle)
	if err != nil {
		return 0, fmt.Errorf("recover: %w", err)
	}
	if len(live) == 0 {
		return 0, nil
	}
	tags, err := a.Tmux.TaggedWindows(ctx)
	if err != nil {
		return 0, fmt.Errorf("recover: %w", err)
	}
	now := a.now()
	n := 0
	var errs []error
	for _, s := range live {
		if s.TmuxTarget == "" {
			continue
		}
		if _, ok := tags[strconv.FormatInt(s.ID, 10)]; ok {
			continue
		}
		changed, err := a.Store.MarkStoppedIfLive(ctx, s.ID, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("recover session %d: %w", s.ID, err))
			continue
		}
		if changed {
			n++
		}
	}
	return n, errors.Join(errs...)
}
