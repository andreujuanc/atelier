package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestFromEnv(t *testing.T) {
	t.Setenv(InEnv, "")
	t.Setenv("ATL_SANDBOX_IMAGE", "")
	for _, off := range []string{"", "off", "0", "none", " OFF "} {
		t.Setenv("ATL_SANDBOX", off)
		if c, err := FromEnv(); c != nil || err != nil {
			t.Errorf("ATL_SANDBOX=%q: %v, %v; want off", off, c, err)
		}
	}
	t.Setenv("ATL_SANDBOX", "docker")
	if c, err := FromEnv(); err != nil || c == nil || c.Image != DefaultImage {
		t.Errorf("docker: %+v, %v", c, err)
	}
	t.Setenv("ATL_SANDBOX_IMAGE", "dev:1")
	if c, _ := FromEnv(); c == nil || c.Image != "dev:1" {
		t.Errorf("image: %+v", c)
	}
	// A typo must not silently run tasks unsandboxed.
	t.Setenv("ATL_SANDBOX", "dokcer")
	if _, err := FromEnv(); err == nil {
		t.Error("unknown value accepted")
	}
	// In a task container there is nothing to sandbox.
	t.Setenv("ATL_SANDBOX", "docker")
	t.Setenv(InEnv, "x-123456")
	if c, err := FromEnv(); c != nil || err != nil {
		t.Errorf("inside: %v, %v", c, err)
	}
}

// TestDefaultImage: the default task image is the Dockerfile's task target,
// not the box image, and its build command builds that target for the user.
func TestDefaultImage(t *testing.T) {
	if DefaultImage != "atelier-task" {
		t.Errorf("DefaultImage = %q", DefaultImage)
	}
	for _, want := range []string{"docker build --target task -t atelier-task ", "--build-arg USERNAME=$(whoami)",
		"--build-arg UID=$(id -u)", "--build-arg GID=$(id -g)"} {
		if !strings.Contains(BuildDefaultImage, want) {
			t.Errorf("BuildDefaultImage lacks %q: %s", want, BuildDefaultImage)
		}
	}
	if !strings.HasSuffix(BuildDefaultImage, " .") || strings.Contains(BuildDefaultImage, "\n") {
		t.Errorf("BuildDefaultImage: %q", BuildDefaultImage)
	}
	run := func(_ context.Context, args ...string) (string, error) {
		if args[len(args)-1] == "gone" {
			return "", errors.New("docker image: Error response from daemon: No such image: gone:latest")
		}
		if args[len(args)-1] == "broken" {
			return "", errors.New("docker image: Cannot connect to the Docker daemon")
		}
		return "sha256:x", nil
	}
	for image, want := range map[string]bool{"here": false, "gone": true} {
		if missing, err := ImageMissing(context.Background(), run, image); missing != want || err != nil {
			t.Errorf("ImageMissing(%s) = %v, %v", image, missing, err)
		}
	}
	if missing, err := ImageMissing(context.Background(), run, "broken"); missing || err == nil {
		t.Errorf("ImageMissing without a daemon = %v, %v", missing, err)
	}
}

func TestIDDiffersPerWorkspace(t *testing.T) {
	a, b := ID("fix-login", "/ws/a/tasks/fix-login"), ID("fix-login", "/ws/b/tasks/fix-login")
	if a == b || !strings.HasPrefix(a, "fix-login-") || ContainerName(a) != "atl-"+a {
		t.Errorf("ids %q %q", a, b)
	}
}

func testSpec() Spec {
	return Spec{ID: "t-abc123", Image: "img", User: "1000:1000", Workdir: "/tasks/t",
		Mounts: []Mount{{Src: "/tasks/t", Dst: "/tasks/t"}, {Src: "/run/x", Dst: RunMount, RO: true}},
		Env:    map[string]string{"B": "2", "A": "1"}}
}

func TestSpecRunArgs(t *testing.T) {
	s := testSpec()
	want := []string{"run", "-d", "--init", "--name", "atl-t-abc123", "--label", "atelier.agent=t-abc123",
		"--label", "atelier.spec=" + s.Hash(), "--user", "1000:1000", "--workdir", "/tasks/t",
		"--mount", "type=bind,src=/tasks/t,dst=/tasks/t", "--mount", "type=bind,src=/run/x,dst=/run/atl,readonly",
		"--env", "A=1", "--env", "B=2", "img", "sleep", "infinity"}
	if got := s.RunArgs(); !slices.Equal(got, want) {
		t.Errorf("RunArgs:\n got %q\nwant %q", got, want)
	}
	other := testSpec()
	other.Mounts = append(other.Mounts, Mount{Src: "/repos/docs", Dst: "/repos/docs", RO: true})
	if other.Hash() == s.Hash() {
		t.Error("the hash does not change with the mounts")
	}
}

func TestExecArgv(t *testing.T) {
	got := ExecArgv("t-abc123", "/tasks/t", []string{"ATL_TASK", "TERM"}, []string{"claude", "--resume", "x"})
	want := []string{"docker", "exec", "-it", "--detach-keys", "ctrl-^,ctrl-^,ctrl-^", "-w", "/tasks/t", "-e", "ATL_TASK", "-e", "COLORFGBG", "-e", "COLORTERM",
		"-e", "NO_COLOR", "-e", "TERM", "atl-t-abc123", "sh", "-c", `PATH=/run/atl/bin:$PATH exec "$@"`, "atl-exec",
		"claude", "--resume", "x"}
	if !slices.Equal(got, want) {
		t.Errorf("ExecArgv:\n got %q\nwant %q", got, want)
	}
}

// script is a Runner answering from a list of (prefix → output, error).
type script struct {
	calls   []string
	answers map[string]struct {
		out string
		err error
	}
}

func (s *script) run(_ context.Context, args ...string) (string, error) {
	call := strings.Join(args, " ")
	s.calls = append(s.calls, call)
	for prefix, a := range s.answers {
		if strings.HasPrefix(call, prefix) {
			return a.out, a.err
		}
	}
	return "", nil
}

func (s *script) answer(prefix, out string, err error) {
	if s.answers == nil {
		s.answers = map[string]struct {
			out string
			err error
		}{}
	}
	s.answers[prefix] = struct {
		out string
		err error
	}{out, err}
}

func (s *script) verbs() string {
	var v []string
	for _, c := range s.calls {
		first, _, _ := strings.Cut(c, " ")
		v = append(v, first)
	}
	return strings.Join(v, " ")
}

func TestEnsure(t *testing.T) {
	spec := testSpec()
	ctx := context.Background()
	busy := func() bool { return true }
	idle := func() bool { return false }
	missing := errors.New("docker inspect: Error response from daemon: No such container: atl-t-abc123")

	for _, tc := range []struct {
		name      string
		inspect   string
		err       error
		busy      func() bool
		want      string
		wantStale bool
	}{
		{"missing: create", "", missing, idle, "inspect run", false},
		{"running, same spec: nothing", "true " + spec.Hash(), nil, busy, "inspect", false},
		{"stopped, same spec: start", "false " + spec.Hash(), nil, idle, "inspect start", false},
		{"other spec, idle: replace", "true old", nil, idle, "inspect rm run", false},
		{"other spec, sessions running: keep", "true old", nil, busy, "inspect", true},
		{"other spec but stopped: replace", "false old", nil, busy, "inspect rm run", false},
	} {
		s := &script{}
		s.answer("inspect", tc.inspect, tc.err)
		// fresh runs right before each docker run, after any docker rm.
		fresh := func() error {
			s.calls = append(s.calls, "fresh")
			return nil
		}
		stale, err := Ensure(ctx, s.run, spec, tc.busy, fresh)
		want := strings.ReplaceAll(tc.want, "run", "fresh run")
		if err != nil || stale != tc.wantStale || s.verbs() != want {
			t.Errorf("%s: calls %q stale %v err %v; want %q stale %v", tc.name, s.verbs(), stale, err, want, tc.wantStale)
		}
	}

	// Two atl at once: the loser of the name race uses the winner's container.
	s := &script{}
	s.answer("inspect", "", missing)
	s.answer("run", "", errors.New(`docker run: Conflict. The container name "/atl-t-abc123" is already in use`))
	if _, err := Ensure(ctx, s.run, spec, idle, nil); err != nil || s.verbs() != "inspect run start" {
		t.Errorf("name race: %q, %v", s.verbs(), err)
	}
	// Any other failure is reported.
	s = &script{}
	s.answer("inspect", "", missing)
	s.answer("run", "", errors.New("docker run: pull access denied for img"))
	if _, err := Ensure(ctx, s.run, spec, idle, nil); err == nil {
		t.Error("a failed docker run was not reported")
	}
	// A failed fresh stops the creation.
	s = &script{}
	s.answer("inspect", "", missing)
	if _, err := Ensure(ctx, s.run, spec, idle, func() error { return errors.New("no") }); err == nil || s.verbs() != "inspect" {
		t.Errorf("failed fresh: %q, %v", s.verbs(), err)
	}
}

func TestCleanup(t *testing.T) {
	s := &script{}
	s.answer("ps", "c1\nc2", nil)
	s.answer("network ls", "", nil)
	s.answer("volume ls", "v1", nil)
	s.answer("rm -f atl-", "", errors.New("docker rm: Error: No such container: atl-t-abc123"))
	if err := Cleanup(context.Background(), s.run, "t-abc123"); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	want := []string{
		"rm -f atl-t-abc123",
		"ps -aq --filter label=atelier.task=t-abc123",
		"rm -f -v c1 c2",
		"network ls -q --filter label=atelier.task=t-abc123",
		"volume ls -q --filter label=atelier.task=t-abc123",
		"volume rm -f v1",
	}
	if !slices.Equal(s.calls, want) {
		t.Errorf("calls:\n got %q\nwant %q", s.calls, want)
	}
	// A failure does not stop the rest, and is reported.
	s = &script{}
	s.answer("ps", "", errors.New("docker ps: daemon down"))
	s.answer("volume ls", "v1", nil)
	if err := Cleanup(context.Background(), s.run, "t-abc123"); err == nil || !slices.Contains(s.calls, "volume rm -f v1") {
		t.Errorf("after a failure: err %v, calls %q", err, s.calls)
	}
}

func TestInstallBin(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "atl")
	if err := os.WriteFile(src, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(dir, "run")
	dst := filepath.Join(run, "bin", "atl")
	if err := InstallBin(src, run); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dst)
	if err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("installed copy: %v %v", fi, err)
	}
	// Unchanged: the copy is left alone (same inode).
	if err := InstallBin(src, run); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.Stat(dst); !os.SameFile(fi, again) {
		t.Error("an unchanged binary was copied again")
	}
	// Changed: replaced.
	if err := os.WriteFile(src, []byte("version two"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := InstallBin(src, run); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "version two" {
		t.Errorf("copy = %q", b)
	}
}
