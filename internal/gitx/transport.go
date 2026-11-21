package gitx

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
)

// PushBranch pushes the local branch of the repo at dir to the same branch
// on origin, and nothing else (no tags, no submodules); force means
// --force-with-lease. git's output goes to out as it comes.
//
// It runs git in a repo whose config and hooks may have been written by a
// sandboxed task, with the credentials of the place atl runs in, so it keeps
// git from running anything that config names (see transportArgs). The
// remote URL itself is still read from the repo's config (remote.origin.url
// and pushurl, url.*.insteadOf): the branch goes wherever origin points.
func PushBranch(ctx context.Context, dir, branch string, force bool, out io.Writer) error {
	ref := "refs/heads/" + branch
	local := allLocal(remoteURLs(ctx, dir, true))
	args := transportArgs(ctx, dir, true, local)
	args = append(args, "push", "--no-follow-tags", "--recurse-submodules=no")
	if force {
		args = append(args, "--force-with-lease")
	}
	args = append(args, "--receive-pack="+packProgram(true, local), "origin", ref+":"+ref)
	return runTransport(ctx, dir, out, args)
}

// FetchOrigin is `git fetch origin` in the repo at dir with the same guards
// as PushBranch. git's output goes to out.
func FetchOrigin(ctx context.Context, dir string, out io.Writer) error {
	local := allLocal(remoteURLs(ctx, dir, false))
	args := transportArgs(ctx, dir, false, local)
	args = append(args, "fetch", "--no-recurse-submodules", "--no-auto-gc",
		"--upload-pack="+packProgram(false, local), "origin")
	return runTransport(ctx, dir, out, args)
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
//     origin is a local path (local; see packProgram);
//   - for a push: no signing, no push options, no mirror mode.
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
		kv = append(kv, "push.gpgSign=false", "push.pushOption=", "remote.origin.mirror=false")
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

// remoteURLs returns origin's URLs (push URLs for a push) as git resolves
// them, insteadOf rules applied.
func remoteURLs(ctx context.Context, dir string, push bool) []string {
	args := []string{"remote", "get-url", "--all"}
	if push {
		args = append(args, "--push")
	}
	out, err := run(ctx, dir, append(args, "origin")...)
	if err != nil || out == "" {
		return nil
	}
	return strings.Split(out, "\n")
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
