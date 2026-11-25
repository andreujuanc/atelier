package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// scriptRun is a script run of these tests: like the sessions of the
// session scripts' tests, it carries this process's PID, so that another
// run of these tests at the same time does not share it.
var scriptRun = "run" + strconv.Itoa(os.Getpid())

// procsWith returns the live processes whose environment has kv.
func procsWith(kv string) []int {
	var pids []int
	dirs, _ := filepath.Glob("/proc/[0-9]*")
	for _, d := range dirs {
		b, err := os.ReadFile(filepath.Join(d, "environ"))
		if err != nil || !slices.Contains(strings.Split(string(b), "\x00"), kv) {
			continue
		}
		if st, err := os.ReadFile(filepath.Join(d, "stat")); err == nil && !strings.Contains(string(st), ") Z ") {
			pid, _ := strconv.Atoi(filepath.Base(d))
			pids = append(pids, pid)
		}
	}
	return pids
}

// marked counts the live processes whose environment has ATL_SCRIPT=run.
func marked(run string) int { return len(procsWith(ScriptVar + "=" + run)) }

// TestScriptArgs: a project script runs in the task container as the
// given user, in the given dir, with exactly the given variables (ATL_SCRIPT
// is the run's, whatever they say), by sh -c; with sh it runs as docker
// exec would run it there: marked with the run, the run dir's atl first on
// PATH, with the script's exit status and all of its output. What it leaves
// running ends when it exits; what it started without the mark (out of
// reach) holds the output open for 5 seconds at most. Without a FIFO (no
// writable temp dir) it runs too.
func TestScriptArgs(t *testing.T) {
	escaped := "ATL_TEST_ESCAPED=" + scriptRun
	t.Cleanup(func() {
		for _, pid := range procsWith(escaped) {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	script := `echo "run=$ATL_SCRIPT foo=$FOO"; echo "path=$PATH"; sleep 300 & ` +
		`[ -z "$ESCAPE" ] || env -u ` + ScriptVar + ` ` + escaped + ` sleep 300 & echo started; sleep 0.2; echo end; exit 3`
	args := ScriptArgs(oldID, "1000:1000", "/tasks/x/api", map[string]string{"FOO": "a b", "BAR": "1", ScriptVar: "forged"}, scriptRun, script)
	head := []string{"exec", "-u", "1000:1000", "-w", "/tasks/x/api", "-e", "BAR=1", "-e", "FOO=a b", oldID, "sh", "-c"}
	if !slices.Equal(args[:len(head)], head) {
		t.Fatalf("args = %q", args)
	}
	rest := args[len(head):] // wrapper, $0, then its arguments
	if len(rest) != 5 || rest[1] != "atl-script" || rest[3] != scriptRun || rest[4] != script {
		t.Fatalf("command = %q", rest)
	}
	for _, c := range []struct {
		name string
		env  []string
	}{
		{"fifo", nil},
		{"fifo, escaped", []string{"ESCAPE=1"}},
		{"no fifo", []string{"TMPDIR=/nonexistent"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cmd := exec.Command("sh", append([]string{"-c"}, rest...)...)
			cmd.Env = append(append(os.Environ(), "FOO=a b"), c.env...)
			start := time.Now()
			out, err := cmd.Output() // returns once nothing holds the output open
			if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 {
				t.Fatalf("exit: %v; output:\n%s", err, out)
			}
			if d := time.Since(start); d > 20*time.Second {
				t.Errorf("took %v: a background process held the output", d)
			}
			for _, want := range []string{"run=" + scriptRun + " foo=a b\n", "path=" + RunMount + "/bin:", "started\n", "end\n"} {
				if !strings.Contains(string(out), want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			until(func() bool { return marked(scriptRun) == 0 })
			if n := marked(scriptRun); n != 0 {
				t.Errorf("%d processes of the run left", n)
			}
		})
	}
}

// TestKillScriptArgs: the clean-up of an interrupted script ends every
// process of its run, children included, and nothing of another run.
func TestKillScriptArgs(t *testing.T) {
	args := KillScriptArgs(oldID, scriptRun)
	if !slices.Equal(args[:4], []string{"exec", oldID, "sh", "-c"}) || args[5] != scriptKiller ||
		!slices.Contains(args, scriptRun) || args[len(args)-1] != ScriptVar {
		t.Fatalf("args = %q", args)
	}
	other := scriptRun + "1" // starts with this run's value
	start := func(run string) *exec.Cmd {
		cmd := exec.Command("sh", "-c", "sleep 300 & sleep 300; wait")
		cmd.Env = append(os.Environ(), ScriptVar+"="+run)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		return cmd
	}
	mine, theirs := start(scriptRun), start(other)
	until(func() bool { return marked(scriptRun) >= 3 && marked(other) >= 3 })
	if out, err := exec.Command("sh", append([]string{"-c", args[4]}, args[5:]...)...).CombinedOutput(); err != nil {
		t.Fatalf("kill script: %v: %s", err, out)
	}
	if err := mine.Wait(); err == nil {
		t.Error("the run's process exited normally, want killed")
	}
	until(func() bool { return marked(scriptRun) == 0 })
	if n := marked(scriptRun); n != 0 {
		t.Errorf("%d processes of the run left", n)
	}
	if n := marked(other); n < 3 || theirs.ProcessState != nil {
		t.Errorf("the other run lost processes: %d left", n)
	}
}
