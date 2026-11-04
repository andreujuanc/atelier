package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/tmux"
)

// ClaudeArgv returns the command line for a Claude window:
// ["claude", ("--resume", id)?, "--add-dir", p...].
func ClaudeArgv(roPaths []string, resumeID string) []string {
	argv := []string{"claude"}
	if resumeID != "" {
		argv = append(argv, "--resume", resumeID)
	}
	for _, p := range roPaths {
		argv = append(argv, "--add-dir", p)
	}
	return argv
}

func windowName(id int64) string { return fmt.Sprintf("claude-%d", id) }

// checkLaunchable refuses closed tasks and tasks whose dir is missing, and
// returns the task dir.
func (a *App) checkLaunchable(t *model.Task) (string, error) {
	if !t.Status.Open() {
		return "", fmt.Errorf("task %s is %s; cannot start Claude sessions in it", t.Slug, t.Status)
	}
	dir := a.Paths.TaskDir(t.Slug)
	fi, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("task %s: task dir: %w", t.Slug, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("task %s: task dir %s is not a directory", t.Slug, dir)
	}
	return dir, nil
}

// launch refreshes the task dir and starts a Claude window for s in the
// task's tmux session, returning the window id.
func (a *App) launch(ctx context.Context, t *model.Task, s *model.Session, dir, resumeID string) (string, error) {
	if err := a.prepareTaskDir(ctx, t); err != nil {
		return "", fmt.Errorf("prepare task dir: %w", err)
	}
	return a.Tmux.NewWindow(ctx, tmux.WindowSpec{
		Session: tmux.SessionName(t.Slug),
		Name:    s.Name,
		Dir:     dir,
		Env:     a.launchEnv(t, s),
		Argv:    ClaudeArgv(a.roPaths(t), resumeID),
		Tag:     strconv.FormatInt(s.ID, 10),
	})
}

// NewSession creates a session row (state idle) for the task, launches a
// Claude window for it in the task's tmux session and records the window.
//
// The row exists before its window and has an empty tmux target until the
// launch is recorded; Recover never touches live rows without a target, so a
// concurrent Recover cannot mark the session stopped mid-launch. The window is
// recorded with a targeted update (SetSessionLaunch) so that a claude session
// id or state written meanwhile by a hook is never clobbered.
func (a *App) NewSession(ctx context.Context, ref string) (*model.Session, error) {
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return nil, err
	}
	dir, err := a.checkLaunchable(t)
	if err != nil {
		return nil, err
	}
	now := a.now()
	s := &model.Session{TaskID: t.ID, State: model.StateIdle, CreatedAt: now, LastEventAt: now}
	if err := a.Store.CreateSession(ctx, s); err != nil {
		return nil, err
	}
	s.Name = windowName(s.ID)
	win, err := a.launch(ctx, t, s, dir, "")
	if err != nil {
		if serr := a.Store.SetSessionState(context.WithoutCancel(ctx), s.ID, model.StateEnded, a.now()); serr != nil {
			err = errors.Join(err, serr)
		}
		return nil, fmt.Errorf("task %s: launch session %d: %w", t.Slug, s.ID, err)
	}
	// The window is running: record it even if ctx was cancelled meanwhile.
	if err := a.Store.SetSessionLaunch(context.WithoutCancel(ctx), s.ID, win, s.Name); err != nil {
		return nil, fmt.Errorf("task %s: record session %d (window %s): %w", t.Slug, s.ID, win, err)
	}
	return a.Store.GetSession(context.WithoutCancel(ctx), s.ID)
}

// Resume runs Recover, then relaunches every stopped session of the task
// that has a Claude session id with `claude --resume <id>`. Stopped sessions
// without a Claude id cannot be resumed and are marked ended. It returns the
// number of sessions relaunched; failures for individual sessions are
// joined into the error and leave those sessions stopped.
//
// Each session is first claimed with Store.ClaimStopped (stopped → idle,
// target cleared); only the winner launches, so concurrent Resumes never
// start two windows for one session, and Recover leaves the claimed row
// alone until SetSessionLaunch records its window. If the launch fails the
// session is simply set back to stopped with a targeted state update
// (SetSessionState; claude id, name and target are untouched), so it stays
// resumable. No window was left running, so nothing else can own the row;
// should a briefly started claude have ended it meanwhile, stopped is still
// correct (the conversation can be resumed).
func (a *App) Resume(ctx context.Context, ref string) (int, error) {
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return 0, err
	}
	if _, err := a.Recover(ctx); err != nil {
		return 0, err
	}
	sessions, err := a.Store.ListSessions(ctx, t.ID)
	if err != nil {
		return 0, err
	}
	var stopped []model.Session
	for _, s := range sessions {
		if s.State != model.StateStopped {
			continue
		}
		if s.ClaudeSessionID == "" {
			if err := a.Store.SetSessionState(ctx, s.ID, model.StateEnded, a.now()); err != nil {
				return 0, err
			}
			continue
		}
		stopped = append(stopped, s)
	}
	if len(stopped) == 0 {
		return 0, nil
	}
	dir, err := a.checkLaunchable(t)
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, c := range stopped {
		claimed, err := a.Store.ClaimStopped(ctx, c.ID, a.now())
		if err != nil {
			errs = append(errs, fmt.Errorf("task %s: claim session %d: %w", t.Slug, c.ID, err))
			continue
		}
		if !claimed {
			continue // resumed (or ended) by someone else meanwhile
		}
		if err := a.resumeClaimed(ctx, t, c.ID, dir); err != nil {
			errs = append(errs, fmt.Errorf("task %s: resume session %d: %w", t.Slug, c.ID, err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// resumeClaimed launches `claude --resume` for a session this caller has
// claimed, then kills any other window still tagged with the session (e.g. a
// dead remain-on-exit window) and records the new window; on a failed launch
// it puts the session back to stopped (see Resume).
func (a *App) resumeClaimed(ctx context.Context, t *model.Task, id int64, dir string) (err error) {
	bg := context.WithoutCancel(ctx)
	s, err := a.Store.GetSession(bg, id)
	if err == nil && s.ClaudeSessionID == "" {
		err = errors.New("no claude session id")
	}
	var win string
	if err == nil {
		if s.Name == "" {
			s.Name = windowName(s.ID)
		}
		win, err = a.launch(ctx, t, s, dir, s.ClaudeSessionID)
	}
	if err != nil {
		if serr := a.Store.SetSessionState(bg, id, model.StateStopped, a.now()); serr != nil {
			err = errors.Join(err, serr)
		}
		return err
	}
	// A crashed claude under remain-on-exit leaves a dead window carrying the
	// session's tag; drop it (and anything else still tagged with this
	// session, which this caller now owns), sparing the new window. This runs
	// after the launch so that, when the dead window was the tmux session's
	// only one, the tmux session survives.
	if _, kerr := a.Tmux.KillTaggedWindows(bg, strconv.FormatInt(id, 10), win); kerr != nil {
		a.warnf("session %d: kill old windows: %v", id, kerr)
	}
	if err := a.Store.SetSessionLaunch(bg, id, win, s.Name); err != nil {
		return fmt.Errorf("record window %s: %w", win, err)
	}
	return nil
}

// Open makes sure the task has a live Claude window — resuming stopped
// sessions, else starting a new Claude session — and returns the command that
// attaches the terminal to the task's tmux session.
//
// It runs Recover first and attaches straight away only if one of the task's
// sessions has a live tagged window: a tmux session holding nothing but dead
// (remain-on-exit) windows does not count, so a crashed claude is relaunched
// rather than attached to.
func (a *App) Open(ctx context.Context, ref string) (*exec.Cmd, error) {
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return nil, err
	}
	name := tmux.SessionName(t.Slug)
	if _, err := a.Recover(ctx); err != nil {
		return nil, err
	}
	live, err := a.hasLiveWindow(ctx, t, name)
	if err != nil {
		return nil, err
	}
	if live {
		return a.Tmux.AttachCmd(name), nil
	}
	sessions, err := a.Store.ListSessions(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	for _, s := range sessions {
		if s.State == model.StateStopped && s.ClaudeSessionID != "" {
			if _, err := a.Resume(ctx, t.Slug); err != nil {
				fmt.Fprintf(a.out(), "warning: %v\n", err)
			}
			break
		}
	}
	if live, err = a.hasLiveWindow(ctx, t, name); err != nil {
		return nil, err
	}
	if !live {
		if _, err := a.NewSession(ctx, t.Slug); err != nil {
			return nil, err
		}
	}
	return a.Tmux.AttachCmd(name), nil
}

// hasLiveWindow reports whether the task's tmux session exists and tmux has a
// live (not dead) window tagged with one of the task's sessions.
func (a *App) hasLiveWindow(ctx context.Context, t *model.Task, name string) (bool, error) {
	if !a.Tmux.HasSession(ctx, name) {
		return false, nil
	}
	tags, err := a.Tmux.TaggedWindows(ctx)
	if err != nil {
		return false, err
	}
	if len(tags) == 0 {
		return false, nil
	}
	sessions, err := a.Store.ListSessions(ctx, t.ID)
	if err != nil {
		return false, err
	}
	for _, s := range sessions {
		if _, ok := tags[strconv.FormatInt(s.ID, 10)]; ok {
			return true, nil
		}
	}
	return false, nil
}
