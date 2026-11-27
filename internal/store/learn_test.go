package store

import (
	"context"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
)

func TestLearnDigest(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task := mkTask(t, s, "fix", model.StatusDone, 0)
	if _, ok, err := s.GetLearnDigest(ctx, task.ID); ok || err != nil {
		t.Fatalf("none yet: %v %v", ok, err)
	}
	at := time.UnixMilli(1777629600000).UTC()
	d := LearnDigest{TaskID: task.ID, InputHash: "h1", CacheKey: "k1", OccurrencesJSON: `[]`, CreatedAt: at, CostUSD: 0.0125}
	if err := s.PutLearnDigest(ctx, d); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetLearnDigest(ctx, task.ID)
	if err != nil || !ok || got != d {
		t.Fatalf("got %+v %v %v, want %+v", got, ok, err, d)
	}
	d2 := LearnDigest{TaskID: task.ID, InputHash: "h2", CacheKey: "k2", OccurrencesJSON: `[{"text":"x"}]`}
	if err := s.PutLearnDigest(ctx, d2); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.GetLearnDigest(ctx, task.ID)
	if got.CacheKey != "k2" || got.InputHash != "h2" || got.OccurrencesJSON != d2.OccurrencesJSON || got.CostUSD != 0 || got.CreatedAt.IsZero() {
		t.Errorf("replaced: %+v", got)
	}
	if err := s.PutLearnDigest(ctx, LearnDigest{TaskID: 999, CacheKey: "k", OccurrencesJSON: "[]"}); err == nil {
		t.Error("a digest of no task")
	}
	if err := s.DeleteTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.GetLearnDigest(ctx, task.ID); ok || err != nil {
		t.Errorf("after the task's delete: %v %v", ok, err)
	}
}
