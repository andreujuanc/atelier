package gitx

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// op is a kind of git command that a setting in a repo's own config could
// make run a program, or contact a server, of that config's choosing.
type op int

const (
	// opDiff reads worktree files through the clean filters (Diff).
	opDiff op = 1 << iota
	// opCheckout writes files through the smudge filters, and git-lfs
	// downloads (AddWorktree).
	opCheckout
	// opTransport talks to origin (PushBranch, FetchOrigin,
	// RemoteBranchExists).
	opTransport
)

// Hazard is a setting in a repo's own config (scopes local and worktree,
// includes followed) that atl refuses to run some git commands with: one
// that -c cannot override, because its name is the config's to choose
// (filter.<name>.clean) or because it is a list (url.<base>.insteadOf).
type Hazard struct {
	Scope, Key, Value string
	Why               string // what it would do, after "which"
	ops               op
}

// Blocks names the atl commands the setting blocks.
func (h Hazard) Blocks() string {
	var s []string
	if h.ops&opDiff != 0 {
		s = append(s, "atl diff")
	}
	if h.ops&opCheckout != 0 {
		s = append(s, "atl new", "atl add")
	}
	if h.ops&opTransport != 0 {
		s = append(s, "atl push", "atl fetch", "atl pr", "the fetch of atl new/add")
	}
	return strings.Join(s, ", ")
}

func (h Hazard) String() string {
	return fmt.Sprintf("its own config (%s) has %s = %s, which %s", h.Scope, h.Key, h.Value, h.Why)
}

// RefusedError is returned instead of running git in Dir while its config
// has the Hazard.
type RefusedError struct {
	Dir string
	Hazard
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("refused: the config of %s (%s) has %s = %s, which %s; remove it and try again",
		e.Dir, e.Scope, e.Key, e.Value, e.Why)
}

// hazardPattern matches every key hazards looks at, in any scope.
const hazardPattern = `^(filter\..+\.(clean|smudge|process)|lfs\..+|remote\.lfs(push)?default|remote\..+\.lfs(push)?url|http\..+|url\..+\.(insteadof|pushinsteadof)|include\.path|includeif\..+\.path)$`

// includeKey matches the keys that include another config file. git lists
// them as entries of the file that has them, whether or not their condition
// holds at the time.
var includeKey = regexp.MustCompile(`^(include\.path|includeif\..+\.path)$`)

// lfsDefaults are the filter commands `git lfs install` writes (with and
// without --skip-smudge): the one filter let through.
var lfsDefaults = map[string][]string{
	"filter.lfs.clean":   {"git-lfs clean -- %f"},
	"filter.lfs.smudge":  {"git-lfs smudge -- %f", "git-lfs smudge --skip -- %f"},
	"filter.lfs.process": {"git-lfs filter-process", "git-lfs filter-process --skip"},
}

var (
	filterKey  = regexp.MustCompile(`^filter\.(.+)\.(clean|smudge|process)$`)
	lfsSafeKey = regexp.MustCompile(`^lfs\.(repositoryformatversion|.+\.access|.+\.locksverify)$`)
	lfsExecKey = regexp.MustCompile(`^lfs\.(extension\..+|customtransfer\..+|standalonetransferagent)$`)
	lfsURLKey  = regexp.MustCompile(`^(remote\.lfs(push)?default|remote\..+\.lfs(push)?url)$`)
	rewriteKey = regexp.MustCompile(`^url\..+\.(insteadof|pushinsteadof)$`)
)

// Hazards lists the settings of the repo at dir that make atl refuse a git
// command now. With origin (the repo's pinned origin), the repo's own http.*
// settings count when origin's URLs are http(s), and with git-lfs on, a
// current origin that differs from it.
//
// An include (include.path, includeIf.<condition>.path) in the repo's own
// config blocks every command that is checked: what an include brings in
// can depend on what differs between this check and the command (the
// remotes the command defines on its command line, the branch checked out,
// the git dir), so the check cannot know what the command will read.
func Hazards(ctx context.Context, dir string, origin *Origin) ([]Hazard, error) {
	return hazards(ctx, dir, origin, nil)
}

// hazards is Hazards with the scan run under cfgArgs, the -c options of the
// command it checks for, so that it reads the config as that command does.
func hazards(ctx context.Context, dir string, origin *Origin, cfgArgs []string) ([]Hazard, error) {
	entries, err := scanConfig(ctx, dir, hazardPattern, cfgArgs)
	if err != nil {
		return nil, err
	}
	lfs := false
	for _, e := range entries {
		if m := filterKey.FindStringSubmatch(e.key); m != nil && m[1] == "lfs" && e.value != "" {
			lfs = true
		}
	}
	httpOrigin := false
	if origin != nil {
		user := userConfig(ctx, dir)
		for _, u := range append(origin.fetchURLs(user), origin.pushURLs(user)...) {
			httpOrigin = httpOrigin || isHTTPURL(u)
		}
	}
	var out []Hazard
	add := func(e scopedEntry, ops op, why string) {
		if ops != 0 {
			out = append(out, Hazard{Scope: e.scope, Key: e.key, Value: e.value, Why: why, ops: ops})
		}
	}
	ifLFS := func(o op) op {
		if lfs {
			return o
		}
		return 0
	}
	for _, e := range entries {
		if e.scope != "local" && e.scope != "worktree" {
			continue
		}
		switch {
		case includeKey.MatchString(e.key):
			add(e, opDiff|opCheckout|opTransport, "includes another config file, whose settings this check cannot vouch for")
		case filterKey.MatchString(e.key):
			if e.value != "" && !slices.Contains(lfsDefaults[e.key], e.value) {
				add(e, opDiff|opCheckout, "git runs on file contents")
			}
		case lfsSafeKey.MatchString(e.key):
		case lfsExecKey.MatchString(e.key):
			add(e, ifLFS(opDiff|opCheckout), "git-lfs runs on file contents")
		case strings.HasPrefix(e.key, "lfs.") || lfsURLKey.MatchString(e.key):
			add(e, ifLFS(opCheckout), "would steer git-lfs's downloads")
		case strings.HasPrefix(e.key, "http."):
			ops := ifLFS(opCheckout)
			if httpOrigin {
				ops |= opTransport
			}
			add(e, ops, "would steer https connections (and their credentials)")
		case rewriteKey.MatchString(e.key):
			add(e, opTransport|ifLFS(opCheckout), "would rewrite origin's URL")
		}
	}
	if lfs && origin != nil {
		now, ok, err := CurrentOrigin(ctx, dir)
		if err != nil {
			return nil, err
		}
		if !ok || !now.Equal(*origin) {
			out = append(out, Hazard{Scope: "local", Key: "remote.origin.url", Value: now.String(), ops: opCheckout,
				Why: "git-lfs would download from, not from the pinned origin " + origin.String()})
		}
	}
	return out, nil
}

// checkHazards refuses (*RefusedError) when the repo at dir has a Hazard for
// a command of kind o, which runs with the -c options cfgArgs (on top of
// safetyArgs).
func checkHazards(ctx context.Context, dir string, o op, origin *Origin, cfgArgs []string) error {
	hs, err := hazards(ctx, dir, origin, cfgArgs)
	if err != nil {
		return err
	}
	for _, h := range hs {
		if h.ops&o != 0 {
			return &RefusedError{Dir: dir, Hazard: h}
		}
	}
	return nil
}

type scopedEntry struct{ scope, key, value string }

// scanConfig returns the config entries of the repo at dir whose key
// matches pattern, in every scope, includes followed, in the order git
// reads them, as git reads them with the -c options cfgArgs (a conditional
// include may depend on them, e.g. hasconfig:remote.*.url). Section and
// variable names come lower case. An included entry has the scope of the
// file that includes it.
func scanConfig(ctx context.Context, dir, pattern string, cfgArgs []string) ([]scopedEntry, error) {
	args := append(slices.Clone(cfgArgs), "config", "--show-scope", "--includes", "--null", "--get-regexp", pattern)
	data, err := run(ctx, dir, args...)
	if err != nil {
		var ge *Error
		if errors.As(err, &ge) && ge.ExitCode == 1 && ge.Stderr == "" {
			return nil, nil // none
		}
		return nil, err
	}
	// Records are scope NUL key LF value NUL.
	var out []scopedEntry
	f := strings.Split(data, "\x00")
	for i := 0; i+1 < len(f); i += 2 {
		key, value, _ := strings.Cut(f[i+1], "\n")
		out = append(out, scopedEntry{f[i], key, value})
	}
	return out, nil
}
