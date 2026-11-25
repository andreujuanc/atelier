package sandbox

import (
	"archive/tar"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// triggers are the ONBUILD triggers of the fake daemon's image id.
func (d *daemon) triggers(id string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.onBuild[id]
}

// addImage gives the fake daemon an image.
func (d *daemon) addImage(name, id string, onBuild ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.objects = append(d.objects, &object{"images", id, name, map[string]string{}})
	if d.onBuild == nil {
		d.onBuild = map[string][]string{}
	}
	d.onBuild[id] = onBuild
}

// IDs of the base images of the build tests.
var (
	alpineID = "sha256:" + strings.Repeat("a", 64)
	nodeID   = "sha256:" + strings.Repeat("b", 64)
	toolID   = "sha256:" + strings.Repeat("c", 64)
	onbID    = "sha256:" + strings.Repeat("d", 64)
	loopID   = "sha256:" + strings.Repeat("e", 64)
)

// newBuildGate is a gate whose daemon has a few base images: alpine:3,
// node:22, ghcr.io/you/builder:1, evil/onbuild:1 (with an ONBUILD ADD of a
// URL) and 127.0.0.1:5300/onb:1 (pulled from a registry on the host's
// loopback before the gate checked registries).
func newBuildGate(t *testing.T) *gateClient {
	c := newGate(t)
	c.d.addImage("alpine:3", alpineID)
	c.d.addImage("node:22", nodeID)
	c.d.addImage("ghcr.io/you/builder:1", toolID)
	c.d.addImage("evil/onbuild:1", onbID, "ADD http://127.0.0.1:5301/x /x")
	c.d.addImage("127.0.0.1:5300/onb:1", loopID, "ADD http://127.0.0.1:5301/x /x")
	return c
}

// sentDockerfile is the Dockerfile at name in the (uncompressed) build
// context the daemon got.
func sentDockerfile(t *testing.T, c *gateClient, name string) string {
	t.Helper()
	tr := tar.NewReader(strings.NewReader(c.d.body("POST /build")))
	df, found := "", false
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("the daemon got an unreadable context: %v", err)
		}
		if h.Name == name {
			b, _ := io.ReadAll(tr)
			df, found = string(b), true
		}
	}
	if !found {
		t.Fatalf("the daemon got no %s", name)
	}
	return df
}

// TestGateBuildPinsBaseImages: a build may use as a base (FROM, COPY
// --from) only an image that is here (the builder would pull a missing
// one, from any registry), from a registry tasks may pull from, without
// ONBUILD triggers (they would run in the build, unchecked); and the
// daemon gets the Dockerfile with each one named by its ID, so that a tag
// moved meanwhile changes nothing. Names are read as the builder reads
// them: ARGs before the first FROM and build args expanded (FROM only),
// stages by name (in any case) or index, scratch, continued lines, the
// escape directive.
func TestGateBuildPinsBaseImages(t *testing.T) {
	A, N, T := alpineID, nodeID, toolID
	ok := []struct {
		name, uri, df, want string
	}{
		{"FROM", "/build", "FROM alpine:3\nRUN true\n", "FROM " + A + "\nRUN true\n"},
		{"stages", "/build", `FROM node:22 AS build
RUN make
from alpine:3 as Final
COPY --from=build /a /a
COPY --from=BUILD /b /b
COPY --from=0 /c /c
COPY --chown=1:1 --from=ghcr.io/you/builder:1 /t /t
COPY --from="ghcr.io/you/builder:1" /t /t
COPY --from= /d /d
FROM final
`, `FROM ` + N + ` AS build
RUN make
from ` + A + ` as Final
COPY --from=build /a /a
COPY --from=BUILD /b /b
COPY --from=0 /c /c
COPY --chown=1:1 --from=` + T + ` /t /t
COPY --from=` + T + ` /t /t
COPY --from= /d /d
FROM final
`},
		{"meta ARGs", "/build", `ARG REG=docker.io
ARG NODE=22
ARG FLAVOR
ARG A=3 B=${A}
ARG X=gone
ARG X
ARG E=
ARG Q="alpine:3"
FROM ${REG}/library/node:${NODE}${FLAVOR:+-$FLAVOR}
FROM $UNSET${BASE:-alpine}:3${B}${X}${E-edge}${E:+edge}
FROM ${Q}
`, `ARG REG=docker.io
ARG NODE=22
ARG FLAVOR
ARG A=3 B=${A}
ARG X=gone
ARG X
ARG E=
ARG Q="alpine:3"
FROM ` + N + `
FROM ` + A + `
FROM ` + A + `
`},
		{"build args", "/build?buildargs=" + url.QueryEscape(`{"V":"3","FLAVOR":null,"HTTP_PROXY":"node:22","UNDECLARED":"x"}`),
			"ARG V=99\nARG FLAVOR=x\nFROM alpine:$V${FLAVOR:+}\nFROM ${HTTP_PROXY}\nFROM alpine:3$UNDECLARED\n",
			"ARG V=99\nARG FLAVOR=x\nFROM " + A + "\nFROM " + N + "\nFROM " + A + "\n"},
		{"stage and scratch by variable", "/build", "ARG S=BUILD\nARG Z=scratch\nFROM alpine:3 AS build\nFROM ${S}\nFROM $Z\nFROM scratch\n",
			"ARG S=BUILD\nARG Z=scratch\nFROM " + A + " AS build\nFROM build\nFROM scratch\nFROM scratch\n"},
		{"stage ARG not for FROM", "/build", "FROM alpine:3\nARG N=node\nFROM alpine:3${N}\n", "FROM " + A + "\nARG N=node\nFROM " + A + "\n"},
		{"continued", "/build", "FROM \\\n# a comment\n\n  alpine:3 \\\n  AS base\nCOPY \\\n  --from=node:22 /a /a\n",
			"FROM \\\n# a comment\n\n  " + A + " \\\n  AS base\nCOPY \\\n  --from=" + N + " /a /a\n"},
		{"escape directive", "/build", "# escape=`\nFROM `\n  alpine:3 `\n  AS base\nRUN dir c:\\ \nFROM base\n",
			"# escape=`\nFROM `\n  " + A + " `\n  AS base\nRUN dir c:\\ \nFROM base\n"},
		{"BOM CRLF", "/build", "\ufeffFROM alpine:3\r\nRUN x\r\n", "\ufeffFROM " + A + "\r\nRUN x\r\n"},
		{"heredocs", "/build", "FROM alpine:3 AS a\nRUN <<EOF\necho from here\nEOF\nCOPY <<-'X' /x\n\tfrom here and there\n\tX\nFROM a\n",
			"FROM " + A + " AS a\nRUN <<EOF\necho from here\nEOF\nCOPY <<-'X' /x\n\tfrom here and there\n\tX\nFROM a\n"},
		{"platform", "/build", "FROM --platform=$BUILDPLATFORM alpine:3\nFROM --platform=linux/amd64 alpine:3\nFROM --platform=LINUX/x86_64 node:22\n",
			"FROM --platform=$BUILDPLATFORM " + A + "\nFROM --platform=linux/amd64 " + A + "\nFROM --platform=LINUX/x86_64 " + N + "\n"},
		{"target", "/build?target=A", "FROM alpine:3 AS a\nFROM missing:1 AS b\nFROM 127.0.0.1:5300/onb:1\n",
			"FROM " + A + " AS a\nFROM missing:1 AS b\nFROM 127.0.0.1:5300/onb:1\n"},
	}
	c := newBuildGate(t)
	for _, tc := range ok {
		body := contextTar(t, tarEntry{name: "Dockerfile", body: tc.df}, tarEntry{name: "app.txt", body: "app"})
		if status, out := c.do("POST", tc.uri, body); status >= 400 {
			t.Errorf("%s: %d %s", tc.name, status, out)
			continue
		}
		if got := sentDockerfile(t, c, "Dockerfile"); got != tc.want {
			t.Errorf("%s: the daemon got\n%s\nwant\n%s", tc.name, got, tc.want)
		}
		if got := sentDockerfile(t, c, "app.txt"); got != "app" {
			t.Errorf("%s: the rest of the context: %q", tc.name, got)
		}
	}

	// Nothing to pin: the very bytes sent.
	body := contextTar(t, tarEntry{name: "Dockerfile", body: "FROM scratch AS s\nFROM scratch\nCOPY --from=s a /a\n"})
	if status, out := c.do("POST", "/build", body); status >= 400 || c.d.body("POST /build") != body {
		t.Errorf("scratch: %d %s; the daemon got another context", status, out)
	}

	// A gzipped context, a named Dockerfile, the last of two entries: the
	// daemon gets the context uncompressed, with each entry at the
	// Dockerfile's path pinned and the rest as it was.
	named := "/build?dockerfile=" + url.QueryEscape("./docker/app.Dockerfile")
	body = gzipped(t, contextTar(t,
		tarEntry{name: "Dockerfile", body: "FROM missing:1\n"},
		tarEntry{name: "docker/", typ: tar.TypeDir},
		tarEntry{name: "docker/app.Dockerfile", body: "FROM node:22\n"},
		tarEntry{name: "docker/./app.Dockerfile", body: "FROM alpine:3\n"},
		tarEntry{name: "link", typ: tar.TypeSymlink, link: "/etc"}))
	if status, out := c.do("POST", named, body); status >= 400 {
		t.Fatalf("gzip: %d %s", status, out)
	}
	if got := sentDockerfile(t, c, "docker/app.Dockerfile"); got != "FROM "+A+"\n" {
		t.Errorf("gzip: first entry (also replaced, by what the last one becomes) %q", got)
	}
	if got := sentDockerfile(t, c, "docker/./app.Dockerfile"); got != "FROM "+A+"\n" {
		t.Errorf("gzip: last entry %q", got)
	}
	if got := sentDockerfile(t, c, "Dockerfile"); got != "FROM missing:1\n" {
		t.Errorf("gzip: another Dockerfile changed: %q", got)
	}
	tr := tar.NewReader(strings.NewReader(c.d.body("POST /build")))
	var names []string
	for h, err := tr.Next(); err == nil; h, err = tr.Next() {
		names = append(names, h.Name+">"+h.Linkname)
	}
	if got := strings.Join(names, " "); got != "Dockerfile> docker/> docker/app.Dockerfile> docker/./app.Dockerfile> link>/etc" {
		t.Errorf("gzip: entries %s", got)
	}

	refused := []struct{ name, uri, df, why string }{
		{"loopback registry", "/build", "FROM 127.0.0.1:5300/onb:1\n", "registry 127.0.0.1:5300"},
		{"localhost registry", "/build", "FROM alpine:3\nCOPY --from=localhost:5300/x:1 /a /a\n", "registry localhost:5300"},
		{"registry by ARG", "/build", "ARG HOST=127.0.0.1:5300\nFROM ${HOST}/onb:1\n", "registry 127.0.0.1:5300"},
		{"registry by build arg", "/build?buildargs=" + url.QueryEscape(`{"BASE":"127.0.0.1:5300/onb:1"}`),
			"ARG BASE=alpine:3\nFROM $BASE\n", "registry 127.0.0.1:5300"},
		{"registry by proxy arg", "/build?buildargs=" + url.QueryEscape(`{"https_proxy":"127.0.0.1:5300/onb:1"}`),
			"FROM ${https_proxy}\n", "registry 127.0.0.1:5300"},
		{"missing", "/build", "FROM alpine:3\nFROM missing:1\n", "pull it first: docker pull missing:1"},
		{"missing COPY", "/build", "FROM alpine:3\nCOPY --from=missing:2 /a /a\n", "docker pull missing:2"},
		{"later stage name", "/build", "FROM alpine:3\nCOPY --from=later /a /a\nFROM alpine:3 AS later\n", "docker pull later"},
		{"own stage name", "/build", "FROM alpine:3 AS self\nCOPY --from=self /a /a\n", "docker pull self"},
		{"COPY --from not expanded", "/build", "ARG X=alpine:3\nFROM alpine:3\nCOPY --from=$X /a /a\n", "invalid image reference"},
		{"ONBUILD", "/build", "FROM evil/onbuild:1\n", "ONBUILD"},
		{"ONBUILD COPY", "/build", "FROM alpine:3\nCOPY --from=evil/onbuild:1 /a /a\n", "ONBUILD"},
		{"empty", "/build", "FROM $NOPE\n", "empty"},
		{"unknown form", "/build", "FROM ${X:?no}\n", "cannot expand"},
		{"pattern", "/build", "ARG X=alpine:3\nFROM ${X%:3}:3\n", "cannot expand"},
		{"positional", "/build", "FROM alpine$1\n", "cannot expand"},
		{"special", "/build", "FROM alpine$$\n", "cannot expand"},
		{"unreadable ARG", "/build", "ARG B=${X/a/b}\nFROM $B\n", "cannot read the ARG"},
		{"ARG with flags", "/build", "ARG --x=1 B=alpine:3\nFROM $B\n", "cannot read the ARG"},
		{"unclosed quote", "/build", "ARG B=\"alpine:3\nFROM $B\n", "cannot read the ARG"},
		{"platform", "/build", "FROM --platform=linux/arm64 alpine:3\n", "not the daemon's platform"},
		{"platform by ARG", "/build", "ARG P=linux/arm64\nFROM --platform=$P alpine:3\n", "not the daemon's platform"},
		{"platform variant", "/build", "FROM --platform=linux/amd64/v3 alpine:3\n", "not the daemon's platform"},
		{"other FROM flag", "/build", "FROM --pull=always alpine:3\n", "not one the classic builder reads"},
		{"COPY --FROM", "/build", "FROM alpine:3\nCOPY --FROM=missing:1 /a /a\n", "not one the classic builder reads"},
		{"two --from", "/build", "FROM alpine:3\nCOPY --from=alpine:3 --from=missing:1 /a /a\n", "not one the classic builder reads"},
		{"bad FROM", "/build", "FROM alpine:3 AS\n", "takes an image"},
		{"bad stage name", "/build", "FROM alpine:3 AS 1st\n", "invalid stage name"},
		{"64-bit index", "/build", "FROM alpine:3\nCOPY --from=9999999999 /a /a\n", "no stage has that index"},
		{"name across lines", "/build", "FROM alp\\\nine:3\n", "more than one line"},
		{"flags read two ways", "/build", "FROM alpine:3\nCOPY --chown=a\\ b --from=missing:1 /a /a\n", "its flags"},
		{"continuation", "/build", "FROM alpine:3\nRUN echo \\\nfrom 127.0.0.1:5300/onb:1\n", "could continue the line before it"},
		{"continuation COPY", "/build", "FROM alpine:3\nRUN echo \\\n  COPY --from=127.0.0.1:5300/onb:1 /a /a\n", "COPY --from"},
		{"backslash under escape=`", "/build", "# escape=`\nFROM alpine:3\nRUN echo \\\nFROM 127.0.0.1:5300/onb:1\n", "registry"},
		{"in a heredoc", "/build", "FROM alpine:3\nRUN <<EOF\nFROM 127.0.0.1:5300/onb:1\nEOF\n", "in a heredoc"},
		{"past a heredoc", "/build", "FROM alpine:3\nRUN cat <<EOF>out\nx\nEOF\nFROM node:22\n", "cannot delimit"},
		{"unterminated heredoc", "/build", "FROM alpine:3\nRUN <<EOF\nFROM node:22\n", "heredoc"},
		{"uncertain escape", "/build", "# check=skip=all\n# escape=`\nFROM alpine:3\n", "escape character"},
		{"late escape", "/build", "\n# escape=`\nFROM alpine:3\n", "escape character"},
		{"two escapes", "/build", "# escape=`\n# escape=\\\nFROM alpine:3\n", "two escape directives"},
		{"build args twice", "/build?buildargs=%7B%7D&buildargs=%7B%7D", "FROM alpine:3\n", "build args"},
		{"long line", "/build", "FROM alpine:3\nRUN " + strings.Repeat("x", 70000) + "\n", "too long"},
	}
	for _, tc := range refused {
		before := len(c.d.requests)
		status, out := c.do("POST", tc.uri, contextTar(t, tarEntry{name: "Dockerfile", body: tc.df}))
		if status != http.StatusForbidden && status != http.StatusBadRequest || !strings.Contains(out, tc.why) {
			t.Errorf("%s: %d %s, want it refused (%s)", tc.name, status, out, tc.why)
		}
		for _, r := range c.d.requests[before:] {
			if strings.HasPrefix(r, "POST /build") {
				t.Errorf("%s: the build reached the daemon", tc.name)
			}
		}
	}

	// A registry of [sandbox] registries may be a base's.
	c = newGateWith(t, func(g *Gate) { g.Registries = func() ([]string, error) { return []string{"127.0.0.1:5300"}, nil } })
	c.d.addImage("127.0.0.1:5300/plain:1", loopID)
	if status, out := c.do("POST", "/build", contextTar(t, tarEntry{name: "Dockerfile", body: "FROM 127.0.0.1:5300/plain:1\n"})); status >= 400 {
		t.Errorf("allowed registry: %d %s", status, out)
	}
}

// TestExpandWord: the builder's expansion of a FROM, as far as the gate
// reads it.
func TestExpandWord(t *testing.T) {
	vars := map[string]string{"A": "a", "E": "", "Ü": "u"}
	lookup := func(n string) (string, bool, error) { v, ok := vars[n]; return v, ok, nil }
	for in, want := range map[string]string{
		"x$A":            "xa",
		"x${A}y":         "xay",
		"$A.$A/$Ü":       "a.a/u",
		"${N:-d}${E:-e}": "de",
		"${N-d}${E-e}":   "d",
		"${A:+p}${E:+q}": "p",
		"${A+p}${E+q}":   "pq",
		"${N:-${A}x}":    "ax",
		"${N:-a\\}b}":    "a}b",
		`'$A'"$A"\$A`:    "$Aa$A",
		`"a\"b\x"`:       `a"b\x`,
		"a$":             "a$",
		"a$/b":           "a$/b",
		"a\\":            "a",
	} {
		if got, err := expandWord(in, '\\', lookup); err != nil || got != want {
			t.Errorf("expandWord(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if got, err := expandWord("`$A$A", '`', lookup); err != nil || got != "$Aa" {
		t.Errorf("backtick escape: %q %v", got, err)
	}
	for _, in := range []string{"$1", "${1}", "$@", "${A:?x}", "${A?x}", "${A#a}", "${A%a}", "${A/a/b}", "${A:0:1}", "${}", "${A",
		"${A:-x", "'a", `"a`, "${:-a}"} {
		if got, err := expandWord(in, '\\', lookup); err == nil {
			t.Errorf("expandWord(%q) = %q, want an error", in, got)
		}
	}
	errLookup := func(string) (string, bool, error) { return "", false, errors.New("unknown") }
	if _, err := expandWord("x$A", '\\', errLookup); err == nil {
		t.Errorf("lookup error not passed on")
	}
	if got, err := expandWord("plain:1", '\\', errLookup); err != nil || got != "plain:1" {
		t.Errorf("no variable: %q %v", got, err)
	}
}
