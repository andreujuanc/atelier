package workspace

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestJSONC(t *testing.T) {
	in := `// a box
{
  /* the image
     it runs */ "image": "atelier", // trailing comment
  "url": "http://example.com/a//b", "glob": "/* not a comment */",
  "quote": "say \"hi\" // still a string\\",
  "list": [1, 2, /* three */ 3,],
  "obj": {"a": "b", // why
  },
}
`
	out, err := jsoncToJSON([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	want := map[string]any{
		"image": "atelier", "url": "http://example.com/a//b", "glob": "/* not a comment */",
		"quote": `say "hi" // still a string\`, "list": []any{1.0, 2.0, 3.0}, "obj": map[string]any{"a": "b"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	for _, bad := range []string{`{"a": 1 /* open`, `{"a": "open`} {
		if _, err := jsoncToJSON([]byte(bad)); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	// A comma inside a string is not a trailing comma.
	if out, _ := jsoncToJSON([]byte(`["a,", "b"]`)); string(out) != `["a,", "b"]` {
		t.Errorf("string comma: %s", out)
	}
}

func TestExpandLocalEnv(t *testing.T) {
	t.Setenv("HOME", "/home/you")
	t.Setenv("ATL_TEST_EMPTY", "")
	for in, want := range map[string]string{
		"${localEnv:HOME}/src/work":           "/home/you/src/work",
		"${localEnv:ATL_TEST_UNSET:/opt/x}":   "/opt/x",
		"${localEnv:ATL_TEST_EMPTY:fallback}": "fallback",
		"${localEnv:HOME:/nope}":              "/home/you",
		"${localEnv:ATL_TEST_UNSET}":          "",
		"a${localEnv:HOME}b${localEnv:HOME}c": "a/home/youb/home/youc",
		"plain":                               "plain",
	} {
		if got, err := expandLocalEnv(in); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"${containerEnv:HOME}/x", "${containerWorkspaceFolder}", "${localWorkspaceFolder}/a", "${localEnv:HOME"} {
		if got, err := expandLocalEnv(in); err == nil {
			t.Errorf("%q: %q, want an error", in, got)
		}
	}
}

func TestGitconfigTarget(t *testing.T) {
	for p, want := range map[string]bool{
		"/home/you/.gitconfig":   true,
		"/home/node/.gitconfig/": true,
		"/root/.gitconfig":       true,
		"/home/.gitconfig":       false,
		"/home/a/b/.gitconfig":   false,
		"/etc/gitconfig":         false,
		"/home/you/.gitconfig.x": false,
		"/srv/.gitconfig":        false,
	} {
		if got := gitconfigTarget(p); got != want {
			t.Errorf("%s: %v", p, got)
		}
	}
}

// box makes directory name under root with a database and, unless dc is
// "", .devcontainer/devcontainer.json with content dc.
func box(t *testing.T, root, name, dc string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data", "atelier.db"), []byte("db"), 0o644); err != nil {
		t.Fatal(err)
	}
	if dc != "" {
		if err := os.MkdirAll(filepath.Join(dir, ".devcontainer"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, DevcontainerFile), []byte(dc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func plannedLines(p *Plan) []string {
	var out []string
	for _, s := range p.Settings {
		out = append(out, s.String())
	}
	return out
}

// TestAdoptDevcontainer: adopting a box's workspace (it has a database)
// on the host takes the box's settings from its devcontainer.json, so it
// needs no flags; secrets and values that exist only in the box are
// skipped and said, and .devcontainer/ stays.
func TestAdoptDevcontainer(t *testing.T) {
	root := env(t)
	home := os.Getenv("HOME")
	t.Setenv("NPMRC_NAME", ".work.npmrc")
	dir := box(t, root, "work", `// the box of workspace work
{
  "image": "atelier",
  "mounts": [
    "source=${localEnv:HOME}/ws/work,target=${localEnv:HOME}/ws/work,type=bind",
    "source=${localEnv:HOME}/src/work,target=${localEnv:HOME}/src/work,type=bind",
    "source=${localEnv:HOME}/ws/work/git/config,target=${localEnv:HOME}/.gitconfig,type=bind,readonly",
    "source=/var/run/docker.sock,target=/var/run/docker.sock,type=bind",
  ],
  "containerEnv": {
    "ATL_WORKSPACE": "${localEnv:HOME}/ws/work", // the dir itself
    "ATL_REPOS": "${localEnv:HOME}/src/work",
    "ATL_SANDBOX": "docker",
    "ATL_SANDBOX_MOUNTS": "${localEnv:HOME}/${localEnv:NPMRC_NAME}:/etc/hosts",
    "ATL_SANDBOX_IMAGE": "${localEnv:ATL_TEST_IMAGE:work-task}",
    "ATL_CONTEXT": "${containerEnv:RULES}",
    "CLAUDE_CODE_OAUTH_TOKEN": "sk-very-secret",
    "NPM_AUTH_KEY": "also-secret",
    "ATL_TASKS": "/elsewhere",
    "LANG": "C.UTF-8",
  },
  "remoteEnv": {"ATL_SANDBOX_CONTEXT": "/* sandboxed */ rules"},
}`)
	p, err := Prepare(root, "work", Config{})
	if err != nil {
		t.Fatal(err)
	}
	const from = " (from " + DevcontainerFile + ")"
	want := []string{
		"repos = ~/src/work" + from,
		"git_config = git/config" + from,
		"sandbox.image = work-task" + from,
		"sandbox.mounts = ~/.work.npmrc, /etc/hosts" + from,
		`sandbox.context = "/* sandboxed */ rules"` + from,
	}
	if got := plannedLines(p); !reflect.DeepEqual(got, want) {
		t.Errorf("settings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	all := strings.Join(append(p.Skipped, p.Warnings...), "\n")
	for _, w := range []string{"CLAUDE_CODE_OAUTH_TOKEN: a secret", "`atl token`", "NPM_AUTH_KEY: a secret",
		"ATL_CONTEXT: ${containerEnv:RULES} is not ${localEnv:...}", "ATL_TASKS: not a workspace setting"} {
		if !strings.Contains(all, w) {
			t.Errorf("skipped lacks %q:\n%s", w, all)
		}
	}
	if strings.Contains(all, "sk-very") || strings.Contains(all, "also-secret") || strings.Contains(all, "LANG") {
		t.Errorf("skipped says too much:\n%s", all)
	}
	if len(p.Warnings) != 0 || len(p.Notes) != 0 || !p.Adopt {
		t.Errorf("warnings %q notes %q adopt %v", p.Warnings, p.Notes, p.Adopt)
	}
	if _, err := os.Stat(filepath.Join(dir, File)); err == nil {
		t.Fatal("Prepare wrote atelier.toml")
	}
	if err := p.Write(); err != nil {
		t.Fatal(err)
	}
	c, err := ReadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	wantC := Config{Repos: filepath.Join(home, "src", "work"), GitConfig: "git/config",
		Sandbox: &SandboxConfig{Image: "work-task", Mounts: []string{filepath.Join(home, ".work.npmrc"), "/etc/hosts"}, Context: "/* sandboxed */ rules"}}
	if !reflect.DeepEqual(c, wantC) {
		t.Errorf("atelier.toml: %+v %+v", c, c.Sandbox)
	}
	if b, err := os.ReadFile(filepath.Join(dir, DevcontainerFile)); err != nil || len(b) == 0 {
		t.Errorf(".devcontainer removed: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, File)); strings.Contains(string(b), "secret") || strings.Contains(string(b), "tmux_socket") {
		t.Errorf("atelier.toml:\n%s", b)
	}

	// The gitconfig: outside the workspace an absolute path; the
	// workspace's gitconfig is the default, not written; object mounts.
	// ATL_SANDBOX=off and an ATL_WORKSPACE of another directory: warnings.
	box(t, root, "personal", `{
  "mounts": [
    {"source": "/srv/git/personal.gitconfig", "target": "/root/.gitconfig", "type": "bind"},
  ],
  "containerEnv": {"ATL_REPOS": "/srv/personal", "ATL_SANDBOX": "off", "ATL_WORKSPACE": "/somewhere/else"}
}`)
	p, err = Prepare(root, "personal", Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got := plannedLines(p); len(got) != 2 || got[1] != "git_config = /srv/git/personal.gitconfig"+from {
		t.Errorf("personal: %q", got)
	}
	if w := strings.Join(p.Warnings, "\n"); !strings.Contains(w, "ATL_SANDBOX=off") || !strings.Contains(w, "names /somewhere/else, not this directory") {
		t.Errorf("warnings %q", p.Warnings)
	}
	def := box(t, root, "def", `{"mounts": ["source=${localEnv:HOME}/ws/def/gitconfig,target=/home/node/.gitconfig,type=bind"],
  "containerEnv": {"ATL_REPOS": "/srv/def", "ATL_WORKSPACE": "def"}}`)
	p, err = Prepare(root, "def", Config{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Config.GitConfig != "" || len(p.Warnings) != 0 || !strings.Contains(strings.Join(p.Skipped, "\n"), filepath.Join(def, "gitconfig")+", which git_config already defaults to") {
		t.Errorf("def: %+v skipped %q warnings %q", p.Config, p.Skipped, p.Warnings)
	}

	// No repos in it (and none given): refused as without one.
	box(t, root, "norepos", `{"containerEnv": {"ATL_SANDBOX_IMAGE": "x"}}`)
	var ae *AdoptDBError
	if _, err := Prepare(root, "norepos", Config{}); !errors.As(err, &ae) || !strings.Contains(err.Error(), "--repos DIR") {
		t.Errorf("norepos: %v", err)
	}
	// Selecting it says to adopt it; with repos in devcontainer.json without --repos.
	if _, err := Load(filepath.Join(root, "norepos")); err == nil || !strings.Contains(err.Error(), "--repos DIR") ||
		!strings.Contains(err.Error(), `"Moving from a devcontainer box" in atelier's docs/workspaces.md`) {
		t.Errorf("load norepos: %v", err)
	}
	box(t, root, "hasrepos", `{"containerEnv": {"ATL_REPOS": "/srv/r"}}`)
	if _, err := Load(filepath.Join(root, "hasrepos")); err == nil || strings.Contains(err.Error(), "--repos") || !strings.Contains(err.Error(), "`atl workspace new hasrepos`") {
		t.Errorf("load hasrepos: %v", err)
	}
	// A devcontainer.json that does not parse: said, nothing taken.
	box(t, root, "broken", `{"containerEnv": {`)
	p, err = Prepare(root, "broken", Config{Repos: "/srv/b"})
	if err != nil || len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "parse "+DevcontainerFile) {
		t.Errorf("broken: %v %+v", err, p)
	}
}

// TestAdoptPrecedence: a setting comes from the flags, else the box's
// devcontainer.json, else the user's environment, else the default; each
// planned setting says which.
func TestAdoptPrecedence(t *testing.T) {
	root := env(t)
	box(t, root, "alpha", `{"containerEnv": {"ATL_REPOS": "/srv/dc-repos", "ATL_SANDBOX_IMAGE": "dc-image", "ATL_SANDBOX_MOUNTS": "/dc/mount"}}`)
	t.Setenv("ATL_REPOS", "/srv/env-repos")
	t.Setenv("ATL_CONTEXT", "env context")
	t.Setenv("ATL_SANDBOX_IMAGE", "env-image")
	p, err := Prepare(root, "alpha", Config{Sandbox: &SandboxConfig{Image: "flag-image"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"repos = /srv/dc-repos (from " + DevcontainerFile + ")",
		`context = "env context" (from $ATL_CONTEXT)`,
		"sandbox.image = flag-image (from --image)",
		"sandbox.mounts = /dc/mount (from " + DevcontainerFile + ")",
	}
	if got := plannedLines(p); !reflect.DeepEqual(got, want) {
		t.Errorf("settings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// A value atl set for a workspace is not the user's.
	t.Setenv(VarsVar, "ATL_CONTEXT="+valueHash("env context"))
	p, err = Prepare(root, "alpha", Config{Repos: "/srv/flag"})
	if err != nil {
		t.Fatal(err)
	}
	if got := plannedLines(p); len(got) != 3 || got[0] != "repos = /srv/flag (from --repos)" || got[1] != "sandbox.image = dc-image (from "+DevcontainerFile+")" {
		t.Errorf("settings %q", got)
	}
	// A new workspace (no directory yet) reads no devcontainer.json.
	p, err = Prepare(root, "beta", Config{})
	if err != nil || p.Adopt || len(plannedLines(p)) != 2 || plannedLines(p)[0] != "repos = /srv/env-repos (from $ATL_REPOS)" {
		t.Errorf("beta: %v %q", err, plannedLines(p))
	}
}
