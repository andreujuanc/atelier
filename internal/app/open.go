package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"sort"
	"strconv"

	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/store"
	"github.com/andreujuanc/atelier/internal/tmux"
)

// SessionChoice is one session the user can open from a task (the TUI's
// open picker): a live one (attach to its window) or a resumable past one.
type SessionChoice struct {
	Session model.Session
	// Live: the session has a live (not dead) tagged tmux window (Window),
	// whatever its recorded state (an ended row may still have one, e.g.
	// during /clear); picking it attaches. Otherwise it is a past (ended or
	// stopped) session that can be resumed with `claude --resume`.
	Live   bool
	Window string
	// Title is Claude Code's title for the conversation (custom, else
	// generated), else its first user prompt, else the window name.
	Title string
	// LastPrompt is the last user prompt, one line ("" if unknown or the
	// transcript is unreadable).
	LastPrompt string
}

// OpenChoices runs Recover and returns the task's sessions that can be
// opened (see sessionChoices).
func (a *App) OpenChoices(ctx context.Context, ref string) ([]SessionChoice, error) {
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return nil, err
	}
	if _, err := a.Recover(ctx); err != nil {
		return nil, err
	}
	return a.sessionChoices(ctx, t)
}

// sessionChoices lists the task's sessions that can be opened:
//   - live sessions: those with a live (not dead) tagged tmux window, as
//     TaggedWindows reports, whatever their recorded state;
//   - past sessions (ended or stopped, without a live window) of an open
//     task that `claude --resume` run in the task dir can find
//     (resumableHere) and whose recorded transcript file still exists.
//
// Live sessions come first, then past ones; each group is ordered by last
// activity, newest first. Titles and prompts are read from the transcripts
// (readTranscriptInfo); an unreadable transcript only loses them.
func (a *App) sessionChoices(ctx context.Context, t *model.Task) ([]SessionChoice, error) {
	sessions, err := a.Store.ListSessions(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	if len(sessions) == 0 {
		return nil, nil
	}
	tags, err := a.Tmux.TaggedWindows(ctx)
	if err != nil {
		return nil, err
	}
	dir := a.Paths.TaskDir(t.Slug)
	var out []SessionChoice
	for _, s := range sessions {
		c := SessionChoice{Session: s}
		if win, ok := tags[strconv.FormatInt(s.ID, 10)]; ok {
			c.Live, c.Window = true, win
		} else if s.State.Live() {
			continue // not launched by atl, or its window is being launched
		} else if !resumableState(s.State) || s.ClaudeSessionID == "" || !t.Status.Open() ||
			resumableHere(dir, s) != nil {
			continue
		}
		var info transcriptInfo
		if s.TranscriptPath != "" {
			var err error
			info, err = readTranscriptInfo(s.TranscriptPath)
			if err != nil && !c.Live && errors.Is(err, fs.ErrNotExist) {
				continue // claude --resume would find no conversation
			}
		}
		c.Title, c.LastPrompt = info.Title, info.LastPrompt
		if c.Title == "" {
			c.Title = info.FirstPrompt
		}
		if c.Title == "" {
			c.Title = sessionName(s)
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		x, y := out[i], out[j]
		if x.Live != y.Live {
			return x.Live
		}
		if !x.Session.LastEventAt.Equal(y.Session.LastEventAt) {
			return x.Session.LastEventAt.After(y.Session.LastEventAt)
		}
		return x.Session.ID > y.Session.ID
	})
	return out, nil
}

func resumableState(st model.SessionState) bool {
	return st == model.StateEnded || st == model.StateStopped
}

// resumableHere returns nil if `claude --resume` run in the task dir dir can
// find the past session s's conversation, else why not. Claude Code looks
// for it only in its project dir for the cwd (claudeProjectDirs), so a
// session the hooks recorded for a claude run elsewhere (a task subdir, a
// nested `claude -p` with ATL_TASK set) cannot be resumed from there: its
// recorded transcript must sit directly in that dir. A row with no recorded
// transcript path (older rows) is trusted only if atl launched it (it has a
// window name), since atl always launches claude in the task dir.
func resumableHere(dir string, s model.Session) error {
	if s.TranscriptPath == "" {
		if s.Name == "" {
			return errors.New("it was not started by atl and has no recorded transcript, so claude --resume in the task dir may not find it")
		}
		return nil
	}
	if !inClaudeProject(dir, s.TranscriptPath) {
		return fmt.Errorf("its transcript %s is not in Claude's project dir for the task dir %s (claude ran in another directory), so claude --resume there cannot find it",
			s.TranscriptPath, dir)
	}
	return nil
}

// sessionName is the session's window name, or the name it would get.
func sessionName(s model.Session) string {
	if s.Name != "" {
		return s.Name
	}
	return windowName(s.ID)
}

// OpenOrChoose is the TUI's Enter: after Recover, if the task has sessions
// that can be opened (OpenChoices) it returns them and launches nothing, so
// the user can pick one (OpenSession) or a new session (OpenNew). Otherwise
// it starts a new Claude session and returns the command that attaches to
// it. Exactly one of the command and the choices is non-nil on success.
func (a *App) OpenOrChoose(ctx context.Context, ref string) (*exec.Cmd, []SessionChoice, error) {
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	if _, err := a.Recover(ctx); err != nil {
		return nil, nil, err
	}
	choices, err := a.sessionChoices(ctx, t)
	if err != nil {
		return nil, nil, err
	}
	if len(choices) > 0 {
		return nil, choices, nil
	}
	cmd, err := a.OpenNew(ctx, t.Slug)
	return cmd, nil, err
}

// OpenNew starts a new Claude session in the task (as NewSession) and
// returns the command that attaches to the task's tmux session with the new
// window selected.
func (a *App) OpenNew(ctx context.Context, ref string) (*exec.Cmd, error) {
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return nil, err
	}
	s, err := a.NewSession(ctx, t.Slug)
	if err != nil {
		return nil, err
	}
	a.selectWindow(ctx, s.ID, s.TmuxTarget)
	return a.Tmux.AttachCmd(tmux.SessionName(t.Slug)), nil
}

// OpenSession opens one session of the task and returns the command that
// attaches to the task's tmux session with that session's window selected:
//   - a session with a live (not dead) tagged window is simply selected,
//     whatever its recorded state: it is never resumed over;
//   - a past (ended or stopped) session is resumed with `claude --resume
//     <claude id>`. It is claimed first with Store.ClaimFrom (a compare-and-
//     set on the state seen here, bumping launch_seq), so concurrent opens or
//     Resumes never start two windows for it; a failed launch puts it back
//     to the state it had. Other stopped sessions are left alone.
//
// It refuses sessions of other tasks, live sessions without an atelier
// window, sessions without a Claude id, past sessions `claude --resume`
// cannot find from the task dir (resumableHere) or whose transcript file is
// gone, and closed tasks (for resuming).
func (a *App) OpenSession(ctx context.Context, ref string, sessionID int64) (*exec.Cmd, error) {
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return nil, err
	}
	if _, err := a.Recover(ctx); err != nil {
		return nil, err
	}
	s, err := a.Store.GetSession(ctx, sessionID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("task %s has no session %d", t.Slug, sessionID)
	}
	if err != nil {
		return nil, err
	}
	if s.TaskID != t.ID {
		other := strconv.FormatInt(s.TaskID, 10)
		if ot, err := a.Store.GetTask(ctx, s.TaskID); err == nil {
			other = ot.Slug
		}
		return nil, fmt.Errorf("session %d belongs to task %s, not %s", s.ID, other, t.Slug)
	}
	attach := a.Tmux.AttachCmd(tmux.SessionName(t.Slug))
	tags, err := a.Tmux.TaggedWindows(ctx)
	if err != nil {
		return nil, err
	}
	if win, ok := tags[strconv.FormatInt(s.ID, 10)]; ok {
		a.selectWindow(ctx, s.ID, win)
		return attach, nil
	}
	if s.State.Live() {
		return nil, fmt.Errorf("session %d of task %s is %s but has no live atelier tmux window (not started by atl, or still starting); cannot attach to it",
			s.ID, t.Slug, s.State)
	}
	if s.ClaudeSessionID == "" {
		return nil, fmt.Errorf("session %d of task %s is %s and has no Claude session id; it cannot be resumed (use --new for a new session)",
			s.ID, t.Slug, s.State)
	}
	if err := resumableHere(a.Paths.TaskDir(t.Slug), *s); err != nil {
		return nil, fmt.Errorf("session %d of task %s cannot be resumed: %w (use --new for a new session)", s.ID, t.Slug, err)
	}
	if s.TranscriptPath != "" {
		if _, err := os.Stat(s.TranscriptPath); errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("session %d of task %s: its transcript %s no longer exists; it cannot be resumed",
				s.ID, t.Slug, s.TranscriptPath)
		}
	}
	dir, err := a.checkLaunchable(t)
	if err != nil {
		return nil, err
	}
	claimed, err := a.Store.ClaimFrom(ctx, s.ID, s.State, a.now())
	if err != nil {
		return nil, fmt.Errorf("task %s: claim session %d: %w", t.Slug, s.ID, err)
	}
	if !claimed {
		now := "changed"
		if cur, err := a.Store.GetSession(ctx, s.ID); err == nil {
			now = string(cur.State)
		}
		return nil, fmt.Errorf("session %d of task %s was resumed or changed meanwhile (now %s); try again", s.ID, t.Slug, now)
	}
	win, err := a.resumeClaimed(ctx, t, s.ID, dir, s.State)
	if err != nil {
		return nil, fmt.Errorf("task %s: resume session %d: %w", t.Slug, s.ID, err)
	}
	a.selectWindow(ctx, s.ID, win)
	return attach, nil
}

// selectWindow makes win the current window of its tmux session; a failure
// is only a warning (attaching still works, on another window).
func (a *App) selectWindow(ctx context.Context, id int64, win string) {
	if win == "" {
		return
	}
	if err := a.Tmux.SelectWindow(context.WithoutCancel(ctx), win); err != nil {
		a.warnf("session %d: %v", id, err)
	}
}
