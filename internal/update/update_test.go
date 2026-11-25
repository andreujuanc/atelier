package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

// fakeGitHub serves a latest release tag with assets atl-linux-arm64
// (bin) and SHA256SUMS (sums; computed from bin when empty).
type fakeGitHub struct {
	tag      string
	bin      []byte
	sums     string
	noAssets bool
	calls    atomic.Int32
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
			assets := fmt.Sprintf(`[{"name":"atl-linux-arm64","browser_download_url":"%[1]s/dl/bin"},{"name":"SHA256SUMS","browser_download_url":"%[1]s/dl/sums"}]`, srv.URL)
			if f.noAssets {
				assets = "[]"
			}
			fmt.Fprintf(w, `{"tag_name":%q,"assets":%s}`, f.tag, assets)
		case "/dl/bin":
			w.Write(f.bin)
		case "/dl/sums":
			sums := f.sums
			if sums == "" {
				h := sha256.Sum256(f.bin)
				sums = hex.EncodeToString(h[:]) + "  atl-linux-amd64\n" + hex.EncodeToString(h[:]) + " *atl-linux-arm64\n"
			}
			fmt.Fprint(w, sums)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// testUpdater talks to srv; the binary "runs" by printing its contents'
// first line (RunVersion), and its attestation checks out unless verify
// says otherwise.
func testUpdater(t *testing.T, srv *httptest.Server) *Updater {
	t.Helper()
	return &Updater{
		API:      srv.URL,
		HTTP:     srv.Client(),
		Arch:     "arm64",
		Now:      func() time.Time { return time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC) },
		Cache:    filepath.Join(t.TempDir(), "state", "update-check.json"),
		Verify:   func(context.Context, string) error { return nil },
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
	gh := &fakeGitHub{tag: "v0.1.5", bin: []byte("v0.1.5\nbinary")}
	u := testUpdater(t, gh.serve(t))
	var verified string
	u.Verify = func(_ context.Context, path string) error { verified = path; return nil }
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
	if verified == "" || verified == dst {
		t.Errorf("verified %q: want the download, before it replaced the target", verified)
	}
	assertNoTemp(t, dst)
}

// TestInstallRefuses: whatever fails, the target stays as it was and no
// temp file is left.
func TestInstallRefuses(t *testing.T) {
	for _, c := range []struct {
		name  string
		gh    *fakeGitHub
		setup func(u *Updater)
		want  string
	}{
		{"checksum", &fakeGitHub{tag: "v0.1.5", bin: []byte("v0.1.5\n"), sums: strings.Repeat("ab", 32) + "  atl-linux-arm64\n"}, nil, "checksum"},
		{"no checksum line", &fakeGitHub{tag: "v0.1.5", bin: []byte("v0.1.5\n"), sums: strings.Repeat("ab", 32) + "  atl-linux-amd64\n"}, nil, "no checksum for atl-linux-arm64"},
		{"no build for arch", &fakeGitHub{tag: "v0.1.5", bin: []byte("v0.1.5\n")}, func(u *Updater) { u.Arch = "riscv64" }, "no build for linux/riscv64"},
		{"no assets", &fakeGitHub{tag: "v0.1.5", noAssets: true}, nil, "has no atl-linux-arm64"},
		{"attestation", &fakeGitHub{tag: "v0.1.5", bin: []byte("v0.1.5\n")}, func(u *Updater) {
			u.Verify = func(context.Context, string) error { return errors.New("attestation does not check out") }
		}, "attestation does not check out"},
		{"not static", &fakeGitHub{tag: "v0.1.5", bin: []byte("v0.1.5\n")}, func(u *Updater) {
			u.CheckBin = func(p string) error { return fmt.Errorf("%s is dynamically linked", p) }
		}, "dynamically linked"},
		{"wrong version", &fakeGitHub{tag: "v0.1.5", bin: []byte("v0.1.4\n")}, nil, `says it is "v0.1.4"`},
		{"runs only after verify", &fakeGitHub{tag: "v0.1.5", bin: []byte("v0.1.5\n")}, func(u *Updater) {
			u.Verify = func(context.Context, string) error { return errors.New("bad provenance") }
			u.RunVersion = func(context.Context, string) (string, error) { panic("ran an unverified binary") }
		}, "bad provenance"},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := testUpdater(t, c.gh.serve(t))
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

func TestLatest(t *testing.T) {
	gh := &fakeGitHub{}
	u := testUpdater(t, gh.serve(t))
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
	u := testUpdater(t, gh.serve(t))
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
