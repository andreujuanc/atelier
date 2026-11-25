package gitx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/andreujuanc/atelier/internal/nofollow"
)

// refStore is a git dir of atl's own, a temporary directory no task sees,
// that a push or fetch of a repo runs in ($GIT_DIR): it has a copy of the
// repo's refs (and shallow file), and the repo's objects are its objects
// ($GIT_OBJECT_DIRECTORY). git writes every ref, reflog, lock file and
// FETCH_HEAD there; sync then copies the refs that changed into the repo,
// through file descriptors that follow no symlink (package nofollow).
//
// Run in the repo itself, git writes them by path, and a task writes the
// repo's git dir: it could make a directory on the way (refs/remotes/origin
// or one below it, logs/refs/remotes/origin) a symlink to any directory of
// the user's, and git, where atl runs, would create or replace a file named
// after a branch there (another repo's branch, say), or append a reflog
// line to one; FETCH_HEAD a symlink to any file of the user's, and git
// would overwrite it.
//
// The repo's own config is not read there (the push or fetch defines its
// remote with -c, see newTransport). The user's (system and global) is,
// as it reads for the repo: its conditional includes (includeIf gitdir:,
// onbranch:) are taken in the repo and the result handed to git
// ($GIT_CONFIG_SYSTEM, $GIT_CONFIG_GLOBAL; see flatConfig). A reftable repo's
// store has a copy of its stack, and changes go back as new tables
// (syncReftable).
type refStore struct {
	dir      string            // the temporary git dir
	common   string            // the repo's common git dir
	reftable bool              // the repo's refs are in reftable, not files
	refs     map[string]string // the repo's refs, as copied in
	shallow  []byte            // the repo's shallow file; nil if none
	env      []string          // what git runs with on the store
}

func newRefStore(ctx context.Context, dir string) (*refStore, error) {
	_, common, err := checkGitDirs(ctx, dir)
	if err != nil {
		return nil, err
	}
	reftable, err := usesReftable(ctx, dir)
	if err != nil {
		return nil, err
	}
	format, err := run(ctx, dir, "rev-parse", "--show-object-format")
	if err != nil {
		return nil, err
	}
	head := "main"
	if b, err := CurrentBranch(ctx, dir); err == nil && b != "" && plainRefName("refs/heads/"+b) {
		head = b // for includeIf onbranch:
	}
	d, err := nofollow.Open(common)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	s := &refStore{common: common, reftable: reftable}
	if s.shallow, err = d.ReadFile("shallow", 64<<20); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if s.dir, err = os.MkdirTemp("", "atl-refs-"); err != nil {
		return nil, err
	}
	if err := s.fill(ctx, dir, d, format, head); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

// fill lays out the store: its config, HEAD (on head) and a copy of the
// refs of the repo at dir (d its common git dir), as packed-refs or, for a
// reftable repo, a copy of its stack (copyReftable); its shallow file; the
// user's config (flatConfig).
func (s *refStore) fill(ctx context.Context, dir string, d *nofollow.Dir, format, head string) error {
	s.env = []string{"GIT_DIR=" + s.dir, "GIT_OBJECT_DIRECTORY=" + filepath.Join(s.common, "objects")}
	files := map[string]string{}
	if s.reftable {
		if _, err := copyReftable(ctx, dir, d, s.dir); err != nil {
			return fmt.Errorf("read the refs of %s: %w", printable(dir), err)
		}
		var err error
		if s.refs, err = listRefs(ctx, s.dir); err != nil {
			return err
		}
		for _, args := range [][]string{{"config", "core.logAllRefUpdates", "false"}, {"symbolic-ref", "HEAD", "refs/heads/" + head}} {
			if _, err := run(ctx, s.dir, args...); err != nil {
				return err
			}
		}
	} else {
		var err error
		if s.refs, err = readRefs(d); err != nil {
			return fmt.Errorf("read the refs of %s: %w", printable(dir), err)
		}
		config := "[core]\n\trepositoryformatversion = 0\n\tbare = true\n"
		if format != "sha1" {
			config = "[core]\n\trepositoryformatversion = 1\n\tbare = true\n[extensions]\n\tobjectformat = " + format + "\n"
		}
		var packed strings.Builder
		packed.WriteString("# pack-refs with: sorted \n")
		for _, name := range slices.Sorted(maps.Keys(s.refs)) {
			packed.WriteString(s.refs[name] + " " + name + "\n")
		}
		files["config"], files["HEAD"], files["packed-refs"] = config, "ref: refs/heads/"+head+"\n", packed.String()
		if err := os.Mkdir(filepath.Join(s.dir, "refs"), 0o755); err != nil {
			return err
		}
	}
	if s.shallow != nil {
		files["shallow"] = string(s.shallow)
	}
	scopes := map[string]string{"global": "GIT_CONFIG_GLOBAL"}
	if os.Getenv("GIT_CONFIG_NOSYSTEM") == "" {
		scopes["system"] = "GIT_CONFIG_SYSTEM"
	}
	for scope, env := range scopes {
		files["user-"+scope] = flatConfig(ctx, dir, scope)
		s.env = append(s.env, env+"="+filepath.Join(s.dir, "user-"+scope))
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(s.dir, name), []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// flatConfig is the user's config file of scope (system or global) as git
// reads it for the repo at dir, includes in place (and conditional ones
// taken or not as for the repo), in the config file syntax: "" if there is
// none.
func flatConfig(ctx context.Context, dir, scope string) string {
	out, err := output(ctx, dir, "", "config", "--"+scope, "--includes", "--null", "--list")
	if err != nil {
		return ""
	}
	quote := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	var b strings.Builder
	for _, rec := range strings.Split(out, "\x00") {
		key, value, hasValue := strings.Cut(rec, "\n")
		first, last := strings.IndexByte(key, '.'), strings.LastIndexByte(key, '.')
		if first <= 0 || includeKey.MatchString(strings.ToLower(key)) {
			continue // and the include itself is not repeated
		}
		b.WriteString("[" + key[:first])
		if last > first {
			b.WriteString(` "` + quote.Replace(key[first+1:last]) + `"`)
		}
		b.WriteString("]\n\t" + key[last+1:])
		if hasValue {
			b.WriteString(` = "` + quote.Replace(value) + `"`)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (s *refStore) close() { os.RemoveAll(s.dir) }

// sync copies into the repo at dir the refs git changed in the store, and
// its shallow file if git changed it, without following a symlink: one in
// the way fails that ref. In a files repo, a ref git deleted (a prune) is
// deleted only if it is a loose ref in the repo; a packed one stays. In a
// reftable repo the changes go in as new tables (syncReftable).
func (s *refStore) sync(ctx context.Context, dir string) error {
	sd, err := nofollow.Open(s.dir)
	if err != nil {
		return err
	}
	defer sd.Close()
	var after map[string]string
	if s.reftable {
		after, err = listRefs(ctx, s.dir)
	} else {
		after, err = readRefs(sd)
	}
	if err != nil {
		return err
	}
	shallow, err := sd.ReadFile("shallow", 64<<20)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	d, err := nofollow.Open(s.common)
	if err != nil {
		return err
	}
	defer d.Close()
	var errs []error
	changed := map[string]string{} // "" for a deleted ref
	for name, id := range after {
		if s.refs[name] != id {
			changed[name] = id
		}
	}
	for name := range s.refs {
		if _, ok := after[name]; !ok {
			changed[name] = ""
		}
	}
	if s.reftable {
		var stdin strings.Builder
		for _, name := range slices.Sorted(maps.Keys(changed)) {
			if id := changed[name]; id != "" {
				stdin.WriteString("update " + name + " " + id + "\n")
			} else {
				stdin.WriteString("delete " + name + "\n")
			}
		}
		if stdin.Len() > 0 {
			errs = append(errs, syncReftable(ctx, dir, d, stdin.String()))
		}
	} else {
		for _, name := range slices.Sorted(maps.Keys(changed)) {
			if id := changed[name]; id != "" {
				if err := d.MkdirAll(path.Dir(name), 0o755); err != nil {
					errs = append(errs, fmt.Errorf("update %s: %w", name, err))
				} else if err := d.WriteFile(name, []byte(id+"\n"), 0o644); err != nil {
					errs = append(errs, fmt.Errorf("update %s: %w", name, err))
				}
			} else if fi, err := d.Lstat(name); err == nil && fi.Mode().IsRegular() {
				if err := d.Remove(name); err != nil {
					errs = append(errs, fmt.Errorf("delete %s: %w", name, err))
				}
			}
		}
	}
	switch {
	case bytes.Equal(shallow, s.shallow) && (shallow == nil) == (s.shallow == nil):
	case shallow == nil:
		if err := d.Remove("shallow"); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	default:
		if err := d.WriteFile("shallow", shallow, 0o644); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// syncReftable makes the changes stdin lists (git update-ref --stdin) in the
// reftable stack of the repo at dir, d its common git dir, as git adds to a stack,
// without following a symlink. The stack as it is now is copied into a
// git dir of atl's (copyReftable), git makes the changes there, and the
// tables it wrote (fresh names; its own compaction may merge old ones)
// are written next to the repo's and its tables.list replaced by the
// copy's: under git's lock (tables.list.lock, made exclusively), and only
// if the stack is still the one copied; else it starts over, a few times.
func syncReftable(ctx context.Context, dir string, d *nofollow.Dir, stdin string) error {
	const lock = "reftable/tables.list.lock"
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
		}
		var retry bool
		if retry, err = func() (bool, error) {
			priv, err := os.MkdirTemp("", "atl-refs-")
			if err != nil {
				return false, err
			}
			defer os.RemoveAll(priv)
			base, err := copyReftable(ctx, dir, d, priv)
			if err != nil {
				return false, err
			}
			args := []string{"-c", "core.logAllRefUpdates=false", "update-ref", "--stdin"}
			cmd := command(ctx, priv, args...)
			cmd.Env = append(cmd.Env, "GIT_OBJECT_DIRECTORY="+filepath.Join(d.Path(), "objects"))
			cmd.Stdin = strings.NewReader(stdin)
			if out, err := cmd.CombinedOutput(); err != nil {
				return false, wrap(ctx, args, string(out), err)
			}
			list, err := os.ReadFile(filepath.Join(priv, "reftable", "tables.list"))
			if err != nil {
				return false, err
			}
			tables := strings.Fields(string(list))
			if err := d.CreateExcl(lock, list, 0o644); errors.Is(err, fs.ErrExist) {
				return true, errors.New("its reftable stack is locked (tables.list.lock)")
			} else if err != nil {
				return false, err
			}
			done := false
			defer func() {
				if !done {
					d.Remove(lock)
				}
			}()
			now, err := d.ReadFile("reftable/tables.list", 1<<20)
			if err != nil {
				return false, err
			}
			if !slices.Equal(strings.Fields(string(now)), base) {
				return true, errors.New("its reftable stack changed meanwhile")
			}
			for _, name := range tables {
				if slices.Contains(base, name) {
					continue
				}
				if !reftableName.MatchString(name) {
					return false, fmt.Errorf("unexpected table %q", firstChars(name, 80))
				}
				data, err := os.ReadFile(filepath.Join(priv, "reftable", name))
				if err == nil {
					err = d.WriteFile("reftable/"+name, data, 0o644)
				}
				if err != nil {
					return false, err
				}
			}
			if err := d.Rename(lock, "tables.list"); err != nil {
				return false, err
			}
			done = true
			return false, nil
		}(); !retry {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("update the refs of %s: %w", printable(dir), err)
	}
	return nil
}

// readRefs returns the refs in the git dir d (packed-refs, then loose refs
// under refs/, which win), read without following a symlink: a loose ref
// that is a symlink, or under one, is left out, as are symbolic refs and
// names git would not take for a ref.
func readRefs(d *nofollow.Dir) (map[string]string, error) {
	refs := map[string]string{}
	f, err := d.OpenFile("packed-refs")
	if err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(nil, 64<<10)
		for sc.Scan() {
			if id, name, ok := strings.Cut(sc.Text(), " "); ok && objectID.MatchString(id) && plainRefName(name) {
				refs[name] = id
			}
		}
		err = sc.Err()
		f.Close()
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var walk func(rel string, depth int) error
	walk = func(rel string, depth int) error {
		names, err := d.ReadDirNames(rel)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, nofollow.ErrSymlink) {
				return nil
			}
			return err
		}
		for _, n := range names {
			p := rel + "/" + n
			fi, err := d.Lstat(p)
			switch {
			case err != nil:
				continue
			case fi.IsDir():
				if depth >= 64 {
					return fmt.Errorf("%s: refs nested too deep", p)
				}
				if err := walk(p, depth+1); err != nil {
					return err
				}
			case fi.Mode().IsRegular() && plainRefName(p):
				delete(refs, p) // a loose ref wins over a packed one
				data, err := d.ReadFile(p, 1024)
				if id := strings.TrimSuffix(string(data), "\n"); err == nil && objectID.MatchString(id) {
					refs[p] = id
				}
			}
		}
		return nil
	}
	if err := walk("refs", 0); err != nil {
		return nil, err
	}
	return refs, nil
}

// refNameChars matches the characters a ref name may not have
// (git-check-ref-format(1)).
var refNameChars = regexp.MustCompile(`[\x00-\x20\x7f~^:?*\[\\]|\.\.|@\{|//`)

// plainRefName reports whether name is a ref under refs/ that git takes for
// one (git check-ref-format).
func plainRefName(name string) bool {
	if !strings.HasPrefix(name, "refs/") || refNameChars.MatchString(name) || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") {
		return false
	}
	for _, c := range strings.Split(name, "/") {
		if c == "" || c[0] == '.' || strings.HasSuffix(c, ".lock") {
			return false
		}
	}
	return true
}

// writeSymref makes ref, in the repo at dir, a symbolic ref to target,
// without following a symlink.
func writeSymref(ctx context.Context, dir, ref, target string) error {
	common, err := revParsePath(ctx, dir, "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	d, err := nofollow.Open(common)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.MkdirAll(path.Dir(ref), 0o755); err != nil {
		return err
	}
	return d.WriteFile(ref, []byte("ref: "+target+"\n"), 0o644)
}

// deleteBranch deletes the local branch in repo, if oid is not "" only
// while its tip is oid, and refuses one a worktree has checked out (as git
// branch -D does). git deletes a loose ref by path, and rewrites
// packed-refs through a symlink: a task that made refs/heads/atl a symlink
// to a directory of the user's (or packed-refs one to a file of theirs)
// had git delete or rewrite the user's file. So the ref is deleted here,
// without following a symlink, as git deletes one (updateRef); in a
// reftable repo through syncReftable.
func deleteBranch(ctx context.Context, repo, branch, oid string) error {
	ref := "refs/heads/" + branch
	if !plainRefName(ref) {
		return fmt.Errorf("not a branch name %q", printable(branch))
	}
	out, err := run(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "branch "+ref {
			return fmt.Errorf("branch %s is checked out in a worktree", branch)
		}
	}
	_, common, err := checkGitDirs(ctx, repo)
	if err != nil {
		return err
	}
	d, err := nofollow.Open(common)
	if err != nil {
		return err
	}
	defer d.Close()
	check := func(cur string) error {
		switch {
		case cur == "":
			return fmt.Errorf("no ref %s", ref)
		case oid != "" && cur != oid:
			return fmt.Errorf("branch %s is at %s, not %s", branch, cur, oid)
		}
		return nil
	}
	if reftable, err := usesReftable(ctx, repo); err != nil {
		return err
	} else if !reftable {
		return updateRef(d, ref, "", check)
	}
	cur, err := refCommit(ctx, repo, common, ref)
	if err != nil {
		return err
	}
	if err := check(cur); err != nil {
		return err
	}
	return syncReftable(ctx, repo, d, "delete "+ref+" "+cur+"\n")
}

// updateRef sets ref (refs/...) in the files repo whose common git dir is
// d to id, or deletes it if id is "", as git does, without following a
// symlink. Under the ref's lock (<ref>.lock, made exclusively, holding id)
// it reads the ref as it is now (liveRef: "" for none), and check, given
// that, may refuse; nothing is done if the ref is at id already. Then the
// lock is renamed over the ref; or, for a delete, the ref leaves
// packed-refs (deletePacked), then its loose file goes, its reflog too: an
// older packed value it shadowed does not come back.
func updateRef(d *nofollow.Dir, ref, id string, check func(cur string) error) error {
	dir := path.Dir(ref)
	if err := d.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if id == "" {
		defer removeEmptyDirs(d, dir) // made for the lock, or emptied
	}
	var data []byte
	if id != "" {
		data = []byte(id + "\n")
	}
	lock := ref + ".lock"
	if err := createLock(d, lock, data); err != nil {
		return err
	}
	locked := true
	defer func() {
		if locked {
			d.Remove(lock)
		}
	}()
	cur, loose, err := liveRef(d, ref)
	if err != nil {
		return err
	}
	if err := check(cur); err != nil || cur == id {
		return err
	}
	if id != "" {
		if err := d.Rename(lock, path.Base(ref)); err != nil {
			return err
		}
		locked = false
		return nil
	}
	// The packed line goes whatever its id when a loose file shadows it;
	// else it is the ref's value, cur.
	packed := cur
	if loose {
		packed = ""
	}
	if err := deletePacked(d, ref, packed); err != nil {
		return err
	}
	if loose {
		if err := d.Remove(ref); err != nil {
			return err
		}
	}
	d.Remove("logs/" + ref) // its reflog, as git deletes it
	return nil
}

// removeEmptyDirs removes dir (refs/<kind>/...) of d and the directories
// above it that are empty, up to refs/<kind>, as git does when it deletes a
// ref.
func removeEmptyDirs(d *nofollow.Dir, dir string) {
	for ; strings.Count(dir, "/") >= 2; dir = path.Dir(dir) {
		if d.Remove(dir) != nil { // only an empty one
			break
		}
	}
}

// createLock creates the lock file rel of d, exclusively, holding data
// (see nofollow.Dir.CreateExcl). While someone else holds it, it waits for
// it, a second at most, as git waits for a ref's or packed-refs' lock.
func createLock(d *nofollow.Dir, rel string, data []byte) error {
	deadline := time.Now().Add(time.Second)
	for {
		err := d.CreateExcl(rel, data, 0o644)
		if !errors.Is(err, fs.ErrExist) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// deletePacked removes ref's line (and its peeled line) from packed-refs
// in d, if it is there, as git does: the new packed-refs written as
// packed-refs.lock (made exclusively, so git's own writers wait) and
// renamed over it, without following a symlink. If want is not "", the
// line must have that id; else it goes whatever its id (a loose ref
// shadows it).
func deletePacked(d *nofollow.Dir, ref, want string) error {
	data, err := d.ReadFile("packed-refs", 256<<20)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var kept strings.Builder
	found, skipPeeled := false, false
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if skipPeeled && strings.HasPrefix(line, "^") {
			continue
		}
		skipPeeled = false
		if id, name, ok := strings.Cut(strings.TrimSuffix(line, "\n"), " "); ok && name == ref {
			if want != "" && id != want {
				return fmt.Errorf("packed %s is at %s, not %s", ref, id, want)
			}
			found, skipPeeled = true, true
			continue
		}
		kept.WriteString(line)
	}
	if !found {
		return nil
	}
	const lock = "packed-refs.lock"
	if err := createLock(d, lock, []byte(kept.String())); err != nil {
		return err
	}
	if now, err := d.ReadFile("packed-refs", 256<<20); err != nil || !bytes.Equal(now, data) {
		d.Remove(lock)
		return errors.New("packed-refs changed meanwhile")
	}
	if err := d.Rename(lock, "packed-refs"); err != nil {
		d.Remove(lock)
		return err
	}
	return nil
}
