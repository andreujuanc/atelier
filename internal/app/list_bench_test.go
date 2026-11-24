package app

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/store"
)

// BenchmarkList is the TUI's once-a-second list: 50 tasks with 3 sessions each.
func BenchmarkList(b *testing.B) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(b.TempDir(), "atelier.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	for i := range 50 {
		t := &model.Task{Slug: fmt.Sprintf("t%d", i), Title: "T", Status: model.StatusActive, Branch: "atl/x",
			PortOffset: i * 100, Repos: []model.TaskRepo{{Name: "api", Mode: model.ModeRW}}}
		if err := st.CreateTask(ctx, t); err != nil {
			b.Fatal(err)
		}
		for range 3 {
			if err := st.CreateSession(ctx, &model.Session{TaskID: t.ID, State: model.StateIdle}); err != nil {
				b.Fatal(err)
			}
		}
	}
	a := &App{Paths: config.Paths{}, Store: st}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := a.List(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
