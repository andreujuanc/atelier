package hook

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andreujuanc/atelier/internal/config"
)

func TestCheckBash(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	repos := filepath.Join(home, "repos")
	task := filepath.Join(root, "tasks", "fix")
	for _, d := range []string{filepath.Join(repos, "docs"), filepath.Join(task, "api"), filepath.Join(home, ".ssh")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink in the task dir pointing into $ATL_REPOS.
	if err := os.Symlink(filepath.Join(repos, "docs"), filepath.Join(task, "docs-link")); err != nil {
		t.Fatal(err)
	}
	env := func(k string) string {
		return map[string]string{"HOME": home, "ATL_REPOS": repos, "ATL_TASK_DIR": task}[k]
	}

	allow := []string{
		"",
		"git status",
		"git -C api commit -m 'fix the thing'",
		"cd api && git add . && git commit -m x",
		"git commit -m \"$(cat <<'EOF'\nFix the login\n\nLonger text with $HOME and `ticks`\nEOF\n)\"",
		"git log --oneline | grep fix | head -5",
		"git -C $ATL_REPOS/docs log -3",
		"cd $ATL_REPOS/docs && git diff HEAD~1 && git branch -a && git remote -v && git stash list",
		"git -C ~/repos/docs show HEAD:README.md",
		"git -C ~/repos/docs config --get remote.origin.url",
		"git push origin HEAD",
		"ls -la $ATL_REPOS/docs; cat $ATL_REPOS/docs/README | wc -l",
		"grep -rn foo . 2>/dev/null | sort | uniq -c | sort -rn | head",
		"find . -name '*.go' -type f -newer go.mod",
		"sed -n '10,20p' file.go",
		"sed -n '/func main/,/^}/p' main.go",
		"for f in a b c; do grep -c x $f; done",
		"if [ -f go.mod ]; then echo yes; fi",
		"[[ -n $HOME && -d /tmp ]] && echo ok",
		"cat < input.txt",
		"echo hi >&2",
		"echo hi > /dev/null 2>&1",
		"jq '.a' file.json",
		"atl note fix 'remember this'",
		"docker compose -f $ATL_TASK_DIR/compose.override.yml up -d",
		"docker run --rm -v $ATL_TASK_DIR/api:/src -w /src node:22 npm test",
		"docker run --rm -v $ATL_REPOS/docs:/docs:ro alpine ls /docs",
		"docker run --rm --mount type=bind,src=$ATL_REPOS/docs,dst=/d,readonly alpine true",
		"docker run --rm -v cache:/root/.npm --security-opt label=disable alpine true",
		"docker run --rm -v /var/run/docker.sock:/var/run/docker.sock alpine true",
		"docker network connect shared $ATL_TASK-api",
	}

	block := map[string]string{
		"awk '{print $1}' f":                 "`awk` is not allowed",
		"mkdir -p x":                         "`mkdir` is not allowed",
		"cat >> PICKUPS.md <<'EOF'\nx\nEOF":  "writing files with >>",
		"echo x > out.txt":                   "writing files with >",
		"echo x >&out.txt":                   "writing files with >&",
		"grep x f | tee out":                 "`tee` is not allowed",
		"rm -rf /tmp/x":                      "`rm` is not allowed",
		"sh -c 'ls'":                         "`sh` is not allowed",
		"bash script.sh":                     "`bash` is not allowed",
		"eval ls":                            "`eval` is not allowed",
		"./run.sh":                           "run commands by name",
		"/usr/bin/git status":                "run commands by name",
		"$CMD x":                             "plain text",
		"'git' status":                       "plain text",
		"echo $(id)":                         "command substitution",
		"echo `id`":                          "command substitution",
		"diff <(ls a) <(ls b)":               "process substitution",
		"X=1 git status":                     "setting variables",
		"GIT_SSH_COMMAND='sh -c x' git push": "setting variables",
		"export PATH=/tmp":                   "`export`",
		"f() { ls; }; f":                     "defining functions",
		"echo $((1+2))":                      "shell arithmetic",
		"for x in 'a[$(id)]'; do echo ${!x}; done": "indirect expansion",
		"[[ $_ -eq 1 ]]":                                                 "numeric [[ ]]",
		"test $x -eq 1":                                                  "-eq is not allowed",
		"echo ${x@P}":                                                    "transformations",
		"find . -name x -delete":                                         "-delete",
		"find . -exec rm {} ;":                                           "-exec",
		"sort -o out f":                                                  "-o",
		"uniq in out":                                                    "output file",
		"sed -i s/a/b/ f":                                                "only printing lines",
		"sed -n '1w out' f":                                              "only printing lines",
		"sed 's/a/b/' f":                                                 "only printing lines",
		"rg --pre=sh x":                                                  "--pre",
		"xargs rm":                                                       "`xargs` is not allowed",
		"git -c core.pager=sh log":                                       "-c is not allowed",
		"git config core.hooksPath /tmp":                                 "may only read",
		"git rebase --exec 'make' main":                                  "--exec",
		"git difftool":                                                   "difftool",
		"git fetch --upload-pack=evil origin":                            "--upload-pack",
		"git grep -Ovim x":                                               "-O",
		"git -C $ATL_REPOS/docs commit -m x":                             "would change a repo in $ATL_REPOS",
		"cd ~/repos/docs && git checkout -b x":                           "would change a repo in $ATL_REPOS",
		"cd /tmp; cd $ATL_REPOS/docs; git reset --hard":                  "would change a repo in $ATL_REPOS",
		"git -C docs-link commit -m x":                                   "would change a repo in $ATL_REPOS",
		"git --git-dir=$ATL_REPOS/docs/.git branch -D main":              "would change a repo in $ATL_REPOS",
		"git -C $ATL_REPOS/docs fetch":                                   "would change a repo in $ATL_REPOS",
		"git -C $ATL_REPOS/docs log --output=x":                          "would change a repo in $ATL_REPOS",
		"git -C $ATL_REPOS/docs branch newb":                             "would change a repo in $ATL_REPOS",
		"git clone https://x/y.git $ATL_REPOS/new":                       "would change a repo in $ATL_REPOS",
		"cd - && git commit -m x":                                        "would change a repo in $ATL_REPOS",
		"docker run --privileged alpine":                                 "--privileged",
		"docker run --network host alpine":                               "--network host",
		"docker run --pid=host alpine":                                   "--pid host",
		"docker run --cap-add SYS_ADMIN alpine":                          "--cap-add",
		"docker run -v $ATL_REPOS/docs:/d alpine":                        "add :ro",
		"docker run -v $HOME:/h:ro alpine":                               "exposes ~/.ssh",
		"docker run -v ~/.ssh:/s:ro alpine":                              "~/.ssh",
		"docker run -v /:/host:ro alpine":                                "exposes",
		"docker run --mount type=bind,src=$ATL_REPOS/docs,dst=/d alpine": "add :ro",
		"docker run -v $(pwd):/x alpine":                                 "command substitution",
		"docker cp c:/out $ATL_REPOS/docs/x":                             "copying into $ATL_REPOS",
		"docker build -o $ATL_REPOS/docs .":                              "writing into $ATL_REPOS",
		"docker run --security-opt seccomp=unconfined alpine":            "--security-opt",
		"git status &&":                                                  "cannot parse",
	}
	for _, cmd := range allow {
		if err := checkBash(cmd, task, env, repos); err != nil {
			t.Errorf("blocked %q: %v", cmd, err)
		}
	}
	for cmd, want := range block {
		err := checkBash(cmd, task, env, repos)
		if err == nil {
			t.Errorf("allowed %q", cmd)
		} else if !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %q, want %q", cmd, err, want)
		}
	}
	// The session's own cwd inside $ATL_REPOS counts too.
	if err := checkBash("git commit -m x", filepath.Join(repos, "docs"), env, repos); err == nil {
		t.Error("commit with cwd in $ATL_REPOS allowed")
	}
	if err := checkBash("git log", filepath.Join(repos, "docs"), env, repos); err != nil {
		t.Errorf("log with cwd in $ATL_REPOS: %v", err)
	}
}

func TestBashGuardMain(t *testing.T) {
	p := config.Paths{Repos: t.TempDir()}
	in := func(cmd string) Input {
		ti, _ := json.Marshal(map[string]string{"command": cmd})
		return Input{ToolName: "Bash", Cwd: t.TempDir(), ToolInput: ti}
	}
	off := func(string) string { return "" }
	on := func(k string) string {
		if k == envBashGuard {
			return "1"
		}
		return ""
	}
	var stderr bytes.Buffer
	if code := bashGuardMain(p, &stderr, off, in("rm -rf /")); code != 0 || stderr.Len() != 0 {
		t.Fatalf("guard off: %d %q", code, stderr.String())
	}
	if code := bashGuardMain(p, &stderr, on, in("git status")); code != 0 || stderr.Len() != 0 {
		t.Fatalf("allowed: %d %q", code, stderr.String())
	}
	if code := bashGuardMain(p, &stderr, on, in("awk 1 f")); code != 2 ||
		!strings.Contains(stderr.String(), "atelier: blocked: `awk` is not allowed here") || !strings.Contains(stderr.String(), "Edit/Write") {
		t.Fatalf("blocked: %d %q", code, stderr.String())
	}
	stderr.Reset()
	if code := bashGuardMain(p, &stderr, on, Input{ToolName: "Bash", ToolInput: json.RawMessage(`{}`)}); code != 2 {
		t.Fatalf("no command: %d", code)
	}
	for _, v := range []string{"", "0", "off", "false", "no"} {
		if BashGuardOn(func(string) string { return v }) {
			t.Errorf("%q turns the guard on", v)
		}
	}
}
