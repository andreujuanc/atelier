package sandbox

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestParseActivity(t *testing.T) {
	out := strings.Join([]string{
		"1\t0\t\t/sbin/docker-init -- sleep infinity",
		"7\t1\t\tsleep infinity",
		"8\t0\t3\tclaude",                                      // a session's Claude
		"9\t8\t3\tnode /x/mcp.js",                              // its helper: not counted
		"10\t9\t3\tbash -c helper",                             // what a helper started: not counted
		"11\t8\t3\t/bin/bash -c source snap.sh && eval 'make'", // a Bash tool command
		"12\t11\t3\tmake",                                      // ... shown by its child
		"13\t12\t3\tcc x.c",                                    // not listed apart
		"14\t8\t3\t-zsh",                                       // a login shell
		"20\t1\t3\tnode server.js",                             // left running by a command
		"30\t0\t\tbash",                                        // a docker exec shell
		"31\t0\t\tapt-get install -y jq",                       // as root: no environment
		"40\t0\t\tsh -c script atl-activity",                   // this probe
		"41\t40\t\ttr \\0 \\n",                                 // ... and its children
		"50\t0\t\tsh -c script atl-end-session end 3",          // atl's clean-up
		"60\t0\t4\tclaude",                                     // another session
		"61\t60\t4\tsh -c sleep 100",
		"62\t60\t4\t/bin/sh -c '" + InBin + "' hook PostToolUse", // atl's hook, as gen.HookCommand writes it
		"63\t60\t4\t/bin/sh -c " + InBin + " statusline",
		"garbage",
		"",
	}, "\n")
	want := []Proc{
		{PID: 11, Session: 3, Command: "make"},
		{PID: 14, Session: 3, Command: "-zsh"},
		{PID: 20, Session: 3, Command: "node server.js"},
		{PID: 30, Command: "bash"},
		{PID: 31, Command: "apt-get install -y jq"},
		{PID: 61, Session: 4, Command: "sh -c sleep 100"},
	}
	got, roots := ParseActivity(out)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	if !reflect.DeepEqual(roots, map[int64]bool{3: true, 4: true}) {
		t.Fatalf("roots %v", roots)
	}
	if got, roots := ParseActivity(strings.Join(strings.Split(out, "\n")[:4], "\n")); len(got) != 0 || !roots[3] {
		t.Fatalf("idle task: %+v %v", got, roots)
	}
}

// The probe runs here with plain sh: it lists itself (left out by
// ParseActivity) and this test's process.
func TestActivityScript(t *testing.T) {
	args := ActivityArgs("c")
	out, err := exec.Command(args[2], args[3:]...).Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "\t") {
		t.Fatalf("no process listed:\n%s", out)
	}
	procs, _ := ParseActivity(string(out))
	for _, p := range procs {
		if strings.Contains(p.Command, activityMarker) {
			t.Fatalf("the probe lists itself: %+v", p)
		}
	}
}

// A command line with a newline cannot forge a line of the probe's output.
func TestActivityScriptNewline(t *testing.T) {
	forger := exec.Command("sh", "-c", "sleep 30", "x\n99999\t0\t\tforged")
	if err := forger.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = forger.Process.Kill(); _ = forger.Wait() }()
	args := ActivityArgs("c")
	out, err := exec.Command(args[2], args[3:]...).Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "\n99999\t") {
		t.Fatalf("forged line in the output:\n%s", out)
	}
	if !strings.Contains(string(out), "x 99999") {
		t.Fatalf("the forger is not listed on one line:\n%s", out)
	}
}
