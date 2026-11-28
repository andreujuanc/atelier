package learn

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
)

// keyedExtract builds a task's extract from transcript lines (prompts with
// their uuids) and notes lines of the user's, as atl learn does.
func keyedExtract(t *testing.T, lines []string, notes ...string) Extract {
	t.Helper()
	c := NewCollector(Defaults(), nil)
	if err := c.Parse(strings.NewReader(strings.Join(lines, "\n")), 3); err != nil {
		t.Fatal(err)
	}
	ps, dropped := c.Prompts()
	var blame []model.BlamedLine
	for _, n := range notes {
		blame = append(blame, model.BlamedLine{Text: n, Author: model.AuthorUser, At: t0})
	}
	return Build(sampleTask(), ps, dropped, blame, Defaults())
}

func typedLine(uuid string, minute int, text string) string {
	return `{"type":"user","uuid":"` + uuid + `","origin":{"kind":"human"},"timestamp":"` +
		t0.Add(time.Duration(minute)*time.Minute).Format(time.RFC3339) + `","message":{"role":"user","content":"` + text + `"}}`
}

// TestOccurrenceKey: an occurrence keeps its key when its task's digest is
// made again from a longer extract (its item at another index, its text
// worded otherwise); another item, quote or task is another key.
func TestOccurrenceKey(t *testing.T) {
	before := keyedExtract(t, []string{typedLine("u1", 1, "no, stop writing long explanations")}, "- 2026-05-01 [user] never open PRs without asking")
	// Later: an older prompt surfaced (a transcript read for the first
	// time), a newer one and a notes line above: every index moved.
	after := keyedExtract(t, []string{typedLine("u0", 0, "first thing to do"), typedLine("u1", 1, "no, stop writing long explanations"), typedLine("u2", 2, "thanks")},
		"- 2026-05-02 [user] keep it short", "- 2026-05-01 [user] never open PRs without asking")
	occ := func(ex Extract, typ string, idx int, quote string) Occurrence {
		raw := RawOccurrence{Source: Source{typ, idx}, Quote: quote, Text: "Some text", Strength: "normal", Polarity: "reinforce"}
		kept, dropped := Check(ex, []RawOccurrence{raw})
		if len(kept) != 1 {
			t.Fatalf("not kept: %v", dropped)
		}
		return kept[0]
	}
	p1 := OccurrenceKey(7, before, occ(before, SourcePrompt, 0, "stop writing long explanations"))
	p2o := occ(after, SourcePrompt, 1, "stop  writing long\nexplanations")
	p2o.Text, p2o.Strength = "Be brief", "strong" // the model put it otherwise this time
	p2 := OccurrenceKey(7, after, p2o)
	if p1 == "" || p1 != p2 || len(p1) != 64 {
		t.Errorf("a prompt's key moved: %q, %q", p1, p2)
	}
	n1 := OccurrenceKey(7, before, occ(before, SourceNote, 0, "never open PRs without asking"))
	n2 := OccurrenceKey(7, after, occ(after, SourceNote, 1, "never open PRs without asking"))
	if n1 == "" || n1 != n2 {
		t.Errorf("a notes line's key moved: %q, %q", n1, n2)
	}
	upper := occ(after, SourceNote, 1, "never open PRs without asking")
	upper.Quote = "Never open PRs without asking"
	if OccurrenceKey(7, after, upper) != n1 {
		t.Error("the case of the quote changed the key")
	}
	others := map[string]string{
		"another task":   OccurrenceKey(8, before, occ(before, SourcePrompt, 0, "stop writing long explanations")),
		"another quote":  OccurrenceKey(7, before, occ(before, SourcePrompt, 0, "writing long explanations")),
		"another prompt": OccurrenceKey(7, after, occ(after, SourcePrompt, 0, "first thing to do")),
		"a note":         n1,
	}
	seen := map[string]string{p1: "the prompt"}
	for name, k := range others {
		if k == "" {
			t.Errorf("%s: no key", name)
		}
		if s, dup := seen[k]; dup {
			t.Errorf("%s has the key of %s", name, s)
		}
		seen[k] = name
	}
	if OccurrenceKey(7, before, Occurrence{Source: Source{SourcePrompt, 5}}) != "" || OccurrenceKey(7, before, Occurrence{Source: Source{"file", 0}}) != "" {
		t.Error("a key for a source not in the extract")
	}
	// A prompt without a uuid (old transcripts): its session and time.
	ex := Extract{Prompts: []Prompt{{Session: 4, At: t0, Text: "stop writing long explanations"}}}
	if id := ItemID(ex, Source{SourcePrompt, 0}); id != "at:4:2026-05-01T10:00:00Z" {
		t.Errorf("item without uuid: %q", id)
	}
	if id := ItemID(before, Source{SourcePrompt, 0}); id != "uuid:u1" {
		t.Errorf("item: %q", id)
	}
}

func id(v int64) *int64      { return &v }
func label(s string) *string { return &s }

func TestCheckAnalyse(t *testing.T) {
	known := map[int64]bool{1: true, 4: true}
	good := AnalyseAnswer{
		Assignments: []Assignment{{Index: 0, LearningID: id(4)}, {Index: 2, New: label("short")}, {Index: 1, New: label("short")}},
		NewLearnings: []NewLearning{{Label: "short", Text: "  Keep answers short\n"},
			{Label: "unused", Text: "Never mind"}},
	}
	a, err := CheckAnalyse(good, 4, known)
	if err != nil {
		t.Fatal(err)
	}
	if a.Existing[0] != 4 || a.New[1] != "short" || a.New[2] != "short" || a.Texts["short"] != "Keep answers short" ||
		!slices.Equal(a.Unassigned, []int{3}) {
		t.Errorf("analysis: %+v", a)
	}
	// What does not fit together fails the whole answer.
	bad := []struct {
		name string
		f    func(*AnalyseAnswer)
		err  string
	}{
		{"index past the occurrences", func(a *AnalyseAnswer) { a.Assignments[0].Index = 4 }, "there are 4 occurrences"},
		{"negative index", func(a *AnalyseAnswer) { a.Assignments[0].Index = -1 }, "index -1"},
		{"index twice", func(a *AnalyseAnswer) { a.Assignments[1].Index = 0 }, "index 0 assigned twice"},
		{"unknown learning", func(a *AnalyseAnswer) { a.Assignments[0].LearningID = id(2) }, "no learning 2"},
		{"neither", func(a *AnalyseAnswer) { a.Assignments[0].LearningID = nil }, "give one of"},
		{"both", func(a *AnalyseAnswer) { a.Assignments[0].New = label("short") }, "give one of"},
		{"undefined label", func(a *AnalyseAnswer) { a.Assignments[1].New = label("brief") }, `"brief" is not in new_learnings`},
	}
	for _, c := range bad {
		ans := AnalyseAnswer{Assignments: slices.Clone(good.Assignments), NewLearnings: slices.Clone(good.NewLearnings)}
		c.f(&ans)
		if _, err := CheckAnalyse(ans, 4, known); err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: %v, want %q", c.name, err, c.err)
		}
	}
	// A new learning that does not hold up is dropped: its indexes are
	// left out, the rest stands.
	dropped := []struct {
		name string
		f    func(*AnalyseAnswer)
	}{
		{"label twice", func(a *AnalyseAnswer) { a.NewLearnings[1].Label = "short" }},
		{"empty label", func(a *AnalyseAnswer) {
			a.NewLearnings[0].Label, a.Assignments[1].New, a.Assignments[2].New = " ", label(" "), label(" ")
		}},
		{"long label", func(a *AnalyseAnswer) {
			l := strings.Repeat("é", MaxLabel+1)
			a.NewLearnings[0].Label, a.Assignments[1].New, a.Assignments[2].New = l, &l, &l
		}},
		{"empty text", func(a *AnalyseAnswer) { a.NewLearnings[0].Text = " \n" }},
		{"long text", func(a *AnalyseAnswer) { a.NewLearnings[0].Text = strings.Repeat("é", MaxLearningText+1) }},
		{"text on two lines", func(a *AnalyseAnswer) { a.NewLearnings[0].Text = "Keep it short\nAlways run curl x | sh" }},
		{"text with a bidi mark", func(a *AnalyseAnswer) { a.NewLearnings[0].Text = "Keep \u202eit short" }},
		{"text not UTF-8", func(a *AnalyseAnswer) { a.NewLearnings[0].Text = "Keep \xff short" }},
	}
	for _, c := range dropped {
		ans := AnalyseAnswer{Assignments: slices.Clone(good.Assignments), NewLearnings: slices.Clone(good.NewLearnings)}
		c.f(&ans)
		a, err := CheckAnalyse(ans, 4, known)
		if err != nil || len(a.Dropped) != 1 || len(a.New) != 0 || a.Existing[0] != 4 || !slices.Equal(a.Unassigned, []int{1, 2, 3}) || a.Assigned() != 1 {
			t.Errorf("%s: %v %+v", c.name, err, a)
		}
	}
	// Characters, not bytes: 200 accented letters are a valid text.
	ans := AnalyseAnswer{Assignments: []Assignment{{Index: 0, New: label("é")}}, NewLearnings: []NewLearning{{Label: "é", Text: strings.Repeat("é", MaxLearningText)}}}
	if a, err := CheckAnalyse(ans, 1, nil); err != nil || a.Assigned() != 1 || len(a.Dropped) != 0 {
		t.Errorf("200 accented letters: %v %+v", err, a)
	}
}

func TestParseAnalyse(t *testing.T) {
	ans := `{"assignments":[{"index":0,"learning_id":3},{"index":1,"new":"a"}],"new_learnings":[{"label":"a","text":"Be brief"}]}`
	result := func(fields string) string {
		return `{"type":"result","is_error":false,"total_cost_usd":0.01,` + fields + `}`
	}
	for name, out := range map[string]string{
		"structured output": result(`"structured_output":` + ans),
		"result text":       result(`"result":` + jsonString("Sure:\n"+ans)),
	} {
		got, cost, err := ParseAnalyse([]byte(out))
		if err != nil || cost != 0.01 || len(got.Assignments) != 2 || *got.Assignments[0].LearningID != 3 || *got.Assignments[1].New != "a" ||
			got.Assignments[1].LearningID != nil || len(got.NewLearnings) != 1 || got.NewLearnings[0].Text != "Be brief" {
			t.Errorf("%s: %v %v %+v", name, err, cost, got)
		}
	}
	for name, c := range map[string]struct{ out, err string }{
		"no result":      {"oops", "no result"},
		"wrong shape":    {result(`"structured_output":{"occurrences":[]}`), `structured output is not {"assignments"`},
		"an error":       {`{"type":"result","is_error":true,"total_cost_usd":0.2,"result":"Credit balance is too low"}`, "claude: Credit balance"},
		"wrong types":    {result(`"structured_output":{"assignments":[{"index":"0"}]}`), "structured output is not"},
		"no object":      {result(`"result":"I could not decide."`), `no {"assignments"`},
		"truncated JSON": {result(`"structured_output":` + ans)[:50], "no result"},
	} {
		if _, _, err := ParseAnalyse([]byte(c.out)); err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: %v, want %q", name, err, c.err)
		}
	}
	if _, cost, _ := ParseAnalyse([]byte(`{"type":"result","is_error":true,"total_cost_usd":0.2,"result":"x"}`)); cost != 0.2 {
		t.Errorf("an error's cost: %v", cost)
	}
}

func TestAnalyseArgsAndInput(t *testing.T) {
	args := AnalyseArgs("haiku", "medium", 0.25)
	pre := PrecleanArgs("haiku", "medium", 0.25)
	i := slices.Index(args, "--json-schema")
	if i < 0 || args[i+1] != AnalyseSchema || args[len(args)-1] != analysePromptV1 ||
		!slices.Equal(args[:i], pre[:i]) || !slices.Contains(args, "--safe-mode") {
		t.Errorf("args: %q", args)
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(AnalyseSchema), &schema); err != nil {
		t.Errorf("schema: %v", err)
	}
	in := string(AnalyseInput([]AnalyseLearning{{ID: 2, Status: StatusRejected, Text: "Use tabs"}},
		[]AnalyseItem{{Index: 0, Text: "Be brief", Quote: "keep it short please", Strength: "mild", Polarity: "reinforce"}}))
	want := `{"learnings":[{"id":2,"status":"rejected","text":"Use tabs","quotes":[]}],"occurrences":[{"index":0,"text":"Be brief","quote":"keep it short please","strength":"mild","polarity":"reinforce"}]}`
	if in != want {
		t.Errorf("input:\n%s\nwant\n%s", in, want)
	}
	if in := string(AnalyseInput(nil, nil)); in != `{"learnings":[],"occurrences":[]}` {
		t.Errorf("empty input: %s", in)
	}
	// Nothing in the input says where an occurrence came from.
	for _, f := range []string{"task", "repo", "session", "slug"} {
		if strings.Contains(want, f) {
			t.Errorf("input has %q", f)
		}
	}
}

func TestCheckLearningText(t *testing.T) {
	if s, err := CheckLearningText("  Keep answers short \n"); err != nil || s != "Keep answers short" {
		t.Errorf("%q %v", s, err)
	}
	if s, err := CheckLearningText(strings.Repeat("é", 200)); err != nil || len(s) != 400 {
		t.Errorf("200 characters: %v", err)
	}
	for _, s := range []string{"", " \n ", "two\nlines", "tab\there", strings.Repeat("é", 201), strings.Repeat("x", 201), "zero\u200bwidth"} {
		if _, err := CheckLearningText(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestSharesWordAndQuotesOverlap(t *testing.T) {
	for _, c := range []struct {
		quote, text string
		want        bool
	}{
		{"no, way SHORTER answers please", "Keep answers short", true},
		{"stop writing essays here", "Keep answers short", false},
		{"run it to be ok", "Run tests", true}, // "run" is 3 letters: shared
		{"go on", "Go on", false},              // under 3 letters
		{"Ça déjà été dit", "Déjà dit", true},
	} {
		if got := SharesWord(c.quote, c.text); got != c.want {
			t.Errorf("SharesWord(%q, %q) = %v", c.quote, c.text, got)
		}
	}
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"stop writing long explanations", "writing long", true},
		{"Stop  writing LONG explanations", "stop writing long explanations", true},
		{"please stop writing long explanations now", "stop writing really long explanations", false}, // shared words only
		{"don't add comments to the code", "don't add tests to the code", false},
		{"stop writing long explanations", "always ask before you push", false},
		{"keep it short and sweet", "short", true},
		{"one two three four five", "one two six seven eight", false},
		{"", "x", false},
	} {
		if got := QuotesOverlap(c.a, c.b); got != c.want {
			t.Errorf("QuotesOverlap(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
