package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// tarEntry is one entry of a test build context: a file (body), a
// directory (dir) or a symlink or hard link (link, with typ).
type tarEntry struct {
	name, body, link string
	typ              byte
}

func contextTar(t *testing.T, entries ...tarEntry) string {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: e.typ, Linkname: e.link}
		switch e.typ {
		case 0:
			h.Typeflag = tar.TypeReg
			h.Size = int64(len(e.body))
		case tar.TypeDir:
			h.Mode = 0o755
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func gzipped(t *testing.T, s string) string {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	zw.Write([]byte(s))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestGateBuildRefusesHostFetches: the classic builder fetches an ADD
// source that is a URL itself, from the host's network (its loopback, a
// cloud's metadata address), and an ONBUILD instruction runs in whatever
// later builds FROM the image. So the gate reads the Dockerfile the daemon
// would build from (the dockerfile parameter, a path in the context, with
// the daemon's fallback to "dockerfile") and refuses such a build before
// the daemon sees it, however the URL is spelled: continued lines, the
// escape directive, quotes, escapes, a JSON form with escapes, a variable.
// Normal Dockerfiles are forwarded unchanged.
func TestGateBuildRefusesHostFetches(t *testing.T) {
	df := func(s string) string { return contextTar(t, tarEntry{name: "Dockerfile", body: s}) }
	refused := []struct{ name, uri, body string }{
		{"ADD URL", "/build", df("FROM scratch\nADD http://127.0.0.53:53/x /x\n")},
		{"ADD https", "/build", df("FROM scratch\nadd https://169.254.169.254/latest/meta-data/ /m\n")},
		{"ADD other scheme", "/build", df("FROM scratch\nADD ftp://127.0.0.1/x /x\n")},
		{"ADD flags", "/build", df("FROM scratch\nADD --chown=1:1 --checksum=sha256:00 http://127.0.0.1/x /x\n")},
		{"ADD several", "/build", df("FROM scratch\nADD a.txt http://127.0.0.1/x /dir/\n")},
		{"continued", "/build", df("FROM scratch\nADD \\\n  http://127.0.0.1/x \\\n  /x\n")},
		{"continued keyword", "/build", df("FROM scratch\nAD\\\nD http://127.0.0.1/x /x\n")},
		{"comment in continuation", "/build", df("FROM scratch\nADD a \\\n# note\n\n http://127.0.0.1/x /x\n")},
		{"escape directive", "/build", df("# escape=`\nFROM scratch\nADD `\n http://127.0.0.1/x /x\n")},
		{"quoted", "/build", df("FROM scratch\nADD \"http://127.0.0.1/x\" /x\n")},
		{"split by quotes", "/build", df("FROM scratch\nADD ht\"\"tp:'/'/127.0.0.1/x /x\n")},
		{"escaped", "/build", df("FROM scratch\nADD http:\\/\\/127.0.0.1/x /x\n")},
		{"JSON", "/build", df("FROM scratch\nADD [\"http://127.0.0.1/x\", \"/x\"]\n")},
		{"JSON escapes", "/build", df("FROM scratch\nADD [\"http\\u003a//127.0.0.1/x\", \"/x\"]\n")},
		{"variable", "/build", df("FROM scratch\nARG U=http://127.0.0.1/x\nADD $U /x\n")},
		{"variable braces", "/build", df("FROM scratch\nADD ${U} /x\n")},
		{"BOM", "/build", df("\ufeffADD http://127.0.0.1/x /x\n")},
		{"CRLF", "/build", df("FROM scratch\r\nADD http://127.0.0.1/x /x\r\n")},
		{"ONBUILD", "/build", df("FROM scratch\nONBUILD ADD http://127.0.0.1/x /x\n")},
		{"onbuild run", "/build", df("FROM scratch\nonbuild RUN true\n")},
		{"named Dockerfile", "/build?dockerfile=" + url.QueryEscape("docker/app.Dockerfile"), contextTar(t,
			tarEntry{name: "Dockerfile", body: "FROM scratch\n"},
			tarEntry{name: "docker/", typ: tar.TypeDir},
			tarEntry{name: "docker/app.Dockerfile", body: "FROM scratch\nADD http://127.0.0.1/x /x\n"})},
		{"named unclean", "/build?dockerfile=" + url.QueryEscape("./docker//../app"), contextTar(t,
			tarEntry{name: "app", body: "FROM scratch\nADD http://127.0.0.1/x /x\n"})},
		{"lowercase fallback", "/build", contextTar(t, tarEntry{name: "dockerfile", body: "FROM scratch\nADD http://127.0.0.1/x /x\n"})},
		{"last entry wins", "/build", contextTar(t,
			tarEntry{name: "Dockerfile", body: "FROM scratch\n"},
			tarEntry{name: "./Dockerfile", body: "FROM scratch\nADD http://127.0.0.1/x /x\n"})},
		{"gzip", "/build", gzipped(t, df("FROM scratch\nADD http://127.0.0.1/x /x\n"))},
		// What the gate could not read as the daemon does.
		{"symlinked Dockerfile", "/build", contextTar(t,
			tarEntry{name: "evil", body: "FROM scratch\nADD http://127.0.0.1/x /x\n"},
			tarEntry{name: "Dockerfile", typ: tar.TypeSymlink, link: "evil"})},
		{"hard-linked Dockerfile", "/build", contextTar(t,
			tarEntry{name: "evil", body: "FROM scratch\nADD http://127.0.0.1/x /x\n"},
			tarEntry{name: "Dockerfile", typ: tar.TypeLink, link: "evil"})},
		{"symlinked dir", "/build?dockerfile=d/Dockerfile", contextTar(t,
			tarEntry{name: "real/Dockerfile", body: "FROM scratch\nADD http://127.0.0.1/x /x\n"},
			tarEntry{name: "d", typ: tar.TypeSymlink, link: "real"})},
		{"written through a symlink", "/build", contextTar(t,
			tarEntry{name: "Dockerfile", body: "FROM scratch\n"},
			tarEntry{name: "here", typ: tar.TypeSymlink, link: "."},
			tarEntry{name: "here/Dockerfile", body: "FROM scratch\nADD http://127.0.0.1/x /x\n"})},
		{"outside", "/build?dockerfile=../Dockerfile", df("FROM scratch\n")},
		{"not a tar", "/build", "FROM scratch\nADD http://127.0.0.1/x /x\n"},
		{"xz", "/build", "\xfd7zXZ\x00rest"},
		{"too much to read", "/build", df(strings.Repeat("ADD a \\\n", 100000))},
	}
	c := newGate(t)
	for _, tc := range refused {
		before := len(c.d.requests)
		status, out := c.do("POST", tc.uri, tc.body)
		if status != http.StatusForbidden && status != http.StatusBadRequest {
			t.Errorf("%s: status %d (%s), want refused", tc.name, status, out)
		}
		for _, r := range c.d.requests[before:] {
			if strings.HasPrefix(r, "POST /build") {
				t.Errorf("%s: the build reached the daemon", tc.name)
			}
		}
	}

	// The plain Dockerfile's base image is here: the daemon gets it pinned.
	c.d.addImage("node:20", nodeID)
	plain := `# syntax=docker/dockerfile:1
# escape=\
ARG BASE=node:20
FROM ${BASE} AS build
ENV APP=/app URL=http://example.com
WORKDIR $APP
COPY package.json ./
ADD vendor.tar.gz /opt/
ADD --chown=node:node . $APP
ADD ["a b.txt", "/dst dir/"]
RUN echo "see http://example.com" \
 && curl -fsS https://example.com/x -o /x
RUN <<EOF
curl -fsS http://example.com/x
EOF
COPY <<EOF /etc/conf
url = http://example.com
EOF
# ADD http://commented-out/x /x
FROM scratch
COPY --from=build /app /app
LABEL url="http://example.com"
CMD ["node", "http://not-fetched"]
`
	forwarded := []struct{ name, uri, body string }{
		{"empty context", "/build", ""},
		{"no Dockerfile", "/build", contextTar(t, tarEntry{name: "a.txt", body: "x"})},
		{"plain", "/build", df(plain)},
		{"lowercase", "/build", contextTar(t, tarEntry{name: "dockerfile", body: "FROM scratch\nADD a /a\n"})},
		{"named", "/build?dockerfile=" + url.QueryEscape(".dockerfile.1234"), contextTar(t,
			tarEntry{name: "Dockerfile", body: "FROM scratch\nADD http://127.0.0.1/x /x\n"}, // not the one built
			tarEntry{name: ".dockerfile.1234", body: "FROM scratch\nADD a /a\n"})},
		{"symlinks elsewhere", "/build", contextTar(t,
			tarEntry{name: "Dockerfile", body: "FROM scratch\nCOPY . /\n"},
			tarEntry{name: "lib", typ: tar.TypeSymlink, link: "/usr/lib"},
			tarEntry{name: "same", typ: tar.TypeLink, link: "Dockerfile"})},
		{"gzip", "/build", gzipped(t, df("FROM scratch\nADD a /a\n"))},
	}
	for _, tc := range forwarded {
		if status, out := c.do("POST", tc.uri, tc.body); status >= 400 {
			t.Errorf("%s: status %d (%s), want it forwarded", tc.name, status, out)
			continue
		}
		if tc.name == "plain" {
			want := strings.Replace(plain, "FROM ${BASE} AS", "FROM "+nodeID+" AS", 1)
			if got := sentDockerfile(t, c, "Dockerfile"); got != want {
				t.Errorf("plain: the daemon got\n%s", got)
			}
			continue
		}
		if got := c.d.body("POST /build"); got != tc.body {
			t.Errorf("%s: the daemon got a context of %d bytes, want the %d sent", tc.name, len(got), len(tc.body))
		}
	}
}
