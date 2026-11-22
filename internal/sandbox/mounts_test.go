package sandbox

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/andreujuanc/atelier/internal/config"
)

// mountsEnv makes a box in a temp dir: a home with files to list, a
// workspace (or, without one, atelier's XDG dirs) and the other places the
// sandbox keeps from tasks, all of them existing. It returns the paths and
// the home.
func mountsEnv(t *testing.T, workspace bool) (config.Paths, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	var p config.Paths
	if workspace {
		ws := filepath.Join(root, "ws")
		// $ATL_TASKS set outside the workspace, so each rule shows on its own.
		p = config.Paths{Workspace: ws, Repos: filepath.Join(root, "repos"), Tasks: filepath.Join(root, "tasks"),
			DB: filepath.Join(ws, "data", "atelier.db"), StateDir: filepath.Join(ws, "state"), ConfigDir: filepath.Join(ws, "config")}
	} else {
		p = config.Paths{Repos: filepath.Join(home, "repos"), Tasks: filepath.Join(home, "tasks"),
			DB: filepath.Join(home, ".local", "share", "atelier", "atelier.db"), StateDir: filepath.Join(home, ".local", "state", "atelier"),
			ConfigDir: filepath.Join(home, ".config", "atelier")}
	}
	for _, d := range []string{home, p.Repos, p.Tasks, filepath.Dir(p.DB), p.StateDir, p.ConfigDir, p.RunRoot(),
		filepath.Join(home, ".claude"), filepath.Join(home, "certs"), filepath.Join(root, "docker"), filepath.Join(root, "agent")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(home, ".npmrc-corp"), filepath.Join(home, "certs", "ca.pem"),
		filepath.Join(p.RunRoot(), "claude-token.json"), filepath.Join(root, "docker", "docker.sock"), filepath.Join(root, "agent", "ssh.sock")} {
		if err := os.WriteFile(f, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(root, "docker", "docker.sock"))
	t.Setenv("SSH_AUTH_SOCK", filepath.Join(root, "agent", "ssh.sock"))
	t.Setenv(MountsVar, "")
	return p, home
}

func TestROMountsParse(t *testing.T) {
	p, home := mountsEnv(t, true)
	for _, empty := range []string{"", " ", ":", " : :: "} {
		t.Setenv(MountsVar, empty)
		if got, err := ROMounts(p); got != nil || err != nil {
			t.Errorf("%q: %q, %v; want nothing", empty, got, err)
		}
	}
	os.Unsetenv(MountsVar)
	if got, err := ROMounts(p); got != nil || err != nil {
		t.Errorf("unset: %q, %v", got, err)
	}
	// "~/" is the home dir, symlinks are resolved once, duplicates go,
	// order stays, empty entries and surrounding blanks are ignored.
	link := filepath.Join(home, "ca-link")
	if err := os.Symlink(filepath.Join(home, "certs", "ca.pem"), link); err != nil {
		t.Fatal(err)
	}
	t.Setenv(MountsVar, "~/.npmrc-corp::"+home+"/certs : "+link+":"+home+"/.npmrc-corp:")
	got, err := ROMounts(p)
	want := []string{filepath.Join(home, ".npmrc-corp"), filepath.Join(home, "certs"), filepath.Join(home, "certs", "ca.pem")}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("ROMounts = %q, %v; want %q", got, err, want)
	}
	for _, m := range got {
		if err := CheckROMount(p, m); err != nil {
			t.Errorf("CheckROMount(%s): %v", m, err)
		}
	}
	if err := CheckROMount(p, link); err == nil || !strings.Contains(err.Error(), "resolves to") {
		t.Errorf("CheckROMount of a symlink: %v", err)
	}
	// A path inside a listed dir is listed too: not a conflict.
	t.Setenv(MountsVar, home+"/certs:"+home+"/certs/ca.pem")
	if got, err := ROMounts(p); err != nil || len(got) != 2 {
		t.Errorf("dir and a file in it: %q, %v", got, err)
	}
}

// TestROMountsRules: each rule refuses with one line that names the
// variable, the path and the rule it broke.
func TestROMountsRules(t *testing.T) {
	for _, workspace := range []bool{true, false} {
		p, home := mountsEnv(t, workspace)
		root := filepath.Dir(home)
		if err := os.Symlink(p.Tasks, filepath.Join(home, "tasks-link")); err != nil && !os.IsExist(err) {
			t.Fatal(err)
		}
		sockDir, err := os.MkdirTemp("", "mnt") // short: a socket path
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(sockDir) })
		l, err := net.Listen("unix", filepath.Join(sockDir, "s.sock"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		comma := filepath.Join(home, "a,b")
		if err := os.WriteFile(comma, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		cases := map[string]string{
			"rel/npmrc":                       "is not an absolute path",
			home + "/missing":                 "does not exist here (it must exist here and at the same path on the host)",
			"/":                               "is the root directory",
			p.Tasks:                           "is the tasks dir",
			p.Repos:                           "is the repos dir ($ATL_REPOS)",
			p.Repos + "/..":                   "contains",
			filepath.Join(home, ".claude"):    "is your Claude Code config dir",
			home:                              "contains",
			filepath.Join(home, "tasks-link"): "(" + p.Tasks + ") is the tasks dir",
			filepath.Join(root, "docker"):     "contains the Docker socket",
			filepath.Join(root, "agent"):      "contains the SSH agent's socket ($SSH_AUTH_SOCK)",
			"/proc/self":                      "lies inside a kernel file system",
			"/dev":                            "is a kernel file system",
			filepath.Join(sockDir, "s.sock"):  "is neither a file nor a directory",
			comma:                             "has a comma",
		}
		if workspace {
			cases[p.Workspace] = "is the workspace dir ($ATL_WORKSPACE)"
			cases[filepath.Join(p.Workspace, "config")] = "lies inside the workspace dir"
			cases[root] = "contains the workspace dir"
			cases[filepath.Join(p.RunRoot(), "claude-token.json")] = "lies inside the workspace dir"
		} else {
			cases[filepath.Join(p.RunRoot(), "claude-token.json")] = "lies inside atelier's run dir"
			cases[filepath.Dir(p.DB)] = "is atelier's data dir"
			cases[filepath.Join(p.StateDir, "x")+"/.."] = "is atelier's state dir"
			cases[filepath.Dir(p.ConfigDir)] = "contains atelier's config dir"
		}
		for path, want := range cases {
			if path == p.StateDir+"/x/.." {
				if err := os.MkdirAll(filepath.Join(p.StateDir, "x"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv(MountsVar, home+"/.npmrc-corp:"+path)
			got, err := ROMounts(p)
			if err == nil || got != nil || !strings.HasPrefix(err.Error(), "$ATL_SANDBOX_MOUNTS: ") || !strings.Contains(err.Error(), want) ||
				strings.Contains(err.Error(), "\n") {
				t.Errorf("workspace %v, %s: %q, %v; want an error with %q", workspace, path, got, err, want)
			}
		}
		// CheckROMounts reports each, and keeps the good ones.
		t.Setenv(MountsVar, home+"/.npmrc-corp:/:"+home+"/certs:"+p.Tasks)
		paths, errs := CheckROMounts(p)
		if !slices.Equal(paths, []string{home + "/.npmrc-corp", home + "/certs"}) || len(errs) != 2 {
			t.Errorf("CheckROMounts = %q, %v", paths, errs)
		}
	}
}

// TestROMountsGuardsResolved: a place the sandbox keeps from tasks is
// recognised by its real path too: a workspace given through a symlink.
func TestROMountsGuardsResolved(t *testing.T) {
	p, home := mountsEnv(t, true)
	link := filepath.Join(home, "ws-link")
	if err := os.Symlink(p.Workspace, link); err != nil {
		t.Fatal(err)
	}
	p.Workspace = link
	t.Setenv(MountsVar, filepath.Join(filepath.Dir(home), "ws", "config"))
	if _, err := ROMounts(p); err == nil || !strings.Contains(err.Error(), "lies inside the workspace dir") {
		t.Errorf("inside the symlinked workspace's real path: %v", err)
	}
}
