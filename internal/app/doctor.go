package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andreujuanc/atelier/internal/store"
)

// Check is one result of Doctor. OK is false only for failures; a warning
// has OK and Warn both true.
type Check struct {
	Name   string
	OK     bool
	Warn   bool
	Detail string
}

const checkTimeout = 5 * time.Second

func pass(name, detail string) Check { return Check{Name: name, OK: true, Detail: detail} }
func warn(name, detail string) Check { return Check{Name: name, OK: true, Warn: true, Detail: detail} }
func fail(name, detail string) Check { return Check{Name: name, Detail: detail} }

// Doctor checks the external tools, directories and database atelier needs.
// It does not use a.Store (it opens the database itself), so it works on an
// App whose store failed to open.
func (a *App) Doctor(ctx context.Context) []Check {
	tools := []struct {
		name string
		args []string
		ver  func(out string) Check
	}{
		{"git", []string{"--version"}, nil},
		{"tmux", []string{"-V"}, checkTmuxVersion},
		{"claude", []string{"--version"}, nil},
		{"gh", []string{"--version"}, nil},
	}
	checks := make([]Check, len(tools))
	var wg sync.WaitGroup
	for i, tl := range tools {
		wg.Add(1)
		go func() {
			defer wg.Done()
			checks[i] = checkTool(ctx, tl.name, tl.args, tl.ver)
		}()
	}
	wg.Wait()
	if a.Paths.Workspace != "" {
		checks = append(checks, pass("workspace", a.Paths.Workspace))
	}
	checks = append(checks,
		a.checkTasksDir(),
		a.checkReposDir(),
		a.checkDB(ctx),
		a.checkStateDir(),
	)
	return checks
}

func checkTool(ctx context.Context, name string, args []string, ver func(string) Check) Check {
	path, err := exec.LookPath(name)
	if err != nil {
		return fail(name, "not found on PATH")
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	first := strings.TrimSpace(string(out))
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = strings.TrimSpace(first[:i])
	}
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		detail := fmt.Sprintf("%s %s: %v", path, strings.Join(args, " "), err)
		if first != "" {
			detail += ": " + first
		}
		return fail(name, detail)
	}
	if ver != nil {
		c := ver(first)
		c.Name = name
		return c
	}
	return pass(name, first)
}

var tmuxVersionRE = regexp.MustCompile(`(\d+)\.(\d+)`)

// checkTmuxVersion requires tmux ≥ 3.2 (`tmux -V` prints e.g. "tmux 3.4" or
// "tmux next-3.5"; unparseable versions such as "tmux master" only warn).
func checkTmuxVersion(out string) Check {
	m := tmuxVersionRE.FindStringSubmatch(out)
	if m == nil {
		return warn("", fmt.Sprintf("%s (cannot parse version; need ≥ 3.2)", out))
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < 3 || (major == 3 && minor < 2) {
		return fail("", fmt.Sprintf("%s is too old; need ≥ 3.2", out))
	}
	return pass("", out)
}

func (a *App) checkTasksDir() Check {
	const name = "tasks dir"
	fi, err := os.Stat(a.Paths.Tasks)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return warn(name, a.Paths.Tasks+" does not exist (created on first `atl new`)")
	case err != nil:
		return fail(name, err.Error())
	case !fi.IsDir():
		return fail(name, a.Paths.Tasks+" is not a directory")
	}
	return pass(name, a.Paths.Tasks)
}

func (a *App) checkReposDir() Check {
	const name = "repos dir"
	fi, err := os.Stat(a.Paths.Repos)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return warn(name, a.Paths.Repos+" does not exist; clone repos there or set $ATL_REPOS (default ~/repos)")
	case err != nil:
		return warn(name, err.Error())
	case !fi.IsDir():
		return warn(name, a.Paths.Repos+" is not a directory; set $ATL_REPOS (default ~/repos)")
	}
	return pass(name, a.Paths.Repos)
}

func (a *App) checkDB(ctx context.Context) Check {
	const name = "database"
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	st, err := store.Open(a.Paths.DB)
	if err != nil {
		return fail(name, err.Error())
	}
	defer st.Close()
	tasks, err := st.ListTasks(ctx)
	if err != nil {
		return fail(name, fmt.Sprintf("%s: %v", a.Paths.DB, err))
	}
	return pass(name, fmt.Sprintf("%s (migrated, %d tasks)", a.Paths.DB, len(tasks)))
}

func (a *App) checkStateDir() Check {
	const name = "hook log"
	log := a.Paths.HookLog()
	if err := os.MkdirAll(a.Paths.StateDir, 0o755); err != nil {
		return fail(name, err.Error())
	}
	f, err := os.OpenFile(log, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return fail(name, err.Error())
	}
	if err := f.Close(); err != nil {
		return fail(name, err.Error())
	}
	return pass(name, log+" writable")
}

// Level returns "ok", "warn" or "FAIL".
func (c Check) Level() string {
	switch {
	case !c.OK:
		return "FAIL"
	case c.Warn:
		return "warn"
	}
	return "ok"
}
