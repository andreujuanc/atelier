package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
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
