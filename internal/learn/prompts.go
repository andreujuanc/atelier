package learn

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Prompt kinds.
const (
	KindPrompt    = "prompt"    // a prompt typed at Claude Code's input
	KindRejection = "rejection" // what the user typed when rejecting a tool use
)

// Prompt is one thing the user typed in a task session.
type Prompt struct {
	Session int64     `json:"session"` // atelier's session id
	At      time.Time `json:"at"`      // the transcript entry's timestamp; zero if absent
	Kind    string    `json:"kind"`    // KindPrompt or KindRejection
	Text    string    `json:"text"`    // what the user typed, capped at Config.PromptCap
	// ReplyBefore is the end (Config.ReplySnippet characters) of Claude's
	// reply before the prompt: its text only, no tool calls or thinking;
	// "" if there was none since the previous prompt.
	ReplyBefore string `json:"reply_before"`

	uuid          string // the transcript entry's (Collector de-duplicates by it)
	claudeSession string // the transcript entry's sessionId
}

// entry is the part of a Claude Code transcript JSONL line used.
type entry struct {
	Type             string `json:"type"`
	UUID             string `json:"uuid"`
	SessionID        string `json:"sessionId"`
	IsMeta           bool   `json:"isMeta"`
	IsSidechain      bool   `json:"isSidechain"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	// IsVisibleInTranscriptOnly, TextNotTyped and HandedOff are set on
	// user entries Claude Code adds itself (it skips them as not typed).
	IsVisibleInTranscriptOnly bool            `json:"isVisibleInTranscriptOnly"`
	TextNotTyped              bool            `json:"textNotTyped"`
	HandedOff                 bool            `json:"handedOff"`
	IsAPIErrorMessage         bool            `json:"isApiErrorMessage"`
	ToolUseResult             json.RawMessage `json:"toolUseResult"`
	// Origin says who a user entry came from in recent Claude Code
	// versions: kind "human" for a typed prompt; "peer" (another session,
	// a subagent's hand-back), "task-notification", "coordinator",
	// "channel", ... otherwise. Older versions do not write it.
	Origin *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	PromptSource string `json:"promptSource"` // "typed", "system", "sdk"; "" in older versions
	Entrypoint   string `json:"entrypoint"`   // "cli"; "sdk-..." for claude -p and SDK runs
	Timestamp    string `json:"timestamp"`
	Message      *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type block struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Content json.RawMessage `json:"content"` // a tool_result's: a string or blocks
}

// maxLine bounds one transcript line that is parsed (as app's
// maxTranscriptLine): a longer one is read past and skipped.
var maxLine = 32 << 20

// ParsePrompts returns every prompt in a transcript (see Collector.Parse),
// in file order, with no cap on their number. Tests use it; atl learn uses
// a Collector.
func ParsePrompts(r io.Reader, cfg Config) ([]Prompt, error) {
	var out []Prompt
	err := parse(r, cfg, nil, func(p Prompt) { out = append(out, p) })
	return out, err
}

// parse reads a Claude Code transcript (JSONL) and calls emit with each
// thing the user typed in it, in file order, with the end of the reply
// before it (Session left 0). A user entry was typed only if nothing marks
// it as added by Claude Code:
//   - its fields: isMeta, isSidechain, isCompactSummary,
//     isVisibleInTranscriptOnly, textNotTyped, handedOff, a toolUseResult,
//     an origin whose kind is not "human", promptSource "system" or "sdk",
//     an entrypoint "sdk-..." (claude -p: a script or an agent typed it),
//     or a tool_result content block;
//   - then its text. With origin "human", only complete <system-reminder>
//     and <user-prompt-submit-hook> spans are removed. Without an origin
//     (older versions), those spans are removed (an open one to the end),
//     and text starting with <task-notification>, <local-command-...>,
//     <bash-...>, a compaction or local-command caveat, or holding a
//     message from another session or a subagent (<agent-message>,
//     <cross-session-message>, <teammate-message>) is dropped;
//   - either way an interrupted marker is dropped, and a slash command
//     keeps only its typed arguments;
//   - pasted content: Claude Code writes pasted text expanded, unmarked.
//     If pastes (history.jsonl) has the prompt, its typed form is used,
//     each paste "[pasted]"; else fenced blocks (```) and Claude Code's
//     placeholders ("[Pasted text #1 +40 lines]", "[Image #1]") become
//     "[pasted]". The user's words around them are kept; a prompt left
//     with nothing else is dropped.
//
// A tool_result rejecting a tool use with the user's feedback ("... To
// tell you how to proceed, the user said:\n<feedback>") gives the feedback,
// KindRejection.
//
// Only text blocks count (images and documents are ignored). Malformed
// lines and lines longer than maxLine are skipped; only read errors are
// returned.
func parse(r io.Reader, cfg Config, pastes *Pastes, emit func(Prompt)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	reply := tail{max: cfg.ReplySnippet}
	var buf []byte
	for {
		line, tooLong, err := readLine(br, buf[:0], maxLine)
		buf = line
		if !tooLong && len(bytes.TrimSpace(line)) > 0 {
			var e entry
			if json.Unmarshal(line, &e) == nil && e.Message != nil && !e.IsSidechain {
				switch e.Type {
				case "assistant":
					if !e.IsMeta && !e.IsAPIErrorMessage {
						for _, t := range texts(e.Message.Content) {
							reply.add(t)
						}
					}
				case "user":
					if kind, text, ok := typed(&e, pastes); ok {
						p := Prompt{Kind: kind, Text: capBytes(text, cfg.PromptCap), ReplyBefore: reply.String(),
							uuid: e.UUID, claudeSession: e.SessionID}
						if ts, err := time.Parse(time.RFC3339Nano, e.Timestamp); err == nil {
							p.At = ts.UTC()
						}
						emit(p)
						reply.reset()
					}
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

// typed returns what the user typed in user entry e (see parse).
func typed(e *entry, pastes *Pastes) (kind, text string, ok bool) {
	if e.IsMeta || e.IsCompactSummary || e.IsVisibleInTranscriptOnly || e.TextNotTyped || e.HandedOff {
		return "", "", false
	}
	if e.PromptSource == "system" || e.PromptSource == "sdk" || strings.HasPrefix(e.Entrypoint, "sdk-") {
		return "", "", false
	}
	if e.Origin != nil && e.Origin.Kind != "human" {
		return "", "", false
	}
	human := e.Origin != nil
	var blocks []block
	if json.Unmarshal(e.Message.Content, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "tool_result" {
				if len(blocks) == 1 {
					if fb := rejection(b); fb != "" {
						if fb = humanText(fb, true); keepable(fb) {
							return KindRejection, fb, true
						}
					}
				}
				return "", "", false
			}
		}
	}
	if len(e.ToolUseResult) > 0 && string(e.ToolUseResult) != "null" {
		return "", "", false
	}
	ts := texts(e.Message.Content)
	if d, ok := pastes.typedForm(e, strings.Join(ts, "\n")); ok {
		ts = []string{d}
	}
	var parts []string
	for _, t := range ts {
		if t = humanText(t, human); t != "" {
			parts = append(parts, t)
		}
	}
	text = strings.Join(parts, "\n")
	if !keepable(text) {
		return "", "", false
	}
	return KindPrompt, text, true
}

// keepable reports whether text has something besides pasted marks.
func keepable(text string) bool {
	return strings.TrimSpace(strings.ReplaceAll(text, pastedMark, "")) != ""
}

// The tool_result text of a rejected tool use with the user's feedback
// after it (Claude Code 2.1's N8, then the feedback, then possibly
// rejectNote). Only this form: the "Permission for this tool use was
// denied. ... The user said:" one wraps text relayed from teammates and
// other processes, and N8 followed by channelDenial is a channel relay's.
// With --permission-prompt-tool, the tool's own message can follow N8;
// atl starts interactive claude without it, so that case does not arise.
const (
	rejectPrefix  = "The user doesn't want to proceed with this tool use. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). To tell you how to proceed, the user said:\n"
	rejectNote    = "\n\nNote: The user's next message may contain a correction or preference."
	channelDenial = "Denied via channel "
)

// rejection returns the feedback the user typed in a rejecting
// tool_result block, "" if b is not one.
func rejection(b block) string {
	var s string
	if json.Unmarshal(b.Content, &s) != nil {
		s = strings.Join(texts(b.Content), "\n")
	}
	s = strings.TrimSuffix(strings.TrimPrefix(s, "<tool_use_error>"), "</tool_use_error>")
	fb, ok := strings.CutPrefix(s, rejectPrefix)
	if !ok || strings.HasPrefix(fb, channelDenial) {
		return ""
	}
	if i := strings.Index(fb, rejectNote); i >= 0 {
		fb = fb[:i]
	}
	return fb
}

// texts returns the text of a message's content: the string, or its text
// blocks.
func texts(content json.RawMessage) []string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return []string{s}
	}
	var blocks []block
	if json.Unmarshal(content, &blocks) != nil {
		return nil
	}
	var out []string
	for _, b := range blocks {
		if b.Type == "text" {
			out = append(out, b.Text)
		}
	}
	return out
}

// Text Claude Code (or a hook, another session) puts in a user entry
// without an origin (older versions). A text starting with one of
// dropPrefixes, or holding one of dropAnywhere, is not the user's.
var (
	dropPrefixes = []string{
		"<task-notification>", "<local-command-", "<bash-input>", "<bash-stdout>", "<bash-stderr>",
		"<user-prompt-submit-hook>", "<agent-message", "<cross-session-message", "<teammate-message",
		"<channel-message", "Another Claude session sent a message",
		"This session is being continued from a previous conversation",
		"Caveat: The messages below were generated by the user while running local commands",
	}
	dropAnywhere = []string{"<agent-message ", "<agent-message>", "<cross-session-message", "<teammate-message", "<task-notification>"}
	commandTags  = []string{"<command-name>", "<command-message>", "<command-args>"}
)

// interrupted starts Claude Code's interrupted markers ("[Request
// interrupted by user]", "... for tool use]"), whatever the origin.
const interrupted = "[Request interrupted by user"

// pastedMark replaces what the user pasted.
const pastedMark = "[pasted]"

// placeholderRE matches the placeholders Claude Code leaves for pasted
// text and images (its own pattern).
var placeholderRE = regexp.MustCompile(`\[(?:Pasted text #\d+(?: \+\d+ lines)?|Image #\d+|\.\.\.Truncated text #\d+ \+\d+ lines\.\.\.|\x{2726} Team setup guide #\d+(?: \+\d+ lines)?)\]`)

// humanText returns what the user typed in one text of a user entry, ""
// if none (see parse). human: the entry's origin says a human typed it.
func humanText(s string, human bool) string {
	s = stripSpans(s, "<system-reminder>", "</system-reminder>", !human)
	s = strings.TrimSpace(stripSpans(s, "<user-prompt-submit-hook>", "</user-prompt-submit-hook>", !human))
	for _, p := range commandTags {
		if strings.HasPrefix(s, p) {
			// A slash command: only its arguments were typed.
			s = strings.TrimSpace(between(s, "<command-args>", "</command-args>"))
			break
		}
	}
	if strings.HasPrefix(s, interrupted) {
		return ""
	}
	if !human {
		for _, p := range dropPrefixes {
			if strings.HasPrefix(s, p) {
				return ""
			}
		}
		for _, p := range dropAnywhere {
			if strings.Contains(s, p) {
				return ""
			}
		}
	}
	s = placeholderRE.ReplaceAllString(dropFences(s), pastedMark)
	return strings.TrimSpace(s)
}

// stripSpans removes every open...close span of s in one pass. An open
// tag with no close after it is removed with the rest of s if toEnd, else
// kept as it is.
func stripSpans(s, open, close string, toEnd bool) string {
	if !strings.Contains(s, open) {
		return s
	}
	var b strings.Builder
	for {
		i := strings.Index(s, open)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		j := strings.Index(s[i+len(open):], close)
		if j < 0 {
			if !toEnd {
				b.WriteString(s)
			} else {
				b.WriteString(s[:i])
			}
			return b.String()
		}
		b.WriteString(s[:i])
		s = s[i+len(open)+j+len(close):]
	}
}

// between returns the text after the first open tag up to the following
// close tag (or the end of s); "" if open is absent.
func between(s, open, close string) string {
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	s = s[i+len(open):]
	if j := strings.Index(s, close); j >= 0 {
		s = s[:j]
	}
	return s
}

// dropFences replaces each fenced block (a line starting with ``` or ~~~
// up to the next such line, or the end) with pastedMark on a line.
func dropFences(s string) string {
	if !strings.Contains(s, "```") && !strings.Contains(s, "~~~") {
		return s
	}
	var out []string
	in := false
	for _, l := range strings.Split(s, "\n") {
		t := strings.TrimSpace(l)
		fence := strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
		switch {
		case fence && !in:
			in = true
			out = append(out, pastedMark)
		case fence:
			in = false
		case !in:
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// capBytes cuts s to at most max bytes on a rune boundary and marks the
// cut: "… [N bytes cut]".
func capBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("… [%d bytes cut]", len(s)-cut)
}

// tail keeps the last max characters of the assistant's text, on one line
// (whitespace runs made one space).
type tail struct {
	max int
	s   string
}

func (t *tail) add(s string) {
	if t.max <= 0 {
		return
	}
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return
	}
	if t.s != "" {
		t.s += " "
	}
	t.s += s
	if len(t.s) > 8*t.max { // bound memory: keep well over max characters
		t.s = lastRunes(t.s, t.max)
	}
}

func (t *tail) String() string { return lastRunes(t.s, t.max) }
func (t *tail) reset()         { t.s = "" }

// lastRunes returns the last n runes of s.
func lastRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	i := len(s)
	for ; n > 0 && i > 0; n-- {
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
	}
	return s[i:]
}

// readLine reads the next line from br into buf, its newline included (the
// last line may have none, with io.EOF). A line longer than max is read to
// its end but not kept: tooLong, and line empty. (As app's readLine.)
func readLine(br *bufio.Reader, buf []byte, max int) (line []byte, tooLong bool, err error) {
	for {
		chunk, err := br.ReadSlice('\n')
		if !tooLong {
			if len(buf)+len(chunk) > max {
				tooLong, buf = true, buf[:0]
			} else {
				buf = append(buf, chunk...)
			}
		}
		if err != bufio.ErrBufferFull {
			return buf, tooLong, err
		}
	}
}
