package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
)

// Recover marks live sessions (running, waiting, idle) whose tagged tmux
// window no longer exists (or only has dead panes) as stopped, and returns
// how many were marked. If tmux cannot be queried (e.g. not installed) it
// returns 0 and an error the caller may treat as a warning.
//
// Live sessions with an empty tmux target are skipped entirely: they are
// either Claude processes atl did not launch (rows created by hooks) or
// sessions whose window is being launched right now (NewSession, Resume).
//
// The session snapshot is taken before the tmux listing, and each row is
// marked with Store.MarkStoppedIfLaunch, a compare-and-set on the launch
// (tmux target + launch sequence) seen in that snapshot. Together these make
// a stale snapshot harmless: a window recorded before the snapshot was
// tagged before it too, so its absence from the later listing means it is
// really gone; and a session relaunched (or ended) after the snapshot no
// longer matches the launch and is left alone.
func (a *App) Recover(ctx context.Context) (int, error) {
	live, err := a.Store.ListLaunchedLive(ctx)
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
	for _, l := range live {
		if _, ok := tags[strconv.FormatInt(l.ID, 10)]; ok {
			continue
		}
		changed, err := a.Store.MarkStoppedIfLaunch(ctx, l, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("recover session %d: %w", l.ID, err))
			continue
		}
		if changed {
			n++
		}
	}
	return n, errors.Join(errs...)
}
