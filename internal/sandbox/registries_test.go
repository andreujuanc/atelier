package sandbox

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestParseRegistries(t *testing.T) {
	got, err := ParseRegistries(" registry.corp:5000,*.pkg.dev\nLocalhost:5000 ,, ")
	if want := []string{"registry.corp:5000", "*.pkg.dev", "Localhost:5000"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("ParseRegistries = %q, %v; want %q", got, err, want)
	}
	for _, bad := range []string{"docker.io/library", "*", "*.", "*.*.corp", "a..b", "-a.b", "a.b:port", "http://a.b", "a.b:", "a_b.c"} {
		if _, err := ParseRegistries(bad); err == nil {
			t.Errorf("ParseRegistries(%q): no error", bad)
		}
	}
}

// TestGatePullRegistries: the daemon pulls from the network of the host it
// runs on (a registry on its loopback, the LAN, a cloud's metadata
// address), so a task may pull only from the default registries and those
// of [sandbox] registries: a domain (with its port, if any) in any case,
// Docker Hub by any of its names, or "*." and a domain for those under it.
// A look at an image in its registry (/distribution) too.
func TestGatePullRegistries(t *testing.T) {
	extra := []string{"registry.corp:5000", "*.pkg.dev", "*.lan:5000"}
	c := newGateWith(t, func(g *Gate) { g.Registries = func() ([]string, error) { return extra, nil } })
	pull := func(ref string) string { return "/images/create?fromImage=" + ref + "&tag=1" }
	for _, ref := range []string{"postgres", "library/postgres", "docker.io/library/postgres", "index.docker.io/x/y",
		"registry-1.docker.io/library/alpine", "Docker.IO/x/y", "GHCR.IO/a/b", "quay.io/a/b", "mcr.microsoft.com/a/b",
		"gcr.io/p/i", "registry.k8s.io/pause", "public.ecr.aws/a/b",
		"registry.corp:5000/x", "REGISTRY.Corp:5000/x", "europe-docker.pkg.dev/p/r/i", "a.b.pkg.dev/x", "reg.lan:5000/x"} {
		if status, out := c.do("POST", pull(ref), ""); status >= 400 {
			t.Errorf("pull %s: %d %s", ref, status, out)
		}
	}
	if status, out := c.do("GET", "/distribution/ghcr.io/a/b:1/json", ""); status >= 400 {
		t.Errorf("distribution ghcr.io: %d %s", status, out)
	}
	for _, ref := range []string{"localhost:5300/probe", "127.0.0.1:5300/x", "localhost/x", "169.254.169.254/x",
		"[::1]:5000/x", "example.com/x", "registry.corp/x", "registry.corp:5001/x", "pkg.dev/x", "a.pkg.dev:443/x",
		"reg.lan/x", "docker.io.evil.com/x", "ghcr.io:443/x"} {
		before := len(c.d.requests)
		status, out := c.do("POST", pull(ref), "")
		if status < 400 {
			t.Errorf("pull %s: %d, want it refused", ref, status)
		}
		if len(c.d.requests) != before {
			t.Errorf("pull %s reached the daemon: %v", ref, c.d.requests[before:])
		}
		if strings.Contains(ref, ":5300") && !strings.Contains(out, "[sandbox] registries") {
			t.Errorf("pull %s: refusal does not name the setting: %s", ref, out)
		}
	}
	for _, uri := range []string{"/distribution/127.0.0.1:5301/x/json", "/v1.45/distribution/localhost:5000/x:1/json"} {
		if status, _ := c.do("GET", uri, ""); status != http.StatusForbidden {
			t.Errorf("%s: %d, want refused", uri, status)
		}
	}

	// Without extras: the defaults only; an unreadable setting refuses.
	c = newGate(t)
	if status, _ := c.do("POST", pull("registry.corp:5000/x"), ""); status != http.StatusForbidden {
		t.Errorf("extra registry with no setting: %d", status)
	}
	if status, _ := c.do("POST", pull("alpine"), ""); status >= 400 {
		t.Errorf("default registry with no setting: %d", status)
	}
	for _, f := range []func() ([]string, error){
		func() ([]string, error) { return nil, errors.New("broken atelier.toml") },
		func() ([]string, error) { return []string{"bad/entry"}, nil },
	} {
		c = newGateWith(t, func(g *Gate) { g.Registries = f })
		if status, out := c.do("POST", pull("alpine"), ""); status != http.StatusForbidden || !strings.Contains(out, "registries") {
			t.Errorf("broken setting: %d %s", status, out)
		}
	}
}
