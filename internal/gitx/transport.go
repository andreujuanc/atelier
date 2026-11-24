package gitx

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/andreujuanc/atelier/internal/nofollow"
)

// Origin is where a repo's origin points, as its config says it: the values
// of remote.origin.url and remote.origin.pushurl, before any
// url.<base>.insteadOf rule. atl pins it once per repo and pushes and
// fetches there, whatever the repo's config says later; the user's own
// rules (system and global config) apply to it then, once, as they would to
// the repo's origin.
type Origin struct {
	URLs     []string `json:"urls"`
	PushURLs []string `json:"push_urls,omitempty"` // none: pushes go to URLs
}

// Equal reports whether o and p have the same URLs, in the same order.
func (o Origin) Equal(p Origin) bool {
	return slices.Equal(o.URLs, p.URLs) && slices.Equal(o.PushURLs, p.PushURLs)
}

// String is the URLs, followed by the push URLs if there are any.
func (o Origin) String() string {
	s := strings.Join(o.URLs, ", ")
	if s == "" {
		s = "none"
	}
	if len(o.PushURLs) > 0 {
		s += " (push: " + strings.Join(o.PushURLs, ", ") + ")"
	}
	return s
}

// Valid reports whether o has a URL.
func (o Origin) Valid() bool {
	return len(o.URLs) > 0 && !slices.Contains(o.URLs, "") && !slices.Contains(o.PushURLs, "")
}

// fetchURLs is the URL a fetch from o goes to, the user's rules applied.
func (o Origin) fetchURLs(user configEntries) []string {
	if len(o.URLs) == 0 {
		return nil
	}
	return []string{user.rewrite(o.URLs[0], false)}
}

// pushURLs are the URLs a push to o goes to, the user's rules applied as
// git applies them: insteadOf to explicit push URLs; else pushInsteadOf to
// the URLs (those it rewrites), else insteadOf.
func (o Origin) pushURLs(user configEntries) []string {
	var out []string
	if len(o.PushURLs) > 0 {
		for _, u := range o.PushURLs {
			out = append(out, user.rewrite(u, false))
		}
		return out
	}
	for _, u := range o.URLs {
		if r := user.rewrite(u, true); r != u {
			out = append(out, r)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, u := range o.URLs {
		out = append(out, user.rewrite(u, false))
	}
	return out
}

// CurrentOrigin returns origin's URLs as the config of the repo at dir has
// them now; ok is false when the repo has no origin URL.
func CurrentOrigin(ctx context.Context, dir string) (o Origin, ok bool, err error) {
	if o.URLs, err = configList(ctx, dir, "remote.origin.url"); err != nil || len(o.URLs) == 0 {
		return Origin{}, false, err
	}
	if o.PushURLs, err = configList(ctx, dir, "remote.origin.pushurl"); err != nil {
		return Origin{}, false, err
	}
	return o, true, nil
}

// configList returns the values of the multi-valued key in the repo at dir,
// as git reads the list: an empty value clears it.
func configList(ctx context.Context, dir, key string) ([]string, error) {
	out, err := run(ctx, dir, "config", "--null", "--get-all", key)
	if err != nil {
		var ge *Error
		if errors.As(err, &ge) && ge.ExitCode == 1 && ge.Stderr == "" {
			return nil, nil
		}
		return nil, err
	}
	var list []string
	for _, v := range strings.Split(strings.TrimSuffix(out, "\x00"), "\x00") {
		if v == "" {
			list = nil
		} else {
			list = append(list, v)
		}
	}
	return list, nil
}

// PushBranch pushes the local branch of the repo at dir to the same branch
// at origin's push URLs, and nothing else (no tags, no submodules); force
// means --force-with-lease. git's output goes to out as it comes.
//
// It runs git in a repo whose config and hooks may have been written by a
// sandboxed task, with the credentials of the place atl runs in, so it keeps
// git from running anything that config names (see transportArgs), and the
// branch goes to origin's URLs, never to what the repo's config says (see
// newTransport).
//
// The commit pushed is the one the branch's ref names, read here
// (branchCommit), not by git: git follows a ref file (or packed-refs, or a
// directory on the way) that is a symlink, and the task could make it one
// to a branch of any other repo where atl runs, and list that repo's
// objects as alternates: git would push that repo's history to origin,
// where the task fetches it.
func PushBranch(ctx context.Context, dir string, origin Origin, branch string, force bool, out io.Writer) error {
	t, err := newTransport(ctx, dir, origin, true)
	if err != nil {
		return err
	}
	ref := "refs/heads/" + branch
	commit, err := branchCommit(ctx, dir, ref)
	if err != nil {
		return err
	}
	args := append(t.args, "push", "--no-follow-tags", "--recurse-submodules=no")
	if force {
		args = append(args, "--force-with-lease")
	}
	args = append(args, "--receive-pack="+packProgram(true, t.local), t.name, commit+":"+ref)
	return runTransport(ctx, dir, out, args)
}

// branchCommit returns the commit id ref (a branch, refs/heads/...) names
// in the repo at dir: its loose ref file in the repo's common git dir, else
// its line in packed-refs, both read without following a symlink (see
// package nofollow). A symbolic ref is refused.
func branchCommit(ctx context.Context, dir, ref string) (string, error) {
	common, err := revParsePath(ctx, dir, "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	d, err := nofollow.Open(common)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", ref, err)
	}
	defer d.Close()
	if reftable, err := usesReftable(ctx, dir); err != nil {
		return "", err
	} else if reftable {
		return reftableCommit(ctx, dir, d, ref)
	}
	data, err := d.ReadFile(ref, 1024)
	if err == nil {
		id := strings.TrimSuffix(string(data), "\n")
		if !objectID.MatchString(id) {
			return "", fmt.Errorf("read %s: not a commit id: %q", ref, firstChars(id, 80))
		}
		return id, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("read %s: %w", ref, err)
	}
	f, err := d.OpenFile("packed-refs")
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("no branch %s", ref)
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", ref, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 64<<10)
	for sc.Scan() {
		if id, name, ok := strings.Cut(sc.Text(), " "); ok && name == ref && objectID.MatchString(id) {
			return id, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read %s: packed-refs: %w", ref, err)
	}
	return "", fmt.Errorf("no branch %s", ref)
}

// objectID matches a full object id, SHA-1 or SHA-256.
var objectID = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

func firstChars(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// FetchOrigin is `git fetch origin` in the repo at dir, from origin's URL,
// with the same guards as PushBranch. git's output goes to out. Like `git
// fetch` since git 2.48, it creates refs/remotes/origin/HEAD when it is
// missing (see setOriginHead).
func FetchOrigin(ctx context.Context, dir string, origin Origin, out io.Writer) error {
	t, err := newTransport(ctx, dir, origin, false)
	if err != nil {
		return err
	}
	args := append(t.args, "fetch", "--no-recurse-submodules", "--no-auto-gc",
		"--upload-pack="+packProgram(false, t.local), t.name)
	if err := runTransport(ctx, dir, out, args); err != nil {
		return err
	}
	t.setOriginHead(ctx, dir)
	return nil
}

// lsRemoteArgs are the arguments of `git ls-remote <flags> <origin>` with
// the guards of FetchOrigin; the caller appends patterns.
func lsRemoteArgs(ctx context.Context, dir string, origin Origin, flags ...string) ([]string, error) {
	t, err := newTransport(ctx, dir, origin, false)
	if err != nil {
		return nil, err
	}
	return t.lsRemote(flags...), nil
}

// transport is what a push to or fetch from a pinned origin runs with.
type transport struct {
	name  string   // of the stand-in remote
	args  []string // -c options: guards, then the stand-in remote
	local bool     // the URLs it goes to are paths on this machine
}

func (t *transport) lsRemote(flags ...string) []string {
	args := append(slices.Clone(t.args), "ls-remote")
	args = append(args, flags...)
	return append(args, "--upload-pack="+packProgram(false, t.local), t.name)
}

// newTransport defines, with -c options only, a remote that stands in for
// origin. Its URLs are origin's (not the repo's remote.origin.*), and its
// fetch refspecs are the repo's own remote.origin.fetch, so that a push
// still updates refs/remotes/origin/<b> and --force-with-lease leases
// against it, and a single-branch clone fetches what it did. A refspec the
// task wrote can only touch local refs, which the task can write anyway.
//
// The name is random: remote.<name>.url and pushurl are lists, so a remote
// the repo's config could name in advance would get its URLs too.
//
// -c cannot remove url.<base>.insteadOf or pushInsteadOf rules, which apply
// to these URLs as to any, nor http.<url>.* settings; so it is refused while
// the repo's own config has a rule, or http.* settings and origin is http(s),
// or an include (see Hazards). The check reads the config with the same -c
// options as the command. The user's own rules (system, global) stay and
// apply once: the pin is what the config said, before them.
func newTransport(ctx context.Context, dir string, origin Origin, push bool) (*transport, error) {
	if !origin.Valid() {
		return nil, errors.New("no origin URL")
	}
	user := userConfig(ctx, dir)
	t := &transport{}
	if push {
		t.local = allLocal(origin.pushURLs(user))
	} else {
		t.local = allLocal(origin.fetchURLs(user))
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	t.name = "atl-" + hex.EncodeToString(b[:])
	kv := []string{"followRemoteHEAD=never"}
	for _, u := range origin.URLs {
		kv = append(kv, "url="+u)
	}
	if push {
		for _, u := range origin.PushURLs {
			kv = append(kv, "pushurl="+u)
		}
	}
	refspecs, err := configList(ctx, dir, "remote.origin.fetch")
	if err != nil {
		return nil, err
	}
	if len(refspecs) == 0 {
		refspecs = []string{"+refs/heads/*:refs/remotes/origin/*"}
	}
	for _, r := range refspecs {
		kv = append(kv, "fetch="+r)
	}
	t.args = transportArgs(push, t.local, user)
	for _, s := range kv {
		t.args = append(t.args, "-c", "remote."+t.name+"."+s)
	}
	if err := checkHazards(ctx, dir, opTransport, &origin, t.args); err != nil {
		return nil, err
	}
	return t, nil
}

// setOriginHead points refs/remotes/origin/HEAD at origin's default branch
// when it is missing and that branch was fetched, as `git fetch` does since
// git 2.48 (remote.origin.followRemoteHEAD=create; "never" in the repo's
// config is respected). The stand-in remote cannot do it: git names the ref
// after the remote. Best effort: failures are ignored.
func (t *transport) setOriginHead(ctx context.Context, dir string) {
	const head = "refs/remotes/origin/HEAD"
	if _, err := run(ctx, dir, "symbolic-ref", "--quiet", head); err == nil {
		return
	}
	if ok, err := RefExists(ctx, dir, head); err != nil || ok {
		return // a detached origin/HEAD is the user's
	}
	if strings.EqualFold(ConfigValue(ctx, dir, "remote.origin.followRemoteHEAD"), "never") {
		return
	}
	out, err := runTransportOutput(ctx, dir, append(t.lsRemote("--symref"), "HEAD"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(out, "\n") {
		target, ok := strings.CutPrefix(line, "ref: refs/heads/")
		if !ok {
			continue
		}
		branch, ok := strings.CutSuffix(target, "\tHEAD")
		if !ok || branch == "" {
			continue
		}
		if ok, err := RefExists(ctx, dir, "refs/remotes/origin/"+branch); err == nil && ok {
			_, _ = run(ctx, dir, "symbolic-ref", "-m", "atl fetch: origin's HEAD", head, "refs/remotes/origin/"+branch)
		}
		return
	}
}

// transportArgs are the -c options of a push or fetch, on top of
// safetyArgs. Options given with -c outrank every config file, so the
// repo's own config cannot undo them:
//
//   - no core.alternateRefsCommand, which would run a program;
//   - the user's own credential helpers, askpass and ssh command (see
//     userTransportKV);
//   - no ext:: transport (it runs a command), and no file transport unless
//     origin's URLs are local paths (local; see packProgram);
//   - no bundle URI (fetch.bundleURI would download from where it says);
//   - for a push: no signing, no push options.
//
// core.gitProxy cannot be undone with -c (git takes its first value), so
// runTransport sets $GIT_PROXY_COMMAND, which comes before it.
func transportArgs(push, local bool, user configEntries) []string {
	kv := []string{
		"core.alternateRefsCommand=true",
		"protocol.ext.allow=never",
		"fetch.bundleURI=",
	}
	kv = append(kv, userTransportKV(user)...)
	if !local {
		kv = append(kv, "protocol.file.allow=never")
	}
	if push {
		kv = append(kv, "push.gpgSign=false", "push.pushOption=")
	}
	return cOptions(kv)
}

// userTransportKV keeps the settings that run a program to authenticate
// to the user's own (system and global config): no credential helpers but
// the user's, no askpass program but the user's, and ssh as the user runs
// it ($GIT_SSH_COMMAND, else their core.sshCommand, else $GIT_SSH or ssh).
func userTransportKV(user configEntries) []string {
	kv := []string{"credential.helper="} // an empty value drops the helpers configured so far
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
	return kv
}

// userTransportArgs are userTransportKV's -c options for the repo at dir.
func userTransportArgs(ctx context.Context, dir string) []string {
	return cOptions(userTransportKV(userConfig(ctx, dir)))
}

func cOptions(kv []string) []string {
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
	return localPackPrefix + name
}

// localPackPrefix is safetyArgs as a command line for sh, and what a local
// receive-pack or upload-pack runs on its own.
var localPackPrefix = func() string {
	var b strings.Builder
	b.WriteString("git")
	for _, a := range slices.Concat(safetyArgs, cOptions([]string{"core.alternateRefsCommand=true", "receive.autogc=false"})) {
		b.WriteString(" " + shellQuote(a))
	}
	return b.String() + " "
}()

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

// isHTTPURL reports whether git reaches url over http(s), or through a
// remote helper (<helper>::<address>), which may.
func isHTTPURL(u string) bool {
	l := strings.ToLower(u)
	if strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") {
		return true
	}
	helper, scheme := strings.Index(l, "::"), strings.Index(l, "://")
	return helper > 0 && (scheme < 0 || helper < scheme)
}

// GitHubRepo returns "OWNER/REPO" (or "HOST/OWNER/REPO" for a host other
// than github.com), what `gh --repo` takes, for the GitHub repository at
// origin's fetch URL, with the user's rules applied (a shorthand like
// gh:o/r), else as it is; ok is false when neither looks like one. An ssh
// URL's host that is not github.com may be an alias in the user's ssh
// config (git@github-work:o/r, for a second account): when `ssh -G` says
// its real host name is github.com, the repository is on github.com.
func GitHubRepo(ctx context.Context, dir string, origin Origin) (repo string, ok bool) {
	for _, u := range append(origin.fetchURLs(userConfig(ctx, dir)), origin.URLs...) {
		host, nwo, ssh, ok := githubRepo(u)
		if !ok {
			continue
		}
		if strings.EqualFold(host, "github.com") || ssh && strings.EqualFold(sshHostname(ctx, host), "github.com") {
			return nwo, true
		}
		return host + "/" + nwo, true
	}
	return "", false
}

// githubRepo splits a URL into its host and OWNER/REPO path; ssh is true
// for an ssh URL (ssh://, or scp-like [user@]host:path).
func githubRepo(raw string) (host, nwo string, ssh, ok bool) {
	var path string
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", "", false, false
		}
		host, path, ssh = u.Hostname(), u.Path, strings.EqualFold(u.Scheme, "ssh")
	} else if !isLocalURL(raw) { // scp-like: [user@]host:path
		h, p, _ := strings.Cut(raw, ":")
		if i := strings.LastIndexByte(h, '@'); i >= 0 {
			h = h[i+1:]
		}
		host, path, ssh = h, p, true
	} else {
		return "", "", false, false
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git"), "/"), "/")
	if host == "" || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false, false
	}
	return host, parts[0] + "/" + parts[1], ssh, true
}

// sshHost is what an ssh host alias may look like; anything else (an
// option, say) never reaches ssh's command line.
var sshHost = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// sshHostname returns the host name the user's ssh configuration gives
// host (`ssh -G`, which only prints the configuration, connecting nowhere),
// or "" if ssh cannot tell. ssh reads the user's own configuration only:
// it runs outside any repo, with nothing of the repo's config.
func sshHostname(ctx context.Context, host string) string {
	if !sshHost.MatchString(host) {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", "-G", "--", host)
	cmd.Dir = "/"
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "hostname "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
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

// rewrite applies the url.<base>.insteadOf rules among c (or pushInsteadOf,
// if push) to u as git does: the longest matching prefix wins. It returns u
// itself when none matches.
func (c configEntries) rewrite(u string, push bool) string {
	suffix := ".insteadof"
	if push {
		suffix = ".pushinsteadof"
	}
	best, base := "", ""
	for _, e := range c {
		b, ok := strings.CutSuffix(e.key, suffix)
		if !ok || !strings.HasPrefix(b, "url.") {
			continue
		}
		if strings.HasPrefix(u, e.value) && len(e.value) > len(best) {
			best, base = e.value, strings.TrimPrefix(b, "url.")
		}
	}
	if best == "" {
		return u
	}
	return base + strings.TrimPrefix(u, best)
}

// userConfig returns the user's own transport settings: those of the
// system and global config files (never the repo's own), in the order git
// reads them, as they apply to the repo at dir (includeIf). Section and
// variable names come lower case; a url.<base>.* key keeps its base as it
// is written.
func userConfig(ctx context.Context, dir string) configEntries {
	const pattern = `^(core\.(sshcommand|askpass)|credential\.(.+\.)?helper|url\..+\.(insteadof|pushinsteadof))$`
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

// newTransportCommand is command for a git that talks to a remote.
// core.gitProxy cannot be undone with -c, but $GIT_PROXY_COMMAND comes
// before it: empty unless the user set it.
func newTransportCommand(ctx context.Context, dir string, args []string) *exec.Cmd {
	cmd := newCommand(ctx, dir, true, args)
	if os.Getenv("GIT_PROXY_COMMAND") == "" {
		cmd.Env = append(cmd.Env, "GIT_PROXY_COMMAND=")
	}
	return cmd
}

// runTransport runs git with args in dir, its stdout and stderr going to
// out. A failure's Error carries the line that says what failed (see
// failureLine; all of git's output went to out already).
func runTransport(ctx context.Context, dir string, out io.Writer, args []string) error {
	if out == nil {
		out = io.Discard
	}
	cmd := newTransportCommand(ctx, dir, args)
	// One writer for both: exec then never calls it from two goroutines.
	w := &teeWriter{w: out}
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Run(); err != nil {
		return wrap(ctx, args, failureLine(w.buf.String()), err)
	}
	return nil
}

// runTransportOutput runs git with args in dir and returns its trimmed
// stdout.
func runTransportOutput(ctx context.Context, dir string, args []string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := newTransportCommand(ctx, dir, args)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", wrap(ctx, args, stderr.String(), err)
	}
	return strings.TrimSpace(stdout.String()), nil
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

// failureLine is the line of git's output s that says what failed: a
// rejected ref (" ! [rejected] …"), else git's first "fatal:" or "error:"
// line, else the last line. git ends with advice (hint: lines, "Please make
// sure you have the correct access rights…") that does not say it.
func failureLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	for _, prefixes := range [][]string{{"! "}, {"fatal: ", "error: "}} {
		for _, l := range lines {
			for _, p := range prefixes {
				if strings.HasPrefix(l, p) {
					return l
				}
			}
		}
	}
	return lines[len(lines)-1]
}

// reftableName matches the name of a table of a reftable stack.
var reftableName = regexp.MustCompile(`^0x[0-9a-f]{12,16}-0x[0-9a-f]{12,16}-[0-9a-f]{8}\.(ref|log)$`)

// reftableCommit is branchCommit for a repo whose refs are in reftable,
// common the common git dir of the repo at dir. git follows a symlink in place of the
// reftable dir or one of its tables (the task could point one at another
// repo's), so the stack is copied, read without following symlinks, into
// a temporary git dir of atl's, and git reads ref there.
func reftableCommit(ctx context.Context, dir string, common *nofollow.Dir, ref string) (string, error) {
	fail := func(err error) (string, error) { return "", fmt.Errorf("read %s: %w", ref, err) }
	objFormat, err := run(ctx, dir, "rev-parse", "--show-object-format")
	if err != nil {
		return "", err
	}
	list, err := common.ReadFile("reftable/tables.list", 1<<20)
	if err != nil {
		return fail(err)
	}
	priv, err := os.MkdirTemp("", "atl-refs-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(priv)
	if _, err := run(ctx, os.TempDir(), "init", "--bare", "--quiet", "--template=", "--ref-format=reftable", "--object-format="+objFormat, priv); err != nil {
		return "", err
	}
	stack := filepath.Join(priv, "reftable")
	if err := os.RemoveAll(stack); err != nil {
		return fail(err)
	}
	if err := os.Mkdir(stack, 0o700); err != nil {
		return fail(err)
	}
	for _, name := range strings.Fields(string(list)) {
		if !reftableName.MatchString(name) {
			return fail(fmt.Errorf("unexpected table %q in reftable/tables.list", firstChars(name, 80)))
		}
		if err := copyOut(common, "reftable/"+name, filepath.Join(stack, name)); err != nil {
			return fail(err)
		}
	}
	if err := os.WriteFile(filepath.Join(stack, "tables.list"), list, 0o600); err != nil {
		return fail(err)
	}
	out, err := run(ctx, priv, "for-each-ref", "--format=%(refname)%00%(symref)%00%(objectname)", ref)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\x00")
		if len(f) != 3 || f[0] != ref {
			continue
		}
		if f[1] != "" {
			return fail(errors.New("is a symbolic ref"))
		}
		if !objectID.MatchString(f[2]) {
			return fail(fmt.Errorf("not a commit id: %q", firstChars(f[2], 80)))
		}
		return f[2], nil
	}
	return "", fmt.Errorf("no branch %s", ref)
}

// copyOut copies the regular file rel of d (not through a symlink) to the
// new file dst.
func copyOut(d *nofollow.Dir, rel, dst string) error {
	in, err := d.OpenFile(rel)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
