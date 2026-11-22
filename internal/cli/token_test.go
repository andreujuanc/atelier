package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const cliTestToken = "sk-ant-oat01-Ab3_dEf-GhIjKlMnOpQrStUvWxYz0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVW-xyzAA"

// TestTokenPasteAndDoctor: atl token --paste stores a token read from
// standard input where only atl reads it; doctor says where the token comes
// from; nothing prints it.
func TestTokenPasteAndDoctor(t *testing.T) {
	root := setup(t)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ATL_SANDBOX", "docker")
	t.Setenv("ATL_SANDBOX_ID", "")
	var all strings.Builder

	_, out, errs := atl(t, "doctor")
	all.WriteString(out + errs)
	if !strings.Contains(out, "claude token") || !strings.Contains(out, "run `atl token`") {
		t.Errorf("doctor without a token:\n%s", out)
	}

	// Without a terminal atl token cannot run claude setup-token.
	code, out, errs := atl(t, "token")
	if code != 1 || !strings.Contains(errs, "atl token --paste") {
		t.Errorf("token without a terminal: %d %q %q", code, out, errs)
	}

	// Junk is refused and not repeated.
	var o, e bytes.Buffer
	if code := runIn(context.Background(), []string{"token", "--paste"}, strings.NewReader("4/0AbC-the-code#x\n"), &o, &e); code != 1 ||
		strings.Contains(o.String()+e.String(), "4/0AbC") {
		t.Errorf("token --paste junk: %d %q %q", code, o.String(), e.String())
	}

	o.Reset()
	e.Reset()
	if code := runIn(context.Background(), []string{"token", "--paste"}, strings.NewReader(cliTestToken+"\n"), &o, &e); code != 0 {
		t.Fatalf("token --paste: %d %q %q", code, o.String(), e.String())
	}
	all.WriteString(o.String() + e.String())
	file := filepath.Join(root, "ws", "run", "claude-token.json")
	if !strings.Contains(o.String(), "stored in "+file) {
		t.Errorf("token --paste output: %q", o.String())
	}
	fi, err := os.Stat(file)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(file); !bytes.Contains(b, []byte(cliTestToken)) {
		t.Error("the file does not hold the token")
	}

	_, out, errs = atl(t, "doctor")
	all.WriteString(out + errs)
	if !strings.Contains(out, "claude token") || !strings.Contains(out, "from "+file+": stored by `atl token` on ") ||
		!strings.Contains(out, "0 days old") {
		t.Errorf("doctor with a stored token:\n%s", out)
	}
	if strings.Contains(all.String(), cliTestToken) || strings.Contains(all.String(), cliTestToken[20:50]) {
		t.Errorf("the token was printed:\n%s", all.String())
	}
}
