package learn

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
)

var t0 = time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

func sampleTask() TaskInfo {
	return TaskInfo{Slug: "fix", Title: "Fix login", Status: "done", Repos: []string{"api", "web"}}
}

func sampleBlame() []model.BlamedLine {
	return []model.BlamedLine{
		{Text: "- 2026-05-01 [user] no prose", Author: model.AuthorUser, At: t0},
		{Text: "", Blank: true},
		{Text: "- 2026-05-01 [user] ran the tests", Author: model.AuthorAgent, At: t0.Add(time.Hour)},
		{Text: "\u200b", Blank: true},
		{Text: "old line", Author: model.AuthorUnknown},
		{Text: "from the assistant", Author: model.AuthorAssistant, At: t0},
	}
}

func TestTaskInfoOf(t *testing.T) {
	got := TaskInfoOf(&model.Task{Slug: "fix", Title: "Fix login", Status: model.StatusDone,
		Repos: []model.TaskRepo{{Name: "api", Mode: model.ModeRW}, {Name: "web", Mode: model.ModeRO}}})
	if fmt.Sprint(got) != fmt.Sprint(sampleTask()) {
		t.Errorf("%+v", got)
	}
	if got := TaskInfoOf(&model.Task{Slug: "x"}); got.Repos == nil {
		t.Error("repos nil: JSON null")
	}
}

func TestBuild(t *testing.T) {
	prompts := []Prompt{
		{Session: 2, At: t0.Add(2 * time.Minute), Text: "second session"},
		{Session: 1, At: t0, Text: "first"},
		{Session: 1, At: t0.Add(3 * time.Minute), Text: "last"},
		{Session: 2, At: t0, Text: "same time, after first"},
	}
	ex := Build(sampleTask(), prompts, 0, sampleBlame(), Defaults())
	var texts []string
	for _, p := range ex.Prompts {
		texts = append(texts, p.Text)
	}
	if got := strings.Join(texts, " | "); got != "first | same time, after first | second session | last" {
		t.Errorf("order: %s", got)
	}
	if prompts[0].Text != "second session" {
		t.Error("the caller's slice was reordered")
	}
	wantNotes := []NoteLine{
		{Author: "user", At: "2026-05-01T10:00:00Z", Text: "- 2026-05-01 [user] no prose"},
		{Author: "agent", At: "2026-05-01T11:00:00Z", Text: "- 2026-05-01 [user] ran the tests"},
		{Author: "unknown", Text: "old line"},
		{Author: "assistant", At: "2026-05-01T10:00:00Z", Text: "from the assistant"},
	}
	if fmt.Sprint(ex.Notes) != fmt.Sprint(wantNotes) {
		t.Errorf("notes:\n%+v\nwant\n%+v", ex.Notes, wantNotes)
	}
	if ex.DroppedPrompts != 0 || len(ex.InputHash) != 64 {
		t.Errorf("dropped %d, hash %q", ex.DroppedPrompts, ex.InputHash)
	}

	// The JSON form: fields in order, no HTML escaping, empty lists as [].
	empty := Build(TaskInfo{Slug: "e", Status: "active"}, nil, 0, nil, Defaults())
	want := `{"task":{"slug":"e","title":"","status":"active","repos":[]},"prompts":[],"dropped_prompts":0,"notes":[],"dropped_notes":0,"input_hash":"` + empty.InputHash + `"}`
	if got := string(Encode(empty)); got != want {
		t.Errorf("JSON:\n%s\nwant\n%s", got, want)
	}
	p := Encode(Prompt{Session: 3, At: t0, Kind: KindPrompt, Text: "<b> & co", ReplyBefore: "ok"})
	if want := `{"session":3,"at":"2026-05-01T10:00:00Z","kind":"prompt","text":"<b> & co","reply_before":"ok"}`; string(p) != want {
		t.Errorf("prompt JSON: %s", p)
	}
}

func TestBuildTaskCap(t *testing.T) {
	cfg := Defaults()
	cfg.TaskCap, cfg.NotesCap = 4<<10, 2<<10
	var prompts []Prompt
	for i := range 40 {
		prompts = append(prompts, Prompt{Session: 1, At: t0.Add(time.Duration(i) * time.Minute), Text: fmt.Sprintf("prompt %02d %s", i, strings.Repeat("x", 200))})
	}
	ex := Build(sampleTask(), prompts, 0, sampleBlame(), cfg)
	if size := len(Encode(ex)); size > cfg.TaskCap {
		t.Errorf("extract is %d bytes, over the cap %d", size, cfg.TaskCap)
	}
	if ex.DroppedPrompts == 0 || ex.DroppedPrompts+len(ex.Prompts) != 40 {
		t.Fatalf("dropped %d, kept %d", ex.DroppedPrompts, len(ex.Prompts))
	}
	if !strings.HasPrefix(ex.Prompts[len(ex.Prompts)-1].Text, "prompt 39") ||
		!strings.HasPrefix(ex.Prompts[0].Text, fmt.Sprintf("prompt %02d", ex.DroppedPrompts)) {
		t.Errorf("not the newest kept: first %q", ex.Prompts[0].Text[:9])
	}
	// One more prompt would not have fitted.
	more := ex
	more.Prompts = append([]Prompt{prompts[ex.DroppedPrompts-1]}, ex.Prompts...)
	more.DroppedPrompts--
	if size := len(Encode(more)); size <= cfg.TaskCap {
		t.Errorf("dropped one too many: %d bytes with it", size)
	}

	// A note over the notes cap alone is dropped; the prompts are kept.
	big := []model.BlamedLine{{Text: strings.Repeat("n", 5000), Author: model.AuthorUser}}
	ex = Build(sampleTask(), prompts[:3], 0, big, cfg)
	if len(ex.Prompts) != 3 || ex.DroppedPrompts != 0 || len(ex.Notes) != 0 || ex.DroppedNotes != 1 {
		t.Errorf("notes over the cap: %d kept, %d dropped, %d notes, %d dropped", len(ex.Prompts), ex.DroppedPrompts, len(ex.Notes), ex.DroppedNotes)
	}
}

func TestInputHash(t *testing.T) {
	prompts := []Prompt{{Session: 1, At: t0, Text: "short please", ReplyBefore: "Here is a long answer"}}
	h := Build(sampleTask(), prompts, 0, sampleBlame(), Defaults()).InputHash
	for i := 0; i < 3; i++ {
		if got := Build(sampleTask(), prompts, 0, sampleBlame(), Defaults()).InputHash; got != h {
			t.Fatalf("not stable: %s then %s", h, got)
		}
	}
	change := map[string]func() Extract{
		"prompt text": func() Extract {
			p := append([]Prompt{}, prompts...)
			p[0].Text = "short, please"
			return Build(sampleTask(), p, 0, sampleBlame(), Defaults())
		},
		"reply": func() Extract {
			p := append([]Prompt{}, prompts...)
			p[0].ReplyBefore = ""
			return Build(sampleTask(), p, 0, sampleBlame(), Defaults())
		},
		"prompt time": func() Extract {
			p := append([]Prompt{}, prompts...)
			p[0].At = t0.Add(time.Second)
			return Build(sampleTask(), p, 0, sampleBlame(), Defaults())
		},
		"note author": func() Extract {
			b := sampleBlame()
			b[0].Author = model.AuthorAgent
			return Build(sampleTask(), prompts, 0, b, Defaults())
		},
		"note text": func() Extract {
			b := sampleBlame()
			b[0].Text += "!"
			return Build(sampleTask(), prompts, 0, b, Defaults())
		},
	}
	for name, f := range change {
		if f().InputHash == h {
			t.Errorf("%s changed, the hash did not", name)
		}
	}
	// A blank line more in the notes is not input, nor the task's metadata.
	b := append(sampleBlame(), model.BlamedLine{Blank: true})
	if Build(sampleTask(), prompts, 0, b, Defaults()).InputHash != h {
		t.Error("a blank notes line changed the hash")
	}
	ti := TaskInfo{Slug: "renamed", Title: "Fix logout", Status: "active", Repos: []string{"api"}}
	if Build(ti, prompts, 0, sampleBlame(), Defaults()).InputHash != h {
		t.Error("the task's metadata changed the hash")
	}
}

func TestBuildNotesCap(t *testing.T) {
	line := func(author string, minute int) model.BlamedLine {
		return model.BlamedLine{Text: fmt.Sprintf("%s %02d %s", author, minute, strings.Repeat("n", 300)), Author: author,
			At: t0.Add(time.Duration(minute) * time.Minute)}
	}
	blame := []model.BlamedLine{
		line("user", 1), line("agent", 2), line("user", 3), {Blank: true}, line("unknown", 0), line("assistant", 4),
		line("agent", 5), line("user", 6), {Text: "unknown, no time " + strings.Repeat("n", 300), Author: model.AuthorUnknown},
	}
	cfg := Defaults()
	cfg.NotesCap = 1 << 10 // two lines fit
	ex := Build(sampleTask(), nil, 0, blame, cfg)
	var got []string
	for _, n := range ex.Notes {
		got = append(got, n.Text[:len(n.Text)-301])
	}
	// Others' lines go first, oldest (no time first), then the user's, oldest.
	if strings.Join(got, ",") != "user 03,user 06" {
		t.Errorf("kept %q", got)
	}
	if ex.DroppedNotes+len(ex.Notes) != 8 || len(Encode(ex.Notes)) > cfg.NotesCap {
		t.Errorf("dropped %d, kept %d, %d bytes", ex.DroppedNotes, len(ex.Notes), len(Encode(ex.Notes)))
	}
	cfg.NotesCap = 2 << 10 // the user's three and two of the others
	ex = Build(sampleTask(), nil, 0, blame, cfg)
	got = got[:0]
	for _, n := range ex.Notes {
		got = append(got, n.Text[:len(n.Text)-301])
	}
	if strings.Join(got, ",") != "user 01,user 03,assistant 04,agent 05,user 06" || ex.DroppedNotes != 3 {
		t.Errorf("kept %q, dropped %d", got, ex.DroppedNotes)
	}
	if ex := Build(sampleTask(), nil, 0, blame, Defaults()); ex.DroppedNotes != 0 || len(ex.Notes) != 8 {
		t.Errorf("under the cap: dropped %d", ex.DroppedNotes)
	}
}

func TestCollector(t *testing.T) {
	entry := func(uuid string, minute int, text string) string {
		return `{"type":"user","uuid":"` + uuid + `","origin":{"kind":"human"},"timestamp":"` +
			t0.Add(time.Duration(minute)*time.Minute).Format(time.RFC3339) + `","message":{"role":"user","content":"` + text + `"}}`
	}
	c := NewCollector(Defaults(), nil)
	// A resumed conversation's file repeats the first one's entries.
	first := strings.Join([]string{entry("u1", 1, "one"), entry("u3", 3, "three")}, "\n")
	resumed := strings.Join([]string{entry("u1", 1, "one"), entry("u3", 3, "three"), entry("u4", 4, "four"), entry("", 0, "no uuid")}, "\n")
	other := strings.Join([]string{entry("u2", 2, "two"), entry("", 0, "no uuid")}, "\n")
	for i, f := range []string{first, resumed, other} {
		if err := c.Parse(strings.NewReader(f), int64(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	ps, dropped := c.Prompts()
	var got []string
	for _, p := range ps {
		got = append(got, fmt.Sprintf("%d:%s", p.Session, p.Text))
	}
	if strings.Join(got, ",") != "2:no uuid,3:no uuid,1:one,3:two,1:three,2:four" || dropped != 0 {
		t.Errorf("got %q, dropped %d", got, dropped)
	}

	// Only the newest that fit the task cap are kept as the files are read.
	cfg := Defaults()
	cfg.TaskCap, cfg.NotesCap = 4<<10, 1<<10
	c = NewCollector(cfg, nil)
	var lines []string
	for i := range 500 {
		lines = append(lines, entry(fmt.Sprintf("u%d", i), i, fmt.Sprintf("p%03d %s", i, strings.Repeat("x", 100))))
	}
	if err := c.Parse(strings.NewReader(strings.Join(lines, "\n")), 1); err != nil {
		t.Fatal(err)
	}
	if c.kept.Len() > 40 || c.size > cfg.TaskCap {
		t.Errorf("holds %d prompts, %d bytes", c.kept.Len(), c.size)
	}
	// An older prompt read later (another session) is dropped too.
	if err := c.Parse(strings.NewReader(entry("old", 10, "old")), 2); err != nil {
		t.Fatal(err)
	}
	ps, dropped = c.Prompts()
	if dropped+len(ps) != 501 || !strings.HasPrefix(ps[len(ps)-1].Text, "p499") ||
		!strings.HasPrefix(ps[0].Text, fmt.Sprintf("p%03d", 500-len(ps))) {
		t.Errorf("kept %d (first %q), dropped %d", len(ps), ps[0].Text[:4], dropped)
	}
	ex := Build(sampleTask(), ps, dropped, nil, cfg)
	if ex.DroppedPrompts < dropped || len(Encode(ex)) > cfg.TaskCap {
		t.Errorf("extract: dropped %d, %d bytes", ex.DroppedPrompts, len(Encode(ex)))
	}

	// Same-time (here: no time, legacy) prompts: the ones read later are
	// the newer, kept; nothing cascades.
	c = NewCollector(cfg, nil)
	lines = lines[:0]
	for i := range 100 {
		lines = append(lines, fmt.Sprintf(`{"type":"user","uuid":"z%d","message":{"role":"user","content":"z%03d %s"}}`, i, i, strings.Repeat("x", 100)))
	}
	if err := c.Parse(strings.NewReader(strings.Join(lines, "\n")), 1); err != nil {
		t.Fatal(err)
	}
	ps, dropped = c.Prompts()
	if len(ps) < 20 || dropped+len(ps) != 100 || !strings.HasPrefix(ps[len(ps)-1].Text, "z099") ||
		!strings.HasPrefix(ps[0].Text, fmt.Sprintf("z%03d", dropped)) {
		t.Fatalf("no time: kept %d, dropped %d", len(ps), dropped)
	}
	// A repeat (another file) of one dropped at the cut's time, or of a
	// kept one, is not counted or kept again.
	if err := c.Parse(strings.NewReader(lines[0]+"\n"+lines[99]), 2); err != nil {
		t.Fatal(err)
	}
	if ps2, dropped2 := c.Prompts(); len(ps2) != len(ps) || dropped2 != dropped {
		t.Errorf("repeats: kept %d, dropped %d; were %d, %d", len(ps2), dropped2, len(ps), dropped)
	}
	if len(c.seen) != len(ps) {
		t.Errorf("%d uuids remembered for %d prompts kept", len(c.seen), len(ps))
	}
}

func TestConfigDefaults(t *testing.T) {
	c := Defaults()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.PromptCap != 2048 || c.ReplySnippet != 200 || c.TaskCap != 65536 || c.WeightStrong != 2 || c.WeightNormal != 1 ||
		c.WeightMild != 0.5 || c.HalfLifeProposed != 45 || c.HalfLifeAccepted != 365 || c.ProposeScore != 3 || c.ProposeTasks != 2 ||
		c.OutdatedScore != 2 || c.InjectBudget != 800 || c.Parallelism != 3 || c.PrecleanModel != "haiku" || c.PrecleanEffort != "low" ||
		c.AnalyseModel != "haiku" || c.AnalyseEffort != "medium" || c.MaxCallUSD != 0.25 || c.FadeRatio != 0.75 {
		t.Errorf("defaults: %+v", c)
	}
	// Every field has a toml key.
	b, _ := json.Marshal(c)
	var m map[string]any
	json.Unmarshal(b, &m)
	if len(m) != 20 {
		t.Errorf("%d fields: update this test and the README's [learn] keys", len(m))
	}
}

func TestConfigValidate(t *testing.T) {
	for _, c := range []struct {
		set  func(*Config)
		want string
	}{
		{func(c *Config) { c.PromptCap = 10 }, "learn.prompt_cap: 10"},
		{func(c *Config) { c.TaskCap = 1 << 30 }, "learn.task_cap"},
		{func(c *Config) { c.PromptCap, c.TaskCap = 8000, 5000 }, "more than learn.task_cap"},
		{func(c *Config) { c.NotesCap, c.PromptCap, c.TaskCap = 4000, 2000, 5000 }, "learn.notes_cap + learn.prompt_cap"},
		{func(c *Config) { c.ReplySnippet = -1 }, "learn.reply_snippet"},
		{func(c *Config) { c.Parallelism = 0 }, "learn.parallelism"},
		{func(c *Config) { c.InjectBudget = -5 }, "learn.inject_budget"},
		{func(c *Config) { c.InjectBudget = MaxInjectBudget + 1 }, "learn.inject_budget"},
		{func(c *Config) { c.HalfLifeProposed = 0 }, "learn.half_life_proposed"},
		{func(c *Config) { c.ProposeTasks = 0 }, "learn.propose_tasks"},
		{func(c *Config) { c.WeightMild = 0 }, "learn.weight_mild"},
		{func(c *Config) { c.ProposeScore = math.NaN() }, "learn.propose_score"},
		{func(c *Config) { c.OutdatedScore = math.Inf(1) }, "learn.outdated_score"},
		{func(c *Config) { c.FadeRatio = 0 }, "learn.fade_ratio"},
		{func(c *Config) { c.FadeRatio = 1.5 }, "learn.fade_ratio"},
		{func(c *Config) { c.WeightNormal = 3 }, "strong >= normal >= mild"},
		{func(c *Config) { c.PrecleanModel = "" }, "learn.preclean_model"},
		{func(c *Config) { c.AnalyseModel = "--dangerously" }, "learn.analyse_model"},
		{func(c *Config) { c.AnalyseModel = "sonnet; rm" }, "learn.analyse_model"},
		{func(c *Config) { c.PrecleanEffort = "huge" }, "learn.preclean_effort"},
		{func(c *Config) { c.MaxCallUSD = 0 }, "learn.max_call_usd"},
		{func(c *Config) { c.MaxCallUSD = 1000 }, "learn.max_call_usd"},
	} {
		cfg := Defaults()
		c.set(&cfg)
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("want %q, got %v", c.want, err)
		}
	}
	cfg := Defaults()
	cfg.AnalyseModel, cfg.AnalyseEffort, cfg.ReplySnippet, cfg.InjectBudget = "sonnet[1m]", "high", 0, 0
	if err := cfg.Validate(); err != nil {
		t.Error(err)
	}
}
