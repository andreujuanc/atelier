package cli

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/andreujuanc/atelier/internal/sandbox"
)

func TestPushAndFetch(t *testing.T) {
	root := setup(t)
	t.Setenv("ATL_TASK", "")
	if code, _, errs := atl(t, "new", "ship it", "--repo", "api", "--no-fetch"); code != 0 {
		t.Fatalf("new: %d %s", code, errs)
	}
	wt := filepath.Join(root, "tasks", "ship-it", "api")
	git(t, wt, "commit", "-q", "--allow-empty", "-m", "work")

	code, out, errs := atl(t, "push", "ship-it")
	if code != 0 || out != "api\tpushed\n" {
		t.Fatalf("push: %d %q %s", code, out, errs)
	}
	head, _ := exec.Command("git", "-C", wt, "rev-parse", "HEAD").Output()
	pushed, _ := exec.Command("git", "-C", filepath.Join(root, "origin.git"), "rev-parse", "atl/ship-it").Output()
	if string(pushed) != string(head) {
		t.Errorf("origin atl/ship-it = %q, want %q", pushed, head)
	}

	// The task defaults to $ATL_TASK.
	t.Setenv("ATL_TASK", "ship-it")
	if code, out, errs := atl(t, "fetch"); code != 0 || out != "api\tfetched\n" {
		t.Errorf("fetch: %d %q %s", code, out, errs)
	}
	if code, out, _ := atl(t, "push", "--repo", "nope"); code != 1 || out != "" {
		t.Errorf("push --repo nope: %d %q", code, out)
	}
	t.Setenv("ATL_TASK", "")
	if code, _, errs := atl(t, "push"); code != 2 || !strings.Contains(errs, "which task") {
		t.Errorf("push without a task: %d %s", code, errs)
	}
}

// TestPushInsideTaskContainer: in a task container atl push and fetch only
// ask the task's gate, and never open atl's database.
func TestPushInsideTaskContainer(t *testing.T) {
	root := setup(t)
	dir := t.TempDir()
	// The gate's atl: records its argv and answers like atl push would.
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + filepath.Join(dir, "argv") + "\n" +
		"echo \"api\tpushed\"\n[ \"$1\" = fetch ] && exit 3\nexit 0\n"
	stand := filepath.Join(dir, "atl")
	if err := os.WriteFile(stand, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "atl.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go (&sandbox.Control{Slug: "boxed", TaskID: 4, Atl: stand}).Serve(l)
	t.Setenv(sandbox.InEnv, "boxed-abc123")
	t.Setenv("ATL_TASK", "boxed")
	t.Setenv("ATL_CONTROL_SOCK", sock)

	if code, out, errs := atl(t, "push", "--repo", "api", "--force"); code != 0 || out != "api\tpushed\n" {
		t.Errorf("push: %d %q %s", code, out, errs)
	}
	if code, out, errs := atl(t, "push", "boxed"); code != 0 || out != "api\tpushed\n" {
		t.Errorf("push boxed: %d %q %s", code, out, errs)
	}
	if code, _, errs := atl(t, "fetch"); code != 3 {
		t.Errorf("fetch: exit %d (%s), want the gate's 3", code, errs)
	}
	// Another task, or a repo name the gate refuses.
	if code, _, errs := atl(t, "push", "other"); code != 2 || !strings.Contains(errs, "this task (boxed) only") {
		t.Errorf("push other: %d %s", code, errs)
	}
	if code, out, _ := atl(t, "push", "--repo", "../x"); code != 2 || !strings.Contains(out, "atelier gate: invalid repo name") {
		t.Errorf("push --repo ../x: %d %q", code, out)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	if want := "push --task-id 4 boxed --repo api --force\npush --task-id 4 boxed\nfetch --task-id 4 boxed\n"; string(argv) != want {
		t.Errorf("the gate ran:\n%s\nwant:\n%s", argv, want)
	}
	if _, err := os.Stat(filepath.Join(root, "data")); !os.IsNotExist(err) {
		t.Errorf("atl touched its data dir in a task container: %v", err)
	}

	// No gate: an error, not a hang or a database.
	t.Setenv("ATL_CONTROL_SOCK", filepath.Join(dir, "none.sock"))
	if code, _, errs := atl(t, "push"); code != 1 || !strings.Contains(errs, "gate") {
		t.Errorf("push without a gate: %d %s", code, errs)
	}
}

// TestPushByTaskID: with --task-id (how the gate names its task) push and
// fetch act only on the task with that id and that exact slug: not on a
// prefix, a slug made of digits or another task.
func TestPushByTaskID(t *testing.T) {
	root := setup(t)
	t.Setenv("ATL_TASK", "")
	for _, title := range []string{"ship it", "ship it too", "7"} {
		if code, _, errs := atl(t, "new", title, "--repo", "api", "--no-fetch"); code != 0 {
			t.Fatalf("new %s: %d %s", title, code, errs)
		}
	}
	ids := map[string]string{}
	_, out, _ := atl(t, "ls", "--json")
	var tasks []struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal([]byte(out), &tasks); err != nil {
		t.Fatal(err)
	}
	for _, tk := range tasks {
		ids[tk.Slug] = strconv.FormatInt(tk.ID, 10)
	}
	git(t, filepath.Join(root, "tasks", "ship-it", "api"), "commit", "-q", "--allow-empty", "-m", "work")

	if code, out, errs := atl(t, "push", "--task-id", ids["ship-it"], "ship-it"); code != 0 || out != "api\tpushed\n" {
		t.Fatalf("push by id: %d %q %s", code, out, errs)
	}
	if code, out, errs := atl(t, "fetch", "--task-id", ids["ship-it"], "ship-it"); code != 0 || out != "api\tfetched\n" {
		t.Fatalf("fetch by id: %d %q %s", code, out, errs)
	}
	for _, c := range [][]string{
		{ids["ship-it"], "ship"},         // a prefix of its slug
		{ids["ship-it"], "ship-it-too"},  // another task's slug
		{ids["ship-it-too"], "ship-it"},  // another task's id
		{ids["ship-it"], ids["ship-it"]}, // the id as the slug
		{"999", "ship-it"},               // no such id
		{ids["7"], ids["ship-it"]},       // a slug that is the other's id
	} {
		if code, out, errs := atl(t, "push", "--task-id", c[0], c[1]); code != 1 || out != "" || !strings.Contains(errs, "any more") {
			t.Errorf("push --task-id %s %s: %d %q %s", c[0], c[1], code, out, errs)
		}
	}
	if _, ok := ids["7"]; !ok {
		t.Fatalf("no task with slug 7: %v", ids)
	}
}
