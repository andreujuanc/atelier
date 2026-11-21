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

	var out bytes.Buffer
	if err := PushBranch(ctx, wt, "atl/t1", false, &out); err != nil {
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
	err := PushBranch(ctx, wt, "atl/t1", false, &out)
	var ge *Error
	if !errors.As(err, &ge) || ge.ExitCode <= 0 || ge.Stderr == "" || !strings.Contains(err.Error(), "git push") {
		t.Errorf("non-fast-forward push without force: %#v", err)
	}
	if err := PushBranch(ctx, wt, "atl/t1", true, &out); err != nil {
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
	if err := FetchOrigin(ctx, f.repo, &out); err != nil {
		t.Fatalf("fetch: %v\n%s", err, out.String())
	}
	if got := git(t, f.repo, "rev-parse", "origin/main"); got != tip {
		t.Errorf("origin/main = %s after the fetch, want %s", got, tip)
	}
	m.check(t)
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
		git(t, f.repo, "remote", "set-url", "origin", url)
		if err := FetchOrigin(ctx, f.repo, nil); err == nil {
			t.Errorf("%s: fetch succeeded", url)
		}
		if err := PushBranch(ctx, f.repo, "main", false, nil); err == nil {
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
		"core.hooksPath=/dev/null", "protocol.ext.allow=never", "protocol.file.allow=never", "remote.origin.mirror=false"} {
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
		slices.Contains(args, "protocol.file.allow=never") || slices.Contains(args, "remote.origin.mirror=false") {
		t.Errorf("fetch args with $GIT_SSH_COMMAND, local origin: %q", args)
	}
}
