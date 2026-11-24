package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"syscall"

	"github.com/andreujuanc/atelier/internal/sandbox"
	"github.com/andreujuanc/atelier/internal/store"
	"github.com/andreujuanc/atelier/internal/tmux"
)

// Recover marks live sessions (running, waiting, idle) whose tagged tmux
// window no longer exists (or only has dead panes) as stopped, and returns
// how many were marked. If tmux cannot be queried (e.g. not installed) it
// returns 0 and an error the caller may treat as a warning.
//
// Live sessions with an empty tmux target are either Claude processes atl
// did not launch (rows created by hooks), which are skipped entirely, or
// sessions atl launched without recording the window yet: a launch under
// way (NewSession, Resume), or one whose atl was killed before it recorded
// it. Those are left alone while their task's launch lock is held (every
// launch holds it until its window is recorded); see recoverUnrecorded.
//
// The session snapshot is taken before the tmux listing, and each row is
// marked with Store.MarkStoppedIfLaunch, a compare-and-set on the launch
// (tmux target + launch sequence) seen in that snapshot. Together these make
// a stale snapshot harmless: a window recorded before the snapshot was
// tagged before it too, so its absence from the later listing means it is
// really gone; and a session relaunched (or ended) after the snapshot no
// longer matches the launch and is left alone.
func (a *App) Recover(ctx context.Context) (int, error) {
	if sandbox.Inside() {
		// No tmux server in a task container: every window would look gone.
		return 0, nil
	}
	live, err := a.Store.ListLaunchedLive(ctx)
	if err != nil {
		return 0, fmt.Errorf("recover: %w", err)
	}
	unrecorded, err := a.Store.ListUnrecordedLaunches(ctx)
	if err != nil {
		return 0, fmt.Errorf("recover: %w", err)
	}
	if len(live) == 0 && len(unrecorded) == 0 {
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
			a.killStopped(ctx, l.ID)
		}
	}
	m, err := a.recoverUnrecorded(ctx, unrecorded)
	return n + m, errors.Join(append(errs, err)...)
}

// recoverUnrecorded marks stopped, as Recover does, the sessions of
// unrecorded (Store.ListUnrecordedLaunches) whose launch is over without
// its window recorded (atl was killed while it launched it) and that have
// no window any more, and returns how many it marked. A task whose launch
// lock it cannot take at once has a launch under way: its sessions are left
// alone. Under the lock, a session whose window is there (tagged with it,
// or, as the killed atl may have made it and not tagged it yet, named after
// it in the task's tmux session) is left alone too, until it is gone.
func (a *App) recoverUnrecorded(ctx context.Context, unrecorded []store.Unrecorded) (int, error) {
	byTask := map[int64][]store.Unrecorded{}
	var order []int64
	for _, u := range unrecorded {
		if byTask[u.TaskID] == nil {
			order = append(order, u.TaskID)
		}
		byTask[u.TaskID] = append(byTask[u.TaskID], u)
	}
	n := 0
	var errs []error
	for _, id := range order {
		m, err := a.recoverUnrecordedIn(ctx, id, byTask[id])
		n += m
		if err != nil {
			errs = append(errs, err)
		}
	}
	return n, errors.Join(errs...)
}

// recoverUnrecordedIn is recoverUnrecorded for the sessions us of task id.
func (a *App) recoverUnrecordedIn(ctx context.Context, id int64, us []store.Unrecorded) (int, error) {
	path, err := a.lockPath(fmt.Sprintf("task-%d.launch.lock", id))
	if err != nil {
		return 0, fmt.Errorf("recover: %w", err)
	}
	unlock, err := lockFile(ctx, path, syscall.LOCK_EX, 0, "a session is being started in the task")
	if err != nil {
		return 0, nil // a launch under way (or no telling): left for a later Recover
	}
	defer unlock()
	// Listed now, under the lock: a window made by a launch that has just
	// ended is in it.
	tags, err := a.Tmux.TaggedWindows(ctx)
	if err != nil {
		return 0, fmt.Errorf("recover: %w", err)
	}
	n := 0
	var errs []error
	for _, u := range us {
		if _, ok := tags[strconv.FormatInt(u.ID, 10)]; ok {
			continue
		}
		name := u.Name
		if name == "" {
			name = windowName(u.ID)
		}
		if alive, err := a.Tmux.WindowAlive(ctx, tmux.SessionName(u.Slug), name); err != nil || alive {
			continue
		}
		changed, err := a.Store.MarkStoppedIfUnrecorded(ctx, u, a.now())
		if err != nil {
			errs = append(errs, fmt.Errorf("recover session %d: %w", u.ID, err))
			continue
		}
		if changed {
			n++
			a.killStopped(ctx, u.ID)
		}
	}
	return n, errors.Join(errs...)
}

// killStopped ends what session id, whose window Recover found gone, left
// running in its task's container (killSessionProcs). Recover runs when
// atl starts, so this covers a box that restarted while the task
// containers kept running.
func (a *App) killStopped(ctx context.Context, id int64) {
	if a.Sandbox == nil {
		return
	}
	s, err := a.Store.GetSession(ctx, id)
	if err != nil {
		return
	}
	t, err := a.Store.GetTask(ctx, s.TaskID)
	if err != nil {
		return
	}
	a.killSessionProcs(ctx, t, id)
}
