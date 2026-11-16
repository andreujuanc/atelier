package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"

	"github.com/andreujuanc/atelier/internal/config"
)

// envBashGuard turns the Bash guard on (any value but "", "0", "off",
// "false"), e.g. in a devcontainer's containerEnv.
const envBashGuard = "ATL_BASH_GUARD"

// BashGuardOn reports whether $ATL_BASH_GUARD turns the Bash guard on.
func BashGuardOn(env func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(env(envBashGuard))) {
	case "", "0", "off", "false", "no":
		return false
	}
	return true
}

const bashHint = "\nAllowed here: git, docker, atl and read-only tools (ls, cat, head, tail, grep, find, wc, sort, diff, jq, …). " +
	"Edit files with the Edit/Write tools; run builds and tests in the repo's containers via docker. ($" + envBashGuard + ")"

// bashGuardMain runs the Bash guard on a decoded PreToolUse input for the
// Bash tool: exit 2 with the reason on stderr when it blocks, else 0.
func bashGuardMain(p config.Paths, stderr io.Writer, env func(string) string, in Input) int {
	if !BashGuardOn(env) {
		return 0
	}
	var ti struct {
		Command *string `json:"command"`
	}
	if err := json.Unmarshal(in.ToolInput, &ti); err != nil || ti.Command == nil {
		fmt.Fprintln(stderr, "atelier: blocked: cannot read the Bash command"+bashHint)
		return 2
	}
	if err := checkBash(*ti.Command, in.Cwd, env, p.Repos); err != nil {
		fmt.Fprintln(stderr, "atelier: blocked: "+err.Error()+bashHint)
		return 2
	}
	return 0
}

// checkBash returns why command may not run in the box, or nil. Commands
// must be plain calls of allowed tools (bashTools); anything that could hide
// another command (command or process substitution, functions, variable
// assignments, arithmetic, which bash can make run code from a variable's
// value) is refused, and so is writing files by redirection. git may only
// read where it would touch $ATL_REPOS (repos, the canonical clones), and
// docker may not mount it writable. cwd is the shell's working directory.
func checkBash(command, cwd string, env func(string) string, repos string) error {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return fmt.Errorf("cannot parse the command (%v)", err)
	}
	c := &bashChecker{
		cfg:  &expand.Config{Env: expand.FuncEnviron(env), ReadDir2: os.ReadDir},
		home: env("HOME"),
		cwds: []string{cwd},
	}
	c.cfg.CmdSubst = func(w io.Writer, cs *syntax.CmdSubst) error {
		hdoc := catHeredoc(cs)
		if hdoc == nil {
			return fmt.Errorf("command substitution")
		}
		body, err := expand.Document(c.cfg, hdoc)
		if err != nil {
			return err
		}
		_, err = io.WriteString(w, body)
		return err
	}
	if repos != "" {
		c.roots = []string{filepath.Clean(repos)}
		if r := realPath(repos); r != c.roots[0] {
			c.roots = append(c.roots, r)
		}
	}
	if err := c.syntaxCheck(f); err != nil {
		return err
	}
	c.collectCds(f)
	var firstErr error
	syntax.Walk(f, func(n syntax.Node) bool {
		if firstErr != nil {
			return false
		}
		switch n := n.(type) {
		case *syntax.Stmt:
			firstErr = c.redirects(n.Redirs)
		case *syntax.CallExpr:
			firstErr = c.call(n)
		}
		return firstErr == nil
	})
	return firstErr
}

type bashChecker struct {
	cfg   *expand.Config
	home  string
	roots []string // $ATL_REPOS (and its real path)
	// cwds: every directory a command may run in: the shell's cwd plus each
	// cd target in the command ("" = unknown). Using all of them is
	// conservative: a cd that fails or runs later in a loop is covered.
	cwds []string
}

// syntaxCheck refuses constructs that can run hidden code or set variables.
func (c *bashChecker) syntaxCheck(f *syntax.File) error {
	var err error
	refuse := func(what string) { err = fmt.Errorf("%s is not allowed", what) }
	syntax.Walk(f, func(n syntax.Node) bool {
		if err != nil {
			return false
		}
		switch n := n.(type) {
		case *syntax.CmdSubst:
			if catHeredoc(n) == nil {
				refuse("command substitution ($(...) or `...`) other than $(cat <<'EOF' ... EOF)")
			}
		case *syntax.ProcSubst:
			refuse("process substitution (<(...) or >(...))")
		case *syntax.FuncDecl:
			refuse("defining functions")
		case *syntax.DeclClause:
			refuse("`" + n.Variant.Value + "`")
		case *syntax.Assign:
			refuse("setting variables (" + n.Name.Value + "=...)")
		case *syntax.CoprocClause, *syntax.TestDecl:
			refuse("this construct")
		case *syntax.Stmt:
			if n.Coprocess {
				refuse("coprocesses")
			}
		case *syntax.ArithmExp, *syntax.ArithmCmd, *syntax.LetClause, *syntax.CStyleLoop:
			refuse("shell arithmetic")
		case *syntax.ForClause:
			if n.Select {
				refuse("`select`")
			}
		case *syntax.ParamExp:
			switch {
			case n.Index != nil:
				refuse("array subscripts in ${...}")
			case n.Slice != nil:
				refuse("${var:offset} slices")
			case n.Excl && n.Names == 0:
				refuse("indirect expansion ${!var}")
			case n.Exp != nil && n.Exp.Op == syntax.OtherParamOps:
				refuse("${var@...} transformations")
			}
		case *syntax.UnaryTest:
			if n.Op == syntax.TsVarSet || n.Op == syntax.TsRefVar {
				refuse("[[ -v ]] / [[ -R ]]")
			}
		case *syntax.BinaryTest:
			switch n.Op {
			case syntax.TsEql, syntax.TsNeq, syntax.TsLeq, syntax.TsGeq, syntax.TsLss, syntax.TsGtr:
				refuse("numeric [[ ]] comparisons")
			}
		}
		return err == nil
	})
	return err
}

// catHeredoc returns the here-document of a $(cat <<EOF ... EOF) command
// substitution (Claude's usual way to pass a multi-line commit message), or
// nil for any other substitution. Its body is still checked like any word.
func catHeredoc(cs *syntax.CmdSubst) *syntax.Word {
	if len(cs.Stmts) != 1 {
		return nil
	}
	st := cs.Stmts[0]
	ce, ok := st.Cmd.(*syntax.CallExpr)
	if !ok || len(ce.Assigns) != 0 || len(ce.Args) != 1 || ce.Args[0].Lit() != "cat" ||
		st.Negated || st.Background || st.Coprocess || len(st.Redirs) != 1 {
		return nil
	}
	if r := st.Redirs[0]; (r.Op == syntax.Hdoc || r.Op == syntax.DashHdoc) && r.Hdoc != nil {
		return r.Hdoc
	}
	return nil
}

// collectCds adds every cd target in f to c.cwds.
func (c *bashChecker) collectCds(f *syntax.File) {
	syntax.Walk(f, func(n syntax.Node) bool {
		ce, ok := n.(*syntax.CallExpr)
		if !ok || len(ce.Args) == 0 || ce.Args[0].Lit() != "cd" {
			return true
		}
		args, err := expand.Fields(c.cfg, ce.Args[1:]...)
		target := c.home
		for _, a := range args {
			if a != "-L" && a != "-P" && a != "-e" && a != "-@" {
				target = a
			}
		}
		if err != nil || target == "-" || target == "" {
			c.cwds = append(c.cwds, "")
			return true
		}
		for _, d := range slices.Clone(c.cwds) {
			c.cwds = append(c.cwds, joinPath(d, target))
		}
		return true
	})
}

// joinPath resolves p against dir ("" when dir is unknown and p relative).
func joinPath(dir, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, p)
}

// protected reports whether path ("" = unknown: yes) is inside $ATL_REPOS,
// lexically or where it resolves to.
func (c *bashChecker) protected(path string) bool {
	if path == "" {
		return true
	}
	path = filepath.Clean(path)
	landing, err := resolvePath(path)
	for _, r := range c.roots {
		if _, ok := within(r, path); ok {
			return true
		}
		if _, ok := within(r, landing); err == nil && ok {
			return true
		}
	}
	return false
}

// redirects allows reading redirections, fd duplication and writing to
// /dev/null (or stdout/stderr) only.
func (c *bashChecker) redirects(rs []*syntax.Redirect) error {
	for _, r := range rs {
		switch r.Op {
		case syntax.RdrIn, syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc:
			continue
		case syntax.DplIn, syntax.DplOut:
			if w := r.Word.Lit(); w == "-" || isDigits(w) {
				continue
			}
		}
		target, err := expand.Literal(c.cfg, r.Word)
		if err == nil {
			switch target {
			case "/dev/null", "/dev/stdout", "/dev/stderr":
				continue
			}
		}
		return fmt.Errorf("writing files with %s is not allowed (use the Write/Edit tools)", r.Op)
	}
	return nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// call checks one simple command.
func (c *bashChecker) call(ce *syntax.CallExpr) error {
	if len(ce.Args) == 0 {
		return nil
	}
	name := ce.Args[0].Lit()
	if name == "" {
		return fmt.Errorf("the command name must be plain text, not quoted or built from variables")
	}
	if strings.Contains(name, "/") {
		return fmt.Errorf("`%s`: run commands by name, not by path", name)
	}
	rule, ok := bashTools[name]
	if !ok {
		return fmt.Errorf("`%s` is not allowed here", name)
	}
	args, err := expand.Fields(c.cfg, ce.Args[1:]...)
	if err != nil {
		return fmt.Errorf("`%s`: cannot expand its arguments (%v)", name, err)
	}
	if rule == nil {
		return nil
	}
	if err := rule(c, args); err != nil {
		return fmt.Errorf("`%s`: %w", name, err)
	}
	return nil
}

// bashTools maps each allowed command to its argument check (nil: any).
var bashTools = map[string]func(c *bashChecker, args []string) error{
	"sort":   noArg("writing a file (-o)", func(a string) bool { return strings.HasPrefix(a, "--output") || isShortWith(a, 'o') }),
	"find":   noArg("-exec, -ok, -delete and -fprint* (run or write)", isFindAction),
	"rg":     noArg("--pre (runs a command)", func(a string) bool { return strings.HasPrefix(a, "--pre") }),
	"tree":   noArg("-o (writes a file)", func(a string) bool { return a == "-o" }),
	"file":   noArg("-C (writes a file)", func(a string) bool { return a == "-C" || a == "--compile" }),
	"printf": noArg("-v (sets a variable)", func(a string) bool { return a == "-v" }),
	"test":   testArgs,
	"[":      testArgs,
	"uniq":   uniqArgs,
	"sed":    sedArgs,
	"git":    (*bashChecker).git,
	"docker": (*bashChecker).docker,
}

func init() {
	for _, n := range strings.Fields(`atl cd ls cat head tail grep egrep fgrep wc cut tr diff cmp comm stat
		pwd echo basename dirname realpath readlink true false which date whoami id uname hostname
		nproc du df nl tac rev fold column paste join od hexdump md5sum sha1sum sha256sum sha512sum
		base64 seq sleep jq`) {
		bashTools[n] = nil
	}
}

func noArg(what string, bad func(string) bool) func(*bashChecker, []string) error {
	return func(_ *bashChecker, args []string) error {
		for _, a := range args {
			if bad(a) {
				return fmt.Errorf("%s is not allowed", what)
			}
		}
		return nil
	}
}

// isShortWith reports whether a is a short-option cluster (-xyz) with opt.
func isShortWith(a string, opt byte) bool {
	return len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.IndexByte(a[1:], opt) >= 0
}

func isFindAction(a string) bool {
	switch a {
	case "-exec", "-execdir", "-ok", "-okdir", "-delete", "-fprint", "-fprint0", "-fprintf", "-fls":
		return true
	}
	return false
}

// testArgs refuses numeric comparisons and -v/-R: bash evaluates their
// operands as arithmetic, which can run code from a variable's value.
func testArgs(_ *bashChecker, args []string) error {
	for _, a := range args {
		switch a {
		case "-eq", "-ne", "-lt", "-le", "-gt", "-ge", "-v", "-R":
			return fmt.Errorf("%s is not allowed", a)
		}
	}
	return nil
}

// uniqArgs allows at most one file: a second one is written to.
func uniqArgs(_ *bashChecker, args []string) error {
	files := 0
	for _, a := range args {
		if !strings.HasPrefix(a, "-") && !isDigits(a) {
			files++
		}
	}
	if files > 1 {
		return fmt.Errorf("an output file is not allowed")
	}
	return nil
}

// sedPrint matches print-only sed scripts: [addr[,addr]]p[;...], addresses
// being line numbers, $ or /regex/.
var sedPrint = regexp.MustCompile(`^\s*((\d+|\$|/[^/]*/)(\s*,\s*(\d+|\$|/[^/]*/))?\s*p\s*;?\s*)+$`)

// sedArgs allows only printing lines: sed -n '<addr>p' [file...].
func sedArgs(_ *bashChecker, args []string) error {
	quiet, script := false, ""
	for _, a := range args {
		switch {
		case a == "-n" || a == "--quiet" || a == "--silent":
			quiet = true
		case a == "-E" || a == "-r" || a == "--regexp-extended":
		case strings.HasPrefix(a, "-") && a != "-":
			script = "\x00"
		case script == "":
			script = a
		}
	}
	if !quiet || !sedPrint.MatchString(script) {
		return fmt.Errorf("only printing lines (sed -n '10,20p' file) is allowed; edit files with the Edit tool")
	}
	return nil
}

// git refuses ways to run other commands (-c, config writes, rebase --exec,
// ...) and, wherever it would touch $ATL_REPOS, anything but reading.
func (c *bashChecker) git(args []string) error {
	var dirs, gitDirs []string
	i := 0
global:
	for ; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := strings.Cut(a, "=")
		switch {
		case a == "-C":
			if i+1 < len(args) {
				i++
				dirs = append(dirs, args[i])
			}
		case a == "-c" || strings.HasPrefix(a, "-c") && !strings.HasPrefix(a, "--") || name == "--config-env" || name == "--exec-path" && hasVal:
			return fmt.Errorf("%s is not allowed (it can make git run commands)", name)
		case name == "--git-dir" || name == "--work-tree":
			if hasVal {
				gitDirs = append(gitDirs, val)
			} else if i+1 < len(args) {
				i++
				gitDirs = append(gitDirs, args[i])
			}
		case a == "--namespace" || a == "--super-prefix":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			break global
		}
	}
	if i >= len(args) {
		return nil
	}
	sub, rest := args[i], args[i+1:]
	if err := gitEscape(sub, rest); err != nil {
		return err
	}
	if c.gitTouchesRepos(dirs, gitDirs, rest) && !gitReadOnly(sub, rest) {
		return fmt.Errorf("`git %s` would change a repo in $ATL_REPOS; those clones are read-only "+
			"(only reading git commands such as log, show, diff, status run there)", sub)
	}
	return nil
}

// gitEscape refuses git subcommands and options that run other programs.
func gitEscape(sub string, rest []string) error {
	switch sub {
	case "config":
		for _, a := range rest {
			switch a {
			case "--get", "--get-all", "--get-regexp", "--get-urlmatch", "-l", "--list":
				return nil
			}
		}
		return fmt.Errorf("`git config` may only read (--get, --list)")
	case "difftool", "mergetool", "filter-branch", "filter-repo":
		return fmt.Errorf("`git %s` is not allowed (it runs other programs)", sub)
	case "submodule":
		if slices.Contains(rest, "foreach") {
			return fmt.Errorf("`git submodule foreach` is not allowed")
		}
	case "bisect":
		if len(rest) > 0 && rest[0] == "run" {
			return fmt.Errorf("`git bisect run` is not allowed")
		}
	case "rebase":
		for _, a := range rest {
			if a == "--exec" || strings.HasPrefix(a, "--exec=") || isShortWith(a, 'x') {
				return fmt.Errorf("`git rebase --exec` is not allowed")
			}
		}
	case "clone":
		if slices.Contains(rest, "-u") {
			return fmt.Errorf("`git clone -u` is not allowed")
		}
	case "grep":
		for _, a := range rest {
			if strings.HasPrefix(a, "--open-files-in-pager") || strings.HasPrefix(a, "-O") {
				return fmt.Errorf("`git grep -O` is not allowed")
			}
		}
	}
	for _, a := range rest {
		if strings.HasPrefix(a, "--upload-pack") || strings.HasPrefix(a, "--receive-pack") ||
			strings.HasPrefix(a, "--exec=") || strings.Contains(a, "ext::") {
			return fmt.Errorf("%s is not allowed (it can make git run commands)", a)
		}
	}
	return nil
}

// gitTouchesRepos reports whether git would work in or name a path inside
// $ATL_REPOS, from any possible cwd.
func (c *bashChecker) gitTouchesRepos(dirs, gitDirs, rest []string) bool {
	for _, cwd := range c.cwds {
		d := cwd
		for _, x := range dirs {
			d = joinPath(d, x)
		}
		if c.protected(d) {
			return true
		}
		for _, g := range gitDirs {
			if c.protected(joinPath(d, g)) {
				return true
			}
		}
		for _, a := range rest {
			if _, v, ok := strings.Cut(a, "="); ok && strings.HasPrefix(a, "-") {
				a = v
			}
			if a != "" && c.protected(joinPath(d, a)) {
				return true
			}
		}
	}
	return false
}

var gitReadSubs = map[string]bool{}

func init() {
	for _, s := range strings.Fields(`log show diff status grep blame annotate ls-files ls-tree ls-remote
		rev-parse rev-list cat-file describe shortlog show-ref name-rev merge-base for-each-ref
		count-objects whatchanged range-diff cherry var help version check-ignore check-attr
		verify-commit verify-tag diff-tree diff-index diff-files show-branch config`) {
		gitReadSubs[s] = true
	}
}

// gitReadOnly reports whether git sub with args rest only reads.
func gitReadOnly(sub string, rest []string) bool {
	for _, a := range rest {
		if strings.HasPrefix(a, "--output") {
			return false
		}
	}
	if gitReadSubs[sub] {
		return true
	}
	first := ""
	if len(rest) > 0 {
		first = rest[0]
	}
	switch sub {
	case "branch", "tag":
		list := false
		for _, a := range rest {
			switch {
			case a == "--list" || a == "-l" || a == "--show-current":
				list = true
			case slices.Contains([]string{"-d", "-D", "-m", "-M", "-c", "-C", "-f", "-u", "-s", "-e", "-F"}, a),
				strings.HasPrefix(a, "--delete"), strings.HasPrefix(a, "--move"), strings.HasPrefix(a, "--copy"),
				strings.HasPrefix(a, "--force"), strings.HasPrefix(a, "--set-upstream"), strings.HasPrefix(a, "--unset-upstream"),
				strings.HasPrefix(a, "--edit-description"), strings.HasPrefix(a, "--sign"), strings.HasPrefix(a, "--annotate"):
				return false
			}
		}
		if list {
			return true
		}
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") {
				return false // a name: create
			}
		}
		return sub == "branch" || len(rest) == 0
	case "remote":
		return first == "" || first == "-v" || first == "--verbose" || first == "show" || first == "get-url"
	case "worktree":
		return first == "list"
	case "stash":
		return first == "list" || first == "show"
	case "reflog":
		return first != "expire" && first != "delete"
	}
	return false
}

// docker refuses privileged and host-namespace options, writable mounts of
// $ATL_REPOS (or a directory containing it) and any mount of credentials,
// and copying or exporting into $ATL_REPOS.
func (c *bashChecker) docker(args []string) error {
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := strings.Cut(a, "=")
		value := func() string {
			if hasVal {
				return val
			}
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case name == "--privileged":
			if !hasVal || val != "false" {
				return fmt.Errorf("--privileged is not allowed")
			}
		case name == "--cap-add" || name == "--device" || name == "--device-cgroup-rule":
			return fmt.Errorf("%s is not allowed", name)
		case name == "--security-opt":
			v := value()
			if v != "label=disable" && v != "label:disable" && !strings.HasPrefix(v, "no-new-privileges") {
				return fmt.Errorf("--security-opt %s is not allowed", v)
			}
		case slices.Contains([]string{"--network", "--net", "--pid", "--ipc", "--uts", "--userns", "--cgroupns"}, name):
			if v := value(); v == "host" {
				return fmt.Errorf("%s host is not allowed", name)
			}
		case a == "-v" || name == "--volume":
			if err := c.dockerVolume(value()); err != nil {
				return err
			}
		case strings.HasPrefix(a, "-v") && !strings.HasPrefix(a, "--"):
			if err := c.dockerVolume(a[2:]); err != nil {
				return err
			}
		case name == "--mount":
			if err := c.dockerMount(value()); err != nil {
				return err
			}
		case a == "-o" || name == "--output":
			v := value()
			for _, kv := range strings.Split(v, ",") {
				if k, p, ok := strings.Cut(kv, "="); ok && k == "dest" {
					v = p
				}
			}
			if c.anyProtected(v) {
				return fmt.Errorf("writing into $ATL_REPOS (%s) is not allowed", v)
			}
		case !strings.HasPrefix(a, "-"):
			pos = append(pos, a)
		}
	}
	if len(pos) >= 3 && (pos[0] == "cp" || pos[0] == "compose" && slices.Contains(pos, "cp")) {
		if dest := pos[len(pos)-1]; !strings.Contains(dest, ":") && c.anyProtected(dest) {
			return fmt.Errorf("copying into $ATL_REPOS (%s) is not allowed", dest)
		}
	}
	return nil
}

// anyProtected reports whether p, resolved against any possible cwd, is
// inside $ATL_REPOS.
func (c *bashChecker) anyProtected(p string) bool {
	if p == "" {
		return false
	}
	for _, d := range c.cwds {
		if c.protected(joinPath(d, p)) {
			return true
		}
	}
	return false
}

// dockerVolume checks a -v src:dst[:opts] bind mount (named volumes pass).
func (c *bashChecker) dockerVolume(v string) error {
	parts := strings.Split(v, ":")
	ro := len(parts) >= 3 && slices.ContainsFunc(strings.Split(parts[2], ","), func(o string) bool {
		return o == "ro" || o == "readonly"
	})
	return c.bindSource(parts[0], ro)
}

// dockerMount checks a --mount spec; only type=bind mounts host paths.
func (c *bashChecker) dockerMount(v string) error {
	var typ, src string
	ro := false
	for _, kv := range strings.Split(v, ",") {
		k, val, hasVal := strings.Cut(kv, "=")
		switch k {
		case "type":
			typ = val
		case "source", "src":
			src = val
		case "readonly", "ro":
			ro = !hasVal || val == "true" || val == "1"
		}
	}
	if typ != "bind" {
		return nil
	}
	return c.bindSource(src, ro)
}

// credentialDirs are home paths never to be mounted into a container.
var credentialDirs = []string{".ssh", ".claude", ".gitconfig", ".config/gh", ".docker", ".aws", ".azure",
	".kube", ".gnupg", ".config/gcloud", ".git-credentials", ".netrc"}

// bindSource checks a bind-mount source path: no credentials (or a
// directory holding them), and $ATL_REPOS (or a directory holding it) only
// read-only.
func (c *bashChecker) bindSource(src string, ro bool) error {
	if src == "" || !strings.ContainsAny(src, "/~.") {
		return nil // a named volume
	}
	for _, d := range c.cwds {
		p := joinPath(d, src)
		if p == "" {
			return fmt.Errorf("cannot tell where mount %s points", src)
		}
		if c.home != "" {
			for _, cred := range credentialDirs {
				cp := filepath.Join(c.home, cred)
				if _, in := within(p, cp); in {
					return fmt.Errorf("mounting %s exposes ~/%s; not allowed", src, cred)
				}
				if _, in := within(cp, p); in {
					return fmt.Errorf("mounting ~/%s is not allowed", cred)
				}
			}
		}
		if ro {
			continue
		}
		if c.protected(p) {
			return fmt.Errorf("mount %s is in $ATL_REPOS: add :ro (read-only)", src)
		}
		for _, r := range c.roots {
			if _, in := within(p, r); in {
				return fmt.Errorf("mount %s contains $ATL_REPOS: add :ro (read-only)", src)
			}
		}
	}
	return nil
}
