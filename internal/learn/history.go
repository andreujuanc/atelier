package learn

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Pastes are the prompts Claude Code's input history (history.jsonl in its
// config dir) has for some sessions, in order: each as typed, a paste as a
// placeholder ("[Pasted text #1 +40 lines]"), where the transcript has the
// pasted text expanded, unmarked. A nil *Pastes has none.
type Pastes struct {
	bySession map[string]*sessionHistory
	n         int
}

// sessionHistory is one session's history entries in time order and how
// far its transcript prompts have been paired with them.
type sessionHistory struct {
	entries  []*typedPrompt
	sorted   bool                        // entries are in time order and the indexes below built (index)
	byExact  map[[sha256.Size]byte][]int // entries' indexes by exact text, ascending
	fallback []int                       // indexes of the entries with segs, ascending
	next     int                         // entries before it are paired or skipped
	last     *typedPrompt                // the entry the last paired prompt had
	lastText [sha256.Size]byte
}

// index puts the entries in time order and builds byExact and fallback.
func (sh *sessionHistory) index() {
	sort.SliceStable(sh.entries, func(i, j int) bool { return sh.entries[i].at.Before(sh.entries[j].at) })
	sh.byExact, sh.fallback = map[[sha256.Size]byte][]int{}, nil
	for i, c := range sh.entries {
		if c.hasExact {
			sh.byExact[c.exact] = append(sh.byExact[c.exact], i)
		} else if len(c.segs) > 0 {
			sh.fallback = append(sh.fallback, i)
		}
	}
	sh.sorted = true
}

type typedPrompt struct {
	at time.Time // when it was submitted
	// exact is the sha256 of the prompt as the transcript has it: display
	// with each text paste replaced by its body. Not set (only a
	// placeholder entry has the fields below) when a body is stored
	// elsewhere (more than 1024 characters: by contentHash).
	exact    [sha256.Size]byte
	hasExact bool
	display  string // as typed, with placeholders; "" if it has none
	// segs is display split at its text paste placeholders (an image's
	// stays: the transcript has it too), lines each paste's newline count
	// (the "+N lines" of its placeholder).
	segs  []string
	lines []int
}

// Bounds on what ReadHistory keeps: entries, and bytes of one entry with
// a placeholder (others are kept as a hash).
const (
	maxPastes       = 50000
	maxPasteDisplay = 64 << 10
)

// pasteSlack: a history entry is written as its prompt is submitted,
// before the transcript entry (which a queued prompt gets only when it is
// sent, maybe minutes later). So a transcript prompt is paired only with
// a history entry from at most pasteSlack after it.
const pasteSlack = 2 * time.Second

// minStoredPaste is the length (UTF-16 code units, as Claude Code counts)
// past which history.jsonl holds a paste only by contentHash.
const minStoredPaste = 1024

var (
	// textPasteRE matches a text paste placeholder: its id and line count.
	textPasteRE = regexp.MustCompile(`\[Pasted text #(\d+)(?: \+(\d+) lines)?\]`)
	// opaqueRE matches the placeholders whose transcript form is not known.
	opaqueRE = regexp.MustCompile(`\[(?:\.\.\.Truncated text #\d+ \+\d+ lines\.\.\.|\x{2726} Team setup guide #\d+(?: \+\d+ lines)?)\]`)
)

// ReadHistory reads Claude Code's history.jsonl (one {display,
// pastedContents, timestamp (unix ms), project, sessionId} per line) and
// adds to into the entries of the sessions in sessions (at most maxPastes).
// Malformed and over-long lines are skipped; only read errors are
// returned, with what was read before kept.
func ReadHistory(r io.Reader, sessions map[string]bool, into *Pastes) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var buf []byte
	for {
		line, tooLong, err := readLine(br, buf[:0], maxLine)
		buf = line
		if !tooLong && into.n < maxPastes {
			var h struct {
				Display        string            `json:"display"`
				Timestamp      int64             `json:"timestamp"`
				SessionID      string            `json:"sessionId"`
				PastedContents map[string]pasted `json:"pastedContents"`
			}
			if json.Unmarshal(line, &h) == nil && sessions[h.SessionID] {
				if tp := historyEntry(h.Display, h.PastedContents, time.UnixMilli(h.Timestamp)); tp != nil {
					if into.bySession == nil {
						into.bySession = map[string]*sessionHistory{}
					}
					sh := into.bySession[h.SessionID]
					if sh == nil {
						sh = &sessionHistory{}
						into.bySession[h.SessionID] = sh
					}
					sh.sorted = false // index again
					sh.entries = append(sh.entries, tp)
					into.n++
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
			}
			return err
		}
	}
}

// pasted is one of a history entry's pastedContents: its body inline, or
// (a long text) only a contentHash into Claude Code's paste store.
type pasted struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

// historyEntry makes the typedPrompt of a history entry; nil if it cannot
// be used.
func historyEntry(display string, pcs map[string]pasted, at time.Time) *typedPrompt {
	tp := &typedPrompt{at: at}
	if !placeholderRE.MatchString(display) {
		tp.exact, tp.hasExact = sha256.Sum256([]byte(strings.TrimSpace(display))), true
		return tp
	}
	if len(display) > maxPasteDisplay {
		return nil
	}
	tp.display = display
	exact := !opaqueRE.MatchString(display)
	expanded := textPasteRE.ReplaceAllStringFunc(display, func(m string) string {
		pc := pcs[textPasteRE.FindStringSubmatch(m)[1]]
		if pc.Type == "text" && pc.Content != "" {
			return pc.Content
		}
		exact = false
		return m
	})
	if exact {
		tp.exact, tp.hasExact = sha256.Sum256([]byte(strings.TrimSpace(expanded))), true
		return tp
	}
	if opaqueRE.MatchString(display) {
		return tp // paired (in order) but never matched
	}
	idx := textPasteRE.FindAllStringSubmatchIndex(display, -1)
	start := 0
	for _, m := range idx {
		tp.segs = append(tp.segs, display[start:m[0]])
		n := 0
		if m[4] >= 0 {
			n, _ = strconv.Atoi(display[m[4]:m[5]])
		}
		tp.lines = append(tp.lines, n)
		start = m[1]
	}
	tp.segs = append(tp.segs, display[start:])
	return tp
}

// Bounds on the fallback (fits): the longest prompt text it is tried on,
// and how many entries, nearest first, are tried for one prompt.
const (
	maxFitText  = 1 << 20
	maxFitTries = 100
)

// typedForm returns the prompt as typed, its pastes "[pasted]", for user
// entry e whose text is text, pairing the session's transcript prompts and
// history entries in order: e takes the first entry from the session's
// next one on, submitted at most pasteSlack after e, that has e's exact
// text; else the nearest one before e whose pastes fit (see fits). Entries
// passed over are not used again (a prompt cleared from the queue or
// blocked by a hook has a history entry and no transcript entry). A
// prompt the same as the last paired one has that one's entry (Claude
// Code does not repeat a history entry). false: no entry with pastes.
func (p *Pastes) typedForm(e *entry, text string) (string, bool) {
	if p == nil || e.SessionID == "" {
		return "", false
	}
	sh := p.bySession[e.SessionID]
	if sh == nil {
		return "", false
	}
	at, err := time.Parse(time.RFC3339Nano, e.Timestamp)
	if err != nil {
		return "", false
	}
	if !sh.sorted {
		sh.index()
	}
	text = strings.TrimSpace(stripSpans(text, "<system-reminder>", "</system-reminder>", false))
	sum := sha256.Sum256([]byte(text))
	var match *typedPrompt
	if sh.last != nil && sum == sh.lastText {
		match = sh.last
	} else {
		limit := at.Add(pasteSlack)
		end := sh.next + sort.Search(len(sh.entries)-sh.next, func(i int) bool { return sh.entries[sh.next+i].at.After(limit) })
		j := -1
		if idx := sh.byExact[sum]; len(idx) > 0 {
			if k := sort.SearchInts(idx, sh.next); k < len(idx) && idx[k] < end {
				j = idx[k]
			}
		}
		if j < 0 && len(text) <= maxFitText {
			// The fallback entries in [next, end), nearest first.
			hi := sort.SearchInts(sh.fallback, end)
			for k, tries := hi-1, 0; k >= 0 && sh.fallback[k] >= sh.next && tries < maxFitTries; k, tries = k-1, tries+1 {
				if sh.entries[sh.fallback[k]].fits(text) {
					j = sh.fallback[k]
					break
				}
			}
		}
		if j < 0 {
			return "", false
		}
		match, sh.next = sh.entries[j], j+1
	}
	sh.last, sh.lastText = match, sum
	if match.display == "" {
		return "", false
	}
	return placeholderRE.ReplaceAllString(match.display, pastedMark), true
}

// fits reports whether text is c's prompt with bodies stored elsewhere:
// c has typed text, text is c's segments in order (the first and last
// trimmed on their outer side) and what stands in place of each paste is
// at least minStoredPaste long with the placeholder's line count, or one
// more, or (a paste at the start or end of text, which was trimmed) one
// less. Linear in text: each gap is measured as it grows, and given up
// past the line count.
func (c *typedPrompt) fits(text string) bool {
	if len(c.segs) < 2 {
		return false
	}
	typed := false
	for _, s := range c.segs {
		typed = typed || strings.TrimSpace(s) != ""
	}
	first := strings.TrimLeft(c.segs[0], " \t\r\n")
	last := strings.TrimRight(c.segs[len(c.segs)-1], " \t\r\n")
	if !typed || !strings.HasPrefix(text, first) || !strings.HasSuffix(text, last) || len(first)+len(last) > len(text) {
		return false
	}
	pos := len(first)
	body := text[:len(text)-len(last)] // the last segment is the suffix
	for i := range c.lines {
		lines := c.lines[i]
		edge := (i == 0 && first == "") || (i == len(c.lines)-1 && last == "")
		ok := func(nl, n16 int) bool {
			return n16 >= minStoredPaste && (nl == lines || nl == lines+1 || (edge && nl == lines-1))
		}
		if i == len(c.lines)-1 {
			// The last gap runs to the last segment.
			var g gap
			return g.grow(body[pos:], lines+1) && ok(g.nl, g.n16)
		}
		next := c.segs[i+1]
		var g gap
		for from := pos; ; {
			k := strings.Index(body[from:], next)
			if k < 0 {
				return false
			}
			if !g.grow(body[pos:from+k], lines+1) {
				return false // past the line count: no later end fits
			}
			if ok(g.nl, g.n16) {
				pos = from + k + len(next)
				break
			}
			if from += k; from >= len(body) {
				return false
			}
			_, size := utf8.DecodeRuneInString(body[from:])
			from += size // the next occurrence, a rune on
		}
	}
	return false
}

// gap measures a growing gap: its newlines (\r\n, \r or \n, as Claude Code
// counts them) and UTF-16 length.
type gap struct {
	n       int // bytes measured
	nl, n16 int
	cr      bool // the last byte measured was \r
}

// grow measures s, which extends the gap (it starts with what was
// measured); false once it has more than max newlines.
func (g *gap) grow(s string, max int) bool {
	for _, r := range s[g.n:] {
		switch {
		case r == '\n' && g.cr:
		case r == '\n' || r == '\r':
			g.nl++
		}
		g.cr = r == '\r'
		if r >= 0x10000 {
			g.n16 += 2
		} else {
			g.n16++
		}
	}
	g.n = len(s)
	return g.nl <= max
}
