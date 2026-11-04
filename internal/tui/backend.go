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
	Open(ctx context.Context, ref string) (*exec.Cmd, error)
	NewSession(ctx context.Context, ref string) (*model.Session, error)
	Resume(ctx context.Context, ref string) (int, error)
	SetStatus(ctx context.Context, ref string, st model.Status) error
	Done(ctx context.Context, ref string, force bool) (*model.Task, error)
	Cancel(ctx context.Context, ref string, force bool) (*model.Task, error)
	PR(ctx context.Context, ref string) ([]app.PRResult, error)
	Recover(ctx context.Context) (int, error)
	ProjectNames() ([]string, error)
	TaskDir(slug string) string
}

// appBackend adapts *app.App to Backend.
type appBackend struct{ *app.App }

var _ Backend = appBackend{}

// NewBackend returns a Backend backed by a.
func NewBackend(a *app.App) Backend { return appBackend{a} }

func (b appBackend) List(ctx context.Context) ([]app.TaskView, error) { return b.App.List(ctx) }

func (b appBackend) ProjectNames() ([]string, error) { return project.List(b.Paths) }

func (b appBackend) TaskDir(slug string) string { return b.Paths.TaskDir(slug) }
