package update

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func pub(k ed25519.PrivateKey) ed25519.PublicKey { return k.Public().(ed25519.PublicKey) }

// fakeGitHub serves a latest release tag with assets atl-linux-arm64
// (bin), SHA256SUMS (sums; computed from bin when empty) and
// SHA256SUMS.sig (made with key for signTag, default tag; sig if set).
type fakeGitHub struct {
	tag      string
	bin      []byte
	sums     string
	noAssets bool
	key      ed25519.PrivateKey
	signTag  string
	sig      []byte
	noSig    bool
	calls    atomic.Int32
}

func (f *fakeGitHub) sumsText() string {
	if f.sums != "" {
		return f.sums
	}
	h := sha256.Sum256(f.bin)
	return hex.EncodeToString(h[:]) + "  atl-linux-amd64\n" + hex.EncodeToString(h[:]) + " *atl-linux-arm64\n"
}

func (f *fakeGitHub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + Repo + "/releases/latest":
			f.calls.Add(1)
			if r.Header.Get("User-Agent") == "" {
				http.Error(w, "no user agent", http.StatusForbidden)
				return
			}
			if f.tag == "" {
				http.NotFound(w, r)
				return
			}
			assets := fmt.Sprintf(`{"name":"atl-linux-arm64","browser_download_url":"%[1]s/dl/bin"},{"name":"SHA256SUMS","browser_download_url":"%[1]s/dl/sums"}`, srv.URL)
			if !f.noSig {
				assets += fmt.Sprintf(`,{"name":"SHA256SUMS.sig","browser_download_url":"%s/dl/sig"}`, srv.URL)
			}
			if f.noAssets {
				assets = ""
			}
			fmt.Fprintf(w, `{"tag_name":%q,"assets":[%s]}`, f.tag, assets)
		case "/dl/bin":
			w.Write(f.bin)
		case "/dl/sums":
			fmt.Fprint(w, f.sumsText())
		case "/dl/sig":
			if f.sig != nil {
				w.Write(f.sig)
				return
			}
			tag := f.signTag
			if tag == "" {
				tag = f.tag
			}
			w.Write(ed25519.Sign(f.key, SignedMessage(tag, []byte(f.sumsText()))))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// testUpdater talks to srv and trusts key; the binary "runs" by printing
// its contents' first line (RunVersion).
func testUpdater(t *testing.T, srv *httptest.Server, key ed25519.PrivateKey) *Updater {
	t.Helper()
	return &Updater{
		API:      srv.URL,
		HTTP:     srv.Client(),
		Arch:     "arm64",
		Now:      func() time.Time { return time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC) },
		Cache:    filepath.Join(t.TempDir(), "state", "update-check.json"),
		Keys:     []ed25519.PublicKey{pub(key)},
		CheckBin: func(string) error { return nil },
		RunVersion: func(_ context.Context, path string) (string, error) {
			data, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			line, _, _ := strings.Cut(string(data), "\n")
			return line + "\n", nil
		},
	}
}

func target(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "atl")
	if err := os.WriteFile(p, []byte("old\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInstall(t *testing.T) {
	key := newKey(t)
	gh := &fakeGitHub{tag: "v0.1.5", bin: []byte("v0.1.5\nbinary"), key: key}
	u := testUpdater(t, gh.serve(t), key)
	rel, err := u.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dst := target(t)
	if err := u.Install(context.Background(), rel, dst); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != string(gh.bin) {
		t.Errorf("target = %q, want the release binary", got)
	}
	if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o755 {
		t.Errorf("mode %v, want 0755", fi.Mode().Perm())
	}
	assertNoTemp(t, dst)
}

// TestInstallRotation: during a key rotation atl trusts both keys and
// installs a release signed with either.
func TestInstallRotation(t *testing.T) {
	oldKey, newKey := newKey(t), newKey(t)
	gh := &fakeGitHub{tag: "v0.1.5", bin: []byte("v0.1.5\n"), key: newKey}
	u := testUpdater(t, gh.serve(t), oldKey)
	u.Keys = append(u.Keys, pub(newKey))
	rel, err := u.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Install(context.Background(), rel, target(t)); err != nil {
		t.Fatal(err)
	}
}

// TestInstallRefuses: whatever fails, the target stays as it was, no temp
// file is left, and a binary is never run unless its release is signed.
func TestInstallRefuses(t *testing.T) {
	key, other := newKey(t), newKey(t)
	for _, c := range []struct {
		name  string
		gh    *fakeGitHub
		setup func(u *Updater)
		want  string
	}{
		{"unsigned", &fakeGitHub{tag: "v0.1.5", bin: []byte("v0.1.5\n"), noSig: true}, nil, "has no SHA256SUMS.sig (it is not signed)"},
		{"other key", &fakeGitHub{tag: "v0.1.5", bin: []byte("v0.1.5\n"), key: other}, nil, "does not check out with atl's release key"},
		{"signed for another tag", &fakeGitHub{tag: "v0.1.5", bin: []byte("v0.1.4\n"), key: key, signTag: "v0.1.4"}, nil, "does not check out with atl's release key"},
		{"garbage signature", &fakeGitHub{tag: "v0.1.5", bin: []byte("v0.1.5\n"), sig: bytes.Repeat([]byte{7}, ed25519.SignatureSize)}, nil, "does not check out"},
		{"short signature", &fakeGitHub{tag: "v0.1.5", bin: []byte("v0.1.5\n"), sig: []byte("sig\n")}, nil, "not an Ed25519 signature"},
		{"no release key", &fakeGitHub{tag: "v0.1.5", bin: []byte("v0.1.5\n"), key: key}, func(u *Updater) { u.Keys = nil }, "no release key"},
		{"checksum", &fakeGitHub{tag: "v0.1.5", bin: []byte("v0.1.5\n"), key: key, sums: strings.Repeat("ab", 32) + "  atl-linux-arm64\n"}, nil, "checksum"},
		{"no checksum line", &fakeGitHub{tag: "v0.1.5", bin: []byte("v0.1.5\n"), key: key, sums: strings.Repeat("ab", 32) + "  atl-linux-amd64\n"}, nil, "no checksum for atl-linux-arm64"},
		{"no build for arch", &fakeGitHub{tag: "v0.1.5", bin: []byte("v0.1.5\n"), key: key}, func(u *Updater) { u.Arch = "riscv64" }, "no build for linux/riscv64"},
		{"no assets", &fakeGitHub{tag: "v0.1.5", noAssets: true}, nil, "has no atl-linux-arm64"},
		{"not static", &fakeGitHub{tag: "v0.1.5", bin: []byte("v0.1.5\n"), key: key}, func(u *Updater) {
			u.CheckBin = func(p string) error { return fmt.Errorf("%s is dynamically linked", p) }
		}, "dynamically linked"},
		{"wrong version", &fakeGitHub{tag: "v0.1.5", bin: []byte("v0.1.4\n"), key: key}, nil, `says it is "v0.1.4"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := testUpdater(t, c.gh.serve(t), key)
			badSig := c.gh.noSig || c.gh.sig != nil || c.gh.key == nil || !bytes.Equal(c.gh.key, key) || c.gh.signTag != ""
			if badSig {
				// Not validly signed: its binary must never run.
				u.RunVersion = func(context.Context, string) (string, error) { panic("ran a binary from an unsigned release") }
			}
			if c.setup != nil {
				c.setup(u)
			}
			rel, err := u.Latest(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			dst := target(t)
			err = u.Install(context.Background(), rel, dst)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Install error %v, want one containing %q", err, c.want)
			}
			if got, _ := os.ReadFile(dst); string(got) != "old\n" {
				t.Errorf("target changed to %q", got)
			}
			assertNoTemp(t, dst)
		})
	}
}

func assertNoTemp(t *testing.T, dst string) {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Dir(dst))
	for _, e := range entries {
		if e.Name() != filepath.Base(dst) {
			t.Errorf("left %s behind", e.Name())
		}
	}
}

func pemOf(t *testing.T, k any) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func TestParseKeys(t *testing.T) {
	a, b := newKey(t), newKey(t)
	keys, err := ParseKeys([]byte("# comment\n" + pemOf(t, pub(a)) + "\nmore text\n" + pemOf(t, pub(b))))
	if err != nil || len(keys) != 2 || !keys[0].Equal(pub(a)) || !keys[1].Equal(pub(b)) {
		t.Errorf("ParseKeys = %d keys, %v", len(keys), err)
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseKeys([]byte(pemOf(t, &ec.PublicKey))); err == nil {
		t.Error("an ECDSA key was accepted")
	}
	// The key built into atl parses.
	if _, err := ParseKeys(releaseKeyPEM); err != nil {
		t.Errorf("release-key.pub: %v", err)
	}
}

func TestLatest(t *testing.T) {
	gh := &fakeGitHub{}
	u := testUpdater(t, gh.serve(t), newKey(t))
	if _, err := u.Latest(context.Background()); err == nil || !strings.Contains(err.Error(), "no release yet") {
		t.Errorf("no release: err %v", err)
	}
	gh.tag = "nightly"
	if _, err := u.Latest(context.Background()); err == nil || !strings.Contains(err.Error(), "not a version") {
		t.Errorf("bad tag: err %v", err)
	}
}

func TestCheckCaches(t *testing.T) {
	gh := &fakeGitHub{tag: "v0.1.5"}
	u := testUpdater(t, gh.serve(t), newKey(t))
	now := u.Now()
	ctx := context.Background()
	for i, c := range []struct {
		at    time.Duration // after now
		tag   string        // GitHub's latest by then
		want  string
		calls int32
	}{
		{0, "v0.1.5", "v0.1.5", 1},
		{time.Hour, "v0.1.6", "v0.1.5", 1},                // cached
		{CheckEvery + time.Minute, "v0.1.6", "v0.1.6", 2}, // stale: asks again
		{-time.Hour, "v0.1.7", "v0.1.7", 3},               // clock went back: asks again
	} {
		gh.tag = c.tag
		u.Now = func() time.Time { return now.Add(c.at) }
		got, err := u.Check(ctx, CheckEvery)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want || gh.calls.Load() != c.calls {
			t.Errorf("%d: Check = %q after %d calls, want %q after %d", i, got, gh.calls.Load(), c.want, c.calls)
		}
	}
}
