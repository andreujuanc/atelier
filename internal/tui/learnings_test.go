package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/andreujuanc/atelier/internal/app"
	"github.com/andreujuanc/atelier/internal/learn"
	"github.com/andreujuanc/atelier/internal/store"
)

func (f *fakeBackend) Learnings(ctx context.Context) ([]app.LearningView, error) {
	f.rec("Learnings")
	return append([]app.LearningView(nil), f.learnings...), nil
}

func (f *fakeBackend) Learning(ctx context.Context, id int64) (app.LearningView, []store.LearningOccurrence, error) {
	f.rec("Learning", id)
	for _, v := range f.learnings {
		if v.ID == id {
			return v, append([]store.LearningOccurrence(nil), f.occs[id]...), nil
		}
	}
	return app.LearningView{}, nil, fmt.Errorf("learning %d: %w", id, store.ErrNotFound)
}

func (f *fakeBackend) ProposedLearnings(ctx context.Context) (int, error) {
	f.countCalls++
	return f.proposedN, nil
}

func (f *fakeBackend) DecideLearning(ctx context.Context, id int64, accept bool) error {
	f.rec("DecideLearning", id, accept)
	if f.learnErr != nil {
		return f.learnErr
	}
	for i := range f.learnings {
		if f.learnings[i].ID == id {
			f.learnings[i].Status = map[bool]string{true: learn.StatusAccepted, false: learn.StatusRejected}[accept]
		}
	}
	return nil
}

// EditLearning checks the text as the app does.
func (f *fakeBackend) EditLearning(ctx context.Context, id int64, text string) (string, error) {
	f.rec("EditLearning", id, text)
	text, err := learn.CheckLearningText(text)
	if err != nil {
		return "", err
	}
	if f.learnErr != nil {
		return "", f.learnErr
	}
	for i := range f.learnings {
		if f.learnings[i].ID == id {
			f.learnings[i].Text, f.learnings[i].Edited = text, true
		}
	}
	return text, nil
}

func (f *fakeBackend) DeleteLearning(ctx context.Context, id int64) (int, error) {
	f.rec("DeleteLearning", id)
	if f.learnErr != nil {
		return 0, f.learnErr
	}
	for i := range f.learnings {
		if f.learnings[i].ID == id {
			f.learnings = append(f.learnings[:i], f.learnings[i+1:]...)
			break
		}
	}
	return len(f.occs[id]), nil
}

func (f *fakeBackend) LearnConfig() learn.Config {
	if f.learnCfg.PrecleanModel == "" {
		return learn.Defaults()
	}
	return f.learnCfg
}

func lv(id int64, status string, score float64, text string) app.LearningView {
	v := app.LearningView{}
	v.ID, v.Status, v.Text = id, status, text
	v.Score.Score, v.Score.Reinforce, v.Score.Tasks, v.Score.Last = score, score, 2, t0
	return v
}

func sampleLearnings() *fakeBackend {
	acc := lv(1, learn.StatusAccepted, 5, "Keep answers short")
	acc.Outdated = true
	acc.DecidedAt = t0
	return &fakeBackend{tasks: sampleTasks(), learnings: []app.LearningView{
		acc,
		lv(2, learn.StatusProposed, 3.5, "Ask before pushing"),
		lv(3, learn.StatusCandidate, 1, "Use tabs"),
		lv(4, learn.StatusRejected, 2, "Write long docs"),
		lv(5, learn.StatusProposed, 4, "Run the tests first"),
	}, occs: map[int64][]store.LearningOccurrence{
		2: {
			{TaskSlug: "alpha", At: t0.Add(-48 * time.Hour), Strength: "normal", Polarity: "reinforce", Quote: "please ask me before pushing anything"},
			{TaskSlug: "beta", At: t0, Strength: "strong", Polarity: "reinforce", Quote: "never do that without me"},
		},
	}}
}

// openLearn opens the learnings view on fb's learnings.
func openLearn(t *testing.T, fb *fakeBackend) *Model {
	t.Helper()
	m, _ := newTest(t, fb)
	key(m, "L")
	if m.mode != modeLearnings || m.learn == nil || !m.learn.loaded {
		t.Fatalf("L: mode %v view %+v", m.mode, m.learn)
	}
	return m
}

func learnIDs(m *Model) []int64 {
	var out []int64
	for _, v := range m.learn.items {
		out = append(out, v.ID)
	}
	return out
}

// TestLearningsView: L opens the list, proposed first (by score), then
// accepted; h toggles the candidates and the rejected; the selected
// learning's occurrences show newest first with the ? marker; esc and q
// go back.
func TestLearningsView(t *testing.T) {
	fb := sampleLearnings()
	m := openLearn(t, fb)
	if got := fmt.Sprint(learnIDs(m)); got != "[5 2 1]" {
		t.Fatalf("rows %s", got)
	}
	v := checkView(t, m, 120, 40)
	for _, want := range []string{"Learnings", "2 proposed · 1 accepted · 2 hidden (h)", "STATUS", "SCORE", "FLAGS",
		"proposed", "Run the tests first", "outdated", "Learning 5"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "Use tabs") || strings.Contains(v, "Write long docs") {
		t.Errorf("hidden statuses shown:\n%s", v)
	}
	if strings.Index(v, "Run the tests first") > strings.Index(v, "Ask before pushing") ||
		strings.Index(v, "Ask before pushing") > strings.Index(v, "Keep answers short") {
		t.Errorf("order:\n%s", v)
	}

	key(m, "j")
	if m.learn.selID != 2 || fb.last("Learning").args[0] != int64(2) {
		t.Fatalf("j: selected %d", m.learn.selID)
	}
	v = checkView(t, m, 120, 40)
	newest, older := strings.Index(v, "never do that without me"), strings.Index(v, "please ask me before pushing")
	if newest < 0 || older < 0 || newest > older {
		t.Fatalf("occurrences not newest first:\n%s", v)
	}
	if !strings.Contains(v, "beta  strong  reinforce  ?") || strings.Contains(v, "alpha  normal  reinforce  ?") {
		t.Errorf("? marks the wrong quote:\n%s", v)
	}
	if !strings.Contains(v, "1 quote(s) share no word with the text") || !strings.Contains(v, "Occurrences (2)") {
		t.Errorf("no ? legend:\n%s", v)
	}

	key(m, "h")
	if got := fmt.Sprint(learnIDs(m)); got != "[5 2 1 3 4]" || m.learn.selID != 2 {
		t.Fatalf("h: rows %s, selected %d", got, m.learn.selID)
	}
	v = checkView(t, m, 120, 40)
	if !strings.Contains(v, "Use tabs") || !strings.Contains(v, "h hide candidates/rejected") {
		t.Errorf("h: hidden not shown:\n%s", v)
	}
	key(m, "G") // not a key here
	key(m, "end")
	key(m, "h") // the selection (3) is hidden again: the nearest one
	if got := fmt.Sprint(learnIDs(m)); got != "[5 2 1]" || m.learn.selID != 1 {
		t.Fatalf("h again: rows %s, selected %d", got, m.learn.selID)
	}

	// Every size: no line wider than the screen, never more lines.
	for _, sz := range [][2]int{{1, 1}, {10, 5}, {30, 10}, {60, 14}, {80, 24}, {200, 60}} {
		m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		checkView(t, m, sz[0], sz[1])
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	key(m, "esc")
	if m.mode != modeNormal || m.learn != nil {
		t.Fatalf("esc: mode %v", m.mode)
	}
	key(m, "L")
	key(m, "q")
	if m.mode != modeNormal || m.learn != nil || m.quitRequested {
		t.Fatalf("q: mode %v quit %v", m.mode, m.quitRequested)
	}

	// No learnings: says how to make them.
	m = openLearn(t, &fakeBackend{tasks: sampleTasks()})
	if v := checkView(t, m, 120, 40); !strings.Contains(v, "no learnings yet: l runs atl learn") {
		t.Fatalf("empty view:\n%s", v)
	}
	key(m, "a")
	if m.status != "no learning selected" {
		t.Fatalf("a with nothing: %q", m.status)
	}
}

// TestLearningsDecide: a accepts at once; x and d ask first (d says the
// evidence comes back and reject is the way to stop it); each calls the
// backend and reloads; a refusal while atl learn runs goes to the status
// line.
func TestLearningsDecide(t *testing.T) {
	fb := sampleLearnings()
	m := openLearn(t, fb)
	loads := fb.count("Learnings")
	counts := fb.countCalls
	key(m, "a") // 5, proposed
	if c := fb.last("DecideLearning"); c == nil || c.args[0] != int64(5) || c.args[1] != true {
		t.Fatalf("a: %v", fb.calls)
	}
	if !m.statusOK || !strings.Contains(m.status, "learning 5 accepted") || fb.count("Learnings") == loads || fb.countCalls == counts {
		t.Fatalf("after accept: %q, reloaded %v", m.status, fb.count("Learnings") != loads)
	}
	// On to the next proposed one, not after 5 to the accepted.
	if m.learn.selID != 2 {
		t.Fatalf("after accept: selected %d, rows %v", m.learn.selID, learnIDs(m))
	}
	// 5 is accepted now: a on it is refused here.
	n := fb.count("DecideLearning")
	key(m, "end")
	if m.learn.selID != 5 {
		t.Fatalf("end: rows %v, selected %d", learnIDs(m), m.learn.selID)
	}
	key(m, "a")
	if fb.count("DecideLearning") != n || !m.statusErr || !strings.Contains(m.status, "only a proposed or candidate") {
		t.Fatalf("a on accepted: %q", m.status)
	}

	key(m, "x")
	if m.mode != modeAsk || m.ask == nil {
		t.Fatalf("x: mode %v", m.mode)
	}
	if v := checkView(t, m, 120, 40); !strings.Contains(v, "Reject learning") || !strings.Contains(v, "does not propose it again") {
		t.Fatalf("x dialog:\n%s", v)
	}
	key(m, "esc")
	if m.mode != modeLearnings || fb.count("DecideLearning") != n || m.status != "reject cancelled" {
		t.Fatalf("x esc: mode %v calls %d %q", m.mode, fb.count("DecideLearning"), m.status)
	}
	key(m, "x")
	key(m, "q") // not an answer
	if m.mode != modeAsk {
		t.Fatal("a stray key closed the question")
	}
	key(m, "y")
	if c := fb.last("DecideLearning"); fb.count("DecideLearning") != n+1 || c.args[1] != false || m.mode != modeLearnings {
		t.Fatalf("x y: %v mode %v", c, m.mode)
	}
	if m.learn.selID != 1 { // the previous accepted one: 5 was the last
		t.Fatalf("after reject: selected %d, rows %v", m.learn.selID, learnIDs(m))
	}

	sel := m.learn.selID
	key(m, "d")
	v := checkView(t, m, 120, 40)
	for _, want := range []string{"Delete learning", "next atl learn", "reject it (x) instead"} {
		if !strings.Contains(v, want) {
			t.Errorf("d dialog lacks %q:\n%s", want, v)
		}
	}
	for _, sz := range [][2]int{{1, 1}, {10, 5}, {30, 10}, {60, 14}, {200, 60}} {
		m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		checkView(t, m, sz[0], sz[1])
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	key(m, "enter")
	if c := fb.last("DeleteLearning"); c == nil || c.args[0] != sel || !strings.Contains(m.status, "deleted") {
		t.Fatalf("d enter: %v %q", fb.calls, m.status)
	}

	// While atl learn runs: refused, said on the status line.
	fb.learnErr = fmt.Errorf("wrapped: %w", app.ErrLearnRunning)
	key(m, "home")
	key(m, "a")
	if !m.statusErr || !strings.Contains(m.status, "atl learn is running in this workspace") {
		t.Fatalf("lock refusal: %q", m.status)
	}
	if v := checkView(t, m, 120, 40); !strings.Contains(v, "atl learn is running") {
		t.Fatalf("refusal not shown:\n%s", v)
	}
}

// TestLearningsEdit: e opens the edit box on the text; a refused text
// keeps the box with the reason; esc cancels; a good one saves and closes.
func TestLearningsEdit(t *testing.T) {
	fb := sampleLearnings()
	m := openLearn(t, fb)
	key(m, "e")
	if m.mode != modeLearnEdit || m.learn.edit == nil || m.learn.edit.ta.Value() != "Run the tests first" {
		t.Fatalf("e: mode %v", m.mode)
	}
	if v := checkView(t, m, 120, 40); !strings.Contains(v, "editing · enter save · esc cancel") {
		t.Fatalf("no edit box:\n%s", v)
	}
	key(m, "ctrl+s") // unchanged: nothing to save
	if m.mode != modeLearnings || fb.count("EditLearning") != 0 || m.status != "edit: text unchanged" {
		t.Fatalf("unchanged save: mode %v %q", m.mode, m.status)
	}

	key(m, "e")
	m.learn.edit.ta.SetValue("   ")
	key(m, "ctrl+s")
	if fb.count("EditLearning") != 1 || m.mode != modeLearnEdit || m.learn.edit == nil || m.learn.edit.err != "the text is empty" {
		t.Fatalf("empty: mode %v edit %+v", m.mode, m.learn.edit)
	}
	if v := checkView(t, m, 120, 40); !strings.Contains(v, "✗ the text is empty") {
		t.Fatalf("validation error not shown:\n%s", v)
	}
	typeText(m, "x")
	if m.learn.edit.err != "" {
		t.Fatal("the refusal stayed after an edit")
	}
	key(m, "esc")
	if m.mode != modeLearnings || m.learn.edit != nil || fb.count("EditLearning") != 1 {
		t.Fatalf("esc: mode %v", m.mode)
	}

	key(m, "e")
	m.learn.edit.ta.SetValue("")
	typeText(m, "Run the tests")
	m.Update(tea.PasteMsg{Content: " before\npushing"}) // on one line
	if got := m.learn.edit.ta.Value(); got != "Run the tests before pushing" {
		t.Fatalf("typed %q", got)
	}

	// ctrl+c with the edit unsaved asks first; another key goes back.
	key(m, "ctrl+c")
	if !m.learnAsking() || m.quitRequested || !strings.Contains(plainView(m), "quit and discard your edit to learning 5? y/n") {
		t.Fatalf("ctrl+c: asking %v quit %v\n%s", m.learnAsking(), m.quitRequested, plainView(m))
	}
	key(m, "n")
	if m.learnAsking() || m.mode != modeLearnEdit || m.learn.edit.ta.Value() != "Run the tests before pushing" {
		t.Fatalf("n: mode %v %q", m.mode, m.learn.edit.ta.Value())
	}
	key(m, "enter") // enter saves

	if c := fb.last("EditLearning"); c.args[1] != "Run the tests before pushing" || m.mode != modeLearnings || m.learn.edit != nil ||
		!m.statusOK {
		t.Fatalf("save: %v mode %v %q", c, m.mode, m.status)
	}
	if !strings.Contains(checkView(t, m, 120, 40), "Run the tests before pushing") {
		t.Fatal("not reloaded")
	}
}

// TestLearnKey: l asks first (naming the model), n leaves it; enter runs
// atl learn on the terminal; its end is reported and the count refreshed.
func TestLearnKey(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks()}
	m, _ := newTest(t, fb)
	runs := 0
	m.learnCmd = func() *exec.Cmd { runs++; return exec.Command("true") }
	key(m, "l")
	if m.mode != modeAsk {
		t.Fatalf("l: mode %v", m.mode)
	}
	v := checkView(t, m, 120, 40)
	for _, want := range []string{"Run atl learn", "It calls haiku", "costs money"} {
		if !strings.Contains(v, want) {
			t.Errorf("question lacks %q:\n%s", want, v)
		}
	}
	key(m, "n")
	if m.mode != modeNormal || runs != 0 {
		t.Fatalf("n: mode %v runs %d", m.mode, runs)
	}
	key(m, "l")
	cmd := press(m, "enter")
	if cmd == nil || runs != 1 || m.mode != modeNormal {
		t.Fatalf("enter: runs %d", runs)
	}
	if msg := cmd(); !strings.Contains(fmt.Sprintf("%T", msg), "execMsg") {
		t.Fatalf("not run on the terminal: %T", msg)
	}

	counts := fb.countCalls
	drain(m, func() tea.Msg {
		return execDoneMsg{op: "learn", stderr: bytes.NewBufferString("atl: another atl learn runs in this workspace (lock x): context deadline exceeded\n"),
			err: errors.New("exit status 1")}
	})
	if !m.statusErr || m.status != "learn: another atl learn runs in this workspace (lock x): context deadline exceeded" {
		t.Fatalf("failed run: %q", m.status)
	}
	fb.proposedN = 3
	drain(m, func() tea.Msg { return execDoneMsg{op: "learn", stderr: &bytes.Buffer{}} })
	if !m.statusOK || m.status != "atl learn finished" || fb.countCalls == counts || m.proposed != 3 {
		t.Fatalf("run: %q, proposed %d", m.status, m.proposed)
	}

	// From the learnings view too; the view comes back.
	key(m, "L")
	key(m, "l")
	if m.mode != modeAsk || m.ask.back != modeLearnings {
		t.Fatalf("l in the view: mode %v", m.mode)
	}
	key(m, "esc")
	if m.mode != modeLearnings {
		t.Fatalf("esc: mode %v", m.mode)
	}
}

// TestLearnExec: atl learn's output goes to the terminal, its stderr also
// to the tail kept; then it waits for enter.
func TestLearnExec(t *testing.T) {
	tail := &tailBuffer{max: 8}
	c := &learnExec{cmd: exec.Command("sh", "-c", "echo out; echo first >&2; echo lasterr >&2; exit 3"), stderr: tail}
	c.cmd.Stdin = strings.NewReader("") // as a terminal, which the command does not drain
	in := strings.NewReader("\nleft")
	var out, errOut bytes.Buffer
	c.SetStdin(in)
	c.SetStdout(&out)
	c.SetStderr(&errOut)
	err := c.Run()
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("err %v", err)
	}
	if !strings.HasPrefix(out.String(), "out\n") || !strings.Contains(out.String(), "atl learn failed (exit status 3). Press enter") {
		t.Fatalf("stdout %q", out.String())
	}
	if errOut.String() != "first\nlasterr\n" || tail.String() != "lasterr\n" {
		t.Fatalf("stderr %q, tail %q", errOut.String(), tail.String())
	}
	if in.Len() != len("left") {
		t.Fatalf("read past the newline: %d left", in.Len())
	}
}

// TestLearningsHint: the top bar says how many learnings wait for review,
// refreshed with the list.
func TestLearningsHint(t *testing.T) {
	fb := &fakeBackend{tasks: sampleTasks(), proposedN: 2}
	m, _ := newTest(t, fb)
	if v := checkView(t, m, 120, 40); !strings.Contains(strings.Split(v, "\n")[0], "2 learnings to review (L)") {
		t.Fatalf("no hint:\n%s", v)
	}
	fb.proposedN = 1
	drain(m, func() tea.Msg { return listTickMsg{} })
	if v := plainView(m); !strings.Contains(strings.Split(v, "\n")[0], "1 learning to review (L)") {
		t.Fatalf("hint not refreshed:\n%s", v)
	}
	fb.proposedN = 0
	drain(m, func() tea.Msg { return listTickMsg{} })
	if v := plainView(m); strings.Contains(v, "to review") {
		t.Fatalf("hint with nothing to review:\n%s", v)
	}
}

// TestLearnEditQuit: y to the ctrl+c question quits; without changes
// ctrl+c quits at once.
func TestLearnEditQuit(t *testing.T) {
	m := openLearn(t, sampleLearnings())
	key(m, "e")
	typeText(m, "!")
	key(m, "ctrl+c")
	_, cmd := m.Update(keyMsg("y"))
	if cmd == nil || m.mode != modeLearnings || m.learn.edit != nil {
		t.Fatalf("y: mode %v", m.mode)
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("y did not quit")
	}
	m = openLearn(t, sampleLearnings())
	key(m, "e")
	_, cmd = m.Update(keyMsg("ctrl+c"))
	if cmd == nil || m.learnAsking() {
		t.Fatal("ctrl+c without changes asked")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("ctrl+c without changes did not quit")
	}
}

// TestLearningsHelpAndPaging: ? shows the view's keys and any key goes
// back to the view; pgdn moves by the rows the list shows.
func TestLearningsHelpAndPaging(t *testing.T) {
	fb := sampleLearnings()
	for i := int64(10); i < 40; i++ {
		fb.learnings = append(fb.learnings, lv(i, learn.StatusAccepted, 1, fmt.Sprintf("learning number %d", i)))
	}
	m := openLearn(t, fb)
	key(m, "?")
	v := checkView(t, m, 120, 40)
	if m.mode != modeHelp || !strings.Contains(v, "delete (asks; atl learn may bring it back)") || strings.Contains(v, "new task") {
		t.Fatalf("?: mode %v\n%s", m.mode, v)
	}
	key(m, "a") // closes only
	if m.mode != modeLearnings || fb.count("DecideLearning") != 0 {
		t.Fatalf("after ?: mode %v", m.mode)
	}
	for _, sz := range [][2]int{{120, 40}, {60, 7}, {80, 12}} {
		m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		key(m, "home")
		key(m, "pgdown")
		l := m.layout()
		_, inner, _ := m.learnSplit(l.bodyH, learnBoxed(l))
		if want := inner - 1; m.learn.sel != want {
			t.Errorf("%dx%d: pgdn to row %d, want %d", sz[0], sz[1], m.learn.sel, want)
		}
		checkView(t, m, sz[0], sz[1])
	}
}

// TestLearnStatusPrefix: atl's own "atl: learn: " is not repeated.
func TestLearnStatusPrefix(t *testing.T) {
	m, _ := newTest(t, &fakeBackend{tasks: sampleTasks()})
	drain(m, func() tea.Msg {
		return execDoneMsg{op: "learn", stderr: bytes.NewBufferString("atl: learn: analyse failed: boom\n"), err: errors.New("exit status 1")}
	})
	if m.status != "learn: analyse failed: boom" {
		t.Fatalf("status %q", m.status)
	}
}

// fakeClock is a settable now for execState.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// stopped reports whether ctx is done within 50ms.
func stopped(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(50 * time.Millisecond):
		return false
	}
}

// TestSignalGuard: while a child runs on the terminal, its ctrl+c
// (SIGINT, and the cancelled signal context of atl) does not stop the
// TUI, nor when read up to execQuiet after it ended; otherwise both still
// do, and SIGTERM always does.
func TestSignalGuard(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1e9, 0)}
	parent, cancelParent := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 4)
	ctx, cancel, e := signalGuard(parent, sigs)
	defer cancel()
	e.now = clock.Now
	e.start()
	sigs <- os.Interrupt
	cancelParent()
	if stopped(ctx) {
		t.Fatal("stopped by a ctrl+c during an exec")
	}
	e.end()
	if stopped(ctx) {
		t.Fatal("stopped once the exec ended")
	}
	// The child exited before atl read the signal of its ctrl+c.
	clock.Add(execQuiet / 2)
	sigs <- os.Interrupt
	if stopped(ctx) {
		t.Fatal("stopped by the child's SIGINT read after it ended")
	}
	clock.Add(execQuiet)
	sigs <- os.Interrupt
	if !stopped(ctx) {
		t.Fatal("SIGINT outside an exec did not stop")
	}

	// The parent cancelled by the child's ctrl+c, seen only after it ended.
	parent, cancelParent = context.WithCancel(context.Background())
	ctx, cancel, e = signalGuard(parent, make(chan os.Signal, 4))
	defer cancel()
	e.now = clock.Now
	e.start()
	cancelParent()
	e.end()
	clock.Add(2 * execQuiet)
	if stopped(ctx) {
		t.Fatal("stopped by the child's cancellation of the parent")
	}

	parent, cancelParent = context.WithCancel(context.Background())
	ctx, cancel, _ = signalGuard(parent, sigs)
	defer cancel()
	cancelParent()
	if !stopped(ctx) {
		t.Fatal("parent cancelled outside an exec did not stop")
	}

	ctx, cancel, e = signalGuard(context.Background(), sigs)
	defer cancel()
	e.start()
	sigs <- syscall.SIGTERM
	if !stopped(ctx) {
		t.Fatal("SIGTERM during an exec did not stop")
	}
}

// TestExecStateEndDrains: the signals waiting when a child ends are its
// own, whenever they are read: end takes them (a SIGTERM among them still
// stops the TUI), with no reader of its own.
func TestExecStateEndDrains(t *testing.T) {
	for _, c := range []struct {
		sigs []os.Signal
		stop bool
	}{
		{[]os.Signal{os.Interrupt, os.Interrupt}, false},
		{[]os.Signal{os.Interrupt, syscall.SIGTERM}, true},
	} {
		sigs := make(chan os.Signal, 4)
		ctx, cancel := context.WithCancel(context.Background())
		e := &execState{sigs: sigs, parent: context.Background(), cancel: cancel, now: time.Now}
		e.start()
		for _, s := range c.sigs {
			sigs <- s
		}
		e.end()
		if len(sigs) != 0 || (ctx.Err() != nil) != c.stop {
			t.Errorf("%v: %d left, stopped %v", c.sigs, len(sigs), ctx.Err() != nil)
		}
		cancel()
	}
	// The parent cancelled when the child ends: spent, whenever its Done is
	// read.
	parent, cancelParent := context.WithCancel(context.Background())
	e := &execState{sigs: make(chan os.Signal), parent: parent, cancel: func() {}, now: time.Now}
	e.start()
	cancelParent()
	e.end()
	if !e.spent {
		t.Error("parent's cancellation during the exec not kept")
	}
}

// TestExecGuard: the child is marked from start (before the terminal is
// released) until done (once it is back), also when the exec failed to
// start; a nil state is not watched.
func TestExecGuard(t *testing.T) {
	e := &execState{sigs: make(chan os.Signal), parent: context.Background(), cancel: func() {}, now: time.Now}
	active := func() bool { e.mu.Lock(); defer e.mu.Unlock(); return e.active }
	var got error
	start, done := execGuard(e, func(err error) tea.Msg { got = err; return execDoneMsg{err: err} })
	if active() {
		t.Fatal("set before start")
	}
	start()
	if !active() {
		t.Fatal("not set during the exec")
	}
	if msg, ok := done(nil).(execDoneMsg); !ok || msg.err != nil || active() {
		t.Fatalf("done: %v, active %v", msg, active())
	}
	failed := errors.New("could not release the terminal")
	start()
	if done(failed); got != failed || active() {
		t.Fatalf("failed start: %v, active %v", got, active())
	}
	start, done = execGuard(nil, func(error) tea.Msg { return nil })
	start()
	done(nil)
}

// TestExecsAreGuarded: D (atl diff) and l (atl learn) go through
// guardedExec: execing is set when their command makes the exec message.
func TestExecsAreGuarded(t *testing.T) {
	execing := &execState{sigs: make(chan os.Signal), parent: context.Background(), cancel: func() {}, now: time.Now}
	m, _ := newTest(t, &fakeBackend{tasks: sampleTasks()})
	m.execing = execing
	m.diffCmd = func(string) *exec.Cmd { return exec.Command("true") }
	m.learnCmd = func() *exec.Cmd { return exec.Command("true") }
	diff := press(m, "D")
	key(m, "l")
	learnRun := press(m, "enter")
	for name, cmd := range map[string]tea.Cmd{"D": diff, "l": learnRun} {
		execing.active = false
		if cmd == nil {
			t.Fatalf("%s: no command", name)
		}
		cmd()
		if !execing.active {
			t.Errorf("%s: execing not set before the terminal is released", name)
		}
	}
}
