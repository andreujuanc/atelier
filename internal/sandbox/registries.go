package sandbox

import (
	"fmt"
	"strings"
	"unicode"
)

// The daemon pulls an image from the registry its name gives (and follows
// the auth realm and redirects that registry hands it), from the network of
// the host it runs on: a task that names a registry on the host's loopback,
// the LAN or a cloud's metadata address has the daemon reach it, though the
// task's container cannot. The gate cannot see the daemon's HTTP traffic,
// only the image names, so a task may pull (docker pull, a build's FROM and
// COPY --from) only from the registries the user trusts: DefaultRegistries
// and the workspace's RegistriesVar.

// RegistriesVar names the workspace setting ([sandbox] registries) that
// adds registries tasks may pull from to DefaultRegistries: entries
// separated by commas or blanks.
const RegistriesVar = "ATL_SANDBOX_REGISTRIES"

// DefaultRegistries are the registries every task may pull from.
// registry-1.docker.io and index.docker.io are docker.io (see
// canonicalRegistry).
var DefaultRegistries = []string{"docker.io", "ghcr.io", "quay.io", "mcr.microsoft.com", "gcr.io", "registry.k8s.io", "public.ecr.aws"}

// ParseRegistries reads the value of RegistriesVar: entries separated by
// commas or blanks (an entry has a ':' before its port), each checked by
// CheckRegistry.
func ParseRegistries(s string) ([]string, error) {
	list := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	for _, e := range list {
		if err := CheckRegistry(e); err != nil {
			return nil, err
		}
	}
	return list, nil
}

// CheckRegistry checks an entry of the registries tasks may pull from: a
// registry's domain, with its port if it is not the default one
// ("registry.corp:5000"), in any case; or "*." and a domain, for every
// domain under it ("*.pkg.dev": "europe-docker.pkg.dev", not "pkg.dev").
func CheckRegistry(e string) error {
	d := strings.TrimPrefix(e, "*.")
	if !refDomain.MatchString(d) || strings.HasPrefix(d, "*") {
		return fmt.Errorf("registry %q: want a domain with an optional port (\"registry.corp:5000\"), or \"*.\" and a domain", e)
	}
	return nil
}

// canonicalRegistry is how the gate compares registries: in lower case,
// Docker Hub's other names (index.docker.io, which dockerd reads as
// docker.io, and registry-1.docker.io, where docker.io is served) as
// docker.io.
func canonicalRegistry(d string) string {
	d = strings.ToLower(d)
	if d == "index.docker.io" || d == "registry-1.docker.io" {
		return "docker.io"
	}
	return d
}

// registryMatches reports whether domain (with its port, if any) is one of
// entries (see CheckRegistry).
func registryMatches(domain string, entries []string) bool {
	domain = canonicalRegistry(domain)
	for _, e := range entries {
		if wild, ok := strings.CutPrefix(e, "*."); ok {
			// "*.pkg.dev" covers "a.pkg.dev" (without a port);
			// "*.corp:5000", "a.corp:5000".
			if strings.HasSuffix(domain, "."+strings.ToLower(wild)) {
				return true
			}
			continue
		}
		if canonicalRegistry(e) == domain {
			return true
		}
	}
	return false
}

// checkRegistry returns why the task may not have the daemon pull image
// ref (read as dockerd does) from its registry, or nil.
func (g *Gate) checkRegistry(ref imageRef) error {
	entries := DefaultRegistries
	if g.Registries != nil {
		more, err := g.Registries()
		if err != nil {
			return fmt.Errorf("cannot read the registries tasks may pull from ([sandbox] registries): %v", err)
		}
		for _, e := range more {
			if err := CheckRegistry(e); err != nil {
				return fmt.Errorf("[sandbox] registries: %v", err)
			}
		}
		entries = append(append([]string(nil), entries...), more...)
	}
	domain := ref.domain()
	if registryMatches(domain, entries) {
		return nil
	}
	return &registryRefusal{domain}
}

// registryRefusal is an image whose registry tasks may not pull from.
type registryRefusal struct{ domain string }

func (e *registryRefusal) Error() string {
	return fmt.Sprintf("registry %s is not one tasks may pull from (the daemon would reach it from the host's network): "+
		"the user can add it to [sandbox] registries in atelier.toml or $%s", e.domain, RegistriesVar)
}

// domain is the registry r is pulled from, with its port if any.
func (r imageRef) domain() string {
	d, _, _ := strings.Cut(r.name, "/")
	return d
}
