package sandbox

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The classic builder takes the image of each FROM, and of each COPY
// --from that names no earlier stage, from the daemon's images, and pulls
// it when it is not there: from any registry, past the registry rules of
// a pull. And it runs the ONBUILD triggers of a FROM image in the build,
// where an ADD of a URL is fetched from the host's network. So before a
// build reaches the daemon the gate reads every FROM and COPY --from of the
// Dockerfile as the builder will (pinBaseImages): each image they name must
// come from a registry tasks may pull from (checkRegistry), be here already
// (it is never pulled for the build) and have no ONBUILD triggers; and the
// gate rewrites the Dockerfile to name it by its ID, so that a tag that
// moves while the build runs (the task pulls it again) changes nothing.
//
// The Dockerfile is read as the parser of the classic builder reads it
// (moby's, which is BuildKit's frontend/dockerfile/parser): the escape
// directive, lines continued with the escape token (comment and blank
// lines in between skipped), heredocs of RUN, COPY and ADD. Where the gate
// could read it otherwise than the parser, it refuses: an escape directive
// the gate is not sure of, a heredoc it cannot delimit, flags it could
// read two ways; and every line is also read as the start of an
// instruction, and any FROM or COPY --from read that way must be one the
// gate pins (see unreadBaseRefs). The rewrite makes the builder use what
// the gate checked, whatever it would have expanded; what it does not
// settle is whether a name is an earlier stage's, which depends on which
// FROMs there are: hence the care.
//
// Variables in a FROM are expanded as the builder does (expandWord) with
// the build args and the ARGs before the first FROM; COPY --from is not
// expanded by the classic builder (checked: "COPY --from=$X" is the image
// "$X"). A FROM whose name has anything to expand is rewritten too, to the
// stage or "scratch" it names, so that only the gate's reading counts.

// maxBaseImages is how many different images a Dockerfile may name.
const maxBaseImages = 100

// maxDockerfileLine is the longest line the parser reads (bufio.Scanner's
// limit, the newline included); it fails on a longer one.
const maxDockerfileLine = 64*1024 - 1

var (
	directiveLine = regexp.MustCompile(`^#\s*([a-zA-Z][a-zA-Z0-9]*)\s*=\s*(.+?)\s*$`)
	stageName     = regexp.MustCompile(`^[a-z][a-z0-9_.-]*$`)
	imageID       = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// heredocWord is a word of a RUN, COPY or ADD that the parser takes for
	// a heredoc and whose name the gate reads as the parser does (see
	// heredocs).
	heredocWord = regexp.MustCompile(`^([0-9]*)<<(-?)(?:([A-Za-z0-9_]+)|'([A-Za-z0-9_]+)'|"([A-Za-z0-9_]+)")$`)
)

// proxyArgs are the build args the builder knows without an ARG.
var proxyArgs = []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "FTP_PROXY", "ftp_proxy",
	"NO_PROXY", "no_proxy", "ALL_PROXY", "all_proxy"}

// dockerfile is a Dockerfile split into lines as the parser reads them.
type dockerfile struct {
	src   []byte
	lines []string // without the line end ("\n" and the "\r"s before it), the first without a BOM
	offs  []int    // where each line starts in src
}

func splitDockerfile(src []byte) (*dockerfile, error) {
	f := &dockerfile{src: src}
	start := 0
	if bytes.HasPrefix(src, utf8BOM) {
		start = len(utf8BOM)
	}
	for start < len(src) {
		end, next := len(src), len(src)
		if i := bytes.IndexByte(src[start:], '\n'); i >= 0 {
			end, next = start+i, start+i+1
		}
		if end-start > maxDockerfileLine {
			return nil, refuseBuild("line %d of the Dockerfile is too long (the parser reads at most %d bytes)", len(f.lines)+1, maxDockerfileLine)
		}
		f.lines = append(f.lines, strings.TrimRight(string(src[start:end]), "\r\n"))
		f.offs = append(f.offs, start)
		start = next
	}
	return f, nil
}

// instruction is a line of the Dockerfile as the parser joins it.
type instruction struct {
	first, last int    // its lines (with comment and blank lines in between)
	text        string // joined
	at          []int  // where each byte of text is in the Dockerfile
	buf         []byte // text, while join reads it
}

func (in *instruction) add(s string, off int) {
	in.buf = append(in.buf, s...)
	for i := range len(s) {
		in.at = append(in.at, off+i)
	}
}

// head is the start of the text read so far, enough for keyword.
func (in *instruction) head() string {
	t := bytes.TrimLeftFunc(in.buf, unicode.IsSpace)
	return string(t[:min(len(t), 40)])
}

// span is where text[i:j] (not empty) is in the Dockerfile, and whether it
// is there in one piece (not across a continued line).
func (in *instruction) span(i, j int) (start, end int, whole bool) {
	if i >= j {
		return -1, -1, false
	}
	return in.at[i], in.at[j-1] + 1, in.at[j-1]-in.at[i] == j-1-i
}

// join reads the instruction that starts on line i, with esc as the escape
// token: the parser's own loop. false: line i starts none (blank, comment),
// or, with only, none of those instructions (it stops reading once it
// knows).
func (f *dockerfile) join(i int, esc byte, only []string, budget *int) (*instruction, bool, error) {
	line := f.lines[i]
	t := strings.TrimLeftFunc(line, unicode.IsSpace)
	if strings.HasPrefix(t, "#") {
		return nil, false, nil
	}
	part, more := continued(t, esc)
	if part == "" && !more {
		return nil, false, nil
	}
	in := &instruction{first: i, last: i}
	in.add(part, f.offs[i]+len(line)-len(t))
	for {
		if *budget -= len(part) + 1; *budget < 0 {
			return nil, false, refuseBuild("the Dockerfile is too large for the gate to check")
		}
		if only != nil {
			if kw, whole := keyword(in.head()); whole && !slices.Contains(only, kw) || len(kw) > 16 {
				return nil, false, nil
			}
		}
		if !more || in.last+1 == len(f.lines) {
			in.text, in.buf = string(in.buf), nil
			return in, true, nil
		}
		in.last++
		l := f.lines[in.last]
		if t := strings.TrimLeftFunc(l, unicode.IsSpace); t == "" || t[0] == '#' {
			part = ""
			continue
		}
		part, more = continued(l, esc)
		in.add(part, f.offs[in.last])
	}
}

// continued is line without its escape token and what follows it (blanks
// and tabs), and whether it had one: the parser's trimContinuationCharacter.
func continued(line string, esc byte) (string, bool) {
	t := strings.TrimRight(line, " \t")
	if t != "" && t[len(t)-1] == esc {
		return t[:len(t)-1], true
	}
	return line, false
}

// command splits an instruction as the parser does: its keyword, in lower
// case, and where its arguments begin and end in text (begin == end: none).
func (in *instruction) command() (kw string, begin, end int) {
	t := in.text
	lead := len(t) - len(strings.TrimLeftFunc(t, unicode.IsSpace))
	end = len(strings.TrimRightFunc(t, unicode.IsSpace))
	if end < lead {
		end = lead
	}
	k := strings.IndexAny(t[lead:end], "\t\v\f\r ")
	if k < 0 {
		return strings.ToLower(t[lead:end]), end, end
	}
	begin = lead + k
	for begin < end && strings.IndexByte("\t\v\f\r ", t[begin]) >= 0 {
		begin++
	}
	return strings.ToLower(t[lead : lead+k]), begin, end
}

// flagWord is a flag of an instruction (--name=value): where it is in the
// text read, and as the parser reads it (quotes and escapes taken out).
type flagWord struct {
	start, end int
	value      string
}

// builderFlags reads the flags at the start of args as the parser's
// extractBuilderFlags does, with esc as the escape character, and returns
// them and where the arguments after them begin.
func builderFlags(args string, esc byte) ([]flagWord, int) {
	const (
		inSpaces = iota
		inWord
		inQuote
	)
	var words []flagWord
	phase, start, blankOK := inSpaces, 0, false
	var word strings.Builder
	var quote byte
	for pos := 0; pos <= len(args); pos++ {
		var ch byte
		if pos != len(args) {
			ch = args[pos]
		}
		if phase == inSpaces {
			if pos == len(args) {
				break
			}
			if unicode.IsSpace(rune(ch)) {
				continue
			}
			if ch != '-' || pos+1 == len(args) || args[pos+1] != '-' {
				return words, pos
			}
			phase, start = inWord, pos
		}
		if pos == len(args) {
			if word.String() != "--" && (blankOK || word.Len() > 0) {
				words = append(words, flagWord{start, pos, word.String()})
			}
			break
		}
		switch phase {
		case inWord:
			if unicode.IsSpace(rune(ch)) {
				phase = inSpaces
				if word.String() == "--" {
					return words, pos
				}
				if blankOK || word.Len() > 0 {
					words = append(words, flagWord{start, pos, word.String()})
				}
				word.Reset()
				blankOK = false
				continue
			}
			if ch == '\'' || ch == '"' {
				quote, blankOK, phase = ch, true, inQuote
				continue
			}
			if ch == esc {
				if pos+1 == len(args) {
					continue
				}
				pos++
				ch = args[pos]
			}
			word.WriteRune(rune(ch))
		case inQuote:
			if ch == quote {
				phase = inWord
				continue
			}
			if ch == esc {
				if pos+1 == len(args) {
					phase = inWord
					continue
				}
				pos++
				ch = args[pos]
			}
			word.WriteRune(rune(ch))
		}
	}
	return words, len(args)
}

// flagsOf reads the flags of args (see builderFlags) with either escape
// character (the parser's has been '\'; the existing checks read both), and
// refuses where the two readings differ.
func flagsOf(args string, line int) ([]flagWord, int, error) {
	a, ia := builderFlags(args, '\\')
	b, ib := builderFlags(args, '`')
	if ia != ib || !slices.Equal(a, b) {
		return nil, 0, refuseBuild("line %d of the Dockerfile: the gate cannot tell how the builder reads its flags (an escape character in them)", line+1)
	}
	return a, ia, nil
}

// fields splits s at the parser's blanks (parseStringsWhitespaceDelimited):
// the start and end of each field.
func fields(s string) [][2]int {
	if s == "" {
		return nil
	}
	var out [][2]int
	at := 0
	for _, sep := range dockerfileSpace.FindAllStringIndex(s, -1) {
		out = append(out, [2]int{at, sep[0]})
		at = sep[1]
	}
	return append(out, [2]int{at, len(s)})
}

// baseRef is an image name a FROM or a COPY --from of the Dockerfile
// gives, as the gate pins it.
type baseRef struct {
	line       int    // the first line of its instruction
	stage      int    // the stage it is in (a FROM: the one it begins); -1 before the first FROM
	copy       bool   // a COPY --from (else a FROM)
	raw        string // a FROM: the name as written; a COPY: the flag's value as the parser reads it
	start, end int    // what the rewrite replaces in the Dockerfile: the name, or the whole --from flag
	whole      bool   // start:end is in one piece
	platform   *string
}

func (b *baseRef) what() string {
	if b.copy {
		return fmt.Sprintf("COPY --from=%s (line %d)", b.raw, b.line+1)
	}
	return fmt.Sprintf("FROM %s (line %d)", b.raw, b.line+1)
}

// dockerfileReading is the Dockerfile as the gate reads it.
type dockerfileReading struct {
	f      *dockerfile
	esc    byte
	refs   []*baseRef
	stages []string // the name of each stage ("" none)
	meta   *metaArgs
	// kind says what each line is to the parser (lineInstr, ...);
	// unread: the line after which the gate does not know (a heredoc it
	// cannot delimit), else -1.
	kind   []byte
	unread int
}

const (
	lineOther   = iota // blank, comment, a directive
	lineInstr          // first line of an instruction
	lineCont           // continues an instruction
	lineHeredoc        // in a heredoc
	lineUnread         // past what the gate can read
)

// readDockerfile reads df as the parser does (see the top of this file).
func readDockerfile(df []byte, opts map[string]*string) (*dockerfileReading, error) {
	f, err := splitDockerfile(df)
	if err != nil {
		return nil, err
	}
	esc, err := escapeToken(f.lines)
	if err != nil {
		return nil, err
	}
	rd := &dockerfileReading{f: f, esc: esc, kind: make([]byte, len(f.lines)), unread: -1,
		meta: &metaArgs{opts: opts, vals: map[string]*string{}, poisoned: map[string]bool{}}}
	budget := maxDockerfileWork
	stage := -1
	for i := 0; i < len(f.lines); {
		in, ok, err := f.join(i, esc, nil, &budget)
		if err != nil {
			return nil, err
		}
		if !ok {
			i++
			continue
		}
		rd.kind[in.first] = lineInstr
		for j := in.first + 1; j <= in.last; j++ {
			rd.kind[j] = lineCont
		}
		i = in.last + 1
		kw, begin, end := in.command()
		args := in.text[begin:end]
		switch kw {
		case "arg":
			if stage < 0 {
				rd.meta.declare(args, esc)
			}
		case "from":
			stage++
			ref, name, err := fromRef(in, begin, end, stage)
			if err != nil {
				return nil, err
			}
			rd.refs = append(rd.refs, ref)
			rd.stages = append(rd.stages, name)
		case "copy":
			ref, err := copyRef(in, begin, end, stage)
			if err != nil {
				return nil, err
			}
			if ref != nil {
				rd.refs = append(rd.refs, ref)
			}
		}
		if (kw == "run" || kw == "copy" || kw == "add") && strings.Contains(in.text, "<<") {
			docs, ok := heredocs(in.text)
			for _, d := range docs {
				if !ok {
					break
				}
				ok = false
				for ; i < len(f.lines); i++ {
					rd.kind[i] = lineHeredoc
					l := f.lines[i]
					if d.chomp {
						l = strings.TrimLeft(l, "\t")
					}
					if l == d.name {
						i++
						ok = true
						break
					}
				}
			}
			if !ok {
				rd.unread = in.first
				for j := i; j < len(f.lines); j++ {
					rd.kind[j] = lineUnread
				}
				break
			}
		}
	}
	if err := rd.unreadBaseRefs(&budget); err != nil {
		return nil, err
	}
	return rd, nil
}

// escapeToken is the escape token of the Dockerfile with lines: '`' with
// an escape directive that says so, else '\'. The directives are the
// comment lines at the very top the parser knows (escape, syntax, check);
// they end at the first other line. An escape directive the parser might
// not take (after a check directive, which older parsers do not know; on a
// later line) is refused: the gate would not know how lines continue.
func escapeToken(lines []string) (byte, error) {
	esc, at := byte('\\'), -1
	seen := map[string]bool{}
	directives := true
	for i, l := range lines {
		s := strings.TrimLeftFunc(l, unicode.IsSpace)
		if directives {
			if m := directiveLine.FindStringSubmatch(s); m != nil {
				if k := strings.ToLower(m[1]); k == "escape" || k == "syntax" || k == "check" {
					if seen[k] {
						return 0, refuseBuild("the Dockerfile has two %s directives", k)
					}
					seen[k] = true
					if k == "escape" {
						if seen["check"] || m[2] != `\` && m[2] != "`" {
							return 0, refuseBuild("the gate cannot tell the escape character of the Dockerfile: " +
								"put its directive (# escape=` or # escape=\\) on the first line")
						}
						esc, at = m[2][0], i
					}
					continue
				}
			}
			directives = false
		}
		if s != "" && s[0] != '#' {
			break
		}
		if i != at && strings.Contains(strings.ToLower(s), "escape") && strings.Contains(s, "=") {
			return 0, refuseBuild("the gate cannot tell the escape character of the Dockerfile: " +
				"put its directive (# escape=` or # escape=\\) on the first line, with nothing like it after")
		}
	}
	return esc, nil
}

// fromRef reads a FROM: its image name (refused when the parser would fail
// on the FROM: the gate reads no further) and its stage name.
func fromRef(in *instruction, begin, end, stage int) (*baseRef, string, error) {
	line := in.first
	args := in.text[begin:end]
	flags, at, err := flagsOf(args, line)
	if err != nil {
		return nil, "", err
	}
	ref := &baseRef{line: line, stage: stage}
	for _, fl := range flags {
		name, value, ok := strings.Cut(strings.TrimPrefix(fl.value, "--"), "=")
		if name != "platform" || !ok || ref.platform != nil {
			return nil, "", refuseBuild("FROM on line %d: flag %s is not one the classic builder reads", line+1, strconv.Quote(fl.value))
		}
		ref.platform = &value
	}
	words := fields(args[at:])
	name := ""
	switch {
	case len(words) == 1:
	case len(words) == 3 && strings.EqualFold(args[at+words[1][0]:at+words[1][1]], "as"):
		name = strings.ToLower(args[at+words[2][0] : at+words[2][1]])
		if !stageName.MatchString(name) {
			return nil, "", refuseBuild("FROM on line %d: invalid stage name %q", line+1, name)
		}
	default:
		return nil, "", refuseBuild("FROM on line %d: it takes an image and, optionally, AS and a stage name", line+1)
	}
	i, j := begin+at+words[0][0], begin+at+words[0][1]
	ref.raw = in.text[i:j]
	ref.start, ref.end, ref.whole = in.span(i, j)
	return ref, name, nil
}

// copyRef reads the --from of a COPY, nil if it has none.
func copyRef(in *instruction, begin, end, stage int) (*baseRef, error) {
	line := in.first
	flags, _, err := flagsOf(in.text[begin:end], line)
	if err != nil {
		return nil, err
	}
	var ref *baseRef
	for _, fl := range flags {
		name, value, ok := strings.Cut(strings.TrimPrefix(fl.value, "--"), "=")
		if !strings.EqualFold(name, "from") {
			continue
		}
		if name != "from" || !ok || ref != nil {
			return nil, refuseBuild("COPY on line %d: flag %s is not one the classic builder reads", line+1, strconv.Quote(fl.value))
		}
		ref = &baseRef{line: line, stage: stage, copy: true, raw: value}
		ref.start, ref.end, ref.whole = in.span(begin+fl.start, begin+fl.end)
	}
	return ref, nil
}

// heredoc is a heredoc of an instruction: the line that ends it, and
// whether leading tabs are taken off the lines first (<<-).
type heredoc struct {
	name  string
	chomp bool
}

// heredocs returns the heredocs the parser reads in the instruction text
// (RUN, COPY, ADD with "<<"), false if the gate cannot tell them: the
// parser splits the text into shell words (quotes and escapes read) and
// takes each word "<<NAME", "<<-NAME", "<<'NAME'", ... for one. A word of
// the gate's (split at blanks) with "<<" that is not such a word, plainly,
// is refused. So the parser takes as heredocs some of those the gate does
// (one of its words spans several of the gate's), in the same order: its
// heredocs end no later than the gate reads them to.
func heredocs(text string) ([]heredoc, bool) {
	var out []heredoc
	for _, w := range strings.FieldsFunc(text, unicode.IsSpace) {
		if !strings.Contains(w, "<<") {
			continue
		}
		m := heredocWord.FindStringSubmatch(w)
		if m == nil {
			return nil, false
		}
		out = append(out, heredoc{name: m[3] + m[4] + m[5], chomp: m[2] == "-"})
	}
	return out, true
}

// unreadBaseRefs reads every line of the Dockerfile as the start of an
// instruction, and refuses any FROM or COPY --from read that way that the
// gate did not read where the parser does: on a continued line, in a
// heredoc, past one the gate cannot delimit. (The escape token is the one
// escapeToken is sure of: the parser's.)
func (rd *dockerfileReading) unreadBaseRefs(budget *int) error {
	type key struct{ line, start, end int }
	have := map[key]bool{}
	for _, r := range rd.refs {
		have[key{r.line, r.start, r.end}] = true
	}
	for i := range rd.f.lines {
		in, ok, err := rd.f.join(i, rd.esc, []string{"from", "copy"}, budget)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		kw, begin, end := in.command()
		args := in.text[begin:end]
		flags, at := builderFlags(args, '\\')
		what, start, stop := "", 0, 0
		switch kw {
		case "from":
			words := fields(args[at:])
			if len(words) != 1 && (len(words) != 3 || !strings.EqualFold(args[at+words[1][0]:at+words[1][1]], "as")) {
				continue // the parser would fail on it
			}
			what = "FROM"
			start, stop, _ = in.span(begin+at+words[0][0], begin+at+words[0][1])
		case "copy":
			for _, fl := range flags {
				if name, _, _ := strings.Cut(strings.TrimPrefix(fl.value, "--"), "="); strings.EqualFold(name, "from") {
					what = "COPY --from"
					start, stop, _ = in.span(begin+fl.start, begin+fl.end)
					break
				}
			}
		}
		if what == "" || have[key{i, start, stop}] {
			continue
		}
		why := "it could continue the line before it"
		switch rd.kind[i] {
		case lineHeredoc:
			why = "it is in a heredoc"
		case lineUnread:
			why = fmt.Sprintf("it is past the heredoc of line %d, which the gate cannot delimit (the classic builder does not run heredocs)", rd.unread+1)
		case lineInstr:
			why = "the gate reads it otherwise"
		}
		return refuseBuild("line %d of the Dockerfile could be read as a %s the gate does not check (%s): write it so that it cannot", i+1, what, why)
	}
	return nil
}

// metaArgs are the ARGs before the first FROM, as the builder gives them to
// a FROM: an ARG's default, unless a build arg names it (the proxy
// variables need no ARG).
type metaArgs struct {
	opts     map[string]*string // the build args
	vals     map[string]*string // declared (nil: no default)
	poisoned map[string]bool    // declared with a default the gate cannot read
	unknown  bool               // an ARG the gate cannot read: any name may be declared
}

// lookup is name's value for a FROM, and whether it has one; an error
// when the gate cannot tell.
func (m *metaArgs) lookup(name string) (string, bool, error) {
	opt := m.opts[name]
	_, declared := m.vals[name]
	if opt != nil && (declared || slices.Contains(proxyArgs, name)) {
		return *opt, true, nil
	}
	if m.unknown || m.poisoned[name] {
		return "", false, fmt.Errorf("the gate cannot read the ARG that sets $%s", name)
	}
	if v := m.vals[name]; v != nil {
		return *v, true, nil
	}
	return "", false, nil
}

// declare reads an ARG before the first FROM, args its arguments: each
// name, or name=default, a shell word (expanded with the ARGs before this
// one).
func (m *metaArgs) declare(args string, esc byte) {
	if strings.HasPrefix(strings.TrimLeftFunc(args, unicode.IsSpace), "--") {
		m.unknown = true // flags: not an ARG the gate knows
		return
	}
	type decl struct {
		key string
		val *string
		bad bool
	}
	var decls []decl
	for _, w := range argWords(args, esc) {
		k, v, hasValue := strings.Cut(w, "=")
		key, err := expandWord(k, esc, m.lookup)
		if err != nil {
			m.unknown = true
			return
		}
		d := decl{key: key}
		if hasValue {
			val, err := expandWord(v, esc, m.lookup)
			d.val, d.bad = &val, err != nil
		}
		decls = append(decls, d)
	}
	for _, d := range decls { // an ARG's own names count from the next one on
		m.vals[d.key] = d.val
		m.poisoned[d.key] = d.bad
	}
}

// argWords splits an ARG's arguments as the parser does (parseWords):
// at blanks outside quotes, which, with escapes, stay in the words.
func argWords(rest string, esc byte) []string {
	const (
		inSpaces = iota
		inWord
		inQuote
	)
	var words []string
	phase := inSpaces
	var word strings.Builder
	var quote rune
	blankOK := false
	for pos := 0; pos <= len(rest); {
		ch, width := rune(0), 0
		if pos != len(rest) {
			ch, width = utf8.DecodeRuneInString(rest[pos:])
		}
		if phase == inSpaces {
			if pos == len(rest) {
				break
			}
			if unicode.IsSpace(ch) {
				pos += width
				continue
			}
			phase = inWord
		}
		if pos == len(rest) {
			if blankOK || word.Len() > 0 {
				words = append(words, word.String())
			}
			break
		}
		switch phase {
		case inWord:
			if unicode.IsSpace(ch) {
				phase = inSpaces
				if blankOK || word.Len() > 0 {
					words = append(words, word.String())
				}
				word.Reset()
				blankOK = false
				pos += width
				continue
			}
			if ch == '\'' || ch == '"' {
				quote, blankOK, phase = ch, true, inQuote
			}
			if ch == rune(esc) {
				if pos+width == len(rest) {
					pos += width
					continue
				}
				word.WriteRune(ch)
				pos += width
				ch, width = utf8.DecodeRuneInString(rest[pos:])
			}
			word.WriteRune(ch)
		case inQuote:
			if ch == quote {
				phase = inWord
			}
			if ch == rune(esc) && quote != '\'' {
				if pos+width == len(rest) {
					phase = inWord
					pos += width
					continue
				}
				pos += width
				word.WriteRune(ch)
				ch, width = utf8.DecodeRuneInString(rest[pos:])
			}
			word.WriteRune(ch)
		}
		pos += width
	}
	return words
}

// expandWord expands a word as the builder's shell lexer does
// (ProcessWord: quotes and escapes taken out, variables expanded), with
// lookup for the variables: $NAME, ${NAME}, ${NAME:-word}, ${NAME-word},
// ${NAME:+word}, ${NAME+word}. Any other form (positional and special
// parameters, ${NAME:?}, patterns, substitutions, whose support has varied
// between versions) is an error, as is what the lexer fails on.
func expandWord(s string, esc byte, lookup func(string) (string, bool, error)) (string, error) {
	w := &shellWord{s: s, esc: rune(esc), lookup: lookup}
	out, err := w.process(0)
	if err != nil {
		return "", fmt.Errorf("cannot expand %q: %v", s, err)
	}
	return out, nil
}

type shellWord struct {
	s      string
	pos    int
	esc    rune
	lookup func(string) (string, bool, error)
}

const eof = rune(-1)

func (w *shellWord) peek() rune {
	if w.pos >= len(w.s) {
		return eof
	}
	r, _ := utf8.DecodeRuneInString(w.s[w.pos:])
	return r
}

func (w *shellWord) next() rune {
	if w.pos >= len(w.s) {
		return eof
	}
	r, n := utf8.DecodeRuneInString(w.s[w.pos:])
	w.pos += n
	return r
}

// process reads up to stop (0: the end): the lexer's processStopOn.
func (w *shellWord) process(stop rune) (string, error) {
	var out strings.Builder
	for w.peek() != eof {
		ch := w.peek()
		if stop != 0 && ch == stop {
			w.next()
			return out.String(), nil
		}
		switch ch {
		case '$':
			v, err := w.dollar()
			if err != nil {
				return "", err
			}
			out.WriteString(v)
		case '\'':
			w.next()
			for {
				c := w.next()
				if c == eof {
					return "", errors.New("unmatched single quote")
				}
				if c == '\'' {
					break
				}
				out.WriteRune(c)
			}
		case '"':
			v, err := w.doubleQuoted()
			if err != nil {
				return "", err
			}
			out.WriteString(v)
		default:
			c := w.next()
			if c == w.esc {
				c = w.next() // an escape token at the end is dropped
			}
			if c != eof {
				out.WriteRune(c)
			}
		}
	}
	if stop != 0 {
		return "", fmt.Errorf("missing %q", stop)
	}
	return out.String(), nil
}

func (w *shellWord) doubleQuoted() (string, error) {
	var out strings.Builder
	w.next()
	for {
		switch ch := w.peek(); ch {
		case eof:
			return "", errors.New("unmatched double quote")
		case '"':
			w.next()
			return out.String(), nil
		case '$':
			v, err := w.dollar()
			if err != nil {
				return "", err
			}
			out.WriteString(v)
		default:
			c := w.next()
			if c == w.esc {
				switch w.peek() {
				case eof:
					continue
				case '"', '$', w.esc:
					c = w.next()
				}
			}
			out.WriteRune(c)
		}
	}
}

// name reads a variable name: a letter or _, then letters, digits and _.
// A digit or a special parameter first is refused.
func (w *shellWord) name() (string, error) {
	var b strings.Builder
	for {
		ch := w.peek()
		if b.Len() == 0 && (unicode.IsDigit(ch) || strings.ContainsRune("@*#?-$!", ch)) {
			return "", fmt.Errorf("$%c: positional and special parameters are not read", ch)
		}
		if ch == eof || !unicode.IsLetter(ch) && !unicode.IsDigit(ch) && ch != '_' {
			return b.String(), nil
		}
		b.WriteRune(w.next())
	}
}

func (w *shellWord) dollar() (string, error) {
	w.next()
	if w.peek() != '{' {
		if c := w.peek(); c == eof || !unicode.IsLetter(c) && !unicode.IsDigit(c) && c != '_' && !strings.ContainsRune("@*#?-$!", c) {
			return "$", nil // a lone $ is itself
		}
		name, err := w.name()
		if err != nil {
			return "", err
		}
		v, _, err := w.lookup(name)
		return v, err
	}
	w.next()
	name, err := w.name()
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", errors.New("bad substitution")
	}
	op := w.next()
	colon := op == ':'
	if colon {
		op = w.next()
	}
	switch {
	case op == '}' && !colon:
		v, _, err := w.lookup(name)
		return v, err
	case op == '-' || op == '+':
		word, err := w.process('}')
		if err != nil {
			return "", err
		}
		v, set, err := w.lookup(name)
		if err != nil {
			return "", err
		}
		unset := !set || colon && v == ""
		switch {
		case op == '-' && unset:
			return word, nil
		case op == '-':
			return v, nil
		case unset:
			return "", nil
		default:
			return word, nil
		}
	}
	return "", fmt.Errorf("${%s...}: only ${NAME}, ${NAME:-word}, ${NAME-word}, ${NAME:+word} and ${NAME+word} are read", name)
}

// plainWord reports whether s is the same expanded: nothing in it the
// lexer reads (no $, quote or escape character).
func plainWord(s string) bool {
	return !strings.ContainsAny(s, "$'\"\\`")
}

// pinBaseImages is the base image pass of a build whose Dockerfile is df,
// its context in spool (see the top of this file). It returns the context
// to send instead, with the Dockerfile rewritten, or nil when nothing needs
// to be (spool is then sent as it is).
func (g *Gate) pinBaseImages(r *http.Request, spool *os.File, df *contextDockerfile) (*os.File, error) {
	if df == nil {
		return nil, nil
	}
	q := r.URL.Query()
	if len(q["buildargs"]) > 1 {
		return nil, refuseBuild("build args given twice")
	}
	var opts map[string]*string
	if s := q.Get("buildargs"); s != "" {
		if err := json.Unmarshal([]byte(s), &opts); err != nil {
			return nil, fmt.Errorf("unreadable build args: %v", err)
		}
	}
	rd, err := readDockerfile(df.content, opts)
	if err != nil {
		return nil, err
	}
	edits, err := g.pinRefs(rd, q.Get("target"))
	if err != nil {
		return nil, err
	}
	if len(edits) == 0 {
		return nil, nil
	}
	content := applyEdits(df.content, edits)
	if err := checkDockerfile(content); err != nil { // as it was: names were replaced by others
		return nil, err
	}
	return rewriteContext(spool, df.path, content)
}

// edit replaces src[start:end] with text.
type edit struct {
	start, end int
	text       string
}

func applyEdits(src []byte, edits []edit) []byte {
	slices.SortFunc(edits, func(a, b edit) int { return a.start - b.start })
	var out bytes.Buffer
	at := 0
	for _, e := range edits {
		out.Write(src[at:e.start])
		out.WriteString(e.text)
		at = e.end
	}
	out.Write(src[at:])
	return out.Bytes()
}

// pinRefs checks each image the Dockerfile names (up to the target stage,
// if one is named: the builder runs no later stage) and returns the edits
// that name each by its ID; a FROM that expands to a stage or "scratch"
// is rewritten to name it plainly.
func (g *Gate) pinRefs(rd *dockerfileReading, target string) ([]edit, error) {
	last := len(rd.stages) - 1
	if target != "" {
		if i := slices.IndexFunc(rd.stages, func(n string) bool { return strings.EqualFold(n, target) }); i >= 0 {
			last = i
		}
	}
	ids := map[string]string{} // image name -> ID
	var edits []edit
	for _, ref := range rd.refs {
		if ref.stage < 0 || ref.stage > last {
			continue // before any FROM (the parser fails) or never built
		}
		name := ref.raw
		if !ref.copy {
			if ref.platform != nil {
				if err := g.checkPlatform(*ref.platform, rd); err != nil {
					return nil, refuseBuild("%s: %v", ref.what(), err)
				}
			}
			var err error
			if name, err = expandWord(ref.raw, rd.esc, rd.meta.lookup); err != nil {
				return nil, refuseBuild("%s: %v", ref.what(), err)
			}
			if name == "" {
				return nil, refuseBuild("%s: the image name is empty", ref.what())
			}
		}
		text := ""
		switch stage := rd.stageBefore(name, ref.stage); {
		case name == "":
			continue // COPY --from=: from the context
		case stage != "":
			if ref.copy || plainWord(ref.raw) {
				continue
			}
			text = stage
		case !ref.copy && name == "scratch":
			if plainWord(ref.raw) {
				continue
			}
			text = name
		case ref.copy && isStageIndex(name, 32):
			continue // a stage by its index, or an error
		case ref.copy && isStageIndex(name, 64):
			return nil, refuseBuild("%s: no stage has that index", ref.what())
		default:
			id, ok := ids[name]
			if !ok {
				if len(ids) == maxBaseImages {
					return nil, refuseBuild("the Dockerfile names more than %d images", maxBaseImages)
				}
				var err error
				if id, err = g.baseImage(name); err != nil {
					var refusal *buildRefusal
					if errors.As(err, &refusal) {
						return nil, refuseBuild("%s: %v", ref.what(), err)
					}
					return nil, err
				}
				ids[name] = id
			}
			text = id
		}
		if ref.copy {
			text = "--from=" + text
		}
		if !ref.whole {
			return nil, refuseBuild("%s: the image name is on more than one line: write it on one", ref.what())
		}
		edits = append(edits, edit{ref.start, ref.end, text})
	}
	return edits, nil
}

// stageBefore is the name of the stage before stage that name names (the
// builder compares in lower case), "" if none does.
func (rd *dockerfileReading) stageBefore(name string, stage int) string {
	name = strings.ToLower(name)
	for _, n := range rd.stages[:max(stage, 0)] {
		if n != "" && n == name {
			return n
		}
	}
	return ""
}

// isStageIndex reports whether COPY --from=s names a stage by its index
// for a builder whose int has bits: it takes any decimal number it can
// read for one (a stage, or an error), anything else for an image.
func isStageIndex(s string, bits int) bool {
	_, err := strconv.ParseInt(s, 10, bits)
	return err == nil
}

// baseImage checks an image a build names, by the name the builder looks
// it up by: it must come from a registry tasks may pull from, be here
// (the gate does not let the builder pull it), and have no ONBUILD
// triggers. It returns its ID.
func (g *Gate) baseImage(name string) (string, error) {
	ref, err := parseImageRef(name)
	if err != nil {
		return "", &buildRefusal{err.Error()}
	}
	if err := g.checkRegistry(ref); err != nil {
		var refusal *registryRefusal
		if errors.As(err, &refusal) {
			return "", &buildRefusal{err.Error()}
		}
		return "", err
	}
	status, out, err := g.call("GET", "/images/"+name+"/json", nil)
	switch {
	case err != nil:
		return "", err
	case status == http.StatusNotFound:
		return "", &buildRefusal{"the image is not here, and a build does not pull: pull it first: docker pull " + name}
	case status != http.StatusOK:
		return "", fmt.Errorf("cannot inspect image %s", name)
	}
	var img struct {
		ID     string `json:"Id"`
		Config *struct{ OnBuild []string }
	}
	if err := json.Unmarshal(out, &img); err != nil || !imageID.MatchString(img.ID) {
		return "", fmt.Errorf("cannot inspect image %s", name)
	}
	if img.Config != nil && len(img.Config.OnBuild) > 0 {
		return "", &buildRefusal{fmt.Sprintf("image %s has ONBUILD triggers, which would run in this build unchecked "+
			"(an ADD of a URL is fetched from the host's network): build from another image", name)}
	}
	return img.ID, nil
}

// checkPlatform checks the --platform of a FROM: expanded, it must be
// empty (the builder then fails the build) or the daemon's own platform;
// for another, the builder would pull the image, as with build --platform.
func (g *Gate) checkPlatform(raw string, rd *dockerfileReading) error {
	p, err := expandWord(raw, rd.esc, rd.meta.lookup)
	if err != nil {
		return err
	}
	if p == "" {
		return nil
	}
	status, out, err := g.call("GET", "/version", nil)
	if err != nil {
		return err
	}
	var v struct{ Os, Arch string }
	if status != http.StatusOK || json.Unmarshal(out, &v) != nil || v.Os == "" || v.Arch == "" {
		return errors.New("cannot read the daemon's platform")
	}
	if !samePlatform(p, v.Os, v.Arch) {
		return fmt.Errorf("--platform=%s is not the daemon's platform (%s/%s): the builder would pull the image for it; "+
			"pull it for that platform first, under another name, and build FROM that", p, v.Os, v.Arch)
	}
	return nil
}

// samePlatform reports whether platform p (os/arch[/variant], as a FROM
// gives it) is os/arch, the daemon's.
func samePlatform(p, os, arch string) bool {
	norm := func(a string) string {
		switch a = strings.ToLower(a); a {
		case "x86_64", "x86-64":
			return "amd64"
		case "aarch64":
			return "arm64"
		}
		return a
	}
	parts := strings.Split(p, "/")
	if len(parts) < 2 || len(parts) > 3 || !strings.EqualFold(parts[0], os) || norm(parts[1]) != norm(arch) {
		return false
	}
	return len(parts) == 2 || norm(arch) == "arm64" && oneOf(strings.ToLower(parts[2]), "v8", "8")
}

// rewriteContext writes the build context in spool again, uncompressed,
// with content as the Dockerfile at path dfPath (every entry there: each
// is a regular file, see readContext), to a new file it returns (at its
// end, as spool was). It reads that file back as checkBuildContext reads a
// context: the daemon finds content there.
func rewriteContext(spool *os.File, dfPath string, content []byte) (*os.File, error) {
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	rd, err := openContext(spool)
	if err != nil {
		return nil, err
	}
	out, err := os.CreateTemp("", "atl-build-context-")
	if err != nil {
		return nil, err
	}
	os.Remove(out.Name())
	fail := func(err error) (*os.File, error) {
		out.Close()
		return nil, err
	}
	tr := tar.NewReader(rd)
	tw := tar.NewWriter(out)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail(fmt.Errorf("unreadable build context: %v", err))
		}
		body := io.Reader(tr)
		if name, ok := contextPath(path.Clean(h.Name)); ok && name == dfPath {
			h.Size = int64(len(content))
			body = bytes.NewReader(content)
		}
		if err := tw.WriteHeader(h); err != nil {
			return fail(fmt.Errorf("cannot write the build context again with the Dockerfile's images pinned (entry %q): %v", h.Name, err))
		}
		if _, err := io.Copy(tw, body); err != nil {
			return fail(fmt.Errorf("cannot write the build context again with the Dockerfile's images pinned (entry %q): %v", h.Name, err))
		}
	}
	if err := tw.Close(); err != nil {
		return fail(err)
	}
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	files, err := readContext(out, []string{dfPath})
	if err != nil {
		return fail(err)
	}
	if !bytes.Equal(files[dfPath], content) {
		return fail(errors.New("the build context written again does not have the Dockerfile written"))
	}
	if _, err := out.Seek(0, io.SeekEnd); err != nil {
		return fail(err)
	}
	return out, nil
}
