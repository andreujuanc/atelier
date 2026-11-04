package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// TestMain isolates git from the user's global/system config and supplies an
// identity so commits work without any global setup.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gitx-test-home")
	if err != nil {
		panic(err)
	}
	cfg := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(cfg, nil, 0o644); err != nil {
		panic(err)
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL":   cfg,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME":     "Test",
		"GIT_AUTHOR_EMAIL":    "test@example.com",
		"GIT_COMMITTER_NAME":  "Test",
		"GIT_COMMITTER_EMAIL": "test@example.com",
	} {
		os.Setenv(k, v)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

var (
	localVarsOnce sync.Once
	localVars     []string
)

// gitLocalEnvVars returns the installed git's `rev-parse --local-env-vars`.
func gitLocalEnvVars(t *testing.T) []string {
	t.Helper()
	localVarsOnce.Do(func() {
		out, err := exec.Command("git", "rev-parse", "--local-env-vars").Output()
		if err != nil {
			panic(fmt.Sprintf("git rev-parse --local-env-vars: %v", err))
		}
		localVars = strings.Fields(string(out))
	})
	return localVars
}

// testEnv is os.Environ() minus repo-local GIT_* variables (so fixtures work
// when tests run inside a git hook). Independent of the package's list.
func testEnv(t *testing.T) []string {
	t.Helper()
	drop := map[string]bool{}
	for _, v := range gitLocalEnvVars(t) {
		drop[v] = true
	}
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if drop[name] || strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "LC_ALL=C")
}

// git runs plain git in dir (test helper, independent of the package code).
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = testEnv(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitFile(t *testing.T, dir, name, content, msg string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, name), content)
	git(t, dir, "add", name)
	git(t, dir, "commit", "-q", "-m", msg)
}

type fixture struct {
	root   string
	origin string // bare repo
	repo   string // canonical clone
}

// newFixture builds a bare "origin" with one commit on branch and a clone of it.
func newFixtureBranch(t *testing.T, branch string) fixture {
	t.Helper()
	root := t.TempDir()
	f := fixture{root: root, origin: filepath.Join(root, "origin.git"), repo: filepath.Join(root, "repo")}
	git(t, root, "init", "-q", "--bare", "-b", branch, f.origin)
	seed := filepath.Join(root, "seed")
	git(t, root, "init", "-q", "-b", branch, seed)
	commitFile(t, seed, "README.md", "hello\n", "initial")
	git(t, seed, "remote", "add", "origin", f.origin)
	git(t, seed, "push", "-q", "origin", branch)
	git(t, root, "clone", "-q", f.origin, f.repo)
	return f
}

func newFixture(t *testing.T) fixture { return newFixtureBranch(t, "main") }

// pushFromOtherClone pushes a new commit to origin/main from a separate clone.
func (f fixture) pushFromOtherClone(t *testing.T, name string) string {
	t.Helper()
	other := filepath.Join(f.root, "other-"+name)
	git(t, f.root, "clone", "-q", f.origin, other)
	commitFile(t, other, name, name+"\n", "add "+name)
	git(t, other, "push", "-q", "origin", "main")
	return git(t, other, "rev-parse", "HEAD")
}

func (f fixture) addWorktree(t *testing.T, ctx context.Context, name string) (wt, base string) {
	t.Helper()
	base, err := RevParse(ctx, f.repo, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	wt = filepath.Join(f.root, "wt-"+name)
	if err := AddWorktree(ctx, f.repo, wt, "atl/"+name, base); err != nil {
		t.Fatal(err)
	}
	return wt, base
}

func TestIsRepo(t *testing.T) {
	f := newFixture(t)
	plain := filepath.Join(f.root, "plain")
	if err := os.Mkdir(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(f.repo, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"clone", f.repo, true},
		{"bare", f.origin, true},
		{"plain", plain, false},
		{"subdir of repo", sub, false},
		{"subdir of bare", filepath.Join(f.origin, "refs"), false},
		{"missing", filepath.Join(f.root, "nope"), false},
		{"file", filepath.Join(f.repo, "README.md"), false},
	}
	for _, c := range cases {
		if got := IsRepo(c.path); got != c.want {
			t.Errorf("IsRepo(%s) = %v, want %v", c.name, got, c.want)
		}
	}

	// Symlink to a repo root still counts.
	link := filepath.Join(f.root, "link")
	if err := os.Symlink(f.repo, link); err == nil {
		if !IsRepo(link) {
			t.Errorf("IsRepo(symlink to repo) = false")
		}
	}
}

func TestDefaultBranch(t *testing.T) {
	ctx := context.Background()

	t.Run("origin HEAD", func(t *testing.T) {
		f := newFixture(t)
		got, err := DefaultBranch(ctx, f.repo)
		if err != nil || got != "main" {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("origin HEAD non-standard", func(t *testing.T) {
		f := newFixtureBranch(t, "trunk")
		got, err := DefaultBranch(ctx, f.repo)
		if err != nil || got != "trunk" {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("fallback main", func(t *testing.T) {
		f := newFixture(t)
		git(t, f.repo, "remote", "set-head", "origin", "-d")
		got, err := DefaultBranch(ctx, f.repo)
		if err != nil || got != "main" {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("fallback master", func(t *testing.T) {
		f := newFixtureBranch(t, "master")
		git(t, f.repo, "remote", "set-head", "origin", "-d")
		got, err := DefaultBranch(ctx, f.repo)
		if err != nil || got != "master" {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("undeterminable", func(t *testing.T) {
		f := newFixtureBranch(t, "trunk")
		git(t, f.repo, "remote", "set-head", "origin", "-d")
		if _, err := DefaultBranch(ctx, f.repo); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("dangling origin HEAD", func(t *testing.T) {
		f := newFixture(t)
		// As after a master→main rename upstream + fetch --prune.
		git(t, f.repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/master")
		got, err := DefaultBranch(ctx, f.repo)
		if err != nil || got != "main" {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("local branch named origin/main", func(t *testing.T) {
		f := newFixture(t)
		git(t, f.repo, "branch", "origin/main")
		got, err := DefaultBranch(ctx, f.repo)
		if err != nil || got != "main" {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("local branch origin/main does not fake fallback", func(t *testing.T) {
		f := newFixtureBranch(t, "trunk")
		git(t, f.repo, "remote", "set-head", "origin", "-d")
		git(t, f.repo, "branch", "origin/main")
		if got, err := DefaultBranch(ctx, f.repo); err == nil {
			t.Fatalf("expected error, got %q", got)
		}
	})

	t.Run("no origin", func(t *testing.T) {
		dir := t.TempDir()
		git(t, dir, "init", "-q", "-b", "main")
		_, err := DefaultBranch(ctx, dir)
		if err == nil || !strings.Contains(err.Error(), "no origin remote") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestFetchAndRevParse(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	before, err := RevParse(ctx, f.repo, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 40 {
		t.Fatalf("sha %q not full", before)
	}
	pushed := f.pushFromOtherClone(t, "new.txt")
	if got, _ := RevParse(ctx, f.repo, "origin/main"); got != before {
		t.Fatalf("origin/main moved before fetch")
	}
	if err := Fetch(ctx, f.repo); err != nil {
		t.Fatal(err)
	}
	after, err := RevParse(ctx, f.repo, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if after != pushed || after == before {
		t.Fatalf("after fetch origin/main = %s, want %s", after, pushed)
	}

	if _, err := RevParse(ctx, f.repo, "does-not-exist"); err == nil {
		t.Fatal("expected error for bad rev")
	} else if !strings.Contains(err.Error(), "rev-parse") {
		t.Fatalf("error lacks subcommand: %v", err)
	}
}

func TestFetchNoOrigin(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	err := Fetch(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "git fetch:") || !strings.Contains(err.Error(), "origin") {
		t.Fatalf("err = %v", err)
	}
}

func TestAddWorktree(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	wt, base := f.addWorktree(t, ctx, "x")
	if !IsRepo(wt) {
		t.Fatal("worktree is not a repo root")
	}
	br, err := CurrentBranch(ctx, wt)
	if err != nil || br != "atl/x" {
		t.Fatalf("branch = %q, %v", br, err)
	}
	head, err := HeadCommit(ctx, wt)
	if err != nil || head != base {
		t.Fatalf("head = %q, %v; want %s", head, err, base)
	}
	if got := git(t, f.repo, "rev-parse", "atl/x"); got != base {
		t.Fatalf("atl/x = %s, want %s", got, base)
	}

	err = AddWorktree(ctx, f.repo, filepath.Join(f.root, "wt-dup"), "atl/x", base)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate branch err = %v", err)
	}
}

func TestWorktreePathWithSpaces(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	base, err := RevParse(ctx, f.repo, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(f.root, "dir with spaces", "wt one")
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := AddWorktree(ctx, f.repo, wt, "atl/sp", base); err != nil {
		t.Fatal(err)
	}
	if !IsRepo(wt) {
		t.Fatal("worktree with spaces is not a repo root")
	}
	if dirty, err := IsDirty(ctx, wt); err != nil || dirty {
		t.Fatalf("IsDirty = %v, %v", dirty, err)
	}
	if err := RemoveWorktree(ctx, f.repo, wt, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree still exists: %v", err)
	}
}

func TestWorktreeRelativePath(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	base, err := RevParse(ctx, f.repo, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(f.root)
	if err := AddWorktree(ctx, f.repo, "rel-wt", "atl/rel", base); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(f.root, "rel-wt")
	if !IsRepo(abs) {
		t.Fatal("relative worktree not created relative to cwd")
	}
	if err := RemoveWorktree(ctx, f.repo, "rel-wt", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Fatalf("worktree still exists: %v", err)
	}
}

func TestAddWorktreeFromDirtyRepo(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	writeFile(t, filepath.Join(f.repo, "README.md"), "local edit\n")
	writeFile(t, filepath.Join(f.repo, "scratch.txt"), "untracked\n")
	if dirty, err := IsDirty(ctx, f.repo); err != nil || !dirty {
		t.Fatalf("base repo IsDirty = %v, %v", dirty, err)
	}
	wt, _ := f.addWorktree(t, ctx, "fromdirty")
	if dirty, err := IsDirty(ctx, wt); err != nil || dirty {
		t.Fatalf("new worktree IsDirty = %v, %v", dirty, err)
	}
	if _, err := os.Stat(filepath.Join(wt, "scratch.txt")); !os.IsNotExist(err) {
		t.Fatalf("untracked file leaked into worktree: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(wt, "README.md"))
	if err != nil || string(b) != "hello\n" {
		t.Fatalf("README in worktree = %q, %v", b, err)
	}
}

func TestIsDirty(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	wt, _ := f.addWorktree(t, ctx, "d")
	check := func(want bool) {
		t.Helper()
		got, err := IsDirty(ctx, wt)
		if err != nil || got != want {
			t.Fatalf("IsDirty = %v, %v; want %v", got, err, want)
		}
	}
	check(false)
	writeFile(t, filepath.Join(wt, "README.md"), "changed\n")
	check(true)
	git(t, wt, "checkout", "--", "README.md")
	check(false)
	writeFile(t, filepath.Join(wt, "untracked.txt"), "x\n")
	check(true)

	// User config hiding untracked files must not hide them from us.
	git(t, wt, "config", "status.showUntrackedFiles", "no")
	check(true)
}

func TestIsDirtyNoIndexLock(t *testing.T) {
	found := false
	for _, kv := range gitEnv() {
		found = found || kv == "GIT_OPTIONAL_LOCKS=0"
	}
	if !found {
		t.Error("gitEnv lacks GIT_OPTIONAL_LOCKS=0")
	}
	// Polling status while another git holds index.lock must succeed.
	ctx := context.Background()
	f := newFixture(t)
	// Make the stat info stale so a locking `git status` would refresh the index.
	readme := filepath.Join(f.repo, "README.md")
	future := mustStat(t, readme).ModTime().Add(10e9)
	if err := os.Chtimes(readme, future, future); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(f.repo, ".git", "index.lock")
	writeFile(t, lock, "")
	defer os.Remove(lock)
	if dirty, err := IsDirty(ctx, f.repo); err != nil || dirty {
		t.Fatalf("IsDirty with index.lock held = %v, %v", dirty, err)
	}
	if b, err := os.ReadFile(lock); err != nil || len(b) != 0 {
		t.Fatalf("index.lock touched: %q, %v", b, err)
	}
}

func mustStat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

func TestCommitsAhead(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	wt, base := f.addWorktree(t, ctx, "c")
	n, err := CommitsAhead(ctx, wt, base)
	if err != nil || n != 0 {
		t.Fatalf("n = %d, %v", n, err)
	}
	commitFile(t, wt, "a.txt", "a\n", "a")
	commitFile(t, wt, "b.txt", "b\n", "b")
	n, err = CommitsAhead(ctx, wt, base)
	if err != nil || n != 2 {
		t.Fatalf("n = %d, %v", n, err)
	}
}

func TestDiff(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	wt, base := f.addWorktree(t, ctx, "diff")
	commitFile(t, wt, "committed.txt", "committed-line\n", "c")
	writeFile(t, filepath.Join(wt, "README.md"), "hello\nuncommitted-line\n")

	var buf bytes.Buffer
	if err := Diff(ctx, wt, base, false, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"committed.txt", "+committed-line", "README.md", "+uncommitted-line"} {
		if !strings.Contains(out, want) {
			t.Errorf("diff missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("no-color diff contains ANSI escapes")
	}

	buf.Reset()
	if err := Diff(ctx, wt, base, true, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "\x1b[") {
		t.Errorf("color diff lacks ANSI escapes")
	}

	if err := Diff(ctx, wt, "nonexistent-rev", false, &bytes.Buffer{}); err == nil {
		t.Error("expected error for bad base")
	}
}

// limitWriter accepts n bytes, then fails every write with err.
type limitWriter struct {
	n   int
	err error
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, l.err
	}
	if len(p) > l.n {
		k := l.n
		l.n = 0
		return k, l.err
	}
	l.n -= len(p)
	return len(p), nil
}

func TestDiffReaderGoesAway(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	wt, base := f.addWorktree(t, ctx, "pipe")
	// A diff far larger than a pipe buffer, so git is still writing.
	var big strings.Builder
	for i := 0; i < 200000; i++ {
		fmt.Fprintf(&big, "line %d of a large file\n", i)
	}
	commitFile(t, wt, "big.txt", big.String(), "big")

	t.Run("EPIPE writer", func(t *testing.T) {
		if err := Diff(ctx, wt, base, false, &limitWriter{n: 1000, err: syscall.EPIPE}); err != nil {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("closed pipe", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		go func() {
			io.CopyN(io.Discard, r, 1000)
			r.Close()
		}()
		if err := Diff(ctx, wt, base, false, w); err != nil {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("other writer error", func(t *testing.T) {
		boom := errors.New("boom")
		err := Diff(ctx, wt, base, false, &limitWriter{n: 1000, err: boom})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
	})
}

func TestRemoteBranchExists(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	wt, _ := f.addWorktree(t, ctx, "x")
	ok, err := RemoteBranchExists(ctx, wt, "atl/x")
	if err != nil || ok {
		t.Fatalf("before push: %v, %v", ok, err)
	}
	git(t, wt, "push", "-q", "origin", "atl/x")
	ok, err = RemoteBranchExists(ctx, wt, "atl/x")
	if err != nil || !ok {
		t.Fatalf("after push: %v, %v", ok, err)
	}
	// A branch whose name is a suffix of an existing one must not match.
	ok, err = RemoteBranchExists(ctx, wt, "x")
	if err != nil || ok {
		t.Fatalf("suffix match: %v, %v", ok, err)
	}

	// No origin → real error, not false.
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	if _, err := RemoteBranchExists(ctx, dir, "atl/x"); err == nil {
		t.Fatal("expected error without origin")
	}
}

func TestRemoveWorktree(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	listed := func(wt string) bool {
		t.Helper()
		resolved, _ := filepath.EvalSymlinks(filepath.Dir(wt))
		out := git(t, f.repo, "worktree", "list", "--porcelain")
		return strings.Contains(out, wt) || strings.Contains(out, filepath.Join(resolved, filepath.Base(wt)))
	}

	t.Run("clean", func(t *testing.T) {
		wt, _ := f.addWorktree(t, ctx, "clean")
		if !listed(wt) {
			t.Fatal("worktree not listed after add")
		}
		if err := RemoveWorktree(ctx, f.repo, wt, false); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(wt); !os.IsNotExist(err) {
			t.Fatalf("worktree dir still exists: %v", err)
		}
		if listed(wt) {
			t.Fatal("worktree still listed")
		}
		// Branch is kept.
		git(t, f.repo, "rev-parse", "--verify", "atl/clean")
	})

	t.Run("dirty", func(t *testing.T) {
		wt, _ := f.addWorktree(t, ctx, "dirty")
		writeFile(t, filepath.Join(wt, "untracked.txt"), "x\n")
		err := RemoveWorktree(ctx, f.repo, wt, false)
		if err == nil || !strings.Contains(err.Error(), "worktree") {
			t.Fatalf("expected error removing dirty worktree, got %v", err)
		}
		if !listed(wt) {
			t.Fatal("dirty worktree removed without force")
		}
		if err := RemoveWorktree(ctx, f.repo, wt, true); err != nil {
			t.Fatal(err)
		}
		if listed(wt) {
			t.Fatal("worktree still listed after force")
		}
	})

	t.Run("already deleted", func(t *testing.T) {
		wt, _ := f.addWorktree(t, ctx, "gone")
		if err := os.RemoveAll(wt); err != nil {
			t.Fatal(err)
		}
		if err := RemoveWorktree(ctx, f.repo, wt, false); err != nil {
			t.Fatal(err)
		}
		if listed(wt) {
			t.Fatal("deleted worktree still listed after prune")
		}
	})
}

func TestCurrentBranchAndHead(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	br, err := CurrentBranch(ctx, f.repo)
	if err != nil || br != "main" {
		t.Fatalf("branch = %q, %v", br, err)
	}
	head, err := HeadCommit(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if want := git(t, f.repo, "rev-parse", "HEAD"); head != want {
		t.Fatalf("head = %s, want %s", head, want)
	}

	git(t, f.repo, "checkout", "-q", "--detach", "HEAD")
	br, err = CurrentBranch(ctx, f.repo)
	if err != nil || br != "" {
		t.Fatalf("detached branch = %q, %v", br, err)
	}
	if h, err := HeadCommit(ctx, f.repo); err != nil || h != head {
		t.Fatalf("detached head = %q, %v", h, err)
	}

	if _, err := CurrentBranch(ctx, filepath.Join(f.root, "nope")); err == nil {
		t.Fatal("expected error for missing repo")
	}
}

func TestEnvIsolation(t *testing.T) {
	// Repo-local GIT_* vars in the environment (e.g. inside a git hook) must
	// not redirect us.
	ctx := context.Background()
	f := newFixture(t)
	other := newFixture(t)
	commitFile(t, other.repo, "extra.txt", "extra\n", "extra") // make HEADs differ
	writeFile(t, filepath.Join(other.repo, "untracked.txt"), "x\n")
	want := git(t, f.repo, "rev-parse", "HEAD")

	t.Setenv("GIT_DIR", filepath.Join(other.repo, ".git"))
	t.Setenv("GIT_WORK_TREE", other.repo)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "bogus-index"))
	t.Setenv("GIT_CONFIG_COUNT", "1") // without KEY_0 git would die
	if !IsRepo(f.repo) {
		t.Fatal("IsRepo affected by env")
	}
	got, err := HeadCommit(ctx, f.repo)
	if err != nil || got != want {
		t.Fatalf("HeadCommit = %q, %v; want %s", got, err, want)
	}
	if dirty, err := IsDirty(ctx, f.repo); err != nil || dirty {
		t.Fatalf("IsDirty = %v, %v (env redirected status)", dirty, err)
	}
}

func TestLocalEnvVarsMatchGit(t *testing.T) {
	for _, v := range gitLocalEnvVars(t) {
		if !stripEnv(v + "=x") {
			t.Errorf("%s (from git rev-parse --local-env-vars) is not stripped", v)
		}
	}
	for _, kv := range []string{"GIT_CONFIG_KEY_0=a", "GIT_CONFIG_VALUE_12=b"} {
		if !stripEnv(kv) {
			t.Errorf("%s not stripped", kv)
		}
	}
	for _, kv := range []string{"GIT_CONFIG_GLOBAL=x", "GIT_AUTHOR_NAME=x", "HOME=/h", "GIT_DIRX=1"} {
		if stripEnv(kv) {
			t.Errorf("%s stripped", kv)
		}
	}
}

func TestContextCancelled(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := HeadCommit(ctx, f.repo)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("HeadCommit err = %v, want context.Canceled", err)
	}
	var ge *Error
	if !errors.As(err, &ge) {
		t.Fatalf("err %T is not *Error", err)
	}
	if err := Diff(ctx, f.repo, "HEAD", false, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("Diff err = %v, want context.Canceled", err)
	}
}

func TestDubiousOwnership(t *testing.T) {
	// GIT_TEST_ASSUME_DIFFERENT_OWNER makes git treat the repo as foreign.
	ctx := context.Background()
	f := newFixture(t)
	t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")
	if IsRepo(f.repo) {
		t.Skip("installed git ignores GIT_TEST_ASSUME_DIFFERENT_OWNER")
	}
	if _, err := RevParse(ctx, f.repo, "HEAD"); err == nil || !strings.Contains(err.Error(), "dubious ownership") {
		t.Errorf("RevParse err = %v", err)
	}
	if _, err := CurrentBranch(ctx, f.repo); err == nil || !strings.Contains(err.Error(), "dubious ownership") {
		t.Errorf("CurrentBranch err = %v", err)
	}
}

func TestErrorFormat(t *testing.T) {
	e := &Error{Args: []string{"-c", "a=b", "worktree", "add"}, Stderr: "fatal: boom"}
	if got := e.Error(); got != "git worktree: fatal: boom" {
		t.Fatalf("got %q", got)
	}
}
