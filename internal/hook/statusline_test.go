package hook

import (
	"bytes"
	"context"
	"database/sql"
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

// dbSnap is the database file's contents and modification time.
type dbSnap struct {
	data  []byte
	mtime time.Time
}

func snapDB(t *testing.T, path string) dbSnap {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return dbSnap{b, fi.ModTime()}
}

// rawDB opens path with plain SQLite (no store.Open, so no migration).
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	return db
}

func userVersion(t *testing.T, path string) int {
	t.Helper()
	db := rawDB(t, path)
	defer db.Close()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestStatusLineReadOnly: `atl statusline` never writes the database. A
// current one is read without changing the file; an older (v4), newer or
// empty one prints nothing, is not migrated or created, and is not logged.
// (Reading a WAL database may leave empty -wal/-shm files next to it, see
// store.OpenReadOnly; the database file itself is untouched.)
func TestStatusLineReadOnly(t *testing.T) {
	f := setup(t)
	f.session(t, model.StateIdle, "c-fix")
	f.st.Close() // the last connection: the WAL is folded into the file
	env := map[string]string{"ATL_TASK": "fix", "NO_COLOR": "1"}

	t.Run("current schema", func(t *testing.T) {
		before := snapDB(t, f.p.DB)
		if out, errOut := statusRun(t, `{"session_id":"c-fix"}`, env); out != "● Fix\n" || errOut != "" {
			t.Errorf("stdout %q stderr %q", out, errOut)
		}
		if after := snapDB(t, f.p.DB); !bytes.Equal(before.data, after.data) || !before.mtime.Equal(after.mtime) {
			t.Error("database file changed")
		}
	})

	t.Run("older schema (v4) stays v4", func(t *testing.T) {
		// Turn the fixture's database into a genuine v4 one: v5 only added
		// tasks.notes (and filled it from task_notes).
		db := rawDB(t, f.p.DB)
		for _, q := range []string{"ALTER TABLE tasks DROP COLUMN notes", "PRAGMA user_version = 4"} {
			if _, err := db.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
		db.Close()
		before := snapDB(t, f.p.DB)
		os.Remove(f.p.HookLog())
		if out, errOut := statusRun(t, `{"session_id":"c-fix"}`, env); out != "" || errOut != "" {
			t.Errorf("stdout %q stderr %q, want nothing", out, errOut)
		}
		if after := snapDB(t, f.p.DB); !bytes.Equal(before.data, after.data) || !before.mtime.Equal(after.mtime) {
			t.Error("database file changed")
		}
		if v := userVersion(t, f.p.DB); v != 4 {
			t.Errorf("user_version = %d, want 4 (migrated by the status line)", v)
		}
		if log := readLog(t, f.p); log != "" {
			t.Errorf("logged: %s", log)
		}
	})

	t.Run("newer schema", func(t *testing.T) {
		db := rawDB(t, f.p.DB)
		if _, err := db.Exec("PRAGMA user_version = 99"); err != nil {
			t.Fatal(err)
		}
		db.Close()
		before := snapDB(t, f.p.DB)
		if out, _ := statusRun(t, `{"session_id":"c-fix"}`, env); out != "" {
			t.Errorf("stdout %q, want nothing", out)
		}
		if after := snapDB(t, f.p.DB); !bytes.Equal(before.data, after.data) || !before.mtime.Equal(after.mtime) {
			t.Error("database file changed")
		}
	})

	t.Run("zero-byte file stays empty", func(t *testing.T) {
		data := filepath.Join(f.root, "emptydata")
		t.Setenv("XDG_DATA_HOME", data)
		dbPath := filepath.Join(data, "atelier", "atelier.db")
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dbPath, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		before := snapDB(t, dbPath)
		os.Remove(f.p.HookLog())
		if out, errOut := statusRun(t, `{"session_id":"c-fix"}`, env); out != "" || errOut != "" {
			t.Errorf("stdout %q stderr %q, want nothing", out, errOut)
		}
		if after := snapDB(t, dbPath); len(after.data) != 0 || !before.mtime.Equal(after.mtime) {
			t.Errorf("empty database written: %d bytes", len(after.data))
		}
		ents, _ := os.ReadDir(filepath.Dir(dbPath))
		if len(ents) != 1 {
			t.Errorf("files next to the empty database: %v", ents)
		}
		if log := readLog(t, f.p); log != "" {
			t.Errorf("logged: %s", log)
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
