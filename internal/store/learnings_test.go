package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
)

func TestLearnings(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	a := mkTask(t, s, "a", model.StatusDone, 0)
	b := mkTask(t, s, "b", model.StatusActive, 1)
	at := time.UnixMilli(1777629600000).UTC()
	occ := func(key string, task int64, learning int64) LearningOccurrence {
		return LearningOccurrence{Key: key, LearningID: learning, TaskID: task, Session: 3, At: at, Strength: "normal", Polarity: "reinforce",
			Quote: "keep it short please", Text: "Be brief"}
	}
	created, err := s.AssignOccurrences(ctx, []string{"Keep answers short", "Ask before pushing"},
		[]Assignment{{Occ: occ("k1", a.ID, 0), NewIndex: 0}, {Occ: occ("k2", b.ID, 0), NewIndex: 0}, {Occ: occ("k3", b.ID, 0), NewIndex: 1}}, at)
	if err != nil || len(created) != 2 {
		t.Fatalf("assign: %v %v", created, err)
	}
	ls, err := s.ListLearnings(ctx)
	if err != nil || len(ls) != 2 || ls[0].Text != "Keep answers short" || ls[0].Status != "candidate" || ls[0].Edited ||
		!ls[0].CreatedAt.Equal(at) || !ls[0].DecidedAt.IsZero() {
		t.Fatalf("learnings: %+v %v", ls, err)
	}
	short, push := ls[0].ID, ls[1].ID
	occs, err := s.ListLearningOccurrences(ctx, short)
	if err != nil || len(occs) != 2 || occs[0].TaskSlug != "a" || occs[1].TaskSlug != "b" || !occs[0].At.Equal(at) || occs[0].Session != 3 {
		t.Fatalf("occurrences: %+v %v", occs, err)
	}
	if all, _ := s.ListLearningOccurrences(ctx, 0); len(all) != 3 {
		t.Errorf("all occurrences: %d", len(all))
	}
	keys, err := s.LearningOccurrenceKeys(ctx, []string{"k1", "k3", "k9"})
	if err != nil || len(keys) != 2 || !keys["k1"] || !keys["k3"] {
		t.Errorf("keys: %v %v", keys, err)
	}

	// A new learning with a known text (any case) is that learning; a
	// key stored already stays where it is.
	created, err = s.AssignOccurrences(ctx, []string{"keep answers SHORT"},
		[]Assignment{{Occ: occ("k4", a.ID, 0), NewIndex: 0}, {Occ: occ("k3", a.ID, short)}}, time.Time{})
	if err != nil || len(created) != 0 {
		t.Fatalf("assign again: %v %v", created, err)
	}
	if o, _ := s.ListLearningOccurrences(ctx, short); len(o) != 3 {
		t.Errorf("short has %d occurrences", len(o))
	}
	if o, _ := s.ListLearningOccurrences(ctx, push); len(o) != 1 || o[0].TaskID != b.ID {
		t.Errorf("k3 moved: %+v", o)
	}
	// Bad rows fail the whole transaction.
	if _, err := s.AssignOccurrences(ctx, []string{"New one"}, []Assignment{{Occ: occ("k5", a.ID, 0)}, {Occ: occ("k6", a.ID, 999)}}, at); err == nil {
		t.Error("an occurrence of no learning")
	}
	if _, err := s.AssignOccurrences(ctx, nil, []Assignment{{Occ: with(occ("k7", a.ID, short), func(o *LearningOccurrence) { o.Strength = "huge" })}}, at); err == nil {
		t.Error("a bad strength")
	}
	if ls, _ := s.ListLearnings(ctx); len(ls) != 2 {
		t.Errorf("a failed assignment left a learning: %+v", ls)
	}

	// Status: only from the allowed ones; decide sets decided_at.
	if err := s.SetLearningStatus(ctx, short, []string{"candidate"}, "proposed", false); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.GetLearning(ctx, short); l.Status != "proposed" || !l.DecidedAt.IsZero() {
		t.Errorf("proposed: %+v", l)
	}
	if err := s.SetLearningStatus(ctx, short, []string{"candidate"}, "proposed", false); !errors.Is(err, ErrLearningStatus) || !strings.Contains(err.Error(), "is proposed") {
		t.Errorf("from the wrong status: %v", err)
	}
	if err := s.SetLearningStatus(ctx, short, []string{"proposed"}, "accepted", true); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.GetLearning(ctx, short); l.Status != "accepted" || l.DecidedAt.IsZero() {
		t.Errorf("accepted: %+v", l)
	}
	if err := s.SetLearningStatus(ctx, 999, []string{"proposed"}, "accepted", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("no such learning: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE learnings SET status = 'bogus' WHERE id = ?`, push); err == nil {
		t.Error("a bogus status")
	}

	// Text.
	if err := s.SetLearningText(ctx, push, "Ask before you push"); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.GetLearning(ctx, push); l.Text != "Ask before you push" || !l.Edited {
		t.Errorf("edited: %+v", l)
	}
	if err := s.SetLearningText(ctx, 999, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("edit no learning: %v", err)
	}

	// A task's delete takes its occurrences; an empty unedited candidate
	// goes with DeleteEmptyCandidates.
	if _, err := s.AssignOccurrences(ctx, []string{"Only in b"}, []Assignment{{Occ: occ("k8", b.ID, 0)}}, at); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTask(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if o, _ := s.ListLearningOccurrences(ctx, 0); len(o) != 2 {
		t.Errorf("after b's delete: %+v", o)
	}
	if n, err := s.DeleteEmptyCandidates(ctx); err != nil || n != 1 { // "Only in b"; push is edited
		t.Errorf("empty candidates: %d %v", n, err)
	}
	if _, err := s.GetLearning(ctx, push); err != nil {
		t.Errorf("the edited one went: %v", err)
	}

	// Delete: the learning and its occurrences.
	n, err := s.DeleteLearning(ctx, short)
	if err != nil || n != 2 {
		t.Fatalf("delete: %d %v", n, err)
	}
	if _, err := s.GetLearning(ctx, short); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
	if keys, _ := s.LearningOccurrenceKeys(ctx, []string{"k1", "k4"}); len(keys) != 0 {
		t.Errorf("its occurrences stay: %v", keys)
	}
	if _, err := s.DeleteLearning(ctx, short); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete again: %v", err)
	}
}

func with(o LearningOccurrence, f func(*LearningOccurrence)) LearningOccurrence { f(&o); return o }

// TestLearningTextsFold: texts are the same by Unicode case folding (not
// SQL's ASCII lower()), for a new learning and for an edit.
func TestLearningTextsFold(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	a := mkTask(t, s, "a", model.StatusDone, 0)
	o := func(key string) LearningOccurrence {
		return LearningOccurrence{Key: key, TaskID: a.ID, ItemID: "line:" + key, At: time.Now(), Strength: "normal", Polarity: "reinforce", Quote: "q q q", Text: "t"}
	}
	if _, err := s.AssignOccurrences(ctx, []string{"Écris sans prose", "Ask first"}, []Assignment{{Occ: o("k1")}, {Occ: o("k2"), NewIndex: 1}}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	created, err := s.AssignOccurrences(ctx, []string{"ÉCRIS SANS PROSE"}, []Assignment{{Occ: o("k3")}}, time.Time{})
	if err != nil || len(created) != 0 {
		t.Errorf("folded text made a learning: %v %v", created, err)
	}
	if occs, _ := s.ListLearningOccurrences(ctx, 1); len(occs) != 2 || occs[1].ItemID != "line:k3" {
		t.Errorf("occurrences: %+v", occs)
	}
	if err := s.SetLearningText(ctx, 2, "écris SANS prose"); !errors.Is(err, ErrLearningExists) {
		t.Errorf("edit to another's text: %v", err)
	}
	if err := s.SetLearningText(ctx, 1, "ÉCRIS sans prose"); err != nil {
		t.Errorf("edit of its own case: %v", err)
	}
}

func TestLearningSkips(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	a := mkTask(t, s, "a", model.StatusDone, 0)
	b := mkTask(t, s, "b", model.StatusDone, 1)
	both := []LearningSkip{{"k1", a.ID}, {"k2", b.ID}}
	// Left out in one run: not skipped yet.
	if n, err := s.AddLearningSkips(ctx, both, "v1"); err != nil || n != 0 {
		t.Fatalf("first run: %d %v", n, err)
	}
	if got, _ := s.LearningSkipKeys(ctx, []string{"k1", "k2"}, "v1"); len(got) != 0 {
		t.Errorf("skipped after one run: %v", got)
	}
	// In a second run: skipped.
	if n, err := s.AddLearningSkips(ctx, both, "v1"); err != nil || n != 2 {
		t.Fatalf("second run: %d %v", n, err)
	}
	if got, err := s.LearningSkipKeys(ctx, []string{"k1", "k2", "k3"}, "v1"); err != nil || len(got) != 2 {
		t.Errorf("skips: %v %v", got, err)
	}
	if err := s.DeleteTask(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.LearningSkipKeys(ctx, []string{"k1", "k2"}, "v1"); len(got) != 1 || !got["k1"] {
		t.Errorf("after b's delete: %v", got)
	}
	// --retry-skipped: all forgotten.
	if n, err := s.ClearLearningSkips(ctx); err != nil || n != 1 {
		t.Errorf("clear: %d %v", n, err)
	}
	if got, _ := s.LearningSkipKeys(ctx, []string{"k1"}, "v1"); len(got) != 0 {
		t.Errorf("after clear: %v", got)
	}
	// Another analyse version: the tries start again.
	for range 2 {
		s.AddLearningSkips(ctx, both[:1], "v1")
	}
	if n, _ := s.AddLearningSkips(ctx, both[:1], "v2"); n != 0 {
		t.Errorf("v2's first run skips")
	}
	if got, _ := s.LearningSkipKeys(ctx, []string{"k1"}, "v2"); len(got) != 0 {
		t.Errorf("v2: %v", got)
	}
	if n, _ := s.AddLearningSkips(ctx, both[:1], "v2"); n != 1 {
		t.Errorf("v2's second run does not skip")
	}
	if got, _ := s.LearningSkipKeys(ctx, []string{"k1"}, "v1"); len(got) != 0 {
		t.Errorf("v1 after v2: %v", got)
	}
}
