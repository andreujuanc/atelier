package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/sandbox"
	"github.com/andreujuanc/atelier/internal/tmux"
)

// ErrRestartNeeded is AddRepo's refusal while the task has sessions that
// would not see the new repo: AddRepos adds it and restarts them.
var ErrRestartNeeded = errors.New("its sessions would not see the repo until they are restarted; add it with atl add, which restarts them")

// BusyError is AddRepos' refusal to restart a task's sessions while things
// run in it (see taskActivity). Nothing was changed.
type BusyError struct {
	Slug    string
	Running []string // one line each: what runs
}

func (e *BusyError) Error() string {
	return fmt.Sprintf("cannot restart task %s's sessions to add the repo: things are running in it: %s",
		e.Slug, strings.Join(e.Running, "; "))
}

// AddOpts are the options of AddRepos.
type AddOpts struct {
	NoFetch bool // do not fetch origin before branching (rw)
	// Force restarts the task's sessions even while things run in it,
	// which ends them.
	Force bool
}

// AddResult is what AddRepos did.
type AddResult struct {
	Task   *model.Task // the task, re-read after the last add that worked
	Added  []RepoArg
	Failed []error // one per repo that was not added, naming it
	// Restarted is how many sessions were resumed in a new container.
	Restarted int
}

// AddRepos adds repos to the open task ref (see AddRepo), one after the
// other, under the task's lock. A repo never reaches a session that runs
// already when the session runs in the task's container (which cannot get
// a new mount) nor, for ro, at all (Claude gets it with --add-dir at
// launch): then the task's sessions are restarted around the adds. Their
// windows are closed and the container removed (which ends whatever runs
// in it), the repos added, and the same Claude conversations resumed
// (claude --resume), in a new container that has the repos.
//
// What can fail on its own comes first, while the sessions still run: each
// repo is checked (checkAdd) and, for rw unless o.NoFetch, fetched; a repo
// that fails is in the result's Failed and is not added, and when none is
// left nothing is restarted. Then, unless o.Force, a task where things run
// (taskActivity) is refused with a *BusyError, before anything changes.
// Once the sessions are closed, the rest runs to its end even if ctx is
// cancelled (an interrupt must not leave them closed), and they are
// resumed even if every add failed.
//
// The error is for what keeps the whole call from running (or the resume
// failing); repos that could not be added are in the result's Failed.
func (a *App) AddRepos(ctx context.Context, ref string, repos []RepoArg, o AddOpts) (*AddResult, error) {
	if err := outsideSandbox("add a repo"); err != nil {
		return nil, err
	}
	if len(repos) == 0 {
		return nil, errors.New("no repo to add")
	}
	type want struct {
		name string
		mode model.Mode
	}
	var wants []want
	for _, r := range repos {
		name, mode, err := validRepoArg(r.Name, r.Mode)
		if err != nil {
			return nil, err
		}
		wants = append(wants, want{name, mode})
	}
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return nil, err
	}
	unlock, err := a.lockTask(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// A launch under way has read the task's repos: it ends first, and
	// none starts until the repo is in (see lockLaunches).
	unlockLaunches, err := a.lockLaunches(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	defer unlockLaunches()
	if t, err = a.Store.GetTask(ctx, t.ID); err != nil {
		return nil, err
	}
	if !t.Status.Open() {
		return nil, fmt.Errorf("task %s is %s; repos can only be added to open tasks", t.Slug, t.Status)
	}

	res := &AddResult{Task: t}
	var ok []want
	seen := map[string]bool{}
	for _, w := range wants {
		err := func() error {
			if seen[w.name] {
				return errors.New("given twice")
			}
			seen[w.name] = true
			if _, err := a.checkAdd(ctx, t, w.name, w.mode); err != nil {
				return err
			}
			if w.mode == model.ModeRW && !o.NoFetch {
				fmt.Fprintf(a.out(), "fetching %s...\n", w.name)
				if err := a.fetchOrigin(ctx, w.name, a.Paths.RepoPath(w.name)); err != nil {
					return fmt.Errorf("repo %s: %w", w.name, err)
				}
			}
			if w.mode == model.ModeRW {
				if _, _, err := resolveBase(ctx, w.name, a.Paths.RepoPath(w.name), t.Base); err != nil {
					return err
				}
				if err := checkBranchFree(ctx, w.name, a.Paths.RepoPath(w.name), t.Branch); err != nil {
					return err
				}
			}
			return nil
		}()
		if err != nil {
			res.Failed = append(res.Failed, fmt.Errorf("%s: %w", w.name, err))
			continue
		}
		ok = append(ok, w)
	}
	if len(ok) == 0 {
		return res, nil
	}
	var args []RepoArg
	for _, w := range ok {
		args = append(args, RepoArg{Name: w.name, Mode: w.mode})
	}
	need, err := a.addNeedsRestart(ctx, t, args)
	if err != nil {
		return nil, err
	}
	var stopped []int64
	if need {
		if !o.Force {
			running, err := a.taskActivity(ctx, t)
			if err != nil {
				// Not knowing is not idle: only a forced restart goes on.
				running = []string{"cannot tell what runs in it: " + cleanLine(err.Error())}
			}
			if len(running) > 0 {
				return nil, &BusyError{Slug: t.Slug, Running: running}
			}
		}
		if stopped, err = a.stopSessions(ctx, t); err != nil {
			return nil, err
		}
		// The sessions are down: nothing may cut the rest short.
		ctx = context.WithoutCancel(ctx)
	}
	for _, w := range ok {
		nt, err := a.addRepoLocked(ctx, t.ID, w.name, w.mode, true) // fetched above
		if err != nil {
			res.Failed = append(res.Failed, fmt.Errorf("%s: %w", w.name, err))
			continue
		}
		res.Task = nt
		res.Added = append(res.Added, RepoArg{Name: w.name, Mode: w.mode})
	}
	if len(stopped) > 0 {
		n, err := a.resumeSessions(ctx, t, stopped)
		res.Restarted = n
		if err != nil {
			return res, fmt.Errorf("task %s: resume its sessions (r resumes them, or atl resume): %w", t.Slug, err)
		}
		fmt.Fprintf(a.out(), "restarted %d session(s) of %s\n", n, t.Slug)
	}
	return res, nil
}

// AddCheck is what an AddRepos of repos on task ref without force would
// find running in the task (see taskActivity), changing nothing: empty
// when it would go on (nothing runs, or no restart is needed). Not knowing
// what runs is a line too.
func (a *App) AddCheck(ctx context.Context, ref string, repos []RepoArg) ([]string, error) {
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return nil, err
	}
	need, err := a.addNeedsRestart(ctx, t, repos)
	if err != nil || !need {
		return nil, err
	}
	running, err := a.taskActivity(ctx, t)
	if err != nil {
		running = []string{"cannot tell what runs in it: " + cleanLine(err.Error())}
	}
	return running, nil
}

// addNeedsRestart reports whether adding repos to t needs its sessions
// restarted: it has live sessions (a live window) and they run in the
// task's container (the sandbox is on, or such a container exists: the
// sandbox was switched off meanwhile), or one of the repos is ro.
func (a *App) addNeedsRestart(ctx context.Context, t *model.Task, repos []RepoArg) (bool, error) {
	live, err := a.hasLiveWindow(ctx, t, tmux.SessionName(t.Slug))
	if err != nil {
		return false, fmt.Errorf("task %s: find its running sessions: %w", t.Slug, err)
	}
	if !live {
		return false, nil
	}
	for _, r := range repos {
		if r.Mode == model.ModeRO {
			return true, nil
		}
	}
	if a.Sandbox != nil {
		return true, nil
	}
	c, err := a.agentContainer(ctx, t)
	return err == nil && c != nil, nil
}

// agentContainer returns the task's container, nil if there is none. With
// the sandbox off a daemon that cannot be reached means none.
func (a *App) agentContainer(ctx context.Context, t *model.Task) (*sandbox.AgentContainer, error) {
	c, err := sandbox.Agent(ctx, a.docker(), a.sandboxID(t))
	switch {
	case err == nil:
		return &c, nil
	case sandbox.IsNoSuch(err):
		return nil, nil
	case a.Sandbox == nil && !errors.Is(err, sandbox.ErrNotAgent):
		return nil, nil
	}
	return nil, err
}

// liveSessions returns the sessions of t that have a live tagged window.
func (a *App) liveSessions(ctx context.Context, t *model.Task) ([]model.Session, error) {
	tags, err := a.Tmux.TaggedWindows(ctx)
	if err != nil {
		return nil, err
	}
	sessions, err := a.Store.ListSessions(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	var live []model.Session
	for _, s := range sessions {
		if _, ok := tags[strconv.FormatInt(s.ID, 10)]; ok {
			live = append(live, s)
		}
	}
	return live, nil
}

// taskActivity says, one line each, what runs in t that a restart of its
// sessions would end or lose: a live session in the middle of a turn
// (Claude works or waits for a permission answer), an idle one whose
// Claude still has background work in flight (a background subagent,
// shell, monitor, ...: as its last Stop hook reported it), one that has not
// reported its Claude id yet (it could not be resumed), and what runs in
// the task's container besides idle sessions (sandbox.ParseActivity:
// commands of Claude's Bash tool, in the foreground or the background,
// processes they left running, shells of a docker exec). A live session
// whose Claude is not found in the container is a line too: then what it
// runs cannot be told. Empty: nothing.
//
// Not seen: with the sandbox off, what an idle session left running on
// this machine (its background shells are, from its Stop hook); and the
// background work of a Claude Code too old to report it.
func (a *App) taskActivity(ctx context.Context, t *model.Task) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	live, err := a.liveSessions(ctx, t)
	if err != nil {
		return nil, err
	}
	var out []string
	names := map[int64]string{}
	for _, s := range live {
		name := sessionName(s)
		names[s.ID] = name
		switch {
		case s.State == model.StateRunning:
			out = append(out, "session "+name+": Claude is working")
		case s.State == model.StateWaiting:
			out = append(out, "session "+name+": Claude waits for a permission answer")
		case s.ClaudeSessionID == "":
			out = append(out, "session "+name+": has not reported its Claude session yet, so it could not be resumed")
		}
		if s.State == model.StateIdle {
			for _, b := range strings.Split(s.Background, "\n") {
				if b == "" {
					continue
				}
				out = append(out, "session "+name+" works in the background: "+firstChars(cleanLine(b), 80))
			}
		}
	}
	c, err := a.agentContainer(ctx, t)
	if err != nil {
		return nil, err
	}
	if c == nil || !c.Running {
		return out, nil
	}
	ps, err := a.docker()(ctx, sandbox.ActivityArgs(c.ID)...)
	if err != nil {
		if noContainer(err) {
			return out, nil
		}
		return nil, fmt.Errorf("list the processes of its container: %w", err)
	}
	procs, roots := sandbox.ParseActivity(ps)
	if a.Sandbox != nil {
		for _, s := range live {
			if !roots[s.ID] {
				out = append(out, "session "+names[s.ID]+": its Claude is not found in the container, so what it runs cannot be told")
			}
		}
	}
	for _, p := range procs {
		cmd := firstChars(cleanLine(p.Command), 80)
		switch name, ok := names[p.Session]; {
		case ok:
			out = append(out, "session "+name+" runs "+cmd)
		case p.Session != 0:
			out = append(out, fmt.Sprintf("session %d (closed) left %s", p.Session, cmd))
		default:
			out = append(out, fmt.Sprintf("process %d: %s", p.PID, cmd))
		}
	}
	return out, nil
}

// stopSessions closes the live sessions of t for a restart and returns
// their ids: the task's container goes first, if there is one (docker rm
// -f: what runs in it ends at once, and no Claude in there gets to report
// its end, which would make the session ended rather than stopped); then
// the sessions' windows are killed and the sessions marked stopped. A
// container that has the task container's name but is not atl's own
// (sandbox.Agent) stops it all, before anything is closed. On a failure
// what was closed is resumed and the error says so.
func (a *App) stopSessions(ctx context.Context, t *model.Task) ([]int64, error) {
	live, err := a.liveSessions(ctx, t)
	if err != nil {
		return nil, fmt.Errorf("task %s: find its running sessions: %w", t.Slug, err)
	}
	c, err := a.agentContainer(ctx, t)
	if err == nil && c != nil {
		if _, err = a.docker()(ctx, "rm", "-f", c.ID); sandbox.IsNoSuch(err) {
			err = nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("task %s: remove its container: %w", t.Slug, err)
	}
	bg := context.WithoutCancel(ctx)
	var ids []int64
	var errs []error
	for _, s := range live {
		ids = append(ids, s.ID)
		if _, err := a.Tmux.KillTaggedWindows(bg, strconv.FormatInt(s.ID, 10), ""); err != nil {
			errs = append(errs, fmt.Errorf("close the window of session %d: %w", s.ID, err))
		}
		if err := a.Store.SetSessionState(bg, s.ID, model.StateStopped, a.now()); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		// Whatever was closed comes back: the caller adds nothing.
		if _, rerr := a.resumeSessions(bg, t, ids); rerr != nil {
			errs = append(errs, rerr)
		}
		return nil, fmt.Errorf("task %s: close its sessions: %w", t.Slug, errors.Join(errs...))
	}
	return ids, nil
}

// resumeSessions resumes (claude --resume) the sessions ids of t that
// stopSessions closed, as Resume does. Each is claimed first, so nothing
// else resumes it too, from the state it has now: stopped; ended, if its
// Claude reported its end meanwhile; or a live state a hook wrote after
// stopSessions' stopped (it is set back to stopped first). A session that
// has a live window again is left alone (resumeClaimed never resumes over
// one). Sessions without a Claude id cannot be resumed: they are marked
// ended, with a warning. It returns how many it resumed.
func (a *App) resumeSessions(ctx context.Context, t *model.Task, ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	nt, err := a.Store.GetTask(ctx, t.ID)
	if err != nil {
		return 0, err
	}
	t = nt
	dir, err := a.checkLaunchable(t)
	if err != nil {
		return 0, err
	}
	tags, err := a.Tmux.TaggedWindows(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, id := range ids {
		if _, ok := tags[strconv.FormatInt(id, 10)]; ok {
			continue // running again: someone resumed it
		}
		s, err := a.Store.GetSession(ctx, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("session %d: %w", id, err))
			continue
		}
		from := s.State
		if from.Live() {
			if _, err := a.Store.SetStateIf(ctx, id, from, model.StateStopped, a.now()); err != nil {
				errs = append(errs, fmt.Errorf("session %d: %w", id, err))
				continue
			}
			from = model.StateStopped
		}
		if s.ClaudeSessionID == "" {
			if err := a.Store.SetSessionState(ctx, id, model.StateEnded, a.now()); err != nil {
				errs = append(errs, err)
			}
			a.warnf("session %s of %s had not reported its Claude session: ended, not resumed", sessionName(*s), t.Slug)
			continue
		}
		claimed, err := a.Store.ClaimFrom(ctx, id, from, a.now())
		if err != nil {
			errs = append(errs, fmt.Errorf("claim session %d: %w", id, err))
			continue
		}
		if !claimed {
			continue
		}
		if _, err := a.resumeClaimed(ctx, t, id, dir, from); err != nil {
			errs = append(errs, fmt.Errorf("resume session %d: %w", id, err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}
