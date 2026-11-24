// Package nofollow reads and writes files under a directory that something
// else (a sandboxed task) can write too, without a symlink taking it
// anywhere else.
//
// A Dir is opened once; every path under it is then walked component by
// component, each one opened relative to the one before with O_NOFOLLOW. A
// symlink anywhere on the way, the last component included, is an error
// (ErrSymlink), never followed, so a task that swaps a directory or a file
// for a symlink cannot make atl read, write, create, rename or remove
// anything outside the Dir. Files are opened without blocking and only
// regular ones are read (ErrNotRegular), so a FIFO or a device cannot hang
// or feed atl. Writes go to a new temporary file that is then renamed over
// the target: a symlink at the target is replaced, not written through.
//
// What is under the Dir is still the task's: atl must treat what it reads as
// data, and a file it wrote can be changed afterwards.
package nofollow

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var (
	// ErrSymlink is the error for a symlink met on the way or at the end.
	ErrSymlink = errors.New("is a symlink (not followed)")
	// ErrNotRegular is the error for opening something that is not a regular
	// file (a directory, FIFO, socket or device) as one.
	ErrNotRegular = errors.New("is not a regular file")
	// ErrTooLarge is the error of ReadFile for a file over its limit.
	ErrTooLarge = errors.New("is too large")
)

// Dir is a directory opened for confined access (see the package comment).
type Dir struct {
	fd   int
	path string
}

// Open opens the directory at path. Its parent directories are followed as
// they are (they are not the task's); path itself must not be a symlink.
func Open(path string) (*Dir, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, pathErr("open", path, err)
	}
	return &Dir{fd: fd, path: path}, nil
}

// Close closes d.
func (d *Dir) Close() error {
	if d.fd < 0 {
		return nil
	}
	err := unix.Close(d.fd)
	d.fd = -1
	return err
}

// Path is the path d was opened at.
func (d *Dir) Path() string { return d.path }

// split checks rel (slash-separated, relative, no "." or ".." components)
// and returns its components.
func (d *Dir) split(op, rel string) ([]string, error) {
	rel = filepath.ToSlash(rel)
	parts := strings.Split(rel, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return nil, &fs.PathError{Op: op, Path: d.join(rel), Err: errors.New("not a plain relative path")}
		}
	}
	return parts, nil
}

func (d *Dir) join(rel string) string { return filepath.Join(d.path, filepath.FromSlash(rel)) }

// parent opens the directory that holds rel's last component, walking its
// components without following symlinks, and returns its fd (which the
// caller closes; it may be d's own, see closeParent) and the last component.
func (d *Dir) parent(op, rel string) (fd int, base string, err error) {
	parts, err := d.split(op, rel)
	if err != nil {
		return -1, "", err
	}
	fd = d.fd
	for i, p := range parts[:len(parts)-1] {
		next, err := unix.Openat(fd, p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		d.closeParent(fd)
		if err != nil {
			return -1, "", pathErr(op, d.join(strings.Join(parts[:i+1], "/")), err)
		}
		fd = next
	}
	return fd, parts[len(parts)-1], nil
}

func (d *Dir) closeParent(fd int) {
	if fd != d.fd {
		unix.Close(fd)
	}
}

// pathErr wraps a system error, naming a symlink (ELOOP from O_NOFOLLOW, or
// ENOTDIR from O_DIRECTORY on a symlink to a file) as ErrSymlink where it
// is one.
func pathErr(op, path string, err error) error {
	if errors.Is(err, unix.ELOOP) {
		err = ErrSymlink
	} else if errors.Is(err, unix.ENOTDIR) {
		if fi, lerr := os.Lstat(path); lerr == nil && fi.Mode()&fs.ModeSymlink != 0 {
			err = ErrSymlink
		}
	}
	return &fs.PathError{Op: op, Path: path, Err: err}
}

// Lstat describes rel itself (a symlink is described, not followed). The
// FileInfo's Sys is a *unix.Stat_t.
func (d *Dir) Lstat(rel string) (fs.FileInfo, error) {
	fd, base, err := d.parent("lstat", rel)
	if err != nil {
		return nil, err
	}
	defer d.closeParent(fd)
	var st unix.Stat_t
	if err := unix.Fstatat(fd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, pathErr("lstat", d.join(rel), err)
	}
	return &fileInfo{name: base, st: st}, nil
}

// Readlink returns the target of the symlink rel, without following it.
func (d *Dir) Readlink(rel string) (string, error) {
	fd, base, err := d.parent("readlink", rel)
	if err != nil {
		return "", err
	}
	defer d.closeParent(fd)
	for size := 256; ; size *= 2 {
		buf := make([]byte, size)
		n, err := unix.Readlinkat(fd, base, buf)
		if err != nil {
			return "", pathErr("readlink", d.join(rel), err)
		}
		if n < size {
			return string(buf[:n]), nil
		}
	}
}

// OpenFile opens the regular file rel for reading, without following a
// symlink and without blocking on a FIFO or device.
func (d *Dir) OpenFile(rel string) (*os.File, error) {
	fd, base, err := d.parent("open", rel)
	if err != nil {
		return nil, err
	}
	defer d.closeParent(fd)
	f, err := unix.Openat(fd, base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENXIO) {
		// What open(2) says of a unix socket (or a device with no driver).
		err = ErrNotRegular
	}
	if err != nil {
		return nil, pathErr("open", d.join(rel), err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(f, &st); err != nil {
		unix.Close(f)
		return nil, pathErr("open", d.join(rel), err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(f)
		return nil, &fs.PathError{Op: "open", Path: d.join(rel), Err: ErrNotRegular}
	}
	if err := unix.SetNonblock(f, false); err != nil {
		unix.Close(f)
		return nil, pathErr("open", d.join(rel), err)
	}
	return os.NewFile(uintptr(f), d.join(rel)), nil
}

// ReadFile reads the regular file rel (see OpenFile), at most max bytes:
// a larger file is ErrTooLarge.
func (d *Dir) ReadFile(rel string, max int64) ([]byte, error) {
	f, err := d.OpenFile(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, &fs.PathError{Op: "read", Path: d.join(rel), Err: ErrTooLarge}
	}
	return data, nil
}

// WriteFile replaces rel with a regular file holding data, mode perm: it
// writes a new temporary file next to it, syncs it and renames it over rel.
// Whatever was at rel (a symlink included) is replaced, never written
// through; a directory there is an error.
func (d *Dir) WriteFile(rel string, data []byte, perm os.FileMode) error {
	fd, base, err := d.parent("write", rel)
	if err != nil {
		return err
	}
	defer d.closeParent(fd)
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := "." + base + ".tmp" + hex.EncodeToString(rnd[:])
	f, err := unix.Openat(fd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return pathErr("write", d.join(rel), err)
	}
	file := os.NewFile(uintptr(f), d.join(tmp))
	werr := func() error {
		if _, err := file.Write(data); err != nil {
			return err
		}
		if err := unix.Fchmod(f, uint32(perm.Perm())); err != nil {
			return err
		}
		return file.Sync()
	}()
	if cerr := file.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = unix.Renameat(fd, tmp, fd, base)
	}
	if werr != nil {
		unix.Unlinkat(fd, tmp, 0)
		return pathErr("write", d.join(rel), werr)
	}
	return nil
}

// MkdirAll creates rel and the directories on the way that are missing,
// mode perm (before the umask). Each one must be a directory, not a symlink
// to one.
func (d *Dir) MkdirAll(rel string, perm os.FileMode) error {
	parts, err := d.split("mkdir", rel)
	if err != nil {
		return err
	}
	fd := d.fd
	defer func() { d.closeParent(fd) }()
	for i, p := range parts {
		if err := unix.Mkdirat(fd, p, uint32(perm.Perm())); err != nil && !errors.Is(err, unix.EEXIST) {
			return pathErr("mkdir", d.join(strings.Join(parts[:i+1], "/")), err)
		}
		next, err := unix.Openat(fd, p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return pathErr("mkdir", d.join(strings.Join(parts[:i+1], "/")), err)
		}
		d.closeParent(fd)
		fd = next
	}
	return nil
}

// Mkdir creates the directory rel, mode perm (before the umask); one that
// exists already (anything at rel) is an error, fs.ErrExist.
func (d *Dir) Mkdir(rel string, perm os.FileMode) error {
	fd, base, err := d.parent("mkdir", rel)
	if err != nil {
		return err
	}
	defer d.closeParent(fd)
	if err := unix.Mkdirat(fd, base, uint32(perm.Perm())); err != nil {
		return pathErr("mkdir", d.join(rel), err)
	}
	return nil
}

// Remove removes the file, symlink or empty directory rel, without
// following a symlink.
func (d *Dir) Remove(rel string) error {
	fd, base, err := d.parent("remove", rel)
	if err != nil {
		return err
	}
	defer d.closeParent(fd)
	err = unix.Unlinkat(fd, base, 0)
	if errors.Is(err, unix.EISDIR) || errors.Is(err, unix.EPERM) {
		if derr := unix.Unlinkat(fd, base, unix.AT_REMOVEDIR); derr == nil || !errors.Is(derr, unix.ENOTDIR) {
			err = derr
		}
	}
	if err != nil {
		return pathErr("remove", d.join(rel), err)
	}
	return nil
}

// ReadDirNames returns the names of the entries of the directory rel, in
// no particular order.
func (d *Dir) ReadDirNames(rel string) ([]string, error) {
	fd, base, err := d.parent("readdir", rel)
	if err != nil {
		return nil, err
	}
	defer d.closeParent(fd)
	dir, err := unix.Openat(fd, base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, pathErr("readdir", d.join(rel), err)
	}
	f := os.NewFile(uintptr(dir), d.join(rel))
	defer f.Close()
	return f.Readdirnames(-1)
}

// RemoveAll removes rel and, if it is a directory, everything in it. It
// follows no symlink: one met is removed itself, and a directory swapped
// for one while it is emptied is left (an error), never walked through,
// since each directory is walked by the descriptor it was opened at.
func (d *Dir) RemoveAll(rel string) error {
	fd, base, err := d.parent("removeall", rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer d.closeParent(fd)
	if err := removeAllAt(fd, base); err != nil {
		return pathErr("removeall", d.join(rel), err)
	}
	return nil
}

// removeAllAt removes name, in the directory open at parent, and what is
// in it.
func removeAllAt(parent int, name string) error {
	err := unix.Unlinkat(parent, name, 0)
	if err == nil || errors.Is(err, unix.ENOENT) {
		return nil
	}
	if !errors.Is(err, unix.EISDIR) && !errors.Is(err, unix.EPERM) {
		return err
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(fd), name)
	defer dir.Close()
	// All names first: deleting while reading could skip some.
	names, err := dir.Readdirnames(-1)
	for _, n := range names {
		if rerr := removeAllAt(fd, n); rerr != nil && err == nil {
			err = rerr
		}
	}
	if err != nil {
		return err
	}
	if err := unix.Unlinkat(parent, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

// fileInfo is an fs.FileInfo from a unix.Stat_t.
type fileInfo struct {
	name string
	st   unix.Stat_t
}

func (fi *fileInfo) Name() string { return fi.name }
func (fi *fileInfo) Size() int64  { return fi.st.Size }
func (fi *fileInfo) ModTime() time.Time {
	sec, nsec := fi.st.Mtim.Unix()
	return time.Unix(sec, nsec)
}
func (fi *fileInfo) IsDir() bool { return fi.Mode().IsDir() }
func (fi *fileInfo) Sys() any    { return &fi.st }

func (fi *fileInfo) Mode() fs.FileMode {
	m := uint32(fi.st.Mode)
	mode := fs.FileMode(m & 0o777)
	switch m & unix.S_IFMT {
	case unix.S_IFDIR:
		mode |= fs.ModeDir
	case unix.S_IFLNK:
		mode |= fs.ModeSymlink
	case unix.S_IFIFO:
		mode |= fs.ModeNamedPipe
	case unix.S_IFSOCK:
		mode |= fs.ModeSocket
	case unix.S_IFCHR:
		mode |= fs.ModeDevice | fs.ModeCharDevice
	case unix.S_IFBLK:
		mode |= fs.ModeDevice
	}
	if m&unix.S_ISUID != 0 {
		mode |= fs.ModeSetuid
	}
	if m&unix.S_ISGID != 0 {
		mode |= fs.ModeSetgid
	}
	if m&unix.S_ISVTX != 0 {
		mode |= fs.ModeSticky
	}
	return mode
}
