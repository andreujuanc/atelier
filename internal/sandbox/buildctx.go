package sandbox

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// The classic builder fetches an ADD source that is a URL itself, from the
// network of the host the daemon runs on: the host's loopback services, a
// cloud's metadata address, the LAN. An ONBUILD instruction is kept in the
// image and runs in a later build FROM it. So before a build reaches the
// daemon the gate reads the Dockerfile the daemon would build from, out of
// the build context, and refuses either (checkBuildContext).
//
// It reads the context as dockerd does (remotecontext.FromArchive: one
// layer of gzip or bzip2, or none; Go's archive/tar; entries unpacked in
// order, a later one replacing an earlier one), and the Dockerfile as
// broadly as the parser could read it: where they could differ the gate
// refuses (a Dockerfile that is a link, or reached or replaced through
// one, a compression it does not read) or reads more (see checkDockerfile).

// maxDockerfile is the largest Dockerfile the gate reads.
const maxDockerfile = 1 << 20

// buildRefusal is a build the gate refuses (403); any other error of
// checkBuildContext is a context it cannot read (400).
type buildRefusal struct{ reason string }

func (e *buildRefusal) Error() string { return e.reason }

func refuseBuild(format string, args ...any) error {
	return &buildRefusal{fmt.Sprintf(format, args...)}
}

// checkBuildContext reads the build context, r's body, into spool (which it
// leaves at its end) and checks the Dockerfile the daemon would build from:
// the dockerfile parameter ("Dockerfile" if none, then "dockerfile" if that
// is missing), a path in the context.
func checkBuildContext(r *http.Request, spool io.Writer) error {
	name := r.URL.Query().Get("dockerfile")
	if name == "" {
		name = "Dockerfile"
	}
	want, ok := contextPath(name)
	if !ok {
		return refuseBuild("the Dockerfile %q is outside the build context", name)
	}
	wants := []string{want}
	if name == "Dockerfile" {
		wants = append(wants, "dockerfile") // dockerd's fallback
	}
	body := io.TeeReader(r.Body, spool)
	files, err := readContext(body, wants)
	if err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, body); err != nil { // the rest goes to the daemon too
		return err
	}
	for _, w := range wants {
		if df, ok := files[w]; ok {
			return checkDockerfile(df)
		}
	}
	return nil // no Dockerfile: the daemon fails the build
}

// contextPath is where in the context dockerd looks for name (joined to the
// context's root), slash-separated without a leading slash; false if that is
// not inside the context.
func contextPath(name string) (string, bool) {
	const root = "/c"
	p := path.Join(root, name)
	rel, ok := strings.CutPrefix(p, root+"/")
	return rel, ok
}

// readContext reads a build context, compressed as dockerd detects it, and
// returns the content of each path of wants that holds a regular file once
// the archive is unpacked. Anything that would make the daemon find another
// file at one of those paths than the last entry of that name is refused:
// an entry that is not a regular file at the path, one that is not a
// directory at a directory on the way, and any entry under a symlink
// (unpacked, it would go where the link points).
func readContext(body io.Reader, wants []string) (map[string][]byte, error) {
	br := bufio.NewReader(body)
	head, err := br.Peek(10)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	var rd io.Reader = br
	switch {
	case bytes.HasPrefix(head, []byte{0x42, 0x5A, 0x68}):
		rd = bzip2.NewReader(br)
	case bytes.HasPrefix(head, []byte{0x1F, 0x8B, 0x08}):
		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, err
		}
		rd = zr
	case bytes.HasPrefix(head, []byte{0xFD, 0x37, 0x7A, 0x58, 0x5A, 0x00}),
		bytes.HasPrefix(head, []byte{0x28, 0xb5, 0x2f, 0xfd}),
		len(head) >= 4 && binary.LittleEndian.Uint32(head)&0xFFFFFFF0 == 0x184D2A50:
		return nil, refuseBuild("a build context compressed with xz or zstd is not accepted: send it uncompressed or with gzip")
	}
	files := map[string][]byte{}
	links := map[string]bool{} // entries that are symlinks
	tr := tar.NewReader(rd)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, fmt.Errorf("unreadable build context: %v", err)
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue // dockerd skips it
		}
		name, ok := contextPath(path.Clean(h.Name))
		if !ok {
			if path.Clean("/c/"+h.Name) != "/c" {
				return nil, refuseBuild("build context entry %q is outside the context", h.Name)
			}
			name = "." // the root
		}
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			if links[dir] {
				return nil, refuseBuild("build context entry %q is under a symlink", h.Name)
			}
		}
		if h.Typeflag == tar.TypeSymlink {
			links[name] = true
		} else {
			delete(links, name)
		}
		for _, w := range wants {
			switch {
			case name == "." || strings.HasPrefix(w, name+"/"):
				if h.Typeflag != tar.TypeDir {
					return nil, refuseBuild("the build context has %q, on the way to the Dockerfile %s, as other than a directory", h.Name, w)
				}
			case name == w:
				if h.Typeflag != tar.TypeReg {
					return nil, refuseBuild("the Dockerfile %s is not a regular file in the build context (a link?): the gate reads only that", w)
				}
				if h.Size > maxDockerfile {
					return nil, refuseBuild("the Dockerfile %s is over %d bytes", w, maxDockerfile)
				}
				b, err := io.ReadAll(tr)
				if err != nil {
					return nil, fmt.Errorf("unreadable build context: %v", err)
				}
				files[w] = b
			}
		}
	}
}

var (
	dockerfileSpace = regexp.MustCompile(`[\t\v\f\r ]+`) // the parser's word separator
	utf8BOM         = []byte{0xEF, 0xBB, 0xBF}
)

// checkDockerfile refuses a Dockerfile with an ONBUILD instruction, or an
// ADD with a source that is a URL or could become one.
//
// It reads every line as the start of an instruction (also lines the
// parser takes for a continuation or a heredoc body: an older parser did
// not know heredocs), continued while it ends with either escape token
// (the escape directive may pick either), skipping comment and blank lines
// in between as the parser does. An ADD's arguments are read every way the
// parser might (its flags off or not, JSON form or words; see addArgs),
// and a source word (not the last, the destination, and not a flag) is
// refused when it has a variable, which could expand to a URL, or, with
// its quotes and escape characters taken out, has "://" in it (dockerd
// fetches http:// and https://; any scheme is refused).
func checkDockerfile(df []byte) error {
	df = bytes.TrimPrefix(df, utf8BOM)
	lines := strings.SplitAfter(string(df), "\n")
	budget := maxDockerfileWork
	for i := range lines {
		seg := strings.TrimLeftFunc(trimNewline(lines[i]), unicode.IsSpace)
		if seg == "" || seg[0] == '#' {
			continue
		}
		var line strings.Builder
		checked := true
		for j := i; ; {
			rest, more := cutContinuation(seg)
			line.WriteString(rest)
			if budget -= len(rest); budget < 0 {
				return refuseBuild("the Dockerfile is too large for the gate to check")
			}
			if kw, whole := keyword(line.String()); whole && !instructionsChecked[kw] || len(kw) > 16 {
				checked = false // not an instruction checkInstruction refuses
				break
			}
			if !more {
				break
			}
			for j++; j < len(lines) && isBlankOrComment(lines[j]); j++ {
			}
			if j >= len(lines) {
				break
			}
			seg = trimNewline(lines[j])
		}
		if checked {
			if budget -= line.Len(); budget < 0 {
				return refuseBuild("the Dockerfile is too large for the gate to check")
			}
			if err := checkInstruction(line.String()); err != nil {
				return err
			}
		}
	}
	return nil
}

// maxDockerfileWork bounds how much text checkDockerfile joins and reads
// in all (it reads lines more than once).
const maxDockerfileWork = 8 << 20

// instructionsChecked are the instructions checkInstruction looks at.
var instructionsChecked = map[string]bool{"add": true, "onbuild": true}

// keyword is the instruction the line starts with, in lower case as the
// parser compares it, and whether it is whole (a blank follows it).
func keyword(line string) (string, bool) {
	t := strings.TrimLeftFunc(line, unicode.IsSpace)
	k := strings.IndexAny(t, "\t\v\f\r ")
	if k < 0 {
		return strings.ToLower(t), false
	}
	return strings.ToLower(t[:k]), true
}

func trimNewline(s string) string { return strings.TrimRight(s, "\r\n") }

func isBlankOrComment(s string) bool {
	s = strings.TrimLeftFunc(s, unicode.IsSpace)
	return s == "" || s[0] == '#'
}

// cutContinuation reports whether line ends with an escape token (either
// one), and returns it without that token and the blanks after it.
func cutContinuation(line string) (string, bool) {
	t := strings.TrimRightFunc(line, unicode.IsSpace)
	if strings.HasSuffix(t, `\`) || strings.HasSuffix(t, "`") {
		return t[:len(t)-1], true
	}
	return line, false
}

func checkInstruction(line string) error {
	cmd := dockerfileSpace.Split(strings.TrimSpace(line), 2)
	switch strings.ToLower(cmd[0]) {
	case "onbuild":
		return refuseBuild("ONBUILD is not allowed: its instructions would run in a later build, unchecked")
	case "add":
		if len(cmd) < 2 {
			return nil
		}
		for _, words := range addArgs(cmd[1]) {
			for i, w := range words {
				if err := checkAddWord(w, i < len(words)-1); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// addArgs returns the ways the parser might split an ADD's arguments into
// words: its flags taken off (with either escape token) or not, then the
// rest as a JSON array (with or without anything after it) or split at
// blanks (the parser's, or any Unicode space).
func addArgs(args string) [][]string {
	var out [][]string
	var seen []string
	for _, rest := range []string{args, builderFlagsOff(args, '\\'), builderFlagsOff(args, '`')} {
		rest = strings.TrimSpace(rest)
		if slices.Contains(seen, rest) {
			continue
		}
		seen = append(seen, rest)
		if strings.HasPrefix(rest, "[") {
			var all []any
			if json.Unmarshal([]byte(rest), &all) == nil {
				out = append(out, jsonStrings(all))
			}
			var first []any
			if json.NewDecoder(strings.NewReader(rest)).Decode(&first) == nil {
				out = append(out, jsonStrings(first))
			}
		}
		out = append(out, dockerfileSpace.Split(rest, -1), strings.FieldsFunc(rest, unicode.IsSpace))
	}
	return out
}

func jsonStrings(list []any) []string {
	var out []string
	for _, v := range list {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

// checkAddWord checks a word of an ADD: source tells it is not the last.
func checkAddWord(w string, source bool) error {
	if source && !strings.HasPrefix(w, "--") && strings.Contains(w, "$") {
		return refuseBuild("ADD %s: a variable in an ADD source is not allowed (it could be a URL): name the source as it is, or use COPY", strconv.Quote(w))
	}
	bare := strings.Map(func(r rune) rune {
		if strings.ContainsRune("\\`'\"", r) {
			return -1
		}
		return r
	}, w)
	if strings.Contains(bare, "://") {
		return refuseBuild("ADD %s: ADD from a URL is not allowed (the daemon would fetch it from the host's network): download it in a RUN step", strconv.Quote(w))
	}
	return nil
}

// builderFlagsOff is args without the flags the parser takes off its start
// (--name=value words, until a word that does not start with --, or --),
// with esc as the escape token: the parser's extractBuilderFlags.
func builderFlagsOff(line string, esc byte) string {
	const (
		inSpaces = iota
		inWord
		inQuote
	)
	phase := inSpaces
	var word strings.Builder
	var quote byte
	for pos := 0; pos <= len(line); pos++ {
		var ch byte
		if pos != len(line) {
			ch = line[pos]
		}
		if phase == inSpaces {
			if pos == len(line) {
				break
			}
			if unicode.IsSpace(rune(ch)) {
				continue
			}
			if ch != '-' || pos+1 == len(line) || line[pos+1] != '-' {
				return line[pos:]
			}
			phase = inWord
		}
		if pos == len(line) {
			break
		}
		switch phase {
		case inWord:
			if unicode.IsSpace(rune(ch)) {
				phase = inSpaces
				if word.String() == "--" {
					return line[pos:]
				}
				word.Reset()
				continue
			}
			if ch == '\'' || ch == '"' {
				quote = ch
				phase = inQuote
				continue
			}
			if ch == esc {
				if pos+1 == len(line) {
					continue
				}
				pos++
				ch = line[pos]
			}
			word.WriteByte(ch)
		case inQuote:
			if ch == quote {
				phase = inWord
				continue
			}
			if ch == esc {
				if pos+1 == len(line) {
					phase = inWord
					continue
				}
				pos++
				ch = line[pos]
			}
			word.WriteByte(ch)
		}
	}
	return ""
}
