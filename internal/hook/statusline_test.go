package hook

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/palette"
)

// statusRun runs `atl statusline` in process with stdin and env.
func statusRun(t *testing.T, stdin string, env map[string]string) (stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	if code := runStatusLine(strings.NewReader(stdin), &out, &errb, envOf(env)); code != 0 {
		t.Fatalf("exit %d", code)
	}
	return out.String(), errb.String()
}

func dot(st model.SessionState) string { return palette.StateLook(st).ANSI("●", true) }

func TestStatusLineResolution(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.session(t, model.StateWaiting, "c-fix")
	other, err := f.st.GetTaskBySlug(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []model.SessionState{model.StateIdle, model.StateRunning, model.StateEnded} {
		if err := f.st.CreateSession(ctx, &model.Session{TaskID: other.ID, State: st, LastEventAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	otherDir := f.p.TaskDir("other")
	fixAPI := f.p.WorktreePath("fix", "api")

	for _, c := range []struct {
		name, stdin string
		env         map[string]string
		want        string
	}{
		{"session id: that session's state", `{"session_id":"c-fix","cwd":"/"}`, nil, dot(model.StateWaiting) + " Fix\n"},
		{"session id beats ATL_TASK and cwd", `{"session_id":"c-fix","cwd":"` + otherDir + `"}`,
			map[string]string{"ATL_TASK": "other"}, dot(model.StateWaiting) + " Fix\n"},
		{"ATL_TASK: the task's sessions' state", `{"session_id":"c-unknown","cwd":"/"}`,
			map[string]string{"ATL_TASK": "other"}, dot(model.StateRunning) + " Other\n"},
		{"ATL_TASK without session id", `{}`, map[string]string{"ATL_TASK": "fix"}, dot(model.StateWaiting) + " Fix\n"},
		{"unknown ATL_TASK falls back to cwd", `{"cwd":"` + otherDir + `"}`,
			map[string]string{"ATL_TASK": "gone"}, dot(model.StateRunning) + " Other\n"},
		{"cwd inside a worktree", `{"session_id":"c-new","cwd":"` + fixAPI + `"}`, nil, dot(model.StateWaiting) + " Fix\n"},
		{"workspace.current_dir when cwd is missing", `{"workspace":{"current_dir":"` + fixAPI + `"}}`, nil, dot(model.StateWaiting) + " Fix\n"},
		{"workspace.project_dir when cwd is outside", `{"cwd":"/tmp","workspace":{"current_dir":"/tmp","project_dir":"` + otherDir + `"}}`,
			nil, dot(model.StateRunning) + " Other\n"},
		{"fields of unexpected types are ignored", `{"session_id":5,"cwd":["x"],"workspace":"x","model":{"id":1}}`,
			map[string]string{"ATL_TASK": "other"}, dot(model.StateRunning) + " Other\n"},
		{"NO_COLOR: plain dot", `{"session_id":"c-fix"}`, map[string]string{"NO_COLOR": "1"}, "● Fix\n"},
		{"light background", `{"session_id":"c-fix"}`, map[string]string{"COLORFGBG": "0;15"},
			palette.StateLook(model.StateWaiting).ANSI("●", false) + " Fix\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, errOut := statusRun(t, c.stdin, c.env)
			if out != c.want || errOut != "" {
				t.Errorf("stdout %q stderr %q, want %q", out, errOut, c.want)
			}
		})
	}
	if log := readLog(t, f.p); log != "" {
		t.Errorf("unexpected log: %s", log)
	}

	// A task with no live or stopped session: a plain dot.
	t.Run("no sessions", func(t *testing.T) {
		if err := f.st.CreateTask(ctx, &model.Task{Slug: "quiet", Title: "Quiet", Status: model.StatusBacklog, Branch: "atl/quiet", PortOffset: 200}); err != nil {
			t.Fatal(err)
		}
		if out, _ := statusRun(t, `{}`, map[string]string{"ATL_TASK": "quiet"}); out != "● Quiet\n" {
			t.Errorf("stdout %q", out)
		}
	})
}

func TestStatusLineNothing(t *testing.T) {
	f := setup(t)
	f.session(t, model.StateIdle, "c-fix")
	big := `{"session_id":"c-fix","x":"` + strings.Repeat("a", maxStatusInput) + `"}`
	for _, c := range []struct {
		name, stdin string
		env         map[string]string
		logged      bool
	}{
		{"unknown session, cwd outside", `{"session_id":"nope","cwd":"/"}`, nil, false},
		{"unknown slug under the tasks dir", `{"cwd":"` + f.p.TaskDir("gone") + `"}`, map[string]string{"ATL_TASK": "gone2"}, false},
		{"the tasks dir itself", `{"cwd":"` + f.p.Tasks + `"}`, nil, false},
		{"relative cwd", `{"cwd":"tasks/fix"}`, nil, false},
		{"empty stdin", ``, map[string]string{"ATL_TASK": "fix"}, true},
		{"bad JSON", `{"session_id":"c-fix"`, map[string]string{"ATL_TASK": "fix"}, true},
		{"array", `[{"session_id":"c-fix"}]`, nil, true},
		{"null", `null`, map[string]string{"ATL_TASK": "fix"}, true},
		{"string", `"c-fix"`, nil, true},
		{"oversized", big, nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			os.Remove(f.p.HookLog())
			out, errOut := statusRun(t, c.stdin, c.env)
			if out != "" || errOut != "" {
				t.Errorf("stdout %q stderr %q, want nothing", out, errOut)
			}
			if log := readLog(t, f.p); (log != "") != c.logged || c.logged && !strings.Contains(log, "event=statusline") {
				t.Errorf("log %q (want logged=%v)", log, c.logged)
			}
		})
	}

	t.Run("no database: nothing, and none created", func(t *testing.T) {
		data := filepath.Join(f.root, "nodata")
		t.Setenv("XDG_DATA_HOME", data)
		out, errOut := statusRun(t, `{"session_id":"c-fix"}`, map[string]string{"ATL_TASK": "fix"})
		if out != "" || errOut != "" {
			t.Errorf("stdout %q stderr %q", out, errOut)
		}
		if _, err := os.Stat(data); !os.IsNotExist(err) {
			t.Errorf("data dir created: %v", err)
		}
	})
}

func TestStatusLineText(t *testing.T) {
	for _, c := range []struct{ name, title, want string }{
		{"plain", "Fix login", "● Fix login"},
		{"control and format characters", "Fix\nthe\tlogin\x1b[31m page‮\r\x00", "● Fix the login [31m page"},
		{"invalid UTF-8", "a\xffb", "● a b"},
		{"blank title shows the slug", " \n\t", "● slug"},
		{"long title cut to 80 columns", strings.Repeat("x", 200), "● " + strings.Repeat("x", 77) + "…"},
		{"wide characters", strings.Repeat("界", 100), "● " + strings.Repeat("界", 38) + "…"},
		{"exactly 78 columns kept", strings.Repeat("y", 78), "● " + strings.Repeat("y", 78)},
	} {
		got := statusLineText(&model.Task{Slug: "slug", Title: c.title}, model.StateIdle, false, true)
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		if w := ansi.StringWidth(got); w > statusLineWidth {
			t.Errorf("%s: width %d", c.name, w)
		}
	}
	// Colour only wraps the dot.
	got := statusLineText(&model.Task{Slug: "s", Title: "T"}, model.StateRunning, true, true)
	if got != "\x1b[38;2;158;206;106m●\x1b[0m T" {
		t.Errorf("coloured: %q", got)
	}
}

// TestStatusLineLatency measures `atl statusline` as a separate process,
// like TestSessionStartContextLatency does for the hook.
func TestStatusLineLatency(t *testing.T) {
	if raceEnabled {
		t.Skip("race-instrumented binaries start too slowly to measure latency")
	}
	f := setup(t)
	f.session(t, model.StateRunning, "c-lat")
	f.st.Close()
	run := func(stdin string) (string, time.Duration) {
		t.Helper()
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), "ATL_HOOK_TEST_MAIN=statusline", "NO_COLOR=1")
		cmd.Stdin = strings.NewReader(stdin)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		start := time.Now()
		if err := cmd.Run(); err != nil || errb.Len() != 0 {
			t.Fatalf("%v: %s", err, errb.String())
		}
		return out.String(), time.Since(start)
	}
	for _, c := range []struct{ name, stdin string }{
		{"session id", `{"session_id":"c-lat","cwd":"/"}`},
		{"cwd", `{"session_id":"c-other","cwd":"` + f.p.WorktreePath("fix", "api") + `"}`},
	} {
		run(c.stdin) // warm the page cache
		var walls []time.Duration
		for i := 0; i < 7; i++ {
			out, wall := run(c.stdin)
			if out != "● Fix\n" {
				t.Fatalf("%s: stdout %q", c.name, out)
			}
			walls = append(walls, wall)
		}
		sort.Slice(walls, func(i, j int) bool { return walls[i] < walls[j] })
		median := walls[len(walls)/2]
		t.Logf("statusline by %s wall time (re-exec of test binary): min %v, median %v, max %v", c.name, walls[0], median, walls[len(walls)-1])
		if median > 200*time.Millisecond {
			t.Errorf("%s: median latency %v exceeds 200ms", c.name, median)
		}
	}
	if log := readLog(t, f.p); log != "" {
		t.Errorf("unexpected errors logged: %s", log)
	}
}
