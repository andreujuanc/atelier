// Package tui implements atelier's terminal UI (`atl` with no arguments)
// with Bubble Tea. It talks to the service layer only through Backend.
package tui

import (
	"context"
	"os/exec"

	"github.com/andreujuanc/atelier/internal/app"
	"github.com/andreujuanc/atelier/internal/learn"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/project"
	"github.com/andreujuanc/atelier/internal/store"
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
	// AddRepos adds repos to the task, restarting its sessions if they would
	// not see them (app.AddRepos); *app.BusyError while things run there.
	AddRepos(ctx context.Context, ref string, repos []app.RepoArg, o app.AddOpts) (*app.AddResult, error)
	// AddCheck is what AddRepos without force would find running there
	// (app.AddCheck); it changes nothing.
	AddCheck(ctx context.Context, ref string, repos []app.RepoArg) ([]string, error)
	// GetNotes returns the task's notes text.
	GetNotes(ctx context.Context, ref string) (string, error)
	// SetNotes replaces the task's notes with text if they are still base
	// (compare-and-set), returning the text as stored; else the error is a
	// *model.NotesConflictError holding the current text.
	SetNotes(ctx context.Context, ref, text, base string) (string, error)
	// Rename changes the task's title only (slug, branch and dirs stay).
	Rename(ctx context.Context, ref, title string) (*model.Task, error)
	TaskDir(slug string) string
	// Assistant ensures the atelier assistant runs (told about task slug,
	// if non-empty) and returns the command that attaches to it and a hint
	// for the user ("" if none), e.g. how to paste the task sentence into
	// an assistant that was already running.
	Assistant(ctx context.Context, slug string) (*exec.Cmd, string, error)

	// Learnings returns every learning scored now (app.Learnings).
	Learnings(ctx context.Context) ([]app.LearningView, error)
	// Learning returns learning id scored now, with its occurrences
	// (oldest first).
	Learning(ctx context.Context, id int64) (app.LearningView, []store.LearningOccurrence, error)
	// ProposedLearnings counts the learnings waiting for review (the top
	// bar's hint).
	ProposedLearnings(ctx context.Context) (int, error)
	// DecideLearning accepts or rejects learning id; EditLearning rewords
	// it (the text checked by learn.CheckLearningText); DeleteLearning
	// forgets it and its occurrences. All refuse with
	// app.ErrLearnRunning while atl learn runs.
	DecideLearning(ctx context.Context, id int64, accept bool) error
	EditLearning(ctx context.Context, id int64, text string) (string, error)
	DeleteLearning(ctx context.Context, id int64) (int, error)
	// LearnConfig is the workspace's [learn] settings (the models atl
	// learn calls); it only reads what is loaded.
	LearnConfig() learn.Config
}

// appBackend adapts *app.App to Backend.
type appBackend struct{ *app.App }

var _ Backend = appBackend{}

// NewBackend returns a Backend backed by a.
func NewBackend(a *app.App) Backend { return appBackend{a} }

func (b appBackend) List(ctx context.Context) ([]app.TaskView, error) { return b.App.List(ctx) }

func (b appBackend) ProjectNames() ([]string, error) { return project.List(b.Paths) }

func (b appBackend) TaskDir(slug string) string { return b.Paths.TaskDir(slug) }

func (b appBackend) GetNotes(ctx context.Context, ref string) (string, error) {
	_, text, err := b.App.Notes(ctx, ref)
	return text, err
}

func (b appBackend) ProposedLearnings(ctx context.Context) (int, error) {
	return b.Store.CountLearnings(ctx, learn.StatusProposed)
}

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
