package gen

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/andreujuanc/atelier/internal/config"
)

// hookEvents lists the Claude Code hook events atelier installs, in order.
var hookEvents = []struct {
	Name    string
	Matcher string
}{
	{"SessionStart", ""},
	{"UserPromptSubmit", ""},
	{"Notification", ""},
	{"Stop", ""},
	{"SessionEnd", ""},
	{"PreToolUse", "Edit|Write|MultiEdit|NotebookEdit"},
	{"PostToolUse", ""},
}

// ShellQuote quotes s as a single POSIX shell word using single quotes.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// HookCommand returns the hook command for event: the shell-quoted atlBin
// followed by " hook <event>".
func HookCommand(atlBin, event string) string {
	return ShellQuote(atlBin) + " hook " + event
}

// StatusLineCommand returns atelier's Claude Code status line command: the
// shell-quoted atlBin followed by " statusline".
func StatusLineCommand(atlBin string) string {
	return ShellQuote(atlBin) + " statusline"
}

// WriteSettings writes <taskDir>/.claude/settings.json with atelier's hooks
// and status line ("statusLine": {"type":"command","command":"<atl> statusline"}).
//
// It is safe to re-run: if the file exists and is valid JSON, every other
// top-level key and every non-atelier hook entry is kept, and existing
// atelier entries are replaced (so there are never duplicates and the atl
// path stays current). A statusLine atelier did not write is left alone;
// atelier's own is updated in place (other keys in it, e.g. padding, are
// kept). If the user's global Claude Code settings define a statusLine
// (see globalStatusLine), atelier's own is not added and an existing one is
// removed, since project settings would override the user's. A file that
// is not a valid settings object is backed up to settings.json.bak and
// replaced. The write is atomic.
func WriteSettings(taskDir, atlBin string) error {
	path, root, err := loadSettings(taskDir)
	if err != nil {
		return err
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	for _, ev := range hookEvents {
		setAtelierHook(hooks, ev.Name, ev.Matcher, ev.Name, atlBin)
	}
	root["hooks"] = hooks
	setStatusLine(root, atlBin, globalStatusLine(config.ClaudeUserSettings()))
	return saveSettings(path, root)
}

// setStatusLine sets root's statusLine to atelier's command unless it holds
// one atelier did not write (see isAtelierStatusLine), which is kept. If
// deferToGlobal is set (the user has a global statusLine), atelier's own
// entry is removed instead of added.
func setStatusLine(root map[string]any, atlBin string, deferToGlobal bool) {
	entry := map[string]any{}
	if cur, ok := root["statusLine"]; ok {
		if !isAtelierStatusLine(cur, atlBin) {
			return
		}
		entry = cur.(map[string]any)
	}
	if deferToGlobal {
		delete(root, "statusLine")
		return
	}
	entry["type"] = "command"
	entry["command"] = StatusLineCommand(atlBin)
	root["statusLine"] = entry
}

// maxGlobalSettings caps how much of the user's global settings file
// globalStatusLine reads; a larger file counts as having no statusLine.
const maxGlobalSettings = 4 << 20

// globalStatusLine reports whether the Claude Code settings file at path
// (the user's global ~/.claude/settings.json) defines a non-null
// statusLine. A missing, unreadable, oversized or unparsable file counts
// as not defining one; errors are deliberately not reported.
func globalStatusLine(path string) bool {
	if path == "" {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxGlobalSettings+1))
	if err != nil || len(data) > maxGlobalSettings {
		return false
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		return false
	}
	v, ok := root["statusLine"]
	return ok && string(bytes.TrimSpace(v)) != "null"
}

// isAtelierStatusLine reports whether v is a statusLine object written by
// atelier: a "command" that is "<binary> statusline" with an atl binary (as
// for hook commands, see isAtelierCommand).
func isAtelierStatusLine(v any, atlBin string) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	if typ, ok := m["type"]; ok && typ != "command" {
		return false
	}
	cmd, ok := m["command"].(string)
	return ok && isAtlInvocation(cmd, atlBin, "statusline")
}

// setAtelierHook replaces atelier's `hook <cmdEvent>` entries under the
// Claude Code hook event `event` with one fresh group (with matcher, if
// non-empty), keeping every other entry.
func setAtelierHook(hooks map[string]any, event, matcher, cmdEvent, atlBin string) {
	groups, _ := hooks[event].([]any)
	groups = removeAtelierHooks(groups, cmdEvent, atlBin)
	group := map[string]any{}
	if matcher != "" {
		group["matcher"] = matcher
	}
	group["hooks"] = []any{map[string]any{"type": "command", "command": HookCommand(atlBin, cmdEvent)}}
	hooks[event] = append(groups, group)
}

// loadSettings creates <dir>/.claude and returns the path of its
// settings.json and the parsed contents ({} if the file is missing or
// empty). A file that is not a valid settings object is backed up to
// settings.json.bak and treated as empty.
func loadSettings(dir string) (string, map[string]any, error) {
	cdir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		return "", nil, fmt.Errorf("gen: create %s: %w", cdir, err)
	}
	path := filepath.Join(cdir, "settings.json")
	root := map[string]any{}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return "", nil, fmt.Errorf("gen: read %s: %w", path, err)
	case len(bytes.TrimSpace(data)) == 0:
	default:
		if root, err = parseSettings(data); err != nil {
			if err := os.WriteFile(path+".bak", data, 0o644); err != nil {
				return "", nil, fmt.Errorf("gen: back up invalid %s: %w", path, err)
			}
			root = map[string]any{}
		}
	}
	return path, root, nil
}

// saveSettings writes root as indented JSON to path atomically.
func saveSettings(path string, root map[string]any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return fmt.Errorf("gen: encode settings: %w", err)
	}
	if err := writeAtomic(path, buf.Bytes()); err != nil {
		return fmt.Errorf("gen: write %s: %w", path, err)
	}
	return nil
}

// parseSettings decodes a settings file, requiring a JSON object whose
// "hooks" (if present) is an object of arrays. Numbers are kept verbatim.
func parseSettings(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}
	if root == nil {
		return nil, errors.New("settings is not a JSON object")
	}
	if _, err := dec.Token(); err == nil {
		return nil, errors.New("trailing data after settings object")
	}
	if h, ok := root["hooks"]; ok {
		hm, ok := h.(map[string]any)
		if !ok {
			return nil, errors.New(`"hooks" is not an object`)
		}
		for ev, v := range hm {
			if _, ok := v.([]any); !ok {
				return nil, fmt.Errorf("hooks.%s is not an array", ev)
			}
		}
	}
	return root, nil
}

// removeAtelierHooks drops atelier's hook entries from an event's matcher
// groups, dropping groups left with no hooks. Everything else is kept as is.
func removeAtelierHooks(groups []any, event, atlBin string) []any {
	out := groups[:0:0]
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			out = append(out, g)
			continue
		}
		hs, ok := gm["hooks"].([]any)
		if !ok {
			out = append(out, g)
			continue
		}
		kept := hs[:0:0]
		for _, h := range hs {
			if hm, ok := h.(map[string]any); ok {
				if cmd, ok := hm["command"].(string); ok && isAtelierCommand(cmd, event, atlBin) {
					continue
				}
			}
			kept = append(kept, h)
		}
		if len(kept) == 0 && len(hs) > 0 {
			continue
		}
		gm["hooks"] = kept
		out = append(out, gm)
	}
	return out
}

// isAtelierCommand reports whether cmd is an atelier hook command for event:
// "<binary> hook <event>" (nothing else) where the binary word is atlBin (quoted
// or not) or a path whose basename is an atl binary name (see isAtlName),
// so entries written by a renamed or moved binary are recognised too.
func isAtelierCommand(cmd, event, atlBin string) bool {
	return isAtlInvocation(cmd, atlBin, "hook", event)
}

// isAtlInvocation reports whether cmd is "<binary> <args...>" (nothing
// else) where the binary word is atlBin (quoted or not) or a path whose
// basename is an atl binary name (see isAtlName).
func isAtlInvocation(cmd, atlBin string, args ...string) bool {
	cmd = strings.TrimSpace(cmd)
	raw, word := firstWord(cmd)
	if raw == "" || !slices.Equal(strings.Fields(cmd[len(raw):]), args) {
		return false
	}
	return raw == ShellQuote(atlBin) || word == atlBin || isAtlName(filepath.Base(word))
}

// isAtlName reports whether a binary basename looks like atelier's CLI:
// "atl", or "atl" followed by a non-letter (atl-dev, atl_old, atl2, atl.bak).
// Names like "atlas" are not matched.
func isAtlName(base string) bool {
	if !strings.HasPrefix(base, "atl") {
		return false
	}
	if len(base) == 3 {
		return true
	}
	c := base[3]
	return !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z')
}

// firstWord returns the first shell word of s, both raw and with quoting
// removed (single quotes, double quotes and backslash escapes).
func firstWord(s string) (raw, word string) {
	var b strings.Builder
	i := 0
loop:
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			break loop
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				b.WriteString(s[i+1:])
				i = len(s)
				break loop
			}
			b.WriteString(s[i+1 : i+1+j])
			i += j + 2
		case c == '"':
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`", s[i+1]) >= 0 {
					i++
				}
				b.WriteByte(s[i])
				i++
			}
			i++
		case c == '\\' && i+1 < len(s):
			b.WriteByte(s[i+1])
			i += 2
		default:
			b.WriteByte(c)
			i++
		}
	}
	if i > len(s) {
		i = len(s)
	}
	return s[:i], b.String()
}

// writeAtomic writes data to path via a temp file in the same directory,
// synced to disk before the rename so a crash never leaves an empty file.
func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
