package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

// BoxImage is the default name of atelier's box image ($ATL_BOX_IMAGE of
// build-images).
const BoxImage = "atelier"

// imageRef is an image reference read as dockerd reads it: the domain
// (docker.io for Docker Hub, which also gets library/ for a one-part path),
// the path, and a tag and/or a digest.
type imageRef struct {
	name   string // domain/path
	tag    string
	digest string
}

func (r imageRef) String() string {
	s := r.name
	if r.tag != "" {
		s += ":" + r.tag
	}
	if r.digest != "" {
		s += "@" + r.digest
	}
	return s
}

// same reports whether r and o name the same tag, the domain in any case
// (the gate errs on the side of a match).
func (r imageRef) same(o imageRef) bool {
	return r.tag == o.tag && strings.EqualFold(r.name, o.name)
}

var (
	refDomain = regexp.MustCompile(`^(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9])(?:\.(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9]))*(?::[0-9]+)?$`)
	refPath   = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	// Compiled on first use (its counted repetition is costly to build), not
	// by every atl process (each hook) at start.
	refTag    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`) })
	refDigest = regexp.MustCompile(`^[a-z0-9]+(?:[.+_-][a-z0-9]+)*:[a-zA-Z0-9=_-]{32,}$`)
)

// parseImageRef reads s as dockerd does (distribution's
// ParseNormalizedNamed): "postgres" is docker.io/library/postgres,
// "index.docker.io/x/y" is docker.io/x/y. Without a tag or digest the tag
// is "" (the caller decides what that means: latest, or every tag for a
// pull). What dockerd would refuse is an error.
func parseImageRef(s string) (imageRef, error) {
	var r imageRef
	rest := s
	if i := strings.IndexByte(rest, '@'); i >= 0 {
		rest, r.digest = rest[:i], rest[i+1:]
		if !refDigest.MatchString(r.digest) {
			return imageRef{}, fmt.Errorf("invalid image reference %q", s)
		}
	}
	if i := strings.LastIndexByte(rest, ':'); i > strings.LastIndexByte(rest, '/') {
		rest, r.tag = rest[:i], rest[i+1:]
		if !refTag().MatchString(r.tag) {
			return imageRef{}, fmt.Errorf("invalid image reference %q", s)
		}
	}
	domain, path := "docker.io", rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		first := rest[:i]
		if strings.ContainsAny(first, ".:") || first == "localhost" || strings.ToLower(first) != first {
			domain, path = first, rest[i+1:]
		}
	}
	if domain == "index.docker.io" {
		domain = "docker.io"
	}
	if domain == "docker.io" && !strings.Contains(path, "/") {
		path = "library/" + path
	}
	if !refDomain.MatchString(domain) || !refPath.MatchString(path) || len(domain)+1+len(path) > 255 {
		return imageRef{}, fmt.Errorf("invalid image reference %q", s)
	}
	r.name = domain + "/" + path
	return r, nil
}

// reservedImages are the tags atl's own containers run from, which no
// task may create or move: DefaultImage, BoxImage and those of
// Gate.Reserved (the workspace's task image, every project's).
func (g *Gate) reservedImages() ([]imageRef, error) {
	names := []string{DefaultImage, BoxImage}
	if g.Reserved != nil {
		more, err := g.Reserved()
		if err != nil {
			return nil, fmt.Errorf("cannot read the image names atl uses: %v", err)
		}
		names = append(names, more...)
	}
	var out []imageRef
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		r, err := parseImageRef(n)
		if err != nil {
			continue // not an image atl could run either
		}
		if r.digest != "" && r.tag == "" {
			continue // pinned by content: no tag to move
		}
		if r.tag == "" {
			r.tag = "latest"
		}
		r.digest = ""
		out = append(out, r)
	}
	return out, nil
}

// checkTag returns why the task may not make s, an image reference that
// names a tag (latest if none), point to a new image, or nil: it is one of
// the tags atl runs its own containers from (reservedImages). A pull may
// re-pull any other tag: it puts only what the registry has under that
// very name there. A build is held to more (checkBuildTag).
func (g *Gate) checkTag(s string, reserved []imageRef) error {
	r, err := parseImageRef(s)
	if err != nil {
		return err
	}
	if r.tag == "" {
		r.tag = "latest"
	}
	r.digest = ""
	for _, res := range reserved {
		if r.same(res) {
			return fmt.Errorf("image name %s is reserved: atl runs its own containers from it", s)
		}
	}
	return nil
}

// checkBuildTag returns why a build may not tag its image s, or nil.
// Besides the reserved names (checkTag), a build may only name a tag in
// the task's namespace (InNamespace, or the namespace and "/"): a
// repository with no registry (a name a registry gives is the registry's
// to fill), "atl_<6 hex>", "atl_<6 hex>-app", "atl_<6 hex>/app" (or
// "library/" and one of them). Any other name a host-side `docker run` or
// compose project could run unbuilt or unpulled ("shop-api", compose's
// default for project shop; "postgres:16" before it is pulled), and the
// task would put its own image there. Compose, with the namespace as
// COMPOSE_PROJECT_NAME, names a service's image "<namespace>-<service>".
// Another task's namespace is out of reach, so that tasks cannot build
// over each other's tags either.
func (g *Gate) checkBuildTag(s string, reserved []imageRef) error {
	if err := g.checkTag(s, reserved); err != nil {
		return err
	}
	ns := g.namespace()
	repo, _, hasDigest := strings.Cut(s, "@")
	if i := strings.LastIndexByte(repo, ':'); i > strings.LastIndexByte(repo, '/') {
		repo = repo[:i]
	}
	repo = strings.TrimPrefix(repo, "library/")
	// A registry before the namespace would make it no prefix (and a
	// domain that starts with it, with its "_", is no valid one).
	if !hasDigest && (InNamespace(ns, repo) || strings.HasPrefix(repo, ns+"/")) {
		return nil
	}
	return fmt.Errorf("build tag %s: a build may only tag images in this task's namespace %s ($COMPOSE_PROJECT_NAME), "+
		"with no registry or digest: -t %s-<name>[:tag]; with compose, drop the image: of built services "+
		"(they become %s-<service>) or name them %s-<name>", s, ns, ns, ns, ns)
}

// gateImagePull allows a pull from a registry tasks may pull from
// (checkRegistry; never an import, fromSrc) whose tag is not reserved
// (checkTag); a pull by digest moves no tag. A pull of every tag of a
// repository (no tag at all) is refused.
func gateImagePull(g *Gate, w http.ResponseWriter, r *http.Request, _ []string) {
	q := r.URL.Query()
	if len(q["fromImage"]) != 1 || q.Get("fromImage") == "" || q.Has("fromSrc") || q.Has("repo") || len(q["tag"]) > 1 {
		g.deny(w, r, http.StatusForbidden, "only pulling one image from a registry is allowed")
		return
	}
	ref, err := parseImageRef(q.Get("fromImage"))
	if err != nil {
		g.deny(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if err := g.checkRegistry(ref); err != nil {
		g.deny(w, r, http.StatusForbidden, "pull "+q.Get("fromImage")+": "+err.Error())
		return
	}
	if tag := q.Get("tag"); tag != "" {
		if refDigest.MatchString(tag) {
			ref.tag, ref.digest = "", tag
		} else {
			ref.tag = tag
		}
	}
	if ref.tag == "" && ref.digest == "" {
		g.deny(w, r, http.StatusForbidden, "pulling every tag of an image is not allowed; name one")
		return
	}
	if ref.tag != "" { // with a digest too: the tag may be set as well
		reserved, err := g.reservedImages()
		if err == nil {
			err = g.checkTag(ref.String(), reserved)
		}
		if err != nil {
			g.deny(w, r, http.StatusForbidden, err.Error())
			return
		}
	}
	g.forward(w, r, nil)
}

// gateDistribution allows a look at an image in its registry (the daemon
// fetches its manifest there) only in a registry tasks may pull from
// (checkRegistry).
func gateDistribution(g *Gate, w http.ResponseWriter, r *http.Request, m []string) {
	ref, err := parseImageRef(m[1])
	if err != nil {
		g.deny(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if err := g.checkRegistry(ref); err != nil {
		g.deny(w, r, http.StatusForbidden, m[1]+": "+err.Error())
		return
	}
	g.forward(w, r, nil)
}

// buildTags checks the tags a build names (t=, any number) and labels the
// image as the task's (labels=), so that the task may build it again and a
// container made from it is never taken for atl's own (see Agent).
func (g *Gate) buildTags(r *http.Request) error {
	q := r.URL.Query()
	if tags := q["t"]; len(tags) > 0 {
		reserved, err := g.reservedImages()
		if err != nil {
			return err
		}
		for _, t := range tags {
			if err := g.checkBuildTag(t, reserved); err != nil {
				return err
			}
		}
	}
	if len(q["labels"]) > 1 {
		return errors.New("labels given twice")
	}
	labels := map[string]string{}
	if s := q.Get("labels"); s != "" {
		if err := json.Unmarshal([]byte(s), &labels); err != nil {
			return errors.New("unreadable build labels")
		}
		if labels == nil { // "null"
			labels = map[string]string{}
		}
	}
	labels[LabelTask] = g.ID
	delete(labels, LabelAgent)
	b, _ := json.Marshal(labels)
	q.Set("labels", string(b))
	r.URL.RawQuery = q.Encode()
	return nil
}
