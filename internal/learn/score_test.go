package learn

import (
	"fmt"
	"math"
	"testing"
	"time"
)

var now0 = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

const day = 24 * time.Hour

// occ is a normal reinforcing occurrence in task, session, days ago.
func occ(task, session int64, daysAgo float64) ScoreInput {
	return ScoreInput{Key: fmt.Sprint(task, session, daysAgo), TaskID: task, Session: session,
		At: now0.Add(-time.Duration(daysAgo * float64(day))), Strength: "normal", Polarity: PolarityReinforce}
}

func with(o ScoreInput, f func(*ScoreInput)) ScoreInput { f(&o); return o }

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

// TestScoreExamples: the examples of docs/learning.md ("3. Score").
func TestScoreExamples(t *testing.T) {
	cfg := Defaults()
	cases := []struct {
		name   string
		status string
		occ    []ScoreInput
		score  float64
		tasks  int
	}{
		{"once, one session", StatusCandidate, []ScoreInput{occ(1, 1, 0)}, 1, 1},
		{"4 times, one session", StatusCandidate, []ScoreInput{occ(1, 1, 0.4), occ(1, 1, 0.3), occ(1, 1, 0.2), occ(1, 1, 0.1)}, 1 + 1.0/2 + 1.0/3 + 1.0/4, 1},
		{"once each in 3 tasks", StatusCandidate, []ScoreInput{occ(1, 1, 0), occ(2, 2, 0), occ(3, 3, 0)}, 3, 3},
		{"2 sessions of one task", StatusCandidate, []ScoreInput{occ(1, 1, 0), occ(1, 2, 0)}, 2, 1},
		{"notes lines of one task: one session", StatusCandidate, []ScoreInput{occ(1, 0, 0), occ(1, 0, 0.1)}, 1.5, 1},
		{"3 times each in 3 tasks", StatusCandidate, []ScoreInput{occ(1, 1, 0), occ(1, 1, 0.001), occ(1, 1, 0.002), occ(2, 2, 0), occ(2, 2, 0.001), occ(2, 2, 0.002),
			occ(3, 3, 0), occ(3, 3, 0.001), occ(3, 3, 0.002)}, 3 * (1 + 0.5 + 1.0/3), 3},
		{"once each in 3 tasks, 90 days ago", StatusProposed, []ScoreInput{occ(1, 1, 90), occ(2, 2, 90), occ(3, 3, 90)}, 0.75, 3},
		{"accepted: a year's half-life", StatusAccepted, []ScoreInput{occ(1, 1, 365), occ(2, 2, 365)}, 1, 2},
		{"strong and mild", StatusCandidate, []ScoreInput{with(occ(1, 1, 0), func(o *ScoreInput) { o.Strength = "strong" }),
			with(occ(2, 2, 0), func(o *ScoreInput) { o.Strength = "mild" })}, 2.5, 2},
		{"contradictions subtract, repeats discounted", StatusCandidate, []ScoreInput{occ(1, 1, 0), occ(2, 2, 0), occ(3, 3, 0),
			with(occ(1, 1, 0), func(o *ScoreInput) { o.Polarity = PolarityContradict }),
			with(occ(1, 1, 0), func(o *ScoreInput) { o.Polarity, o.Key = PolarityContradict, "c2" })}, 1.5, 3},
		{"a contradiction decays", StatusCandidate, []ScoreInput{occ(1, 1, 0), occ(2, 2, 0),
			with(occ(3, 3, 45), func(o *ScoreInput) { o.Polarity, o.Strength = PolarityContradict, "strong" })}, 1, 2},
		{"from the future: as of now", StatusCandidate, []ScoreInput{occ(1, 1, -3)}, 1, 1},
		{"none", StatusCandidate, nil, 0, 0},
	}
	for _, c := range cases {
		s := ScoreLearning(c.status, time.Time{}, c.occ, now0, cfg)
		if !near(s.Score, c.score) || s.Tasks != c.tasks {
			t.Errorf("%s: score %.3f, %d tasks; want %.3f, %d", c.name, s.Score, s.Tasks, c.score, c.tasks)
		}
	}
	// The repeat discount goes by time order, not by the order given.
	a := ScoreLearning(StatusCandidate, time.Time{}, []ScoreInput{with(occ(1, 1, 0), func(o *ScoreInput) { o.Strength = "strong" }), occ(1, 1, 10)}, now0, cfg)
	if want := 1*math.Pow(0.5, 10.0/45) + 2.0/2; !near(a.Score, want) {
		t.Errorf("time order: %.3f, want %.3f", a.Score, want)
	}
	// Weights and half-lives come from Config.
	cfg.WeightNormal, cfg.HalfLifeProposed = 3, 90
	if s := ScoreLearning(StatusCandidate, time.Time{}, []ScoreInput{occ(1, 1, 90)}, now0, cfg); !near(s.Score, 1.5) {
		t.Errorf("config: %.3f", s.Score)
	}
}

func TestScoreTrendAndLast(t *testing.T) {
	cfg := Defaults()
	// Rising: all of it in the last 30 days.
	s := ScoreLearning(StatusCandidate, time.Time{}, []ScoreInput{occ(1, 1, 1), occ(2, 2, 5)}, now0, cfg)
	if !near(s.Trend, s.Score) || s.Trend <= 0 || !s.Last.Equal(now0.Add(-day)) || !near(s.Reinforce, s.Score) || s.Contradict != 0 {
		t.Errorf("rising: %+v", s)
	}
	// Fading: nothing since 60 days ago.
	s = ScoreLearning(StatusCandidate, time.Time{}, []ScoreInput{occ(1, 1, 60), occ(2, 2, 60)}, now0, cfg)
	then := 2 * math.Pow(0.5, 30.0/45)
	if !near(s.Trend, s.Score-then) || s.Trend >= 0 {
		t.Errorf("fading: %+v, then %.3f", s, then)
	}
}

func TestScoreAcceptedFlags(t *testing.T) {
	cfg := Defaults()
	decided := now0.Add(-10 * day)
	contra := func(task int64, daysAgo float64, strength string) ScoreInput {
		return with(occ(task, task, daysAgo), func(o *ScoreInput) {
			o.Polarity, o.Strength, o.Key = PolarityContradict, strength, fmt.Sprint("c", task, daysAgo)
		})
	}
	plus := func(more ...ScoreInput) []ScoreInput {
		return append([]ScoreInput{occ(1, 1, 100), occ(2, 2, 100), occ(3, 3, 100)}, more...)
	}
	cases := []struct {
		name            string
		status          string
		occ             []ScoreInput
		outdated, still bool
	}{
		{"accepted, quiet", StatusAccepted, plus(), false, false},
		{"accepted, corrected again", StatusAccepted, plus(occ(5, 5, 2)), false, true},
		{"accepted, one contradiction", StatusAccepted, plus(contra(4, 1, "normal")), false, false},
		{"accepted, contradictions reach 2 from 2 tasks", StatusAccepted, plus(contra(4, 0, "normal"), contra(5, 0, "normal")), true, false},
		{"accepted, 2 strong from one task: not enough tasks", StatusAccepted, plus(contra(4, 0, "strong"), contra(4, 1, "strong")), false, false},
		{"accepted, net below 0 from 2 tasks", StatusAccepted, []ScoreInput{occ(1, 1, 100), contra(4, 1, "normal"), contra(5, 2, "mild")}, true, false},
		{"accepted, net below 0 from one task", StatusAccepted, []ScoreInput{occ(1, 1, 100), contra(4, 1, "normal"), contra(4, 2, "strong")}, false, false},
		// Contradictions the user saw before accepting do not count, though
		// the accepted half-life revives them.
		{"accepted, contradicted before it was accepted", StatusAccepted, plus(contra(4, 20, "strong"), contra(5, 30, "strong")), false, false},
		{"accepted, before and after", StatusAccepted, plus(contra(4, 20, "strong"), contra(5, 1, "strong"), contra(6, 1, "normal")), true, false},
		{"proposed: no flags", StatusProposed, plus(contra(4, 0, "strong"), contra(5, 0, "strong"), occ(5, 5, 2)), false, false},
	}
	for _, c := range cases {
		s := ScoreLearning(c.status, decided, c.occ, now0, cfg)
		if s.Outdated != c.outdated || s.StillCorrected != c.still {
			t.Errorf("%s: outdated %v, still corrected %v (%+v)", c.name, s.Outdated, s.StillCorrected, s)
		}
	}
}

// TestScoreCollapse: occurrences of one item of one task count once per
// polarity (the strongest), whatever the quotes; a forged future date
// counts as now.
func TestScoreCollapse(t *testing.T) {
	cfg := Defaults()
	item := func(o ScoreInput, id, strength, key string) ScoreInput {
		return with(o, func(o *ScoreInput) { o.ItemID, o.Strength, o.Key = id, strength, key })
	}
	cases := []struct {
		name  string
		occ   []ScoreInput
		score float64
	}{
		{"one prompt quoted three ways", []ScoreInput{item(occ(1, 1, 0), "uuid:a", "normal", "k1"), item(occ(1, 1, 0), "uuid:a", "strong", "k2"),
			item(occ(1, 1, 0), "uuid:a", "mild", "k3")}, 2},
		{"two prompts", []ScoreInput{item(occ(1, 1, 0), "uuid:a", "normal", "k1"), item(occ(1, 1, 0), "uuid:b", "normal", "k2")}, 1.5},
		{"one item in two tasks", []ScoreInput{item(occ(1, 1, 0), "uuid:a", "normal", "k1"), item(occ(2, 1, 0), "uuid:a", "normal", "k2")}, 2},
		{"one item, both polarities", []ScoreInput{item(occ(1, 1, 0), "uuid:a", "strong", "k1"),
			with(item(occ(1, 1, 0), "uuid:a", "normal", "k2"), func(o *ScoreInput) { o.Polarity = PolarityContradict })}, 1},
		{"no item id: each counts", []ScoreInput{item(occ(1, 1, 0), "", "normal", "k1"), item(occ(1, 1, 0), "", "normal", "k2")}, 1.5},
	}
	for _, c := range cases {
		if s := ScoreLearning(StatusCandidate, time.Time{}, c.occ, now0, cfg); !near(s.Score, c.score) {
			t.Errorf("%s: %.3f, want %.3f", c.name, s.Score, c.score)
		}
	}
	// 2099: as of now (no score above its weight, Last now).
	future := with(occ(1, 2, 0), func(o *ScoreInput) { o.At = time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) })
	s := ScoreLearning(StatusAccepted, now0.Add(-day), []ScoreInput{occ(1, 1, 400), future}, now0, cfg)
	if !s.Last.Equal(now0) || !near(s.Score, 1+math.Pow(0.5, 400.0/365)) {
		t.Errorf("future: %+v", s)
	}
}

func TestTransition(t *testing.T) {
	cfg := Defaults()
	three := []ScoreInput{occ(1, 1, 0), occ(2, 2, 0), occ(3, 3, 0)}
	oneTask := []ScoreInput{occ(1, 1, 0), occ(1, 2, 0), occ(1, 3, 0), occ(1, 4, 0)}
	faded := []ScoreInput{occ(1, 1, 90), occ(2, 2, 90), occ(3, 3, 90)}  // 0.75
	dipped := []ScoreInput{occ(1, 1, 10), occ(2, 2, 10), occ(3, 3, 10)} // 2.57: below 3, above 2.25
	cases := []struct {
		name, status string
		occ          []ScoreInput
		want         string
	}{
		{"candidate at 3 in 3 tasks: proposed", StatusCandidate, three, StatusProposed},
		{"candidate at 4 in one task: stays", StatusCandidate, oneTask, StatusCandidate},
		{"candidate below 3: stays", StatusCandidate, three[:2], StatusCandidate},
		{"candidate at 2.57: stays", StatusCandidate, dipped, StatusCandidate},
		{"proposed still at 3: stays", StatusProposed, three, StatusProposed},
		{"proposed at 2.57, above 0.75 × 3: stays", StatusProposed, dipped, StatusProposed},
		{"proposed, faded: back to candidate", StatusProposed, faded, StatusCandidate},
		{"proposed, one task left: back to candidate", StatusProposed, oneTask, StatusCandidate},
		{"accepted, faded: untouched", StatusAccepted, faded, StatusAccepted},
		{"accepted, none: untouched", StatusAccepted, nil, StatusAccepted},
		{"rejected at 3 in 3 tasks: untouched", StatusRejected, three, StatusRejected},
	}
	for _, c := range cases {
		s := ScoreLearning(c.status, time.Time{}, c.occ, now0, cfg)
		if got := Transition(c.status, s, cfg); got != c.want {
			t.Errorf("%s: %s (score %.2f, %d tasks)", c.name, got, s.Score, s.Tasks)
		}
	}
	cfg.ProposeTasks = 1
	if got := Transition(StatusCandidate, ScoreLearning(StatusCandidate, time.Time{}, oneTask, now0, cfg), cfg); got != StatusProposed {
		t.Errorf("propose_tasks = 1: %s", got)
	}
	cfg.FadeRatio = 1
	if got := Transition(StatusProposed, ScoreLearning(StatusProposed, time.Time{}, dipped, now0, cfg), cfg); got != StatusCandidate {
		t.Errorf("fade_ratio = 1: %s", got)
	}
}
