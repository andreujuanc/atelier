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
// ($GIT_CONFIG_SYSTEM, $GIT_CONFIG_GLOBAL; see flatConfig). Refs are files:
// a repo with another ref storage is refused.
type refStore struct {
	dir     string            // the temporary git dir
	common  string            // the repo's common git dir
	refs    map[string]string // the repo's refs, as copied in
	shallow []byte            // the repo's shallow file; nil if none
	env     []string          // what git runs with on the store
}

func newRefStore(ctx context.Context, dir string) (*refStore, error) {
	_, common, err := checkGitDirs(ctx, dir)
	if err != nil {
		return nil, err
	}
	if s := ConfigValue(ctx, dir, "extensions.refstorage"); s != "" && !strings.EqualFold(s, "files") {
		return nil, fmt.Errorf("the refs of %s are stored as %s: atl only reads and writes files", printable(dir), printable(s))
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
	s := &refStore{common: common}
	if s.refs, err = readRefs(d); err != nil {
		return nil, fmt.Errorf("read the refs of %s: %w", printable(dir), err)
	}
	if s.shallow, err = d.ReadFile("shallow", 64<<20); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if s.dir, err = os.MkdirTemp("", "atl-refs-"); err != nil {
		return nil, err
	}
	s.env = []string{"GIT_DIR=" + s.dir, "GIT_OBJECT_DIRECTORY=" + filepath.Join(common, "objects")}
	config := "[core]\n\trepositoryformatversion = 0\n\tbare = true\n"
	if format != "sha1" {
		config = "[core]\n\trepositoryformatversion = 1\n\tbare = true\n[extensions]\n\tobjectformat = " + format + "\n"
	}
	var packed strings.Builder
	packed.WriteString("# pack-refs with: sorted \n")
	for _, name := range slices.Sorted(maps.Keys(s.refs)) {
		packed.WriteString(s.refs[name] + " " + name + "\n")
	}
	files := map[string]string{"config": config, "HEAD": "ref: refs/heads/" + head + "\n", "packed-refs": packed.String()}
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
	err = os.Mkdir(filepath.Join(s.dir, "refs"), 0o755)
	for name, content := range files {
		if err == nil {
			err = os.WriteFile(filepath.Join(s.dir, name), []byte(content), 0o644)
		}
	}
	if err != nil {
		s.close()
		return nil, err
	}
	return s, nil
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

// sync copies into the repo the refs git changed in the store, and its
// shallow file if git changed it, without following a symlink: one in the
// way fails that ref. A ref git deleted (a prune) is deleted only if it is
// a loose ref in the repo; a packed one stays.
func (s *refStore) sync() error {
	sd, err := nofollow.Open(s.dir)
	if err != nil {
		return err
	}
	defer sd.Close()
	after, err := readRefs(sd)
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
	for _, name := range slices.Sorted(maps.Keys(after)) {
		if id := after[name]; s.refs[name] != id {
			if err := d.MkdirAll(path.Dir(name), 0o755); err != nil {
				errs = append(errs, fmt.Errorf("update %s: %w", name, err))
			} else if err := d.WriteFile(name, []byte(id+"\n"), 0o644); err != nil {
				errs = append(errs, fmt.Errorf("update %s: %w", name, err))
			}
		}
	}
	for name := range s.refs {
		if _, ok := after[name]; !ok {
			if fi, err := d.Lstat(name); err == nil && fi.Mode().IsRegular() {
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
