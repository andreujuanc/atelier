// Package learn builds what `atl learn` reads from a workspace's tasks (see
// docs/learning.md): per task, the prompts the user typed, the end of the
// reply before each, and the notes lines with who wrote them. Pure logic:
// internal/app opens the transcripts and the store and calls it.
package learn

import (
	"errors"
	"fmt"
	"slices"
)

// Config is the learning module's settings: Defaults, each overridable from
// atelier.toml's [learn] table (workspace.ReadConfig; unknown keys are an
// error there).
type Config struct {
	PromptCap    int `toml:"prompt_cap"`    // bytes of one prompt's text
	ReplySnippet int `toml:"reply_snippet"` // characters of the reply kept before each prompt (0: none)
	TaskCap      int `toml:"task_cap"`      // bytes of one task's extract (JSON); past it the oldest prompts are dropped
	// NotesCap is the bytes of the notes in an extract (JSON); past it
	// lines are dropped, others' before the user's.
	NotesCap int `toml:"notes_cap"`

	WeightStrong float64 `toml:"weight_strong"`
	WeightNormal float64 `toml:"weight_normal"`
	WeightMild   float64 `toml:"weight_mild"`

	HalfLifeProposed int `toml:"half_life_proposed"` // days
	HalfLifeAccepted int `toml:"half_life_accepted"` // days

	ProposeScore float64 `toml:"propose_score"` // a learning is proposed at this score ...
	ProposeTasks int     `toml:"propose_tasks"` // ... from at least this many distinct tasks
	// OutdatedScore is the decayed score of contradictions at which an
	// accepted learning goes back to review ("possibly outdated"; so does a
	// net score below 0).
	OutdatedScore float64 `toml:"outdated_score"`

	Budget      int `toml:"budget"`      // bytes of the "User preferences" start context section
	Parallelism int `toml:"parallelism"` // pre-clean runs at a time

	PrecleanModel  string `toml:"preclean_model"`
	PrecleanEffort string `toml:"preclean_effort"`
	AnalyseModel   string `toml:"analyse_model"`
	AnalyseEffort  string `toml:"analyse_effort"`
}

// Defaults returns the defaults of docs/learning.md ("Defaults (to tune)").
func Defaults() Config {
	return Config{
		PromptCap:    2 << 10,
		ReplySnippet: 200,
		TaskCap:      64 << 10,
		NotesCap:     16 << 10,

		WeightStrong: 2,
		WeightNormal: 1,
		WeightMild:   0.5,

		HalfLifeProposed: 45,
		HalfLifeAccepted: 365,

		ProposeScore:  3,
		ProposeTasks:  2,
		OutdatedScore: 2,

		Budget:      800,
		Parallelism: 3,

		PrecleanModel:  "haiku",
		PrecleanEffort: "low",
		AnalyseModel:   "haiku",
		AnalyseEffort:  "medium",
	}
}

// Efforts are the reasoning efforts a model run may be given.
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// Validate checks every value's range; the error names the atelier.toml
// key ("learn.prompt_cap").
func (c Config) Validate() error {
	var errs []error
	ints := []struct {
		key      string
		v        int
		min, max int
	}{
		{"prompt_cap", c.PromptCap, 64, 64 << 10},
		{"reply_snippet", c.ReplySnippet, 0, 2000},
		{"task_cap", c.TaskCap, 4 << 10, 16 << 20},
		{"notes_cap", c.NotesCap, 1 << 10, 16 << 20},
		{"half_life_proposed", c.HalfLifeProposed, 1, 36500},
		{"half_life_accepted", c.HalfLifeAccepted, 1, 36500},
		{"propose_tasks", c.ProposeTasks, 1, 1000},
		{"budget", c.Budget, 0, 10000},
		{"parallelism", c.Parallelism, 1, 16},
	}
	for _, f := range ints {
		if f.v < f.min || f.v > f.max {
			errs = append(errs, fmt.Errorf("learn.%s: %d: must be %d..%d", f.key, f.v, f.min, f.max))
		}
	}
	if c.PromptCap > c.TaskCap {
		errs = append(errs, fmt.Errorf("learn.prompt_cap: %d: more than learn.task_cap (%d)", c.PromptCap, c.TaskCap))
	}
	if c.NotesCap+c.PromptCap > c.TaskCap { // room for the notes and at least one prompt
		errs = append(errs, fmt.Errorf("learn.notes_cap + learn.prompt_cap: %d + %d: more than learn.task_cap (%d)", c.NotesCap, c.PromptCap, c.TaskCap))
	}
	floats := []struct {
		key string
		v   float64
		max float64
	}{
		{"weight_strong", c.WeightStrong, 100},
		{"weight_normal", c.WeightNormal, 100},
		{"weight_mild", c.WeightMild, 100},
		{"propose_score", c.ProposeScore, 1000},
		{"outdated_score", c.OutdatedScore, 1000},
	}
	for _, f := range floats {
		if !(f.v > 0 && f.v <= f.max) { // NaN too
			errs = append(errs, fmt.Errorf("learn.%s: %v: must be more than 0 and at most %v", f.key, f.v, f.max))
		}
	}
	if !(c.WeightStrong >= c.WeightNormal && c.WeightNormal >= c.WeightMild) {
		errs = append(errs, fmt.Errorf("learn: weights must be strong >= normal >= mild, not %v, %v, %v", c.WeightStrong, c.WeightNormal, c.WeightMild))
	}
	for _, m := range []struct{ key, v string }{{"preclean_model", c.PrecleanModel}, {"analyse_model", c.AnalyseModel}} {
		if err := checkModel(m.v); err != nil {
			errs = append(errs, fmt.Errorf("learn.%s: %q: %v", m.key, m.v, err))
		}
	}
	for _, e := range []struct{ key, v string }{{"preclean_effort", c.PrecleanEffort}, {"analyse_effort", c.AnalyseEffort}} {
		if !slices.Contains(Efforts, e.v) {
			errs = append(errs, fmt.Errorf("learn.%s: %q: must be one of %v", e.key, e.v, Efforts))
		}
	}
	return errors.Join(errs...)
}

// checkModel accepts a model name or alias as `claude --model` takes it
// ("haiku", "claude-sonnet-4-5", "sonnet[1m]"): letters, digits and
// . _ - [ ], at most 100 bytes, not starting with '-'.
func checkModel(m string) error {
	if m == "" || len(m) > 100 || m[0] == '-' {
		return errors.New("must be a model name")
	}
	for _, r := range m {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-', r == '[', r == ']':
		default:
			return fmt.Errorf("%q is not allowed in a model name", r)
		}
	}
	return nil
}
