package tui

import (
	"errors"
	"strings"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/andreujuanc/atelier/internal/app"
	"github.com/andreujuanc/atelier/internal/model"
)

// Form fields, in order. The backlog checkbox is last: enter on it submits.
const (
	fieldTitle = iota
	fieldProject
	fieldRepos
	fieldBase
	fieldDesc
	fieldBacklog
	numFields
)

var fieldLabels = [numFields]string{"Title", "Project", "Repos", "Base", "Description", "Backlog"}

// baseHint explains the Base field's auto choice.
const baseHint = "auto = origin/dev → develop → default branch; production patch = origin's default branch"

// newForm is the modal "new task" form. Repos are chosen in the repo
// picker (enter or space on the Repos field), so inputs[fieldRepos] is
// unused; Base is a two-state toggle, so inputs[fieldBase] is unused too.
type newForm struct {
	inputs  [fieldBacklog]textinput.Model
	backlog bool
	// prodBase: branch from origin's default branch (production patch)
	// instead of auto (origin/dev → develop → default branch).
	prodBase bool
	focus    int
	err      string
	projects []string
	stem     string // what the user typed in project before tab cycling
	cycling  bool

	repos []app.RepoArg // the picker's selection
	// preProject is the project whose repos were preselected (and are
	// recorded in fromProject); loadingProject is one being loaded.
	preProject     string
	loadingProject string
	fromProject    map[string]bool
	// projErr is why project projErrFor's repos could not be loaded.
	projErr, projErrFor string
}

func newTaskForm(projects []string) *newForm {
	f := &newForm{projects: projects}
	placeholders := [fieldBacklog]string{"required", "optional (tab cycles)", "", "", "optional"}
	for i := range f.inputs {
		ti := textinput.New()
		ti.Prompt = ""
		ti.Placeholder = placeholders[i]
		ti.Cursor.SetMode(cursor.CursorStatic)
		ti.CharLimit = 500
		f.inputs[i] = ti
	}
	f.setFocus(fieldTitle)
	return f
}

func (f *newForm) setFocus(i int) {
	if i < 0 {
		i = 0
	}
	if i >= numFields {
		i = numFields - 1
	}
	f.focus = i
	for j := range f.inputs {
		if j == i {
			f.inputs[j].Focus()
		} else {
			f.inputs[j].Blur()
		}
	}
}

func (f *newForm) setWidth(w int) {
	if w < 1 {
		w = 1
	}
	for i := range f.inputs {
		f.inputs[i].Width = w
	}
}

// cycleProject replaces the project field with the next (dir > 0) or
// previous project whose name starts with what the user typed.
func (f *newForm) cycleProject(dir int) {
	in := &f.inputs[fieldProject]
	if !f.cycling {
		f.stem = strings.TrimSpace(in.Value())
	}
	var cands []string
	for _, p := range f.projects {
		if strings.HasPrefix(strings.ToLower(p), strings.ToLower(f.stem)) {
			cands = append(cands, p)
		}
	}
	if len(cands) == 0 {
		cands = f.projects
	}
	if len(cands) == 0 {
		return
	}
	cur := -1
	for i, c := range cands {
		if c == in.Value() {
			cur = i
		}
	}
	next := 0
	switch {
	case cur >= 0:
		next = (cur + dir + len(cands)) % len(cands)
	case dir < 0:
		next = len(cands) - 1
	}
	in.SetValue(cands[next])
	in.CursorEnd()
	f.cycling = true
}

func (f *newForm) project() string { return strings.TrimSpace(f.inputs[fieldProject].Value()) }

// applyProject preselects project name's repos: repos preselected from the
// previous project are dropped, then name's repos come first (in project
// order, with the project's modes) followed by the rest of the selection.
func (f *newForm) applyProject(name string, repos []app.RepoArg) {
	from := map[string]bool{}
	for _, r := range repos {
		from[r.Name] = true
	}
	f.repos = mergeProject(f.repos, f.fromProject, repos)
	f.fromProject, f.preProject = from, name
	f.projErr, f.projErrFor = "", ""
}

// mergeProject returns repos followed by the repos of sel that are neither
// in repos nor in oldFrom (the previous project's preselection).
func mergeProject(sel []app.RepoArg, oldFrom map[string]bool, repos []app.RepoArg) []app.RepoArg {
	from := map[string]bool{}
	var out []app.RepoArg
	for _, r := range repos {
		from[r.Name] = true
		out = append(out, r)
	}
	for _, r := range sel {
		if !from[r.Name] && !oldFrom[r.Name] {
			out = append(out, r)
		}
	}
	return out
}

// opts validates the form and builds the NewTask options. The picker's
// selection is the exact repo list once the project's repos have been
// preselected into it; with no selection a project's own repos are used.
func (f *newForm) opts() (app.NewTaskOpts, error) {
	o := app.NewTaskOpts{
		Title:       strings.TrimSpace(f.inputs[fieldTitle].Value()),
		Project:     f.project(),
		Description: strings.TrimSpace(f.inputs[fieldDesc].Value()),
		Backlog:     f.backlog,
		Base:        model.BaseAuto,
	}
	if f.prodBase {
		o.Base = model.BaseDefault
	}
	if o.Title == "" {
		return o, errors.New("title is required")
	}
	preselected := o.Project != "" && o.Project == f.preProject
	if len(f.repos) == 0 {
		if o.Project == "" {
			return o, errors.New("pick at least one repo (enter on Repos) or choose a project")
		}
		if preselected {
			return o, errors.New("no repos selected (enter on Repos to pick some)")
		}
		return o, nil
	}
	o.Repos = append([]app.RepoArg(nil), f.repos...)
	o.ExactRepos = preselected
	return o, nil
}

// formResult is what a key did to the form.
type formResult int

const (
	formContinue formResult = iota
	formCancel
	formSubmit
	formPick // open the repo picker
)

// update handles a key in the form. On formSubmit the caller reads opts().
func (f *newForm) update(msg tea.KeyMsg) (formResult, tea.Cmd) {
	key := msg.String()
	if key != "tab" && key != "shift+tab" {
		f.cycling = false
	}
	switch key {
	case "esc":
		return formCancel, nil
	case "ctrl+b":
		f.backlog = !f.backlog
		return formContinue, nil
	case "tab", "shift+tab":
		dir := 1
		if key == "shift+tab" {
			dir = -1
		}
		if f.focus == fieldProject && len(f.projects) > 0 {
			f.cycleProject(dir)
		} else {
			f.setFocus((f.focus + dir + numFields) % numFields)
		}
		return formContinue, nil
	case "up":
		f.setFocus(f.focus - 1)
		return formContinue, nil
	case "down":
		f.setFocus(f.focus + 1)
		return formContinue, nil
	case "enter":
		if f.focus == fieldBacklog {
			if _, err := f.opts(); err != nil {
				f.err = err.Error()
				return formContinue, nil
			}
			return formSubmit, nil
		}
		if f.focus == fieldRepos {
			return formPick, nil
		}
		f.setFocus(f.focus + 1)
		return formContinue, nil
	}
	if f.focus == fieldRepos {
		if key == " " {
			return formPick, nil
		}
		return formContinue, nil
	}
	if f.focus == fieldBase {
		switch key {
		case " ", "left", "right":
			f.prodBase = !f.prodBase
		}
		return formContinue, nil
	}
	if f.focus == fieldBacklog {
		if key == " " || key == "x" {
			f.backlog = !f.backlog
		}
		return formContinue, nil
	}
	f.err = ""
	var cmd tea.Cmd
	f.inputs[f.focus], cmd = f.inputs[f.focus].Update(msg)
	return formContinue, cmd
}
