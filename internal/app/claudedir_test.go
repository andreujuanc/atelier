package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/nofollow"
)

// TestSeedCopyForbiddenNamesStartingWithDots: what is under a forbidden
// source is never copied, even when its name starts with "..", which a
// path relative to the forbidden source then starts with too.
func TestSeedCopyForbiddenNamesStartingWithDots(t *testing.T) {
	user := t.TempDir()
	secret := filepath.Join(user, "projects", "..odd", "s.jsonl")
	if err := os.MkdirAll(filepath.Dir(secret), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("SECRET-TRANSCRIPT"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(user, "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(user, "projects", "..odd"), filepath.Join(user, "skills", "odd")); err != nil {
		t.Fatal(err)
	}
	// Next to a forbidden source, not under it: copied.
	if err := os.WriteFile(filepath.Join(user, "skills", "..fine.md"), []byte("fine"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	d, err := nofollow.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	c := &seedCopy{forbidden: forbiddenSources(user), dst: d}
	if err := c.refresh(filepath.Join(user, "skills"), "skills"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "skills", "odd")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a transcript under projects/ was carried over: %v", err)
	}
	if got := readFile(t, filepath.Join(dst, "skills", "..fine.md")); got != "fine" {
		t.Errorf("skills/..fine.md = %q", got)
	}
}

// TestPruneClaudeDirPastUnreadableDir: a directory in projects/ that
// cannot be read does not stop the prune: a symlink further on goes all
// the same, with a warning.
func TestPruneClaudeDirPastUnreadableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	root := t.TempDir()
	var out bytes.Buffer
	a := &App{Paths: config.Paths{Workspace: root, StateDir: filepath.Join(root, "state")}, Out: &out}
	task := &model.Task{Slug: "pruned"}
	dir := a.Paths.ClaudeDir(task.Slug)
	locked := filepath.Join(dir, "projects", "a")
	keep := filepath.Join(dir, "projects", "b", "t.jsonl")
	link := filepath.Join(dir, "projects", "b", "link.jsonl")
	for _, d := range []string{locked, filepath.Dir(keep)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(keep, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", link); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })

	a.pruneClaudeDir(task)
	if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("symlink left in projects/: %v", err)
	}
	if readFile(t, keep) != "{}\n" {
		t.Error("transcript removed")
	}
	if !bytes.Contains(out.Bytes(), []byte("warning: task pruned:")) {
		t.Errorf("no warning for the unreadable dir: %q", out.String())
	}
}
