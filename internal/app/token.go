package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/andreujuanc/atelier/internal/sandbox"
	"github.com/andreujuanc/atelier/internal/tmux"
)

// Sandboxed sessions authenticate with a token (claudeAuthVars). It comes
// from atl's environment or, when that has none, from the file `atl token`
// stores it in (Paths.ClaudeToken), which only the box side can read.

// claudeAuthVars are the variables that authenticate Claude Code in a task
// container: a token from `claude setup-token`, or an API key.
var claudeAuthVars = []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"}

// errNoClaudeAuth is what starting a sandboxed session without a token says.
var errNoClaudeAuth = errors.New("task sessions run in the task's own container and need a Claude Code token there: run `atl token` " +
	"(or set CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY where atl runs)")

// A token from `claude setup-token` lasts a year; doctor warns a month
// before that.
const (
	tokenLifetime   = 365 * 24 * time.Hour
	tokenWarnBefore = 30 * 24 * time.Hour
)

// tokenRE is the shape of a token from `claude setup-token`: Claude Code
// itself tells OAuth tokens by the prefix sk-ant-oat (sk-ant-oat01- today);
// the rest is base64url.
var (
	tokenRE     = regexp.MustCompile(`^sk-ant-oat[0-9]{2}-[A-Za-z0-9_-]{32,512}$`)
	tokenHeadRE = regexp.MustCompile(`^sk-ant-oat[0-9]{2}-[A-Za-z0-9_-]*$`)
	tokenTailRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// storedToken is the content of Paths.ClaudeToken.
type storedToken struct {
	Token    string    `json:"token"`
	StoredAt time.Time `json:"stored_at"`
}

const maxTokenFile = 64 << 10

// loadToken reads the stored token; ok is false when there is none.
func (a *App) loadToken() (st storedToken, ok bool, err error) {
	path := a.Paths.ClaudeToken()
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return st, false, nil
	}
	if err != nil {
		return st, false, fmt.Errorf("stored Claude Code token: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxTokenFile))
	if err != nil {
		return st, false, fmt.Errorf("stored Claude Code token %s: %w", path, err)
	}
	if json.Unmarshal(data, &st) != nil || !tokenRE.MatchString(st.Token) {
		return storedToken{}, false, fmt.Errorf("stored Claude Code token %s is not one atl wrote; run `atl token` again", path)
	}
	return st, true, nil
}

// StoreToken stores tok, which must have the shape of a token from `claude
// setup-token`, with the current time, in Paths.ClaudeToken (mode 0600,
// replaced atomically). Neither the value nor any part of it is in an error.
func (a *App) StoreToken(tok string) error {
	if !tokenRE.MatchString(tok) {
		return errors.New("that is not a Claude Code token from `claude setup-token` (sk-ant-oat01-…); nothing stored")
	}
	path := a.Paths.ClaudeToken()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(storedToken{Token: tok, StoredAt: a.now().UTC().Truncate(time.Second)})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".claude-token.tmp*") // mode 0600
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// claudeAuth is what authenticates sandboxed sessions.
type claudeAuth struct {
	vars     map[string]string // claudeAuthVars → value
	stored   bool              // from Paths.ClaudeToken, not the environment
	storedAt time.Time         // when stored
}

// claudeAuth returns the claudeAuthVars set in atl's environment; when none
// is, the stored token as CLAUDE_CODE_OAUTH_TOKEN; else errNoClaudeAuth.
func (a *App) claudeAuth() (claudeAuth, error) {
	vars := map[string]string{}
	for _, k := range claudeAuthVars {
		if v := os.Getenv(k); v != "" {
			vars[k] = v
		}
	}
	if len(vars) > 0 {
		return claudeAuth{vars: vars}, nil
	}
	st, ok, err := a.loadToken()
	if err != nil {
		return claudeAuth{}, err
	}
	if !ok {
		return claudeAuth{}, errNoClaudeAuth
	}
	return claudeAuth{vars: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": st.Token}, stored: true, storedAt: st.StoredAt}, nil
}

// sandboxAuth returns the variables that authenticate sandboxed sessions
// (claudeAuth), warning when a stored token is over a year old.
func (a *App) sandboxAuth() (map[string]string, error) {
	auth, err := a.claudeAuth()
	if err != nil {
		return nil, err
	}
	if auth.stored && a.now().Sub(auth.storedAt) >= tokenLifetime {
		a.warnf("the Claude Code token stored on %s is over a year old and has probably expired: run `atl token`",
			auth.storedAt.Local().Format(time.DateOnly))
	}
	return auth.vars, nil
}

// TokenSession is the tmux session in which `atl token` runs `claude
// setup-token`. Task sessions are named "atl-<slug>", so the underscore can
// never collide.
const TokenSession = "atl_token"

const tokenWindow = "setup-token"

// tokenScript runs in the token window: after claude, the screen stays
// until the user presses Enter, so they can read it. The tmux server's
// environment may hold the token of sandboxed sessions (see launch): it is
// not for setup-token.
const tokenScript = `unset CLAUDE_CODE_OAUTH_TOKEN ANTHROPIC_API_KEY
claude setup-token
printf '\n%s ' 'Press Enter to go back to atl (it reads the token from this screen).'
read -r _`

// tokenContainerScript is tokenScript with the sandbox on: its arguments
// are the command that runs claude setup-token in a throwaway container
// (tokenArgv). docker run reads the window's size when it starts and then
// follows its resizes, but loses one that comes while it starts (as docker
// exec does, see sandbox.WindowArgv), and Claude Code breaks the token at
// the width it sees: so it waits, up to 10 seconds, for the client atl
// attaches, and a helper has the window's size sent again (SIGWINCH to the
// window's process group, which every process in it but docker run
// ignores) 1, 3 and 7 seconds after the start.
const tokenContainerScript = `unset CLAUDE_CODE_OAUTH_TOKEN ANTHROPIC_API_KEY
i=0
while [ $i -lt 100 ]; do
	n=$(tmux display-message -p -t "$TMUX_PANE" '#{session_attached}' 2> /dev/null) || break
	[ "${n:-0}" = 0 ] || break
	sleep 0.1 2> /dev/null || { sleep 1; i=$((i + 9)); }
	i=$((i + 1))
done
(for d in 1 2 4; do sleep $d; kill -WINCH 0 || exit 0; done) < /dev/null > /dev/null 2>&1 &
"$@"
kill $! 2> /dev/null
printf '\n%s ' 'Press Enter to go back to atl (it reads the token from this screen).'
read -r _`

// tokenArgv is the command of the token window: tokenScript next to atl;
// with the sandbox on, tokenContainerScript running claude setup-token in a
// throwaway container named name of the task image (`docker run --rm -it --pull=never`,
// as the user running atl, with no mount and no variable but TERM, LANG
// and HOME=/tmp, a home that exists and is writable whatever users the
// image has: claude writes its config there, and it goes with --rm), so Claude Code need not be installed where atl runs.
func (a *App) tokenArgv(name string) []string {
	if a.Sandbox == nil {
		return []string{"sh", "-c", tokenScript}
	}
	return []string{"sh", "-c", tokenContainerScript, "atl-token",
		"docker", "run", "--rm", "-it", "--pull=never", "--name", name, "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-e", "TERM", "-e", "LANG", "-e", "HOME=/tmp", a.Sandbox.Image, "claude", "setup-token"}
}

// checkTokenImage refuses, with the sandbox on, to run claude setup-token
// without a Docker daemon or the task image (one line each, as a session's
// launch says it). The image is never pulled for it (`--pull=never`): the
// token is minted by the image's own claude, so it must be one you built or
// pulled yourself, not whatever a registry has under that name.
func (a *App) checkTokenImage(ctx context.Context) error {
	if a.Sandbox == nil {
		return nil
	}
	if _, err := a.checkDocker(ctx); err != nil {
		return err
	}
	image := a.Sandbox.Image
	if missing, err := sandbox.ImageMissing(ctx, a.docker(), image); err == nil && missing {
		if image == sandbox.DefaultImage {
			return missingImage(image, "default")
		}
		return errors.New("task image " + image + " (from $ATL_SANDBOX_IMAGE) is not on the Docker daemon, and atl token does not pull it " +
			"(its claude makes the token): build or pull it yourself")
	}
	return nil
}

// tokenPoll is how often SetupToken looks whether the token window ended.
const tokenPoll = 200 * time.Millisecond

// SetupToken runs `claude setup-token` in a tmux window the user is attached
// to (attach runs the attach command with the terminal), reads the token
// from the window's screen when the window ends and stores it (StoreToken).
// If no single token of the right shape is on the screen, it asks for the
// token with ask (input not shown) instead. auto reports which way it went.
// The token is never printed, logged nor put on a command line; the window
// and its scrollback are gone when SetupToken returns.
func (a *App) SetupToken(ctx context.Context, attach func(*exec.Cmd) error, ask func(prompt string) (string, error)) (auto bool, err error) {
	if err := outsideSandbox("get a Claude Code token"); err != nil {
		return false, err
	}
	if err := a.checkTokenImage(ctx); err != nil {
		return false, err
	}
	screen, width, err := a.runTokenWindow(ctx, attach)
	if err != nil {
		return false, err
	}
	if tok, ok := pickToken(screen, width); ok {
		return true, a.StoreToken(tok)
	}
	return false, a.PasteToken(ask, "atl could not find the token on that screen. Paste the token "+
		"(the sk-ant-oat01-… line under \"Your OAuth token\"; it is not shown) and press Enter: ")
}

// PasteToken asks for the token with ask and stores it.
func (a *App) PasteToken(ask func(prompt string) (string, error), prompt string) error {
	if err := outsideSandbox("store a Claude Code token"); err != nil {
		return err
	}
	tok, err := ask(prompt)
	if err != nil {
		return fmt.Errorf("read the token: %w", err)
	}
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return errors.New("no token given; nothing stored")
	}
	return a.StoreToken(tok)
}

// runTokenWindow runs tokenScript (with the sandbox on,
// tokenContainerScript, see tokenArgv) in window tokenWindow of
// TokenSession, attaches the user to it and returns the window's text and
// width once it ended (or the user detached), then removes the session, and
// the token's container should it still run (the user left early).
func (a *App) runTokenWindow(ctx context.Context, attach func(*exec.Cmd) error) (string, int, error) {
	if a.Tmux.HasSession(ctx, TokenSession) {
		if err := a.Tmux.KillSession(ctx, TokenSession); err != nil {
			return "", 0, err
		}
	}
	var rnd [3]byte
	rand.Read(rnd[:])
	container := "atl-token-" + hex.EncodeToString(rnd[:])
	dir, _ := os.UserHomeDir()
	win, err := a.Tmux.NewWindow(ctx, tmux.WindowSpec{
		Session: TokenSession,
		Name:    tokenWindow,
		Dir:     dir,
		Env:     a.windowEnv(nil),
		Argv:    a.tokenArgv(container),
	})
	kill := func() {
		if err := a.Tmux.KillSession(context.WithoutCancel(ctx), TokenSession); err != nil {
			a.warnf("%v", err)
		}
		if a.Sandbox != nil {
			// docker run's client may die with the window and leave
			// setup-token waiting in the container (it is gone already
			// when it ended: --rm). Best effort: an error says nothing more.
			a.docker()(context.WithoutCancel(ctx), "rm", "-f", container)
		}
	}
	if err != nil {
		if win != "" {
			kill()
		}
		return "", 0, err
	}
	// The screen must outlive the script for atl to read it.
	if err := a.Tmux.SetWindowOption(ctx, win, "remain-on-exit", "on"); err != nil {
		kill()
		return "", 0, err
	}
	// Inside tmux the user's client switches to the window and back to
	// where it was when the session goes (not detach).
	inTmux := os.Getenv("TMUX") != ""
	if inTmux {
		if err := a.Tmux.SetSessionOption(ctx, TokenSession, "detach-on-destroy", "off"); err != nil {
			a.warnf("%v", err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- attach(a.Tmux.AttachCmd(TokenSession)) }()
	attached := true
	tick := time.NewTicker(tokenPoll)
	defer tick.Stop()
wait:
	for {
		select {
		case err := <-done:
			attached = false
			if err != nil {
				kill()
				return "", 0, fmt.Errorf("attach: %w", err)
			}
			if !inTmux {
				break wait // the user detached
			}
		case <-ctx.Done():
			break wait
		case <-tick.C:
			if alive, err := a.Tmux.WindowAlive(ctx, TokenSession, tokenWindow); err == nil && !alive {
				break wait
			}
		}
	}
	screen, width, cerr := a.Tmux.CapturePane(context.WithoutCancel(ctx), win)
	kill()
	if attached && !inTmux {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	if cerr != nil {
		a.warnf("%v", cerr)
	}
	return screen, width, nil
}

// pickToken finds the token `claude setup-token` printed on screen, a
// pane's text with the lines tmux wrapped joined, width columns wide. The
// token is a line of its own: sk-ant-oatNN-… and nothing else but the
// indent. Claude Code breaks a word longer than the screen at its width
// itself, so a line that reaches the right edge (within a few columns of
// margin) continues on the next ones: lines of token characters only, as
// long as the first part but the last. Exactly one distinct token of the
// right shape must be found; ok is false otherwise.
func pickToken(screen string, width int) (tok string, ok bool) {
	const margin = 4
	lines := strings.Split(screen, "\n")
	found := map[string]bool{}
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], " \t\r")
		head := strings.TrimLeft(line, " ")
		if !tokenHeadRE.MatchString(head) {
			continue
		}
		cand := head
		part := len(head)
		more := width > 0 && len(line) >= width-margin
		for more && i+1 < len(lines) {
			next := strings.TrimSpace(lines[i+1])
			if !tokenTailRE.MatchString(next) || len(next) > part {
				break
			}
			cand += next
			i++
			more = len(next) == part
		}
		if tokenRE.MatchString(cand) {
			found[cand] = true
		}
	}
	if len(found) != 1 {
		return "", false
	}
	for t := range found {
		tok = t
	}
	return tok, true
}
