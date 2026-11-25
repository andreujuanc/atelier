// Package update finds atl's latest release on GitHub and replaces the
// running binary with it (`atl update`, the TUI's U).
//
// Every merge to main is released by .github/workflows/release.yml: a
// static binary per architecture, SHA256SUMS, and a build provenance
// attestation. Install takes a binary only when its checksum matches and
// its attestation says it was built by that workflow from main of the
// atelier repository (`gh attestation verify`): a release made any other
// way (e.g. by a workflow on a pushed task branch) is refused.
package update

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/andreujuanc/atelier/internal/sandbox"
	"github.com/andreujuanc/atelier/internal/version"
)

// Where releases come from, and what their attestation must say.
const (
	Repo      = "andreujuanc/atelier"
	Workflow  = Repo + "/.github/workflows/release.yml"
	SourceRef = "refs/heads/main"
	SumsAsset = "SHA256SUMS"
)

// CheckVar turns the TUI's background check for a newer release off when
// set to "off" (`atl update` still works).
const CheckVar = "ATL_UPDATE_CHECK"

// CheckEvery is how old the cached result of the TUI's check may be.
const CheckEvery = 6 * time.Hour

// maxBinary bounds a downloaded binary, maxSmall the API answer and
// SHA256SUMS.
const (
	maxBinary = 512 << 20
	maxSmall  = 4 << 20
)

// CheckEnabled reports whether the TUI checks for newer releases: not for
// a dev build (nothing to compare with), not when $ATL_UPDATE_CHECK=off.
func CheckEnabled() bool {
	return version.Current() != version.Dev && os.Getenv(CheckVar) != "off"
}

// AssetName is the release asset of atl for GOARCH arch.
func AssetName(arch string) string { return "atl-linux-" + arch }

// Release is a GitHub release: its tag and its assets' download URLs.
type Release struct {
	Tag    string
	Assets map[string]string
}

// Updater talks to GitHub. The zero value is not usable: see New.
type Updater struct {
	API   string // GitHub's API, without a trailing slash
	HTTP  *http.Client
	Arch  string // GOARCH of the binary to install
	Now   func() time.Time
	Cache string // the check's cache file ("" none)
	// Verify checks path's build provenance (see GhVerify).
	Verify func(ctx context.Context, path string) error
	// CheckBin checks that path is a binary task containers can run
	// (sandbox.CheckStaticBin).
	CheckBin func(path string) error
	// RunVersion runs `<path> version` and returns its output.
	RunVersion func(ctx context.Context, path string) (string, error)
}

// New returns the Updater for atelier's GitHub releases.
func New() *Updater {
	return &Updater{
		API:        "https://api.github.com",
		HTTP:       &http.Client{Timeout: 5 * time.Minute},
		Arch:       runtime.GOARCH,
		Now:        time.Now,
		Cache:      cachePath(),
		Verify:     GhVerify,
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

// Latest asks GitHub for the latest release.
func (u *Updater) Latest(ctx context.Context) (*Release, error) {
	body, err := u.get(ctx, u.API+"/repos/"+Repo+"/releases/latest", maxSmall)
	if err != nil {
		return nil, fmt.Errorf("latest release: %w", err)
	}
	var r struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("latest release: %w", err)
	}
	if _, ok := version.Parse(r.TagName); !ok {
		return nil, fmt.Errorf("latest release: tag %q is not a version (vMAJOR.MINOR.PATCH)", r.TagName)
	}
	rel := &Release{Tag: r.TagName, Assets: map[string]string{}}
	for _, a := range r.Assets {
		rel.Assets[a.Name] = a.URL
	}
	return rel, nil
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

// Install replaces target with rel's binary for u.Arch, once its checksum
// matches SHA256SUMS, it is a static Linux binary, its attestation checks
// out (u.Verify) and it says it is rel.Tag. The new file replaces target
// in one rename: running atl processes go on with the old one.
func (u *Updater) Install(ctx context.Context, rel *Release, target string) error {
	name := AssetName(u.Arch)
	binURL, ok := rel.Assets[name]
	if !ok {
		return fmt.Errorf("release %s has no %s (no build for linux/%s)", rel.Tag, name, u.Arch)
	}
	sumsURL, ok := rel.Assets[SumsAsset]
	if !ok {
		return fmt.Errorf("release %s has no %s", rel.Tag, SumsAsset)
	}
	sums, err := u.get(ctx, sumsURL, maxSmall)
	if err != nil {
		return fmt.Errorf("download %s: %w", SumsAsset, err)
	}
	want, err := sumFor(sums, name)
	if err != nil {
		return fmt.Errorf("release %s: %w", rel.Tag, err)
	}

	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".atl-update-*")
	if err != nil {
		return fmt.Errorf("cannot write next to %s: %w", target, err)
	}
	ok = false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	h := sha256.New()
	if err := u.download(ctx, binURL, io.MultiWriter(tmp, h)); err != nil {
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
	if err := u.Verify(ctx, tmp.Name()); err != nil {
		return fmt.Errorf("release %s: %w", rel.Tag, err)
	}
	// Only now is it ours to run.
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

func (u *Updater) request(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "atl/"+version.Current())
	if strings.HasPrefix(url, u.API+"/") {
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound && strings.HasSuffix(url, "/releases/latest") {
			return nil, errors.New("no release yet")
		}
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp, nil
}

func (u *Updater) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	resp, err := u.request(ctx, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("GET %s: more than %d bytes", url, limit)
	}
	return data, nil
}

func (u *Updater) download(ctx context.Context, url string, w io.Writer) error {
	resp, err := u.request(ctx, url)
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

// GhVerify checks with `gh attestation verify` that path was built by
// atelier's release workflow from main, on a GitHub-hosted runner.
func GhVerify(ctx context.Context, path string) error {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return errors.New("gh not found on PATH: atl update checks where the release was built with `gh attestation verify`")
	}
	cmd := exec.CommandContext(ctx, gh, "attestation", "verify", path,
		"--repo", Repo,
		"--signer-workflow", Workflow,
		"--source-ref", SourceRef,
		"--deny-self-hosted-runners")
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("its build attestation does not check out (built by %s from %s?): %s", Workflow, SourceRef, msg)
	}
	return nil
}

func runVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	return string(out), err
}
