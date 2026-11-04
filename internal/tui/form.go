package tui

import (
	"errors"
	"strings"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/andreujuanc/atelier/internal/app"
)

// Form fields, in order. The backlog checkbox is last: enter on it submits.
const (
	fieldTitle = iota
	fieldProject
	fieldRepos
	fieldDesc
	fieldBacklog
	numFields
)

var fieldLabels = [numFields]string{"Title", "Project", "Repos", "Description", "Backlog"}

// newForm is the modal "new task" form.
type newForm struct {
	inputs   [fieldBacklog]textinput.Model
	backlog  bool
	focus    int
	err      string
	projects []string
	stem     string // what the user typed in project before tab cycling
	cycling  bool
}

func newTaskForm(projects []string) *newForm {
	f := &newForm{projects: projects}
	placeholders := [fieldBacklog]string{"required", "optional (tab cycles)", "name:rw name2:ro (optional)", "optional"}
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

// opts validates the form and builds the NewTask options.
func (f *newForm) opts() (app.NewTaskOpts, error) {
	o := app.NewTaskOpts{
		Title:       strings.TrimSpace(f.inputs[fieldTitle].Value()),
		Project:     strings.TrimSpace(f.inputs[fieldProject].Value()),
		Description: strings.TrimSpace(f.inputs[fieldDesc].Value()),
		Backlog:     f.backlog,
	}
	if o.Title == "" {
		return o, errors.New("title is required")
	}
	repos, err := parseRepos(f.inputs[fieldRepos].Value())
	if err != nil {
		return o, err
	}
	o.Repos = repos
	return o, nil
}

// formResult is what a key did to the form.
type formResult int

const (
	formContinue formResult = iota
	formCancel
	formSubmit
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
		f.setFocus(f.focus + 1)
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
