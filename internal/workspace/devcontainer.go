package workspace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/sandbox"
)

// DevcontainerFile is where a devcontainer box's workspace has its box's
// definition, relative to the workspace directory.
const DevcontainerFile = ".devcontainer/devcontainer.json"

// devcontainerImport is what a box's devcontainer.json says about the
// settings of its workspace.
type devcontainerImport struct {
	Config   Config   // the settings found (Sandbox never nil)
	Skipped  []string // values not taken, each with the reason
	Warnings []string
}

// envKeys maps the variables a box set (in containerEnv or remoteEnv) to
// the atelier.toml keys they become.
var envKeys = map[string]string{
	"ATL_REPOS":           "repos",
	"ATL_CONTEXT":         "context",
	"ATL_TMUX_SOCKET":     "tmux_socket",
	"ATL_SANDBOX_IMAGE":   "sandbox.image",
	sandbox.MountsVar:     "sandbox.mounts",
	"ATL_SANDBOX_CONTEXT": "sandbox.context",
	sandbox.RegistriesVar: "sandbox.registries",
}

// secretRE matches the names of variables that hold secrets, which are
// never written to atelier.toml.
var secretRE = regexp.MustCompile(`KEY|TOKEN|SECRET|PASSWORD`)

// readDevcontainer reads dir's .devcontainer/devcontainer.json (JSON with
// comments and trailing commas), if it exists, for the settings of the
// workspace in dir: the ATL_* variables of containerEnv and remoteEnv
// (remoteEnv winning, as in the box's shells), with ${localEnv:VAR} and
// ${localEnv:VAR:default} expanded from atl's environment, and the source
// of a bind mount on the container user's ~/.gitconfig as git_config. It
// returns false when the file does not exist.
func readDevcontainer(dir string) (devcontainerImport, bool, error) {
	im := devcontainerImport{Config: Config{Sandbox: &SandboxConfig{}}}
	data, err := os.ReadFile(filepath.Join(dir, DevcontainerFile))
	if errors.Is(err, os.ErrNotExist) {
		return im, false, nil
	}
	if err != nil {
		return im, true, err
	}
	var dc struct {
		ContainerEnv map[string]any    `json:"containerEnv"`
		RemoteEnv    map[string]any    `json:"remoteEnv"`
		Mounts       []json.RawMessage `json:"mounts"`
	}
	clean, err := jsoncToJSON(data)
	if err == nil {
		err = json.Unmarshal(clean, &dc)
	}
	if err != nil {
		return im, true, fmt.Errorf("parse %s: %w", DevcontainerFile, err)
	}
	env := map[string]any{}
	for k, v := range dc.ContainerEnv {
		env[k] = v
	}
	for k, v := range dc.RemoteEnv {
		env[k] = v
	}
	for _, k := range sortedKeys(env) {
		im.variable(dir, k, env[k])
	}
	for _, raw := range dc.Mounts {
		im.mount(dir, raw)
	}
	return im, true, nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (im *devcontainerImport) skip(format string, a ...any) {
	im.Skipped = append(im.Skipped, fmt.Sprintf(format, a...))
}

func (im *devcontainerImport) warn(format string, a ...any) {
	im.Warnings = append(im.Warnings, fmt.Sprintf(format, a...))
}

// variable takes variable k of the box's environment, with value v.
func (im *devcontainerImport) variable(dir, k string, v any) {
	if secretRE.MatchString(strings.ToUpper(k)) {
		im.skip("%s: a secret, never written to %s (on the host, `atl token` stores the Claude token in the workspace, or set it in the environment you start atl in)", k, File)
		return
	}
	key, setting := envKeys[k]
	if !setting && k != Var && k != "ATL_SANDBOX" {
		if strings.HasPrefix(k, "ATL_") {
			im.skip("%s: not a workspace setting (set it in the environment you start atl in, if you need it)", k)
		}
		return // not atl's
	}
	s, ok := v.(string)
	if !ok {
		im.skip("%s: not a string", k)
		return
	}
	s, err := expandLocalEnv(s)
	if err != nil {
		im.skip("%s: %v", k, err)
		return
	}
	switch k {
	case Var:
		if !sameWorkspace(s, dir) {
			im.warn("%s in %s names %s, not this directory (%s): check that this is the box's workspace", k, DevcontainerFile, s, dir)
		}
		return
	case "ATL_SANDBOX":
		if s != "" && s != "docker" {
			im.warn("ATL_SANDBOX=%s in %s is not carried over: on the host, sessions run in task containers", s, DevcontainerFile)
		}
		return
	}
	setKey(&im.Config, key, s)
}

// sameWorkspace reports whether v, an $ATL_WORKSPACE value (a name or a
// path), names the workspace in dir.
func sameWorkspace(v, dir string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return true
	}
	if !strings.ContainsRune(v, '/') && !strings.HasPrefix(v, "~") {
		return v == filepath.Base(dir)
	}
	home, _ := os.UserHomeDir()
	p := filepath.Clean(config.ExpandHome(v, home))
	if p == filepath.Clean(dir) {
		return true
	}
	a, err1 := os.Stat(p)
	b, err2 := os.Stat(dir)
	return err1 == nil && err2 == nil && os.SameFile(a, b)
}

// mount takes one entry of the box's mounts: only a bind mount on the
// container user's ~/.gitconfig says something about the workspace (its
// global git config). The others are the box's own plumbing, which atl on
// the host does not need: the workspace directory and the repos are the
// host's own paths there, ~/.claude is the user's Claude Code config at
// the same path, the Docker socket is reached directly, and the files of
// ATL_SANDBOX_MOUNTS come from that variable.
func (im *devcontainerImport) mount(dir string, raw json.RawMessage) {
	var m struct {
		Source string `json:"source"`
		Target string `json:"target"`
		Type   string `json:"type"`
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		for _, part := range strings.Split(s, ",") {
			k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
			switch strings.ToLower(k) {
			case "source", "src":
				m.Source = v
			case "target", "destination", "dst":
				m.Target = v
			case "type":
				m.Type = v
			}
		}
	} else if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	if m.Type != "bind" {
		return
	}
	target, err := expandLocalEnv(m.Target)
	if err != nil {
		if strings.HasSuffix(m.Target, "/.gitconfig") {
			im.skip("the mount on %s: %v", m.Target, err)
		}
		return
	}
	if !gitconfigTarget(target) {
		return
	}
	src, err := expandLocalEnv(m.Source)
	if err != nil {
		im.skip("the mount on %s: %v", target, err)
		return
	}
	if !filepath.IsAbs(src) {
		im.skip("the mount on %s: source %q is not an absolute path", target, src)
		return
	}
	src = filepath.Clean(src)
	rel, err := filepath.Rel(dir, src)
	switch {
	case err == nil && rel == "gitconfig":
		im.skip("the mount on %s: its source is %s, which git_config already defaults to", target, src)
		return
	case err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)):
		src = rel
	}
	im.Config.GitConfig = src
}

// gitconfigTarget reports whether p is ~/.gitconfig of a container user:
// /root/.gitconfig, /home/<user>/.gitconfig, or the one in atl's own $HOME
// (a box mounted things at the host's paths, ${localEnv:HOME}).
func gitconfigTarget(p string) bool {
	p = filepath.Clean(p)
	if p == "/root/.gitconfig" {
		return true
	}
	if h := os.Getenv("HOME"); h != "" && p == filepath.Join(h, ".gitconfig") {
		return true
	}
	d, f := filepath.Split(p)
	user := filepath.Base(d)
	return f == ".gitconfig" && filepath.Clean(d) == filepath.Join("/home", user)
}

// expandLocalEnv expands ${localEnv:VAR} and ${localEnv:VAR:default} in s
// from atl's environment (the default when VAR is unset or empty); any
// other ${...} (containerEnv:, containerWorkspaceFolder, ...) is an error:
// what it means existed only in the box.
func expandLocalEnv(s string) (string, error) {
	var b strings.Builder
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			b.WriteString(s)
			return b.String(), nil
		}
		b.WriteString(s[:i])
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			return "", fmt.Errorf("%q: unterminated ${", s)
		}
		expr := s[i+2 : i+j]
		rest, ok := strings.CutPrefix(expr, "localEnv:")
		if !ok {
			return "", fmt.Errorf("${%s} is not ${localEnv:...}, so it has no value outside the box", expr)
		}
		name, def, hasDef := strings.Cut(rest, ":")
		v := os.Getenv(name)
		if v == "" && hasDef {
			v = def
		}
		b.WriteString(v)
		s = s[i+j+1:]
	}
}

// jsoncToJSON turns JSON with comments (// and /* */, outside strings)
// and trailing commas (before } or ]) into JSON.
func jsoncToJSON(data []byte) ([]byte, error) {
	out := make([]byte, 0, len(data))
	comma := -1 // index in out of a comma not yet followed by a value
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch {
		case c == '"':
			start := i
			for i++; i < len(data) && data[i] != '"'; i++ {
				if data[i] == '\\' {
					i++
				}
			}
			if i >= len(data) {
				return nil, errors.New("unterminated string")
			}
			out = append(out, data[start:i+1]...)
			comma = -1
		case c == '/' && i+1 < len(data) && data[i+1] == '/':
			for i < len(data) && data[i] != '\n' {
				i++
			}
			if i < len(data) {
				out = append(out, '\n')
			}
		case c == '/' && i+1 < len(data) && data[i+1] == '*':
			end := bytes.Index(data[i+2:], []byte("*/"))
			if end < 0 {
				return nil, errors.New("unterminated /* comment")
			}
			i += 2 + end + 1
			out = append(out, ' ')
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			out = append(out, c)
		default:
			if (c == '}' || c == ']') && comma >= 0 {
				out[comma] = ' '
			}
			comma = -1
			if c == ',' {
				comma = len(out)
			}
			out = append(out, c)
		}
	}
	return out, nil
}
