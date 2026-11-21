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
	"syscall"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/gen"
	"github.com/andreujuanc/atelier/internal/model"
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
func (a *App) seedClaudeDir(t *model.Task) error {
	dir := a.Paths.ClaudeDir(t.Slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if user := config.ClaudeConfigDir(); user != "" {
		c := &seedCopy{forbidden: forbiddenSources(user)}
		for _, name := range claudeCopied {
			if err := c.refresh(filepath.Join(user, name), filepath.Join(dir, name)); err != nil {
				a.warnf("task %s: copy %s of your Claude config: %v", t.Slug, name, err)
			}
		}
	}
	if err := writeClaudeState(dir, a.Paths.TaskDir(t.Slug), os.Getenv("ANTHROPIC_API_KEY"), userTheme()); err != nil {
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

// seedCopy copies from the user's Claude dir within the seed bounds.
type seedCopy struct {
	forbidden    []string
	bytes, files int
}

// refresh replaces dst with a copy of src, or removes it when src does not
// exist. dst is removed first, without following it: what the task left
// there (a symlink, say) is never written through. src is followed.
func (c *seedCopy) refresh(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return c.copy(src, dst, 0)
}

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
		if err := os.Mkdir(dst, 0o700); err != nil {
			return err
		}
		var errs []error
		for _, e := range entries {
			if err := c.copy(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), depth+1); err != nil {
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
		if filepath.Base(dst) == "settings.json" && depth == 0 {
			data = dropSettings(data)
		}
		return writeNew(dst, data, fi.Mode().Perm()&0o755|0o600)
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

// writeNew creates path, which must not exist, without following a symlink.
func writeNew(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
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
func writeClaudeState(dir, taskDir, apiKey, theme string) error {
	path := filepath.Join(dir, ".claude.json")
	st := map[string]any{}
	if f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0); err == nil {
		fi, err := f.Stat()
		if err == nil && fi.Mode().IsRegular() && fi.Size() <= maxStateFile {
			data, _ := io.ReadAll(io.LimitReader(f, maxStateFile))
			dec := json.NewDecoder(bytes.NewReader(data))
			dec.UseNumber()
			if dec.Decode(&st) != nil || st == nil {
				st = map[string]any{}
			}
		}
		f.Close()
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
	// WriteAtomic renames over path: a symlink there is replaced, not followed.
	return gen.WriteAtomic(path, append(data, '\n'))
}
