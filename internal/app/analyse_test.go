package app

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/learn"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/store"
)

// analyseInputs returns what each Analyse call read, in call order.
func (e *env) analyseInputs(t *testing.T) []map[string]json.RawMessage {
	t.Helper()
	var out []map[string]json.RawMessage
	for i := 0; ; i++ {
		p := filepath.Join(e.out, "analyse-stdin."+strconv.Itoa(i))
		b, err := os.ReadFile(p)
		if err != nil {
			return out
		}
		var in map[string]json.RawMessage
		if err := json.Unmarshal(b, &in); err != nil {
			t.Fatalf("analyse input %d: %v\n%s", i, err, b)
		}
		out = append(out, in)
	}
}

// TestLearnAnalyse: atl learn assigns the occurrences of the current
// digests to learnings once each (by key, also when a digest is made
// again; a re-quoted item joins its learning without a call), checks the
// answer before it stores anything, shows rejected learnings so they get
// their matches, and moves statuses at the end.
func TestLearnAnalyse(t *testing.T) {
	e := newEnv(t)
	learnStub(t, e)
	ctx := context.Background()
	a := e.learnTask(t, "a", "please write no prose\n")
	e.learnTask(t, "b", "- 2026-05-01 [user] write no prose, please\n")
	e.setStub(t, "analyse", analyseAnswer(`[{"index":0,"new":"np"},{"index":1,"new":"np"}]`, `[{"label":"np","text":"Write no prose"},{"label":"x","text":"Unused"}]`))

	run, err := e.app.Learn(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	r := run.Analyse
	if len(run.Tasks) != 2 || r.Err != nil || r.New != 2 || r.Assigned != 2 || r.ToNew != 2 || r.NewLearnings != 1 || r.Calls != 1 || r.CostUSD != 0.01 || r.Unassigned != 0 {
		t.Fatalf("analyse: %+v", r)
	}
	// "No prose", strong, in two tasks: 4 ≥ 3, proposed at once.
	if len(r.Transitions) != 1 || r.Transitions[0].To != learn.StatusProposed || r.Transitions[0].From != learn.StatusCandidate {
		t.Errorf("transitions: %+v", r.Transitions)
	}
	vs, err := e.app.Learnings(ctx)
	if err != nil || len(vs) != 1 || vs[0].Text != "Write no prose" || vs[0].Status != learn.StatusProposed || vs[0].Tasks != 2 || vs[0].Score.Score < 3.9 {
		t.Fatalf("learnings: %+v %v", vs, err)
	}
	np := vs[0].ID
	score := vs[0].Score.Score
	ins := e.analyseInputs(t)
	if len(ins) != 1 || string(ins[0]["learnings"]) != "[]" {
		t.Fatalf("analyse inputs: %v", ins)
	}
	var items []map[string]any
	if err := json.Unmarshal(ins[0]["occurrences"], &items); err != nil || len(items) != 2 {
		t.Fatalf("occurrences in: %s", ins[0]["occurrences"])
	}
	for i, it := range items {
		if it["index"] != float64(i) || it["text"] != "No prose" || it["quote"] != "write no prose" || it["strength"] != "strong" || len(it) != 5 {
			t.Errorf("item %d: %v", i, it)
		}
	}
	if argv := readFile(t, filepath.Join(e.out, "claude-argv")); !strings.Contains(argv, "--effort\x00medium\x00") || !strings.Contains(argv, learn.AnalyseSchema) {
		t.Errorf("analyse argv: %q", argv)
	}

	// Again: nothing new, no call.
	if run, err = e.app.Learn(ctx, "", false); err != nil || run.Analyse.New != 0 || run.Analyse.Calls != 0 || len(e.analyseInputs(t)) != 1 {
		t.Fatalf("again: %v %+v", err, run.Analyse)
	}

	// a's digest is made again (a notes line above: every index moves);
	// its occurrence is the same one: nothing new.
	if _, err := e.app.Store.ReplaceNotes(ctx, a.ID, model.AuthorUser, "keep it short\nplease write no prose\n"); err != nil {
		t.Fatal(err)
	}
	e.setStub(t, "answer", strings.ReplaceAll(precleanAnswer, `"index":0`, `"index":1`))
	if run, err = e.app.Learn(ctx, "a", false); err != nil || run.Tasks[0].Status != PrecleanRan || len(run.Tasks[0].Occurrences) != 1 || run.Analyse.New != 0 {
		t.Fatalf("re-made digest: %v %+v %+v", err, run.Tasks, run.Analyse)
	}
	// Made again once more, the same line quoted longer: another key, but
	// the same item with an overlapping quote: it joins the learning, no
	// call, and the score does not count the line twice.
	if _, err := e.app.Store.ReplaceNotes(ctx, a.ID, model.AuthorUser, "keep it short\nplease write no prose\nthanks\n"); err != nil {
		t.Fatal(err)
	}
	e.setStub(t, "answer", strings.ReplaceAll(strings.ReplaceAll(precleanAnswer, `"index":0`, `"index":1`), `"quote":"write no prose"`, `"quote":"please write no prose"`))
	if run, err = e.app.Learn(ctx, "a", false); err != nil || run.Analyse.New != 1 || run.Analyse.Joined != 1 || run.Analyse.Calls != 0 {
		t.Fatalf("re-quoted: %v %+v", err, run.Analyse)
	}
	if v, occs, _ := e.app.Learning(ctx, np); len(occs) != 3 || !near(v.Score.Score, score) {
		t.Errorf("re-quoted: score %.3f (was %.3f), %d occurrences", v.Score.Score, score, len(occs))
	}

	// Rejected: Analyse still sees it, with its quotes (one per task
	// first), and may give it what says the same; it stays rejected.
	if err := e.app.DecideLearning(ctx, np, false); err != nil {
		t.Fatal(err)
	}
	e.learnTask(t, "c", "write no prose\n")
	e.setStub(t, "answer", precleanAnswer)
	e.setStub(t, "analyse", analyseAnswer(`[{"index":0,"learning_id":`+strconv.Itoa(int(np))+`}]`, `[]`))
	if run, err = e.app.Learn(ctx, "", false); err != nil || run.Analyse.Assigned != 1 || run.Analyse.NewLearnings != 0 || len(run.Analyse.Transitions) != 0 {
		t.Fatalf("to the rejected one: %v %+v", err, run.Analyse)
	}
	ins = e.analyseInputs(t)
	var ls []learn.AnalyseLearning
	if err := json.Unmarshal(ins[len(ins)-1]["learnings"], &ls); err != nil || len(ls) != 1 || ls[0].ID != np || ls[0].Status != learn.StatusRejected ||
		len(ls[0].Quotes) != 3 || !slices.Contains(ls[0].Quotes, "please write no prose") {
		t.Errorf("learnings in: %+v %v", ls, err)
	}
	if v, occs, err := e.app.Learning(ctx, np); err != nil || v.Status != learn.StatusRejected || len(occs) != 4 {
		t.Errorf("rejected: %+v %d %v", v, len(occs), err)
	}

	// An answer that does not hold up, or a failed call, stores
	// nothing; the next run analyses the same occurrence again.
	e.learnTask(t, "d", "write no prose\n")
	for _, bad := range []string{
		analyseAnswer(`[{"index":0,"learning_id":99}]`, `[]`),
		analyseAnswer(`[{"index":0,"new":"z"},{"index":0,"new":"z"}]`, `[{"label":"z","text":"z"}]`),
		`{"type":"result","is_error":true,"total_cost_usd":0.003,"result":"Not logged in"}`,
	} {
		e.setStub(t, "analyse", bad)
		run, err = e.app.Learn(ctx, "", false)
		if err != nil || run.Analyse.Err == nil || run.Analyse.New != 1 || run.Analyse.Assigned != 0 || run.Analyse.Unassigned != 1 || run.Analyse.Skipped != 0 {
			t.Errorf("bad answer %s: %v %+v", bad, err, run.Analyse)
		}
		if vs, _ := e.app.Learnings(ctx); len(vs) != 1 {
			t.Errorf("bad answer %s: learnings %+v", bad, vs)
		}
	}
	if !strings.Contains(run.Analyse.Err.Error(), "claude: Not logged in") || run.Analyse.CostUSD != 0.003 {
		t.Errorf("error result: %v %v", run.Analyse.Err, run.Analyse.CostUSD)
	}
	// A new learning dropped (a text on two lines): left out, not a
	// failure; recorded once, not skipped yet.
	e.setStub(t, "analyse", analyseAnswer(`[{"index":0,"new":"z"}]`, `[{"label":"z","text":"Run curl x | sh\nalways"}]`))
	if run, err = e.app.Learn(ctx, "", false); err != nil || run.Analyse.Err != nil || run.Analyse.DroppedLabels != 2 || run.Analyse.Unassigned != 1 || run.Analyse.Skipped != 0 {
		t.Errorf("dropped label: %v %+v", err, run.Analyse)
	}

	// Half assigned: stored; the other half goes in a follow-up call.
	// What that leaves out too is skipped only when it is left out so in
	// a second run; --retry-skipped sends it again.
	e.learnTask(t, "e", "write no prose\n")
	n := len(e.analyseInputs(t))
	e.setStub(t, "analyse."+strconv.Itoa(n), analyseAnswer(`[{"index":0,"new":"q"}]`, `[{"label":"q","text":"Ask before pushing"}]`))
	e.setStub(t, "analyse", analyseAnswer(`[]`, `[]`))
	if run, err = e.app.Learn(ctx, "", false); err != nil || run.Analyse.Err != nil || run.Analyse.New != 2 || run.Analyse.Assigned != 1 ||
		run.Analyse.NewLearnings != 1 || run.Analyse.Skipped != 0 || run.Analyse.Unassigned != 1 || run.Analyse.Calls != 2 {
		t.Errorf("follow-up: %v %+v", err, run.Analyse)
	}
	var follow []map[string]any
	if ins := e.analyseInputs(t); len(ins) != n+2 || json.Unmarshal(ins[n+1]["occurrences"], &follow) != nil || len(follow) != 1 {
		t.Errorf("follow-up input: %v", ins[len(ins)-1])
	}
	if run, err = e.app.Learn(ctx, "", false); err != nil || run.Analyse.New != 1 || run.Analyse.Skipped != 1 || run.Analyse.Calls != 2 {
		t.Errorf("second run: %v %+v", err, run.Analyse)
	}
	if run, err = e.app.Learn(ctx, "", false); err != nil || run.Analyse.New != 0 || run.Analyse.Calls != 0 {
		t.Errorf("after the skip: %v %+v", err, run.Analyse)
	}
	if run, err = e.app.Learn(ctx, "", true); err != nil || run.RetriedSkips != 1 || run.Analyse.New != 1 || run.Analyse.Skipped != 0 {
		t.Errorf("--retry-skipped: %v %d %+v", err, run.RetriedSkips, run.Analyse)
	}
}

// TestLearnAnalyseFloors: an answer that assigns under half of a chunk
// of more than 3, or a follow-up that assigns none of more than 3, stores
// and records nothing.
func TestLearnAnalyseFloors(t *testing.T) {
	e := newEnv(t)
	learnStub(t, e)
	ctx := context.Background()
	for _, s := range []string{"a", "b", "c", "d", "e"} {
		e.learnTask(t, s, "write no prose\n")
	}
	e.setStub(t, "analyse", analyseAnswer(`[{"index":0,"new":"np"},{"index":1,"new":"np"}]`, `[{"label":"np","text":"Write no prose"}]`))
	run, err := e.app.Learn(ctx, "", false)
	if err != nil || run.Analyse.Err == nil || !strings.Contains(run.Analyse.Err.Error(), "assigns 2 of 5") || run.Analyse.Assigned != 0 || run.Analyse.Unassigned != 5 {
		t.Errorf("under half: %v %+v", err, run.Analyse)
	}
	e.setStub(t, "analyse.1", analyseAnswer(`[{"index":0,"new":"np"},{"index":1,"new":"np"},{"index":2,"new":"np"}]`, `[{"label":"np","text":"Write no prose"}]`))
	e.setStub(t, "analyse", analyseAnswer(`[]`, `[]`)) // follow-up: none of 2 (allowed: 2 ≤ 3)
	if run, err = e.app.Learn(ctx, "", false); err != nil || run.Analyse.Err != nil || run.Analyse.Assigned != 3 || run.Analyse.Unassigned != 2 || run.Analyse.Calls != 2 {
		t.Errorf("3 of 5, then a follow-up of 2: %v %+v", err, run.Analyse)
	}
	for _, s := range []string{"f", "g", "h", "i", "j", "k", "l", "m"} {
		e.learnTask(t, s, "write no prose\n")
	}
	// 10 new: 5 assigned, the follow-up assigns none of 5: failed, nothing recorded.
	e.setStub(t, "analyse.3", analyseAnswer(`[{"index":0,"new":"np"},{"index":1,"new":"np"},{"index":2,"new":"np"},{"index":3,"new":"np"},{"index":4,"new":"np"}]`,
		`[{"label":"np","text":"Write no prose"}]`))
	if run, err = e.app.Learn(ctx, "", false); err != nil || run.Analyse.New != 10 || run.Analyse.Assigned != 5 || run.Analyse.Err == nil ||
		!strings.Contains(run.Analyse.Err.Error(), "follow-up answer assigns none of 5") || run.Analyse.Unassigned != 5 || run.Analyse.Skipped != 0 {
		t.Errorf("empty follow-up of 5: %v %+v", err, run.Analyse)
	}
	// The 2 left out the first time are still not skipped: they failed
	// with the rest this run (nothing recorded), so they are sent again.
	if run, err = e.app.Learn(ctx, "", false); err != nil || run.Analyse.New != 5 {
		t.Errorf("next run: %v %+v", err, run.Analyse)
	}
}

// TestLearnAnalyseChunks: past analyseChunk new occurrences, Analyse
// runs per chunk, each stored before the next is sent (so the next knows
// the learnings it made); a chunk that fails stores nothing and the
// others go on.
func TestLearnAnalyseChunks(t *testing.T) {
	e := newEnv(t)
	learnStub(t, e)
	ctx := context.Background()
	old := analyseChunk
	analyseChunk = 1
	t.Cleanup(func() { analyseChunk = old })
	for _, s := range []string{"a", "b", "c"} {
		e.learnTask(t, s, "write no prose\n")
	}
	e.setStub(t, "analyse", analyseAnswer(`[{"index":0,"new":"np"}]`, `[{"label":"np","text":"Write no prose"}]`))
	e.setStub(t, "analyse.1", analyseAnswer(`[{"index":0,"new":"np"}]`, `[{"label":"np","text":"WRITE NO PROSE"}]`)) // the same text, by case folding
	run, err := e.app.Learn(ctx, "", false)
	if err != nil || run.Analyse.Calls != 3 || run.Analyse.Assigned != 3 || run.Analyse.NewLearnings != 1 || run.Analyse.CostUSD != 0.03 {
		t.Fatalf("chunks: %v %+v", err, run.Analyse)
	}
	ins := e.analyseInputs(t)
	if len(ins) != 3 || string(ins[0]["learnings"]) != "[]" || !strings.Contains(string(ins[1]["learnings"]), `"text":"Write no prose"`) {
		t.Errorf("inputs: %v", ins)
	}
	vs, _ := e.app.Learnings(ctx)
	if len(vs) != 1 || vs[0].Tasks != 3 {
		t.Errorf("learnings: %+v", vs)
	}

	for _, s := range []string{"d", "e", "f"} {
		e.learnTask(t, s, "write no prose\n")
	}
	e.setStub(t, "analyse.3", analyseAnswer(`[{"index":0,"new":"np"}]`, `[{"label":"np","text":"Write no prose"}]`))
	e.setStub(t, "analyse.4", analyseAnswer(`[{"index":1,"new":"np"}]`, `[{"label":"np","text":"Write no prose"}]`)) // out of range: fails
	e.setStub(t, "analyse.5", analyseAnswer(`[{"index":0,"learning_id":1}]`, `[]`))
	run, err = e.app.Learn(ctx, "", false)
	if err != nil || run.Analyse.Calls != 3 || run.Analyse.Err == nil || run.Analyse.Unassigned != 1 || run.Analyse.Assigned != 2 {
		t.Errorf("a failed chunk: %v %+v", err, run.Analyse)
	}
}

// TestLearnTransitions: a proposal fades back to candidate as its
// occurrences age; accepted learnings stay accepted; the user's decision
// meanwhile wins.
func TestLearnTransitions(t *testing.T) {
	e := newEnv(t)
	learnStub(t, e)
	ctx := context.Background()
	e.learnTask(t, "a", "write no prose\n")
	e.learnTask(t, "b", "write no prose\n")
	e.setStub(t, "analyse", analyseAnswer(`[{"index":0,"new":"np"},{"index":1,"new":"np"}]`, `[{"label":"np","text":"Write no prose"}]`))
	if _, err := e.app.Learn(ctx, "", false); err != nil {
		t.Fatal(err)
	}
	vs, _ := e.app.Learnings(ctx)
	if len(vs) != 1 || vs[0].Status != learn.StatusProposed {
		t.Fatalf("learnings: %+v", vs)
	}
	id := vs[0].ID
	later := time.Now().Add(200 * 24 * time.Hour)
	e.app.Now = func() time.Time { return later }
	run, err := e.app.Learn(ctx, "", false)
	if err != nil || len(run.Analyse.Transitions) != 1 || run.Analyse.Transitions[0].To != learn.StatusCandidate {
		t.Fatalf("fade: %v %+v", err, run.Analyse)
	}
	if v, _, _ := e.app.Learning(ctx, id); v.Status != learn.StatusCandidate || v.Trend >= 0 {
		t.Errorf("faded: %+v", v)
	}
	// Accepted (from candidate): a year later still accepted.
	if err := e.app.DecideLearning(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	later = later.Add(365 * 24 * time.Hour)
	if run, err := e.app.Learn(ctx, "", false); err != nil || len(run.Analyse.Transitions) != 0 {
		t.Errorf("accepted: %v %+v", err, run.Analyse)
	}
	if v, _, _ := e.app.Learning(ctx, id); v.Status != learn.StatusAccepted || v.DecidedAt.IsZero() || v.Outdated || v.StillCorrected {
		t.Errorf("accepted: %+v", v)
	}
	if err := e.app.DecideLearning(ctx, id, true); err == nil {
		t.Error("accepted twice")
	}
	if err := e.app.DecideLearning(ctx, id, false); err != nil {
		t.Errorf("reject an accepted one: %v", err)
	}
	if err := e.app.DecideLearning(ctx, id, true); err == nil {
		t.Error("accepted a rejected one")
	}
	if _, err := e.app.EditLearning(ctx, id, "two\nlines"); err == nil {
		t.Error("edited to two lines")
	}
	if text, err := e.app.EditLearning(ctx, id, " Write no prose at all \n"); err != nil || text != "Write no prose at all" {
		t.Errorf("edit: %q %v", text, err)
	}
	if n, err := e.app.DeleteLearning(ctx, id); err != nil || n != 2 {
		t.Errorf("delete: %d %v", n, err)
	}
	// Forgotten: the next run analyses its occurrences again.
	e.setStub(t, "analyse", analyseAnswer(`[]`, `[]`))
	if run, err := e.app.Learn(ctx, "", false); err != nil || run.Analyse.New != 2 {
		t.Errorf("after rm: %v %+v", err, run.Analyse)
	}
	if _, err := e.app.Store.GetLearning(ctx, id); err == nil || !strings.Contains(err.Error(), store.ErrNotFound.Error()) {
		t.Errorf("deleted: %v", err)
	}
}

// TestLearnSandboxOneLearner: with the sandbox on, the pre-clean and
// Analyse calls of a run share one learner container, removed at the end;
// Analyse runs there with its own arguments.
func TestLearnSandboxOneLearner(t *testing.T) {
	e, fd := sandboxEnv(t)
	learnStub(t, e)
	learnerDockerStub(t, e)
	ctx := context.Background()
	e.learnTask(t, "a", "write no prose\n")
	e.setStub(t, "analyse", analyseAnswer(`[{"index":0,"new":"np"}]`, `[{"label":"np","text":"Write no prose"}]`))
	run, err := e.app.Learn(ctx, "", false)
	if err != nil || run.Tasks[0].Status != PrecleanRan || run.Analyse.Assigned != 1 || run.Analyse.Err != nil {
		t.Fatalf("%v %+v %+v", err, run.Tasks, run.Analyse)
	}
	if n := e.stubCount(t, "calls"); n != 2 || len(fd.find("run")) != 1 {
		t.Errorf("%d calls, docker run %q", n, fd.find("run"))
	}
	ch := fd.changes()
	if last := ch[len(ch)-1]; strings.Join(last, " ") != "rm -f "+fakeContainerID {
		t.Errorf("last docker call: %q", last)
	}
	execArgs := strings.Split(strings.TrimSuffix(readFile(t, filepath.Join(e.out, "docker-exec")), "\x00"), "\x00")
	cfg := learn.Defaults()
	want := append([]string{"exec", "-i", "-e", "CLAUDE_CODE_OAUTH_TOKEN", fakeContainerID, "sh", "-c", learnerTimeoutScript, "290"},
		learn.AnalyseArgs(cfg.AnalyseModel, cfg.AnalyseEffort, cfg.MaxCallUSD)...)
	if strings.Join(execArgs, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("docker exec:\n%q\nwant\n%q", execArgs, want)
	}
	// Nothing new next time: no learner at all.
	if run, err := e.app.Learn(ctx, "", false); err != nil || run.Analyse.New != 0 || len(fd.find("run")) != 1 {
		t.Errorf("again: %v %+v, docker run %d", err, run.Analyse, len(fd.find("run")))
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

// TestLearningChangesWaitForNoLearn: accept, reject, edit and rm do not
// run while atl learn holds the workspace's lock (no race with an Analyse
// chunk), and say so at once.
func TestLearningChangesWaitForNoLearn(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	task := e.learnTask(t, "a", "")
	if _, err := e.app.Store.AssignOccurrences(ctx, []string{"Write no prose", "Ask before pushing"}, []store.Assignment{
		{Occ: store.LearningOccurrence{Key: "k1", TaskID: task.ID, At: time.Now(), Strength: "normal", Polarity: "reinforce", Quote: "write no prose", Text: "x"}},
		{Occ: store.LearningOccurrence{Key: "k2", TaskID: task.ID, At: time.Now(), Strength: "normal", Polarity: "reinforce", Quote: "ask before pushing", Text: "y"}, NewIndex: 1},
	}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	unlock, err := e.app.lockLearn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	errs := []error{e.app.DecideLearning(ctx, 1, true), e.app.DecideLearning(ctx, 1, false)}
	_, err = e.app.EditLearning(ctx, 1, "Write no prose at all")
	errs = append(errs, err)
	_, err = e.app.DeleteLearning(ctx, 1)
	errs = append(errs, err)
	for i, err := range errs {
		if !errors.Is(err, ErrLearnRunning) {
			t.Errorf("change %d while atl learn runs: %v", i, err)
		}
	}
	if time.Since(start) > time.Second {
		t.Errorf("waited %s", time.Since(start))
	}
	unlock()
	if l, _ := e.app.Store.GetLearning(ctx, 1); l.Status != learn.StatusCandidate || l.Text != "Write no prose" {
		t.Errorf("changed: %+v", l)
	}
	if err := e.app.DecideLearning(ctx, 1, true); err != nil {
		t.Error(err)
	}
	// Another learning's text, by Unicode case folding: refused.
	if _, err := e.app.EditLearning(ctx, 1, "ASK BEFORE PUSHING"); !errors.Is(err, store.ErrLearningExists) {
		t.Errorf("edit to another's text: %v", err)
	}
}

// TestSampleQuotes: a learning's sample quotes come from different tasks
// first, newest first, so one task cannot fill them.
func TestSampleQuotes(t *testing.T) {
	o := func(learning, task int64, quote string, pol string) store.LearningOccurrence {
		return store.LearningOccurrence{LearningID: learning, TaskID: task, Quote: quote, Polarity: pol}
	}
	r := learn.PolarityReinforce
	got := sampleQuotes([]store.LearningOccurrence{ // oldest first
		o(1, 2, "b old", r), o(1, 3, "c", r), o(1, 1, "a1", r), o(1, 1, "a2", r), o(1, 1, "a3", r), o(1, 4, "against", learn.PolarityContradict),
		o(2, 1, "x1", r), o(2, 1, "x2", r), o(2, 1, "x3", r), o(2, 1, "x4", r),
	})
	if strings.Join(got[1], ",") != "a3,c,b old" || strings.Join(got[2], ",") != "x4,x3,x2" {
		t.Errorf("quotes: %v", got)
	}
}
