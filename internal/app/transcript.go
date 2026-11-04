package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/andreujuanc/atelier/internal/model"
)

// Turn is one condensed conversation turn of a Claude Code transcript.
type Turn struct {
	Role string    // "user", "assistant" or "system" (a compaction marker)
	Text string    // text blocks joined by newlines; tool calls as "[tool: Name] ..." lines
	Time time.Time // the entry's timestamp (first entry of a collapsed turn); zero if absent
}

// Transcript returns the last tail turns (all if tail <= 0) of a task
// session's Claude Code transcript. sessionID 0 picks the most recent
// session (by created_at, then id) with a recorded transcript path.
func (a *App) Transcript(ctx context.Context, ref string, sessionID int64, tail int) ([]Turn, error) {
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return nil, err
	}
	sessions, err := a.Store.ListSessions(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	var sess *model.Session
	for i := len(sessions) - 1; i >= 0; i-- {
		s := &sessions[i]
		if (sessionID != 0 && s.ID == sessionID) || (sessionID == 0 && s.TranscriptPath != "") {
			sess = s
			break
		}
	}
	switch {
	case sess == nil && sessionID != 0:
		return nil, fmt.Errorf("task %s has no session %d", t.Slug, sessionID)
	case sess == nil:
		return nil, fmt.Errorf("task %s has no session with a recorded transcript", t.Slug)
	case sess.TranscriptPath == "":
		return nil, fmt.Errorf("session %d of task %s has no recorded transcript", sess.ID, t.Slug)
	}
	f, err := os.Open(sess.TranscriptPath)
	if err != nil {
		return nil, fmt.Errorf("session %d transcript: %w", sess.ID, err)
	}
	defer f.Close()
	turns, err := ParseTranscript(f)
	if err != nil {
		return nil, fmt.Errorf("session %d transcript %s: %w", sess.ID, sess.TranscriptPath, err)
	}
	if tail > 0 && len(turns) > tail {
		turns = turns[len(turns)-tail:]
	}
	return turns, nil
}

// transcriptEntry is the part of a Claude Code transcript JSONL line used.
type transcriptEntry struct {
	Type             string `json:"type"`
	IsMeta           bool   `json:"isMeta"`
	IsSidechain      bool   `json:"isSidechain"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	Timestamp        string `json:"timestamp"`
	Message          *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type transcriptBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ParseTranscript condenses a Claude Code transcript (JSONL, one object
// per line) into user/assistant turns:
//   - only entries with type "user" or "assistant" and a message whose
//     content is a string or an array of blocks are used; isMeta and
//     sidechain (subagent) entries are skipped;
//   - text blocks are the turn's text; an assistant entry with no text but
//     tool_use blocks becomes one "[tool: Name] <summary>" line per call;
//     user entries without text (tool results) are skipped; other blocks
//     (thinking, images) are ignored;
//   - user entries Claude Code injects are condensed: a compaction summary
//     (isCompactSummary) becomes a "system" turn "[conversation compacted]";
//     <task-notification> text becomes "[background task notification]";
//     slash-command text (<command-name>, <command-message>,
//     <local-command-...>) becomes "[command: /name]" (or "[command]");
//     <bash-input> becomes "[shell: <first 80 chars>]" and
//     <bash-stdout>/<bash-stderr> output is dropped;
//   - consecutive entries of the same role are collapsed into one turn;
//   - each turn's text is capped at maxTurnRunes runes;
//   - malformed lines are ignored.
//
// Only read errors are returned.
func ParseTranscript(r io.Reader) ([]Turn, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var turns []Turn
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if role, text, at, ok := parseEntry(line); ok {
				if n := len(turns); n > 0 && turns[n-1].Role == role {
					turns[n-1].Text += "\n" + text
				} else {
					turns = append(turns, Turn{Role: role, Text: text, Time: at})
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return capTurns(turns), nil
		}
		if err != nil {
			return capTurns(turns), err
		}
	}
}

// maxTurnRunes caps the text of one condensed turn.
const maxTurnRunes = 3000

// capTurns truncates each turn's text to maxTurnRunes runes, appending
// "… (N more chars)".
func capTurns(turns []Turn) []Turn {
	for i := range turns {
		if r := []rune(turns[i].Text); len(r) > maxTurnRunes {
			turns[i].Text = string(r[:maxTurnRunes]) + fmt.Sprintf("… (%d more chars)", len(r)-maxTurnRunes)
		}
	}
	return turns
}

func parseEntry(line []byte) (role, text string, at time.Time, ok bool) {
	var e transcriptEntry
	if json.Unmarshal(line, &e) != nil || e.Message == nil || e.IsMeta || e.IsSidechain {
		return "", "", at, false
	}
	if e.Type != "user" && e.Type != "assistant" {
		return "", "", at, false
	}
	if e.Timestamp != "" {
		if ts, err := time.Parse(time.RFC3339Nano, e.Timestamp); err == nil {
			at = ts
		}
	}
	if e.IsCompactSummary {
		return "system", "[conversation compacted]", at, true
	}
	var texts, tools []string
	var s string
	var blocks []transcriptBlock
	switch {
	case json.Unmarshal(e.Message.Content, &s) == nil:
		texts = append(texts, s)
	case json.Unmarshal(e.Message.Content, &blocks) == nil:
		for _, b := range blocks {
			switch b.Type {
			case "text":
				texts = append(texts, b.Text)
			case "tool_use":
				tools = append(tools, toolLine(b))
			}
		}
	default:
		return "", "", at, false
	}
	if e.Type == "user" {
		for i, t := range texts {
			texts[i] = injectedUserText(t)
		}
	}
	text = strings.TrimSpace(strings.Join(nonEmpty(texts), "\n"))
	if text == "" {
		if e.Type != "assistant" || len(tools) == 0 {
			return "", "", at, false
		}
		text = strings.Join(tools, "\n")
	}
	return e.Type, text, at, true
}

// injectedUserText condenses user text that Claude Code injects rather
// than the human typing it (see ParseTranscript); other text is returned
// unchanged, and "" means drop it.
func injectedUserText(s string) string {
	t := strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(t, "<task-notification>"):
		return "[background task notification]"
	case strings.HasPrefix(t, "<command-name>"), strings.HasPrefix(t, "<command-message>"),
		strings.HasPrefix(t, "<local-command-"):
		if name := oneLine(between(t, "<command-name>", "</command-name>")); name != "" {
			return "[command: " + firstChars(name, 80) + "]"
		}
		return "[command]"
	case strings.HasPrefix(t, "<bash-input>"):
		return "[shell: " + firstChars(oneLine(between(t, "<bash-input>", "</bash-input>")), 80) + "]"
	case strings.HasPrefix(t, "<bash-stdout>"), strings.HasPrefix(t, "<bash-stderr>"):
		return ""
	}
	return s
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

func nonEmpty(ss []string) []string {
	var out []string
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// toolLine summarises a tool_use block as "[tool: Name] <first 80 chars of
// its command, file path, pattern, ...>".
func toolLine(b transcriptBlock) string {
	name := strings.TrimSpace(b.Name)
	if name == "" {
		name = "?"
	}
	line := "[tool: " + name + "]"
	var in map[string]any
	if json.Unmarshal(b.Input, &in) == nil {
		for _, k := range []string{"command", "file_path", "notebook_path", "path", "pattern", "url", "query", "description", "prompt"} {
			if v, ok := in[k].(string); ok && strings.TrimSpace(v) != "" {
				line += " " + firstChars(oneLine(v), 80)
				break
			}
		}
	}
	return line
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// cleanLine is oneLine after turning control and format characters (ESC,
// bidi overrides, ...) into spaces: transcript text safe to show on one line.
func cleanLine(s string) string {
	return oneLine(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s))
}

func firstChars(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// transcriptInfo is what the session picker shows from a transcript.
type transcriptInfo struct {
	// Title is Claude Code's own title for the conversation: the newest
	// custom title (/rename), else the newest generated one ("ai-title" or
	// an older "summary" entry); "" if none was seen.
	Title       string
	FirstPrompt string // the first real user prompt seen, one line
	LastPrompt  string // the last real user prompt seen, one line
}

// transcriptWindow caps how much of a transcript readTranscriptInfo scans:
// a file larger than twice this is read only at its head and tail.
const transcriptWindow = 1 << 20

// Display caps (runes) for transcript-derived one-line texts.
const (
	maxTitleRunes  = 120
	maxPromptRunes = 200
)

// readTranscriptInfo reads a session's title and first/last user prompts
// from its Claude Code transcript (see transcriptInfo). Large files are
// scanned only in their first and last transcriptWindow bytes (the first
// prompt is at the head, the last prompt and the newest titles near the
// tail), so a title or prompt in the middle of a huge file may be missed.
func readTranscriptInfo(path string) (transcriptInfo, error) {
	var info transcriptInfo
	f, err := os.Open(path)
	if err != nil {
		return info, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return info, err
	}
	var chunks [][]byte
	if size := fi.Size(); size <= 2*transcriptWindow {
		b, err := io.ReadAll(io.LimitReader(f, 2*transcriptWindow))
		if err != nil {
			return info, err
		}
		chunks = append(chunks, b)
	} else {
		head := make([]byte, transcriptWindow)
		if _, err := io.ReadFull(f, head); err != nil {
			return info, err
		}
		if i := bytes.LastIndexByte(head, '\n'); i >= 0 {
			head = head[:i+1] // drop the partial last line
		}
		tail := make([]byte, transcriptWindow)
		if _, err := f.ReadAt(tail, size-transcriptWindow); err != nil && !errors.Is(err, io.EOF) {
			return info, err
		}
		if i := bytes.IndexByte(tail, '\n'); i >= 0 {
			tail = tail[i+1:] // drop the partial first line
		}
		chunks = append(chunks, head, tail)
	}
	var custom, generated string
	for _, c := range chunks {
		for len(c) > 0 {
			line := c
			if i := bytes.IndexByte(c, '\n'); i >= 0 {
				line, c = c[:i], c[i+1:]
			} else {
				c = nil
			}
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var e struct {
				Type        string `json:"type"`
				CustomTitle string `json:"customTitle"`
				AITitle     string `json:"aiTitle"`
				Summary     string `json:"summary"`
			}
			if json.Unmarshal(line, &e) != nil {
				continue
			}
			switch e.Type {
			case "custom-title":
				if t := cleanLine(e.CustomTitle); t != "" {
					custom = t
				}
			case "ai-title":
				if t := cleanLine(e.AITitle); t != "" {
					generated = t
				}
			case "summary":
				if t := cleanLine(e.Summary); t != "" {
					generated = t
				}
			case "user":
				if role, text, _, ok := parseEntry(line); ok && role == "user" {
					if p := userPrompt(text); p != "" {
						if info.FirstPrompt == "" {
							info.FirstPrompt = p
						}
						info.LastPrompt = p
					}
				}
			}
		}
	}
	info.Title = custom
	if info.Title == "" {
		info.Title = generated
	}
	info.Title = firstChars(info.Title, maxTitleRunes)
	info.FirstPrompt = firstChars(info.FirstPrompt, maxPromptRunes)
	info.LastPrompt = firstChars(info.LastPrompt, maxPromptRunes)
	return info, nil
}

// userPrompt returns the text the human typed in a condensed user turn, on
// one line: lines that are only a bracketed marker ("[command: /clear]",
// "[Request interrupted by user]", ...) are dropped; "" if nothing is left.
func userPrompt(text string) string {
	var keep []string
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || (strings.HasPrefix(l, "[") && strings.HasSuffix(l, "]")) {
			continue
		}
		keep = append(keep, l)
	}
	return cleanLine(strings.Join(keep, " "))
}
