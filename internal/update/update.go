// Package update finds atl's latest release on GitHub and replaces the
// running binary with it (`atl update`, the TUI's U).
//
// Every merge to main is released by .github/workflows/release.yml: a
// static binary per architecture, SHA256SUMS, and SHA256SUMS.sig, an
// Ed25519 signature of SignedMessage made with the release key. That key
// is a secret of the repository's "release" environment, which only main
// may use, so a workflow on a pushed task branch cannot sign. Install takes
// a binary only when the signature checks out with a public key built into
// atl (release-key.pub) and the binary's checksum is the signed one.
package update

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/andreujuanc/atelier/internal/sandbox"
	"github.com/andreujuanc/atelier/internal/version"
)

// Where releases come from, and what they hold besides the binaries.
const (
	Repo      = "andreujuanc/atelier"
	SumsAsset = "SHA256SUMS"
	SigAsset  = "SHA256SUMS.sig"
)

// CheckVar turns the TUI's background check for a newer release off when
// set to "off" (`atl update` still works).
const CheckVar = "ATL_UPDATE_CHECK"

// CheckEvery is how old the cached result of the TUI's check may be.
const CheckEvery = 6 * time.Hour

// maxBinary bounds a downloaded binary, maxSmall SHA256SUMS.
const (
	maxBinary = 512 << 20
	maxSmall  = 4 << 20
)

// errNotFound is a download GitHub answered with 404 Not Found.
var errNotFound = errors.New("not found")

// CheckEnabled reports whether the TUI checks for newer releases: not for
// a dev build (nothing to compare with), not when $ATL_UPDATE_CHECK=off.
func CheckEnabled() bool {
	return version.Current() != version.Dev && os.Getenv(CheckVar) != "off"
}

// AssetName is the release asset of atl for GOARCH arch.
func AssetName(arch string) string { return "atl-linux-" + arch }

// Release is a GitHub release: its tag, and where its assets are.
type Release struct {
	Tag  string
	base string // <web>/<repo>/releases/download/<tag>
}

// URL is where the release's asset name is.
func (r *Release) URL(name string) string { return r.base + "/" + url.PathEscape(name) }

// Updater talks to GitHub's web pages, not its API: the API answers 60
// requests an hour to everyone behind one address, and nothing here needs
// it. The zero value is not usable: see New.
type Updater struct {
	Web   string // GitHub, without a trailing slash
	HTTP  *http.Client
	Arch  string // GOARCH of the binary to install
	Now   func() time.Time
	Cache string // the check's cache file ("" none)
	// Keys are the public keys a release's signature may be made with
	// (ReleaseKeys); none: Install refuses.
	Keys []ed25519.PublicKey
	// CheckBin checks that path is a binary task containers can run
	// (sandbox.CheckStaticBin).
	CheckBin func(path string) error
	// RunVersion runs `<path> version` and returns its output.
	RunVersion func(ctx context.Context, path string) (string, error)
}

// New returns the Updater for atelier's GitHub releases.
func New() *Updater {
	return &Updater{
		Web:        "https://github.com",
		HTTP:       &http.Client{Timeout: 5 * time.Minute},
		Arch:       runtime.GOARCH,
		Now:        time.Now,
		Cache:      cachePath(),
		Keys:       ReleaseKeys(),
		CheckBin:   sandbox.CheckStaticBin,
		RunVersion: runVersion,
	}
}

// cachePath is $XDG_STATE_HOME/atelier/update-check.json (default
// ~/.local/state): one per user, shared by every workspace's TUI.
func cachePath() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" || !filepath.IsAbs(dir) {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "atelier", "update-check.json")
}

// Latest finds the latest release: github.com/<repo>/releases/latest
// redirects to .../releases/tag/<tag> (to .../releases while there is
// none).
func (u *Updater) Latest(ctx context.Context) (*Release, error) {
	page := u.Web + "/" + Repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, "GET", page, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "atl/"+version.Current())
	c := *u.HTTP
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("latest release: %w", err)
	}
	resp.Body.Close()
	loc, err := resp.Location()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errors.New("latest release: no release yet")
	case resp.StatusCode < 300 || resp.StatusCode > 399 || err != nil:
		return nil, fmt.Errorf("latest release: GET %s: %s, not a redirect to the release", page, resp.Status)
	}
	escaped, found := strings.CutPrefix(loc.EscapedPath(), "/"+Repo+"/releases/tag/")
	if !found {
		return nil, errors.New("latest release: no release yet")
	}
	tag, err := url.PathUnescape(escaped)
	if err != nil {
		return nil, fmt.Errorf("latest release: %s: %w", loc, err)
	}
	if _, ok := version.Parse(tag); !ok {
		return nil, fmt.Errorf("latest release: tag %q is not a version (vMAJOR.MINOR.PATCH)", tag)
	}
	return &Release{Tag: tag, base: u.Web + "/" + Repo + "/releases/download/" + url.PathEscape(tag)}, nil
}

type cached struct {
	Checked time.Time `json:"checked"`
	Latest  string    `json:"latest"`
}

// Check returns the latest release's tag, from the cache when it is
// younger than maxAge, else from GitHub (then cached).
func (u *Updater) Check(ctx context.Context, maxAge time.Duration) (string, error) {
	if u.Cache != "" {
		if data, err := os.ReadFile(u.Cache); err == nil {
			var c cached
			if json.Unmarshal(data, &c) == nil && c.Latest != "" {
				if age := u.Now().Sub(c.Checked); age >= 0 && age < maxAge {
					return c.Latest, nil
				}
			}
		}
	}
	rel, err := u.Latest(ctx)
	if err != nil {
		return "", err
	}
	u.saveCache(rel.Tag)
	return rel.Tag, nil
}

// saveCache records latest as checked now; failures only cost a request.
func (u *Updater) saveCache(latest string) {
	if u.Cache == "" {
		return
	}
	data, err := json.Marshal(cached{Checked: u.Now().UTC(), Latest: latest})
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(u.Cache), 0o755) != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(u.Cache), ".update-check-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(append(data, '\n'))
	if cerr := tmp.Close(); werr != nil || cerr != nil || os.Rename(tmp.Name(), u.Cache) != nil {
		os.Remove(tmp.Name())
	}
}

// Target is the binary atl update replaces, symlinks resolved: the one
// atl runs hooks with, gives task containers and re-executes ($ATL_BIN,
// else the running one).
func Target() (string, error) {
	bin := os.Getenv("ATL_BIN")
	if bin == "" {
		var err error
		if bin, err = os.Executable(); err != nil {
			return "", fmt.Errorf("locate the running atl: %w", err)
		}
	}
	bin, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return "", fmt.Errorf("locate the running atl: %w", err)
	}
	return bin, nil
}

// Install replaces target with rel's binary for u.Arch, once SHA256SUMS
// carries a valid signature for rel.Tag (VerifySums), the binary's
// checksum is the one it lists, it is a static Linux binary and it says
// it is rel.Tag. The new file replaces target in one rename: running atl
// processes go on with the old one.
func (u *Updater) Install(ctx context.Context, rel *Release, target string) error {
	name := AssetName(u.Arch)
	sums, err := u.get(ctx, rel.URL(SumsAsset), maxSmall)
	if errors.Is(err, errNotFound) {
		return fmt.Errorf("release %s has no %s", rel.Tag, SumsAsset)
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", SumsAsset, err)
	}
	sig, err := u.get(ctx, rel.URL(SigAsset), 1024)
	if errors.Is(err, errNotFound) {
		return fmt.Errorf("release %s has no %s (it is not signed)", rel.Tag, SigAsset)
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", SigAsset, err)
	}
	if err := VerifySums(u.Keys, rel.Tag, sums, sig); err != nil {
		return fmt.Errorf("release %s: %w", rel.Tag, err)
	}
	want, err := sumFor(sums, name)
	if err != nil {
		return fmt.Errorf("release %s: %w (no build for linux/%s)", rel.Tag, err, u.Arch)
	}

	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".atl-update-*")
	if err != nil {
		return fmt.Errorf("cannot write next to %s: %w", target, err)
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	h := sha256.New()
	if err := u.download(ctx, rel.URL(name), io.MultiWriter(tmp, h)); err != nil {
		if errors.Is(err, errNotFound) {
			return fmt.Errorf("release %s has no %s (no build for linux/%s)", rel.Tag, name, u.Arch)
		}
		return fmt.Errorf("download %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("download %s: checksum %s, %s says %s", name, got, SumsAsset, want)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	if err := u.CheckBin(tmp.Name()); err != nil {
		return fmt.Errorf("release %s: %w", rel.Tag, err)
	}
	// Signed and checked: only now is it ours to run.
	out, err := u.RunVersion(ctx, tmp.Name())
	if err != nil {
		return fmt.Errorf("release %s: run its atl version: %w", rel.Tag, err)
	}
	if got := strings.TrimSpace(out); got != rel.Tag {
		return fmt.Errorf("release %s: its atl says it is %q", rel.Tag, got)
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return fmt.Errorf("replace %s: %w", target, err)
	}
	ok = true
	return nil
}

// sumFor finds name's SHA-256 in a sha256sum listing.
func sumFor(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			if b, err := hex.DecodeString(f[0]); err == nil && len(b) == sha256.Size {
				return strings.ToLower(f[0]), nil
			}
		}
	}
	return "", fmt.Errorf("%s has no checksum for %s", SumsAsset, name)
}

// request GETs addr (following redirects: assets are served from another
// host); 404 is errNotFound.
func (u *Updater) request(ctx context.Context, addr string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", addr, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "atl/"+version.Current())
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("GET %s: %w", addr, errNotFound)
		}
		return nil, fmt.Errorf("GET %s: %s", addr, resp.Status)
	}
	return resp, nil
}

func (u *Updater) get(ctx context.Context, addr string, limit int64) ([]byte, error) {
	resp, err := u.request(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("GET %s: more than %d bytes", addr, limit)
	}
	return data, nil
}

func (u *Updater) download(ctx context.Context, addr string, w io.Writer) error {
	resp, err := u.request(ctx, addr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	n, err := io.Copy(w, io.LimitReader(resp.Body, maxBinary+1))
	if err != nil {
		return err
	}
	if n > maxBinary {
		return fmt.Errorf("more than %d bytes", maxBinary)
	}
	return nil
}

func runVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	return string(out), err
}
