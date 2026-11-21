package gitx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestIsLocalURL(t *testing.T) {
	for url, want := range map[string]bool{
		"/srv/repo.git":                     true,
		"../repo":                           true,
		"repo":                              true,
		"./a:b":                             true,
		"file:///srv/repo.git":              true,
		"git@github.com:org/repo.git":       false,
		"host:path":                         false,
		"ssh://git@host:2222/org/repo.git":  false,
		"https://github.com/org/repo.git":   false,
		"git://127.0.0.1/x":                 false,
		"ext::sh -c touch% /tmp/x":          false,
		"codecommit::us-east-1://repo-name": false,
	} {
		if got := isLocalURL(url); got != want {
			t.Errorf("isLocalURL(%q) = %v, want %v", url, got, want)
		}
	}
}

// markers is a directory where planted programs leave a file named after
// themselves when they run.
type markers string

func newMarkers(t *testing.T) markers { return markers(t.TempDir()) }

// script writes an executable that leaves the marker name, at dir/name.
func (m markers) script(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	writeFile(t, path, "#!/bin/sh\ntouch '"+filepath.Join(string(m), name)+"'\ncat >/dev/null\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func (m markers) check(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(string(m))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("%s ran", e.Name())
	}
}

// plantRepoConfig writes hooks and config into repo (and its origin, when
// that is a local path) that would each run a program on a push or fetch.
func plantRepoConfig(t *testing.T, m markers, repo, origin string) {
	t.Helper()
	hooks := filepath.Join(repo, ".git", "hooks")
	own := filepath.Join(repo, "own-hooks")
	for _, h := range []string{"pre-push", "reference-transaction", "post-checkout"} {
		m.script(t, hooks, h)
		m.script(t, own, "own-"+h)
		if err := os.Rename(filepath.Join(own, "own-"+h), filepath.Join(own, h)); err != nil {
			t.Fatal(err)
		}
	}
	if origin != "" {
		for _, h := range []string{"pre-receive", "update", "post-receive", "post-update", "reference-transaction"} {
			m.script(t, filepath.Join(origin, "hooks"), "origin-"+h)
			if err := os.Rename(filepath.Join(origin, "hooks", "origin-"+h), filepath.Join(origin, "hooks", h)); err != nil {
				t.Fatal(err)
			}
		}
	}
	bin := filepath.Join(t.TempDir(), "bin")
	for key, value := range map[string]string{
		"core.hooksPath":            own,
		"core.fsmonitor":            m.script(t, bin, "fsmonitor"),
		"core.sshCommand":           m.script(t, bin, "ssh-command"),
		"core.askPass":              m.script(t, bin, "askpass"),
		"core.gitProxy":             m.script(t, bin, "git-proxy"),
		"core.alternateRefsCommand": m.script(t, bin, "alternate-refs"),
		"credential.helper":         m.script(t, bin, "credential-helper"),
		"remote.origin.receivepack": m.script(t, bin, "receive-pack") + "; git-receive-pack",
		"remote.origin.uploadpack":  m.script(t, bin, "upload-pack") + "; git-upload-pack",
		"push.gpgSign":              "true",
		"gpg.program":               m.script(t, bin, "gpg"),
		"push.pushOption":           "ci.skip",
		"push.followTags":           "true",
		"push.recurseSubmodules":    "on-demand",
		"remote.origin.mirror":      "true",
		"fetch.recurseSubmodules":   "true",
		"submodule.recurse":         "true",
		"protocol.allow":            "always",
	} {
		git(t, repo, "config", key, value)
	}
}

// to is an Origin with one URL for fetch and push.
func to(url string) Origin { return Origin{URLs: []string{url}} }

func TestPushBranchPushesOnlyTheBranchAndRunsNothing(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	wt, _ := f.addWorktree(t, ctx, "t1")
	commitFile(t, wt, "work.txt", "work\n", "work")
	git(t, wt, "tag", "-a", "-m", "tag", "v1")
	// main moved too, but only locally.
	commitFile(t, f.repo, "local.txt", "local\n", "local main")
	originMain := git(t, f.origin, "rev-parse", "main")
	m := newMarkers(t)
	plantRepoConfig(t, m, f.repo, f.origin)
	origin := to(f.origin)

	var out bytes.Buffer
	if err := PushBranch(ctx, wt, origin, "atl/t1", false, &out); err != nil {
		t.Fatalf("push: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "atl/t1") {
		t.Errorf("git's output did not come through: %q", out.String())
	}
	refs := git(t, f.origin, "for-each-ref", "--format=%(refname) %(objectname)")
	want := "refs/heads/atl/t1 " + git(t, wt, "rev-parse", "HEAD") + "\nrefs/heads/main " + originMain
	if refs != want {
		t.Errorf("origin refs:\n%s\nwant:\n%s", refs, want)
	}
	m.check(t)

	// A rewritten branch needs force, and gets it with a lease.
	git(t, wt, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "commit", "-q", "--amend", "-m", "work, amended")
	err := PushBranch(ctx, wt, origin, "atl/t1", false, &out)
	var ge *Error
	if !errors.As(err, &ge) || ge.ExitCode <= 0 || ge.Stderr == "" || !strings.Contains(err.Error(), "git push") {
		t.Errorf("non-fast-forward push without force: %#v", err)
	}
	if err := PushBranch(ctx, wt, origin, "atl/t1", true, &out); err != nil {
		t.Fatalf("push --force: %v\n%s", err, out.String())
	}
	if got, want := git(t, f.origin, "rev-parse", "atl/t1"), git(t, wt, "rev-parse", "HEAD"); got != want {
		t.Errorf("origin atl/t1 = %s after the forced push, want %s", got, want)
	}
	m.check(t)
}

func TestFetchOriginRunsNothing(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	tip := f.pushFromOtherClone(t, "new")
	m := newMarkers(t)
	plantRepoConfig(t, m, f.repo, f.origin)
	var out bytes.Buffer
	if err := FetchOrigin(ctx, f.repo, to(f.origin), &out); err != nil {
		t.Fatalf("fetch: %v\n%s", err, out.String())
	}
	if got := git(t, f.repo, "rev-parse", "origin/main"); got != tip {
		t.Errorf("origin/main = %s after the fetch, want %s", got, tip)
	}
	m.check(t)
}

func TestCurrentOrigin(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if o, ok, err := CurrentOrigin(ctx, f.repo); err != nil || !ok || !o.Equal(to(f.origin)) {
		t.Errorf("CurrentOrigin = %+v, %v, %v; want %s", o, ok, err, f.origin)
	}
	// What the config says, before the user's rules.
	global := filepath.Join(t.TempDir(), "gitconfig")
	writeFile(t, global, "[url \"git@github.com:\"]\n\tinsteadOf = https://github.com/\n")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	git(t, f.repo, "remote", "set-url", "origin", "https://github.com/o/r.git")
	git(t, f.repo, "remote", "set-url", "--add", "--push", "origin", "/srv/a.git")
	git(t, f.repo, "remote", "set-url", "--add", "--push", "origin", "/srv/b.git")
	want := Origin{URLs: []string{"https://github.com/o/r.git"}, PushURLs: []string{"/srv/a.git", "/srv/b.git"}}
	if o, ok, err := CurrentOrigin(ctx, f.repo); err != nil || !ok || !o.Equal(want) {
		t.Errorf("CurrentOrigin = %+v, %v, %v; want %+v", o, ok, err, want)
	}
	if got := want.String(); got != "https://github.com/o/r.git (push: /srv/a.git, /srv/b.git)" {
		t.Errorf("String = %q", got)
	}
	// An empty value clears the list, as for git.
	git(t, f.repo, "config", "--add", "remote.origin.pushurl", "")
	git(t, f.repo, "config", "--add", "remote.origin.pushurl", "/srv/c.git")
	if o, _, _ := CurrentOrigin(ctx, f.repo); !slices.Equal(o.PushURLs, []string{"/srv/c.git"}) {
		t.Errorf("push URLs after an empty value = %q", o.PushURLs)
	}
	git(t, f.repo, "remote", "remove", "origin")
	if o, ok, err := CurrentOrigin(ctx, f.repo); err != nil || ok {
		t.Errorf("CurrentOrigin without origin = %+v, %v, %v", o, ok, err)
	}
	if o, ok, _ := CurrentOrigin(ctx, t.TempDir()); ok {
		t.Errorf("CurrentOrigin outside a repo = %+v", o)
	}
}

// TestUserRulesApplyOnce: the pin is the config's URL, and the user's own
// insteadOf rules apply to it once, even one whose replacement starts with
// what it replaces (applied twice, it would grow the URL again).
func TestUserRulesApplyOnce(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	decoy := f.decoy(t)
	tip := f.pushFromOtherClone(t, "new")
	global := filepath.Join(t.TempDir(), "gitconfig")
	// "short:" → the origin's path; and a rule whose replacement starts with
	// its own prefix: <root>/ → <root>/x/ would make <root>/x/x/... if
	// applied to its own result.
	writeFile(t, global, "[url \""+f.origin+"\"]\n\tinsteadOf = short:\n"+
		"[url \""+f.root+"/x/\"]\n\tinsteadOf = "+f.root+"/\n")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	git(t, f.repo, "remote", "set-url", "origin", "short:")
	o, ok, err := CurrentOrigin(ctx, f.repo)
	if err != nil || !ok || !o.Equal(Origin{URLs: []string{"short:"}}) {
		t.Fatalf("CurrentOrigin = %+v, %v, %v", o, ok, err)
	}
	var out bytes.Buffer
	if err := FetchOrigin(ctx, f.repo, o, &out); err != nil {
		t.Fatalf("fetch: %v\n%s", err, out.String())
	}
	if got := git(t, f.repo, "rev-parse", "origin/main"); got != tip {
		t.Errorf("origin/main = %s, want %s", got, tip)
	}
	// The double rewrite: a path under the root, pinned as it is.
	if err := os.MkdirAll(filepath.Join(f.root, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(decoy, filepath.Join(f.root, "x", "decoy.git")); err != nil {
		t.Fatal(err)
	}
	p := Origin{URLs: []string{filepath.Join(f.root, "decoy.git")}}
	if got := p.fetchURLs(userConfig(ctx, f.repo)); len(got) != 1 || got[0] != filepath.Join(f.root, "x", "decoy.git") {
		t.Errorf("fetch URL with the user's rules = %q", got)
	}
	if err := FetchOrigin(ctx, f.repo, p, &out); err != nil {
		t.Errorf("fetch through a rule applied once: %v\n%s", err, out.String())
	}
}

// TestFetchCreatesOriginHead: like `git fetch` since git 2.48, a fetch
// creates a missing origin/HEAD (the stand-in remote cannot), and leaves an
// existing one alone.
func TestFetchCreatesOriginHead(t *testing.T) {
	ctx := context.Background()
	f := newFixtureBranch(t, "trunk")
	head := func() string {
		t.Helper()
		out, err := run(ctx, f.repo, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD")
		if err != nil {
			return ""
		}
		return out
	}
	git(t, f.repo, "remote", "set-head", "origin", "-d")
	if err := FetchOrigin(ctx, f.repo, to(f.origin), nil); err != nil {
		t.Fatal(err)
	}
	if got := head(); got != "refs/remotes/origin/trunk" {
		t.Errorf("origin/HEAD = %q after the fetch", got)
	}
	if got, err := DefaultBranch(ctx, f.repo); err != nil || got != "trunk" {
		t.Errorf("DefaultBranch = %q, %v", got, err)
	}
	f.pushFromOtherCloneBranch(t, "other")
	git(t, f.repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	if err := FetchOrigin(ctx, f.repo, to(f.origin), nil); err != nil {
		t.Fatal(err)
	}
	if got := head(); got != "refs/remotes/origin/main" {
		t.Errorf("an existing origin/HEAD changed to %q", got)
	}
	git(t, f.repo, "remote", "set-head", "origin", "-d")
	git(t, f.repo, "config", "remote.origin.followRemoteHEAD", "never")
	if err := FetchOrigin(ctx, f.repo, to(f.origin), nil); err != nil {
		t.Fatal(err)
	}
	if got := head(); got != "" {
		t.Errorf("origin/HEAD = %q with followRemoteHEAD=never", got)
	}
}

// pushFromOtherCloneBranch pushes a commit on a new branch main to origin
// (whose default branch is not main).
func (f fixture) pushFromOtherCloneBranch(t *testing.T, name string) {
	t.Helper()
	other := filepath.Join(f.root, "other-"+name)
	git(t, f.root, "clone", "-q", f.origin, other)
	git(t, other, "checkout", "-q", "-b", "main")
	commitFile(t, other, name, name+"\n", "add "+name)
	git(t, other, "push", "-q", "origin", "main")
}

// TestAddWorktreeWithLFS: with git-lfs's filter on, the checkout may
// download; the repo's own git-lfs settings and an origin that is not the
// pinned one are refused, and the user's credential helpers are the only
// ones git-lfs could reach.
func TestAddWorktreeWithLFS(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	global := filepath.Join(t.TempDir(), "gitconfig")
	writeFile(t, global, "[filter \"lfs\"]\n\tclean = git-lfs clean -- %f\n\tsmudge = git-lfs smudge -- %f\n\tprocess = git-lfs filter-process\n\trequired = true\n")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	git(t, f.repo, "config", "lfs.repositoryformatversion", "0")
	base := git(t, f.repo, "rev-parse", "origin/main")
	n := 0
	add := func() error {
		n++
		return AddWorktree(ctx, f.repo, filepath.Join(f.root, fmt.Sprintf("wt%d", n)), fmt.Sprintf("atl/%d", n), base, to(f.origin))
	}
	if err := add(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ key, value string }{
		{"lfs.customtransfer.x.path", "/bin/x"},
		{"lfs.url", "https://elsewhere/"},
		{"http.proxy", "http://elsewhere:1"},
	} {
		git(t, f.repo, "config", c.key, c.value)
		var re *RefusedError
		if err := add(); !errors.As(err, &re) || !strings.EqualFold(re.Key, c.key) {
			t.Errorf("with %s: %v", c.key, err)
		}
		git(t, f.repo, "config", "--unset", c.key)
	}
	git(t, f.repo, "remote", "set-url", "origin", f.decoy(t))
	var re *RefusedError
	if err := add(); !errors.As(err, &re) || re.Key != "remote.origin.url" || !strings.Contains(err.Error(), f.origin) {
		t.Errorf("with origin pointed elsewhere: %v", err)
	}
	git(t, f.repo, "remote", "set-url", "origin", f.origin)
	if err := add(); err != nil {
		t.Error(err)
	}
	// The user's credential helper and askpass reach git-lfs, the repo's
	// do not.
	args := userTransportArgs(ctx, f.repo)
	if !slices.Contains(args, "credential.helper=") || !slices.Contains(args, "core.askPass=") {
		t.Errorf("userTransportArgs = %q", args)
	}
}

func TestPushURLsFollowGit(t *testing.T) {
	user := configEntries{
		{"url.ssh://h/.pushinsteadof", "https://h/"},
		{"url.git@g:.insteadof", "https://g/"},
		{"url.https://g/longer/.insteadof", "https://g/lo"},
	}
	for _, c := range []struct {
		o     Origin
		fetch string
		push  []string
	}{
		{Origin{URLs: []string{"https://h/r"}}, "https://h/r", []string{"ssh://h/r"}},
		{Origin{URLs: []string{"https://g/r"}}, "git@g:r", []string{"git@g:r"}},
		{Origin{URLs: []string{"https://g/lo/r"}}, "https://g/longer//r", []string{"https://g/longer//r"}},
		{Origin{URLs: []string{"https://h/r"}, PushURLs: []string{"https://h/p", "https://g/p"}}, "https://h/r",
			[]string{"https://h/p", "git@g:p"}},
		{Origin{URLs: []string{"https://h/a", "https://g/b"}}, "https://h/a", []string{"ssh://h/a"}},
	} {
		if got := c.o.fetchURLs(user); len(got) != 1 || got[0] != c.fetch {
			t.Errorf("%+v: fetch %q, want %q", c.o, got, c.fetch)
		}
		if got := c.o.pushURLs(user); !slices.Equal(got, c.push) {
			t.Errorf("%+v: push %q, want %q", c.o, got, c.push)
		}
	}
}

func TestGitHubRepo(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	global := filepath.Join(t.TempDir(), "gitconfig")
	writeFile(t, global, "[url \"git@github.com:\"]\n\tinsteadOf = gh:\n")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	for url, want := range map[string]string{
		"git@github.com:o/r.git":          "o/r",
		"https://github.com/o/r":          "o/r",
		"https://github.com/o/r.git/":     "o/r",
		"ssh://git@github.com:22/o/r.git": "o/r",
		"gh:o/r":                          "o/r",
		"git@ghe.example.com:o/r.git":     "ghe.example.com/o/r",
		"https://gitlab.com/g/s/r.git":    "",
		"/srv/r.git":                      "",
		"file:///srv/r.git":               "",
	} {
		got, ok := GitHubRepo(ctx, f.repo, to(url))
		if got != want || ok != (want != "") {
			t.Errorf("GitHubRepo(%s) = %q, %v; want %q", url, got, ok, want)
		}
	}
}

// TestGitHubRepoSSHAlias: an ssh host alias (a second GitHub account's,
// say) whose real host name in the user's ssh config is github.com names a
// github.com repository; ssh is asked with nothing but the host, and only
// for an ssh URL whose host looks like one.
func TestGitHubRepoSSHAlias(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	writeFile(t, filepath.Join(bin, "ssh"), `#!/bin/sh
printf '%s\n' "$*" >> `+log+`
[ "$1" = -G ] && [ "$2" = -- ] || exit 2
case "$3" in
github-work) echo "user git"; echo "hostname github.com" ;;
*) echo "hostname $3" ;;
esac
`)
	if err := os.Chmod(filepath.Join(bin, "ssh"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for url, want := range map[string]string{
		"git@github-work:o/r.git":         "o/r",
		"ssh://git@github-work:22/o/r":    "o/r",
		"git@ghe.example.com:o/r.git":     "ghe.example.com/o/r",
		"https://github-work/o/r.git":     "github-work/o/r", // not ssh: no alias
		"git@-oProxyCommand=x:o/r.git":    "-oProxyCommand=x/o/r",
		"git@github.com:o/r.git":          "o/r",
		"ssh://git@github.com:22/o/r.git": "o/r",
	} {
		got, ok := GitHubRepo(ctx, f.repo, to(url))
		if got != want || !ok {
			t.Errorf("GitHubRepo(%s) = %q, %v; want %q", url, got, ok, want)
		}
	}
	calls, _ := os.ReadFile(log)
	if got, want := string(calls), "-G -- github-work\n-G -- github-work\n-G -- ghe.example.com\n"; !sameLines(got, want) {
		t.Errorf("ssh was run with:\n%s\nwant:\n%s", got, want)
	}
}

// sameLines reports whether a and b have the same lines, in any order.
func sameLines(a, b string) bool {
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

// decoy makes a bare repo that looks like f's origin with one more commit
// on main, so that a fetch from it would show.
func (f fixture) decoy(t *testing.T) string {
	t.Helper()
	decoy := filepath.Join(f.root, "decoy.git")
	git(t, f.root, "clone", "-q", "--bare", f.origin, decoy)
	work := filepath.Join(f.root, "decoy-work")
	git(t, f.root, "clone", "-q", decoy, work)
	commitFile(t, work, "decoy.txt", "decoy\n", "decoy")
	git(t, work, "push", "-q", "origin", "main")
	return decoy
}

// TestTransportGoesToTheGivenOrigin: whatever the repo's config says about
// origin, or about a remote it guesses atl might use, push and fetch go to
// the Origin given, and still keep refs/remotes/origin/* as they did.
func TestTransportGoesToTheGivenOrigin(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	decoy := f.decoy(t)
	decoyRefs := git(t, decoy, "for-each-ref")
	wt, _ := f.addWorktree(t, ctx, "t1")
	commitFile(t, wt, "work.txt", "work\n", "work")
	git(t, f.repo, "remote", "set-url", "origin", decoy)
	git(t, f.repo, "remote", "set-url", "--add", "--push", "origin", decoy)
	for _, name := range []string{"atl", "atl-0000000000000000", "origin-atl"} {
		git(t, f.repo, "config", "remote."+name+".url", decoy)
		git(t, f.repo, "config", "remote."+name+".pushurl", decoy)
	}
	git(t, f.repo, "config", "branch.atl/t1.pushRemote", "atl")
	git(t, f.repo, "config", "remote.pushDefault", "atl")
	origin := to(f.origin)

	var out bytes.Buffer
	if err := PushBranch(ctx, wt, origin, "atl/t1", false, &out); err != nil {
		t.Fatalf("push: %v\n%s", err, out.String())
	}
	head := git(t, wt, "rev-parse", "HEAD")
	if got := git(t, f.origin, "rev-parse", "atl/t1"); got != head {
		t.Errorf("origin atl/t1 = %s, want %s", got, head)
	}
	if got := git(t, wt, "rev-parse", "refs/remotes/origin/atl/t1"); got != head {
		t.Errorf("refs/remotes/origin/atl/t1 = %s after the push, want %s", got, head)
	}
	if got := git(t, decoy, "for-each-ref"); got != decoyRefs {
		t.Errorf("the decoy got refs:\n%s", got)
	}
	if got := git(t, f.repo, "for-each-ref", "--format=%(refname)", "refs/remotes"); strings.Contains(got, "atl-") {
		t.Errorf("refs of the stand-in remote were left:\n%s", got)
	}

	// The lease is refs/remotes/origin/atl/t1: someone else's push to the
	// branch fails a forced push until it is fetched.
	git(t, wt, "-c", "core.hooksPath=/dev/null", "commit", "-q", "--amend", "-m", "work, amended")
	if err := PushBranch(ctx, wt, origin, "atl/t1", false, &out); err == nil {
		t.Error("push of a rewritten branch without force succeeded")
	}
	other := filepath.Join(f.root, "other")
	git(t, f.root, "clone", "-q", "-b", "atl/t1", f.origin, other)
	commitFile(t, other, "other.txt", "other\n", "someone else's")
	git(t, other, "push", "-q", "origin", "atl/t1")
	if err := PushBranch(ctx, wt, origin, "atl/t1", true, &out); err == nil {
		t.Error("forced push over a commit not fetched yet succeeded")
	}
	if err := FetchOrigin(ctx, wt, origin, &out); err != nil {
		t.Fatalf("fetch: %v\n%s", err, out.String())
	}
	if err := PushBranch(ctx, wt, origin, "atl/t1", true, &out); err != nil {
		t.Fatalf("forced push after the fetch: %v\n%s", err, out.String())
	}
	if got, want := git(t, f.origin, "rev-parse", "atl/t1"), git(t, wt, "rev-parse", "HEAD"); got != want {
		t.Errorf("origin atl/t1 = %s after the forced push, want %s", got, want)
	}

	// The fetch came from origin, not the decoy.
	tip := f.pushFromOtherClone(t, "new")
	if err := FetchOrigin(ctx, f.repo, origin, &out); err != nil {
		t.Fatalf("fetch: %v\n%s", err, out.String())
	}
	if got := git(t, f.repo, "rev-parse", "origin/main"); got != tip {
		t.Errorf("origin/main = %s after the fetch, want %s", got, tip)
	}
	if got := git(t, decoy, "for-each-ref"); got != decoyRefs {
		t.Errorf("the decoy changed:\n%s", got)
	}

	// A single-branch clone's refspec is kept.
	git(t, f.repo, "config", "remote.origin.fetch", "+refs/heads/main:refs/remotes/origin/main")
	git(t, f.repo, "update-ref", "-d", "refs/remotes/origin/atl/t1")
	if err := FetchOrigin(ctx, f.repo, origin, &out); err != nil {
		t.Fatalf("fetch: %v\n%s", err, out.String())
	}
	if ok, _ := RefExists(ctx, f.repo, "refs/remotes/origin/atl/t1"); ok {
		t.Error("a single-branch clone fetched another branch")
	}
}

// TestTransportRefusesRepoURLRewrites: url.<base>.insteadOf and
// pushInsteadOf apply to any URL, so the repo's own config must have none;
// the user's own (global) ones stay.
func TestTransportRefusesRepoURLRewrites(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		plant func(t *testing.T, f fixture, wt, decoy string)
		key   string // in the refusal; "" → allowed
	}{
		{"local insteadOf", func(t *testing.T, f fixture, wt, decoy string) {
			git(t, f.repo, "config", "url."+decoy+".insteadOf", f.origin)
		}, "url.DECOY.insteadof"},
		{"local pushInsteadOf", func(t *testing.T, f fixture, wt, decoy string) {
			git(t, f.repo, "config", "url."+decoy+".pushInsteadOf", f.origin)
		}, "url.DECOY.pushinsteadof"},
		{"worktree insteadOf", func(t *testing.T, f fixture, wt, decoy string) {
			git(t, f.repo, "config", "extensions.worktreeConfig", "true")
			git(t, wt, "config", "--worktree", "url."+decoy+".insteadOf", f.origin)
		}, "url.DECOY.insteadof"},
		{"included insteadOf", func(t *testing.T, f fixture, wt, decoy string) {
			inc := filepath.Join(f.root, "inc")
			writeFile(t, inc, "[url \""+decoy+"\"]\n\tinsteadOf = "+f.origin+"\n")
			git(t, f.repo, "config", "include.path", inc)
		}, "include.path"}, // the include itself, before what it brings in
		{"global insteadOf", func(t *testing.T, f fixture, wt, decoy string) {
			global := filepath.Join(f.root, "gitconfig")
			writeFile(t, global, "[url \""+f.origin+"\"]\n\tinsteadOf = elsewhere:\n\tpushInsteadOf = elsewhere:\n")
			t.Setenv("GIT_CONFIG_GLOBAL", global)
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			decoy := f.decoy(t)
			decoyRefs := git(t, decoy, "for-each-ref")
			wt, _ := f.addWorktree(t, ctx, "t1")
			commitFile(t, wt, "work.txt", "work\n", "work")
			c.plant(t, f, wt, decoy)
			key := strings.ReplaceAll(c.key, "DECOY", decoy)
			for op, err := range map[string]error{
				"push":  PushBranch(ctx, wt, to(f.origin), "atl/t1", false, nil),
				"fetch": FetchOrigin(ctx, wt, to(f.origin), nil),
			} {
				if key == "" && err != nil {
					t.Errorf("%s: %v", op, err)
				}
				if key != "" && (err == nil || !strings.Contains(err.Error(), key)) {
					t.Errorf("%s: error %v, want one naming %s", op, err, key)
				}
			}
			if got := git(t, decoy, "for-each-ref"); got != decoyRefs {
				t.Errorf("the decoy got refs:\n%s", got)
			}
		})
	}
}

// TestTransportRefusesRepoHTTPSettings: an https origin's connection (and
// the user's credentials for it) must not go through the repo's own proxy
// or TLS settings; with an ssh origin they do not matter.
func TestTransportRefusesRepoHTTPSettings(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	git(t, f.repo, "config", "http.https://example.invalid/.proxy", "http://127.0.0.1:1")
	https := to("https://example.invalid/o/r.git")
	for op, err := range map[string]error{
		"push":      PushBranch(ctx, f.repo, https, "main", false, nil),
		"fetch":     FetchOrigin(ctx, f.repo, https, nil),
		"ls-remote": func() error { _, err := RemoteBranchExists(ctx, f.repo, https, "main"); return err }(),
	} {
		var re *RefusedError
		if !errors.As(err, &re) || re.Key != "http.https://example.invalid/.proxy" {
			t.Errorf("%s: %v, want a refusal naming the proxy", op, err)
		}
	}
	// A push URL counts too.
	o := Origin{URLs: []string{f.origin}, PushURLs: []string{"https://example.invalid/o/r.git"}}
	var re *RefusedError
	if err := FetchOrigin(ctx, f.repo, o, nil); !errors.As(err, &re) {
		t.Errorf("fetch with an https push URL: %v", err)
	}
	hs, err := Hazards(ctx, f.repo, &Origin{URLs: []string{"git@example.invalid:o/r.git"}})
	if err != nil || len(hs) != 0 {
		t.Errorf("with an ssh origin: %+v, %v", hs, err)
	}
}

// TestTransportRefusesIncludes: http.* and insteadOf settings hidden behind
// an include whose condition is false for the check but true for the
// command (hasconfig:remote.*.url matches the stand-in remote the command
// defines with -c) never reach a push, fetch or remote check: the include
// itself is refused, and nothing connects to the task's proxy.
func TestTransportRefusesIncludes(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	wt, _ := f.addWorktree(t, ctx, "x")
	commitFile(t, wt, "a.txt", "a\n", "task commit")
	pinned := to("https://github.com/example/api.git")

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	connected := make(chan string, 8)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadString('\n')
			connected <- strings.TrimSpace(line)
			c.Close()
		}
	}()

	// What the task writes in its clone's config: origin elsewhere (atl only
	// warns about that and uses the pin), and its settings in a file
	// included only when a remote has the pinned URL.
	git(t, f.repo, "config", "remote.origin.url", "https://example.invalid/whatever.git")
	evil := filepath.Join(f.root, "evil.cfg")
	writeFile(t, evil, "[http]\n\tproxy = http://"+l.Addr().String()+"\n\tsslVerify = false\n"+
		"[url \"https://attacker.example/\"]\n\tinsteadOf = https://github.com/\n")
	key := "includeif.hasconfig:remote.*.url:https://github.com/example/api.git.path"
	git(t, f.repo, "config", key, evil)

	for op, err := range map[string]error{
		"push":      PushBranch(ctx, wt, pinned, "atl/x", false, nil),
		"fetch":     FetchOrigin(ctx, wt, pinned, nil),
		"ls-remote": func() error { _, err := RemoteBranchExists(ctx, wt, pinned, "atl/x"); return err }(),
	} {
		var re *RefusedError
		if !errors.As(err, &re) || re.Key != key || re.Value != evil {
			t.Errorf("%s: %v, want a refusal naming the include", op, err)
		}
	}
	select {
	case line := <-connected:
		t.Errorf("git connected to the task's proxy: %q", line)
	case <-time.After(200 * time.Millisecond):
	}
	hs, err := Hazards(ctx, wt, &pinned)
	if err != nil || len(hs) != 1 || hs[0].Key != key {
		t.Errorf("Hazards (atl doctor) = %+v, %v; want the include", hs, err)
	}
}

// TestTransportRefusesProgramsInURLs: remotes that would run a program here
// (ext::, a git:// proxy, an ssh command) do not.
func TestTransportRefusesProgramsInURLs(t *testing.T) {
	ctx := context.Background()
	for _, url := range []string{
		"ext::sh -c touch% MARKERS/ext",
		"git://127.0.0.1:1/repo.git",
		"ssh://127.0.0.1:1/repo.git",
		"127.0.0.1:repo.git",
	} {
		f := newFixture(t)
		m := newMarkers(t)
		url = strings.ReplaceAll(url, "MARKERS", string(m))
		plantRepoConfig(t, m, f.repo, "")
		if err := FetchOrigin(ctx, f.repo, to(url), nil); err == nil {
			t.Errorf("%s: fetch succeeded", url)
		}
		if err := PushBranch(ctx, f.repo, to(url), "main", false, nil); err == nil {
			t.Errorf("%s: push succeeded", url)
		}
		m.check(t)
	}
}

// TestTransportArgsKeepTheUsersOwnSettings: credential helpers, askpass and
// ssh command come from the user's own config, never the repo's.
func TestTransportArgsKeepTheUsersOwnSettings(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	global := filepath.Join(t.TempDir(), "gitconfig")
	writeFile(t, global, "[credential]\n\thelper = store\n[credential \"https://github.com\"]\n\thelper =\n\thelper = !gh auth git-credential\n"+
		"[core]\n\tsshCommand = ssh -i ~/.ssh/work\n\taskPass = /usr/bin/my-askpass\n")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_SSH_COMMAND", "")
	git(t, f.repo, "config", "credential.helper", "evil")
	git(t, f.repo, "config", "core.sshCommand", "evil")

	args := slices.Concat(safetyArgs[1:], transportArgs(true, false, userConfig(ctx, f.repo)))
	var kv []string
	for i := 0; i+1 < len(args); i += 2 {
		if args[i] != "-c" {
			t.Fatalf("args %q", args)
		}
		kv = append(kv, args[i+1])
	}
	reset := slices.Index(kv, "credential.helper=")
	if reset < 0 {
		t.Fatalf("no credential.helper reset in %q", kv)
	}
	if got, want := kv[reset+1:reset+4], []string{
		"credential.helper=store",
		"credential.https://github.com.helper=",
		"credential.https://github.com.helper=!gh auth git-credential",
	}; !slices.Equal(got, want) {
		t.Errorf("helpers after the reset = %q, want %q", got, want)
	}
	for _, want := range []string{"core.sshCommand=ssh -i ~/.ssh/work", "core.askPass=/usr/bin/my-askpass",
		"core.hooksPath=/dev/null", "protocol.ext.allow=never", "protocol.file.allow=never", "push.pushOption="} {
		if !slices.Contains(kv, want) {
			t.Errorf("args lack %q: %q", want, kv)
		}
	}
	for _, s := range kv {
		if strings.Contains(s, "evil") {
			t.Errorf("the repo's own setting got in: %q", s)
		}
	}

	// $GIT_SSH_COMMAND wins over any config: no core.sshCommand then.
	t.Setenv("GIT_SSH_COMMAND", "ssh -F /dev/null")
	if args := transportArgs(false, true, userConfig(ctx, f.repo)); slices.ContainsFunc(args, func(s string) bool { return strings.HasPrefix(s, "core.sshCommand=") }) ||
		slices.Contains(args, "protocol.file.allow=never") || slices.Contains(args, "push.pushOption=") {
		t.Errorf("fetch args with $GIT_SSH_COMMAND, local origin: %q", args)
	}
}
