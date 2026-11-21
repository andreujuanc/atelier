package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/nofollow"
)

// A sandboxed task's Claude Code runs with its own config dir
// (Paths.ClaudeDir), never the user's: that one holds the login and every
// other session's transcripts. Claude Code in the task is authenticated by a
// token instead (see token.go: from atl's environment, else the one `atl
// token` stored), handed to each session by name at docker exec time.

// claudeConfigDirFor is the Claude config dir the task's sessions use: the
// task's own with the sandbox on, else the user's.
func (a *App) claudeConfigDirFor(t *model.Task) string {
	if a.Sandbox != nil {
		return a.Paths.ClaudeDir(t.Slug)
	}
	return config.ClaudeConfigDir()
}

// claudeCopied are what a task's Claude dir gets copied from the user's:
// their own setup, nothing secret. Never the credentials, the state file
// (account, MCP servers and their secrets), transcripts (projects/) or
// anything else Claude Code keeps there.
var claudeCopied = []string{"settings.json", "CLAUDE.md", "keybindings.json", "skills", "agents", "commands"}

// settingsDropped are the settings that run a program to get credentials:
// in a task they would run there (where the program and its secrets are not)
// and take precedence over the token.
var settingsDropped = []string{"apiKeyHelper", "awsAuthRefresh", "awsCredentialExport", "gcpAuthRefresh", "otelHeadersHelper"}

// Bounds of what seedClaudeDir copies from the user's dir.
const (
	maxSeedBytes = 64 << 20
	maxSeedFiles = 10000
	maxSeedDepth = 16
	maxStateFile = 16 << 20
)

// seedClaudeDir prepares the task's Claude dir for a new container (see
// sandbox.Ensure: none of the task's runs meanwhile, so nothing in the dir
// changes under it): it refreshes the copies of the user's setup
// (claudeCopied) and writes the state file's onboarding and trust keys.
// A copy that fails is a warning; a state file that cannot be written is an
// error (Claude Code would show its first-start screens).
//
// The dir is the task's to write; what is in it is reached through package
// nofollow, so a symlink the task left there is never written through.
func (a *App) seedClaudeDir(t *model.Task) error {
	dir := a.Paths.ClaudeDir(t.Slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	d, err := nofollow.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if user := config.ClaudeConfigDir(); user != "" {
		c := &seedCopy{forbidden: forbiddenSources(user), dst: d}
		for _, name := range claudeCopied {
			if err := c.refresh(filepath.Join(user, name), name); err != nil {
				a.warnf("task %s: copy %s of your Claude config: %v", t.Slug, name, err)
			}
		}
	}
	if err := writeClaudeState(d, a.Paths.TaskDir(t.Slug), os.Getenv("ANTHROPIC_API_KEY"), userTheme()); err != nil {
		return fmt.Errorf("task %s: Claude state file: %w", t.Slug, err)
	}
	return nil
}

// pruneClaudeDir removes the task's Claude dir when the task closes, except
// the transcripts (projects/), which atl show and atl transcript still read
// for closed tasks. Nothing runs in the task any more. What the task may
// have left in projects/ for atl to trip over later (symlinks, pipes) goes
// too: only plain files and directories stay. Failures are warnings.
func (a *App) pruneClaudeDir(t *model.Task) {
	dir := a.Paths.ClaudeDir(t.Slug)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		a.warnf("task %s: %v", t.Slug, err)
		return
	}
	for _, e := range entries {
		if e.Name() == "projects" && e.IsDir() {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			a.warnf("task %s: %v", t.Slug, err)
		}
	}
	projects := filepath.Join(dir, "projects")
	err = filepath.WalkDir(projects, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Type().IsRegular() {
			return err
		}
		return os.Remove(path)
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		a.warnf("task %s: %v", t.Slug, err)
	}
	os.Remove(projects) // only if empty
	os.Remove(dir)
}

// forbiddenSources are the real paths in the user's Claude dir that are
// never copied, even through a symlink in a copied directory: the
// credentials, the state file and the transcripts.
func forbiddenSources(user string) []string {
	real := user
	if r, err := filepath.EvalSymlinks(user); err == nil {
		real = r
	}
	out := []string{filepath.Join(real, ".credentials.json"), filepath.Join(real, ".claude.json"), filepath.Join(real, "projects")}
	if f := userStateFile(); f != "" {
		if r, err := filepath.EvalSymlinks(f); err == nil {
			f = r
		}
		out = append(out, f)
	}
	return out
}

// seedCopy copies from the user's Claude dir within the seed bounds into
// dst, the task's Claude dir.
type seedCopy struct {
	forbidden    []string
	dst          *nofollow.Dir
	bytes, files int
}

// refresh replaces name (a top-level entry of the task's Claude dir) with a
// copy of src, or removes it when src does not exist. It is removed first,
// without following it (os.RemoveAll does not follow the last component
// and walks what it removes by file descriptor): what the task left there
// (a symlink, say) is never written through. src is followed.
func (c *seedCopy) refresh(src, name string) error {
	if err := os.RemoveAll(filepath.Join(c.dst.Path(), name)); err != nil {
		return err
	}
	if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return c.copy(src, name, 0)
}

// copy copies src to dst, a path relative to the task's Claude dir.
func (c *seedCopy) copy(src, dst string, depth int) error {
	real, err := filepath.EvalSymlinks(src)
	if err != nil {
		return err
	}
	for _, f := range c.forbidden {
		if rel, err := filepath.Rel(f, real); err == nil && (rel == "." || !strings.HasPrefix(rel, "..")) {
			return nil // never carried over
		}
	}
	fi, err := os.Stat(real)
	if err != nil {
		return err
	}
	switch {
	case fi.IsDir():
		if depth >= maxSeedDepth {
			return fmt.Errorf("%s: nested too deep", src)
		}
		entries, err := os.ReadDir(real)
		if err != nil {
			return err
		}
		if err := c.dst.MkdirAll(dst, 0o700); err != nil {
			return err
		}
		var errs []error
		for _, e := range entries {
			if err := c.copy(filepath.Join(src, e.Name()), dst+"/"+e.Name(), depth+1); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	case fi.Mode().IsRegular():
		if c.files++; c.files > maxSeedFiles {
			return fmt.Errorf("%s: more than %d files", src, maxSeedFiles)
		}
		if c.bytes += int(fi.Size()); c.bytes > maxSeedBytes {
			return fmt.Errorf("%s: more than %d MiB in all", src, maxSeedBytes>>20)
		}
		data, err := os.ReadFile(real)
		if err != nil {
			return err
		}
		if dst == "settings.json" && depth == 0 {
			data = dropSettings(data)
		}
		return c.dst.WriteFile(dst, data, fi.Mode().Perm()&0o755|0o600)
	}
	return nil // sockets, devices, pipes: not setup
}

// dropSettings removes settingsDropped from a settings file; a file that is
// not a JSON object is copied as it is (Claude Code reports it).
func dropSettings(data []byte) []byte {
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil || root == nil {
		return data
	}
	n := len(root)
	for _, k := range settingsDropped {
		delete(root, k)
	}
	if len(root) == n {
		return data
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return data
	}
	return append(out, '\n')
}

// userStateFile is the user's Claude Code state file: .claude.json in
// $CLAUDE_CONFIG_DIR, else in the home directory ("" if unknown).
func userStateFile() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude.json")
}

// userTheme returns the "theme" of the user's state file, the one thing
// read from it ("" if none).
func userTheme() string {
	f, err := os.Open(userStateFile())
	if err != nil {
		return ""
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxStateFile+1))
	if err != nil || len(data) > maxStateFile {
		return ""
	}
	var st struct {
		Theme string `json:"theme"`
	}
	if json.Unmarshal(data, &st) != nil {
		return ""
	}
	return st.Theme
}

// writeClaudeState sets, in the state file .claude.json of the Claude dir
// dir, what keeps Claude Code from showing its first-start screens in the
// task: onboarding done (and the user's theme, if any and none is set),
// the task dir trusted, and, with an API key, that key approved (Claude Code
// asks once per key and keeps its last 20 characters). Everything else
// Claude Code wrote there is kept; a file that is not a JSON object is
// replaced.
func writeClaudeState(d *nofollow.Dir, taskDir, apiKey, theme string) error {
	st := map[string]any{}
	if data, err := d.ReadFile(".claude.json", maxStateFile); err == nil {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if dec.Decode(&st) != nil || st == nil {
			st = map[string]any{}
		}
	}
	st["hasCompletedOnboarding"] = true
	if _, ok := st["theme"]; !ok && theme != "" {
		st["theme"] = theme
	}
	projects, _ := st["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
	}
	forms := []string{taskDir}
	if r, err := filepath.EvalSymlinks(taskDir); err == nil && r != taskDir {
		forms = append(forms, r)
	}
	for _, d := range forms {
		p, _ := projects[d].(map[string]any)
		if p == nil {
			p = map[string]any{}
		}
		p["hasTrustDialogAccepted"] = true
		projects[d] = p
	}
	st["projects"] = projects
	if apiKey != "" {
		suffix := apiKey[max(0, len(apiKey)-20):]
		resp, _ := st["customApiKeyResponses"].(map[string]any)
		if resp == nil {
			resp = map[string]any{}
		}
		approved, _ := resp["approved"].([]any)
		found := false
		for _, v := range approved {
			found = found || v == suffix
		}
		if !found {
			approved = append(approved, suffix)
		}
		resp["approved"] = approved
		rejected, _ := resp["rejected"].([]any)
		kept := []any{}
		for _, v := range rejected {
			if v != suffix {
				kept = append(kept, v)
			}
		}
		resp["rejected"] = kept
		st["customApiKeyResponses"] = resp
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	// A symlink there is replaced, not followed.
	return d.WriteFile(".claude.json", append(data, '\n'), 0o644)
}
