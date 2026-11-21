package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/model"
)

// storedTestToken is a made-up token of the shape `claude setup-token`
// prints: sk-ant-oat01- and base64url characters (106 in all).
const storedTestToken = "sk-ant-oat01-Ab3_dEf-GhIjKlMnOpQrStUvWxYz0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVW-xyzAA"

// successScreen is the end of `claude setup-token` as Claude Code 2.1.327
// renders it (texts from its source), the token as given.
func successScreen(tokenLines ...string) string {
	return " Paste code here if prompted\n" +
		"\n" +
		" ✓ Long-lived authentication token created\n" +
		"\n" +
		" Your OAuth token (valid for 1 year):\n" +
		"\n" +
		strings.Join(tokenLines, "\n") + "\n" +
		"\n" +
		" Store this token securely. You won't be able to see it again.\n" +
		"\n" +
		" Use this token by setting: export CLAUDE_CODE_OAUTH_TOKEN=<token>\n" +
		"\n" +
		" You can revoke it later at https://claude.ai/settings/claude-code\n" +
		"\n" +
		"Press Enter to go back to atl (it reads the token from this screen).\n"
}

// wrap breaks s into lines of n characters, each after indent, the way
// Claude Code breaks a word longer than the screen.
func wrap(s, indent string, n int) []string {
	var out []string
	for len(s) > n {
		out = append(out, indent+s[:n])
		s = s[n:]
	}
	return append(out, indent+s)
}

// urlScreen is what `claude setup-token` shows before the browser approval
// (seen in the atelier image, 100 columns; the URL is broken by Claude Code).
const urlScreen = ` This will guide you through long-lived (1-year) auth token setup for your Claude account. Claude
 subscription required.

 Browser didn't open? Use the URL below to sign in (c to copy)

https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&resp
onse_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=user%3
Ainference&code_challenge=IV6fhQx95SOjhPHfhVu_Dg3s6WGXTxtz95hZGx6HBTQ&code_challenge_method=S256&sta
te=dEkQG3QxHIAgZVIqCVumkdqM0fJK7qGvMQw59H3esT0

 Paste code here if prompted
 ╭─────────────────────────────────────────────────────────────────────────────────────────────────╮
 │                                                                                                 │
 ╰─────────────────────────────────────────────────────────────────────────────────────────────────╯
`

const errorScreen = ` OAuth error: That code wasn't accepted. It may be incomplete, expired, already used, or from an
 earlier sign-in attempt. Sign in again to get a new code.


 Press Enter to retry.
`

func TestPickToken(t *testing.T) {
	tok := storedTestToken
	other := strings.Replace(tok, "Ab3", "Zz9", 1)
	for _, c := range []struct {
		name   string
		screen string
		width  int
		want   string
	}{
		{"one line", successScreen(" " + tok), 200, tok},
		{"one line, no width", successScreen(" " + tok), 0, tok},
		{"wrapped by tmux (joined by capture-pane -J)", urlScreen + successScreen(tok), 60, tok},
		{"broken by Claude Code at 60 columns", successScreen(wrap(tok, " ", 59)...), 60, tok},
		{"broken at 40 columns, three lines", successScreen(wrap(tok, " ", 39)...), 40, tok},
		{"broken flush at the edge", successScreen(wrap(tok, "", 50)...), 50, tok},
		{"twice the same (an earlier frame in the scrollback)", successScreen(" "+tok) + successScreen(" "+tok), 200, tok},
		{"no token: browser step", urlScreen, 100, ""},
		{"no token: wrong code", urlScreen + errorScreen, 100, ""},
		{"no token: empty", "", 80, ""},
		{"two different tokens", successScreen(" "+tok) + successScreen(" "+other), 200, ""},
		{"too short", successScreen(" sk-ant-oat01-abc"), 200, ""},
		{"inside a sentence", successScreen(" your token is " + tok), 200, ""},
		{"after export", successScreen(" export CLAUDE_CODE_OAUTH_TOKEN=" + tok), 200, ""},
		{"an API key, not a setup-token token", successScreen(" sk-ant-api03-" + tok[13:]), 200, ""},
		{"a stray character", successScreen(" " + tok + "!"), 200, ""},
		// The line ends short of the edge: what follows is not the token's.
		{"short line not continued", successScreen(" "+tok[:60], " "+tok[60:]), 100, tok[:60]},
		// A continuation longer than the first part is not one.
		{"continuation too long", successScreen(" "+tok[:49], " "+tok[49:]), 50, tok[:49]},
	} {
		got, ok := pickToken(c.screen, c.width)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, ok, c.want)
		}
	}
}

func TestStoreToken(t *testing.T) {
	e := newEnv(t)
	a := e.app
	t0 := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	a.Now = func() time.Time { return t0 }
	if err := a.StoreToken("not-a-token"); err == nil || strings.Contains(err.Error(), "not-a-token") {
		t.Errorf("StoreToken of junk: %v", err)
	}
	if exists(a.Paths.ClaudeToken()) {
		t.Fatal("junk stored")
	}
	if err := a.StoreToken(storedTestToken); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(a.Paths.ClaudeToken())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %v, want 0600", fi.Mode().Perm())
	}
	st, ok, err := a.loadToken()
	if err != nil || !ok || st.Token != storedTestToken || !st.StoredAt.Equal(t0) {
		t.Errorf("loadToken = %+v, %v, %v", st, ok, err)
	}
	// Replacing it keeps no temp file behind.
	if err := a.StoreToken(storedTestToken); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Dir(a.Paths.ClaudeToken()))
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), ".claude-token") {
			t.Errorf("left %s", en.Name())
		}
	}
	// A file atl did not write is an error that says what to do.
	if err := os.WriteFile(a.Paths.ClaudeToken(), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	if _, err := a.claudeAuth(); err == nil || !strings.Contains(err.Error(), "atl token") {
		t.Errorf("claudeAuth with a broken file: %v", err)
	}
}

// TestSandboxStoredToken: with none in the environment the stored token
// reaches the session (by name, as from the environment); a variable in the
// environment wins over it; without either nothing starts and the error
// names atl token.
func TestSandboxStoredToken(t *testing.T) {
	e, fd := sandboxEnv(t)
	ctx := context.Background()
	e.task(t, "boxed", model.StatusActive, true)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

	_, err := e.app.NewSession(ctx, "boxed")
	if err == nil || !strings.Contains(err.Error(), "run `atl token`") || strings.Contains(err.Error(), "\n") {
		t.Fatalf("NewSession without any token: %v", err)
	}
	if len(fd.calls) != 0 {
		t.Errorf("started something without a token: %q", fd.calls)
	}

	if err := e.app.StoreToken(storedTestToken); err != nil {
		t.Fatal(err)
	}
	s, err := e.app.NewSession(ctx, "boxed")
	if err != nil {
		t.Fatal(err)
	}
	e.argv(t, s.ID)
	if env := readFile(t, filepath.Join(e.out, "env-"+strconv.FormatInt(s.ID, 10))); !strings.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN="+storedTestToken+"\n") {
		t.Errorf("claude did not get the stored token:\n%s", env)
	}
	if cmd := e.tmux(t, "display-message", "-p", "-t", s.TmuxTarget, "#{pane_start_command}"); !strings.Contains(cmd, "-e CLAUDE_CODE_OAUTH_TOKEN ") ||
		strings.Contains(cmd, storedTestToken) {
		t.Errorf("window command: %s", cmd)
	}

	// The environment wins.
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", testToken)
	s2, err := e.app.NewSession(ctx, "boxed")
	if err != nil {
		t.Fatal(err)
	}
	e.argv(t, s2.ID)
	if env := readFile(t, filepath.Join(e.out, "env-"+strconv.FormatInt(s2.ID, 10))); !strings.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN="+testToken+"\n") {
		t.Errorf("the environment's token did not win:\n%s", env)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	const key = "sk-ant-api03-made-up-key-0123456789abcdefghij"
	t.Setenv("ANTHROPIC_API_KEY", key)
	auth, err := e.app.sandboxAuth()
	if err != nil || len(auth) != 1 || auth["ANTHROPIC_API_KEY"] != key {
		t.Errorf("an API key in the environment: %v, %v", len(auth), err)
	}

	var all strings.Builder
	for _, c := range fd.calls {
		all.WriteString(strings.Join(c, " ") + "\n")
	}
	all.WriteString(e.app.Out.(*bytes.Buffer).String())
	if strings.Contains(all.String(), storedTestToken) {
		t.Errorf("the stored token is in docker's arguments or the output:\n%s", all.String())
	}
}

// TestTokenFileOutsideMounts: the token file is in none of a task
// container's mounts, with or without a workspace.
func TestTokenFileOutsideMounts(t *testing.T) {
	e, _ := sandboxEnv(t)
	ctx := context.Background()
	p := e.app.Paths
	root := filepath.Dir(e.out)
	task := e.task(t, "boxed", model.StatusActive, true, model.TaskRepo{Name: "api", Mode: model.ModeRW}, model.TaskRepo{Name: "docs", Mode: model.ModeRO})
	for _, d := range []string{filepath.Join(p.RepoPath("api"), ".git"), p.RepoPath("docs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ws := filepath.Join(root, "ws")
	for _, paths := range []config.Paths{p, {
		Workspace: ws, Repos: p.Repos, Tasks: filepath.Join(ws, "tasks"),
		DB: filepath.Join(ws, "data", "atelier.db"), StateDir: filepath.Join(ws, "state"),
		ConfigDir: filepath.Join(ws, "config"),
	}} {
		if err := os.MkdirAll(paths.TaskDir("boxed"), 0o755); err != nil {
			t.Fatal(err)
		}
		e.app.Paths = paths
		spec, err := e.app.sandboxSpec(ctx, task)
		if err != nil {
			t.Fatal(err)
		}
		if len(spec.Mounts) < 4 {
			t.Fatalf("mounts: %+v", spec.Mounts)
		}
		file := paths.ClaudeToken()
		for _, m := range spec.Mounts {
			if rel, err := filepath.Rel(m.Src, file); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
				t.Errorf("mount %s reaches the token file %s", m.Src, file)
			}
		}
		for k, v := range spec.Env {
			if strings.Contains(v, "claude-token") {
				t.Errorf("container env %s=%s", k, v)
			}
		}
	}
}

func TestDoctorClaudeToken(t *testing.T) {
	e, _ := sandboxEnv(t)
	a := e.app
	t0 := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	now := t0
	a.Now = func() time.Time { return now }

	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	c := a.checkClaudeToken()
	if c.OK || !strings.Contains(c.Detail, "atl token") {
		t.Errorf("no token: %+v", c)
	}

	if err := a.StoreToken(storedTestToken); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		after time.Duration
		level string
		has   string
	}{
		{0, "ok", "0 days old"},
		{100 * 24 * time.Hour, "ok", "100 days old"},
		{334 * 24 * time.Hour, "ok", "334 days old"},
		{335 * 24 * time.Hour, "warn", "expires by 2027-01-10"},
		{364 * 24 * time.Hour, "warn", "364 days old"},
		{365 * 24 * time.Hour, "FAIL", "has expired"},
	} {
		now = t0.Add(step.after)
		c := a.checkClaudeToken()
		if c.Level() != step.level || !strings.Contains(c.Detail, step.has) || !strings.Contains(c.Detail, "atl token") {
			t.Errorf("after %v: %s %q", step.after, c.Level(), c.Detail)
		}
		if strings.Contains(c.Detail, storedTestToken) {
			t.Errorf("doctor shows the token: %q", c.Detail)
		}
	}
	now = t0
	if c := a.checkClaudeToken(); !strings.Contains(c.Detail, "from "+a.Paths.ClaudeToken()) {
		t.Errorf("stored: %q", c.Detail)
	}

	// The environment wins, and doctor says the stored one is not used.
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", testToken)
	c = a.checkClaudeToken()
	if c.Level() != "ok" || !strings.Contains(c.Detail, "from the environment: $CLAUDE_CODE_OAUTH_TOKEN") ||
		!strings.Contains(c.Detail, "not used") || strings.Contains(c.Detail, testToken) {
		t.Errorf("env: %s %q", c.Level(), c.Detail)
	}
}

// tokenEnv sets up newEnv's PATH with a `claude` whose setup-token prints
// screen.
func tokenEnv(t *testing.T, screen string) *env {
	t.Helper()
	e := newEnv(t)
	root := filepath.Dir(e.out)
	f := filepath.Join(root, "screen")
	if err := os.WriteFile(f, []byte(screen), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := "#!/bin/sh\n[ \"$1\" = setup-token ] || exit 3\ncat '" + f + "'\n"
	if err := os.WriteFile(filepath.Join(root, "bin", "claude"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	return e
}

// userAtWindow plays the user attached to the token window: it waits for
// the prompt, presses Enter and returns once the window ended. The attach
// command is not run (there is no terminal); it records its arguments.
func userAtWindow(t *testing.T, e *env, attachArgs *[]string) func(*exec.Cmd) error {
	return func(cmd *exec.Cmd) error {
		*attachArgs = cmd.Args
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			out, _ := exec.Command("tmux", "-L", e.sock, "capture-pane", "-p", "-t", "="+TokenSession+":").Output()
			if bytes.Contains(out, []byte("Press Enter to go back to atl")) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err := exec.Command("tmux", "-L", e.sock, "send-keys", "-t", "="+TokenSession+":", "Enter").Run(); err != nil {
			return err
		}
		for time.Now().Before(deadline) {
			if alive, err := e.app.Tmux.WindowAlive(context.Background(), TokenSession, tokenWindow); err != nil || !alive {
				return nil
			}
			time.Sleep(50 * time.Millisecond)
		}
		return errors.New("token window still alive")
	}
}

func noAsk(t *testing.T) func(string) (string, error) {
	return func(string) (string, error) {
		t.Error("asked for the token")
		return "", errors.New("no")
	}
}

// TestSetupTokenFromScreen: atl token reads the token from the window,
// stores it and leaves nothing behind: no session, nothing printed.
func TestSetupTokenFromScreen(t *testing.T) {
	// Claude Code breaks the token at tmux's default width of 80 columns.
	e := tokenEnv(t, urlScreen+successScreen(wrap(storedTestToken, " ", 79)...))
	var attachArgs []string
	auto, err := e.app.SetupToken(context.Background(), userAtWindow(t, e, &attachArgs), noAsk(t))
	if err != nil || !auto {
		t.Fatalf("SetupToken = %v, %v", auto, err)
	}
	if st, ok, err := e.app.loadToken(); err != nil || !ok || st.Token != storedTestToken {
		t.Errorf("stored %v %v %v", st.Token == storedTestToken, ok, err)
	}
	if !strings.Contains(strings.Join(attachArgs, " "), "attach-session -t ="+TokenSession) {
		t.Errorf("attach command: %q", attachArgs)
	}
	if e.app.Tmux.HasSession(context.Background(), TokenSession) {
		t.Error("the token session is still there")
	}
	if out := e.app.Out.(*bytes.Buffer).String(); strings.Contains(out, storedTestToken[20:40]) {
		t.Errorf("output has the token: %q", out)
	}
}

// TestSetupTokenInsideTmux: inside tmux the attach command (switch-client)
// returns at once; atl waits for the window to end all the same, and the
// client goes back to its session (detach-on-destroy off) when the token
// session goes.
func TestSetupTokenInsideTmux(t *testing.T) {
	e := tokenEnv(t, successScreen(" "+storedTestToken))
	t.Setenv("TMUX", "/tmp/tmux-1000/default,1,0")
	var attachArgs, ignored []string
	user := userAtWindow(t, e, &ignored)
	var opt string
	attach := func(cmd *exec.Cmd) error {
		attachArgs = cmd.Args
		out, _ := exec.Command("tmux", "-L", e.sock, "show-options", "-v", "-t", "="+TokenSession+":", "detach-on-destroy").Output()
		opt = strings.TrimSpace(string(out))
		go user(cmd)
		return nil
	}
	auto, err := e.app.SetupToken(context.Background(), attach, noAsk(t))
	if err != nil || !auto {
		t.Fatalf("SetupToken = %v, %v", auto, err)
	}
	if !strings.Contains(strings.Join(attachArgs, " "), "switch-client -t ="+TokenSession) || opt != "off" {
		t.Errorf("attach command %q, detach-on-destroy %q", attachArgs, opt)
	}
	if out := e.app.Out.(*bytes.Buffer).String(); out != "" {
		t.Errorf("output: %q", out)
	}
}

// TestSetupTokenPasteFallback: without a token on the screen atl asks for
// it; what is pasted is checked before it is stored.
func TestSetupTokenPasteFallback(t *testing.T) {
	e := tokenEnv(t, urlScreen+errorScreen)
	var attachArgs []string
	var prompts []string
	ask := func(p string) (string, error) {
		prompts = append(prompts, p)
		return "  " + storedTestToken + "\n", nil
	}
	auto, err := e.app.SetupToken(context.Background(), userAtWindow(t, e, &attachArgs), ask)
	if err != nil || auto {
		t.Fatalf("SetupToken = %v, %v", auto, err)
	}
	if len(prompts) != 1 || !strings.Contains(prompts[0], "Paste") {
		t.Errorf("prompts: %q", prompts)
	}
	if st, ok, err := e.app.loadToken(); err != nil || !ok || st.Token != storedTestToken {
		t.Errorf("stored %v %v %v", st.Token == storedTestToken, ok, err)
	}

	// Junk, or nothing, is not stored, and the error does not repeat it.
	if err := os.Remove(e.app.Paths.ClaudeToken()); err != nil {
		t.Fatal(err)
	}
	for _, pasted := range []string{"4/0AbCdEf-the-code-not-the-token#state", ""} {
		_, err := e.app.SetupToken(context.Background(), userAtWindow(t, e, &attachArgs),
			func(string) (string, error) { return pasted, nil })
		if err == nil || (pasted != "" && strings.Contains(err.Error(), pasted)) {
			t.Errorf("pasted %q: %v", pasted, err)
		}
		if exists(e.app.Paths.ClaudeToken()) {
			t.Errorf("pasted %q: stored", pasted)
		}
	}
}

func TestTokenRefusedInsideSandbox(t *testing.T) {
	e := tokenEnv(t, "")
	t.Setenv("ATL_SANDBOX_ID", "t1-abcdef")
	if _, err := e.app.SetupToken(context.Background(), func(*exec.Cmd) error { return nil }, noAsk(t)); err == nil ||
		!strings.Contains(err.Error(), "task container") {
		t.Errorf("SetupToken inside: %v", err)
	}
	if err := e.app.PasteToken(func(string) (string, error) { return storedTestToken, nil }, ""); err == nil {
		t.Error("PasteToken inside a task container")
	}
	if exists(e.app.Paths.ClaudeToken()) {
		t.Error("stored inside a task container")
	}
}
