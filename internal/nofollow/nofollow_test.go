package nofollow

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// setup returns a Dir on a fresh directory with a file, a subdirectory and,
// outside it, a victim file and directory that symlinks inside point to.
func setup(t *testing.T) (d *Dir, root, victim, victimDir string) {
	t.Helper()
	root = t.TempDir()
	outside := t.TempDir()
	victim = filepath.Join(outside, "victim")
	victimDir = filepath.Join(outside, "victim-dir")
	must(t, os.WriteFile(victim, []byte("the user's file\n"), 0o644))
	must(t, os.Mkdir(victimDir, 0o755))
	must(t, os.WriteFile(filepath.Join(victimDir, "f"), []byte("the user's other file\n"), 0o644))
	must(t, os.Mkdir(filepath.Join(root, "sub"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "sub", "f"), []byte("inside\n"), 0o644))
	must(t, os.Symlink(victim, filepath.Join(root, "link")))
	must(t, os.Symlink(victimDir, filepath.Join(root, "dirlink")))
	d, err := Open(root)
	must(t, err)
	t.Cleanup(func() { d.Close() })
	return d, root, victim, victimDir
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func unchanged(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Errorf("%s = %q, %v; want %q untouched", path, got, err, want)
	}
}

func TestReadsDoNotFollowSymlinks(t *testing.T) {
	d, _, _, _ := setup(t)
	if got, err := d.ReadFile("sub/f", 100); err != nil || string(got) != "inside\n" {
		t.Errorf("ReadFile(sub/f) = %q, %v", got, err)
	}
	for _, rel := range []string{"link", "dirlink/f"} {
		if _, err := d.ReadFile(rel, 100); !errors.Is(err, ErrSymlink) {
			t.Errorf("ReadFile(%s) = %v, want ErrSymlink", rel, err)
		}
	}
	if fi, err := d.Lstat("link"); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("Lstat(link) = %v, %v; want the symlink itself", fi, err)
	}
	if _, err := d.Lstat("dirlink/f"); !errors.Is(err, ErrSymlink) {
		t.Errorf("Lstat(dirlink/f) = %v, want ErrSymlink", err)
	}
	if target, err := d.Readlink("link"); err != nil || target == "" {
		t.Errorf("Readlink(link) = %q, %v", target, err)
	}
	if _, err := d.ReadFile("../x", 100); err == nil {
		t.Error("ReadFile(../x) succeeded")
	}
	if _, err := d.ReadFile("sub/f", 3); !errors.Is(err, ErrTooLarge) {
		t.Errorf("ReadFile over the limit = %v, want ErrTooLarge", err)
	}
	if _, err := d.ReadFile("missing", 3); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadFile(missing) = %v, want ErrNotExist", err)
	}
}

// A FIFO (or anything else that is not a regular file) is refused at once,
// not opened for a read that would block.
func TestOpenFileRefusesAFIFOWithoutBlocking(t *testing.T) {
	d, root, _, _ := setup(t)
	must(t, syscall.Mkfifo(filepath.Join(root, "fifo"), 0o644))
	done := make(chan error, 1)
	go func() { _, err := d.ReadFile("fifo", 100); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegular) {
			t.Errorf("ReadFile(fifo) = %v, want ErrNotRegular", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadFile(fifo) blocked")
	}
	if _, err := d.OpenFile("sub"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("OpenFile(sub) = %v, want ErrNotRegular", err)
	}
}

func TestWritesDoNotFollowSymlinks(t *testing.T) {
	d, root, victim, victimDir := setup(t)
	// A symlink at the last component is replaced, not written through.
	must(t, d.WriteFile("link", []byte("written\n"), 0o644))
	unchanged(t, victim, "the user's file\n")
	if fi, err := os.Lstat(filepath.Join(root, "link")); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o644 {
		t.Errorf("link after WriteFile: %v, %v; want a regular file, 0644", fi, err)
	}
	// A symlink on the way is an error.
	if err := d.WriteFile("dirlink/f", []byte("x"), 0o644); !errors.Is(err, ErrSymlink) {
		t.Errorf("WriteFile(dirlink/f) = %v, want ErrSymlink", err)
	}
	if err := d.WriteFile("dirlink/new", []byte("x"), 0o644); !errors.Is(err, ErrSymlink) {
		t.Errorf("WriteFile(dirlink/new) = %v, want ErrSymlink", err)
	}
	if _, err := os.Stat(filepath.Join(victimDir, "new")); err == nil {
		t.Error("a file was created through the symlink")
	}
	unchanged(t, filepath.Join(victimDir, "f"), "the user's other file\n")
	if err := d.MkdirAll("dirlink/x", 0o755); !errors.Is(err, ErrSymlink) {
		t.Errorf("MkdirAll(dirlink/x) = %v, want ErrSymlink", err)
	}
	if _, err := os.Stat(filepath.Join(victimDir, "x")); err == nil {
		t.Error("a directory was created through the symlink")
	}
	must(t, d.MkdirAll("a/b", 0o755))
	must(t, d.WriteFile("a/b/c", []byte("c"), 0o600))
	if got, err := os.ReadFile(filepath.Join(root, "a", "b", "c")); err != nil || string(got) != "c" {
		t.Errorf("a/b/c = %q, %v", got, err)
	}
	// Remove removes the symlink, not its target.
	must(t, d.Remove("dirlink"))
	unchanged(t, filepath.Join(victimDir, "f"), "the user's other file\n")
	must(t, d.Remove("a/b/c"))
	must(t, d.Remove("a/b"))
	if entries, _ := os.ReadDir(root); len(entries) != 3 { // sub, link, a
		t.Errorf("left in the dir: %v", entries)
	}
}

// Open refuses a symlink in place of the directory itself.
func TestOpenRefusesASymlink(t *testing.T) {
	_, root, _, _ := setup(t)
	if _, err := Open(filepath.Join(root, "dirlink")); !errors.Is(err, ErrSymlink) {
		t.Errorf("Open(dirlink) = %v, want ErrSymlink", err)
	}
}
