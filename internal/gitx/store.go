package gitx

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/andreujuanc/atelier/internal/nofollow"
)

// checkGitDirs returns the git dir and the common git dir of the repo or
// worktree at dir (absolute), or refuses it while git there would read
// another repository's objects or refs, which a task that writes the git
// dir can arrange where atl's git runs (the host, every repo of the user's
// in reach) and the task's own cannot:
//
//   - a git dir that takes its refs and objects from another
//     (commondir): only a worktree's registration,
//     <common>/worktrees/<id>, may;
//   - objects/info/alternates, which adds another repository's objects;
//   - an entry of objects/ (a loose objects directory, pack/, info/) that
//     is a symlink, or objects/ itself one.
//
// Like the config checks (checkHazards), this is a check before git runs:
// a task rewriting its git dir in a tight loop can slip a change in
// between.
func checkGitDirs(ctx context.Context, dir string) (gitDir, common string, err error) {
	gitDir, err = revParsePath(ctx, dir, "--path-format=absolute", "--absolute-git-dir")
	if err != nil {
		return "", "", err
	}
	common, err = revParsePath(ctx, dir, "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", "", err
	}
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("refused: git in %s would read another repository's objects or refs: %s",
			printable(dir), fmt.Sprintf(format, args...))
	}
	if !samePath(gitDir, common) &&
		(filepath.Base(filepath.Dir(gitDir)) != "worktrees" || !samePath(filepath.Dir(filepath.Dir(gitDir)), common)) {
		return "", "", refuse("its git dir %s takes them from %s (its commondir file)", printable(gitDir), printable(common))
	}
	d, err := nofollow.Open(common)
	if err != nil {
		return "", "", refuse("%v", printable(err.Error()))
	}
	defer d.Close()
	names, err := d.ReadDirNames("objects")
	if err != nil {
		return "", "", refuse("%v", printable(err.Error()))
	}
	for _, name := range names {
		if fi, err := d.Lstat("objects/" + name); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			return "", "", refuse("%s is a symlink", printable(filepath.Join(common, "objects", name)))
		}
	}
	const alternates = "objects/info/alternates"
	if _, err := d.Lstat(alternates); err == nil {
		what := ""
		if data, err := d.ReadFile(alternates, 4096); err == nil {
			what = " (" + printable(strings.TrimSpace(string(data))) + ")"
		}
		return "", "", refuse("%s lists other object directories%s; if you set that up (git clone --reference), "+
			"run git repack -a -d in the repo, then remove the file", printable(filepath.Join(common, alternates)), what)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", "", refuse("%v", printable(err.Error()))
	}
	return gitDir, common, nil
}

// headCommit returns the commit HEAD names in the repo or worktree at dir,
// whose git dir is gitDir and common git dir common: its id, or the id of
// the branch it names (refCommit). In a files repo HEAD is read without
// following a symlink. In a reftable repo it is in the worktree's own ref
// store, which git reads: only the branch name or id it holds is taken
// from there, the branch read as refCommit reads it.
func headCommit(ctx context.Context, dir, gitDir, common string) (string, error) {
	reftable, err := usesReftable(ctx, dir)
	if err != nil {
		return "", err
	}
	var head string
	if reftable {
		if head, err = run(ctx, dir, "symbolic-ref", "--quiet", "HEAD"); err == nil {
			head = "ref: " + head
		} else if head, err = run(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
			return "", fmt.Errorf("read HEAD: %w", err)
		}
	} else {
		d, err := nofollow.Open(gitDir)
		if err != nil {
			return "", fmt.Errorf("read HEAD: %w", err)
		}
		defer d.Close()
		data, err := d.ReadFile("HEAD", 1024)
		if err != nil {
			return "", fmt.Errorf("read HEAD: %w", err)
		}
		head = strings.TrimRight(string(data), " \t\r\n")
	}
	if ref, ok := strings.CutPrefix(head, "ref: "); ok {
		return refCommit(ctx, dir, common, strings.TrimSpace(ref))
	}
	if !objectID.MatchString(head) {
		return "", fmt.Errorf("read HEAD: not a commit id: %q", firstChars(head, 80))
	}
	return head, nil
}

// refCommit returns the object id ref (refs/...) names in the repo at dir,
// common its common git dir, read without following a symlink: git follows
// one, to a ref of any repo where it runs. In a files repo that is its
// loose ref file, else its line in packed-refs (readRef); in a reftable
// repo, ref in a copy of its stack (reftableCommit). A symbolic ref is
// refused.
func refCommit(ctx context.Context, dir, common, ref string) (string, error) {
	reftable, err := usesReftable(ctx, dir)
	if err != nil {
		return "", err
	}
	if !reftable {
		return readRef(common, ref)
	}
	d, err := nofollow.Open(common)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", ref, err)
	}
	defer d.Close()
	return reftableCommit(ctx, dir, d, ref)
}

// readRef returns the object id ref (refs/...) names in the git dir common:
// its loose ref file, else its line in packed-refs, both read without
// following a symlink (see package nofollow): git follows one, to a ref of
// any repo where it runs. A symbolic ref is refused.
func readRef(common, ref string) (string, error) {
	if !strings.HasPrefix(ref, "refs/") {
		return "", fmt.Errorf("read %s: not a ref under refs/", printable(ref))
	}
	d, err := nofollow.Open(common)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", ref, err)
	}
	defer d.Close()
	id, _, err := liveRef(d, ref)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", ref, err)
	}
	if id == "" {
		return "", fmt.Errorf("no ref %s", ref)
	}
	return id, nil
}

// liveRef returns the object id ref (refs/...) has in the files repo whose
// common git dir is d, as git reads it, without following a symlink: its
// loose ref file (loose is true), else its line in packed-refs; "" if
// neither has it. A loose file that holds no object id (a symbolic ref) is
// an error.
func liveRef(d *nofollow.Dir, ref string) (id string, loose bool, err error) {
	data, err := d.ReadFile(ref, 1024)
	if err == nil {
		id := strings.TrimSuffix(string(data), "\n")
		if !objectID.MatchString(id) {
			return "", true, fmt.Errorf("not a commit id: %q", firstChars(id, 80))
		}
		return id, true, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", false, err
	}
	f, err := d.OpenFile("packed-refs")
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 64<<10)
	for sc.Scan() {
		if id, name, ok := strings.Cut(sc.Text(), " "); ok && name == ref && objectID.MatchString(id) {
			return id, false, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", false, fmt.Errorf("packed-refs: %w", err)
	}
	return "", false, nil
}
