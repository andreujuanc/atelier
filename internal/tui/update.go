package tui

import (
	"context"
	"errors"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/andreujuanc/atelier/internal/update"
	"github.com/andreujuanc/atelier/internal/version"
)

// updateEvery is how often the TUI asks its Updater for a newer release
// (the Updater caches GitHub's answer for hours: update.CheckEvery).
const updateEvery = time.Hour

// ErrRestart is returned by RunContext when atl was updated from the TUI
// (U) and the TUI quit to start again on the new binary, every operation
// having ended.
var ErrRestart = errors.New("restart atl")

// Updater finds and installs newer atl releases (internal/update).
type Updater interface {
	// Available returns the version of a release newer than the running
	// atl, or "" when there is none.
	Available(ctx context.Context) (string, error)
	// Install replaces atl with the latest release and returns its version.
	Install(ctx context.Context) (string, error)
}

type (
	updateTickMsg  struct{}
	updateCheckMsg struct {
		version string
		err     error
	}
	updateDoneMsg struct {
		version string
		err     error
	}
)

// RestartRequested reports whether the TUI quit to restart on an updated
// atl. A forced quit is never a restart.
func (m *Model) RestartRequested() bool { return m.restarting && !m.forceQuit }

// updateCheckCmd asks the Updater whether a newer release exists.
func (m *Model) updateCheckCmd() tea.Cmd {
	if m.updater == nil {
		return nil
	}
	up, ctx := m.updater, m.ctx
	return func() tea.Msg {
		v, err := up.Available(ctx)
		return updateCheckMsg{version: v, err: err}
	}
}

// updateChecked keeps what the check found and arms the next one. A
// failed check (offline, GitHub's rate limit) says nothing: the bar keeps
// what an earlier check showed.
func (m *Model) updateChecked(msg updateCheckMsg) tea.Cmd {
	if msg.err == nil && !m.updating && m.updated == "" {
		m.available = msg.version
	}
	return m.after(updateEvery, updateTickMsg{})
}

// startUpdate is U: install the newer release the top bar offers.
func (m *Model) startUpdate() tea.Cmd {
	switch {
	case m.updater == nil:
		m.setStatus("this atl does not check for updates (a local build, or $ATL_UPDATE_CHECK=off): run atl update")
		return nil
	case m.updating:
		m.setStatus("busy: updating atl…")
		return nil
	case m.updated != "":
		m.setOK("atl updated to " + m.updated + ": restarting once nothing runs")
		return nil
	case m.available == "":
		m.setStatus("atl is up to date")
		return nil
	}
	m.updating = true
	up, ctx := m.updater, m.ctx
	return func() tea.Msg {
		v, err := up.Install(ctx)
		return updateDoneMsg{version: v, err: err}
	}
}

// updateDone ends an install: the TUI restarts on the new binary once it
// is back in normal mode with nothing running (Update).
func (m *Model) updateDone(msg updateDoneMsg) tea.Cmd {
	m.updating = false
	if msg.err != nil {
		m.setStatusErr("update: "+firstLine(msg.err.Error()), true)
		return nil
	}
	m.available, m.updated = "", msg.version
	m.setOK("atl updated to " + msg.version + ": restarting once nothing runs")
	return nil
}

// restartDue reports whether the TUI should quit now to restart on the
// updated binary: never over a form, dialog or editor, or while anything
// runs.
func (m *Model) restartDue() bool {
	return m.updated != "" && !m.restarting && m.mode == modeNormal && !m.quitRequested && m.opsRunning() == 0
}

// releases is the Updater over GitHub's releases (internal/update).
type releases struct{ u *update.Updater }

// NewUpdater returns the Updater RunContext uses, or nil when the TUI
// does not check for updates (update.CheckEnabled).
func NewUpdater() Updater {
	if !update.CheckEnabled() {
		return nil
	}
	return releases{update.New()}
}

func (r releases) Available(ctx context.Context) (string, error) {
	latest, err := r.u.Check(ctx, update.CheckEvery)
	if err != nil || !version.Newer(version.Current(), latest) {
		return "", err
	}
	return latest, nil
}

// Install installs the latest release, asked for again: the one the bar
// offered may have been superseded since.
func (r releases) Install(ctx context.Context) (string, error) {
	rel, err := r.u.Latest(ctx)
	if err != nil {
		return "", err
	}
	if !version.Newer(version.Current(), rel.Tag) {
		return "", errors.New("atl " + version.Current() + " is already the latest release")
	}
	target, err := update.Target()
	if err != nil {
		return "", err
	}
	if err := r.u.Install(ctx, rel, target); err != nil {
		return "", err
	}
	return rel.Tag, nil
}
