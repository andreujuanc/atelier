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
				rs := app.RepoStatus{Name: r.Name, Mode: r.Mode, Dirty: r.Name == "dirtyrepo"}
				if r.Name == "gonerepo" {
					rs.Err = "missing"
				}
				v.RepoStatus = append(v.RepoStatus, rs)
			}
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

func (f *fakeBackend) ProjectNames() ([]string, error) { return f.projects, nil }

func (f *fakeBackend) TaskDir(slug string) string { return "/tasks/" + slug }

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func tv(id int64, slug string, st model.Status, sessions ...model.Session) app.TaskView {
	return app.TaskView{
		Task: model.Task{
			ID: id, Slug: slug, Title: "Title of " + slug, Status: st, Branch: "atl/" + slug,
			PortOffset: int(id) * 100, Repos: []model.TaskRepo{{Name: "api", Mode: model.ModeRW}, {Name: "docs", Mode: model.ModeRO}},
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
	for _, s := range []string{"slug", "atl/gamma", "+300", "/tasks/gamma", "rw  api", "ro  docs"} {
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
	fb := &fakeBackend{tasks: sampleTasks(), projects: []string{"shop", "site"}}
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
	key(m, "enter")
	typeText(m, "api:rw docs:ro web")
	key(m, "enter")
	typeText(m, "the description")
	key(m, "ctrl+b")
	key(m, "enter") // focus backlog
	key(m, "enter") // submit
	c := fb.last("NewTask")
	if c == nil {
		t.Fatalf("no NewTask call; form err %q", m.form.err)
	}
	want := app.NewTaskOpts{
		Title: "Add login", Project: "site", Description: "the description", Backlog: true,
		Repos: []app.RepoArg{{Name: "api", Mode: model.ModeRW}, {Name: "docs", Mode: model.ModeRO}, {Name: "web", Mode: model.ModeRW}},
	}
	if got := c.args[0].(app.NewTaskOpts); !reflect.DeepEqual(got, want) {
		t.Fatalf("opts\n got %+v\nwant %+v", got, want)
	}
	if m.mode != modeNormal || !strings.Contains(m.status, "created add-login") {
		t.Fatalf("after submit: mode %d status %q", m.mode, m.status)
	}
}

func TestNewTaskFormValidation(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks()}
	m, _ := newTest(t, fb)
	key(m, "n")
	key(m, "down")
	key(m, "down")
	key(m, "down")
	key(m, "down")
	key(m, "enter") // submit with empty title
	if fb.count("NewTask") != 0 || !strings.Contains(m.form.err, "title") {
		t.Fatalf("empty title: err %q", m.form.err)
	}
	key(m, "up")
	key(m, "up")
	key(m, "up")
	key(m, "up")
	typeText(m, "T")
	key(m, "enter")
	key(m, "enter")
	typeText(m, "api:rx")
	key(m, "enter")
	key(m, "enter")
	key(m, "enter")
	if fb.count("NewTask") != 0 || m.mode != modeForm || !strings.Contains(plainView(m), "invalid mode") {
		t.Fatalf("invalid repos: calls %v err %q", fb.calls, m.form.err)
	}
	key(m, "esc")
	if m.mode != modeNormal || fb.count("NewTask") != 0 {
		t.Fatal("esc did not cancel")
	}
}

func TestParseRepos(t *testing.T) {
	got, err := parseRepos(" a:rw, b:ro  c ")
	if err != nil {
		t.Fatal(err)
	}
	want := []app.RepoArg{{Name: "a", Mode: model.ModeRW}, {Name: "b", Mode: model.ModeRO}, {Name: "c", Mode: model.ModeRW}}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	for _, bad := range []string{":rw", "a:x", "a/b", ".."} {
		if _, err := parseRepos(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if r, err := parseRepos(""); err != nil || r != nil {
		t.Fatal("empty")
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
	fb := &fakeBackend{tasks: sampleTasks(), projects: []string{"p"}}
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
	m, _ = newTest(t, fb)
	key(m, "n")
	typeText(m, "x")
	for i := 0; i < 4; i++ {
		key(m, "down")
	}
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
	send, stop := orderedSend(func(s string) {
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
	send, stop = orderedSend(func(string) { <-block }, &fallback)
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
