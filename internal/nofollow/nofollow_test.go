package nofollow

import (
	"errors"
	"io/fs"
	"net"
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

// A unix socket, which open(2) refuses with ENXIO, is ErrNotRegular too.
func TestOpenFileRefusesASocket(t *testing.T) {
	d, root, _, _ := setup(t)
	l, err := net.Listen("unix", filepath.Join(root, "sock"))
	must(t, err)
	defer l.Close()
	if _, err := d.ReadFile("sock", 100); !errors.Is(err, ErrNotRegular) {
		t.Errorf("ReadFile(sock) = %v, want ErrNotRegular", err)
	}
}

// RemoveAll removes a tree with symlinks in it, not their targets; a
// symlink on the way is an error; what is missing is no error.
// ReadDirNames lists a directory, not one through a symlink.
func TestRemoveAllDoesNotFollowSymlinks(t *testing.T) {
	d, root, victim, victimDir := setup(t)
	must(t, os.MkdirAll(filepath.Join(root, "tree", "a", "b"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "tree", "a", "b", "f"), []byte("x"), 0o644))
	must(t, os.Symlink(victimDir, filepath.Join(root, "tree", "a", "dirlink")))
	must(t, os.Symlink(victim, filepath.Join(root, "tree", "link")))
	names, err := d.ReadDirNames("tree")
	if err != nil || len(names) != 2 {
		t.Errorf("ReadDirNames(tree) = %q, %v; want a and link", names, err)
	}
	if _, err := d.ReadDirNames("dirlink"); !errors.Is(err, ErrSymlink) {
		t.Errorf("ReadDirNames(dirlink) = %v, want ErrSymlink", err)
	}
	if err := d.RemoveAll("dirlink/f"); !errors.Is(err, ErrSymlink) {
		t.Errorf("RemoveAll(dirlink/f) = %v, want ErrSymlink", err)
	}
	must(t, d.RemoveAll("tree"))
	must(t, d.RemoveAll("tree"))
	must(t, d.RemoveAll("link"))
	if _, err := os.Lstat(filepath.Join(root, "tree")); !os.IsNotExist(err) {
		t.Errorf("tree after RemoveAll: %v", err)
	}
	unchanged(t, victim, "the user's file\n")
	unchanged(t, filepath.Join(victimDir, "f"), "the user's other file\n")
}

// Mkdir makes a new directory only: not over anything, not through a
// symlink.
func TestMkdir(t *testing.T) {
	d, root, _, victimDir := setup(t)
	must(t, d.Mkdir("new", 0o755))
	if fi, err := os.Lstat(filepath.Join(root, "new")); err != nil || !fi.IsDir() {
		t.Fatalf("new: %v, %v", fi, err)
	}
	for _, rel := range []string{"new", "sub", "link", "dirlink"} {
		if err := d.Mkdir(rel, 0o755); !errors.Is(err, fs.ErrExist) {
			t.Errorf("Mkdir(%s) = %v, want ErrExist", rel, err)
		}
	}
	if err := d.Mkdir("dirlink/x", 0o755); !errors.Is(err, ErrSymlink) {
		t.Errorf("Mkdir(dirlink/x) = %v, want ErrSymlink", err)
	}
	if _, err := os.Stat(filepath.Join(victimDir, "x")); err == nil {
		t.Error("a directory was made through the symlink")
	}
}

// TestCreateExclAndRename: a lock file is made only where nothing is, a
// symlink included, never through one; a rename replaces a symlink itself.
func TestCreateExclAndRename(t *testing.T) {
	d, root, victim, victimDir := setup(t)
	if err := d.CreateExcl("sub/f.lock", []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateExcl("sub/f.lock", nil, 0o644); !errors.Is(err, fs.ErrExist) {
		t.Errorf("second CreateExcl: %v, want ErrExist", err)
	}
	if err := d.CreateExcl("link", nil, 0o644); !errors.Is(err, fs.ErrExist) {
		t.Errorf("CreateExcl on a symlink: %v, want ErrExist", err)
	}
	if err := d.CreateExcl("dirlink/new", nil, 0o644); !errors.Is(err, ErrSymlink) {
		t.Errorf("CreateExcl under a symlink: %v, want ErrSymlink", err)
	}
	if _, err := os.Stat(filepath.Join(victimDir, "new")); !os.IsNotExist(err) {
		t.Errorf("a file was made in the symlinked dir: %v", err)
	}
	must(t, os.Symlink(victim, filepath.Join(root, "sub", "g")))
	if err := d.Rename("sub/f.lock", "g"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "sub", "g")); string(data) != "new\n" {
		t.Errorf("sub/g = %q", data)
	}
	if data, _ := os.ReadFile(victim); string(data) != "the user's file\n" {
		t.Errorf("victim = %q", data)
	}
	if err := d.Rename("sub/g", "../x"); err == nil {
		t.Error("rename out of the directory: no error")
	}
}
