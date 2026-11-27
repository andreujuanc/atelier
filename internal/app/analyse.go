package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"syscall"
	"time"

	"github.com/andreujuanc/atelier/internal/hook"
	"github.com/andreujuanc/atelier/internal/learn"
	"github.com/andreujuanc/atelier/internal/store"
)

// LearnRun is what an atl learn run did: each task's pre-clean, then
// Analyse and the status transitions.
type LearnRun struct {
	Tasks   []PrecleanTask
	Analyse AnalyseResult
	// RetriedSkips counts the skipped occurrences forgotten first
	// (--retry-skipped).
	RetriedSkips int
}

// AnalyseResult is what Analyse did in a run.
type AnalyseResult struct {
	New int // occurrences of the current digests neither stored nor skipped before
	// Joined counts those stored with the learning of an occurrence of
	// the same item whose quote overlaps (learn.QuotesOverlap), not sent.
	Joined       int
	Assigned     int // assigned by the model
	ToNew        int // of those, to learnings made this run
	NewLearnings int // learnings made this run
	// Unassigned counts those of failed calls, and those left out not
	// skipped yet: analysed again next run.
	Unassigned int
	// Skipped counts those the model left out (a chunk, then the
	// follow-up call) in this run and store.LearningSkipTries - 1 runs
	// before: not sent again while learn.AnalyseVersion stays.
	Skipped int
	// DroppedLabels counts new learnings dropped (learn.CheckAnalyse):
	// their occurrences were treated as left out.
	DroppedLabels int
	Calls         int     // model calls
	CostUSD       float64 // of the calls
	Err           error   // why calls failed (each failed chunk; the others are stored)
	// Transitions are the status changes at the end of the run
	// (learn.Transition).
	Transitions []LearningTransition
	// Dropped counts the candidates deleted at the end of the run: no
	// occurrence left (their tasks were deleted).
	Dropped int
}

// LearningTransition is a learning's automatic status change.
type LearningTransition struct {
	ID       int64
	Text     string
	From, To string
}

// analyseChunk is how many new occurrences one Analyse call reads
// (learn.AnalyseChunk; tests change it).
var analyseChunk = learn.AnalyseChunk

// Learn is an atl learn run (docs/learning.md, "Pipeline"), serialised
// in the workspace (state/learn.lock): the pre-clean of task ref, or of
// every task when ref is "" (Preclean); then Analyse of the occurrences
// of the current digests that no learning has yet (by
// learn.OccurrenceKey); then each learning's status moves as its score
// says (learn.Transition). One learner serves the whole run, started only
// when a call needs it. A failed Analyse is AnalyseResult.Err, not an
// error: the pre-cleans stay cached, the transitions still apply. With
// retrySkipped, the skipped occurrences (store.LearningSkipKeys) are
// forgotten first, so Analyse sees them again.
func (a *App) Learn(ctx context.Context, ref string, retrySkipped bool) (LearnRun, error) {
	unlock, err := a.lockLearn(ctx)
	if err != nil {
		return LearnRun{}, err
	}
	defer unlock()
	var run LearnRun
	if retrySkipped {
		if run.RetriedSkips, err = a.Store.ClearLearningSkips(ctx); err != nil {
			return run, err
		}
	}
	ls := &learnerSession{a: a, ctx: ctx}
	defer ls.close()
	if run.Tasks, err = a.preclean(ctx, ref, ls); err != nil {
		return run, err
	}
	run.Analyse = a.analyse(ctx, run.Tasks, ls)
	if ctx.Err() != nil {
		return run, ctx.Err()
	}
	if run.Analyse.Transitions, run.Analyse.Dropped, err = a.applyTransitions(ctx); err != nil {
		return run, err
	}
	return run, nil
}

// pendingOccurrence is an occurrence of a current digest, by its key.
type pendingOccurrence struct {
	key    string
	item   string // learn.ItemID
	taskID int64
	occ    learn.Occurrence
	at     time.Time
}

// analyse assigns to learnings the occurrences of pts' current digests
// (ran or cached) neither stored nor skipped before:
//
//   - one whose task has an occurrence of the same item stored with an
//     overlapping quote (learn.QuotesOverlap: the same statement quoted
//     another way by a pre-clean made again) joins that one's learning,
//     not sent;
//   - the others go to the model in chunks of analyseChunk, oldest first,
//     each answer checked (learn.CheckAnalyse) and stored before the next
//     chunk is sent, so the next knows the learnings it made. A chunk
//     whose call fails, whose answer does not hold up, or that assigns
//     under half of it (of more than 3) stores nothing (analysed again next run); the
//     other chunks go on;
//   - what the answers left out goes once more in a follow-up call (one
//     that assigns none of more than 3 fails); what that leaves out too
//     is recorded (store.AddLearningSkips): left out so in
//     store.LearningSkipTries runs, it is skipped until
//     learn.AnalyseVersion changes or atl learn --retry-skipped.
//
// Times after now are stored as now.
func (a *App) analyse(ctx context.Context, pts []PrecleanTask, ls *learnerSession) AnalyseResult {
	var res AnalyseResult
	now := a.now()
	seen := map[string]bool{}
	var all []pendingOccurrence
	for _, pt := range pts {
		if pt.Status != PrecleanRan && pt.Status != PrecleanCached {
			continue
		}
		for _, o := range pt.Occurrences {
			key := learn.OccurrenceKey(pt.TaskID, pt.Extract, o)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			at, err := time.Parse(time.RFC3339, o.At)
			if err != nil || at.After(now) {
				at = now // not known: when atl first saw it; never later than now
			}
			all = append(all, pendingOccurrence{key, learn.ItemID(pt.Extract, o.Source), pt.TaskID, o, at})
		}
	}
	keys := make([]string, len(all))
	for i, p := range all {
		keys[i] = p.key
	}
	stored, err := a.Store.LearningOccurrenceKeys(ctx, keys)
	if err != nil {
		res.Err = err
		return res
	}
	skipped, err := a.Store.LearningSkipKeys(ctx, keys, learn.AnalyseVersion)
	if err != nil {
		res.Err = err
		return res
	}
	var todo []pendingOccurrence
	for _, p := range all {
		if !stored[p.key] && !skipped[p.key] {
			todo = append(todo, p)
		}
	}
	sort.SliceStable(todo, func(i, j int) bool {
		if !todo[i].at.Equal(todo[j].at) {
			return todo[i].at.Before(todo[j].at)
		}
		return todo[i].key < todo[j].key
	})
	res.New = len(todo)
	if len(todo) == 0 {
		return res
	}
	if todo, err = a.joinOverlapping(ctx, todo, &res); err != nil {
		res.Err, res.Unassigned = err, len(todo)
		return res
	}
	if len(todo) == 0 {
		return res
	}
	cfg := a.LearnConfig()
	fmt.Fprintf(a.out(), "learn: analyse of %d new occurrence(s) with %s (effort %s)\n", len(todo), cfg.AnalyseModel, cfg.AnalyseEffort)
	run, err := ls.get()
	if err != nil {
		res.Err, res.Unassigned = err, len(todo)
		return res
	}
	argv := learn.AnalyseArgs(cfg.AnalyseModel, cfg.AnalyseEffort, cfg.MaxCallUSD)
	var errs []error
	pass := func(items []pendingOccurrence, followUp bool) (left []pendingOccurrence) {
		size := max(analyseChunk, 1)
		for start := 0; start < len(items); start += size {
			chunk := items[start:min(start+size, len(items))]
			if ctx.Err() != nil {
				errs = append(errs, ctx.Err())
				res.Unassigned += len(items) - start
				return left
			}
			l, err := a.analyseChunk(ctx, run, argv, chunk, followUp, &res)
			if err != nil {
				errs = append(errs, err)
				res.Unassigned += len(chunk)
				continue
			}
			left = append(left, l...)
		}
		return left
	}
	if left := pass(todo, false); len(left) > 0 {
		left = pass(left, true)
		if len(left) > 0 {
			skips := make([]store.LearningSkip, len(left))
			for i, p := range left {
				skips[i] = store.LearningSkip{Key: p.key, TaskID: p.taskID}
			}
			n, err := a.Store.AddLearningSkips(context.WithoutCancel(ctx), skips, learn.AnalyseVersion)
			if err != nil {
				errs = append(errs, err)
				n = 0
			}
			res.Skipped += n
			res.Unassigned += len(left) - n
		}
	}
	res.Err = joinDistinct(errs)
	return res
}

// joinDistinct joins errs, each message once.
func joinDistinct(errs []error) error {
	seen := map[string]bool{}
	var out []error
	for _, err := range errs {
		if !seen[err.Error()] {
			seen[err.Error()] = true
			out = append(out, err)
		}
	}
	return errors.Join(out...)
}

// joinOverlapping stores each of todo whose task has an occurrence of the
// same item with an overlapping quote with that one's learning, and
// returns the others.
func (a *App) joinOverlapping(ctx context.Context, todo []pendingOccurrence, res *AnalyseResult) ([]pendingOccurrence, error) {
	occs, err := a.Store.ListLearningOccurrences(ctx, 0)
	if err != nil {
		return todo, err
	}
	type item struct {
		task int64
		id   string
	}
	byItem := map[item][]store.LearningOccurrence{}
	for _, o := range occs {
		if o.ItemID != "" {
			byItem[item{o.TaskID, o.ItemID}] = append(byItem[item{o.TaskID, o.ItemID}], o)
		}
	}
	var rest []pendingOccurrence
	var as []store.Assignment
	for _, p := range todo {
		joined := false
		for _, o := range byItem[item{p.taskID, p.item}] {
			if p.item != "" && learn.QuotesOverlap(p.occ.Quote, o.Quote) {
				lo := p.row()
				lo.LearningID = o.LearningID
				as = append(as, store.Assignment{Occ: lo})
				joined = true
				break
			}
		}
		if !joined {
			rest = append(rest, p)
		}
	}
	if len(as) == 0 {
		return rest, nil
	}
	if _, err := a.Store.AssignOccurrences(context.WithoutCancel(ctx), nil, as, a.now()); err != nil {
		return todo, fmt.Errorf("store the occurrences of items seen before: %w", err)
	}
	res.Joined += len(as)
	return rest, nil
}

// row is p as a stored occurrence, of no learning yet.
func (p pendingOccurrence) row() store.LearningOccurrence {
	return store.LearningOccurrence{Key: p.key, TaskID: p.taskID, ItemID: p.item, Session: p.occ.Session, At: p.at,
		Strength: p.occ.Strength, Polarity: p.occ.Polarity, Quote: p.occ.Quote, Text: p.occ.Text}
}

// sampleQuotes returns, by learning, up to learn.MaxSampleQuotes quotes
// of its reinforcing occurrences (occs oldest first): the newest of each
// task first, so one task cannot fill them all; then the newest others.
func sampleQuotes(occs []store.LearningOccurrence) map[int64][]string {
	out := map[int64][]string{}
	used := map[int]bool{}
	type lt struct{ learning, task int64 }
	tasks := map[lt]bool{}
	for i := len(occs) - 1; i >= 0; i-- {
		o := occs[i]
		if o.Polarity != learn.PolarityReinforce || tasks[lt{o.LearningID, o.TaskID}] || len(out[o.LearningID]) >= learn.MaxSampleQuotes {
			continue
		}
		tasks[lt{o.LearningID, o.TaskID}] = true
		used[i] = true
		out[o.LearningID] = append(out[o.LearningID], o.Quote)
	}
	for i := len(occs) - 1; i >= 0; i-- {
		o := occs[i]
		if o.Polarity == learn.PolarityReinforce && !used[i] && len(out[o.LearningID]) < learn.MaxSampleQuotes {
			out[o.LearningID] = append(out[o.LearningID], o.Quote)
		}
	}
	return out
}

// analyseChunk runs Analyse on chunk and stores its checked answer; it
// returns the occurrences the answer left out (or gave a dropped label).
// For a chunk of more than 3, an answer that assigns under half of it
// (none, in a follow-up) is an error: nothing of it is stored. (A smaller
// one is left out, and skipped only after another run.)
func (a *App) analyseChunk(ctx context.Context, run learnerRun, argv []string, chunk []pendingOccurrence, followUp bool, res *AnalyseResult) ([]pendingOccurrence, error) {
	learnings, err := a.Store.ListLearnings(ctx)
	if err != nil {
		return nil, err
	}
	occs, err := a.Store.ListLearningOccurrences(ctx, 0)
	if err != nil {
		return nil, err
	}
	quotes := sampleQuotes(occs)
	known := map[int64]bool{}
	in := make([]learn.AnalyseLearning, len(learnings))
	for i, l := range learnings {
		known[l.ID] = true
		in[i] = learn.AnalyseLearning{ID: l.ID, Status: l.Status, Text: l.Text, Quotes: quotes[l.ID]}
	}
	items := make([]learn.AnalyseItem, len(chunk))
	for i, p := range chunk {
		items[i] = learn.AnalyseItem{Index: i, Text: p.occ.Text, Quote: p.occ.Quote, Strength: p.occ.Strength, Polarity: p.occ.Polarity}
	}
	var ans learn.AnalyseAnswer
	res.Calls++
	err = learnerCall(ctx, run, argv, learn.AnalyseInput(in, items), func(stdout []byte) error {
		var cost float64
		var err error
		ans, cost, err = learn.ParseAnalyse(stdout)
		res.CostUSD += cost
		return err
	})
	if err != nil {
		return nil, err
	}
	an, err := learn.CheckAnalyse(ans, len(chunk), known)
	if err != nil {
		return nil, fmt.Errorf("the answer does not hold up, nothing of it is stored: %w", err)
	}
	if !followUp && len(chunk) > 3 && 2*an.Assigned() < len(chunk) {
		return nil, fmt.Errorf("the answer assigns %d of %d occurrences, nothing of it is stored", an.Assigned(), len(chunk))
	}
	if followUp && an.Assigned() == 0 && len(chunk) > 3 {
		return nil, fmt.Errorf("the follow-up answer assigns none of %d occurrences, nothing of it is recorded", len(chunk))
	}
	// The new learnings in the order of their first occurrence.
	var labels []string
	newIndex := map[string]int{}
	for i := range chunk {
		if l, ok := an.New[i]; ok {
			if _, ok := newIndex[l]; !ok {
				newIndex[l] = len(labels)
				labels = append(labels, l)
			}
		}
	}
	texts := make([]string, len(labels))
	for i, l := range labels {
		texts[i] = an.Texts[l]
	}
	var as []store.Assignment
	toNew := 0
	for i, p := range chunk {
		o := p.row()
		if id, ok := an.Existing[i]; ok {
			o.LearningID = id
			as = append(as, store.Assignment{Occ: o})
		} else if l, ok := an.New[i]; ok {
			as = append(as, store.Assignment{Occ: o, NewIndex: newIndex[l]})
			toNew++
		}
	}
	created, err := a.Store.AssignOccurrences(context.WithoutCancel(ctx), texts, as, a.now())
	if err != nil {
		return nil, fmt.Errorf("store the answer: %w", err)
	}
	res.Assigned += len(as)
	res.ToNew += toNew
	res.NewLearnings += len(created)
	res.DroppedLabels += len(an.Dropped)
	left := make([]pendingOccurrence, len(an.Unassigned))
	for i, idx := range an.Unassigned {
		left[i] = chunk[idx]
	}
	return left, nil
}

// LearningView is a learning with its score now.
type LearningView struct {
	store.Learning
	learn.Score
}

// Learnings returns every learning scored at now (learn.ScoreLearning),
// by id.
func (a *App) Learnings(ctx context.Context) ([]LearningView, error) {
	ls, err := a.Store.ListLearnings(ctx)
	if err != nil {
		return nil, err
	}
	occs, err := a.Store.ListLearningOccurrences(ctx, 0)
	if err != nil {
		return nil, err
	}
	by := map[int64][]learn.ScoreInput{}
	for _, o := range occs {
		by[o.LearningID] = append(by[o.LearningID], scoreInput(o))
	}
	cfg, now := a.LearnConfig(), a.now()
	out := make([]LearningView, len(ls))
	for i, l := range ls {
		out[i] = LearningView{l, learn.ScoreLearning(l.Status, l.DecidedAt, by[l.ID], now, cfg)}
	}
	return out, nil
}

// Learning returns learning id scored at now, with its occurrences
// (oldest first).
func (a *App) Learning(ctx context.Context, id int64) (LearningView, []store.LearningOccurrence, error) {
	l, err := a.Store.GetLearning(ctx, id)
	if err != nil {
		return LearningView{}, nil, err
	}
	occs, err := a.Store.ListLearningOccurrences(ctx, id)
	if err != nil {
		return LearningView{}, nil, err
	}
	in := make([]learn.ScoreInput, len(occs))
	for i, o := range occs {
		in[i] = scoreInput(o)
	}
	return LearningView{l, learn.ScoreLearning(l.Status, l.DecidedAt, in, a.now(), a.LearnConfig())}, occs, nil
}

func scoreInput(o store.LearningOccurrence) learn.ScoreInput {
	return learn.ScoreInput{Key: o.Key, ItemID: o.ItemID, TaskID: o.TaskID, Session: o.Session, At: o.At, Strength: o.Strength, Polarity: o.Polarity}
}

// applyTransitions moves each learning to the status its score now says
// (learn.Transition), only if its status is still the one scored (the
// user may decide meanwhile), deletes the candidates with no occurrence
// left, and rewrites the injection snapshot (scores change with every
// run), all in one transaction (store.ApplyStatusChanges).
func (a *App) applyTransitions(ctx context.Context) ([]LearningTransition, int, error) {
	views, err := a.Learnings(ctx)
	if err != nil {
		return nil, 0, err
	}
	cfg := a.LearnConfig()
	var changes []store.StatusChange
	texts := map[int64]string{}
	for _, v := range views {
		if to := learn.Transition(v.Status, v.Score, cfg); to != v.Status {
			changes = append(changes, store.StatusChange{ID: v.ID, From: v.Status, To: to})
			texts[v.ID] = v.Text
		}
	}
	made, n, err := a.Store.ApplyStatusChanges(ctx, changes, a.injection)
	if err != nil {
		return nil, 0, err
	}
	var out []LearningTransition
	for _, c := range made {
		out = append(out, LearningTransition{ID: c.ID, Text: texts[c.ID], From: c.From, To: c.To})
	}
	return out, n, nil
}

// injection renders the injection snapshot (store.InjectionRenderer): the
// accepted learnings, an outdated one too (the user decides), scored at
// now, highest first (the lower id first of equals), in the "User
// preferences" section (hook.Preferences) within [learn] inject_budget.
func (a *App) injection(accepted []store.Learning, occs []store.LearningOccurrence) string {
	by := map[int64][]learn.ScoreInput{}
	for _, o := range occs {
		by[o.LearningID] = append(by[o.LearningID], scoreInput(o))
	}
	cfg, now := a.LearnConfig(), a.now()
	type scored struct {
		store.Learning
		score float64
	}
	ls := make([]scored, 0, len(accepted))
	for _, l := range accepted {
		if l.Status == learn.StatusAccepted {
			ls = append(ls, scored{l, learn.ScoreLearning(l.Status, l.DecidedAt, by[l.ID], now, cfg).Score})
		}
	}
	sort.SliceStable(ls, func(i, j int) bool {
		if ls[i].score != ls[j].score {
			return ls[i].score > ls[j].score
		}
		return ls[i].ID < ls[j].ID
	})
	texts := make([]string, len(ls))
	for i, l := range ls {
		texts[i] = l.Text
	}
	return hook.Preferences(texts, cfg.InjectBudget)
}

// ErrLearnRunning is returned (wrapped) when a learning is to change while
// atl learn runs in the workspace.
var ErrLearnRunning = errors.New("atl learn is running in this workspace; try again when it is done")

// changeLearning runs change where atelier runs (what: what it does, for
// the refusal in a container) under the workspace's learn lock, which it
// does not wait for: a change must not race an Analyse chunk.
func (a *App) changeLearning(ctx context.Context, what string, change func() error) error {
	if err := outsideSandbox(what); err != nil {
		return err
	}
	if err := os.MkdirAll(a.Paths.StateDir, 0o755); err != nil {
		return err
	}
	unlock, err := lockFile(ctx, a.learnLock(), syscall.LOCK_EX, 0, "busy")
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return ErrLearnRunning
		}
		return err
	}
	defer unlock()
	return change()
}

// DecideLearning accepts (accept) or rejects learning id: a proposed or
// candidate learning can be accepted, any learning not rejected yet can
// be rejected. It records when (decided_at). Only where atelier runs,
// not while atl learn runs (changeLearning).
func (a *App) DecideLearning(ctx context.Context, id int64, accept bool) error {
	return a.changeLearning(ctx, "decide on a learning", func() error {
		if accept {
			return a.Store.SetLearningStatus(ctx, id, []string{learn.StatusCandidate, learn.StatusProposed}, learn.StatusAccepted, true, a.injection)
		}
		return a.Store.SetLearningStatus(ctx, id, []string{learn.StatusCandidate, learn.StatusProposed, learn.StatusAccepted}, learn.StatusRejected, true, a.injection)
	})
}

// EditLearning rewords learning id with text, checked as a model's
// (learn.CheckLearningText) and not another learning's
// (store.ErrLearningExists), and returns the form kept. As
// DecideLearning, only where atelier runs, not while atl learn runs.
func (a *App) EditLearning(ctx context.Context, id int64, text string) (string, error) {
	text, err := learn.CheckLearningText(text)
	if err != nil {
		return "", err
	}
	return text, a.changeLearning(ctx, "edit a learning", func() error { return a.Store.SetLearningText(ctx, id, text, a.injection) })
}

// DeleteLearning forgets learning id and its occurrences, and returns how
// many occurrences went. Occurrences still in a current digest are
// analysed again by the next atl learn (to reject a learning for good,
// reject it). As DecideLearning, only where atelier runs, not while atl
// learn runs.
func (a *App) DeleteLearning(ctx context.Context, id int64) (n int, err error) {
	err = a.changeLearning(ctx, "delete a learning", func() error {
		n, err = a.Store.DeleteLearning(ctx, id, a.injection)
		return err
	})
	return n, err
}
