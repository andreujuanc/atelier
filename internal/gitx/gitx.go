// Package gitx is a thin wrapper around the git binary. Every function shells
// out to git via os/exec (never a shell) and wraps failures with the git
// subcommand and git's trimmed stderr.
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
	"strconv"
	"strings"
	"syscall"
	"time"
)

// localEnvVars is the output of `git rev-parse --local-env-vars`: repo-local
// variables that would redirect git away from the -C path. They are commonly
// set when running inside git hooks, so they are all stripped.
var localEnvVars = []string{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_CONFIG",
	"GIT_CONFIG_PARAMETERS",
	"GIT_CONFIG_COUNT",
	"GIT_OBJECT_DIRECTORY",
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_IMPLICIT_WORK_TREE",
	"GIT_GRAFT_FILE",
	"GIT_INDEX_FILE",
	"GIT_NO_REPLACE_OBJECTS",
	"GIT_REPLACE_REF_BASE",
	"GIT_PREFIX",
	"GIT_SHALLOW_FILE",
	"GIT_COMMON_DIR",
}

// stripEnv reports whether the environment entry kv must not reach git.
// GIT_CONFIG_KEY_<n>/GIT_CONFIG_VALUE_<n> go together with GIT_CONFIG_COUNT
// (a dangling COUNT without its keys is a fatal error in git).
func stripEnv(kv string) bool {
	name, _, _ := strings.Cut(kv, "=")
	if strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
		return true
	}
	for _, v := range localEnvVars {
		if name == v {
			return true
		}
	}
	return false
}

func gitEnv() []string {
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		if !stripEnv(kv) {
			env = append(env, kv)
		}
	}
	// GIT_OPTIONAL_LOCKS=0: polling `git status` must not take index.lock
	// while another git (e.g. Claude's) runs in the same worktree.
	return append(env, "LC_ALL=C", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
}

// subcommand returns the first non-flag argument (the git subcommand) for
// error messages, skipping "-C <dir>" and "-c <kv>".
func subcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-C" || a == "-c":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return a
		}
	}
	return ""
}

// Error is returned when a git invocation fails.
type Error struct {
	Args     []string // arguments passed to git (excluding "-C dir")
	Stderr   string   // trimmed stderr
	ExitCode int      // -1 if git did not run / exit normally
	Err      error    // underlying error; the context's error if it was cancelled
}

func (e *Error) Error() string {
	sub := subcommand(e.Args)
	if e.Stderr != "" {
		return fmt.Sprintf("git %s: %s", sub, e.Stderr)
	}
	return fmt.Sprintf("git %s: %v", sub, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

func command(ctx context.Context, dir string, args ...string) *exec.Cmd {
	full := args
	if dir != "" {
		full = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = gitEnv()
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// wrap turns a failed run into an *Error. If ctx is done, Err is ctx.Err() so
// that errors.Is(err, context.Canceled/DeadlineExceeded) holds.
func wrap(ctx context.Context, args []string, stderr string, err error) error {
	if err == nil {
		return nil
	}
	code := -1
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	}
	if cerr := ctx.Err(); cerr != nil {
		err = cerr
	}
	return &Error{Args: args, Stderr: strings.TrimSpace(stderr), ExitCode: code, Err: err}
}

// run executes git in dir and returns trimmed stdout.
func run(ctx context.Context, dir string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := command(ctx, dir, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", wrap(ctx, args, stderr.String(), err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func samePath(a, b string) bool {
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		return false
	}
	ra, _ = filepath.Abs(ra)
	rb, _ = filepath.Abs(rb)
	return filepath.Clean(ra) == filepath.Clean(rb)
}

// IsRepo reports whether path is the root of a git work tree or a bare
// repository. Plain directories (including ones nested inside another repo)
// and missing paths return false. A repo that git refuses to use because of
// "dubious ownership" (owned by another user and not listed in
// safe.directory) also returns false; RevParse/CurrentBranch on it return an
// error carrying git's stderr, which explains the reason.
func IsRepo(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || !fi.IsDir() {
		return false
	}
	ctx := context.Background()
	bare, err := run(ctx, path, "rev-parse", "--is-bare-repository")
	if err != nil {
		return false
	}
	if bare == "true" {
		gd, err := run(ctx, path, "rev-parse", "--absolute-git-dir")
		return err == nil && samePath(gd, path)
	}
	top, err := run(ctx, path, "rev-parse", "--show-toplevel")
	return err == nil && top != "" && samePath(top, path)
}

// CurrentBranch returns the checked-out branch name, or "" if HEAD is detached.
func CurrentBranch(ctx context.Context, repo string) (string, error) {
	args := []string{"symbolic-ref", "--quiet", "--short", "HEAD"}
	out, err := run(ctx, repo, args...)
	if err != nil {
		var ge *Error
		// exit 1 with --quiet: HEAD is not a symbolic ref (detached).
		if errors.As(err, &ge) && ge.ExitCode == 1 && ge.Stderr == "" {
			return "", nil
		}
		return "", err
	}
	return out, nil
}

// HeadCommit returns the full sha of HEAD.
func HeadCommit(ctx context.Context, repo string) (string, error) {
	return RevParse(ctx, repo, "HEAD")
}

// Fetch runs `git fetch origin` non-interactively.
func Fetch(ctx context.Context, repo string) error {
	_, err := run(ctx, repo, "fetch", "origin")
	return err
}

func hasOrigin(ctx context.Context, repo string) (bool, error) {
	out, err := run(ctx, repo, "remote")
	if err != nil {
		return false, err
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == "origin" {
			return true, nil
		}
	}
	return false, nil
}

func refExists(ctx context.Context, repo, ref string) bool {
	_, err := run(ctx, repo, "show-ref", "--verify", "--quiet", ref)
	return err == nil
}

// DefaultBranch returns origin's default branch name (e.g. "main"), from
// refs/remotes/origin/HEAD, else origin/main, else origin/master.
func DefaultBranch(ctx context.Context, repo string) (string, error) {
	ok, err := hasOrigin(ctx, repo)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("repo %s has no origin remote", repo)
	}
	// Full ref names avoid --short ambiguity (e.g. a local branch named
	// "origin/main"); a dangling origin/HEAD (target pruned) falls through.
	const prefix = "refs/remotes/origin/"
	if target, err := run(ctx, repo, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); err == nil &&
		strings.HasPrefix(target, prefix) && len(target) > len(prefix) && refExists(ctx, repo, target) {
		return strings.TrimPrefix(target, prefix), nil
	}
	for _, b := range []string{"main", "master"} {
		if refExists(ctx, repo, prefix+b) {
			return b, nil
		}
	}
	return "", fmt.Errorf("repo %s: cannot determine origin's default branch (no origin/HEAD, origin/main or origin/master; try fetching)", repo)
}

// RevParse resolves rev to a full commit sha.
func RevParse(ctx context.Context, repo, rev string) (string, error) {
	return run(ctx, repo, "rev-parse", "--verify", rev+"^{commit}")
}

// AddWorktree creates a new worktree at path on a new branch starting at startPoint.
// A relative path is resolved against the current directory, not repo.
func AddWorktree(ctx context.Context, repo, path, branch, startPoint string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("add worktree %s: %w", path, err)
	}
	_, err = run(ctx, repo, "worktree", "add", "-b", branch, abs, startPoint)
	return err
}

// RemoveWorktree removes the worktree at path (with --force if force) and
// prunes stale worktree metadata. A path that no longer exists is just pruned.
// A relative path is resolved against the current directory, not repo.
func RemoveWorktree(ctx context.Context, repo, path string, force bool) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("remove worktree: %w", err)
	}
	if _, err := os.Stat(path); err == nil {
		args := []string{"worktree", "remove"}
		if force {
			args = append(args, "--force")
		}
		args = append(args, path)
		if _, err := run(ctx, repo, args...); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove worktree %s: %w", path, err)
	}
	_, err = run(ctx, repo, "worktree", "prune")
	return err
}

// DeleteBranch force-deletes the local branch in repo (`git branch -D`).
func DeleteBranch(ctx context.Context, repo, branch string) error {
	_, err := run(ctx, repo, "branch", "-D", branch)
	return err
}

// UniqueCommits returns the number of commits reachable from the local
// branch that are not reachable from any other local branch or any
// remote-tracking ref, i.e. the commits that deleting the branch would lose.
// It only looks at local refs (no fetch). A missing branch is an error.
func UniqueCommits(ctx context.Context, repo, branch string) (int, error) {
	// --exclude patterns are relative to refs/heads/ for --branches and
	// apply to the next --branches only.
	out, err := run(ctx, repo, "rev-list", "--count", "refs/heads/"+branch,
		"--not", "--exclude="+branch, "--branches", "--remotes")
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(out)
	if err != nil {
		return 0, fmt.Errorf("git rev-list: unexpected output %q", out)
	}
	return n, nil
}

// IsDirty reports whether the worktree has uncommitted changes, counting
// untracked files (regardless of status.showUntrackedFiles). It takes no
// optional locks, so it is safe to poll while other git commands run.
func IsDirty(ctx context.Context, worktree string) (bool, error) {
	out, err := run(ctx, worktree, "--no-optional-locks", "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// CommitsAhead returns the number of commits in HEAD that are not in base.
func CommitsAhead(ctx context.Context, worktree, base string) (int, error) {
	out, err := run(ctx, worktree, "rev-list", "--count", base+"..HEAD")
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(out)
	if err != nil {
		return 0, fmt.Errorf("git rev-list: unexpected output %q", out)
	}
	return n, nil
}

// Diff streams `git diff base` (committed and uncommitted tracked changes
// relative to base) to w. If the reader goes away early (e.g. the pager quit:
// git dies of SIGPIPE or w returns EPIPE) the result is nil, not an error.
func Diff(ctx context.Context, worktree, base string, color bool, w io.Writer) error {
	colorFlag := "--no-color"
	if color {
		colorFlag = "--color=always"
	}
	args := []string{"diff", "--no-ext-diff", colorFlag, base, "--"}
	var stderr bytes.Buffer
	cmd := command(ctx, worktree, args...)
	// An *os.File is handed to git directly (git gets SIGPIPE itself); any
	// other writer is wrapped so its first error can be inspected.
	var ew *errWriter
	if _, ok := w.(*os.File); ok {
		cmd.Stdout = w
	} else {
		ew = &errWriter{w: w}
		cmd.Stdout = ew
	}
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ew != nil && ew.err != nil && ctx.Err() == nil {
		if errors.Is(ew.err, syscall.EPIPE) {
			return nil
		}
		return fmt.Errorf("git diff: write output: %w", ew.err)
	}
	if err != nil && ctx.Err() == nil && diedOfSIGPIPE(err) {
		return nil
	}
	return wrap(ctx, args, stderr.String(), err)
}

// errWriter records the first error returned by w.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	if err != nil {
		e.err = err
	}
	return n, err
}

func diedOfSIGPIPE(err error) bool {
	if errors.Is(err, syscall.EPIPE) {
		return true
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGPIPE {
		return true
	}
	return ee.ExitCode() == 128+int(syscall.SIGPIPE)
}

// RemoteBranchExists reports whether origin has a branch named branch.
func RemoteBranchExists(ctx context.Context, worktree, branch string) (bool, error) {
	_, err := run(ctx, worktree, "ls-remote", "--exit-code", "--heads", "origin", "refs/heads/"+branch)
	if err == nil {
		return true, nil
	}
	var ge *Error
	if errors.As(err, &ge) && ge.ExitCode == 2 {
		return false, nil
	}
	return false, err
}

// Untracked returns the untracked, non-ignored files of the worktree
// (each file individually, like `git status --untracked-files=all`), as
// paths relative to the worktree root, in git's order.
func Untracked(ctx context.Context, worktree string) ([]string, error) {
	var stdout, stderr bytes.Buffer
	args := []string{"ls-files", "-z", "--others", "--exclude-standard"}
	cmd := command(ctx, worktree, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, wrap(ctx, args, stderr.String(), err)
	}
	var out []string
	for _, p := range strings.Split(stdout.String(), "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

// LastCommit returns the sha, subject and committer date of HEAD in
// worktree.
func LastCommit(ctx context.Context, worktree string) (sha, subject string, when time.Time, err error) {
	out, err := run(ctx, worktree, "-c", "log.showSignature=false", "log", "-1", "--format=%H%x00%ct%x00%s", "HEAD", "--")
	if err != nil {
		return "", "", time.Time{}, err
	}
	parts := strings.SplitN(out, "\x00", 3)
	if len(parts) != 3 {
		return "", "", time.Time{}, fmt.Errorf("git log: unexpected output %q", out)
	}
	secs, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("git log: unexpected date %q", parts[1])
	}
	return parts[0], parts[2], time.Unix(secs, 0).UTC(), nil
}

// RefExists reports whether the fully qualified ref (e.g.
// "refs/remotes/origin/atl/x") exists in repo. It never touches the
// network.
func RefExists(ctx context.Context, repo, ref string) (bool, error) {
	_, err := run(ctx, repo, "show-ref", "--verify", "--quiet", ref)
	if err == nil {
		return true, nil
	}
	var ge *Error
	if errors.As(err, &ge) && ge.ExitCode == 1 && ge.Stderr == "" {
		return false, nil
	}
	return false, err
}
