package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/andreujuanc/atelier/internal/app"
	"github.com/andreujuanc/atelier/internal/model"
)

// forceGrace is how long a forced quit waits for cancelled operations.
const forceGrace = 5 * time.Second

// ErrSwitchWorkspace is returned by RunContext when the user left the TUI
// for the workspace selector (w), every operation having ended.
var ErrSwitchWorkspace = errors.New("switch workspace")

// Run starts the TUI on the terminal with the alternate screen.
func Run(a *app.App) error { return RunContext(context.Background(), a) }

// RunContext is Run with a context that stops the TUI when cancelled.
//
// Operations run with their own context, derived from ctx, which is only
// cancelled once they have returned — or on a forced quit (second ctrl+c),
// after which they get forceGrace to wind down. App.Out is restored only
// when no operation can still write to it.
func RunContext(ctx context.Context, a *app.App) error {
	opsCtx, cancelOps := context.WithCancel(ctx)
	defer cancelOps()
	tr := &tracker{}
	m := New(opsCtx, &trackedBackend{Backend: NewBackend(a), tr: tr})
	if a.Workspace != nil && a.Workspace.Workspace != nil {
		m.workspace = a.Workspace.Name
	}
	m.updater = NewUpdater()
	bin, err := os.Executable()
	if err != nil || bin == "" {
		bin = a.AtlBin
	}
	m.diffCmd = func(slug string) *exec.Cmd {
		c := exec.Command(bin, "diff", slug)
		if os.Getenv("LESS") == "" {
			// No -F: a short diff must not flash by and return to the TUI.
			c.Env = append(os.Environ(), "LESS=R")
		}
		return c
	}
	p := tea.NewProgram(m, tea.WithContext(ctx))

	// Progress and warnings from the app would corrupt the screen: route
	// them, in order, to the status line instead. Once the program has
	// stopped they go to the previous writer.
	old := a.Out
	fallback := old
	if fallback == nil {
		fallback = io.Discard
	}
	send, flush, stop := orderedSend(func(s string) { p.Send(warnMsg(s)) }, fallback)
	a.Out = &lineWriter{send: send}
	m.flushOut = flush

	_, err = p.Run()
	stop()
	force := m.Forced() || errors.Is(err, tea.ErrInterrupted) || errors.Is(err, tea.ErrProgramKilled)
	if shutdown(ctx, cancelOps, tr, force, forceGrace) {
		a.Out = old
	}
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil
	}
	if err == nil && m.SwitchRequested() && ctx.Err() == nil {
		return ErrSwitchWorkspace
	}
	if err == nil && m.RestartRequested() && ctx.Err() == nil {
		return ErrRestart
	}
	return err
}

// orderedSend returns send, which hands lines to deliver one at a time and
// in order from a single goroutine, flush, which returns once every line
// sent before it has been delivered (or stop was called), and stop. Lines
// still queued when stop is called, and those sent after it, are written
// to fallback instead, in order, and send then never blocks; deliver must
// return once the consumer is gone (tea.Program.Send does after the
// program stops).
func orderedSend(deliver func(string), fallback io.Writer) (send func(string), flush func(), stop func()) {
	type item struct {
		line    string
		barrier chan struct{} // non-nil: a flush marker, closed when reached
	}
	const maxQueued = 64 // send waits while this many items are queued
	var (
		mu      sync.Mutex // guards queue and stopped; held while writing to fallback
		changed = sync.NewCond(&mu)
		queue   []item
		stopped bool
	)
	// put queues it, unless stop was called; with mu held.
	put := func(it item) bool {
		for len(queue) >= maxQueued && !stopped {
			changed.Wait()
		}
		if stopped {
			return false
		}
		queue = append(queue, it)
		changed.Broadcast()
		return true
	}
	go func() {
		for {
			mu.Lock()
			for len(queue) == 0 && !stopped {
				changed.Wait()
			}
			if stopped {
				mu.Unlock()
				return
			}
			it := queue[0]
			queue = queue[1:]
			changed.Broadcast()
			mu.Unlock()
			if it.barrier != nil {
				close(it.barrier)
			} else {
				deliver(it.line)
			}
		}
	}()
	send = func(s string) {
		mu.Lock()
		defer mu.Unlock()
		if !put(item{line: s}) {
			fmt.Fprintln(fallback, s)
		}
	}
	flush = func() {
		b := make(chan struct{})
		mu.Lock()
		queued := put(item{barrier: b})
		mu.Unlock()
		if queued {
			<-b // closed when reached, or by stop
		}
	}
	stop = func() {
		mu.Lock()
		defer mu.Unlock()
		if stopped {
			return
		}
		stopped = true
		for _, it := range queue {
			if it.barrier != nil {
				close(it.barrier)
			} else {
				fmt.Fprintln(fallback, it.line)
			}
		}
		queue = nil
		changed.Broadcast()
	}
	return send, flush, stop
}

// shutdown closes tr and waits for the tracked backend calls. Unless force,
// their context stays live until they all return (or parent is
// cancelled); then cancel is called and they get up to grace more. It
// reports whether every call returned.
func shutdown(parent context.Context, cancel context.CancelFunc, tr *tracker, force bool, grace time.Duration) bool {
	tr.close()
	done := make(chan struct{})
	go func() {
		tr.wg.Wait()
		close(done)
	}()
	if !force {
		select {
		case <-done:
		case <-parent.Done():
		}
	}
	cancel()
	select {
	case <-done:
		return true
	case <-time.After(grace):
		return false
	}
}

// errStopped is returned by backend calls started after the TUI stopped.
var errStopped = errors.New("tui stopped")

// tracker counts backend calls in flight; after close no new call starts,
// so waiting on wg is race-free.
type tracker struct {
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

func (t *tracker) begin() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	t.wg.Add(1)
	return true
}

func (t *tracker) end() { t.wg.Done() }

// stopped reports whether the tracker was closed.
func (t *tracker) stopped() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

func (t *tracker) close() {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
}

// trackedBackend records every call (they may write to App.Out) in tr.
type trackedBackend struct {
	Backend
	tr *tracker
}

func (b *trackedBackend) List(ctx context.Context) ([]app.TaskView, error) {
	if !b.tr.begin() {
		return nil, errStopped
	}
	defer b.tr.end()
	return b.Backend.List(ctx)
}

func (b *trackedBackend) Get(ctx context.Context, ref string) (*app.TaskView, error) {
	if !b.tr.begin() {
		return nil, errStopped
	}
	defer b.tr.end()
	return b.Backend.Get(ctx, ref)
}

func (b *trackedBackend) NewTask(ctx context.Context, o app.NewTaskOpts) (*model.Task, error) {
	if !b.tr.begin() {
		return nil, errStopped
	}
	defer b.tr.end()
	return b.Backend.NewTask(ctx, o)
}

func (b *trackedBackend) OpenOrChoose(ctx context.Context, ref string) (*exec.Cmd, []app.SessionChoice, error) {
	if !b.tr.begin() {
		return nil, nil, errStopped
	}
	defer b.tr.end()
	return b.Backend.OpenOrChoose(ctx, ref)
}

func (b *trackedBackend) OpenSession(ctx context.Context, ref string, sessionID int64) (*exec.Cmd, error) {
	if !b.tr.begin() {
		return nil, errStopped
	}
	defer b.tr.end()
	return b.Backend.OpenSession(ctx, ref, sessionID)
}

func (b *trackedBackend) OpenNew(ctx context.Context, ref string) (*exec.Cmd, error) {
	if !b.tr.begin() {
		return nil, errStopped
	}
	defer b.tr.end()
	return b.Backend.OpenNew(ctx, ref)
}

func (b *trackedBackend) NewSession(ctx context.Context, ref string) (*model.Session, error) {
	if !b.tr.begin() {
		return nil, errStopped
	}
	defer b.tr.end()
	return b.Backend.NewSession(ctx, ref)
}

func (b *trackedBackend) Resume(ctx context.Context, ref string) (int, error) {
	if !b.tr.begin() {
		return 0, errStopped
	}
	defer b.tr.end()
	return b.Backend.Resume(ctx, ref)
}

func (b *trackedBackend) SetStatus(ctx context.Context, ref string, st model.Status) error {
	if !b.tr.begin() {
		return errStopped
	}
	defer b.tr.end()
	return b.Backend.SetStatus(ctx, ref, st)
}

func (b *trackedBackend) Done(ctx context.Context, ref string, force bool) (*model.Task, error) {
	if !b.tr.begin() {
		return nil, errStopped
	}
	defer b.tr.end()
	return b.Backend.Done(ctx, ref, force)
}

func (b *trackedBackend) Cancel(ctx context.Context, ref string, force bool) (*model.Task, error) {
	if !b.tr.begin() {
		return nil, errStopped
	}
	defer b.tr.end()
	return b.Backend.Cancel(ctx, ref, force)
}

func (b *trackedBackend) PR(ctx context.Context, ref string) ([]app.PRResult, error) {
	if !b.tr.begin() {
		return nil, errStopped
	}
	defer b.tr.end()
	return b.Backend.PR(ctx, ref)
}

func (b *trackedBackend) Recover(ctx context.Context) (int, error) {
	if !b.tr.begin() {
		return 0, errStopped
	}
	defer b.tr.end()
	return b.Backend.Recover(ctx)
}

func (b *trackedBackend) ProjectNames() ([]string, error) {
	if !b.tr.begin() {
		return nil, errStopped
	}
	defer b.tr.end()
	return b.Backend.ProjectNames()
}

func (b *trackedBackend) ProjectRepos(name string) ([]app.RepoArg, error) {
	if !b.tr.begin() {
		return nil, errStopped
	}
	defer b.tr.end()
	return b.Backend.ProjectRepos(name)
}

func (b *trackedBackend) AvailableRepos(ctx context.Context) ([]string, error) {
	if !b.tr.begin() {
		return nil, errStopped
	}
	defer b.tr.end()
	return b.Backend.AvailableRepos(ctx)
}

func (b *trackedBackend) AddRepos(ctx context.Context, ref string, repos []app.RepoArg, o app.AddOpts) (*app.AddResult, error) {
	if !b.tr.begin() {
		return nil, errStopped
	}
	defer b.tr.end()
	return b.Backend.AddRepos(ctx, ref, repos, o)
}

func (b *trackedBackend) Assistant(ctx context.Context, slug string) (*exec.Cmd, string, error) {
	if !b.tr.begin() {
		return nil, "", errStopped
	}
	defer b.tr.end()
	return b.Backend.Assistant(ctx, slug)
}

func (b *trackedBackend) GetNotes(ctx context.Context, ref string) (string, error) {
	if !b.tr.begin() {
		return "", errStopped
	}
	defer b.tr.end()
	return b.Backend.GetNotes(ctx, ref)
}

func (b *trackedBackend) SetNotes(ctx context.Context, ref, text, base string) (string, error) {
	if !b.tr.begin() {
		return "", errStopped
	}
	defer b.tr.end()
	return b.Backend.SetNotes(ctx, ref, text, base)
}

// lineWriter calls send for each complete non-empty line written, in
// order (send is called with the writer's lock held).
type lineWriter struct {
	mu   sync.Mutex
	buf  []byte
	send func(string)
}

var _ io.Writer = (*lineWriter)(nil)

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := string(bytes.TrimSpace(w.buf[:i]))
		w.buf = w.buf[i+1:]
		if line != "" {
			w.send(line)
		}
	}
	return len(p), nil
}

func (b *trackedBackend) AddCheck(ctx context.Context, ref string, repos []app.RepoArg) ([]string, error) {
	if !b.tr.begin() {
		return nil, errStopped
	}
	defer b.tr.end()
	return b.Backend.AddCheck(ctx, ref, repos)
}

func (b *trackedBackend) Rename(ctx context.Context, ref, title string) (*model.Task, error) {
	if !b.tr.begin() {
		return nil, errStopped
	}
	defer b.tr.end()
	return b.Backend.Rename(ctx, ref, title)
}

// DirtyTasks only reads (it never writes to App.Out) and may take up to
// dirtyTimeout: it is refused once the TUI stopped but not waited for, so
// a quit never hangs on it (shutdown cancels its context).
func (b *trackedBackend) DirtyTasks(ctx context.Context) (map[int64]bool, error) {
	if b.tr.stopped() {
		return nil, errStopped
	}
	return b.Backend.DirtyTasks(ctx)
}
