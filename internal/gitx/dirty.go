package gitx

import (
	"bufio"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/andreujuanc/atelier/internal/nofollow"
)

// IsDirty reports whether the worktree has uncommitted changes, counting
// untracked files (as `git status --porcelain` does, whatever
// status.showUntrackedFiles says) and changes inside checked-out
// submodules. It takes no locks and writes nothing, so it is safe to poll
// while other git commands run.
//
// It does not run `git status`: status reads a file whose stat data no
// longer matches the index through the clean filter the repo's config
// names, which is the task's to choose. Here git only lists what it has
// recorded (index against HEAD, the index's entries with their stat data,
// the untracked files), and the worktree's files are compared with the
// index here, by stat data and, where that is not conclusive, by hashing
// the file as it is, with no filter or line-ending conversion. Where a
// conversion applies (a filter, eol, text, working-tree-encoding), a file
// whose stat data changed but whose content did not reads as dirty: a
// conservative answer.
func IsDirty(ctx context.Context, worktree string) (bool, error) {
	return isDirty(ctx, worktree, 0)
}

// maxSubmoduleDepth bounds the recursion into nested submodules.
const maxSubmoduleDepth = 8

func isDirty(ctx context.Context, worktree string, depth int) (bool, error) {
	// The index against HEAD: staged changes.
	unborn := false
	if _, err := run(ctx, worktree, "diff-index", "--cached", "--quiet", "--ignore-submodules=none", "HEAD", "--"); err != nil {
		var ge *Error
		if errors.As(err, &ge) && ge.ExitCode == 1 && ge.Stderr == "" {
			return true, nil
		}
		if _, herr := run(ctx, worktree, "rev-parse", "--verify", "--quiet", "HEAD"); herr == nil {
			return false, err
		}
		unborn = true // no commit yet: anything in the index is staged
	}
	// Untracked files (directories as one entry).
	out, err := run(ctx, worktree, "ls-files", "-z", "--others", "--exclude-standard", "--directory", "--no-empty-directory")
	if err != nil {
		return false, err
	}
	if out != "" {
		return true, nil
	}
	ix, err := readIndex(ctx, worktree)
	if err != nil {
		return false, err
	}
	if unborn && len(ix.entries) > 0 {
		return true, nil
	}
	// The worktree's files are the task's: they are looked at through
	// package nofollow, so a symlink in place of a directory on the way to a
	// tracked file is a change, not a way to make atl read elsewhere, and a
	// FIFO or device in place of a file is a change, not something to block
	// on.
	d, err := nofollow.Open(worktree)
	if err != nil {
		return false, err
	}
	defer d.Close()
	// core.filemode is read only for a file whose executable bit differs from
	// its entry's: a clean worktree costs no git call for it.
	fileMode := sync.OnceValue(func() bool {
		v, err := run(ctx, worktree, "config", "--type=bool", "--get", "core.filemode")
		return err != nil || v != "false"
	})
	for i := range ix.entries {
		e := &ix.entries[i]
		if e.stage != 0 || e.flags&ceIntentToAdd != 0 {
			return true, nil // unmerged, or added with -N
		}
		if e.flags&(ceValid|ceSkipWorktree) != 0 {
			continue // assume-unchanged or outside the sparse checkout: git does not look either
		}
		if e.mode == modeGitlink {
			dirty, err := submoduleDirty(ctx, d, e.path, e.oid, depth)
			if err != nil || dirty {
				return dirty, err
			}
			continue
		}
		changed, err := ix.changed(e, d, fileMode)
		if err != nil || changed {
			return changed, err
		}
	}
	return false, nil
}

// submoduleDirty compares a submodule's checkout at rel in the worktree d
// with the commit oid its superproject records: dirty if it is gone, is at
// another commit or has changes of its own. An empty directory is a
// submodule that is not checked out, which is not a change.
func submoduleDirty(ctx context.Context, d *nofollow.Dir, rel, oid string, depth int) (bool, error) {
	fi, err := d.Lstat(rel)
	if gone(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !fi.IsDir() {
		return true, nil
	}
	if _, err := d.Lstat(rel + "/.git"); err != nil {
		return false, nil // not populated
	}
	path := filepath.Join(d.Path(), filepath.FromSlash(rel))
	head, err := run(ctx, path, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil || head != oid {
		return true, nil
	}
	if depth >= maxSubmoduleDepth {
		return true, nil // conservative
	}
	return isDirty(ctx, path, depth+1)
}

// populated reports whether the submodule directory path is checked out
// (has a .git).
func populated(path string) bool {
	_, err := os.Lstat(filepath.Join(path, ".git"))
	return err == nil
}

// gone reports whether err (from a nofollow lookup of a tracked path) means
// the path is not there as the index has it: missing, or a file or a
// symlink in place of a directory on the way.
func gone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, nofollow.ErrSymlink)
}

const (
	modeTypeMask = 0o170000
	modeRegular  = 0o100000
	modeSymlink  = 0o120000
	modeGitlink  = 0o160000

	// In-memory index entry flags, as `ls-files --debug` prints them.
	ceValid        = 0x8000  // assume-unchanged
	ceIntentToAdd  = 1 << 29 // git add -N
	ceSkipWorktree = 1 << 30
)

type statTime struct{ sec, nsec uint32 }

type indexEntry struct {
	mode, flags         uint32
	oid, path           string
	stage               int
	ctime, mtime        statTime
	ino, uid, gid, size uint32
}

type gitIndex struct {
	entries []indexEntry
	mtime   time.Time // of the index file; zero if there is none
}

// readIndex lists the index of the worktree with each entry's stat data
// (`git ls-files --stage --debug`), and the index file's mtime.
func readIndex(ctx context.Context, worktree string) (*gitIndex, error) {
	stdout, err := output(ctx, worktree, "", "ls-files", "-z", "--stage", "--debug")
	if err != nil {
		return nil, err
	}
	ix := &gitIndex{}
	r := bufio.NewReader(strings.NewReader(stdout))
	for {
		head, err := r.ReadString(0)
		if err == io.EOF && head == "" {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("git ls-files: truncated output")
		}
		e, err := parseIndexEntry(strings.TrimSuffix(head, "\x00"), r)
		if err != nil {
			return nil, err
		}
		ix.entries = append(ix.entries, e)
	}
	path, err := revParsePath(ctx, worktree, "--git-path", "index")
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(worktree, path)
	}
	if fi, err := os.Lstat(path); err == nil { // the index's own mtime, never what a symlink there names
		ix.mtime = fi.ModTime()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return ix, nil
}

// parseIndexEntry parses "<mode> <oid> <stage>\t<path>" and the five lines
// of stat data that follow it in r:
//
//	ctime: <sec>:<nsec>
//	mtime: <sec>:<nsec>
//	dev: <n>	ino: <n>
//	uid: <n>	gid: <n>
//	size: <n>	flags: <hex>
func parseIndexEntry(head string, r *bufio.Reader) (indexEntry, error) {
	var e indexEntry
	bad := func() (indexEntry, error) { return e, fmt.Errorf("git ls-files: unexpected output %q", head) }
	meta, path, ok := strings.Cut(head, "\t")
	f := strings.Fields(meta)
	if !ok || len(f) != 3 {
		return bad()
	}
	mode, err1 := strconv.ParseUint(f[0], 8, 32)
	stage, err2 := strconv.Atoi(f[2])
	if err1 != nil || err2 != nil {
		return bad()
	}
	e.mode, e.oid, e.stage, e.path = uint32(mode), f[1], stage, path
	vals := map[string]string{}
	for i := 0; i < 5; i++ {
		line, err := r.ReadString('\n')
		if err != nil {
			return bad()
		}
		for _, kv := range strings.Split(strings.TrimSpace(line), "\t") {
			k, v, ok := strings.Cut(kv, ": ")
			if !ok {
				return bad()
			}
			vals[k] = v
		}
	}
	u := func(k string, base int) uint32 {
		n, err := strconv.ParseUint(vals[k], base, 32)
		if err != nil {
			ok = false
		}
		return uint32(n)
	}
	t := func(k string) statTime {
		s, ns, found := strings.Cut(vals[k], ":")
		a, err1 := strconv.ParseUint(s, 10, 32)
		b, err2 := strconv.ParseUint(ns, 10, 32)
		if !found || err1 != nil || err2 != nil {
			ok = false
		}
		return statTime{uint32(a), uint32(b)}
	}
	e.ctime, e.mtime = t("ctime"), t("mtime")
	e.ino, e.uid, e.gid, e.size = u("ino", 10), u("uid", 10), u("gid", 10), u("size", 10)
	e.flags = u("flags", 16)
	if !ok {
		return bad()
	}
	return e, nil
}

// changed reports whether the file of index entry e in the worktree d
// differs from it. Matching stat data is enough unless the file may have
// changed within the second the index was written (git's "racily clean"
// case); otherwise the file is hashed as a blob, as is, and compared with
// the entry's object. Anything that is not what the index has there (a
// directory on the way that is a symlink; a FIFO, device or directory in
// place of a file) is a change.
func (ix *gitIndex) changed(e *indexEntry, d *nofollow.Dir, fileMode func() bool) (bool, error) {
	fi, err := d.Lstat(e.path)
	if gone(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	switch e.mode & modeTypeMask {
	case modeRegular:
		if !fi.Mode().IsRegular() {
			return true, nil
		}
		if (e.mode&0o100 != 0) != (fi.Mode().Perm()&0o100 != 0) && fileMode() {
			return true, nil
		}
	case modeSymlink:
		if fi.Mode()&fs.ModeSymlink == 0 {
			return true, nil
		}
	default:
		return true, nil
	}
	// Size 0 is what git records before it has the file's stat data
	// (read-tree): then, as git does, the content decides.
	if uint32(fi.Size()) != e.size && e.size != 0 {
		return true, nil
	}
	st, ok := fi.Sys().(*unix.Stat_t)
	if !ok {
		return false, fmt.Errorf("stat %s: no stat data", e.path)
	}
	if sameStat(e, st) && !ix.racy(e) {
		return false, nil
	}
	sum, err := blobHash(d, e.path, fi, len(e.oid))
	if errors.Is(err, nofollow.ErrNotRegular) || gone(err) {
		return true, nil // swapped meanwhile
	}
	if err != nil {
		return false, err
	}
	return sum != e.oid, nil
}

func sameStat(e *indexEntry, st *unix.Stat_t) bool {
	same := func(t statTime, ts unix.Timespec) bool {
		// An index written without nanoseconds has 0 there.
		return t.sec == uint32(ts.Sec) && (t.nsec == 0 || t.nsec == uint32(ts.Nsec))
	}
	return same(e.mtime, st.Mtim) && same(e.ctime, st.Ctim) &&
		e.ino == uint32(st.Ino) && e.uid == st.Uid && e.gid == st.Gid
}

// racy: the entry's file was modified no earlier than the second its stat
// data was recorded in the index, so a change within that second does not
// show in it.
func (ix *gitIndex) racy(e *indexEntry) bool {
	return ix.mtime.IsZero() || uint32(ix.mtime.Unix()) <= e.mtime.sec
}

// blobHash returns the object id the file rel of the worktree d would have
// as a blob, as it is (its target, for a symlink), in the object format
// whose ids are hexLen long. A file that changes size while it is read
// hashes to "". It opens only a regular file (nofollow.ErrNotRegular
// otherwise), without blocking.
func blobHash(d *nofollow.Dir, rel string, fi fs.FileInfo, hexLen int) (string, error) {
	var h hash.Hash
	switch hexLen {
	case 40:
		h = sha1.New()
	case 64:
		h = sha256.New()
	default:
		return "", fmt.Errorf("unknown object id length %d", hexLen)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		target, err := d.Readlink(rel)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "blob %d\x00%s", len(target), target)
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	f, err := d.OpenFile(rel)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fmt.Fprintf(h, "blob %d\x00", fi.Size())
	n, err := io.Copy(h, f)
	if err != nil {
		return "", err
	}
	if n != fi.Size() {
		return "", nil
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
