package gitx

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// secretRepo is another repo of the user's on the host, which no task sees:
// its last commit's subject has "private" in it, its file "hunter2".
func secretRepo(t *testing.T, f fixture) string {
	t.Helper()
	secret := filepath.Join(t.TempDir(), "secret")
	git(t, f.root, "init", "-q", "-b", "main", secret)
	commitFile(t, secret, "creds", "hunter2\n", "the user's private work")
	return secret
}

// TestHostGitReadsOnlyTheRepo: a task writes its clone's git dir. Listing
// another host repo's objects as alternates (or linking its object
// directories in), or pointing the clone's git dir at another repo's
// (commondir), would have atl's git on the host read that repo: atl show
// print its last commit (HEAD a symlink to its branch), atl diff its files,
// atl push send its objects to origin (given a commit id), atl fetch write
// that repo's refs. Each is refused; HEAD and the branch it names are read
// without following a symlink.
func TestHostGitReadsOnlyTheRepo(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	wt, base := f.addWorktree(t, ctx, "x")
	commitFile(t, wt, "work.txt", "work\n", "the task's work")
	head := git(t, wt, "rev-parse", "HEAD")
	secret := secretRepo(t, f)
	secretMain := git(t, secret, "rev-parse", "main")
	gitDir := filepath.Join(f.repo, ".git")
	regHEAD := filepath.Join(git(t, wt, "rev-parse", "--absolute-git-dir"), "HEAD")
	alternates := filepath.Join(gitDir, "objects", "info", "alternates")

	if sha, subject, _, err := LastCommit(ctx, wt); err != nil || sha != head || subject != "the task's work" {
		t.Fatalf("last commit of a plain worktree = %s %q, %v", sha, subject, err)
	}
	leaks := func(name string) {
		t.Helper()
		if sha, subject, _, err := LastCommit(ctx, wt); err == nil || sha == secretMain || strings.Contains(subject, "private") {
			t.Errorf("%s: last commit = %s %q, %v; want refused", name, sha, subject, err)
		}
	}

	// HEAD a symlink to the other repo's branch, whose objects are
	// alternates.
	writeFile(t, alternates, filepath.Join(secret, ".git", "objects")+"\n")
	must(t, os.Rename(regHEAD, regHEAD+".saved"))
	symlinkHEAD(t, regHEAD, filepath.Join(secret, ".git", "refs", "heads", "main"))
	leaks("symlinked HEAD, alternates")
	var out bytes.Buffer
	if err := Diff(ctx, wt, base, false, &out); err == nil || strings.Contains(out.String(), "hunter2") {
		t.Errorf("diff with alternates: %v\n%s", err, out.String())
	}
	git(t, f.repo, "update-ref", "refs/heads/atl/x", secretMain) // a commit id the task knows
	if err := PushBranch(ctx, f.repo, to(f.origin), "atl/x", true, io.Discard); err == nil {
		t.Errorf("push with alternates: no error")
	}
	if out := testGit(t, f.origin, "log", "--format=%s", "atl/x"); strings.Contains(out, "private") {
		t.Errorf("push with alternates sent the other repo's history: %s", out)
	}
	git(t, f.repo, "update-ref", "refs/heads/atl/x", head)

	// HEAD a symlink to a branch of the repo's own (still not followed);
	// the branch HEAD names a symlink.
	must(t, os.Remove(alternates))
	must(t, os.Remove(regHEAD))
	symlinkHEAD(t, regHEAD, filepath.Join(gitDir, "refs", "heads", "atl", "x"))
	leaks("symlinked HEAD")
	must(t, os.Remove(regHEAD))
	must(t, os.Rename(regHEAD+".saved", regHEAD))
	branch := filepath.Join(gitDir, "refs", "heads", "atl", "x")
	must(t, os.Rename(branch, branch+".saved"))
	writeFile(t, filepath.Join(f.root, "elsewhere"), head+"\n")
	must(t, os.Symlink(filepath.Join(f.root, "elsewhere"), branch))
	leaks("symlinked branch")
	must(t, os.Remove(branch))
	must(t, os.Rename(branch+".saved", branch))

	// The other repo's packs linked in, HEAD detached at its commit.
	git(t, secret, "gc", "-q")
	writeFile(t, regHEAD+".detached", secretMain+"\n")
	must(t, os.Rename(regHEAD, regHEAD+".saved"))
	must(t, os.Rename(regHEAD+".detached", regHEAD))
	pack := filepath.Join(gitDir, "objects", "pack")
	must(t, os.Rename(pack, pack+".saved"))
	must(t, os.Symlink(filepath.Join(secret, ".git", "objects", "pack"), pack))
	leaks("linked packs")
	must(t, os.Remove(pack))
	must(t, os.Rename(pack+".saved", pack))
	must(t, os.Rename(regHEAD+".saved", regHEAD))

	// The clone's git dir takes its refs and objects from the other repo's.
	writeFile(t, filepath.Join(gitDir, "commondir"), filepath.Join(secret, ".git")+"\n")
	if err := FetchOrigin(ctx, f.repo, to(f.origin), io.Discard); err == nil {
		t.Errorf("fetch with a commondir: no error")
	}
	if out := testGit(t, secret, "for-each-ref"); strings.Contains(out, "refs/remotes") {
		t.Errorf("fetch with a commondir wrote the other repo's refs:\n%s", out)
	}
	must(t, os.Remove(filepath.Join(gitDir, "commondir")))

	if sha, _, _, err := LastCommit(ctx, wt); err != nil || sha != head {
		t.Errorf("last commit once restored = %s, %v", sha, err)
	}
}

// testGit runs git in dir as a user would; a failure is not fatal.
func testGit(t *testing.T, dir string, args ...string) string {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = testEnv(t)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// symlinkHEAD makes head a symlink to target that git takes for a valid
// HEAD: one into refs/ (a directory made next to it), out again with "..".
func symlinkHEAD(t *testing.T, head, target string) {
	t.Helper()
	dir := filepath.Dir(head)
	must(t, os.MkdirAll(filepath.Join(dir, "refs"), 0o755))
	rel, err := filepath.Rel(dir, target)
	must(t, err)
	must(t, os.Symlink("refs/../"+rel, head))
}

// TestTransportWritesOnlyTheRepo: a task writes its clone's git dir. A
// directory there that git writes into for a fetch or push (refs/remotes/
// origin and below, their reflogs in logs/) made a symlink to one of the
// user's directories had atl's git on the host create or replace a file
// named after a branch there, and FETCH_HEAD a symlink to a file of the
// user's had it overwritten. Nothing outside the git dir is written, and
// the refs git did not write there are not written at all.
func TestTransportWritesOnlyTheRepo(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	wt, base := f.addWorktree(t, ctx, "x")
	commitFile(t, wt, "work.txt", "work\n", "the task's work")
	f.pushFromOtherClone(t, "new")
	gitDir := filepath.Join(f.repo, ".git")
	// The user's directory: another repo's refs, say.
	victim := t.TempDir()
	content := map[string]string{"main": base + "\n", "x": base + "\n", "HEAD": "ref: refs/heads/main\n"}
	precious := filepath.Join(t.TempDir(), "precious")
	writeFile(t, precious, "the user's\n")
	for name, c := range content {
		writeFile(t, filepath.Join(victim, name), c)
	}
	untouched := func(name string) {
		t.Helper()
		entries, _ := os.ReadDir(victim)
		if len(entries) != len(content) {
			t.Errorf("%s: the user's directory has %d entries, want %d", name, len(entries), len(content))
		}
		if data, _ := os.ReadFile(precious); string(data) != "the user's\n" {
			t.Errorf("%s: the user's file now has %q", name, data)
		}
		for n, c := range content {
			if data, _ := os.ReadFile(filepath.Join(victim, n)); string(data) != c {
				t.Errorf("%s: the user's file %s now has %q", name, n, data)
			}
		}
	}
	swap := func(rel, target string) (restore func()) {
		p := filepath.Join(gitDir, rel)
		must(t, os.MkdirAll(p, 0o755))
		must(t, os.Rename(p, p+".saved"))
		must(t, os.Symlink(target, p))
		return func() {
			must(t, os.Remove(p))
			must(t, os.Rename(p+".saved", p))
		}
	}

	restore := swap("refs/remotes/origin", victim)
	FetchOrigin(ctx, f.repo, to(f.origin), io.Discard)
	untouched("fetch, refs/remotes/origin a symlink")
	restore()

	restore = swap("logs/refs/remotes/origin", victim)
	FetchOrigin(ctx, f.repo, to(f.origin), io.Discard)
	untouched("fetch, its reflogs' dir a symlink")
	restore()

	os.Remove(filepath.Join(gitDir, "FETCH_HEAD"))
	must(t, os.Symlink(precious, filepath.Join(gitDir, "FETCH_HEAD")))
	if err := FetchOrigin(ctx, f.repo, to(f.origin), io.Discard); err != nil {
		t.Errorf("fetch: %v", err)
	}
	untouched("fetch, FETCH_HEAD a symlink")
	must(t, os.Remove(filepath.Join(gitDir, "FETCH_HEAD")))

	restore = swap("refs/remotes/origin/atl", victim)
	PushBranch(ctx, wt, to(f.origin), "atl/x", false, io.Discard)
	untouched("push, refs/remotes/origin/atl a symlink")
	restore()

	restore = swap("logs/refs/remotes/origin", victim)
	commitFile(t, wt, "more.txt", "more\n", "more work")
	PushBranch(ctx, wt, to(f.origin), "atl/x", false, io.Discard)
	untouched("push, its reflogs' dir a symlink")
	restore()

	// Without symlinks, the refs are where they belong.
	if err := FetchOrigin(ctx, f.repo, to(f.origin), io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, want := git(t, f.repo, "rev-parse", "refs/remotes/origin/main"), git(t, f.origin, "rev-parse", "main"); got != want {
		t.Errorf("origin/main = %s after the fetch, want %s", got, want)
	}
	if err := PushBranch(ctx, wt, to(f.origin), "atl/x", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, want := git(t, f.repo, "rev-parse", "refs/remotes/origin/atl/x"), git(t, wt, "rev-parse", "HEAD"); got != want {
		t.Errorf("origin/atl/x = %s after the push, want %s", got, want)
	}
}

// TestTransportKeepsTheUsersConditionalConfig: git runs a push or fetch in
// a git dir of atl's own, yet the user's config applies as for the repo,
// conditional includes too (a work identity's rules for repos under a
// directory, say).
func TestTransportKeepsTheUsersConditionalConfig(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	wt, _ := f.addWorktree(t, ctx, "x")
	commitFile(t, wt, "work.txt", "work\n", "the task's work")
	tip := f.pushFromOtherClone(t, "new")
	work := filepath.Join(t.TempDir(), "work.gitconfig")
	writeFile(t, work, "[url \""+f.origin+"\"]\n\tinsteadOf = work:\n")
	global := filepath.Join(t.TempDir(), "gitconfig")
	writeFile(t, global, "[includeIf \"gitdir:"+f.root+"/\"]\n\tpath = "+work+"\n"+
		"[includeIf \"gitdir:/nowhere/\"]\n\tpath = /nowhere/config\n")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	o := Origin{URLs: []string{"work:"}}
	if err := FetchOrigin(ctx, f.repo, o, io.Discard); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := git(t, f.repo, "rev-parse", "origin/main"); got != tip {
		t.Errorf("origin/main = %s, want %s", got, tip)
	}
	if err := PushBranch(ctx, wt, o, "atl/x", false, io.Discard); err != nil {
		t.Fatalf("push: %v", err)
	}
	if got, want := git(t, f.origin, "rev-parse", "atl/x"), git(t, wt, "rev-parse", "HEAD"); got != want {
		t.Errorf("origin atl/x = %s, want %s", got, want)
	}
}
