package learn

import (
	"math"
	"sort"
	"time"
)

// Score (docs/learning.md, "3. Score"): atl, not a model, weighs each
// learning's occurrences. Pure: the same rows, time and Config give the
// same score, so nothing is stored and a change of weights or half-lives
// re-scores everything.

// Learning statuses. A learning starts as a candidate; Transition moves
// it between candidate and proposed; only the user accepts or rejects.
const (
	StatusCandidate = "candidate" // below the propose threshold: hidden by default
	StatusProposed  = "proposed"
	StatusAccepted  = "accepted"
	StatusRejected  = "rejected"
)

// Statuses are the statuses a learning may have.
var Statuses = []string{StatusCandidate, StatusProposed, StatusAccepted, StatusRejected}

// Polarities, by name.
const (
	PolarityReinforce  = "reinforce"
	PolarityContradict = "contradict"
)

// TrendWindow is how far back a learning's trend looks: its score now
// minus its score then.
const TrendWindow = 30 * 24 * time.Hour

// ScoreInput is one occurrence of a learning as scoring reads it.
type ScoreInput struct {
	Key      string // tie-break of same-time occurrences
	ItemID   string // the item it quotes (learn.ItemID); "" if not known
	TaskID   int64
	Session  int64 // 0 for a notes line: the task's notes count as one session
	At       time.Time
	Strength string
	Polarity string
}

// Score is a learning's score and what review shows of it.
type Score struct {
	Score      float64   // Reinforce - Contradict
	Reinforce  float64   // what its reinforcing occurrences are worth
	Contradict float64   // what its contradicting occurrences take away
	Tasks      int       // distinct tasks of its reinforcing occurrences
	Last       time.Time // its newest occurrence; zero if none
	Trend      float64   // Score now minus Score TrendWindow ago (counting the occurrences up to then)
	// Outdated: an accepted learning contradicted since its acceptance
	// from at least 2 tasks, by Config.OutdatedScore (decayed) or by more
	// than its reinforcements (ScoreLearning).
	Outdated bool
	// StillCorrected: an accepted learning with reinforcing occurrences
	// newer than its acceptance: agents still get it wrong.
	StillCorrected bool
}

// Weight is what an occurrence of strength is worth before repeat and
// decay; 0 for an unknown strength.
func (c Config) Weight(strength string) float64 {
	switch strength {
	case "strong":
		return c.WeightStrong
	case "normal":
		return c.WeightNormal
	case "mild":
		return c.WeightMild
	}
	return 0
}

// HalfLife is the half-life of the occurrences of a learning with status:
// Config.HalfLifeAccepted for an accepted one (silence does not count
// against a rule that works), else Config.HalfLifeProposed.
func (c Config) HalfLife(status string) time.Duration {
	days := c.HalfLifeProposed
	if status == StatusAccepted {
		days = c.HalfLifeAccepted
	}
	return time.Duration(days) * 24 * time.Hour
}

// ScoreLearning scores a learning of status, decided (accepted or
// rejected) at decided, from its occurrences, at now:
//
//   - an occurrence dated after now (a clock skew, a forged date) is as of
//     now;
//   - of the occurrences of one item (a prompt or notes line: ItemID) of
//     one task with one polarity, only the one with the highest weight
//     counts (the newest of equals): one statement read twice, or quoted
//     two ways, is one;
//   - an occurrence is worth Weight(strength) × repeat × decay;
//   - repeat: within one session of one task (a task's notes lines count
//     as one session), the k-th occurrence of a polarity in time order
//     counts 1/k;
//   - decay: 0.5^(age / HalfLife(status));
//   - contradicting occurrences subtract.
//
// Trend is the score now minus the score TrendWindow ago, each counting
// only the occurrences up to its time. Outdated looks only at the
// contradictions after decided (those before it the user saw when
// deciding): from at least 2 distinct tasks, and worth at least
// Config.OutdatedScore or more than the reinforcements.
func ScoreLearning(status string, decided time.Time, occ []ScoreInput, now time.Time, cfg Config) Score {
	occ = collapse(occ, now, cfg)
	half := cfg.HalfLife(status)
	var s Score
	var newContra float64
	s.Reinforce, s.Contradict, newContra = scoreAt(occ, now, half, decided, cfg)
	s.Score = s.Reinforce - s.Contradict
	r0, c0, _ := scoreAt(occ, now.Add(-TrendWindow), half, decided, cfg)
	s.Trend = s.Score - (r0 - c0)
	tasks, contraTasks := map[int64]bool{}, map[int64]bool{}
	for _, o := range occ {
		if o.At.After(s.Last) {
			s.Last = o.At
		}
		switch o.Polarity {
		case PolarityReinforce:
			tasks[o.TaskID] = true
			if status == StatusAccepted && o.At.After(decided) {
				s.StillCorrected = true
			}
		case PolarityContradict:
			if o.At.After(decided) {
				contraTasks[o.TaskID] = true
			}
		}
	}
	s.Tasks = len(tasks)
	if status == StatusAccepted {
		s.Outdated = len(contraTasks) >= 2 && (newContra >= cfg.OutdatedScore || s.Reinforce-newContra < 0)
	}
	return s
}

// collapse returns occ with dates after now made now, one occurrence per
// (task, item, polarity) (the highest weight, the newest of equals; an
// occurrence with no ItemID is its own), in time order (by key for the
// same time).
func collapse(occ []ScoreInput, now time.Time, cfg Config) []ScoreInput {
	type group struct {
		task     int64
		item     string
		polarity string
	}
	best := map[group]int{}
	var out []ScoreInput
	for _, o := range occ {
		if o.At.After(now) {
			o.At = now
		}
		if o.ItemID == "" {
			out = append(out, o)
			continue
		}
		g := group{o.TaskID, o.ItemID, o.Polarity}
		i, ok := best[g]
		if !ok {
			best[g] = len(out)
			out = append(out, o)
			continue
		}
		b := out[i]
		if w, bw := cfg.Weight(o.Strength), cfg.Weight(b.Strength); w > bw || w == bw && (o.At.After(b.At) || o.At.Equal(b.At) && o.Key > b.Key) {
			out[i] = o
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// scoreAt is what occ (collapsed, in time order) are worth at t:
// reinforcing, contradicting, and contradicting after decided apart,
// counting only those up to t.
func scoreAt(occ []ScoreInput, t time.Time, half time.Duration, decided time.Time, cfg Config) (reinforce, contradict, contradictAfter float64) {
	type group struct {
		task, session int64
		polarity      string
	}
	seen := map[group]int{}
	for _, o := range occ {
		if o.At.After(t) {
			break
		}
		g := group{o.TaskID, o.Session, o.Polarity}
		seen[g]++
		v := cfg.Weight(o.Strength) * decay(t.Sub(o.At), half) / float64(seen[g])
		switch o.Polarity {
		case PolarityReinforce:
			reinforce += v
		case PolarityContradict:
			contradict += v
			if o.At.After(decided) {
				contradictAfter += v
			}
		}
	}
	return reinforce, contradict, contradictAfter
}

// decay is 0.5^(age/half); 1 for an age below 0.
func decay(age, half time.Duration) float64 {
	if age <= 0 || half <= 0 {
		return 1
	}
	return math.Pow(0.5, float64(age)/float64(half))
}

// Transition is the status a learning of status with score s moves to at
// the end of an atl learn run: a candidate is proposed when its Score
// reaches Config.ProposeScore and its occurrences span
// Config.ProposeTasks distinct tasks; a proposed learning goes back to
// candidate (a proposal fades) only below Config.FadeRatio × ProposeScore
// or when it spans fewer tasks (hysteresis). Accepted and rejected
// learnings never change on their own.
func Transition(status string, s Score, cfg Config) string {
	switch {
	case status == StatusCandidate && s.Score >= cfg.ProposeScore && s.Tasks >= cfg.ProposeTasks:
		return StatusProposed
	case status == StatusProposed && (s.Score < cfg.FadeRatio*cfg.ProposeScore || s.Tasks < cfg.ProposeTasks):
		return StatusCandidate
	}
	return status
}
