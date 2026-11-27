package gitx

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The poisoned repo: a canonical clone, its origin, a task's worktree with
// work in it and a checked-out submodule, and a second, clean worktree, with
// every setting known to make git run a program or go elsewhere planted in
// the config (local, worktree, included, the submodule's own), hooks and
// attributes they can write. Each planted program leaves a marker when it
// runs. Every exported function of the package then runs against it (see
// TestPoisonedRepoCoversEveryFunction): none may leave a marker, and those
// that cannot be made safe must refuse, naming the setting.
//
// Not covered here: the pager (git starts one only on a terminal; checked
// by hand with a pty: --no-pager wins over core.pager and pager.<cmd>) and
// a lazy fetch of a partial clone (TestNoLazyFetch).

// prog writes an executable at dir/name that leaves the marker name and
// exits (it does not read its input: a reader that waits for the end of its
// input would hang git, e.g. as an ssh command).
func (m markers) prog(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	writeFile(t, path, "#!/bin/sh\ntouch '"+filepath.Join(string(m), name)+"'\nexit 1\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// names lists the markers left so far.
func (m markers) names(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(string(m))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func (m markers) clear(t *testing.T) {
	t.Helper()
	for _, n := range m.names(t) {
		os.Remove(filepath.Join(string(m), n))
	}
}

type poisoned struct {
	fixture
	m      markers
	wt     string // the task's worktree: committed, changed, untracked work; a dirty submodule
	clean  string // a second worktree, clean (one file only touched)
	sub    string // the submodule in wt
	decoy  string // where the planted remote config points
	origin Origin // the pin
	// refusing: also the settings that make some commands refuse (a filter,
	// URL rewrite rules); without, every command must succeed.
	refusing bool
}

// newPoisoned builds the repos clean, then plants everything.
func newPoisoned(t *testing.T, refusing bool) *poisoned {
	t.Helper()
	ctx := context.Background()
	p := &poisoned{fixture: newFixture(t), m: newMarkers(t), refusing: refusing}
	p.origin = to(p.fixture.origin)

	// A submodule, and attributes naming filters and diff drivers (with
	// no driver defined yet, they do nothing).
	subsrc := filepath.Join(p.root, "subsrc")
	git(t, p.root, "init", "-q", "-b", "main", subsrc)
	commitFile(t, subsrc, "s.txt", "s\n", "sub")
	git(t, p.repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", subsrc, "sub")
	writeFile(t, filepath.Join(p.repo, ".gitattributes"), "* filter=evil diff=evil merge=evil\n")
	git(t, p.repo, "add", ".")
	git(t, p.repo, "commit", "-q", "-m", "submodule and attributes")
	git(t, p.repo, "push", "-q", "origin", "main")
	git(t, p.repo, "fetch", "-q", "origin")

	p.wt = filepath.Join(p.root, "wt-task")
	git(t, p.repo, "worktree", "add", "-q", "-b", "atl/task", p.wt, "origin/main")
	git(t, p.wt, "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init")
	p.sub = filepath.Join(p.wt, "sub")
	commitFile(t, p.wt, "work.txt", "work\n", "work")
	writeFile(t, filepath.Join(p.wt, "README.md"), "changed\n")
	writeFile(t, filepath.Join(p.wt, "new.txt"), "untracked\n")
	writeFile(t, filepath.Join(p.sub, "s.txt"), "changed in the submodule\n")
	p.clean = filepath.Join(p.root, "wt-clean")
	git(t, p.repo, "worktree", "add", "-q", "-b", "atl/clean", p.clean, "origin/main")
	git(t, p.repo, "branch", "-q", "spare", "origin/main")
	git(t, p.repo, "branch", "-q", "spare2", "origin/main")
	git(t, p.repo, "branch", "-q", "spare3", "origin/main")
	p.decoy = p.fixture.decoy(t)
	p.plant(t)
	// Stat data that no longer matches, same content: `git status` would
	// read it through the clean filter.
	readme := filepath.Join(p.clean, "README.md")
	future := mustStat(t, readme).ModTime().Add(5e9)
	if err := os.Chtimes(readme, future, future); err != nil {
		t.Fatal(err)
	}
	_ = ctx
	return p
}

func (p *poisoned) plant(t *testing.T) {
	t.Helper()
	m := p.m
	bin := filepath.Join(p.root, "bin")
	prog := func(name string) string { return m.prog(t, bin, name) }

	// Hooks of every event: in .git/hooks, in a core.hooksPath directory,
	// and as configured hooks (git 2.54+), in the clone, the origin and
	// the submodule.
	own := filepath.Join(p.root, "own-hooks")
	subGit := git(t, p.sub, "rev-parse", "--absolute-git-dir")
	for _, e := range hookEvents {
		for dir, prefix := range map[string]string{
			filepath.Join(p.repo, ".git", "hooks"):   "hook-",
			own:                                      "hookspath-",
			filepath.Join(p.fixture.origin, "hooks"): "origin-hook-",
			filepath.Join(subGit, "hooks"):           "sub-hook-",
		} {
			path := m.prog(t, dir, prefix+e)
			if err := os.Rename(path, filepath.Join(dir, e)); err != nil {
				t.Fatal(err)
			}
		}
		for _, dir := range []string{p.repo, p.fixture.origin, p.sub} {
			git(t, dir, "config", "hook.evil-"+e+".command", prog("cfghook-"+filepath.Base(dir)+"-"+e))
			git(t, dir, "config", "hook.evil-"+e+".event", e)
		}
	}
	// git looks up hooks by name in these directories: rename them back
	// from their marker names happened above; the marker name is in the
	// script.

	set := func(dir string, kv ...string) {
		for i := 0; i+1 < len(kv); i += 2 {
			git(t, dir, "config", "--add", kv[i], kv[i+1])
		}
	}
	// The clone's own config (shared by its worktrees).
	set(p.repo,
		"core.hooksPath", own,
		"core.fsmonitor", prog("fsmonitor"),
		"core.sshCommand", prog("ssh-command"),
		"core.askPass", prog("askpass"),
		"core.gitProxy", prog("git-proxy"),
		"core.alternateRefsCommand", prog("alternate-refs"),
		"core.pager", prog("pager"),
		"core.editor", prog("editor"),
		"sequence.editor", prog("sequence-editor"),
		"pager.diff", prog("pager-diff"),
		"pager.log", prog("pager-log"),
		"credential.helper", prog("credential-helper"),
		"credential.https://example.com.helper", prog("credential-helper-url"),
		"diff.external", prog("diff-external"),
		"diff.evil.command", prog("diff-command"),
		"diff.evil.textconv", prog("textconv"),
		"diff.evil2.textconv", prog("textconv2"),
		"merge.evil.driver", prog("merge-driver"),
		"gpg.program", prog("gpg"),
		"gpg.ssh.program", prog("gpg-ssh"),
		"log.showSignature", "true",
		"status.submoduleSummary", "true",
		"diff.submodule", "diff",
		"diff.ignoreSubmodules", "none",
		"submodule.recurse", "true",
		"submodule.sub.update", "!"+prog("submodule-update"),
		"fetch.recurseSubmodules", "true",
		"push.recurseSubmodules", "on-demand",
		"push.gpgSign", "true",
		"push.pushOption", "ci.skip",
		"push.followTags", "true",
		"gc.auto", "1",
		"gc.autoDetach", "false",
		"maintenance.auto", "true",
		"fetch.bundleURI", "file://"+filepath.Join(string(m), "bundle-uri"),
		"protocol.allow", "always",
		"protocol.file.allow", "always",
		"protocol.ext.allow", "always",
		"trace2.normalTarget", filepath.Join(string(m), "trace2-normal"),
		"trace2.eventTarget", filepath.Join(string(m), "trace2-event"),
		"uploadpack.packObjectsHook", prog("pack-objects-hook"),
		"remote.origin.url", p.decoy,
		"remote.origin.pushurl", p.decoy,
		"remote.origin.uploadpack", prog("upload-pack")+"; git-upload-pack",
		"remote.origin.receivepack", prog("receive-pack")+"; git-receive-pack",
		"remote.origin.mirror", "true",
		"remote.origin.tagOpt", "--tags",
		"remote."+p.fixture.origin+".uploadpack", prog("upload-pack-by-url"),
		"remote."+p.fixture.origin+".receivepack", prog("receive-pack-by-url"),
		"remote.pushDefault", "origin",
		"branch.atl/task.pushRemote", "origin",
		"extensions.worktreeConfig", "true",
	)
	// Remote names atl might be guessed to use.
	for _, name := range []string{"atl", "atl-0000000000000000"} {
		set(p.repo, "remote."+name+".url", p.decoy, "remote."+name+".uploadpack", prog("upload-pack-"+name))
	}
	// The worktree's own config, and attributes from $GIT_DIR/info.
	git(t, p.wt, "config", "--worktree", "core.fsmonitor", prog("fsmonitor-worktree"))
	writeFile(t, filepath.Join(p.repo, ".git", "info", "attributes"), "* filter=evil2 diff=evil2\n")
	// The origin's (a local path: its upload-pack and receive-pack run here).
	set(p.fixture.origin,
		"core.fsmonitor", prog("origin-fsmonitor"),
		"core.alternateRefsCommand", prog("origin-alternate-refs"),
		"uploadpack.packObjectsHook", prog("origin-pack-objects-hook"),
		"receive.autogc", "true",
		"core.hooksPath", own,
	)
	// The submodule's own config, and attributes in it: nothing atl runs
	// may look into the submodule's files.
	set(p.sub,
		"core.fsmonitor", prog("sub-fsmonitor"),
		"core.hooksPath", own,
		"filter.evil.clean", prog("sub-filter-clean"),
		"filter.evil.smudge", prog("sub-filter-smudge"),
		"filter.evil.process", prog("sub-filter-process"),
		"diff.external", prog("sub-diff-external"),
	)
	writeFile(t, filepath.Join(p.sub, ".gitattributes"), "* filter=evil\n")

	if !p.refusing {
		return
	}
	// What -c cannot override: filters, URL rewrite rules (one included).
	set(p.repo,
		"filter.evil.clean", prog("filter-clean"),
		"filter.evil.smudge", prog("filter-smudge"),
		"filter.evil.process", prog("filter-process"),
		"url."+p.decoy+".insteadOf", p.fixture.origin,
	)
	inc := filepath.Join(p.root, "included")
	writeFile(t, inc, "[filter \"evil2\"]\n\tclean = "+prog("filter2-clean")+"\n[url \""+p.decoy+"\"]\n\tpushInsteadOf = "+p.fixture.origin+"\n")
	set(p.repo, "include.path", inc)
}

// gitxCall runs one function of the package against p. A result of
// errSkip means "not applicable".
type gitxCall func(ctx context.Context, p *poisoned) error

func boolErr(_ bool, err error) error     { return err }
func intErr(_ int, err error) error       { return err }
func strErr(_ string, err error) error    { return err }
func strsErr(_ []string, err error) error { return err }

// refuses says which functions must refuse in the refusing fixture, and
// which setting they name.
var refuses = map[string]string{
	"AddWorktree":        "filter.evil.",
	"AddBranchWorktree":  "filter.evil.",
	"Diff":               "filter.evil",
	"PushBranch":         "url." + "DECOY",
	"FetchOrigin":        "url." + "DECOY",
	"RemoteBranchExists": "url." + "DECOY",
}

// gitxCalls is every exported function of the package that runs git, run
// against the poisoned repo, in this order. The last ones remove things.
var gitxCalls = []struct {
	name string
	call gitxCall
}{
	{"ConfigValue", func(ctx context.Context, p *poisoned) error { ConfigValue(ctx, p.wt, "user.name"); return nil }},
	{"IsRepo", func(ctx context.Context, p *poisoned) error {
		if !IsRepo(p.repo) || !IsRepo(p.wt) || !IsRepo(p.sub) {
			return errors.New("IsRepo = false")
		}
		return nil
	}},
	{"CheckWorktree", func(ctx context.Context, p *poisoned) error { return CheckWorktree(ctx, p.wt, p.repo) }},
	{"CurrentBranch", func(ctx context.Context, p *poisoned) error { return strErr(CurrentBranch(ctx, p.wt)) }},
	{"HeadCommit", func(ctx context.Context, p *poisoned) error { return strErr(HeadCommit(ctx, p.wt)) }},
	{"DefaultBranch", func(ctx context.Context, p *poisoned) error { return strErr(DefaultBranch(ctx, p.repo)) }},
	{"IntegrationBranch", func(ctx context.Context, p *poisoned) error { return strErr(IntegrationBranch(ctx, p.repo)) }},
	{"RevParse", func(ctx context.Context, p *poisoned) error { return strErr(RevParse(ctx, p.repo, "origin/main")) }},
	{"RefExists", func(ctx context.Context, p *poisoned) error {
		return boolErr(RefExists(ctx, p.repo, "refs/remotes/origin/main"))
	}},
	{"RefOrBelowExists", func(ctx context.Context, p *poisoned) error {
		return boolErr(RefOrBelowExists(ctx, p.repo, "refs/remotes/origin/atl/task"))
	}},
	{"UniqueCommits", func(ctx context.Context, p *poisoned) error { return intErr(UniqueCommits(ctx, p.repo, "atl/task")) }},
	{"UniqueCommitsAt", func(ctx context.Context, p *poisoned) error {
		tip, err := RevParse(ctx, p.repo, "atl/task")
		if err != nil {
			return err
		}
		return intErr(UniqueCommitsAt(ctx, p.repo, "atl/task", tip))
	}},
	{"OrphanCommits", func(ctx context.Context, p *poisoned) error { return intErr(OrphanCommits(ctx, p.wt)) }},
	{"CommitsAhead", func(ctx context.Context, p *poisoned) error { return intErr(CommitsAhead(ctx, p.wt, "origin/main")) }},
	{"LastCommit", func(ctx context.Context, p *poisoned) error {
		_, _, _, err := LastCommit(ctx, p.wt)
		return err
	}},
	{"Untracked", func(ctx context.Context, p *poisoned) error { return strsErr(Untracked(ctx, p.wt)) }},
	{"Worktrees", func(ctx context.Context, p *poisoned) error { return strsErr(Worktrees(ctx, p.repo)) }},
	{"CurrentOrigin", func(ctx context.Context, p *poisoned) error {
		_, _, err := CurrentOrigin(ctx, p.wt)
		return err
	}},
	{"Hazards", func(ctx context.Context, p *poisoned) error {
		_, err := Hazards(ctx, p.wt, &p.origin)
		return err
	}},
	{"GitHubRepo", func(ctx context.Context, p *poisoned) error { GitHubRepo(ctx, p.wt, p.origin); return nil }},
	{"IsDirty", func(ctx context.Context, p *poisoned) error {
		for dir, want := range map[string]bool{p.wt: true, p.clean: false, p.sub: true} {
			got, err := IsDirty(ctx, dir)
			if err != nil {
				return err
			}
			if got != want {
				return errors.New("IsDirty(" + dir + ") wrong")
			}
		}
		return nil
	}},
	{"Diff", func(ctx context.Context, p *poisoned) error {
		var out bytes.Buffer
		err := Diff(ctx, p.wt, "origin/main", true, &out)
		if err == nil && !strings.Contains(out.String(), "work.txt") {
			return errors.New("diff lacks work.txt: " + out.String())
		}
		return err
	}},
	{"RemoteBranchExists", func(ctx context.Context, p *poisoned) error {
		return boolErr(RemoteBranchExists(ctx, p.wt, p.origin, "main"))
	}},
	{"FetchOrigin", func(ctx context.Context, p *poisoned) error {
		var out bytes.Buffer
		err := FetchOrigin(ctx, p.repo, p.origin, &out)
		if strings.Contains(out.String(), "bundle") {
			return errors.New("fetch looked at a bundle URI: " + out.String())
		}
		return err
	}},
	{"PushBranch", func(ctx context.Context, p *poisoned) error {
		return PushBranch(ctx, p.wt, p.origin, "atl/task", false, io.Discard)
	}},
	{"AddWorktree", func(ctx context.Context, p *poisoned) error {
		return AddWorktree(ctx, p.repo, filepath.Join(p.root, "wt-new"), "atl/new", "origin/main", p.origin)
	}},
	{"AddBranchWorktree", func(ctx context.Context, p *poisoned) error {
		return AddBranchWorktree(ctx, p.repo, filepath.Join(p.root, "wt-spare3"), "spare3", p.origin)
	}},
	{"DeleteBranch", func(ctx context.Context, p *poisoned) error { return DeleteBranch(ctx, p.repo, "spare") }},
	{"DeleteBranchIfAt", func(ctx context.Context, p *poisoned) error {
		tip, err := RevParse(ctx, p.repo, "spare2")
		if err != nil {
			return err
		}
		return DeleteBranchIfAt(ctx, p.repo, "spare2", tip)
	}},
	{"RemoveWorktree", func(ctx context.Context, p *poisoned) error {
		// The task's has changes and a submodule: refused without force.
		if err := RemoveWorktree(ctx, p.repo, p.wt, false); err == nil || !strings.Contains(err.Error(), "submodules") {
			return errors.New("RemoveWorktree(wt) without force: " + errString(err))
		}
		// The clean one goes, though a file's stat data changed.
		if err := RemoveWorktree(ctx, p.repo, p.clean, false); err != nil {
			return err
		}
		return RemoveWorktree(ctx, p.repo, p.wt, true)
	}},
}

func errString(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}

// noGit are exported functions that do not run git.
var noGit = []string{"GuardedEnv", "RemoveLeftover"}

// TestPoisonedRepoCoversEveryFunction: every exported function of the
// package is either run against the poisoned repo or known not to run git.
// A function added to the package fails this until it is.
func TestPoisonedRepoCoversEveryFunction(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	covered := map[string]bool{}
	for _, c := range gitxCalls {
		covered[c.name] = true
	}
	for _, n := range noGit {
		covered[n] = true
	}
	var missing []string
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.IsExported() && !covered[fd.Name.Name] {
					missing = append(missing, fd.Name.Name)
				}
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("exported functions not run against the poisoned repo (add them to gitxCalls, or to noGit): %v", missing)
	}
}

func TestPoisonedRepoRunsNothing(t *testing.T) {
	for _, refusing := range []bool{false, true} {
		name := "overridable settings"
		if refusing {
			name = "and settings that refuse"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			p := newPoisoned(t, refusing)
			p.m.clear(t)
			decoyRefs := git(t, p.decoy, "for-each-ref")
			for _, c := range gitxCalls {
				err := c.call(ctx, p)
				if ran := p.m.names(t); len(ran) > 0 {
					t.Errorf("%s ran %v", c.name, ran)
					p.m.clear(t)
				}
				want, refused := refuses[c.name]
				want = strings.ReplaceAll(want, "DECOY", p.decoy)
				var re *RefusedError
				switch {
				case refusing && refused:
					if !errors.As(err, &re) || !strings.HasPrefix(re.Key, want) || !strings.Contains(err.Error(), re.Value) ||
						!strings.Contains(err.Error(), re.Key) {
						t.Errorf("%s: %v; want a refusal naming %s and its value", c.name, err, want)
					}
				case err != nil:
					t.Errorf("%s: %v", c.name, err)
				}
			}
			if got := git(t, p.decoy, "for-each-ref"); got != decoyRefs {
				t.Errorf("the decoy got refs:\n%s", got)
			}
			if !refusing {
				// The push went to the given origin, the fetch came from it.
				if got, want := git(t, p.fixture.origin, "rev-parse", "atl/task"), git(t, p.repo, "rev-parse", "atl/task"); got != want {
					t.Errorf("origin atl/task = %s, want %s", got, want)
				}
			}
		})
	}
}

// TestHazards: what Hazards lists, and what it leaves alone.
func TestHazards(t *testing.T) {
	ctx := context.Background()
	lfsGlobal := "[filter \"lfs\"]\n\tclean = git-lfs clean -- %f\n\tsmudge = git-lfs smudge -- %f\n\tprocess = git-lfs filter-process\n\trequired = true\n"
	cases := []struct {
		name   string
		global string
		local  []string // key, value pairs
		origin string   // the pin; "" → the fixture's origin
		want   []string // keys, "op" for ops
	}{
		{"healthy repo", "", []string{"core.hooksPath", ".husky", "core.fsmonitor", "true", "core.sshCommand", "ssh -i k",
			"http.postBuffer", "1000"}, "", nil},
		{"a filter", "", []string{"filter.x.clean", "x"}, "", []string{"filter.x.clean diff,checkout"}},
		{"an empty filter", "", []string{"filter.x.clean", ""}, "", nil},
		{"lfs defaults, locally", "", []string{"filter.lfs.clean", "git-lfs clean -- %f", "filter.lfs.process", "git-lfs filter-process",
			"lfs.repositoryformatversion", "0", "lfs.https://h/r.git/info/lfs.access", "basic"}, "", nil},
		{"lfs lookalike", "", []string{"filter.lfs.clean", "git-lfs clean -- %f; touch x"}, "", []string{"filter.lfs.clean diff,checkout"}},
		{"lfs settings without lfs", "", []string{"lfs.url", "https://e/", "lfs.customtransfer.x.path", "x"}, "", nil},
		{"lfs settings with lfs", lfsGlobal, []string{"lfs.url", "https://e/", "lfs.customtransfer.x.path", "x", "remote.origin.lfsurl", "https://e/",
			"remote.lfsdefault", "e", "lfs.repositoryformatversion", "0"}, "",
			// (git lists a section's keys together, where the section first was)
			[]string{"remote.origin.lfsurl checkout", "lfs.url checkout", "lfs.customtransfer.x.path diff,checkout", "remote.lfsdefault checkout"}},
		{"http with an ssh origin", "", []string{"http.proxy", "p"}, "git@h:o/r", nil},
		{"http with an https origin", "", []string{"http.proxy", "p", "http.https://h/.extraHeader", "x"}, "https://h/o/r",
			[]string{"http.proxy transport", "http.https://h/.extraheader transport"}},
		{"http with lfs", lfsGlobal, []string{"http.sslVerify", "false"}, "", []string{"http.sslverify checkout"}},
		{"url rules", "", []string{"url.x.insteadOf", "y"}, "", []string{"url.x.insteadof transport"}},
		{"url rules with lfs", lfsGlobal, []string{"url.x.pushInsteadOf", "y"}, "", []string{"url.x.pushinsteadof checkout,transport"}},
		{"lfs and another origin", lfsGlobal, []string{"remote.origin.url", "https://e/r"}, "", []string{"remote.origin.url checkout"}},
		// An include blocks everything checked, whether its file exists and
		// its condition holds now or not.
		{"an include", "", []string{"include.path", "/nonexistent.cfg"}, "", []string{"include.path diff,checkout,transport"}},
		{"a conditional include", "", []string{"includeIf.onbranch:nothing/**.path", "/nonexistent.cfg",
			"includeIf.hasconfig:remote.*.url:https://h/o/r.git.path", "/nonexistent.cfg"}, "",
			[]string{"includeif.onbranch:nothing/**.path diff,checkout,transport",
				"includeif.hasconfig:remote.*.url:https://h/o/r.git.path diff,checkout,transport"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			global := filepath.Join(t.TempDir(), "gitconfig")
			writeFile(t, global, c.global)
			t.Setenv("GIT_CONFIG_GLOBAL", global)
			for i := 0; i+1 < len(c.local); i += 2 {
				if c.local[i] == "remote.origin.url" {
					git(t, f.repo, "remote", "set-url", "origin", c.local[i+1])
					continue
				}
				git(t, f.repo, "config", "--add", c.local[i], c.local[i+1])
			}
			o := to(f.origin)
			if c.origin != "" {
				o = to(c.origin)
			}
			hs, err := Hazards(ctx, f.repo, &o)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, h := range hs {
				var ops []string
				for _, x := range []struct {
					o op
					s string
				}{{opDiff, "diff"}, {opCheckout, "checkout"}, {opTransport, "transport"}} {
					if h.ops&x.o != 0 {
						ops = append(ops, x.s)
					}
				}
				got = append(got, h.Key+" "+strings.Join(ops, ","))
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("hazards %q, want %q", got, c.want)
			}
		})
	}
}

// TestNoLazyFetch: in a partial clone, an object that is not there is not
// fetched from the promisor remote the config names.
func TestNoLazyFetch(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	git(t, f.origin, "config", "uploadpack.allowFilter", "true")
	partial := filepath.Join(f.root, "partial")
	git(t, f.root, "-c", "protocol.file.allow=always", "clone", "-q", "--no-local", "--filter=blob:none", "--no-checkout",
		"file://"+f.origin, partial)
	m := newMarkers(t)
	git(t, partial, "remote", "set-url", "origin", "ssh://example.invalid/x.git")
	git(t, partial, "config", "core.sshCommand", m.prog(t, t.TempDir(), "ssh-command"))
	// The worktree is empty (no checkout): every file is a change, and
	// none of their blobs is here.
	if _, err := IsDirty(ctx, partial); err != nil {
		t.Logf("IsDirty: %v", err)
	}
	var out bytes.Buffer
	if err := Diff(ctx, partial, "HEAD", false, &out); err != nil {
		t.Logf("Diff: %v", err)
	}
	if err := AddWorktree(ctx, partial, filepath.Join(f.root, "wt"), "atl/x", "HEAD", to(f.origin)); err == nil {
		t.Error("AddWorktree with missing blobs succeeded")
	}
	m.check(t)
}
