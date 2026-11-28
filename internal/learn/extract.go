package learn

import (
	"bytes"
	"container/heap"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash/fnv"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
)

// Extract is one task's input to learning (docs/learning.md, "1a. Static
// extract"). Its JSON is the form models read.
type Extract struct {
	Task    TaskInfo `json:"task"`
	Prompts []Prompt `json:"prompts"` // in time order
	// DroppedPrompts counts the oldest prompts left out to keep the
	// extract within Config.TaskCap.
	DroppedPrompts int        `json:"dropped_prompts"`
	Notes          []NoteLine `json:"notes"` // in the notes' order
	// DroppedNotes counts the notes lines left out to keep them within
	// Config.NotesCap: others' lines first, then the user's, oldest first.
	DroppedNotes int `json:"dropped_notes"`
	// InputHash is the hex sha256 of the canonical JSON of Prompts and
	// Notes: the same for the same input, run after run. The task's
	// metadata is not in it (a rename does not make it new input).
	InputHash string `json:"input_hash"`
}

// TaskInfo is the task's metadata in an Extract.
type TaskInfo struct {
	Slug   string   `json:"slug"`
	Title  string   `json:"title"`
	Status string   `json:"status"`
	Repos  []string `json:"repos"`
}

// NoteLine is one non-blank line of the task's notes with who first wrote
// it (store.NotesBlame: a model.Author* constant, never the line's tag).
type NoteLine struct {
	Author string `json:"author"`
	At     string `json:"at,omitempty"` // when it was first written (RFC 3339, UTC); "" if not recorded
	Text   string `json:"text"`
}

// TaskInfoOf returns t's metadata.
func TaskInfoOf(t *model.Task) TaskInfo {
	repos := make([]string, 0, len(t.Repos))
	for _, r := range t.Repos {
		repos = append(repos, r.Name)
	}
	return TaskInfo{Slug: t.Slug, Title: t.Title, Status: string(t.Status), Repos: repos}
}

// Collector gathers a task's prompts from its transcripts as they are
// read: each transcript entry once (by uuid: a resumed or forked
// conversation repeats its entries), and only the newest that fit in
// Config.TaskCap, so memory stays bounded whatever the transcripts' size.
// Same-time prompts are ordered as read: the one read later is the newer.
// Only the kept prompts' uuids are remembered (and those dropped at the
// cut's time), so a repeat of a prompt dropped earlier than that is
// counted as dropped again.
type Collector struct {
	cfg     Config
	pastes  *Pastes
	seen    map[uint64]struct{} // uuid hashes of the kept prompts
	kept    promptHeap
	size    int // encoded size of kept
	seq     int
	dropped int
	// cut is the newest dropped prompt's time: prompts older than it
	// are dropped too; cutSeen holds the uuid hashes dropped at it.
	cut     time.Time
	hasCut  bool
	cutSeen map[uint64]struct{}
}

// NewCollector returns a Collector using pastes (may be nil) for the
// typed form of prompts with pasted content.
func NewCollector(cfg Config, pastes *Pastes) *Collector {
	return &Collector{cfg: cfg, pastes: pastes, seen: map[uint64]struct{}{}, cutSeen: map[uint64]struct{}{}}
}

// Parse adds what the user typed in a transcript of session (see parse).
// On a read error, what was read before it is kept.
func (c *Collector) Parse(r io.Reader, session int64) error {
	return parse(r, c.cfg, c.pastes, func(p Prompt) {
		p.Session = session
		c.add(p)
	})
}

func uuidKey(uuid string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(uuid))
	return h.Sum64()
}

func (c *Collector) add(p Prompt) {
	k := uuidKey(p.uuid)
	if p.uuid != "" {
		if _, ok := c.seen[k]; ok {
			return
		}
	}
	if c.hasCut {
		if p.At.Before(c.cut) {
			c.dropped++
			return
		}
		if _, ok := c.cutSeen[k]; ok && p.uuid != "" && p.At.Equal(c.cut) {
			return
		}
	}
	c.seq++
	it := heapItem{p: p, seq: c.seq, size: len(Encode(p)) + 1}
	heap.Push(&c.kept, it)
	c.size += it.size
	if p.uuid != "" {
		c.seen[k] = struct{}{}
	}
	for c.size > c.cfg.TaskCap && c.kept.Len() > 0 {
		old := heap.Pop(&c.kept).(heapItem)
		c.size -= old.size
		c.dropped++
		if !c.hasCut || old.p.At.After(c.cut) {
			c.cut, c.hasCut = old.p.At, true
			clear(c.cutSeen)
		}
		if old.p.uuid != "" {
			ok := uuidKey(old.p.uuid)
			delete(c.seen, ok)
			c.cutSeen[ok] = struct{}{}
		}
	}
}

// Prompts returns the prompts kept, in time order (same-time prompts in
// the order read), and how many were dropped.
func (c *Collector) Prompts() ([]Prompt, int) {
	items := append([]heapItem{}, c.kept...)
	sort.Slice(items, func(i, j int) bool { return items[i].less(items[j]) })
	out := make([]Prompt, len(items))
	for i, it := range items {
		out[i] = it.p
	}
	return out, c.dropped
}

type heapItem struct {
	p    Prompt
	seq  int
	size int
}

func (a heapItem) less(b heapItem) bool {
	if !a.p.At.Equal(b.p.At) {
		return a.p.At.Before(b.p.At)
	}
	return a.seq < b.seq
}

// promptHeap is a min-heap of prompts by time: the oldest on top.
type promptHeap []heapItem

func (h promptHeap) Len() int           { return len(h) }
func (h promptHeap) Less(i, j int) bool { return h[i].less(h[j]) }
func (h promptHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *promptHeap) Push(x any)        { *h = append(*h, x.(heapItem)) }
func (h *promptHeap) Pop() any {
	old := *h
	it := old[len(old)-1]
	*h = old[:len(old)-1]
	return it
}

// hashVersion starts what InputHash covers: a change of the extract's form
// changes every hash.
const hashVersion = "atl-learn-extract-v2\n"

// Build makes a task's extract from its prompts (Collector.Prompts;
// dropped already left out), put in time order (stable), and its notes
// blame: the non-blank notes lines, cut to cfg.NotesCap (see
// Extract.DroppedNotes); then, while the extract is over cfg.TaskCap
// bytes, the oldest prompts are dropped.
func Build(task TaskInfo, prompts []Prompt, dropped int, blame []model.BlamedLine, cfg Config) Extract {
	if task.Repos == nil {
		task.Repos = []string{}
	}
	ex := Extract{Task: task, Prompts: append([]Prompt{}, prompts...), DroppedPrompts: dropped}
	sort.SliceStable(ex.Prompts, func(i, j int) bool { return ex.Prompts[i].At.Before(ex.Prompts[j].At) })
	ex.Notes, ex.DroppedNotes = capNotes(blame, cfg.NotesCap)

	// The size with every prompt, computed once: the base (no prompts, a
	// full-length hash, room for the count's digits) plus each prompt and
	// its comma.
	base := ex
	base.Prompts, base.InputHash, base.DroppedPrompts = []Prompt{}, strings.Repeat("0", 64), dropped+len(prompts)
	size := len(Encode(base))
	sizes := make([]int, len(ex.Prompts))
	for i, p := range ex.Prompts {
		sizes[i] = len(Encode(p)) + 1
		size += sizes[i]
	}
	drop := 0
	for drop < len(sizes) && size > cfg.TaskCap {
		size -= sizes[drop]
		drop++
	}
	ex.Prompts, ex.DroppedPrompts = ex.Prompts[drop:], dropped+drop

	in := struct {
		Prompts []Prompt   `json:"prompts"`
		Notes   []NoteLine `json:"notes"`
	}{ex.Prompts, ex.Notes}
	sum := sha256.Sum256(append([]byte(hashVersion), Encode(in)...))
	ex.InputHash = hex.EncodeToString(sum[:])
	return ex
}

// capNotes returns the non-blank lines of blame, in order, within max
// bytes (encoded, with commas), and how many were left out: first lines
// not written by the user, then the user's, each group oldest first (a
// line with no time first, then by position).
func capNotes(blame []model.BlamedLine, max int) ([]NoteLine, int) {
	type line struct {
		n    NoteLine
		user bool
		at   time.Time
		size int
		drop bool
	}
	var lines []line
	size := 2
	for _, l := range blame {
		if l.Blank {
			continue
		}
		n := NoteLine{Author: l.Author, Text: l.Text}
		if !l.At.IsZero() {
			n.At = l.At.UTC().Format(time.RFC3339)
		}
		ln := line{n: n, user: l.Author == model.AuthorUser, at: l.At, size: len(Encode(n)) + 1}
		size += ln.size
		lines = append(lines, ln)
	}
	order := make([]int, len(lines))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		la, lb := lines[order[a]], lines[order[b]]
		if la.user != lb.user {
			return !la.user
		}
		return la.at.Before(lb.at)
	})
	dropped := 0
	for _, i := range order {
		if size <= max {
			break
		}
		lines[i].drop = true
		size -= lines[i].size
		dropped++
	}
	out := []NoteLine{}
	for _, l := range lines {
		if !l.drop {
			out = append(out, l.n)
		}
	}
	return out, dropped
}

// Encode is v's canonical JSON: compact, fields in declaration order, no
// HTML escaping, no trailing newline.
func Encode(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err) // only plain types here
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}
