// Package gitx is a thin wrapper around the git binary. Every function shells
// out to git via os/exec (never a shell) and wraps failures with the git
// subcommand and git's trimmed stderr.
//
// atl runs git in repos whose config, hooks and files a sandboxed task can
// write (its worktrees and its clone's .git are mounted in its container),
// with the credentials of the place atl runs in. So no git command here may
// run a program, or contact a server, that such a repo names:
//
//   - every command goes through command, which turns off what has a fixed
//     name (see safetyArgs and gitEnv);
//   - what cannot be turned off is avoided: IsDirty reads no file through a
//     filter (see dirty.go), RemoveWorktree checks with IsDirty itself;
//   - where it cannot be avoided (Diff and AddWorktree may run filters, a
//     push or fetch follows URL rules), the repo's own config is checked
//     right before, and the command is refused with a *RefusedError naming
//     the setting (see hazard.go). The check and git's own read are two
//     moments: a task rewriting its config in a tight loop can slip a
//     setting in between.
//
// Pushes and fetches go to a pinned Origin, never to what the repo's config
// says (see transport.go).
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/andreujuanc/atelier/internal/nofollow"
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

// gitEnv is the environment of every git atl runs. Besides the user's own
// (minus localEnvVars), it has:
//
//   - GIT_OPTIONAL_LOCKS=0: polling git must not take index.lock while
//     another git (e.g. Claude's) runs in the same worktree;
//   - GIT_NO_LAZY_FETCH=1: in a partial clone, a missing object would be
//     fetched from the promisor remote the repo's config names;
//   - GIT_NO_REPLACE_OBJECTS=1: refs/replace is the task's to write, and a
//     replace ref (a symlink to another repo's branch, say) would show
//     other history and files as the repo's;
//   - GIT_EDITOR, GIT_SEQUENCE_EDITOR and GIT_PAGER set to programs of our
//     own (they outrank core.editor, sequence.editor and core.pager; no
//     command here opens an editor, and --no-pager is given too);
//   - unless transport: GIT_ALLOW_PROTOCOL empty, so no transport at all
//     (it outranks every protocol.*.allow); only push, fetch and ls-remote
//     talk to a remote, with their own guards (transport.go).
func gitEnv(transport bool) []string {
	env := make([]string, 0, len(os.Environ())+8)
	for _, kv := range os.Environ() {
		if !stripEnv(kv) {
			env = append(env, kv)
		}
	}
	env = append(env, "LC_ALL=C", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1",
		"GIT_EDITOR=:", "GIT_SEQUENCE_EDITOR=:", "GIT_PAGER=cat")
	if !transport {
		env = append(env, "GIT_ALLOW_PROTOCOL=")
	}
	return env
}

// hookEvents are the hook events git knows (githooks(5), git 2.56). Newer
// git (2.56 does, 2.47 does not) also runs hooks named in config
// (hook.<name>.command and .event), under names of the config's choosing,
// which core.hooksPath does not turn off; hook.<event>.enabled=false turns
// off every configured hook of the event. A hook event git adds later is
// not in this list.
var hookEvents = []string{
	"applypatch-msg", "pre-applypatch", "post-applypatch", "pre-commit", "pre-merge-commit",
	"prepare-commit-msg", "commit-msg", "post-commit", "pre-rebase", "post-checkout", "post-merge",
	"pre-push", "pre-receive", "update", "proc-receive", "post-receive", "post-update",
	"reference-transaction", "receive-report", "push-to-checkout", "pre-auto-gc", "post-rewrite",
	"sendemail-validate", "fsmonitor-watchman", "p4-changelist", "p4-prepare-changelist",
	"p4-post-changelist", "p4-pre-submit", "post-index-change",
}

// safetyArgs precede every git command atl runs. Options given with -c
// outrank every config file, so the repo's own config cannot undo them:
//
//   - --no-pager (core.pager, pager.<cmd>);
//   - no fsmonitor (core.fsmonitor runs a program, or a daemon);
//   - no hooks: core.hooksPath to a directory that has none, and every
//     configured hook of every event off (see hookEvents);
//   - no recursing into submodules (their config is the task's too), no
//     automatic gc or maintenance, no signature checks in log output.
//
// Single-valued keys only: for a multi-valued key, or one whose name the
// config chooses, -c adds a value instead of replacing it (see hazard.go).
var safetyArgs = append([]string{"--no-pager"}, cOptions(safetyKV)...)

var safetyKV = func() []string {
	kv := []string{
		"core.fsmonitor=false",
		"core.hooksPath=/dev/null",
		"submodule.recurse=false",
		"gc.auto=0",
		"maintenance.auto=false",
		"log.showSignature=false",
	}
	for _, e := range hookEvents {
		kv = append(kv, "hook."+e+".enabled=false")
	}
	return kv
}()

// GuardedEnv is the environment for a program that runs git itself (gh):
// gitEnv's, with safetyArgs' settings as environment config
// (GIT_CONFIG_COUNT), which reaches every git it starts.
func GuardedEnv() []string {
	env := append(gitEnv(false), "GIT_CONFIG_COUNT="+strconv.Itoa(len(safetyKV)))
	for i, s := range safetyKV {
		k, v, _ := strings.Cut(s, "=")
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, k), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, v))
	}
	return env
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

// command is git with args in dir, with safetyArgs and gitEnv: every git
// atl runs is made here. Only transport commands (transport.go) may talk to
// a remote.
func command(ctx context.Context, dir string, args ...string) *exec.Cmd {
	return newCommand(ctx, dir, false, args)
}

func newCommand(ctx context.Context, dir string, transport bool, args []string) *exec.Cmd {
	full := make([]string, 0, len(safetyArgs)+len(args)+2)
	full = append(full, safetyArgs...)
	if dir != "" {
		full = append(full, "-C", dir)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = gitEnv(transport)
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

// output executes git in dir, with input (if not "") on its stdin, and
// returns its stdout as it is.
func output(ctx context.Context, dir, input string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := command(ctx, dir, args...)
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", wrap(ctx, args, stderr.String(), err)
	}
	return stdout.String(), nil
}

// run executes git in dir and returns trimmed stdout.
func run(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := output(ctx, dir, "", args...)
	return strings.TrimSpace(out), err
}

// revParsePath is the one path `git rev-parse <opts>` prints in dir, as it
// is: only the newline git ends it with is removed (a path may end in
// whitespace, or have a newline in it).
func revParsePath(ctx context.Context, dir string, opts ...string) (string, error) {
	out, err := output(ctx, dir, "", append([]string{"rev-parse"}, opts...)...)
	return strings.TrimSuffix(out, "\n"), err
}

// ConfigValue returns the value git uses for config key in dir (a repo, or
// any directory for the user's global value): "" if it is unset or cannot be
// read.
func ConfigValue(ctx context.Context, dir, key string) string {
	out, err := run(ctx, dir, "config", "--get", key)
	if err != nil {
		return ""
	}
	return out
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
		gd, err := revParsePath(ctx, path, "--absolute-git-dir")
		return err == nil && samePath(gd, path)
	}
	top, err := revParsePath(ctx, path, "--show-toplevel")
	return err == nil && top != "" && samePath(top, path)
}

// CheckWorktree returns nil if path is a worktree of the canonical clone
// repo: a directory (not a symlink to one) that is the top of a work tree
// whose common git dir is repo's .git. A sandboxed task can replace its
// worktree with a symlink, or point the worktree's .git file at another
// repo's git dir, to make git that atl runs there (with the user's
// credentials, for a push or fetch) act on another repo. A missing path is
// an error that satisfies errors.Is(err, fs.ErrNotExist).
func CheckWorktree(ctx context.Context, path, repo string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		what := "not a directory"
		if fi.Mode()&os.ModeSymlink != 0 {
			what = "a symlink"
		}
		return fmt.Errorf("%s is %s, not a worktree of %s", path, what, repo)
	}
	common, top, err := commonDirAndTop(ctx, path)
	if err != nil {
		return fmt.Errorf("%s is not a worktree of %s: %w", path, repo, err)
	}
	if !samePath(common, filepath.Join(repo, ".git")) {
		return fmt.Errorf("%s is not a worktree of %s: its git dir is in %s", path, repo, common)
	}
	if !samePath(top, path) {
		return fmt.Errorf("%s is not a worktree of %s: its work tree is %s", path, repo, top)
	}
	return nil
}

// commonDirAndTop returns the absolute git common dir and the top of the work
// tree of dir. Both come from one `git rev-parse` when its output, a line
// each, has no other newline; a path with a newline in it is asked for
// alone (one path per call), as is either path when the joint call fails,
// so its error is the one that call gives.
func commonDirAndTop(ctx context.Context, dir string) (common, top string, err error) {
	out, err := output(ctx, dir, "", "rev-parse", "--path-format=absolute", "--git-common-dir", "--show-toplevel")
	if err == nil && strings.Count(out, "\n") == 2 && strings.HasSuffix(out, "\n") {
		common, top, _ = strings.Cut(strings.TrimSuffix(out, "\n"), "\n")
		return common, top, nil
	}
	if common, err = revParsePath(ctx, dir, "--path-format=absolute", "--git-common-dir"); err != nil {
		return "", "", err
	}
	if top, err = revParsePath(ctx, dir, "--show-toplevel"); err != nil {
		return "", "", err
	}
	return common, top, nil
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

// IntegrationBranch returns the branch feature work starts from on
// origin: "dev" if refs/remotes/origin/dev exists, else "develop" if
// refs/remotes/origin/develop exists, else DefaultBranch. Only local refs
// are consulted (fetch first for an up-to-date answer).
func IntegrationBranch(ctx context.Context, repo string) (string, error) {
	ok, err := hasOrigin(ctx, repo)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("repo %s has no origin remote", repo)
	}
	for _, b := range []string{"dev", "develop"} {
		if refExists(ctx, repo, "refs/remotes/origin/"+b) {
			return b, nil
		}
	}
	return DefaultBranch(ctx, repo)
}

// RevParse resolves rev to a full commit sha.
func RevParse(ctx context.Context, repo, rev string) (string, error) {
	return run(ctx, repo, "rev-parse", "--verify", rev+"^{commit}")
}

// AddWorktree creates a new worktree at path on a new branch starting at startPoint.
// A relative path is resolved against the current directory, not repo.
//
// The checkout runs the filters .gitattributes and the config name, and the
// config's are the task's: it is refused while the repo's own config has
// one (see checkHazards; git-lfs's are let through). git-lfs, when it runs,
// reads the repo's config itself and downloads: its credential helpers,
// askpass and ssh command are the user's own here (as for a push), and the
// LFS server must be the pinned origin's (see lfsHazards).
func AddWorktree(ctx context.Context, repo, path, branch, startPoint string, origin Origin) error {
	return addWorktree(ctx, repo, path, branch, origin, func(cfg []string) error {
		// As `git worktree add -b` does it.
		_, err := run(ctx, repo, append(cfg, "branch", "--quiet", branch, startPoint)...)
		return err
	})
}

// AddBranchWorktree creates a new worktree at path on the existing local
// branch, with the checks of AddWorktree. A branch that another worktree
// has checked out, or is rebasing or bisecting, is refused, as git does.
func AddBranchWorktree(ctx context.Context, repo, path, branch string, origin Origin) error {
	return addWorktree(ctx, repo, path, branch, origin, func([]string) error {
		inUse := func() error {
			return &Error{Args: []string{"worktree", "add", path, branch}, ExitCode: 128,
				Stderr: fmt.Sprintf("fatal: '%s' is already used by a worktree", branch)}
		}
		out, err := run(ctx, repo, "worktree", "list", "--porcelain", "-z")
		if err != nil {
			return err
		}
		for _, f := range strings.Split(out, "\x00") {
			if f == "branch refs/heads/"+branch {
				return inUse()
			}
		}
		busy, err := rebasingOrBisecting(repo, branch)
		if err != nil {
			return err
		}
		if busy {
			return inUse()
		}
		return nil
	})
}

// rebasingOrBisecting reports whether a worktree of repo (its main one or
// a linked one) is rebasing branch (rebase-merge/head-name or
// rebase-apply/head-name names it) or bisecting from it (BISECT_START), as
// git's die_if_checked_out looks. Read without following symlinks.
func rebasingOrBisecting(repo, branch string) (bool, error) {
	gitDir, err := nofollow.Open(filepath.Join(repo, ".git"))
	if err != nil {
		return false, err
	}
	defer gitDir.Close()
	dirs := []string{""}
	ids, err := gitDir.ReadDirNames("worktrees")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	for _, id := range ids {
		dirs = append(dirs, "worktrees/"+id+"/")
	}
	read := func(rel string) string {
		data, err := gitDir.ReadFile(rel, 4096)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(data))
	}
	for _, d := range dirs {
		if read(d+"rebase-merge/head-name") == "refs/heads/"+branch ||
			read(d+"rebase-apply/head-name") == "refs/heads/"+branch ||
			read(d+"BISECT_START") == branch {
			return true, nil
		}
	}
	return false, nil
}

// addWorktree adds a worktree at path on branch, as `git worktree add` does,
// after the checks of AddWorktree and once branch (made ready by the
// caller's) can be checked out: it makes the worktree dir, registers it in
// repo (<repo>/.git/worktrees/<id>: gitdir, commondir, HEAD, and for a
// reftable repo the worktree's own ref store; locked while it is made) and
// checks out there the commit branch names (see checkout).
//
// git worktree add is not run: when it fails after it made the
// registration dir, it deletes that dir and the worktree by path (an lstat,
// then an opendir per entry). Every task with the repo rw writes its git
// dir, so a neighbouring task could make the checkout fail (an index.lock
// in the new registration dir) and swap a directory there for a symlink as
// the deletion starts: git would delete on the host what it points to.
// Here the registration is made, and a failed add undone, through file
// descriptors (package nofollow, RemoveWorktree); a worktree dir that was
// there (empty) is kept, emptied.
func addWorktree(ctx context.Context, repo, path, branch string, origin Origin, prepare func(cfg []string) error) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("add worktree %s: %w", path, err)
	}
	fail := func(msg string) error {
		return &Error{Args: []string{"worktree", "add", abs}, ExitCode: 128, Stderr: "fatal: " + msg}
	}
	cfg := userTransportArgs(ctx, repo)
	if err := checkHazards(ctx, repo, opCheckout, &origin, cfg); err != nil {
		return err
	}
	if entries, err := os.ReadDir(abs); err == nil && len(entries) > 0 || err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(fmt.Sprintf("'%s' already exists", abs))
	}
	gitDir, err := nofollow.Open(filepath.Join(repo, ".git"))
	if err != nil {
		return fmt.Errorf("add worktree %s: %w", abs, err)
	}
	defer gitDir.Close()
	if ids, err := worktreeIDs(gitDir, abs); err != nil {
		return fmt.Errorf("add worktree %s: %w", abs, err)
	} else if len(ids) > 0 {
		return fail(fmt.Sprintf("'%s' is a missing but already registered worktree", abs))
	}
	if err := prepare(cfg); err != nil {
		return err
	}
	ref := "refs/heads/" + branch
	commit, err := branchCommit(ctx, repo, ref)
	if err != nil {
		return err
	}
	reftable, err := usesReftable(ctx, repo)
	if err != nil {
		return err
	}
	made := false
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("add worktree %s: %w", abs, err)
	}
	if err := os.Mkdir(abs, 0o755); err == nil {
		made = true
	} else if !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("add worktree %s: %w", abs, err)
	}
	id, err := register(gitDir, abs, branch, reftable)
	if err == nil {
		err = linkWorktree(gitDir, abs, id)
	}
	if err == nil && reftable {
		// Into the worktree's own (reftable) ref store; no program runs.
		_, err = run(ctx, abs, "symbolic-ref", "HEAD", ref)
	}
	if err == nil {
		err = checkout(ctx, repo, abs, commit, origin, cfg, func(index []byte) error {
			return gitDir.WriteFile("worktrees/"+id+"/index", index, 0o644)
		})
	}
	if err == nil {
		err = gitDir.Remove("worktrees/" + id + "/locked")
	}
	if err != nil {
		// Undone as git would, without following a symlink: the
		// registration, and the worktree dir if made here, else what is
		// in it.
		if id != "" {
			gitDir.RemoveAll("worktrees/" + id)
		}
		if made {
			os.RemoveAll(abs)
		} else {
			emptyDir(abs)
		}
		return err
	}
	return nil
}

// emptyDir removes what is in the directory dir, without following a
// symlink.
func emptyDir(dir string) {
	d, err := nofollow.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	names, _ := d.ReadDirNames(".")
	for _, n := range names {
		d.RemoveAll(n)
	}
}

// checkoutKeys are the settings of the repo's config that shape how files
// are written, which checkout copies (none runs a program).
var checkoutKeys = []string{"core.autocrlf", "core.eol", "core.safecrlf", "core.symlinks", "core.ignorecase",
	"core.precomposeunicode", "core.filemode", "core.checkroundtripencoding"}

// checkout writes the files of commit into the worktree dir wt (empty but
// for its .git file) and hands their index to save (the worktree's, which
// save writes without following a symlink: git would write through one):
// first as read from the commit, before any file is written, so that an
// atl killed meanwhile leaves the files git wrote, with an index git
// compares them to; then with the files' stat data.
//
// It runs git (read-tree, checkout-index) with a git dir of its own: a temporary one
// that borrows the objects of repo (alternates), with a config atl writes
// (checkoutKeys copied from repo's, and git-lfs's filter and pinned origin
// if repo uses git-lfs) and no info/. The repo's own config and
// info/attributes, which a task writes, are not read: a filter it defines
// after checkHazards looked (a neighbouring task that sees the new
// worktree's registration appear) would run where atl runs. Filters still
// come from the user's own config, as the attributes in the commit name
// them.
func checkout(ctx context.Context, repo, wt, commit string, origin Origin, cfg []string, save func(index []byte) error) error {
	objFormat, err := run(ctx, repo, "rev-parse", "--show-object-format")
	if err != nil {
		return err
	}
	priv, err := os.MkdirTemp("", "atl-checkout-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(priv)
	if _, err := run(ctx, os.TempDir(), "init", "--bare", "--quiet", "--template=", "--object-format="+objFormat, priv); err != nil {
		return err
	}
	objects := filepath.Join(repo, ".git", "objects")
	if err := os.WriteFile(filepath.Join(priv, "objects", "info", "alternates"), []byte(alternatesLine(objects)+"\n"), 0o644); err != nil {
		return err
	}
	kv := [][2]string{{"core.bare", "false"}, {"core.splitIndex", "false"}}
	for _, k := range checkoutKeys {
		if v := ConfigValue(ctx, repo, k); v != "" {
			kv = append(kv, [2]string{k, v})
		}
	}
	if ConfigValue(ctx, repo, "filter.lfs.process") != "" || ConfigValue(ctx, repo, "filter.lfs.smudge") != "" {
		kv = append(kv, [2]string{"filter.lfs.clean", lfsDefaults["filter.lfs.clean"][0]},
			[2]string{"filter.lfs.smudge", lfsDefaults["filter.lfs.smudge"][0]},
			[2]string{"filter.lfs.process", lfsDefaults["filter.lfs.process"][0]},
			[2]string{"filter.lfs.required", "true"},
			[2]string{"lfs.storage", filepath.Join(repo, ".git", "lfs")})
		if len(origin.URLs) > 0 {
			kv = append(kv, [2]string{"remote.origin.url", origin.URLs[0]})
		}
	}
	for _, s := range kv {
		if _, err := run(ctx, priv, "config", "--file", filepath.Join(priv, "config"), s[0], s[1]); err != nil {
			return err
		}
	}
	git := append([]string{"--git-dir=" + priv, "--work-tree=" + wt}, cfg...)
	saveIndex := func() error {
		index, err := os.ReadFile(filepath.Join(priv, "index"))
		if err != nil {
			return err
		}
		return save(index)
	}
	if _, err := run(ctx, "", append(git, "read-tree", commit)...); err != nil {
		return err
	}
	if err := saveIndex(); err != nil {
		return err
	}
	if _, err := run(ctx, "", append(git, "checkout-index", "--all", "--force", "-u")...); err != nil {
		return err
	}
	return saveIndex()
}

// usesReftable reports whether repo keeps its refs in reftable (git
// rev-parse --show-ref-format; a git too old to say has only files).
func usesReftable(ctx context.Context, repo string) (bool, error) {
	out, err := run(ctx, repo, "rev-parse", "--show-ref-format")
	if err != nil {
		var ge *Error
		if errors.As(err, &ge) && ge.ExitCode > 0 && !strings.Contains(ge.Stderr, "reftable") {
			return false, nil
		}
		return false, err
	}
	return out == "reftable", nil
}

// register makes the registration of a new worktree at path, on branch, in
// gitDir (a repo's own git dir): worktrees/<id>, id the first free of
// path's base name, and that name with 1, 2, ... after it, as git names
// it. It is locked ("initializing") until the worktree is checked out. In
// a reftable repo the worktree gets its own empty ref store, laid out as
// git lays it out (HEAD and refs/heads stubs), HEAD set afterwards.
func register(gitDir *nofollow.Dir, path, branch string, reftable bool) (id string, err error) {
	if err := gitDir.MkdirAll("worktrees", 0o755); err != nil {
		return "", fmt.Errorf("add worktree %s: %w", path, err)
	}
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, filepath.Base(path))
	for i := 0; ; i++ {
		id = name
		if i > 0 {
			id = name + strconv.Itoa(i)
		}
		err := gitDir.Mkdir("worktrees/"+id, 0o755)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) || i >= 1000 {
			return "", fmt.Errorf("add worktree %s: %w", path, err)
		}
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return id, fmt.Errorf("add worktree %s: %w", path, err)
	}
	dir := "worktrees/" + id + "/"
	files := []struct{ name, content string }{
		{"locked", "initializing"},
		{"gitdir", real + "/.git\n"},
		{"commondir", "../..\n"},
		{"HEAD", "ref: refs/heads/" + branch + "\n"},
	}
	if reftable {
		for _, d := range []string{"refs", "reftable"} {
			if err := gitDir.Mkdir(dir+d, 0o755); err != nil {
				return id, fmt.Errorf("add worktree %s: %w", path, err)
			}
		}
		files[3].content = "ref: refs/heads/.invalid\n"
		files = append(files, struct{ name, content string }{"refs/heads", "this repository uses the reftable format\n"},
			struct{ name, content string }{"reftable/tables.list", ""})
	}
	for _, f := range files {
		if err := gitDir.WriteFile(dir+f.name, []byte(f.content), 0o644); err != nil {
			return id, fmt.Errorf("add worktree %s: %w", path, err)
		}
	}
	return id, nil
}

// linkWorktree writes the .git file of the worktree at path, which points
// to its registration id in gitDir (without following a symlink there).
func linkWorktree(gitDir *nofollow.Dir, path, id string) error {
	common := gitDir.Path()
	if real, err := filepath.EvalSymlinks(common); err == nil {
		common = real
	}
	wt, err := nofollow.Open(path)
	if err != nil {
		return fmt.Errorf("add worktree %s: %w", path, err)
	}
	defer wt.Close()
	if err := wt.WriteFile(".git", []byte("gitdir: "+filepath.Join(common, "worktrees", id)+"\n"), 0o644); err != nil {
		return fmt.Errorf("add worktree %s: %w", path, err)
	}
	return nil
}

// RemoveWorktree removes the worktree at path (with force even if it has
// changes) and its registration in repo, <repo>/.git/worktrees/<id>. A path
// that no longer exists only loses its registration. A relative path is
// resolved against the current directory, not repo.
//
// Without force it refuses, as git does, a worktree with changes (IsDirty)
// or with checked-out submodules; git's own check is `git status`, which
// reads files through the repo's filters, so it is IsDirty's instead. A
// locked worktree is removed too, after the same checks, and so is a
// missing one's registration (git worktree prune would keep it): atl
// removes only its tasks' own worktrees, and a git killed while it added
// one leaves it locked ("initializing"), its branch taken.
//
// git deletes nothing here: `git worktree remove` and `git worktree prune`
// delete a directory by path, an lstat and then an opendir per entry, and
// every task with the repo rw can write its git dir (worktrees/ too). Such
// a task swaps a directory there for a symlink once the deletion starts,
// and git deletes on the host what the symlink points to. So the two
// directories are removed here through file descriptors (os.RemoveAll,
// package nofollow), which follow no symlink, and only this worktree's
// registrations go: a stale one a task planted is left alone.
func RemoveWorktree(ctx context.Context, repo, path string, force bool) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("remove worktree: %w", err)
	}
	gitDir, err := nofollow.Open(filepath.Join(repo, ".git"))
	if err != nil {
		return fmt.Errorf("remove worktree %s: %w", path, err)
	}
	defer gitDir.Close()
	ids, err := worktreeIDs(gitDir, path)
	if err != nil {
		return fmt.Errorf("remove worktree %s: %w", path, err)
	}
	_, err = os.Lstat(path)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove worktree %s: %w", path, err)
	}
	if exists {
		if len(ids) == 0 {
			return &Error{Args: []string{"worktree", "remove", path}, ExitCode: 128,
				Stderr: fmt.Sprintf("fatal: '%s' is not a working tree", path)}
		}
		if !force {
			if err := removable(ctx, path); err != nil {
				return err
			}
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove worktree %s: %w", path, err)
		}
	}
	for _, id := range ids {
		if err := gitDir.RemoveAll("worktrees/" + id); err != nil {
			return fmt.Errorf("remove worktree %s: %w", path, err)
		}
	}
	gitDir.Remove("worktrees") // only once empty, as git does
	return nil
}

// worktreeIDs returns the names of the entries of gitDir's worktrees/ that
// register a worktree at path: their gitdir file names path/.git (or the
// same with path's parent's symlinks resolved, as git writes it), absolute
// or relative to the entry.
func worktreeIDs(gitDir *nofollow.Dir, path string) ([]string, error) {
	want := []string{path}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		want = append(want, filepath.Join(dir, filepath.Base(path)))
	}
	base := gitDir.Path()
	if real, err := filepath.EvalSymlinks(base); err == nil {
		base = real
	}
	names, err := gitDir.ReadDirNames("worktrees")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, id := range names {
		data, err := gitDir.ReadFile("worktrees/"+id+"/gitdir", 64<<10)
		if err != nil {
			continue
		}
		// As git reads it: trailing whitespace and /.git off.
		p := strings.TrimSuffix(strings.TrimRight(string(data), " \t\n\v\f\r"), "/.git")
		if !filepath.IsAbs(p) {
			p = filepath.Join(base, "worktrees", id, p)
		}
		if slices.Contains(want, filepath.Clean(p)) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// removable is git's check before `worktree remove` without --force, made
// without running anything the repo names.
func removable(ctx context.Context, path string) error {
	ix, err := readIndex(ctx, path)
	if err != nil {
		return err
	}
	for _, e := range ix.entries {
		if e.mode == modeGitlink && populated(filepath.Join(path, e.path)) {
			return &Error{Args: []string{"worktree", "remove", path}, ExitCode: 128,
				Stderr: "fatal: working trees containing submodules cannot be moved or removed"}
		}
	}
	dirty, err := IsDirty(ctx, path)
	if err != nil {
		return err
	}
	if dirty {
		return &Error{Args: []string{"worktree", "remove", path}, ExitCode: 128,
			Stderr: fmt.Sprintf("fatal: '%s' contains modified or untracked files, use --force to delete it", path)}
	}
	return nil
}

// Worktrees returns the paths of the repo's linked worktrees (not its main
// one) that exist on disk.
func Worktrees(ctx context.Context, repo string) ([]string, error) {
	out, err := run(ctx, repo, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	var paths []string
	for i, f := range strings.Split(out, "\x00") {
		p, ok := strings.CutPrefix(f, "worktree ")
		if !ok || i == 0 {
			continue // the first is the main worktree
		}
		if _, err := os.Stat(p); err == nil {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// DeleteBranch force-deletes the local branch in repo (`git branch -D`).
func DeleteBranch(ctx context.Context, repo, branch string) error {
	_, err := run(ctx, repo, "branch", "-D", branch)
	return err
}

// DeleteBranchIfAt deletes the local branch in repo only if its tip is
// still oid (a compare-and-delete via `git update-ref -d`). If the branch
// moved, is missing, or is checked out in a worktree, it fails and the
// branch is left untouched.
func DeleteBranchIfAt(ctx context.Context, repo, branch, oid string) error {
	if oid == "" {
		return fmt.Errorf("delete branch %s: empty expected oid", branch)
	}
	ref := "refs/heads/" + branch
	// update-ref does not refuse a branch checked out in a worktree (unlike
	// `git branch -D`), so check that first.
	out, err := run(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "branch "+ref {
			return fmt.Errorf("delete branch %s: checked out in a worktree", branch)
		}
	}
	_, err = run(ctx, repo, "update-ref", "-d", ref, oid)
	return err
}

// UniqueCommits returns the number of commits reachable from the local
// branch that are not reachable from any other local branch or any
// remote-tracking ref, i.e. the commits that deleting the branch would lose.
// It only looks at local refs (no fetch). A missing branch is an error.
func UniqueCommits(ctx context.Context, repo, branch string) (int, error) {
	tip, err := RevParse(ctx, repo, "refs/heads/"+branch)
	if err != nil {
		return 0, err
	}
	return UniqueCommitsAt(ctx, repo, branch, tip)
}

// UniqueCommitsAt is UniqueCommits counted from the commit tip (the
// branch's tip as read earlier by the caller) instead of the branch's
// current tip. Neither the branch itself nor any symbolic ref that resolves
// to it (e.g. refs/heads/alias -> refs/heads/<branch>) counts as another
// ref.
func UniqueCommitsAt(ctx context.Context, repo, branch, tip string) (int, error) {
	self := "refs/heads/" + branch
	out, err := run(ctx, repo, "for-each-ref", "--format=%(refname)%00%(symref)%00%(objectname)",
		"refs/heads", "refs/remotes")
	if err != nil {
		return 0, err
	}
	type entry struct{ symref, oid string }
	refs := map[string]entry{}
	var order []string
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\x00", 3)
		if len(f) != 3 {
			return 0, fmt.Errorf("git for-each-ref: unexpected output %q", line)
		}
		refs[f[0]] = entry{symref: f[1], oid: f[2]}
		order = append(order, f[0])
	}
	// resolvesToSelf follows symref chains (with a cycle guard) and reports
	// whether name ends up at the branch.
	resolvesToSelf := func(name string) bool {
		seen := map[string]bool{}
		for name != "" && !seen[name] {
			if name == self {
				return true
			}
			seen[name] = true
			e, ok := refs[name]
			if !ok {
				return false
			}
			name = e.symref
		}
		return false
	}
	// Revisions go on stdin so that many refs cannot overflow argv.
	var in strings.Builder
	in.WriteString(tip + "\n")
	done := map[string]bool{}
	for _, name := range order {
		e := refs[name]
		if e.oid == "" || done[e.oid] || resolvesToSelf(name) {
			continue
		}
		done[e.oid] = true
		in.WriteString("^" + e.oid + "\n")
	}
	out, err = output(ctx, repo, in.String(), "rev-list", "--count", "--stdin")
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("git rev-list: unexpected output %q", out)
	}
	return n, nil
}

// OrphanCommits returns the number of commits reachable from the
// worktree's HEAD that no branch, remote-tracking ref or tag reaches: commits
// made on a detached HEAD (or during a stopped rebase) that removing the
// worktree would make unreachable. It only looks at local refs (no fetch).
func OrphanCommits(ctx context.Context, worktree string) (int, error) {
	out, err := run(ctx, worktree, "rev-list", "--count", "HEAD", "--not", "--branches", "--remotes", "--tags")
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(out)
	if err != nil {
		return 0, fmt.Errorf("git rev-list: unexpected output %q", out)
	}
	return n, nil
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
//
// No external diff programs or textconv, and a submodule is one line (its
// commit), never a look into its worktree. Changed files are read through
// the filters the repo's config names, so it is refused while that config
// has one of its own (see checkHazards).
func Diff(ctx context.Context, worktree, base string, color bool, w io.Writer) error {
	if err := checkHazards(ctx, worktree, opDiff, nil, nil); err != nil {
		return err
	}
	colorFlag := "--no-color"
	if color {
		colorFlag = "--color=always"
	}
	args := []string{"diff", "--no-ext-diff", "--no-textconv", "--ignore-submodules=dirty", "--submodule=short",
		colorFlag, base, "--"}
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

// RemoteBranchExists reports whether origin has a branch named branch,
// asking origin's fetch URL with the guards of FetchOrigin.
func RemoteBranchExists(ctx context.Context, dir string, origin Origin, branch string) (bool, error) {
	args, err := lsRemoteArgs(ctx, dir, origin, "--exit-code", "--heads")
	if err != nil {
		return false, err
	}
	_, err = runTransportOutput(ctx, dir, append(args, "refs/heads/"+branch))
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
	stdout, err := output(ctx, worktree, "", "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range strings.Split(stdout, "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

// LastCommit returns the sha, subject and committer date of HEAD in
// worktree. HEAD, and the branch it names, are read here without following
// a symlink (headCommit), in a git dir that reads no other repository
// (checkGitDirs): git would follow a HEAD or ref that is a symlink to
// another repo's branch.
func LastCommit(ctx context.Context, worktree string) (sha, subject string, when time.Time, err error) {
	gitDir, common, err := checkGitDirs(ctx, worktree)
	if err != nil {
		return "", "", time.Time{}, err
	}
	head, err := headCommit(ctx, worktree, gitDir, common)
	if err != nil {
		return "", "", time.Time{}, err
	}
	out, err := run(ctx, worktree, "log", "-1", "--format=%H%x00%ct%x00%s", head, "--")
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

// RefOrBelowExists reports whether the fully qualified ref (e.g.
// "refs/heads/atl/x"), or a ref below it as a directory ("refs/heads/atl/x/y",
// which keeps git from creating ref), exists in repo. It never touches the
// network. ref must not contain glob characters (for-each-ref would take it
// for a pattern).
func RefOrBelowExists(ctx context.Context, repo, ref string) (bool, error) {
	if strings.ContainsAny(ref, "*?[\\") || !strings.HasPrefix(ref, "refs/") {
		return false, fmt.Errorf("git for-each-ref: not a plain ref %q", ref)
	}
	// A pattern without glob characters matches the ref itself and every
	// ref below it ("up to a slash"), not refs/heads/atl/x-2.
	out, err := run(ctx, repo, "for-each-ref", "--count=1", "--format=%(refname)", "--", ref)
	if err != nil {
		return false, err
	}
	return out != "", nil
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

// alternatesLine is path as a line of objects/info/alternates: C-quoted
// when it has a character a plain line cannot hold.
func alternatesLine(path string) string {
	if !strings.ContainsAny(path, "\"\\\n\r\t") && !strings.HasPrefix(path, "#") {
		return path
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(path); i++ {
		switch c := path[i]; {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, "\\%03o", c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
