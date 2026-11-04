package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/andreujuanc/atelier/internal/app"
	"github.com/andreujuanc/atelier/internal/model"
)

// Compile-time check that the adapter satisfies Backend over *app.App.
var _ Backend = appBackend{App: (*app.App)(nil)}

type call struct {
	name string
	args []any
}

type fakeBackend struct {
	tasks    []app.TaskView
	calls    []call
	doneErr  error // returned by Done/Cancel when force is false
	projects []string
	newTask  *model.Task
	// repos is AvailableRepos' result; projRepos ProjectRepos' per project.
	repos      []string
	projRepos  map[string][]app.RepoArg
	addErr     map[string]error       // AddRepo error per repo name
	addOut     func(r app.RepoArg)    // called by AddRepo (to write App.Out)
	notes      map[string]*model.Note // latest note per slug (Get's LatestNote)
	noteErr    error
	assistErr  error
	assistHint string
	dirty      map[int64]bool // DirtyTasks' result
	dirtyErr   error
}

func (f *fakeBackend) rec(name string, args ...any) { f.calls = append(f.calls, call{name, args}) }

func (f *fakeBackend) count(name string) int {
	n := 0
	for _, c := range f.calls {
		if c.name == name {
			n++
		}
	}
	return n
}

func (f *fakeBackend) last(name string) *call {
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].name == name {
			return &f.calls[i]
		}
	}
	return nil
}

func (f *fakeBackend) List(ctx context.Context) ([]app.TaskView, error) {
	f.rec("List")
	out := make([]app.TaskView, len(f.tasks))
	copy(out, f.tasks)
	return out, nil
}

func (f *fakeBackend) Get(ctx context.Context, ref string) (*app.TaskView, error) {
	f.rec("Get", ref)
	for _, t := range f.tasks {
		if t.Task.Slug == ref {
			v := t
			for _, r := range t.Task.Repos {
				rs := app.RepoStatus{Name: r.Name, Mode: r.Mode, BaseBranch: r.BaseBranch, Dirty: r.Name == "dirtyrepo"}
				if r.Name == "gonerepo" {
					rs.Err = "missing"
				}
				v.RepoStatus = append(v.RepoStatus, rs)
			}
			v.LatestNote = f.notes[ref]
			return &v, nil
		}
	}
	return nil, fmt.Errorf("task %s not found", ref)
}

func (f *fakeBackend) NewTask(ctx context.Context, o app.NewTaskOpts) (*model.Task, error) {
	f.rec("NewTask", o)
	if f.newTask != nil {
		return f.newTask, nil
	}
	return &model.Task{ID: 999, Slug: model.Slugify(o.Title), Title: o.Title}, nil
}

func (f *fakeBackend) Open(ctx context.Context, ref string) (*exec.Cmd, error) {
	f.rec("Open", ref)
	return exec.Command("true"), nil
}

func (f *fakeBackend) Assistant(ctx context.Context, slug string) (*exec.Cmd, string, error) {
	f.rec("Assistant", slug)
	if f.assistErr != nil {
		return nil, "", f.assistErr
	}
	return exec.Command("true"), f.assistHint, nil
}

func (f *fakeBackend) NewSession(ctx context.Context, ref string) (*model.Session, error) {
	f.rec("NewSession", ref)
	return &model.Session{ID: 1, Name: "claude-1"}, nil
}

func (f *fakeBackend) Resume(ctx context.Context, ref string) (int, error) {
	f.rec("Resume", ref)
	return 1, nil
}

func (f *fakeBackend) SetStatus(ctx context.Context, ref string, st model.Status) error {
	f.rec("SetStatus", ref, st)
	for i := range f.tasks {
		if f.tasks[i].Task.Slug == ref {
			f.tasks[i].Task.Status = st
		}
	}
	return nil
}

func (f *fakeBackend) Done(ctx context.Context, ref string, force bool) (*model.Task, error) {
	f.rec("Done", ref, force)
	if !force && f.doneErr != nil {
		return nil, f.doneErr
	}
	return &model.Task{Slug: ref, Status: model.StatusDone}, nil
}

func (f *fakeBackend) Cancel(ctx context.Context, ref string, force bool) (*model.Task, error) {
	f.rec("Cancel", ref, force)
	if !force && f.doneErr != nil {
		return nil, f.doneErr
	}
	return &model.Task{Slug: ref, Status: model.StatusCancelled}, nil
}

func (f *fakeBackend) PR(ctx context.Context, ref string) ([]app.PRResult, error) {
	f.rec("PR", ref)
	return []app.PRResult{{Repo: "api", URL: "https://example.com/pr/1"}, {Repo: "web", Skipped: true, Reason: "no commits"}}, nil
}

func (f *fakeBackend) Recover(ctx context.Context) (int, error) {
	f.rec("Recover")
	return 0, nil
}

func (f *fakeBackend) DirtyTasks(ctx context.Context) (map[int64]bool, error) {
	f.rec("DirtyTasks")
	if f.dirtyErr != nil {
		return nil, f.dirtyErr
	}
	out := map[int64]bool{}
	for id, d := range f.dirty {
		out[id] = d
	}
	return out, nil
}

func (f *fakeBackend) ProjectNames() ([]string, error) { return f.projects, nil }

func (f *fakeBackend) TaskDir(slug string) string { return "/tasks/" + slug }

func (f *fakeBackend) ProjectRepos(name string) ([]app.RepoArg, error) {
	f.rec("ProjectRepos", name)
	r, ok := f.projRepos[name]
	if !ok {
		return nil, fmt.Errorf("project %q not found", name)
	}
	return r, nil
}

func (f *fakeBackend) AvailableRepos(ctx context.Context) ([]string, error) {
	f.rec("AvailableRepos")
	return f.repos, nil
}

func (f *fakeBackend) AddRepo(ctx context.Context, ref string, r app.RepoArg, noFetch bool) (*model.Task, error) {
	f.rec("AddRepo", ref, r, noFetch)
	if f.addOut != nil {
		f.addOut(r)
	}
	if err := f.addErr[r.Name]; err != nil {
		return nil, err
	}
	for i := range f.tasks {
		if f.tasks[i].Task.Slug == ref {
			f.tasks[i].Task.Repos = append(f.tasks[i].Task.Repos, model.TaskRepo{Name: r.Name, Mode: r.Mode})
			t := f.tasks[i].Task
			return &t, nil
		}
	}
	return nil, fmt.Errorf("task %s not found", ref)
}

func (f *fakeBackend) AddNote(ctx context.Context, ref, body, author string) (*model.Note, error) {
	f.rec("AddNote", ref, body, author)
	if f.noteErr != nil {
		return nil, f.noteErr
	}
	if f.notes == nil {
		f.notes = map[string]*model.Note{}
	}
	n := &model.Note{ID: int64(len(f.calls)), Body: body, Author: author, CreatedAt: t0.Add(55 * time.Minute)}
	f.notes[ref] = n
	return n, nil
}

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func tv(id int64, slug string, st model.Status, sessions ...model.Session) app.TaskView {
	return app.TaskView{
		Task: model.Task{
			ID: id, Slug: slug, Title: "Title of " + slug, Status: st, Branch: "atl/" + slug,
			PortOffset: int(id) * 100, Repos: []model.TaskRepo{{Name: "api", Mode: model.ModeRW, BaseBranch: "dev"}, {Name: "docs", Mode: model.ModeRO}},
			UpdatedAt: t0.Add(time.Duration(id) * time.Minute),
		},
		Sessions: sessions,
	}
}

// newTest returns a model over fb with ticks disabled and the list loaded.
func newTest(t *testing.T, fb *fakeBackend) (*Model, *[]tea.Msg) {
	t.Helper()
	m := New(context.Background(), fb)
	m.now = func() time.Time { return t0.Add(time.Hour) }
	var scheduled []tea.Msg
	m.after = func(d time.Duration, msg tea.Msg) tea.Cmd {
		scheduled = append(scheduled, msg)
		return nil
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	drain(m, m.Init())
	return m, &scheduled
}

// drain runs cmd and feeds the resulting messages back into m until none
// are left. Exec messages (unexported) are dropped.
func drain(m *Model, cmd tea.Cmd) {
	queue := []tea.Cmd{cmd}
	for n := 0; len(queue) > 0 && n < 1000; n++ {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		msg := c()
		switch msg := msg.(type) {
		case nil:
		case tea.BatchMsg:
			queue = append(queue, msg...)
		case tea.QuitMsg:
		default:
			if strings.Contains(fmt.Sprintf("%T", msg), "execMsg") {
				continue
			}
			_, next := m.Update(msg)
			queue = append(queue, next)
		}
	}
}

func key(m *Model, k string) {
	_, cmd := m.Update(keyMsg(k))
	drain(m, cmd)
}

// press sends key k and returns the resulting command without running it.
func press(m *Model, k string) tea.Cmd {
	_, cmd := m.Update(keyMsg(k))
	return cmd
}

func keyMsg(k string) tea.KeyMsg {
	var msg tea.KeyMsg
	switch k {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		msg = tea.KeyMsg{Type: tea.KeyTab}
	case "up":
		msg = tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	case "ctrl+b":
		msg = tea.KeyMsg{Type: tea.KeyCtrlB}
	case "ctrl+c":
		msg = tea.KeyMsg{Type: tea.KeyCtrlC}
	case "ctrl+n":
		msg = tea.KeyMsg{Type: tea.KeyCtrlN}
	case "ctrl+p":
		msg = tea.KeyMsg{Type: tea.KeyCtrlP}
	case "ctrl+u":
		msg = tea.KeyMsg{Type: tea.KeyCtrlU}
	case "backspace":
		msg = tea.KeyMsg{Type: tea.KeyBackspace}
	case "pgdown":
		msg = tea.KeyMsg{Type: tea.KeyPgDown}
	case "pgup":
		msg = tea.KeyMsg{Type: tea.KeyPgUp}
	case " ":
		msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
	return msg
}

func typeText(m *Model, s string) {
	for _, r := range s {
		if r == ' ' {
			key(m, " ")
		} else {
			key(m, string(r))
		}
	}
}

func plainView(m *Model) string { return ansi.Strip(m.View()) }

func sampleTasks() []app.TaskView {
	return []app.TaskView{
		tv(1, "alpha", model.StatusActive),
		tv(2, "beta", model.StatusBacklog),
		tv(3, "gamma", model.StatusActive, model.Session{ID: 7, Name: "claude-7", State: model.StateWaiting, LastEventAt: t0.Add(58 * time.Minute)}),
		tv(4, "delta", model.StatusReview),
	}
}

func rowSlugs(m *Model) []string {
	var out []string
	for _, r := range m.rows {
		if r.header {
			out = append(out, "#"+string(r.status))
		} else {
			out = append(out, r.task.Task.Slug)
		}
	}
	return out
}

func TestGroupingAndClosedLimit(t *testing.T) {
	tasks := sampleTasks()
	for i := 0; i < 15; i++ {
		tasks = append(tasks, tv(int64(100+i), fmt.Sprintf("done-%02d", i), model.StatusDone))
		tasks = append(tasks, tv(int64(200+i), fmt.Sprintf("cxl-%02d", i), model.StatusCancelled))
	}
	rows := buildRows(tasks, "")
	var order []model.Status
	counts := map[model.Status]int{}
	for _, r := range rows {
		if r.header {
			order = append(order, r.status)
			continue
		}
		if r.task.Task.Status != r.status {
			t.Fatalf("task %s under %s", r.task.Task.Slug, r.status)
		}
		counts[r.status]++
	}
	if !reflect.DeepEqual(order, model.AllStatuses) {
		t.Fatalf("header order %v", order)
	}
	if counts[model.StatusDone] != 10 || counts[model.StatusCancelled] != 10 || counts[model.StatusActive] != 2 {
		t.Fatalf("counts %v", counts)
	}
	// most recent done tasks kept: done-14 .. done-05
	for _, r := range rows {
		if r.isTask() && r.status == model.StatusDone && r.task.Task.Slug < "done-05" {
			t.Fatalf("old done task %s shown", r.task.Task.Slug)
		}
	}
	m, _ := newTest(t, &fakeBackend{tasks: tasks})
	if !strings.Contains(plainView(m), "DONE (10/15)") {
		t.Fatalf("header count missing:\n%s", plainView(m))
	}
}

func TestSelectionSkipsHeadersAndSurvivesReload(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks()}
	m, _ := newTest(t, fb)
	want := []string{"#backlog", "beta", "#active", "alpha", "gamma", "#review", "delta"}
	if got := rowSlugs(m); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows %v", got)
	}
	if m.selected().Task.Slug != "beta" {
		t.Fatalf("initial selection %s", m.selected().Task.Slug)
	}
	key(m, "j")
	if m.selected().Task.Slug != "alpha" {
		t.Fatalf("after j: %s", m.selected().Task.Slug)
	}
	key(m, "down")
	key(m, "j")
	if m.selected().Task.Slug != "delta" {
		t.Fatalf("after 3 down: %s", m.selected().Task.Slug)
	}
	key(m, "j") // at the end: stays
	if m.selected().Task.Slug != "delta" {
		t.Fatal("moved past end")
	}
	key(m, "k")
	key(m, "up")
	key(m, "k")
	key(m, "k")
	if m.selected().Task.Slug != "beta" {
		t.Fatalf("after ups: %s", m.selected().Task.Slug)
	}

	// select gamma, then it moves to review: selection follows the ID
	key(m, "j")
	key(m, "j")
	fb.tasks[2].Task.Status = model.StatusReview
	drain(m, m.listCmd())
	if s := m.selected().Task.Slug; s != "gamma" {
		t.Fatalf("selection after reload: %s", s)
	}
	// gamma disappears: nearest task is selected
	fb.tasks = append(fb.tasks[:2], fb.tasks[3])
	drain(m, m.listCmd())
	if m.selected() == nil {
		t.Fatal("no selection after task vanished")
	}
	if s := m.selected().Task.Slug; s != "delta" {
		t.Fatalf("nearest after vanish: %s (rows %v)", s, rowSlugs(m))
	}
	// everything disappears
	fb.tasks = nil
	drain(m, m.listCmd())
	if m.selected() != nil || !strings.Contains(plainView(m), "no tasks") {
		t.Fatal("expected empty list")
	}
	key(m, "j")
	key(m, "d")
	if m.mode != modeNormal {
		t.Fatal("d without selection changed mode")
	}
}

func TestFilter(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks()}
	fb.tasks[3].Task.Title = "Fix the LOGIN page"
	m, _ := newTest(t, fb)
	key(m, "/")
	typeText(m, "login")
	if got := rowSlugs(m); !reflect.DeepEqual(got, []string{"#review", "delta"}) {
		t.Fatalf("filtered rows %v", got)
	}
	key(m, "enter")
	if m.mode != modeNormal || m.filter != "login" {
		t.Fatalf("enter did not apply: mode %d filter %q", m.mode, m.filter)
	}
	drain(m, m.listCmd()) // filter survives reload
	if len(m.rows) != 2 {
		t.Fatalf("rows after reload %v", rowSlugs(m))
	}
	key(m, "esc")
	if m.filter != "" || len(m.rows) != 7 {
		t.Fatalf("esc did not clear: %q %v", m.filter, rowSlugs(m))
	}
	key(m, "/")
	typeText(m, "AMM") // gamma, case-insensitive
	if got := rowSlugs(m); !reflect.DeepEqual(got, []string{"#active", "gamma"}) {
		t.Fatalf("rows %v", got)
	}
	key(m, "esc")
	if m.filter != "" || m.mode != modeNormal || len(m.rows) != 7 {
		t.Fatal("esc in filter mode did not clear")
	}
}

func TestWaitingMarkerAndHighlight(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks()}
	fb.tasks[0].Sessions = []model.Session{{ID: 3, Name: "claude-3", State: model.StateEnded, LastEventAt: t0}}
	m, _ := newTest(t, fb)
	v := plainView(m)
	if !strings.Contains(v, "● gamma") {
		t.Fatalf("no waiting marker:\n%s", v)
	}
	if strings.Contains(v, "● alpha") || strings.Contains(v, "● beta") {
		t.Fatalf("marker on non-waiting task:\n%s", v)
	}
	key(m, "j")
	key(m, "j") // gamma
	v = plainView(m)
	if !strings.Contains(v, "claude-7") || !strings.Contains(v, "waiting") || !strings.Contains(v, "2m") {
		t.Fatalf("session line missing:\n%s", v)
	}
	raw := m.View()
	var line string
	for _, l := range strings.Split(raw, "\n") {
		if strings.Contains(ansi.Strip(l), "claude-7") {
			line = l
		}
	}
	if line == "" {
		t.Fatal("no session line")
	}
	right := line[strings.Index(line, "│"):]
	if waitStyle.Render("x") != "x" && right == ansi.Strip(right) {
		t.Fatalf("waiting session not styled: %q", line)
	}
	if !strings.Contains(ansi.Strip(line), "▶") {
		t.Fatalf("waiting session not highlighted: %q", ansi.Strip(line))
	}
	// details pane
	for _, s := range []string{"slug", "atl/gamma", "+300", "/tasks/gamma", "rw  api  ← dev", "ro  docs"} {
		if !strings.Contains(v, s) {
			t.Errorf("details missing %q:\n%s", s, v)
		}
	}
}

func TestDirtyRepoShown(t *testing.T) {
	fb := &fakeBackend{tasks: []app.TaskView{tv(1, "alpha", model.StatusActive)}}
	fb.tasks[0].Task.Repos[0].Name = "dirtyrepo"
	m, _ := newTest(t, fb)
	drain(m, m.detailCmd(1, "alpha"))
	if !strings.Contains(plainView(m), "rw  dirtyrepo *") {
		t.Fatalf("dirty marker missing:\n%s", plainView(m))
	}
}

func TestStatusKeys(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks()}
	m, _ := newTest(t, fb)
	key(m, "j") // alpha (active)
	for i, st := range model.AllStatuses {
		if st == model.StatusActive {
			continue
		}
		key(m, fmt.Sprint(i+1))
		c := fb.last("SetStatus")
		if c == nil || c.args[0] != "alpha" || c.args[1] != st {
			t.Fatalf("key %d: %+v", i+1, c)
		}
		key(m, "2") // back to active
	}
	if m.selected().Task.Slug != "alpha" {
		t.Fatal("selection lost")
	}
}

func TestDoneConfirm(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks()}
	m, _ := newTest(t, fb)
	key(m, "d")
	if !strings.Contains(plainView(m), "done beta? y/N") {
		t.Fatalf("no prompt:\n%s", plainView(m))
	}
	key(m, "n")
	if fb.count("Done") != 0 || m.mode != modeNormal {
		t.Fatal("n should abort")
	}
	key(m, "d")
	key(m, "y")
	c := fb.last("Done")
	if c == nil || c.args[0] != "beta" || c.args[1] != false {
		t.Fatalf("done call %+v", c)
	}
	key(m, "c")
	key(m, "y")
	if c := fb.last("Cancel"); c == nil || c.args[0] != "beta" {
		t.Fatalf("cancel call %+v", c)
	}
}

func TestDoneDirtyForce(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks(), doneErr: errors.New("task beta has uncommitted changes in api; commit or stash them, or use --force to discard")}
	m, _ := newTest(t, fb)
	key(m, "d")
	key(m, "y")
	if m.mode != modeConfirm || !strings.Contains(plainView(m), "F force") {
		t.Fatalf("no force prompt:\n%s", plainView(m))
	}
	key(m, "F")
	c := fb.last("Done")
	if c == nil || c.args[1] != true || fb.count("Done") != 2 {
		t.Fatalf("force call %+v", fb.calls)
	}
	// any other key aborts
	key(m, "d")
	key(m, "y")
	key(m, "x")
	if fb.count("Done") != 3 || m.mode != modeNormal {
		t.Fatal("x should abort the force")
	}
	// non-dirty error: shown, no force prompt
	fb.doneErr = errors.New("kill tmux session: boom")
	key(m, "d")
	key(m, "y")
	if m.mode != modeNormal || !m.statusErr || !strings.Contains(m.status, "boom") {
		t.Fatalf("error not shown: %q", m.status)
	}
}

func TestNewTaskForm(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks(), projects: []string{"shop", "site"}, repos: []string{"api", "docs", "web"},
		projRepos: map[string][]app.RepoArg{"site": {{Name: "web", Mode: model.ModeRW}, {Name: "docs", Mode: model.ModeRO}}}}
	m, _ := newTest(t, fb)
	key(m, "n")
	if m.mode != modeForm {
		t.Fatal("no form")
	}
	typeText(m, "Add login")
	key(m, "enter")
	key(m, "s")
	key(m, "tab") // completes s → shop
	if v := m.form.inputs[fieldProject].Value(); v != "shop" {
		t.Fatalf("project after tab %q", v)
	}
	key(m, "tab")
	if v := m.form.inputs[fieldProject].Value(); v != "site" {
		t.Fatalf("project after 2nd tab %q", v)
	}
	if !strings.Contains(plainView(m), "projects: shop, site") {
		t.Fatalf("no project hint:\n%s", plainView(m))
	}
	// site's repos are preselected with the project's modes
	if got := summary(m.form.repos); got != "web(rw) docs(ro)" || m.form.preProject != "site" {
		t.Fatalf("preselection %q (pre %q); calls %v", got, m.form.preProject, fb.calls)
	}
	if fb.count("ProjectRepos") != 2 { // shop (unknown to the fake) then site
		t.Fatalf("ProjectRepos calls: %d", fb.count("ProjectRepos"))
	}
	key(m, "enter") // → Repos
	if !strings.Contains(plainView(m), "Repos       web(rw) docs(ro)") {
		t.Fatalf("repos field:\n%s", plainView(m))
	}
	key(m, "enter") // opens the picker
	if m.mode != modePicker || m.picker == nil {
		t.Fatalf("picker not open: mode %d", m.mode)
	}
	v := plainView(m)
	for _, want := range []string{"[rw] web", "[ro] docs", "[  ] api", "selected: web(rw) docs(ro)"} {
		if !strings.Contains(v, want) {
			t.Fatalf("picker lacks %q:\n%s", want, v)
		}
	}
	key(m, " ") // api (highlighted first) → rw
	key(m, "down")
	key(m, " ") // docs ro → none
	typeText(m, "we")
	key(m, " ") // web rw → ro
	key(m, "enter")
	if m.mode != modeForm || summary(m.form.repos) != "web(ro) api(rw)" {
		t.Fatalf("after picker: mode %d repos %q", m.mode, summary(m.form.repos))
	}
	// confirming moves on to Base: enter does not reopen the picker
	if m.form.focus != fieldBase {
		t.Fatalf("focus after confirm: %d", m.form.focus)
	}
	key(m, "up")
	// esc in the picker keeps the previous selection (and Repos focused)
	key(m, " ")
	key(m, " ")
	key(m, "esc")
	if m.mode != modeForm || summary(m.form.repos) != "web(ro) api(rw)" || m.form.focus != fieldRepos {
		t.Fatalf("after esc: mode %d repos %q focus %d", m.mode, summary(m.form.repos), m.form.focus)
	}
	key(m, "down") // Base: auto by default, toggled by right/space/left
	if v := plainView(m); !strings.Contains(v, "(•) auto (dev → develop → default)") || !strings.Contains(v, baseHint) {
		t.Fatalf("base field:\n%s", v)
	}
	key(m, "right")
	if !m.form.prodBase || !strings.Contains(plainView(m), "(•) production patch (default branch)") {
		t.Fatalf("right did not toggle:\n%s", plainView(m))
	}
	key(m, " ")
	if m.form.prodBase {
		t.Fatal("space did not toggle back")
	}
	key(m, "left")
	if !m.form.prodBase || m.form.focus != fieldBase {
		t.Fatalf("left: prod %v focus %d", m.form.prodBase, m.form.focus)
	}
	key(m, "enter") // enter advances to Description without toggling
	if !m.form.prodBase || m.form.focus != fieldDesc {
		t.Fatalf("enter: prod %v focus %d", m.form.prodBase, m.form.focus)
	}
	typeText(m, "the description")
	key(m, "ctrl+b")
	key(m, "enter") // focus backlog
	key(m, "enter") // submit
	c := fb.last("NewTask")
	if c == nil {
		t.Fatalf("no NewTask call; form err %q", m.form.err)
	}
	want := app.NewTaskOpts{
		Title: "Add login", Project: "site", Description: "the description", Backlog: true, ExactRepos: true,
		Base:  model.BaseDefault,
		Repos: []app.RepoArg{{Name: "web", Mode: model.ModeRO}, {Name: "api", Mode: model.ModeRW}},
	}
	if got := c.args[0].(app.NewTaskOpts); !reflect.DeepEqual(got, want) {
		t.Fatalf("opts\n got %+v\nwant %+v", got, want)
	}
	if m.mode != modeNormal || !strings.Contains(m.status, "created add-login") {
		t.Fatalf("after submit: mode %d status %q", m.mode, m.status)
	}
}

// The project preselection is replaced when the project changes, manual
// picks are kept, and a project left alone (not yet loaded) is sent as is.
func TestNewTaskFormProjectSwitch(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks(), projects: []string{"shop", "site"}, repos: []string{"api", "docs", "lib", "web"},
		projRepos: map[string][]app.RepoArg{
			"shop": {{Name: "api", Mode: model.ModeRW}, {Name: "lib", Mode: model.ModeRO}},
			"site": {{Name: "web", Mode: model.ModeRW}},
		}}
	m, _ := newTest(t, fb)
	key(m, "n")
	typeText(m, "T")
	key(m, "down")
	key(m, "down") // Repos
	key(m, " ")    // picker
	typeText(m, "docs")
	key(m, " ")
	key(m, "enter") // → Description
	key(m, "up")
	key(m, "up") // Project
	typeText(m, "shop")
	if got := summary(m.form.repos); got != "api(rw) lib(ro) docs(rw)" {
		t.Fatalf("shop preselect: %q", got)
	}
	for range "shop" {
		key(m, "backspace")
	}
	typeText(m, "site")
	if got := summary(m.form.repos); got != "web(rw) docs(rw)" {
		t.Fatalf("site preselect: %q", got)
	}
	key(m, "ctrl+u") // textinput: clear the field → preselection dropped
	if got := summary(m.form.repos); got != "docs(rw)" || m.form.preProject != "" {
		t.Fatalf("cleared project: %q", got)
	}
	key(m, "down")
	key(m, "down")
	key(m, "down")
	key(m, "down")
	key(m, "enter")
	c := fb.last("NewTask")
	want := app.NewTaskOpts{Title: "T", Repos: []app.RepoArg{{Name: "docs", Mode: model.ModeRW}}}
	if c == nil || !reflect.DeepEqual(c.args[0].(app.NewTaskOpts), want) {
		t.Fatalf("opts %+v; err %q", c, m.lastForm.err)
	}

	// Project typed but its repos never loaded (no picker selection): sent
	// as the project alone, so NewTask uses the project's repos.
	fb.projRepos = nil
	m.lastForm = nil
	key(m, "n")
	typeText(m, "U")
	key(m, "down")
	typeText(m, "shop") // load fails: not preselected
	if !strings.Contains(plainView(m), "project \"shop\" not found") {
		t.Fatalf("project error not shown:\n%s", plainView(m))
	}
	key(m, "down")
	key(m, "down")
	key(m, "down")
	key(m, "down")
	key(m, "enter")
	if c := fb.last("NewTask"); c == nil || c.args[0].(app.NewTaskOpts).Title != "U" {
		t.Fatalf("NewTask not called: %q", m.form.err)
	}
}

func TestNewTaskFormValidation(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks(), repos: []string{"api"}, projects: []string{"shop"},
		projRepos: map[string][]app.RepoArg{"shop": {{Name: "api", Mode: model.ModeRW}}}}
	m, _ := newTest(t, fb)
	key(m, "n")
	for i := 0; i < numFields-1; i++ {
		key(m, "down")
	}
	key(m, "enter") // submit with empty title
	if fb.count("NewTask") != 0 || !strings.Contains(m.form.err, "title") {
		t.Fatalf("empty title: err %q", m.form.err)
	}
	for i := 0; i < numFields-1; i++ {
		key(m, "up")
	}
	typeText(m, "T")
	for i := 0; i < numFields-1; i++ {
		key(m, "down")
	}
	key(m, "enter") // no repos, no project
	if fb.count("NewTask") != 0 || m.mode != modeForm || !strings.Contains(plainView(m), "pick at least one repo") {
		t.Fatalf("no repos: calls %v err %q", fb.calls, m.form.err)
	}
	// a preselected project whose repos were all deselected
	for i := fieldBacklog; i > fieldProject; i-- {
		key(m, "up")
	}
	typeText(m, "shop")
	key(m, "down")
	key(m, "enter") // picker
	key(m, " ")     // api rw → ro
	key(m, " ")     // → none
	key(m, "enter")
	key(m, "down")
	key(m, "down")
	key(m, "enter")
	if fb.count("NewTask") != 0 || !strings.Contains(m.form.err, "no repos selected") {
		t.Fatalf("deselected all: err %q", m.form.err)
	}
	key(m, "esc")
	if m.mode != modeNormal || fb.count("NewTask") != 0 {
		t.Fatal("esc did not cancel")
	}
}

func manyRepos(n int) []string {
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("repo-%03d", i))
	}
	return out
}

func TestPicker(t *testing.T) {
	if v := strings.Join(newPicker("Pick", nil, nil).lines(40, 8), "\n"); !strings.Contains(ansi.Strip(v), "loading repos") {
		t.Fatalf("loading: %s", v)
	}
	p := newPicker("Pick", []app.RepoArg{{Name: "gone", Mode: model.ModeRO}}, map[string]model.Mode{"api": model.ModeRW})
	p.setAvailable([]string{"api", "Bill-app", "docs", "web-app"}, nil)
	// a selected repo missing on disk is still listed (sorted in)
	if got := p.visible(); !reflect.DeepEqual(got, []string{"Bill-app", "api", "docs", "gone", "web-app"}) {
		t.Fatalf("items %v", got)
	}
	upd := func(k string) pickResult { return p.update(keyMsg(k)) }
	for _, r := range "BIL" {
		upd(string(r))
	}
	if got := p.visible(); !reflect.DeepEqual(got, []string{"Bill-app"}) {
		t.Fatalf("filter %q: %v", p.filter, got)
	}
	upd("backspace")
	upd("backspace")
	upd("backspace")
	if p.filter != "" || len(p.visible()) != 5 {
		t.Fatalf("backspace: %q %v", p.filter, p.visible())
	}
	upd("a") // Bill-app, api, web-app
	if got := p.visible(); !reflect.DeepEqual(got, []string{"Bill-app", "api", "web-app"}) {
		t.Fatalf("filter a: %v", got)
	}
	// mode cycling: none → rw → ro → none
	upd(" ")
	if p.modeOf("Bill-app") != model.ModeRW {
		t.Fatal("not rw")
	}
	upd(" ")
	if p.modeOf("Bill-app") != model.ModeRO {
		t.Fatal("not ro")
	}
	upd(" ")
	if p.modeOf("Bill-app") != "" {
		t.Fatal("not none")
	}
	upd(" ")
	// locked (in task) repos: dimmed, not selectable
	upd("ctrl+n")
	if p.current() != "api" {
		t.Fatalf("current %q", p.current())
	}
	upd(" ")
	if p.modeOf("api") != "" || !strings.Contains(p.msg, "already in the task") {
		t.Fatalf("locked repo selected: %v %q", p.sel, p.msg)
	}
	view := ansi.Strip(strings.Join(p.lines(60, 10), "\n"))
	if !strings.Contains(view, "[rw] api  (in task)") || !strings.Contains(view, "already in the task") {
		t.Fatalf("locked row:\n%s", view)
	}
	upd("ctrl+n")
	upd(" ")
	upd("ctrl+p")
	upd("ctrl+p")
	if p.current() != "Bill-app" {
		t.Fatalf("ctrl+p: %q", p.current())
	}
	upd("ctrl+u")
	upd("up") // clamps at the top
	if p.current() != "Bill-app" {
		t.Fatalf("top clamp: %q", p.current())
	}
	want := []app.RepoArg{{Name: "gone", Mode: model.ModeRO}, {Name: "Bill-app", Mode: model.ModeRW}, {Name: "web-app", Mode: model.ModeRW}}
	if !reflect.DeepEqual(p.selection(), want) {
		t.Fatalf("selection %v", p.selection())
	}
	view = ansi.Strip(strings.Join(p.lines(60, 12), "\n"))
	for _, s := range []string{"[ro] gone", "[rw] Bill-app", "[  ] docs", "selected: gone(ro) Bill-app(rw) web-app(rw)", "filter: "} {
		if !strings.Contains(view, s) {
			t.Fatalf("view lacks %q:\n%s", s, view)
		}
	}
	if upd("enter") != pickConfirm || upd("esc") != pickCancel {
		t.Fatal("enter/esc")
	}
	upd("zzz")
	if v := ansi.Strip(strings.Join(p.lines(40, 6), "\n")); !strings.Contains(v, "no repos match zzz") {
		t.Fatalf("no match:\n%s", v)
	}
	// a pasted multi-rune string filters; spaces are dropped
	p.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" do cs"), Paste: true})
	if p.filter != "zzzdocs" {
		t.Fatalf("paste: %q", p.filter)
	}
}

func TestPickerScroll(t *testing.T) {
	p := newPicker("Pick", nil, nil)
	p.setAvailable(manyRepos(100), nil)
	for _, size := range [][2]int{{1, 1}, {5, 2}, {10, 3}, {20, 5}, {30, 8}, {80, 24}} {
		w, h := size[0], size[1]
		p.cursor, p.offset = 0, 0
		for i := 0; i < 120; i++ {
			lines := p.lines(w, h)
			if len(lines) != h {
				t.Fatalf("%dx%d: %d lines", w, h, len(lines))
			}
			found := false
			for _, l := range lines {
				if ansi.StringWidth(l) > w {
					t.Fatalf("%dx%d: line too wide: %q", w, h, ansi.Strip(l))
				}
				found = found || strings.Contains(ansi.Strip(l), p.current()) && w >= 20
			}
			if w >= 20 && !found {
				t.Fatalf("%dx%d: cursor row %q not visible at step %d:\n%s", w, h, p.current(), i, ansi.Strip(strings.Join(lines, "\n")))
			}
			p.update(keyMsg("down"))
		}
		if p.current() != "repo-099" {
			t.Fatalf("bottom clamp: %q", p.current())
		}
		p.update(keyMsg("pgup"))
		if p.current() != "repo-089" {
			t.Fatalf("pgup: %q", p.current())
		}
	}
}

func TestAddRepoFlow(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks(), repos: []string{"api", "docs", "lib", "web"},
		addErr: map[string]error{"web": errors.New("repo web: boom")}}
	m, _ := newTest(t, fb) // beta (backlog) selected; it has api rw, docs ro
	if !strings.Contains(plainView(m), "a add repo") {
		t.Fatal("help line lacks a")
	}
	key(m, "a")
	if m.mode != modePicker || m.pickTask == nil || m.pickTask.Task.Slug != "beta" {
		t.Fatalf("picker not open for beta: mode %d", m.mode)
	}
	v := plainView(m)
	if !strings.Contains(v, "Add repos to beta") || !strings.Contains(v, "[rw] api  (in task)") || !strings.Contains(v, "[ro] docs  (in task)") {
		t.Fatalf("add picker:\n%s", v)
	}
	// the cursor starts on the first repo not in the task
	if cur := m.picker.current(); cur != "lib" || !strings.Contains(plainView(m), "> [  ] lib") {
		t.Fatalf("initial cursor on %q:\n%s", cur, plainView(m))
	}
	key(m, "up") // docs: locked, still shows the cursor marker
	if !strings.Contains(plainView(m), "> [ro] docs  (in task)") {
		t.Fatalf("no cursor on locked row:\n%s", plainView(m))
	}
	key(m, " ")
	if len(m.picker.sel) != 0 {
		t.Fatal("locked repo selected")
	}
	key(m, "down") // lib: the hint stays until the selection changes
	if !strings.Contains(plainView(m), "docs is already in the task") {
		t.Fatalf("hint cleared by a move:\n%s", plainView(m))
	}
	key(m, " ")
	if strings.Contains(plainView(m), "already in the task") {
		t.Fatalf("hint kept after a selection change:\n%s", plainView(m))
	}
	key(m, " ") // lib ro
	key(m, "down")
	key(m, " ") // web rw
	key(m, "down")
	if !strings.Contains(plainView(m), "selected: lib(ro) web(rw)") {
		t.Fatalf("summary:\n%s", plainView(m))
	}
	// Run step by step to watch progress.
	cmd := press(m, "enter")
	if m.mode != modeNormal || !strings.Contains(m.busy[2], "add lib (1/2) to beta") {
		t.Fatalf("busy %q", m.busy[2])
	}
	if !strings.Contains(plainView(m), "working… add lib (1/2) to beta") {
		t.Fatalf("no progress:\n%s", plainView(m))
	}
	_, next := m.Update(cmd())
	if !strings.Contains(m.busy[2], "add web (2/2) to beta") {
		t.Fatalf("busy after step 1: %q", m.busy[2])
	}
	// another op on the task waits
	key(m, "a")
	if m.mode != modeNormal || !strings.Contains(m.status, "busy") {
		t.Fatalf("a while busy: mode %d status %q", m.mode, m.status)
	}
	drain(m, next)
	var got []string
	for _, c := range fb.calls {
		if c.name == "AddRepo" {
			got = append(got, fmt.Sprintf("%v %v %v", c.args[0], c.args[1], c.args[2]))
		}
	}
	if want := []string{"beta {lib ro} false", "beta {web rw} false"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("AddRepo calls %v", got)
	}
	if _, busy := m.busy[2]; busy {
		t.Fatal("still busy")
	}
	if !m.statusErr || !strings.Contains(m.status, "added lib(ro) to beta; start a new session (s) to see lib") ||
		!strings.Contains(m.status, "failed: web: repo web: boom") {
		t.Fatalf("status %q", m.status)
	}

	// esc cancels; enter with nothing selected does nothing
	n := fb.count("AddRepo")
	key(m, "a")
	key(m, "esc")
	key(m, "a")
	key(m, "enter")
	if fb.count("AddRepo") != n || m.mode != modeNormal || !strings.Contains(m.status, "nothing selected") {
		t.Fatalf("cancel/empty: %d calls, status %q", fb.count("AddRepo")-n, m.status)
	}
}

func TestAddRepoClosedTask(t *testing.T) {
	fb := &fakeBackend{tasks: []app.TaskView{tv(1, "old", model.StatusDone)}, repos: []string{"api"}}
	m, _ := newTest(t, fb)
	key(m, "a")
	if m.mode != modeNormal || !m.statusErr || !strings.Contains(m.status, "task is closed") {
		t.Fatalf("mode %d status %q", m.mode, m.status)
	}
	if fb.count("AvailableRepos") != 0 {
		t.Fatal("picker loaded repos for a closed task")
	}
}

func TestTicks(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks()}
	m, sched := newTest(t, fb)
	n := fb.count("List")
	// the initial selection's debounced fetch is pending: ticks skip it
	if !m.selPending {
		t.Fatal("no pending selection fetch after load")
	}
	var selTicks []tea.Msg
	for _, msg := range *sched {
		if _, ok := msg.(selTickMsg); ok {
			selTicks = append(selTicks, msg)
		}
	}
	*sched = nil
	_, cmd := m.Update(listTickMsg{})
	if len(*sched) != 1 || (*sched)[0] != (listTickMsg{}) {
		t.Fatalf("list tick not rescheduled: %v", *sched)
	}
	drain(m, cmd)
	if fb.count("List") != n+1 {
		t.Fatalf("list calls %d → %d", n, fb.count("List"))
	}
	if fb.count("Get") != 0 {
		t.Fatalf("tick bypassed the selection debounce: %v", fb.calls)
	}
	for _, msg := range selTicks {
		drain(m, func() tea.Msg { return msg })
	}
	if fb.count("Get") != 1 || m.selPending {
		t.Fatalf("debounced fetch: %d gets, pending %v", fb.count("Get"), m.selPending)
	}
	// detail: not fetched again within 3s
	gets := fb.count("Get")
	m.Update(listTickMsg{})
	if fb.count("Get") != gets {
		t.Fatal("Get without running cmd?")
	}
	_, cmd = m.Update(listTickMsg{})
	drain(m, cmd)
	if fb.count("Get") != gets {
		t.Fatalf("Get within 3s: %d → %d", gets, fb.count("Get"))
	}
	now := m.now()
	m.now = func() time.Time { return now.Add(4 * time.Second) }
	_, cmd = m.Update(listTickMsg{})
	drain(m, cmd)
	if fb.count("Get") != gets+1 {
		t.Fatalf("Get after 3s: %d → %d", gets, fb.count("Get"))
	}
	// recover
	r := fb.count("Recover")
	*sched = nil
	_, cmd = m.Update(recoverTickMsg{})
	drain(m, cmd)
	if fb.count("Recover") != r+1 || len(*sched) != 1 || (*sched)[0] != (recoverTickMsg{}) {
		t.Fatalf("recover tick: %d → %d, sched %v", r, fb.count("Recover"), *sched)
	}
	// selection change: debounced Get for the new task
	*sched = nil
	key(m, "j")
	if len(*sched) != 1 {
		t.Fatalf("no debounce scheduled: %v", *sched)
	}
	drain(m, func() tea.Msg { return (*sched)[0] })
	if c := fb.last("Get"); c == nil || c.args[0] != "alpha" {
		t.Fatalf("Get after move: %+v", c)
	}
}

func TestOtherKeys(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks()}
	m, _ := newTest(t, fb)
	key(m, "s")
	key(m, "r")
	key(m, "p")
	key(m, "enter")
	for _, n := range []string{"NewSession", "Resume", "PR", "Open"} {
		if c := fb.last(n); c == nil || c.args[0] != "beta" {
			t.Errorf("%s: %+v", n, c)
		}
	}
	// busy flag: a pending op blocks a second one on the same task
	m.busy[2] = "pr beta"
	key(m, "s")
	if fb.count("NewSession") != 1 || !strings.Contains(m.status, "busy") {
		t.Fatalf("busy not honoured: %q", m.status)
	}
	delete(m.busy, 2)
	key(m, "p")
	if !strings.Contains(m.status, "https://example.com/pr/1") || !strings.Contains(m.status, "web skipped (no commits)") {
		t.Fatalf("pr status %q", m.status)
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil || cmd() != tea.Quit() {
		t.Fatal("q does not quit")
	}
}

func TestSmallSizes(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks(), projects: []string{"p"}, repos: manyRepos(100)}
	for i := 0; i < 30; i++ {
		fb.tasks = append(fb.tasks, tv(int64(10+i), fmt.Sprintf("task-with-a-rather-long-slug-%d", i), model.StatusActive,
			model.Session{ID: int64(i), State: model.StateWaiting, LastEventAt: t0}))
	}
	m, _ := newTest(t, fb)
	sizes := [][2]int{{0, 0}, {1, 1}, {10, 5}, {30, 10}, {39, 12}, {40, 3}, {80, 24}, {200, 60}}
	check := func(w, h int) {
		t.Helper()
		v := m.View()
		lines := strings.Split(v, "\n")
		if h > 0 && len(lines) > h {
			t.Errorf("%dx%d: %d lines", w, h, len(lines))
		}
		for _, l := range lines {
			if ansi.StringWidth(l) > w {
				t.Errorf("%dx%d: line too wide (%d): %q", w, h, ansi.StringWidth(l), ansi.Strip(l))
			}
		}
	}
	for _, s := range sizes {
		m.Update(tea.WindowSizeMsg{Width: s[0], Height: s[1]})
		check(s[0], s[1])
		for i := 0; i < 40; i++ {
			key(m, "j")
		}
		check(s[0], s[1])
		key(m, "n")
		check(s[0], s[1])
		key(m, "down")
		key(m, "down")
		key(m, " ") // repo picker
		check(s[0], s[1])
		key(m, "esc")
		key(m, "esc")
		key(m, "a")
		check(s[0], s[1])
		key(m, "esc")
		key(m, "d")
		check(s[0], s[1])
		key(m, "x")
	}
	m.Update(tea.WindowSizeMsg{Width: 30, Height: 10})
	if strings.Contains(plainView(m), "│") {
		t.Fatal("narrow terminal shows the details pane")
	}
}

func TestHumanAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second: "0s", 5 * time.Second: "5s", 3 * time.Minute: "3m", 2*time.Hour + 5*time.Minute: "2h", 100 * time.Hour: "4d",
	} {
		if got := humanAge(d); got != want {
			t.Errorf("%v: %q want %q", d, got, want)
		}
	}
}

func TestLineWriter(t *testing.T) {
	var got []string
	w := &lineWriter{send: func(s string) { got = append(got, s) }}
	fmt.Fprint(w, "fetching a...\nwarn")
	fmt.Fprint(w, "ing: x\n\n")
	if !reflect.DeepEqual(got, []string{"fetching a...", "warning: x"}) {
		t.Fatal(got)
	}
}

// isQuit reports whether cmd is tea.Quit.
func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

func TestQuitWaitsForOps(t *testing.T) {
	for _, first := range []string{"q", "ctrl+c"} {
		fb := &fakeBackend{tasks: sampleTasks()}
		m, _ := newTest(t, fb)
		if !isQuit(press(m, first)) || m.quitRequested {
			t.Fatalf("idle %s does not quit at once", first)
		}
		op := press(m, "s") // session on beta, not yet run
		if len(m.busyOps()) != 1 {
			t.Fatal("op not busy")
		}
		if isQuit(press(m, first)) {
			t.Fatalf("%s quit with an op in flight", first)
		}
		if !strings.Contains(plainView(m), "1 operation(s) running") || !m.quitRequested {
			t.Fatalf("%s: no quit notice:\n%s", first, plainView(m))
		}
		if isQuit(press(m, "q")) {
			t.Fatal("second q quit with an op in flight")
		}
		// the op ends: the TUI quits by itself
		_, cmd := m.Update(op())
		if !isQuit(cmd) || m.Forced() {
			t.Fatalf("%s: no automatic quit after the op ended", first)
		}
	}

	// second ctrl+c forces
	fb := &fakeBackend{tasks: sampleTasks()}
	m, _ := newTest(t, fb)
	press(m, "s")
	if isQuit(press(m, "q")) {
		t.Fatal("q quit")
	}
	if !isQuit(press(m, "ctrl+c")) || !m.Forced() {
		t.Fatal("second ctrl+c did not force")
	}

	// a new task in flight counts too
	fb.repos = []string{"api"}
	m, _ = newTest(t, fb)
	key(m, "n")
	typeText(m, "x")
	key(m, "down")
	key(m, "down")
	key(m, " ") // picker
	key(m, " ") // api rw
	key(m, "enter")
	key(m, "down")
	key(m, "down")
	create := press(m, "enter")
	if !m.creating || isQuit(press(m, "ctrl+c")) {
		t.Fatal("ctrl+c quit while creating")
	}
	_, cmd := m.Update(create())
	if !isQuit(cmd) {
		t.Fatal("no quit after create ended")
	}
}

func TestShutdown(t *testing.T) {
	// not forced: the context stays live until the call returns
	tr := &tracker{}
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	cancelledEarly := make(chan bool, 1)
	if !tr.begin() {
		t.Fatal("begin")
	}
	go func(tr *tracker) {
		defer tr.end()
		select {
		case <-ctx.Done():
			cancelledEarly <- true
		case <-release:
			cancelledEarly <- false
		}
	}(tr)
	time.AfterFunc(50*time.Millisecond, func() { close(release) })
	if !shutdown(context.Background(), cancel, tr, false, time.Second) {
		t.Fatal("shutdown reported unfinished calls")
	}
	if <-cancelledEarly {
		t.Fatal("context cancelled before the call returned")
	}
	if ctx.Err() == nil {
		t.Fatal("context not cancelled after shutdown")
	}
	if tr.begin() {
		t.Fatal("begin after close")
	}
	tb := &trackedBackend{Backend: &fakeBackend{}, tr: tr}
	if _, err := tb.Done(context.Background(), "x", false); !errors.Is(err, errStopped) {
		t.Fatalf("call after close: %v", err)
	}

	// forced: cancelled at once; a call honouring ctx ends in time
	tr = &tracker{}
	fctx, fcancel := context.WithCancel(context.Background())
	tr.begin()
	go func(tr *tracker) { defer tr.end(); <-fctx.Done() }(tr)
	if !shutdown(context.Background(), fcancel, tr, true, 5*time.Second) {
		t.Fatal("forced shutdown did not see the call end")
	}

	// forced with a stuck call: gives up after the grace period
	tr = &tracker{}
	_, cancel = context.WithCancel(context.Background())
	stuck := make(chan struct{})
	defer close(stuck)
	tr.begin()
	go func(tr *tracker) { defer tr.end(); <-stuck }(tr)
	start := time.Now()
	if shutdown(context.Background(), cancel, tr, true, 50*time.Millisecond) {
		t.Fatal("stuck call reported finished")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("grace period not honoured")
	}

	// parent cancelled while waiting (not forced): stops waiting
	tr = &tracker{}
	parent, cancelParent := context.WithCancel(context.Background())
	pctx, pcancel := context.WithCancel(parent)
	tr.begin()
	go func(tr *tracker) { defer tr.end(); <-pctx.Done() }(tr)
	time.AfterFunc(20*time.Millisecond, cancelParent)
	if !shutdown(parent, pcancel, tr, false, 5*time.Second) {
		t.Fatal("parent cancel: call did not end")
	}
}

func TestOrderedSend(t *testing.T) {
	var mu sync.Mutex
	var got []string
	send, _, stop := orderedSend(func(s string) {
		mu.Lock()
		got = append(got, s)
		mu.Unlock()
	}, io.Discard)
	var want []string
	for i := 0; i < 200; i++ {
		s := fmt.Sprint(i)
		want = append(want, s)
		send(s)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == len(want) || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	mu.Lock()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("out of order or lost: %v", got)
	}
	mu.Unlock()

	// a consumer that never returns: send must not block after stop
	block := make(chan struct{})
	defer close(block)
	var fallback bytes.Buffer
	send, _, stop = orderedSend(func(string) { <-block }, &fallback)
	for i := 0; i < 10; i++ {
		send("x")
	}
	stop()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			send("late")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("send blocked after stop")
	}
	if !strings.Contains(fallback.String(), "late\n") {
		t.Fatalf("fallback %q", fallback.String())
	}
}

func TestSanitize(t *testing.T) {
	if got := clean("a\n\nb\x1b[31mc\t\r d\x00\x7f"); got != "a b [31mc d" {
		t.Fatalf("clean: %q", got)
	}
	fb := &fakeBackend{tasks: sampleTasks()}
	fb.tasks[1].Task.Title = "evil\x1b[2J\nsecond line\ttab"
	fb.tasks[1].Task.Description = "\x1b]0;pwn\x07desc\r\nmore"
	fb.tasks[1].Task.Project = "p\nq"
	m, _ := newTest(t, fb)
	raw := m.View()
	for _, bad := range []string{"\x1b[2J", "\x1b]0;", "\x07", "\t", "\r"} {
		if strings.Contains(raw, bad) {
			t.Fatalf("view contains %q", bad)
		}
	}
	if n := len(strings.Split(raw, "\n")); n != m.height {
		t.Fatalf("%d lines, want %d", n, m.height)
	}
	v := plainView(m)
	for _, want := range []string{"beta  evil [2J second line tab", "evil [2J second line tab", "]0;pwn desc", "p q"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q:\n%s", want, v)
		}
	}
}

func TestDirtyRefusalQueued(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks(), doneErr: errors.New("task beta has uncommitted changes in api")}
	m, _ := newTest(t, fb)
	key(m, "d")
	done := press(m, "y") // Done runs in the background…
	key(m, "n")           // …while the user opens the form
	typeText(m, "half typed")
	m.Update(done())
	if m.mode != modeForm || m.form == nil || m.form.inputs[fieldTitle].Value() != "half typed" {
		t.Fatalf("refusal switched mode: %d", m.mode)
	}
	if m.lastForm != m.form {
		t.Fatal("typed form not kept in lastForm")
	}
	key(m, "esc")
	if m.mode != modeConfirm || m.confirm == nil || m.confirm.dirty == "" || !strings.Contains(plainView(m), "F force") {
		t.Fatalf("queued refusal not shown: mode %d\n%s", m.mode, plainView(m))
	}
	key(m, "x") // abort
	if m.mode != modeNormal {
		t.Fatal("abort")
	}
	key(m, "n")
	if m.form.inputs[fieldTitle].Value() != "half typed" {
		t.Fatal("form not restored")
	}
	key(m, "esc")

	// filter mode: queued until the filter closes
	key(m, "c")
	cancelCmd := press(m, "y")
	key(m, "/")
	typeText(m, "be")
	m.Update(cancelCmd())
	if m.mode != modeFilter || m.filterInput.Value() != "be" {
		t.Fatalf("refusal switched mode from filter: %d", m.mode)
	}
	key(m, "enter")
	if m.mode != modeConfirm || m.confirm.verb != "cancel" {
		t.Fatalf("queued cancel refusal not shown: %d", m.mode)
	}
	key(m, "F")
	if c := fb.last("Cancel"); c == nil || c.args[1] != true {
		t.Fatalf("force cancel: %+v", c)
	}
}

func TestDetailStaleDropped(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks()}
	m, _ := newTest(t, fb) // beta (id 2) selected
	// a reply for another task is dropped
	alpha := m.detailCmd(1, "alpha")
	m.Update(alpha())
	if m.detail != nil {
		t.Fatalf("detail for unselected task applied: %+v", m.detail.Task.Slug)
	}
	// per-request in-flight tracking and ordering
	c1 := m.detailCmd(2, "beta")
	c2 := m.detailCmd(2, "beta")
	r1, r2 := c1(), c2()
	m.Update(r1)
	if m.detailInflight == 0 {
		t.Fatal("earlier reply cleared the in-flight flag of a later request")
	}
	m.Update(r2)
	if m.detailInflight != 0 || m.detail == nil || m.detail.Task.Slug != "beta" {
		t.Fatal("latest reply not applied")
	}
	applied := m.detailApplied
	m.Update(r1) // a late duplicate of the older reply
	if m.detailApplied != applied {
		t.Fatal("older reply applied after a newer one")
	}
}

func TestRemovedRepoOfClosedTask(t *testing.T) {
	done := tv(1, "old", model.StatusDone)
	done.Task.Repos[0].Name = "gonerepo"
	active := tv(2, "live", model.StatusActive)
	active.Task.Repos[0].Name = "gonerepo"
	fb := &fakeBackend{tasks: []app.TaskView{done, active}}
	m, _ := newTest(t, fb) // live (active) first
	drain(m, m.detailCmd(2, "live"))
	if !strings.Contains(plainView(m), "gonerepo  missing") {
		t.Fatalf("active task: no missing marker:\n%s", plainView(m))
	}
	key(m, "j")
	drain(m, m.detailCmd(1, "old"))
	v := plainView(m)
	if strings.Contains(v, "missing") || !strings.Contains(v, "gonerepo  removed") {
		t.Fatalf("done task repo:\n%s", v)
	}
}

func TestFilterRestoresSelection(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks()}
	m, _ := newTest(t, fb)
	key(m, "j")
	key(m, "j") // gamma
	key(m, "/")
	typeText(m, "delta")
	if m.selected().Task.Slug != "delta" {
		t.Fatal("filtered selection")
	}
	drain(m, m.listCmd()) // a reload while filtered keeps the memory
	key(m, "esc")
	if s := m.selected().Task.Slug; s != "gamma" {
		t.Fatalf("after clearing the filter: %s", s)
	}
	// applied filter cleared with esc in normal mode
	key(m, "/")
	typeText(m, "alp")
	key(m, "enter")
	key(m, "esc")
	if s := m.selected().Task.Slug; s != "gamma" {
		t.Fatalf("after esc in normal mode: %s", s)
	}
	// an explicit move while filtered wins
	key(m, "/")
	typeText(m, "a") // alpha, beta, gamma, delta all match "a"… narrow it
	typeText(m, "l") // alpha
	key(m, "enter")
	key(m, "/")
	key(m, "esc")
	if s := m.selected().Task.Slug; s != "gamma" {
		t.Fatalf("gamma not restored: %s", s)
	}
	key(m, "/")
	typeText(m, "et") // beta
	key(m, "enter")
	key(m, "k") // no-op move (single match) does not forget gamma
	key(m, "esc")
	if s := m.selected().Task.Slug; s != "gamma" {
		t.Fatalf("after no-op move: %s", s)
	}
}

func TestMultiRuneKeys(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks()}
	m, _ := newTest(t, fb) // beta
	key(m, "jj")
	if s := m.selected().Task.Slug; s != "gamma" {
		t.Fatalf("jj: %s", s)
	}
	key(m, "/del")
	if m.mode != modeFilter || m.filterInput.Value() != "del" {
		t.Fatalf("/del: mode %d value %q", m.mode, m.filterInput.Value())
	}
	key(m, "esc")
	// a paste in normal mode does nothing
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("dy"), Paste: true})
	drain(m, cmd)
	if m.mode != modeNormal || fb.count("Done") != 0 {
		t.Fatal("paste acted")
	}
	// in the form, multi-rune input is text
	key(m, "n")
	key(m, "abc")
	if v := m.form.inputs[fieldTitle].Value(); v != "abc" {
		t.Fatalf("form text %q", v)
	}
}

// AddRepo's output lines travel through the ordered queue; the summary of
// the `a` run must land after them, and no later progress line may hide an
// error summary.
func TestAddSummaryAfterOutput(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks(), repos: []string{"api", "docs", "lib", "web"},
		addErr: map[string]error{"web": errors.New("repo web: boom")}}
	m, _ := newTest(t, fb)
	var mu sync.Mutex
	var delivered []tea.Msg
	send, flush, stop := orderedSend(func(s string) {
		time.Sleep(20 * time.Millisecond) // a slow program: lines lag behind
		mu.Lock()
		delivered = append(delivered, warnMsg(s))
		mu.Unlock()
	}, io.Discard)
	defer stop()
	m.flushOut = flush
	fb.addOut = func(r app.RepoArg) {
		send("fetching " + r.Name + "...")
		send("added " + r.Name + " (rw) to beta: worktree at /x")
	}
	// run cmd (one AddRepo step) as the program would: its result is
	// handled after whatever the queue delivered while it ran, and lines
	// the queue delivers later after it
	feed := func() {
		mu.Lock()
		msgs := delivered
		delivered = nil
		mu.Unlock()
		for _, x := range msgs {
			m.Update(x)
		}
	}
	step := func(cmd tea.Cmd) {
		msg := cmd()
		feed()
		m.Update(msg)
		flush()
		feed()
	}

	key(m, "a")
	key(m, " ") // lib rw
	step(press(m, "enter"))
	if m.statusErr || m.status != "added lib(rw) to beta" {
		t.Fatalf("summary overwritten: %q (err %v)", m.status, m.statusErr)
	}

	key(m, "a")
	key(m, "down")
	key(m, " ") // web rw: fails
	step(press(m, "enter"))
	if !m.statusErr || !strings.Contains(m.status, "failed: web: repo web: boom") {
		t.Fatalf("error summary overwritten: %q (err %v)", m.status, m.statusErr)
	}
	want := m.status
	for _, line := range []string{"fetching api...", "warning: something else"} {
		m.Update(warnMsg(line))
		if m.status != want || !m.statusErr {
			t.Fatalf("%q replaced the error summary: %q", line, m.status)
		}
	}
	// once a new action sets the status, lines show again
	key(m, "a")
	key(m, "esc")
	m.Update(warnMsg("fetching api..."))
	if m.status != "fetching api..." || m.statusErr {
		t.Fatalf("progress after action: %q", m.status)
	}
	m.Update(warnMsg("warning: w1"))
	m.Update(warnMsg("warning: w2"))
	m.Update(warnMsg("fetching docs..."))
	if m.status != "fetching docs..." {
		t.Fatalf("a line did not replace a line: %q", m.status)
	}
}

func TestOrderedSendFlush(t *testing.T) {
	var mu sync.Mutex
	n := 0
	send, flush, stop := orderedSend(func(string) {
		time.Sleep(time.Millisecond)
		mu.Lock()
		n++
		mu.Unlock()
	}, io.Discard)
	for i := 0; i < 20; i++ {
		send("x")
	}
	flush()
	mu.Lock()
	if n != 20 {
		t.Fatalf("flush returned after %d of 20 lines", n)
	}
	mu.Unlock()
	stop()
	done := make(chan struct{})
	go func() { flush(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("flush blocked after stop")
	}
}

// esc in the picker clears a filter first, then cancels.
func TestPickerEscClearsFilter(t *testing.T) {
	p := newPicker("Pick", nil, nil)
	p.setAvailable([]string{"api", "web"}, nil)
	p.update(keyMsg("w"))
	if res := p.update(keyMsg("esc")); res != pickContinue || p.filter != "" || len(p.visible()) != 2 {
		t.Fatalf("esc with filter: res %d filter %q", res, p.filter)
	}
	if res := p.update(keyMsg("esc")); res != pickCancel {
		t.Fatalf("esc without filter: res %d", res)
	}

	fb := &fakeBackend{tasks: sampleTasks(), repos: []string{"api", "docs", "lib", "web"}}
	m, _ := newTest(t, fb)
	key(m, "a")
	typeText(m, "we")
	key(m, "esc")
	if m.mode != modePicker || m.picker == nil || m.picker.filter != "" {
		t.Fatalf("esc with filter left the picker: mode %d", m.mode)
	}
	key(m, "esc")
	if m.mode != modeNormal || m.picker != nil {
		t.Fatalf("esc without filter: mode %d", m.mode)
	}
}

// Locked rows: the cursor marker is drawn on them, the cursor starts on
// the first unlocked row (also when the repo list is already known) and
// the "already in the task" hint survives moves.
func TestPickerLockedRows(t *testing.T) {
	locked := map[string]model.Mode{"api": model.ModeRW, "docs": model.ModeRO}
	p := newPicker("Pick", nil, locked)
	p.setAvailable([]string{"api", "docs", "lib", "web"}, nil)
	if p.current() != "lib" {
		t.Fatalf("initial cursor on %q", p.current())
	}
	p.update(keyMsg("home"))
	p.update(keyMsg(" "))
	view := ansi.Strip(strings.Join(p.lines(60, 10), "\n"))
	if !strings.Contains(view, "> [rw] api  (in task)") || !strings.Contains(view, "api is already in the task") {
		t.Fatalf("locked cursor row:\n%s", view)
	}
	p.update(keyMsg("down"))
	p.update(keyMsg("down"))
	if !strings.Contains(p.msg, "already in the task") {
		t.Fatal("hint cleared by moving")
	}
	// a refresh after the user moved keeps the cursor where it is
	p.setAvailable([]string{"api", "docs", "lib", "web"}, nil)
	if p.current() != "lib" || !strings.Contains(p.msg, "already in the task") {
		t.Fatalf("refresh moved the cursor to %q / dropped the hint", p.current())
	}
	p.update(keyMsg("x")) // filter change clears the hint
	if p.msg != "" {
		t.Fatalf("hint kept after filtering: %q", p.msg)
	}

	// everything locked: cursor on the first row
	p = newPicker("Pick", nil, map[string]model.Mode{"a": model.ModeRO, "b": model.ModeRO})
	p.setAvailable([]string{"a", "b"}, nil)
	if p.current() != "a" {
		t.Fatalf("all locked: %q", p.current())
	}

	// the second `a` opens with the known repo list: cursor on lib too
	fb := &fakeBackend{tasks: sampleTasks(), repos: []string{"api", "docs", "lib", "web"}}
	m, _ := newTest(t, fb)
	key(m, "a")
	key(m, "esc")
	_, cmd := m.Update(keyMsg("a")) // repos refresh not yet delivered
	if m.picker == nil || m.picker.current() != "lib" {
		t.Fatalf("known repos: cursor not on lib")
	}
	drain(m, cmd)
	if m.picker.current() != "lib" {
		t.Fatalf("after refresh: cursor on %q", m.picker.current())
	}
}

// Project repos that arrive while the form's picker is open are
// preselected in the picker too, so confirming keeps them.
func TestProjectReposWhilePickerOpen(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks(), projects: []string{"shop"}, repos: []string{"api", "docs", "lib", "web"},
		projRepos: map[string][]app.RepoArg{"shop": {{Name: "api", Mode: model.ModeRW}, {Name: "lib", Mode: model.ModeRO}}}}
	m, _ := newTest(t, fb)
	key(m, "n")
	typeText(m, "T")
	key(m, "down")
	// type the project but hold back the repos load
	var load tea.Cmd
	for _, r := range "shop" {
		_, c := m.Update(keyMsg(string(r)))
		load = c
	}
	key(m, "down") // Repos
	key(m, " ")    // picker opens before the load completes
	if m.mode != modePicker {
		t.Fatalf("mode %d", m.mode)
	}
	key(m, "end")
	key(m, " ") // web rw
	drain(m, load)
	if got := summary(m.picker.sel); got != "api(rw) lib(ro) web(rw)" {
		t.Fatalf("picker selection %q", got)
	}
	key(m, "enter")
	if got := summary(m.form.repos); got != "api(rw) lib(ro) web(rw)" || m.form.preProject != "shop" {
		t.Fatalf("form repos after confirm %q (pre %q)", got, m.form.preProject)
	}
}

func TestNoteDisplay(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks(), notes: map[string]*model.Note{
		"alpha": {ID: 1, Body: "line one\nline\x1b[31m two " + strings.Repeat("long ", 40), Author: model.AuthorUser, CreatedAt: t0.Add(30 * time.Minute)},
	}}
	m, _ := newTest(t, fb)
	selectTask(t, m, "alpha")
	v := plainView(m)
	var line string
	for _, l := range strings.Split(v, "\n") {
		if strings.Contains(l, "note:") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no note line:\n%s", v)
	}
	if !strings.Contains(line, "note:     line one line [31m two long") || !strings.Contains(line, "…  30m ago") {
		t.Fatalf("note line %q", line)
	}
	if strings.Contains(m.View(), "\x1b[31m two") {
		t.Fatal("unsanitized note rendered")
	}
	for _, l := range strings.Split(v, "\n") {
		if ansi.StringWidth(l) > 120 {
			t.Fatalf("line wider than terminal: %q", l)
		}
	}

	// assistant notes are marked; tasks without notes show none
	fb.notes["alpha"] = &model.Note{ID: 2, Body: "short", Author: model.AuthorAssistant, CreatedAt: t0.Add(59 * time.Minute)}
	m.Update(listTickMsg{})
	drain(m, m.reload())
	if v := plainView(m); !strings.Contains(v, "note:     short  1m ago (assistant)") {
		t.Fatalf("assistant note:\n%s", v)
	}
	selectTask(t, m, "beta")
	if v := plainView(m); strings.Contains(v, "note:") {
		t.Fatalf("note shown for a task without notes:\n%s", v)
	}
}

func TestNoteKey(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks()}
	m, _ := newTest(t, fb)
	selectTask(t, m, "gamma")
	if !strings.Contains(plainView(m), "m note") {
		t.Fatal("help lacks m")
	}

	// esc cancels
	key(m, "m")
	if m.mode != modeNote {
		t.Fatalf("mode = %v", m.mode)
	}
	typeText(m, "nope")
	key(m, "esc")
	if m.mode != modeNormal || fb.count("AddNote") != 0 || !strings.Contains(m.status, "cancelled") {
		t.Fatalf("esc: mode %v calls %d status %q", m.mode, fb.count("AddNote"), m.status)
	}

	// enter saves (async, busy while running), typed keys don't trigger commands
	key(m, "m")
	if v := plainView(m); !strings.Contains(v, "note>") || !strings.Contains(v, "enter save") {
		t.Fatalf("note input not shown:\n%s", v)
	}
	typeText(m, "d s q waiting on review")
	if m.mode != modeNote || m.confirm != nil || fb.count("NewSession") != 0 {
		t.Fatal("keys leaked to normal mode")
	}
	cmd := press(m, "enter")
	if m.mode != modeNormal || m.busy[3] != "note gamma" {
		t.Fatalf("after enter: mode %v busy %v", m.mode, m.busy)
	}
	if !strings.Contains(plainView(m), "working… note gamma") {
		t.Fatalf("busy not shown:\n%s", plainView(m))
	}
	drain(m, cmd)
	c := fb.last("AddNote")
	if c == nil || c.args[0] != "gamma" || c.args[1] != "d s q waiting on review" || c.args[2] != model.AuthorUser {
		t.Fatalf("AddNote call %+v", c)
	}
	if len(m.busy) != 0 || m.status != "note added to gamma" {
		t.Fatalf("busy %v status %q", m.busy, m.status)
	}
	if v := plainView(m); !strings.Contains(v, "note:     d s q waiting on review  5m ago") {
		t.Fatalf("new note not shown after reload:\n%s", v)
	}

	// empty note is not saved; reopened input starts empty
	key(m, "m")
	if m.noteInput.Value() != "" {
		t.Fatalf("input not reset: %q", m.noteInput.Value())
	}
	// resizing while typing resizes the input too
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	if want := 80 - len(m.noteInput.Prompt) - 1; m.noteInput.Width != want {
		t.Fatalf("note input width after resize = %d, want %d", m.noteInput.Width, want)
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	key(m, " ")
	key(m, "enter")
	if fb.count("AddNote") != 1 || !strings.Contains(m.status, "empty") {
		t.Fatalf("empty note: calls %d status %q", fb.count("AddNote"), m.status)
	}

	// errors land in the status line
	fb.noteErr = errors.New("database is locked")
	key(m, "m")
	typeText(m, "x")
	key(m, "enter")
	if !m.statusErr || !strings.Contains(m.status, "note: database is locked") {
		t.Fatalf("error status %q", m.status)
	}

	// busy task: m refused
	m.busy[3] = "pr gamma"
	key(m, "m")
	if m.mode != modeNormal || !strings.Contains(m.status, "busy") {
		t.Fatalf("busy: mode %v status %q", m.mode, m.status)
	}
}

// selectTask selects the task with slug and loads its detail (as the
// debounced selection fetch would).
func selectTask(t *testing.T, m *Model, slug string) {
	t.Helper()
	for i := 0; i < len(m.rows) && (m.selected() == nil || m.selected().Task.Slug != slug); i++ {
		key(m, "k")
	}
	for i := 0; i < len(m.rows) && (m.selected() == nil || m.selected().Task.Slug != slug); i++ {
		key(m, "j")
	}
	sel := m.selected()
	if sel == nil || sel.Task.Slug != slug {
		t.Fatalf("cannot select %s (rows %v)", slug, rowSlugs(m))
	}
	drain(m, m.detailCmd(sel.Task.ID, slug))
}

func TestAssistantKey(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks()}
	m, _ := newTest(t, fb)
	selectTask(t, m, "gamma")
	if !strings.Contains(plainView(m), "A assistant") {
		t.Fatal("help lacks A")
	}
	cmd := press(m, "A")
	if cmd == nil || !m.assisting {
		t.Fatalf("A: cmd=%v assisting=%v", cmd, m.assisting)
	}
	// a second A while the first is in flight is refused
	if press(m, "A") != nil || !strings.Contains(m.status, "busy") {
		t.Fatalf("second A not refused: %q", m.status)
	}
	msg := cmd()
	if c := fb.last("Assistant"); c == nil || c.args[0] != "gamma" || fb.count("Assistant") != 1 {
		t.Fatalf("Assistant calls: %+v", fb.calls)
	}
	_, next := m.Update(msg)
	if m.assisting || next == nil {
		t.Fatalf("after assistantMsg: assisting=%v next=%v", m.assisting, next)
	}
	if got := fmt.Sprintf("%T", next()); !strings.Contains(got, "execMsg") {
		t.Fatalf("A did not exec the attach command: %s", got)
	}

	if strings.Contains(m.status, "paste") {
		t.Fatalf("hint without one: %q", m.status)
	}

	// a running assistant's paste hint lands in the status line
	fbh := &fakeBackend{tasks: sampleTasks(), assistHint: "assistant already running: press Ctrl-b ] to paste"}
	mh, _ := newTest(t, fbh)
	selectTask(t, mh, "gamma")
	hc := press(mh, "A")
	if _, next := mh.Update(hc()); next == nil || !strings.Contains(mh.status, "Ctrl-b ] to paste") || mh.statusErr {
		t.Fatalf("hint status %q err=%v next=%v", mh.status, mh.statusErr, next)
	}

	// no task selected: the assistant opens without a task
	fb2 := &fakeBackend{}
	m2, _ := newTest(t, fb2)
	key(m2, "A")
	if c := fb2.last("Assistant"); c == nil || c.args[0] != "" {
		t.Fatalf("Assistant without selection: %+v", fb2.calls)
	}

	// errors land in the status line
	fb3 := &fakeBackend{tasks: sampleTasks(), assistErr: errors.New("tmux exploded")}
	m3, _ := newTest(t, fb3)
	key(m3, "A")
	if !strings.Contains(m3.status, "assistant: tmux exploded") || m3.assisting {
		t.Fatalf("status %q assisting=%v", m3.status, m3.assisting)
	}
}

func sess(id int64, st model.SessionState) model.Session {
	return model.Session{ID: id, State: st, LastEventAt: t0}
}

func TestAggregateDot(t *testing.T) {
	cases := []struct {
		name  string
		ss    []model.Session
		want  dotKind
		glyph string
		style lipgloss.Style
	}{
		{"none", nil, dotNone, " ", lipgloss.NewStyle()},
		{"ended only", []model.Session{sess(1, model.StateEnded), sess(2, model.StateEnded)}, dotNone, " ", lipgloss.NewStyle()},
		{"stopped", []model.Session{sess(1, model.StateEnded), sess(2, model.StateStopped)}, dotStopped, "○", stopStyle},
		{"idle beats stopped", []model.Session{sess(1, model.StateStopped), sess(2, model.StateIdle)}, dotIdle, "●", idleStyle},
		{"running beats idle", []model.Session{sess(1, model.StateIdle), sess(2, model.StateRunning), sess(3, model.StateStopped)}, dotRunning, "●", runStyle},
		{"waiting beats all", []model.Session{sess(1, model.StateRunning), sess(2, model.StateWaiting), sess(3, model.StateIdle)}, dotWaiting, "●", waitStyle},
	}
	for _, c := range cases {
		k := aggregate(c.ss)
		if k != c.want {
			t.Errorf("%s: aggregate %d, want %d", c.name, k, c.want)
		}
		g, st := dotFor(k)
		if g != c.glyph || st.GetForeground() != c.style.GetForeground() || st.GetBold() != c.style.GetBold() {
			t.Errorf("%s: dot %q fg %v bold %v", c.name, g, st.GetForeground(), st.GetBold())
		}
		if ansi.StringWidth(g) != 1 {
			t.Errorf("%s: glyph width %d", c.name, ansi.StringWidth(g))
		}
	}
	if waitStyle.GetForeground() != lipgloss.TerminalColor(accent) || !waitStyle.GetBold() {
		t.Error("waiting dot is not accent + bold")
	}
	if runStyle.GetForeground() != lipgloss.TerminalColor(okColor) {
		t.Error("running dot is not green")
	}
}

// listLine returns the plain list line of task slug (left pane).
func listLine(t *testing.T, m *Model, slug string) string {
	t.Helper()
	for _, l := range strings.Split(plainView(m), "\n") {
		left := strings.SplitN(l, "│", 2)[0]
		if strings.Contains(left, slug) && !strings.Contains(left, "slug") {
			return left
		}
	}
	t.Fatalf("no list line for %s:\n%s", slug, plainView(m))
	return ""
}

// selectSlug selects task slug directly (no debounce).
func selectSlug(t *testing.T, m *Model, slug string) {
	t.Helper()
	for i, r := range m.rows {
		if r.isTask() && r.task.Task.Slug == slug {
			m.selIdx, m.selID = i, r.task.Task.ID
			return
		}
	}
	t.Fatalf("no row for %s", slug)
}

func TestRowDots(t *testing.T) {
	fb := &fakeBackend{tasks: []app.TaskView{
		tv(1, "ended", model.StatusActive, sess(1, model.StateEnded)),
		tv(2, "empty", model.StatusActive),
		tv(3, "waits", model.StatusActive, sess(2, model.StateRunning), sess(3, model.StateWaiting)),
		tv(4, "runs", model.StatusActive, sess(4, model.StateIdle), sess(5, model.StateRunning)),
		tv(5, "idles", model.StatusActive, sess(6, model.StateIdle), sess(7, model.StateStopped)),
		tv(6, "stops", model.StatusActive, sess(8, model.StateStopped), sess(9, model.StateEnded)),
	}}
	m, _ := newTest(t, fb)
	want := map[string]string{"ended": "  ended", "empty": "  empty", "waits": "● waits", "runs": "● runs", "idles": "● idles", "stops": "○ stops"}
	for slug, prefix := range want {
		for _, sel := range []bool{false, true} {
			if sel {
				selectSlug(t, m, slug)
			}
			if l := listLine(t, m, slug); !strings.HasPrefix(l, prefix+"  Title of "+slug) {
				t.Errorf("%s (sel %v): %q, want prefix %q", slug, sel, l, prefix)
			}
		}
	}
}

func TestDirtyStar(t *testing.T) {
	fb := &fakeBackend{tasks: append(sampleTasks(),
		tv(5, "shut", model.StatusDone), tv(6, "busy", model.StatusActive, sess(1, model.StateRunning))),
		dirty: map[int64]bool{1: true, 3: true, 5: true, 6: true}}
	m, _ := newTest(t, fb)
	if fb.count("DirtyTasks") != 1 {
		t.Fatalf("DirtyTasks at start: %d", fb.count("DirtyTasks"))
	}
	for slug, want := range map[string]string{
		"alpha": "  alpha*  Title of alpha",
		"beta":  "  beta  Title of beta",
		"gamma": "● gamma*  Title of gamma",
		"delta": "  delta  Title of delta",
		"shut":  "  shut  Title of shut", // closed: never starred
		"busy":  "● busy*  Title of busy",
	} {
		for _, sel := range []bool{false, true} {
			if sel {
				selectSlug(t, m, slug)
			}
			if l := listLine(t, m, slug); !strings.HasPrefix(l, want) {
				t.Errorf("%s (sel %v): %q, want prefix %q", slug, sel, l, want)
			}
		}
	}
	// the star survives truncation of a long slug
	m.Update(tea.WindowSizeMsg{Width: 12, Height: 20})
	found := false
	for _, l := range strings.Split(plainView(m), "\n") {
		if strings.HasPrefix(l, "  alph") || strings.HasPrefix(l, "  al") {
			found = true
			if !strings.Contains(l, "*") || ansi.StringWidth(l) > 12 {
				t.Errorf("narrow alpha row %q", l)
			}
		}
	}
	if !found {
		t.Fatalf("no alpha row:\n%s", plainView(m))
	}
}

func TestDirtyTicks(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks(), dirty: map[int64]bool{1: true}}
	m, sched := newTest(t, fb)
	countTicks := func() int {
		n := 0
		for _, msg := range *sched {
			if _, ok := msg.(dirtyTickMsg); ok {
				n++
			}
		}
		return n
	}
	if countTicks() != 1 || fb.count("DirtyTasks") != 1 || !m.dirty[1] || m.dirtying {
		t.Fatalf("start: ticks %d calls %d dirty %v", countTicks(), fb.count("DirtyTasks"), m.dirty)
	}
	// a tick reschedules itself once and starts one check
	*sched = nil
	fb.dirty = map[int64]bool{2: true}
	_, cmd := m.Update(dirtyTickMsg{})
	if countTicks() != 1 || len(*sched) != 1 || !m.dirtying {
		t.Fatalf("tick: sched %v dirtying %v", *sched, m.dirtying)
	}
	if fb.count("DirtyTasks") != 1 {
		t.Fatal("tick blocked on DirtyTasks")
	}
	if !m.dirty[1] {
		t.Fatal("last result dropped before the new one arrived")
	}
	// a second tick while the check is in flight starts nothing new
	*sched = nil
	_, cmd2 := m.Update(dirtyTickMsg{})
	drain(m, cmd2)
	if countTicks() != 1 || fb.count("DirtyTasks") != 1 {
		t.Fatalf("in-flight tick: ticks %d calls %d", countTicks(), fb.count("DirtyTasks"))
	}
	drain(m, cmd)
	if fb.count("DirtyTasks") != 2 || m.dirtying || m.dirty[1] || !m.dirty[2] {
		t.Fatalf("result not applied: %v", m.dirty)
	}
	if !strings.Contains(listLine(t, m, "beta"), "beta*") || strings.Contains(listLine(t, m, "alpha"), "alpha*") {
		t.Fatalf("view:\n%s", plainView(m))
	}
	// an error keeps the last result and is reported once
	fb.dirtyErr = errors.New("boom")
	_, cmd = m.Update(dirtyTickMsg{})
	drain(m, cmd)
	if !m.dirty[2] || !m.statusErr || !strings.Contains(m.status, "boom") {
		t.Fatalf("error: dirty %v status %q", m.dirty, m.status)
	}
	m.status, m.statusErr = "", false
	_, cmd = m.Update(dirtyTickMsg{})
	drain(m, cmd)
	if m.status != "" {
		t.Fatalf("same error shown again: %q", m.status)
	}
	// list ticks do not start dirty checks
	n := fb.count("DirtyTasks")
	_, cmd = m.Update(listTickMsg{})
	drain(m, cmd)
	if fb.count("DirtyTasks") != n {
		t.Fatal("list tick ran DirtyTasks")
	}
}

func TestLegend(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks()}
	m, _ := newTest(t, fb)
	m.Update(tea.WindowSizeMsg{Width: 200, Height: 30})
	lines := strings.Split(plainView(m), "\n")
	if help := lines[len(lines)-1]; !strings.Contains(help, "● waiting/running/idle  * uncommitted") || ansi.StringWidth(help) > 200 {
		t.Fatalf("wide help %q", help)
	}
	for _, w := range []int{120, 80, 30} {
		m.Update(tea.WindowSizeMsg{Width: w, Height: 30})
		lines = strings.Split(plainView(m), "\n")
		if help := lines[len(lines)-1]; strings.Contains(help, "uncommitted") {
			t.Fatalf("legend at width %d: %q", w, help)
		}
	}
}

func TestDotsSmallSizes(t *testing.T) {
	fb := &fakeBackend{tasks: []app.TaskView{
		tv(1, "a-very-long-slug-that-will-not-fit", model.StatusActive, sess(1, model.StateWaiting)),
		tv(2, "b", model.StatusActive, sess(2, model.StateStopped)),
		tv(3, "c", model.StatusReview),
		tv(4, "d", model.StatusDone, sess(3, model.StateRunning)),
	}, dirty: map[int64]bool{1: true, 2: true, 3: true, 4: true}}
	m, _ := newTest(t, fb)
	m.busy[1] = "pr"
	for w := 0; w <= 50; w++ {
		for _, h := range []int{0, 1, 2, 3, 6} {
			m.Update(tea.WindowSizeMsg{Width: w, Height: h})
			for _, slug := range []string{"a-very-long-slug-that-will-not-fit", "b", "d"} {
				selectSlug(t, m, slug)
				for _, l := range strings.Split(m.View(), "\n") {
					if ansi.StringWidth(l) > w {
						t.Fatalf("%dx%d: line too wide: %q", w, h, ansi.Strip(l))
					}
				}
			}
		}
	}
}
