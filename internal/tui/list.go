package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/andreujuanc/atelier/internal/app"
	"github.com/andreujuanc/atelier/internal/model"
)

// closedLimit is how many done / cancelled tasks are shown (most recent).
const closedLimit = 10

// row is one line of the task list: a status header or a task.
type row struct {
	header bool
	status model.Status
	count  int // header: tasks shown
	total  int // header: tasks with this status (before the closed limit)
	task   *app.TaskView
}

func (r row) isTask() bool { return !r.header && r.task != nil }

// matchFilter reports whether t matches the case-insensitive substring f
// (on slug and title).
func matchFilter(t model.Task, f string) bool {
	f = strings.ToLower(strings.TrimSpace(f))
	if f == "" {
		return true
	}
	return strings.Contains(strings.ToLower(t.Slug), f) || strings.Contains(strings.ToLower(t.Title), f)
}

// buildRows groups tasks under status headers in model.AllStatuses order,
// applying the filter; done and cancelled keep only their closedLimit most
// recently updated tasks. Empty groups are omitted.
func buildRows(tasks []app.TaskView, filter string) []row {
	groups := map[model.Status][]*app.TaskView{}
	for i := range tasks {
		t := &tasks[i]
		if matchFilter(t.Task, filter) {
			groups[t.Task.Status] = append(groups[t.Task.Status], t)
		}
	}
	var rows []row
	for _, st := range model.AllStatuses {
		g := groups[st]
		if len(g) == 0 {
			continue
		}
		total := len(g)
		if !st.Open() {
			sort.SliceStable(g, func(i, j int) bool { return g[i].Task.UpdatedAt.After(g[j].Task.UpdatedAt) })
			if len(g) > closedLimit {
				g = g[:closedLimit]
			}
		}
		rows = append(rows, row{header: true, status: st, count: len(g), total: total})
		for _, t := range g {
			rows = append(rows, row{status: st, task: t})
		}
	}
	return rows
}

// dotKind is a task's aggregate session state, shown as the list row's dot.
// Higher values win: waiting > running > idle > stopped > none.
type dotKind int

const (
	dotNone    dotKind = iota // no sessions, or all ended
	dotStopped                // only stopped (and ended) sessions
	dotIdle
	dotRunning
	dotWaiting
)

// aggregate returns the highest-priority state of ss.
func aggregate(ss []model.Session) dotKind {
	k := dotNone
	for _, s := range ss {
		var sk dotKind
		switch s.State {
		case model.StateWaiting:
			sk = dotWaiting
		case model.StateRunning:
			sk = dotRunning
		case model.StateIdle:
			sk = dotIdle
		case model.StateStopped:
			sk = dotStopped
		}
		if sk > k {
			k = sk
		}
	}
	return k
}

// humanAge formats d as 5s, 3m, 2h or 4d.
func humanAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}
