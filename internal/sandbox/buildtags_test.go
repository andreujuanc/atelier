package sandbox

import (
	"net/http"
	"strings"
	"testing"
)

// TestGateBuildTagNamespace: a build may only tag its image in the task's
// namespace (atl_abc123 for testID, its COMPOSE_PROJECT_NAME): the
// namespace itself, or it and "-", "_" or "/" and more, without a registry
// or digest. Any other name is one a host-side docker run or compose
// project could run (compose's default "<project>-<service>", an unpulled
// "postgres:16"), or another task's. Compose with the namespace as its
// project names a service's image "<namespace>-<service>".
func TestGateBuildTagNamespace(t *testing.T) {
	c := newGate(t)
	if ns := ComposeProject(testID); ns != "atl_abc123" {
		t.Fatalf("namespace %s", ns)
	}
	for _, tag := range []string{
		"atl_abc123",
		"atl_abc123:dev",
		"atl_abc123-api",       // compose: project atl_abc123, service api
		"atl_abc123-api:1.2.3", // with a tag
		"atl_abc123_worker",
		"atl_abc123/tool",
		"atl_abc123/sub/tool:x",
		"library/atl_abc123-api",
	} {
		if status, out := c.do("POST", "/build?t="+tag, ""); status >= 400 {
			t.Errorf("t=%s: %d %s, want it forwarded", tag, status, out)
		}
	}
	if status, out := c.do("POST", "/build?t=atl_abc123-api&t=atl_abc123-api:2&t=atl_abc123/x", ""); status >= 400 {
		t.Errorf("several tags in the namespace: %d %s", status, out)
	}

	for _, uri := range []string{
		"/build?t=shop-api",         // compose's default, for project shop
		"/build?t=postgres:16",      // not pulled yet
		"/build?t=api",              // bare
		"/build?t=atl_def456-api",   // another task's namespace
		"/build?t=atl_abc1234-api",  // not the namespace and a separator
		"/build?t=atl_abc123x",      // likewise
		"/build?t=atl_abc12-api",    // a prefix of it
		"/build?t=xatl_abc123-api",  //
		"/build?t=you/atl_abc123-a", // under another repository
		"/build?t=ATL_ABC123-api",   // dockerd refuses upper case; another name anyway
		"/build?t=Atl_abc123-api",
		"/build?t=localhost:5000/atl_abc123-api", // a registry
		"/build?t=docker.io/atl_abc123-api",
		"/build?t=docker.io/library/atl_abc123-api",
		"/build?t=ghcr.io/atl_abc123/api",
		"/build?t=atl_abc123-x.com/api", // a domain (invalid: "_")
		"/build?t=atl_abc123-api@sha256:" + strings.Repeat("a", 64),
		"/build?t=atl_abc123-api&t=shop-api", // one of several
		"/build?t=shop-api&t=atl_abc123-api",
	} {
		before := len(c.d.requests)
		status, out := c.do("POST", uri, "")
		if status != http.StatusForbidden && status != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want refused", uri, status, out)
		}
		for _, r := range c.d.requests[before:] {
			if strings.HasPrefix(r, "POST /build") {
				t.Errorf("%s: the build reached the daemon", uri)
			}
		}
	}
	if _, out := c.do("POST", "/build?t=shop-api", ""); !strings.Contains(out, "-t atl_abc123-") ||
		!strings.Contains(out, "COMPOSE_PROJECT_NAME") || !strings.Contains(out, "image:") {
		t.Errorf("refusal does not say what to do: %s", out)
	}

	// The reserved names are refused in the namespace too.
	c = newGateWith(t, func(g *Gate) { g.Reserved = func() ([]string, error) { return []string{"atl_abc123-dev"}, nil } })
	if status, out := c.do("POST", "/build?t=atl_abc123-dev:latest", ""); status != http.StatusForbidden || !strings.Contains(out, "reserved") {
		t.Errorf("reserved name in the namespace: %d %s", status, out)
	}
}
