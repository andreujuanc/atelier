package gen

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// WriteSettings writes <taskDir>/.claude/settings.json with atelier's hooks.
//
// It is safe to re-run: if the file exists and is valid JSON, every other
// top-level key and every non-atelier hook entry is kept, and existing
// atelier entries are replaced (so there are never duplicates and the atl
// path stays current). A file that is not a valid settings object is
// backed up to settings.json.bak and replaced. The write is atomic.
func WriteSettings(taskDir, atlBin string) error {
	dir := filepath.Join(taskDir, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("gen: create %s: %w", dir, err)
	}
	path := filepath.Join(dir, "settings.json")

	root := map[string]any{}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("gen: read %s: %w", path, err)
	case len(bytes.TrimSpace(data)) == 0:
	default:
		if root, err = parseSettings(data); err != nil {
			if err := os.WriteFile(path+".bak", data, 0o644); err != nil {
				return fmt.Errorf("gen: back up invalid %s: %w", path, err)
			}
			root = map[string]any{}
		}
	}

	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	for _, ev := range hookEvents {
		groups, _ := hooks[ev.Name].([]any)
		groups = removeAtelierHooks(groups, ev.Name, atlBin)
		group := map[string]any{}
		if ev.Matcher != "" {
			group["matcher"] = ev.Matcher
		}
		group["hooks"] = []any{map[string]any{"type": "command", "command": HookCommand(atlBin, ev.Name)}}
		hooks[ev.Name] = append(groups, group)
	}
	root["hooks"] = hooks

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
	cmd = strings.TrimSpace(cmd)
	raw, word := firstWord(cmd)
	if rest := strings.Fields(cmd[len(raw):]); raw == "" || len(rest) != 2 || rest[0] != "hook" || rest[1] != event {
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
