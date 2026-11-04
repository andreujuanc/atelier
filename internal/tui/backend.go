// Package tui implements atelier's terminal UI (`atl` with no arguments)
// with Bubble Tea. It talks to the service layer only through Backend.
package tui

import (
	"context"
	"os/exec"

	"github.com/andreujuanc/atelier/internal/app"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/project"
)

// Backend is everything the UI needs from the service layer.
type Backend interface {
	List(ctx context.Context) ([]app.TaskView, error)
	Get(ctx context.Context, ref string) (*app.TaskView, error)
	NewTask(ctx context.Context, o app.NewTaskOpts) (*model.Task, error)
	// OpenOrChoose returns either the command that attaches to the task
	// (a new session was started: nothing to choose from) or the sessions
	// to choose from (live first, then resumable past ones).
	OpenOrChoose(ctx context.Context, ref string) (*exec.Cmd, []app.SessionChoice, error)
	// OpenSession attaches to a live session or resumes a past one, with
	// its window selected; OpenNew starts a new session and attaches.
	OpenSession(ctx context.Context, ref string, sessionID int64) (*exec.Cmd, error)
	OpenNew(ctx context.Context, ref string) (*exec.Cmd, error)
	NewSession(ctx context.Context, ref string) (*model.Session, error)
	Resume(ctx context.Context, ref string) (int, error)
	SetStatus(ctx context.Context, ref string, st model.Status) error
	Done(ctx context.Context, ref string, force bool) (*model.Task, error)
	Cancel(ctx context.Context, ref string, force bool) (*model.Task, error)
	PR(ctx context.Context, ref string) ([]app.PRResult, error)
	Recover(ctx context.Context) (int, error)
	// DirtyTasks maps open task ids to true when any of their rw worktrees
	// has uncommitted changes.
	DirtyTasks(ctx context.Context) (map[int64]bool, error)
	ProjectNames() ([]string, error)
	// ProjectRepos returns project name's repos with their default modes.
	ProjectRepos(name string) ([]app.RepoArg, error)
	AvailableRepos(ctx context.Context) ([]string, error)
	AddRepo(ctx context.Context, ref string, r app.RepoArg, noFetch bool) (*model.Task, error)
	AddNote(ctx context.Context, ref, body, author string) (*model.Note, error)
	TaskDir(slug string) string
	// Assistant ensures the atelier assistant runs (told about task slug,
	// if non-empty) and returns the command that attaches to it and a hint
	// for the user ("" if none), e.g. how to paste the task sentence into
	// an assistant that was already running.
	Assistant(ctx context.Context, slug string) (*exec.Cmd, string, error)
}

// appBackend adapts *app.App to Backend.
type appBackend struct{ *app.App }

var _ Backend = appBackend{}

// NewBackend returns a Backend backed by a.
func NewBackend(a *app.App) Backend { return appBackend{a} }

func (b appBackend) List(ctx context.Context) ([]app.TaskView, error) { return b.App.List(ctx) }

func (b appBackend) ProjectNames() ([]string, error) { return project.List(b.Paths) }

func (b appBackend) TaskDir(slug string) string { return b.Paths.TaskDir(slug) }

func (b appBackend) ProjectRepos(name string) ([]app.RepoArg, error) {
	pr, err := project.Load(b.Paths, name)
	if err != nil {
		return nil, err
	}
	out := make([]app.RepoArg, 0, len(pr.Repos))
	for _, r := range pr.Repos {
		out = append(out, app.RepoArg{Name: r.Name, Mode: r.Mode})
	}
	return out, nil
}
