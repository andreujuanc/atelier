package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
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
	data, err := os.ReadFile(filepath.Join("..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, "curl ") {
			continue
		}
		_, after, ok := strings.Cut(line, "|")
		if !ok {
			continue
		}
		if f := strings.Fields(after); len(f) > 0 && (f[0] == "sh" || f[0] == "bash") {
			t.Errorf("Dockerfile:%d pipes curl into a shell: %s", i+1, strings.TrimSpace(line))
		}
	}
}
