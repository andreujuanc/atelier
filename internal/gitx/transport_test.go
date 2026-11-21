package gitx

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
func to(url string) Origin { return Origin{URL: url, PushURLs: []string{url}} }

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
	// The user's rules are applied, and the usual https → ssh rule leaves
	// the URL it made as it is when git applies it again.
	global := filepath.Join(t.TempDir(), "gitconfig")
	writeFile(t, global, "[url \"git@github.com:\"]\n\tinsteadOf = https://github.com/\n")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	git(t, f.repo, "remote", "set-url", "origin", "https://github.com/o/r.git")
	git(t, f.repo, "remote", "set-url", "--add", "--push", "origin", "/srv/a.git")
	git(t, f.repo, "remote", "set-url", "--add", "--push", "origin", "/srv/b.git")
	want := Origin{URL: "git@github.com:o/r.git", PushURLs: []string{"/srv/a.git", "/srv/b.git"}}
	if o, ok, err := CurrentOrigin(ctx, f.repo); err != nil || !ok || !o.Equal(want) {
		t.Errorf("CurrentOrigin = %+v, %v, %v; want %+v", o, ok, err, want)
	}
	if got := git(t, f.repo, "ls-remote", "--get-url", want.URL); got != want.URL {
		t.Errorf("the rule applied again: %s → %s", want.URL, got)
	}
	git(t, f.repo, "remote", "remove", "origin")
	if o, ok, err := CurrentOrigin(ctx, f.repo); err != nil || ok {
		t.Errorf("CurrentOrigin without origin = %+v, %v, %v", o, ok, err)
	}
	if _, _, err := CurrentOrigin(ctx, t.TempDir()); err == nil {
		t.Error("CurrentOrigin outside a repo: no error")
	}
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
		}, "url.DECOY.insteadof"},
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

	args := transportArgs(ctx, f.repo, true, false)
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
	if args := transportArgs(ctx, f.repo, false, true); slices.ContainsFunc(args, func(s string) bool { return strings.HasPrefix(s, "core.sshCommand=") }) ||
		slices.Contains(args, "protocol.file.allow=never") || slices.Contains(args, "push.pushOption=") {
		t.Errorf("fetch args with $GIT_SSH_COMMAND, local origin: %q", args)
	}
}
