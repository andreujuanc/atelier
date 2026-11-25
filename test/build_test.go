package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestBuildImagesUnderCDPATH: build-images run by a relative path, with a
// CDPATH that has "." (cd then prints the directory it changed to), still
// hands docker build the checkout itself as the context.
func TestBuildImagesUnderCDPATH(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	stub := "#!/bin/sh\nfor a; do last=$a; done\nprintf '[%s]' \"$last\" >> \"$DOCKER_LOG\"\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "docker.log")
	cmd := exec.Command("sh", filepath.Join(filepath.Base(root), "build-images"))
	cmd.Dir = filepath.Dir(root)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "CDPATH=.:/", "DOCKER_LOG="+log)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build-images: %v\n%s", err, out)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	// The last argument of each docker build call: the context.
	if got, want := string(data), "["+root+"]["+root+"]"; got != want {
		t.Errorf("build contexts %q, want %q", got, want)
	}
}

// TestDockerfileNoPipeToShell: no RUN of the Dockerfile pipes a download
// into a shell. RUN's sh has no pipefail, so a failed download would run an
// empty script and the build would go on without what it installs.
func TestDockerfileNoPipeToShell(t *testing.T) {
	for _, c := range []struct {
		dockerfile string
		bad        bool
	}{
		{"RUN curl -fsSL https://x/install.sh | bash\n", true},
		{"RUN curl -fsSL https://x/install.sh \\\n    | sh -s -- -y\n", true},
		{"RUN apt-get update \\\n    && wget -qO- https://x/i.sh |\\\n    sudo bash\n", true},
		{"RUN curl -fsSL https://x/i.sh -o /tmp/i.sh \\\n    && bash /tmp/i.sh\n", false},
		{"RUN grep x f | sha256sum -c -\n# curl x | sh\n", false},
	} {
		if got := pipesDownloadToShell(c.dockerfile); (len(got) > 0) != c.bad {
			t.Errorf("pipesDownloadToShell(%q) = %q, want bad %v", c.dockerfile, got, c.bad)
		}
	}
	data, err := os.ReadFile(filepath.Join("..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range pipesDownloadToShell(string(data)) {
		t.Errorf("the Dockerfile pipes a download into a shell: %s", run)
	}
}

// downloadToShell matches curl or wget piped into sh, bash or dash.
var downloadToShell = regexp.MustCompile(`\b(curl|wget)\b[^|;&]*\|\s*(sudo\s+)?(ba|da)?sh\b`)

// pipesDownloadToShell returns the RUN instructions of dockerfile, with
// their continuation lines joined, that pipe a download into a shell.
func pipesDownloadToShell(dockerfile string) []string {
	var bad []string
	var inst strings.Builder
	flush := func() {
		s := strings.TrimSpace(inst.String())
		inst.Reset()
		if f := strings.Fields(s); len(f) > 0 && strings.EqualFold(f[0], "RUN") && downloadToShell.MatchString(s) {
			bad = append(bad, s)
		}
	}
	for _, line := range strings.Split(dockerfile, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue // a comment, also between continuation lines
		}
		if body, ok := strings.CutSuffix(strings.TrimRight(line, " \t"), `\`); ok {
			inst.WriteString(body + " ")
			continue
		}
		inst.WriteString(line)
		flush()
	}
	flush()
	return bad
}

// TestNextVersion: .github/next-version.sh numbers releases from VERSION
// and the tags already there, and refuses a VERSION behind the newest tag.
func TestNextVersion(t *testing.T) {
	script, err := filepath.Abs(filepath.Join("..", ".github", "next-version.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, version string
		tags          []string
		want          string // "" means it must fail
	}{
		{"first", "0.1", nil, "v0.1.0"},
		{"next patch", "0.1", []string{"v0.1.0", "v0.1.1"}, "v0.1.2"},
		{"numeric order", "0.1", []string{"v0.1.9", "v0.1.10", "v0.1.2"}, "v0.1.11"},
		{"new minor", "0.2", []string{"v0.1.7"}, "v0.2.0"},
		{"other tags ignored", "0.2", []string{"v0.1.7", "v0.2.3", "v0.2.x", "v0.2.3-rc1", "nightly"}, "v0.2.4"},
		{"newer line refused", "0.2", []string{"v0.20.1"}, ""},
		{"behind newest", "0.1", []string{"v0.2.0"}, ""},
		{"bad VERSION", "1", nil, ""},
		{"bad VERSION 3 parts", "0.1.2", nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			git := func(args ...string) {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = dir
				cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
			}
			git("init", "-q")
			git("commit", "-q", "--allow-empty", "-m", "x")
			for _, tag := range c.tags {
				git("tag", tag)
			}
			if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte(c.version+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", script)
			cmd.Dir = dir
			out, err := cmd.Output()
			got := strings.TrimSpace(string(out))
			if c.want == "" {
				if err == nil {
					t.Errorf("printed %q, want a failure", got)
				}
				return
			}
			if err != nil || got != c.want {
				t.Errorf("got %q, %v; want %q", got, err, c.want)
			}
		})
	}
}
