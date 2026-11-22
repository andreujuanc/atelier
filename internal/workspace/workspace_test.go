package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// env clears every variable a workspace sets or reads, and points HOME and
// the root at temp dirs; it returns the root.
func env(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(home, "ws")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv(RootVar, root)
	for _, k := range []string{Var, VarsVar, "ATL_REPOS", "GIT_CONFIG_GLOBAL", "ATL_CONTEXT", "ATL_TMUX_SOCKET",
		"ATL_SANDBOX_IMAGE", "ATL_SANDBOX_MOUNTS", "ATL_SANDBOX_CONTEXT"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Chdir(home)
	return root
}

// mk makes workspace name under root with atelier.toml content toml.
func mk(t *testing.T, root, name, toml string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, File), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCheckName(t *testing.T) {
	for _, ok := range []string{"alpha", "beta", "a", "0", "a-b_c", "x1234567890123456789012345678901"} {
		if err := CheckName(ok); err != nil {
			t.Errorf("CheckName(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-a", "_a", "A", "a.b", "a/b", "..", ".", "a b", "x12345678901234567890123456789012", "é"} {
		if CheckName(bad) == nil {
			t.Errorf("CheckName(%q) accepted", bad)
		}
	}
}

func TestRoot(t *testing.T) {
	env(t)
	home := os.Getenv("HOME")
	t.Setenv(RootVar, "")
	if r, err := Root(); err != nil || r != filepath.Join(home, "atelier", "workspaces") {
		t.Errorf("default root %q %v", r, err)
	}
	t.Setenv(RootVar, "~/w")
	if r, _ := Root(); r != filepath.Join(home, "w") {
		t.Errorf("~ root %q", r)
	}
	t.Setenv(RootVar, "rel")
	if r, _ := Root(); r != filepath.Join(home, "rel") { // cwd is home
		t.Errorf("relative root %q", r)
	}
}

func TestSelectOrder(t *testing.T) {
	root := env(t)
	alpha, beta := mk(t, root, "alpha", ""), mk(t, root, "beta", "")
	other := t.TempDir()

	// Nothing: none, and Open says how to pick one, naming the workspaces.
	if sel, err := Select(""); sel != nil || err != nil {
		t.Fatalf("none: %+v %v", sel, err)
	}
	_, _, err := Open("")
	var none *NoneError
	if !errors.As(err, &none) || !strings.Contains(err.Error(), "--workspace <name>") ||
		!strings.Contains(err.Error(), "cd $ATL_WORKSPACES/<name>") || !strings.HasSuffix(err.Error(), "workspaces: beta, alpha") {
		t.Fatalf("none: %v", err)
	}

	// The current directory.
	t.Chdir(filepath.Join(alpha))
	if sel, _ := Select(""); sel == nil || sel.Dir != alpha || sel.By != ByCwd || sel.Name != "alpha" {
		t.Fatalf("cwd: %+v", sel)
	}
	// $ATL_WORKSPACE wins over it: a name, or a path.
	t.Setenv(Var, "beta")
	if sel, _ := Select(""); sel == nil || sel.Dir != beta || sel.By != ByEnv {
		t.Fatalf("env name: %+v", sel)
	}
	t.Setenv(Var, other)
	if sel, _ := Select(""); sel == nil || sel.Dir != other || sel.By != ByEnv || sel.Name != filepath.Base(other) {
		t.Fatalf("env path: %+v", sel)
	}
	t.Setenv(Var, "Bad Name")
	if _, err := Select(""); err == nil || !strings.Contains(err.Error(), "$ATL_WORKSPACE") {
		t.Fatalf("env bad name: %v", err)
	}
	// The flag wins over both.
	if sel, _ := Select("alpha"); sel == nil || sel.Dir != alpha || sel.By != ByFlag {
		t.Fatalf("flag: %+v", sel)
	}
	if sel, _ := Select("~/ws/beta"); sel == nil || sel.Dir != beta {
		t.Fatalf("flag ~ path: %+v", sel)
	}

	// A selected directory without atelier.toml: how to adopt it.
	if err := os.Mkdir(filepath.Join(root, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, err = Open("old")
	var nw *NotWorkspaceError
	if !errors.As(err, &nw) || !strings.Contains(err.Error(), "atl workspace new old") {
		t.Fatalf("no atelier.toml: %v", err)
	}
	// Outside the root: adopting it needs that root.
	if _, _, err := Open(other); err == nil || !strings.Contains(err.Error(), "ATL_WORKSPACES="+filepath.Dir(other)+" atl workspace new "+filepath.Base(other)) {
		t.Fatalf("outside the root: %v", err)
	}
	if _, _, err := Open("gone"); err == nil || !strings.Contains(err.Error(), "atl workspace new gone") {
		t.Fatalf("missing: %v", err)
	}
	if ws, by, err := Open("beta"); err != nil || ws.Dir != beta || ws.Name != "beta" || by != ByFlag {
		t.Fatalf("open beta: %+v %s %v", ws, by, err)
	}
}

func TestSelectCwd(t *testing.T) {
	root := env(t)
	alpha := mk(t, root, "alpha", "")
	deep := filepath.Join(alpha, "tasks", "x", "api")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root+"2/alpha", 0o755); err != nil { // a sibling with the root as prefix
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(deep, link); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]string{
		alpha:                 "alpha",
		deep:               "alpha",
		link:               "alpha", // the cwd through a symlink
		root:               "",   // the root itself is no workspace
		root + "2/alpha":      "",
		filepath.Dir(root): "",
	} {
		t.Chdir(dir)
		got := ""
		if sel, _ := Select(""); sel != nil {
			got = sel.Name
			if sel.Dir != filepath.Join(root, got) {
				t.Errorf("cwd %s: dir %s", dir, sel.Dir)
			}
		}
		if got != want {
			t.Errorf("cwd %s: %q, want %q", dir, got, want)
		}
	}
	// The root through a symlink.
	rootLink := filepath.Join(t.TempDir(), "root")
	if err := os.Symlink(root, rootLink); err != nil {
		t.Fatal(err)
	}
	t.Setenv(RootVar, rootLink)
	t.Chdir(deep)
	if sel, _ := Select(""); sel == nil || sel.Name != "alpha" || sel.Dir != filepath.Join(rootLink, "alpha") {
		t.Errorf("symlinked root: %+v", sel)
	}
	// An invalid name under the root selects nothing.
	bad := filepath.Join(root, "Bad")
	if err := os.Mkdir(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(bad)
	if sel, _ := Select(""); sel != nil {
		t.Errorf("invalid name: %+v", sel)
	}
	// A workspace that is a symlink under the root to a directory elsewhere.
	t.Setenv(RootVar, root)
	away := mk(t, t.TempDir(), "away", "")
	if err := os.MkdirAll(filepath.Join(away, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(away, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(away, "tasks"))
	if sel, _ := Select(""); sel == nil || sel.Name != "linked" || sel.Dir != filepath.Join(root, "linked") {
		t.Errorf("symlinked workspace: %+v", sel)
	}
}

func TestLoadStrict(t *testing.T) {
	root := env(t)
	for content, want := range map[string]string{
		`repo = "x"`:                      "unknown keys: repo",
		"[sandbox]\nimages = \"x\"":       "unknown keys: sandbox.images",
		`repos = `:                        "parse",
		`tmux_socket = "a/b"`:             "not a path",
		"[sandbox]\nmounts = [\"/a:/b\"]": "contains ':'",
		`repos = 3`:                       "parse",
	} {
		dir := mk(t, root, "w", content)
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", content, err, want)
		}
	}
}

func setting(ws []Setting, key string) Setting {
	for _, s := range ws {
		if s.Key == key {
			return s
		}
	}
	return Setting{}
}

func TestLoadDefaults(t *testing.T) {
	root := env(t)
	home := os.Getenv("HOME")
	dir := mk(t, root, "beta", "")
	ws, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]Setting{
		"repos":           {Var: "ATL_REPOS", Value: filepath.Join(dir, "repos"), Source: FromDefault},
		"git_config":      {Var: "GIT_CONFIG_GLOBAL"},
		"tmux_socket":     {Var: "ATL_TMUX_SOCKET", Value: "atl-beta", Source: FromDefault},
		"context":         {Var: "ATL_CONTEXT"},
		"sandbox.image":   {Var: "ATL_SANDBOX_IMAGE"},
		"sandbox.mounts":  {Var: "ATL_SANDBOX_MOUNTS"},
		"sandbox.context": {Var: "ATL_SANDBOX_CONTEXT"},
	} {
		want.Key = key
		if got := setting(ws.Settings, key); got != want {
			t.Errorf("%s = %+v, want %+v", key, got, want)
		}
	}
	// git_config defaults to <ws>/gitconfig once it exists.
	if err := os.WriteFile(filepath.Join(dir, "gitconfig"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ws, _ = Load(dir)
	if s := setting(ws.Settings, "git_config"); s.Value != filepath.Join(dir, "gitconfig") || s.Source != FromDefault {
		t.Errorf("git_config default %+v", s)
	}

	// Every key; relative paths are the workspace's, ~ is home.
	dir = mk(t, root, "full", `repos = "../src"
git_config = "~/gc"
context = "ctx"
tmux_socket = "sock"
[sandbox]
image = "img"
mounts = ["~/.npmrc", "/etc/x"]
context = "sctx"
`)
	ws, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"repos":           filepath.Join(root, "src"),
		"git_config":      filepath.Join(home, "gc"),
		"context":         "ctx",
		"tmux_socket":     "sock",
		"sandbox.image":   "img",
		"sandbox.mounts":  "~/.npmrc:/etc/x",
		"sandbox.context": "sctx",
	} {
		if s := setting(ws.Settings, key); s.Value != want || s.Source != FromFile {
			t.Errorf("%s = %+v, want %q from the file", key, s, want)
		}
	}
	vars := make([]string, len(ws.Settings))
	for i, s := range ws.Settings {
		vars[i] = s.Var
	}
	if want := []string{"ATL_REPOS", "GIT_CONFIG_GLOBAL", "ATL_CONTEXT", "ATL_TMUX_SOCKET", "ATL_SANDBOX_IMAGE",
		"ATL_SANDBOX_MOUNTS", "ATL_SANDBOX_CONTEXT"}; !slices.Equal(vars, want) {
		t.Errorf("vars %v", vars)
	}
}

func TestApply(t *testing.T) {
	root := env(t)
	alpha := mk(t, root, "alpha", "context = \"from file\"\n[sandbox]\nimage = \"img\"\n")
	beta := mk(t, root, "beta", "")
	t.Setenv("ATL_REPOS", "/srv/repos") // the environment wins
	before := os.Environ()

	ws, _ := Load(alpha)
	a, err := Apply(ws, ByFlag)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		Var: alpha, "ATL_REPOS": "/srv/repos", "ATL_CONTEXT": "from file", "ATL_TMUX_SOCKET": "atl-alpha", "ATL_SANDBOX_IMAGE": "img",
		VarsVar: "ATL_CONTEXT=" + valueHash("from file") + ",ATL_TMUX_SOCKET=" + valueHash("atl-alpha") + ",ATL_SANDBOX_IMAGE=" + valueHash("img"),
	} {
		if got := os.Getenv(k); got != want {
			t.Errorf("$%s = %q, want %q", k, got, want)
		}
	}
	if _, ok := os.LookupEnv("GIT_CONFIG_GLOBAL"); ok {
		t.Error("GIT_CONFIG_GLOBAL set without a git_config")
	}
	for key, src := range map[string]Source{"repos": FromEnv, "context": FromFile, "tmux_socket": FromDefault, "git_config": ""} {
		if s := setting(a.Settings, key); s.Source != src {
			t.Errorf("%s: %+v, want source %q", key, s, src)
		}
	}
	if !AtlSet("ATL_CONTEXT") || AtlSet("ATL_REPOS") || UserEnv("ATL_CONTEXT") != "" || UserEnv("ATL_REPOS") != "/srv/repos" {
		t.Error("AtlSet/UserEnv")
	}
	if !slices.Equal(OriginalEnv(), before) && !slices.Contains(OriginalEnv(), "ATL_REPOS=/srv/repos") {
		t.Error("OriginalEnv is not the environment before Apply")
	}
	if slices.Contains(OriginalEnv(), "ATL_TMUX_SOCKET=atl-alpha") {
		t.Error("OriginalEnv has what Apply set")
	}

	// An atl started from there in the same workspace (the gate, `atl
	// diff` from the TUI): what atl set keeps its source, and follows
	// atelier.toml when it changed.
	if err := os.WriteFile(filepath.Join(alpha, File), []byte("context = \"edited\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws2, _ := Load(alpha)
	b, err := Apply(ws2, ByEnv)
	if err != nil {
		t.Fatal(err)
	}
	if s := setting(b.Settings, "tmux_socket"); s.Source != FromDefault || s.Value != "atl-alpha" {
		t.Errorf("same workspace again: %+v", s)
	}
	if s := setting(b.Settings, "context"); s.Source != FromFile || os.Getenv("ATL_CONTEXT") != "edited" {
		t.Errorf("edited context: %+v", s)
	}
	if v := os.Getenv("ATL_SANDBOX_IMAGE"); v != "" {
		t.Errorf("image removed from the file, still %q", v)
	}
	if s := setting(b.Settings, "repos"); s.Source != FromEnv {
		t.Errorf("same workspace again: %+v", s)
	}
	b.Undo()

	// From there, another workspace: what atl set for alpha is not taken
	// for the user's; what the user set (also since: a value that is not
	// the recorded one) still wins.
	t.Setenv("ATL_SANDBOX_IMAGE", "mine")
	other, _ := Load(beta)
	c, err := Apply(other, ByFlag)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{Var: beta, "ATL_REPOS": "/srv/repos", "ATL_TMUX_SOCKET": "atl-beta", "ATL_CONTEXT": "", "ATL_SANDBOX_IMAGE": "mine"} {
		if got := os.Getenv(k); got != want {
			t.Errorf("another workspace: $%s = %q, want %q", k, got, want)
		}
	}
	if s := setting(c.Settings, "sandbox.image"); s.Source != FromEnv {
		t.Errorf("user's image: %+v", s)
	}
	c.Undo()
	t.Setenv("ATL_SANDBOX_IMAGE", "img")

	// $ATL_WORKSPACE_VARS left over without $ATL_WORKSPACE: what it
	// records is still atl's, and replaced.
	os.Unsetenv(Var)
	d, err := Apply(other, ByCwd)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"ATL_TMUX_SOCKET": "atl-beta", "ATL_CONTEXT": "", "ATL_SANDBOX_IMAGE": ""} {
		if got := os.Getenv(k); got != want {
			t.Errorf("leftover vars: $%s = %q, want %q", k, got, want)
		}
	}
	d.Undo()
	a.Undo()
	for _, k := range []string{VarsVar, "ATL_CONTEXT", "ATL_TMUX_SOCKET"} {
		if v, ok := os.LookupEnv(k); ok {
			t.Errorf("after Undo $%s = %q", k, v)
		}
	}
	if os.Getenv("ATL_REPOS") != "/srv/repos" {
		t.Error("Undo changed ATL_REPOS")
	}
}

func TestWindowEnv(t *testing.T) {
	root := env(t)
	ws, _ := Load(mk(t, root, "alpha", "context = \"c\"\n"))
	a, err := Apply(ws, ByFlag)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Undo()
	got := WindowEnv()
	for k, want := range map[string]string{Var: ws.Dir, "ATL_CONTEXT": "c", "ATL_TMUX_SOCKET": "atl-alpha", "ATL_SANDBOX_IMAGE": "",
		"ATL_REPOS": filepath.Join(ws.Dir, "repos"), VarsVar: os.Getenv(VarsVar)} {
		if v, ok := got[k]; !ok || v != want {
			t.Errorf("%s = %q (%v), want %q", k, v, ok, want)
		}
	}
	if _, ok := got["GIT_CONFIG_GLOBAL"]; ok {
		t.Error("GIT_CONFIG_GLOBAL given empty")
	}
}

func TestDefaultSocket(t *testing.T) {
	root := env(t)
	in := mk(t, root, "alpha", "")
	out := mk(t, t.TempDir(), "alpha", "")
	a, _ := Load(in)
	b, _ := Load(out)
	sa, sb := setting(a.Settings, "tmux_socket").Value, setting(b.Settings, "tmux_socket").Value
	if sa != "atl-alpha" || !strings.HasPrefix(sb, "atl-alpha-") || len(sb) != len("atl-alpha-")+6 {
		t.Errorf("sockets %q %q", sa, sb)
	}
}

func TestCreate(t *testing.T) {
	root := env(t)
	home := os.Getenv("HOME")
	dir, _, err := Create(root, "beta", Config{})
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(root, "beta") {
		t.Fatalf("dir %s", dir)
	}
	for _, d := range []string{"tasks", "data", "config", "state", "repos"} {
		if fi, err := os.Stat(filepath.Join(dir, d)); err != nil || !fi.IsDir() {
			t.Errorf("%s: %v", d, err)
		}
	}
	ws, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s := setting(ws.Settings, "repos"); s.Source != FromDefault {
		t.Errorf("repos %+v", s)
	}
	// atelier.toml exists: refused, whatever the settings.
	if _, _, err := Create(root, "beta", Config{Repos: "/x"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("existing: %v", err)
	}

	// Adopt a directory without atelier.toml: its contents stay.
	old := filepath.Join(root, "old")
	if err := os.MkdirAll(filepath.Join(old, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "data", "atelier.db"), []byte("db"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATL_REPOS", "~/src")
	t.Setenv("ATL_SANDBOX_MOUNTS", "~/.npmrc:/etc/hosts")
	t.Setenv("ATL_CONTEXT", "box rules\nline 2")
	c := Config{Sandbox: &SandboxConfig{Image: "img"}}
	FillFromEnv(&c)
	_, notes, err := Create(root, "old", c)
	if err != nil {
		t.Fatal(err)
	}
	// It had a database and $ATL_TMUX_SOCKET is unset: its sessions stay
	// on tmux's default server.
	if len(notes) != 1 || !strings.Contains(notes[0], `tmux_socket = "default"`) {
		t.Errorf("notes %q", notes)
	}
	if b, _ := os.ReadFile(filepath.Join(old, File)); !strings.Contains(string(b), "# Remove this line once no session runs on the old server") ||
		!strings.Contains(string(b), "atl-<name>, a tmux server of its own.\ntmux_socket = \"default\"\n") {
		t.Errorf("atelier.toml:\n%s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(old, "data", "atelier.db")); string(b) != "db" {
		t.Error("adopting changed the database")
	}
	if _, err := os.Stat(filepath.Join(old, "repos")); err == nil {
		t.Error("repos dir made inside the workspace although repos is elsewhere")
	}
	ws, err = Load(old)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"repos":          filepath.Join(home, "src"),
		"sandbox.image":  "img",
		"sandbox.mounts": "~/.npmrc:/etc/hosts",
		"context":        "box rules\nline 2",
	} {
		if s := setting(ws.Settings, key); s.Value != want || s.Source != FromFile {
			t.Errorf("%s = %+v, want %q", key, s, want)
		}
	}

	if s := setting(ws.Settings, "tmux_socket"); s.Value != "default" || s.Source != FromFile {
		t.Errorf("tmux_socket %+v", s)
	}

	// Not with $ATL_TMUX_SOCKET the user's, nor without a database; values
	// atl set for another workspace are not taken.
	t.Setenv("ATL_TMUX_SOCKET", "box")
	t.Setenv("ATL_SANDBOX_IMAGE", "theirs")
	t.Setenv(VarsVar, "ATL_SANDBOX_IMAGE="+valueHash("theirs")+",ATL_REPOS="+valueHash("~/src"))
	for _, n := range []string{"old2", "old3"} {
		if err := os.MkdirAll(filepath.Join(root, n, "data"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "old2", "data", "atelier.db"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	c = Config{}
	FillFromEnv(&c)
	if c.Repos != "" || c.Sandbox.Image != "" || c.Context == "" {
		t.Errorf("FillFromEnv took atl's values: %+v %+v", c, c.Sandbox)
	}
	if _, notes, err := Create(root, "old2", c); err != nil || len(notes) != 0 {
		t.Errorf("old2: %q %v", notes, err)
	}
	t.Setenv("ATL_TMUX_SOCKET", "")
	if _, notes, err := Create(root, "old3", Config{}); err != nil || len(notes) != 0 {
		t.Errorf("old3 (no database): %q %v", notes, err)
	}

	// A repos dir inside the workspace is written relative to it, and made.
	dir, _, err = Create(root, "in", Config{Repos: filepath.Join(root, "in", "clones")})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, File)); !strings.Contains(string(b), `repos = "clones"`) {
		t.Errorf("atelier.toml:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "clones")); err != nil {
		t.Error(err)
	}

	if _, _, err := Create(root, "Bad", Config{}); err == nil {
		t.Error("invalid name accepted")
	}
	if _, _, err := Create(root, "m", Config{Sandbox: &SandboxConfig{Mounts: []string{"/a:b"}}}); err == nil {
		t.Error("mount with ':' accepted")
	}
	// The gate socket must fit: <ws>/run/<12>/docker.sock ≤ 107 bytes.
	long := filepath.Join(root, strings.Repeat("d", 100-len(root)))
	if _, _, err := Create(long, "w", Config{}); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Errorf("long path: %v", err)
	}
	if _, err := os.Stat(long); err == nil {
		t.Error("made the too-long workspace")
	}
}

// TestLast: the last workspace opened, kept in the root; anything wrong
// reads as none.
func TestLast(t *testing.T) {
	root := t.TempDir()
	if got := Last(root); got != "" {
		t.Errorf("none: %q", got)
	}
	if err := SaveLast(root, "alpha"); err != nil {
		t.Fatal(err)
	}
	if got := Last(root); got != "alpha" {
		t.Errorf("Last = %q", got)
	}
	if err := SaveLast(root, "Bad"); err == nil {
		t.Error("saved an invalid name")
	}
	if err := SaveLast(filepath.Join(root, "missing"), "alpha"); err == nil {
		t.Error("saved into a missing root")
	}
	if err := os.WriteFile(filepath.Join(root, ".last"), []byte("../x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Last(root); got != "" {
		t.Errorf("invalid content: %q", got)
	}
	if es, _ := List(root); len(es) != 0 {
		t.Errorf("listed %v", es)
	}
}
