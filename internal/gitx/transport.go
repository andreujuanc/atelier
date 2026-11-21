package gitx

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

// Origin is where a repo's origin points: its fetch URL and push URLs as git
// resolves them (insteadOf rules applied). atl pins it once per repo and
// pushes and fetches there, whatever the repo's config says later.
type Origin struct {
	URL      string   `json:"url"`
	PushURLs []string `json:"push_urls"`
}

// Equal reports whether o and p have the same URLs, in the same order.
func (o Origin) Equal(p Origin) bool {
	return o.URL == p.URL && slices.Equal(o.PushURLs, p.PushURLs)
}

// String is the fetch URL, followed by the push URLs if they differ.
func (o Origin) String() string {
	if len(o.PushURLs) == 1 && o.PushURLs[0] == o.URL {
		return o.URL
	}
	return o.URL + " (push: " + strings.Join(o.PushURLs, ", ") + ")"
}

// CurrentOrigin returns origin's URLs as the repo at dir resolves them now;
// ok is false when the repo has no origin.
func CurrentOrigin(ctx context.Context, dir string) (o Origin, ok bool, err error) {
	if o.URL, err = run(ctx, dir, "remote", "get-url", "origin"); err != nil {
		var ge *Error
		if errors.As(err, &ge) && ge.ExitCode == 2 { // no such remote
			return Origin{}, false, nil
		}
		return Origin{}, false, err
	}
	push, err := run(ctx, dir, "remote", "get-url", "--push", "--all", "origin")
	if err != nil {
		return Origin{}, false, err
	}
	o.PushURLs = strings.Split(push, "\n")
	return o, true, nil
}

// PushBranch pushes the local branch of the repo at dir to the same branch
// at origin's push URLs, and nothing else (no tags, no submodules); force
// means --force-with-lease. git's output goes to out as it comes.
//
// It runs git in a repo whose config and hooks may have been written by a
// sandboxed task, with the credentials of the place atl runs in, so it keeps
// git from running anything that config names (see transportArgs), and the
// branch goes to origin's URLs, never to what the repo's config says (see
// remoteArgs).
func PushBranch(ctx context.Context, dir string, origin Origin, branch string, force bool, out io.Writer) error {
	ref := "refs/heads/" + branch
	local := allLocal(origin.PushURLs)
	name, remote, err := remoteArgs(ctx, dir, origin, true)
	if err != nil {
		return err
	}
	args := append(transportArgs(ctx, dir, true, local), remote...)
	args = append(args, "push", "--no-follow-tags", "--recurse-submodules=no")
	if force {
		args = append(args, "--force-with-lease")
	}
	args = append(args, "--receive-pack="+packProgram(true, local), name, ref+":"+ref)
	return runTransport(ctx, dir, out, args)
}

// FetchOrigin is `git fetch origin` in the repo at dir, from origin's URL,
// with the same guards as PushBranch. git's output goes to out.
func FetchOrigin(ctx context.Context, dir string, origin Origin, out io.Writer) error {
	local := isLocalURL(origin.URL)
	name, remote, err := remoteArgs(ctx, dir, origin, false)
	if err != nil {
		return err
	}
	args := append(transportArgs(ctx, dir, false, local), remote...)
	args = append(args, "fetch", "--no-recurse-submodules", "--no-auto-gc",
		"--upload-pack="+packProgram(false, local), name)
	return runTransport(ctx, dir, out, args)
}

// remoteArgs defines, with -c options only, a remote that stands in for
// origin and returns its name and the options. Its URLs are origin's (not
// the repo's remote.origin.*), and its fetch refspecs are the repo's own
// remote.origin.fetch, so that a push still updates refs/remotes/origin/<b>
// and --force-with-lease leases against it, and a single-branch clone
// fetches what it did. A refspec the task wrote can only touch local refs,
// which the task can write anyway.
//
// The name is random: remote.<name>.url and pushurl are lists, so a remote
// the repo's config could name in advance would get its URLs too.
//
// -c cannot remove url.<base>.insteadOf or pushInsteadOf rules, which apply
// to these URLs as to any; so a push or fetch is refused while the repo's
// own config has one (see repoRewrites). The user's own rules (system,
// global) stay: origin's URLs were resolved with them, and the usual ones
// (https://host/ → git@host:) leave a rewritten URL as it is. The check and
// git's own read of the config are two moments, so a task rewriting its
// config in a loop could slip a rule in between; that is part of the task
// being able to write the clone's .git at all (see README, Sandbox).
func remoteArgs(ctx context.Context, dir string, origin Origin, push bool) (string, []string, error) {
	if origin.URL == "" || len(origin.PushURLs) == 0 {
		return "", nil, errors.New("no origin URL")
	}
	if err := repoRewrites(ctx, dir); err != nil {
		return "", nil, err
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", nil, err
	}
	name := "atl-" + hex.EncodeToString(b[:])
	kv := []string{"url=" + origin.URL, "followRemoteHEAD=never"}
	if push {
		for _, u := range origin.PushURLs {
			kv = append(kv, "pushurl="+u)
		}
	}
	refspecs, _ := run(ctx, dir, "config", "--get-all", "remote.origin.fetch")
	n := len(kv)
	for _, r := range strings.Split(refspecs, "\n") {
		if r != "" {
			kv = append(kv, "fetch="+r)
		}
	}
	if len(kv) == n {
		kv = append(kv, "fetch=+refs/heads/*:refs/remotes/origin/*")
	}
	args := make([]string, 0, 2*len(kv))
	for _, s := range kv {
		args = append(args, "-c", "remote."+name+"."+s)
	}
	return name, args, nil
}

// repoRewrites refuses when the config of the repo at dir itself (scopes
// local and worktree, includes followed) has a url.<base>.insteadOf or
// pushInsteadOf rule.
func repoRewrites(ctx context.Context, dir string) error {
	data, err := run(ctx, dir, "config", "--show-scope", "--includes", "--null",
		"--get-regexp", `^url\..*\.(insteadof|pushinsteadof)$`)
	if err != nil {
		var ge *Error
		if errors.As(err, &ge) && ge.ExitCode == 1 && ge.Stderr == "" {
			return nil // none
		}
		return err
	}
	// Records are scope NUL key LF value NUL.
	f := strings.Split(data, "\x00")
	for i := 0; i+1 < len(f); i += 2 {
		if scope := f[i]; scope == "local" || scope == "worktree" {
			key, value, _ := strings.Cut(f[i+1], "\n")
			return fmt.Errorf("refused: the repo's own config (%s) has %s = %s, which would rewrite origin's URL; remove it and try again",
				scope, key, value)
		}
	}
	return nil
}

// transportArgs are the -c options of a push or fetch in dir. Options given
// with -c outrank every config file, so the repo's own config cannot undo
// them:
//
//   - no hooks (pre-push, reference-transaction, ...), no fsmonitor and no
//     core.alternateRefsCommand, which would run programs;
//   - no credential helpers but the user's own (system and global config),
//     no askpass program but the user's, and ssh as the user runs it
//     ($GIT_SSH_COMMAND, else their core.sshCommand, else $GIT_SSH or ssh);
//   - no ext:: transport (it runs a command), and no file transport unless
//     origin's URLs are local paths (local; see packProgram);
//   - for a push: no signing, no push options.
//
// core.gitProxy cannot be undone with -c (git takes its first value), so
// runTransport sets $GIT_PROXY_COMMAND, which comes before it.
func transportArgs(ctx context.Context, dir string, push, local bool) []string {
	kv := []string{
		"core.hooksPath=/dev/null",
		"core.fsmonitor=false",
		"core.alternateRefsCommand=true",
		"protocol.ext.allow=never",
		"credential.helper=", // an empty value drops the helpers configured so far
	}
	user := userConfig(ctx, dir)
	for _, e := range user {
		if strings.HasPrefix(e.key, "credential.") {
			kv = append(kv, e.key+"="+e.value)
		}
	}
	kv = append(kv, "core.askPass="+user.last("core.askpass"))
	if os.Getenv("GIT_SSH_COMMAND") == "" { // else it wins over any config
		ssh := user.last("core.sshcommand")
		if ssh == "" {
			if ssh = os.Getenv("GIT_SSH"); ssh != "" {
				ssh = shellQuote(ssh)
			} else {
				ssh = "ssh"
			}
		}
		kv = append(kv, "core.sshCommand="+ssh)
	}
	if !local {
		kv = append(kv, "protocol.file.allow=never")
	}
	if push {
		kv = append(kv, "push.gpgSign=false", "push.pushOption=")
	}
	args := make([]string, 0, 2*len(kv))
	for _, s := range kv {
		args = append(args, "-c", s)
	}
	return args
}

// packProgram is the program git runs on the other side of a push
// (receive-pack) or fetch (upload-pack). Over ssh that runs on the server.
// For a local origin it runs here, in the origin repo, and git does not pass
// it our -c options: so it is given them itself, and the origin repo's hooks
// do not run either. When origin did not look local, file transport is off
// (transportArgs), so a URL that turns out local after all is refused.
func packProgram(push, local bool) string {
	name := "upload-pack"
	if push {
		name = "receive-pack"
	}
	if !local {
		return "git-" + name
	}
	return "git -c core.hooksPath=/dev/null -c core.fsmonitor=false -c core.alternateRefsCommand=true -c receive.autogc=false " + name
}

func allLocal(urls []string) bool {
	if len(urls) == 0 {
		return false
	}
	for _, u := range urls {
		if !isLocalURL(u) {
			return false
		}
	}
	return true
}

// isLocalURL reports whether git treats url as a path on this machine: a
// file:// URL, or one with no colon before its first slash (else it is
// scp-like ssh, host:path).
func isLocalURL(url string) bool {
	if strings.HasPrefix(url, "file://") {
		return true
	}
	if strings.Contains(url, "://") || strings.Contains(url, "::") {
		return false
	}
	colon, slash := strings.IndexByte(url, ':'), strings.IndexByte(url, '/')
	return colon < 0 || (slash >= 0 && slash < colon)
}

type configEntry struct{ key, value string }

type configEntries []configEntry

// last returns the last value of key (lower case), or "".
func (c configEntries) last(key string) string {
	v := ""
	for _, e := range c {
		if e.key == key {
			v = e.value
		}
	}
	return v
}

// userConfig returns the user's own transport settings: those of the
// system and global config files (never the repo's own), in the order git
// reads them, as they apply to the repo at dir (includeIf). Section and
// variable names come lower case.
func userConfig(ctx context.Context, dir string) configEntries {
	const pattern = `^(core\.(sshcommand|askpass)|credential\.(.+\.)?helper)$`
	scopes := []string{"--global"}
	if os.Getenv("GIT_CONFIG_NOSYSTEM") == "" {
		scopes = []string{"--system", "--global"}
	}
	var out configEntries
	for _, scope := range scopes {
		data, err := run(ctx, dir, "config", scope, "--includes", "--null", "--get-regexp", pattern)
		if err != nil {
			continue // none, or no such file
		}
		for _, rec := range strings.Split(data, "\x00") {
			if rec == "" {
				continue
			}
			key, value, _ := strings.Cut(rec, "\n")
			out = append(out, configEntry{key, value})
		}
	}
	return out
}

// shellQuote quotes s for sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runTransport runs git with args in dir, its stdout and stderr going to
// out. A failure's Error carries the last line git wrote (the rest went to
// out already).
func runTransport(ctx context.Context, dir string, out io.Writer, args []string) error {
	if out == nil {
		out = io.Discard
	}
	cmd := command(ctx, dir, args...)
	if os.Getenv("GIT_PROXY_COMMAND") == "" {
		cmd.Env = append(cmd.Env, "GIT_PROXY_COMMAND=")
	}
	// One writer for both: exec then never calls it from two goroutines.
	w := &teeWriter{w: out}
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Run(); err != nil {
		return wrap(ctx, args, lastLine(w.buf.String()), err)
	}
	return nil
}

// teeWriter writes to w and keeps a copy.
type teeWriter struct {
	w   io.Writer
	buf bytes.Buffer
}

func (t *teeWriter) Write(p []byte) (int, error) {
	t.buf.Write(p)
	t.w.Write(p) // a reader that went away does not stop git
	return len(p), nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
