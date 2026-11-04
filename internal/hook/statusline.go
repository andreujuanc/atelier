package hook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/palette"
	"github.com/andreujuanc/atelier/internal/store"
)

// `atl statusline` is the command of Claude Code's status line in task
// sessions (settings key "statusLine": {"type":"command","command":...},
// written by gen.WriteSettings). Claude Code runs it with session JSON on
// stdin (session_id, transcript_path, cwd, workspace.current_dir,
// workspace.project_dir, model, ...) and shows its first stdout line under
// the prompt. It prints "● <task title>", the dot coloured like the TUI
// draws the session's state, or nothing at all when the task cannot be
// resolved. Like the hook it is fast (one DB open, two queries), never
// prints errors to stdout and always exits 0; errors go to hook.log.

// EventStatusLine names `atl statusline` in hook.log lines.
const EventStatusLine = "statusline"

// maxStatusInput caps the status line JSON read from stdin (Claude Code
// sends a few KiB); larger input prints nothing.
const maxStatusInput = 1 << 20

// statusLineWidth caps the printed line in terminal columns (dot, space
// and title; the title is cut with "…").
const statusLineWidth = 80

// statusLineTimeout bounds the DB work, far below Claude Code's own limit.
const statusLineTimeout = 2 * time.Second

// statusInput is the part of Claude Code's status line JSON atelier uses.
type statusInput struct {
	SessionID string
	Cwds      []string // cwd, workspace.current_dir, workspace.project_dir (non-empty, in that order)
}

// StatusLineMain runs `atl statusline` with the process's stdio and
// environment and returns the exit code (always 0).
func StatusLineMain() int {
	return runStatusLine(os.Stdin, os.Stdout, os.Stderr, os.Getenv)
}

func runStatusLine(stdin io.Reader, stdout, stderr io.Writer, env func(string) string) (code int) {
	var p config.Paths
	sid := ""
	defer func() {
		if r := recover(); r != nil {
			logError(p, stderr, EventStatusLine, sid, fmt.Errorf("panic: %v\n%s", r, debug.Stack()))
			code = 0
		}
	}()
	p, err := config.Resolve()
	if err != nil {
		logError(p, stderr, EventStatusLine, sid, err)
		return 0
	}
	data, err := io.ReadAll(io.LimitReader(stdin, maxStatusInput+1))
	if err != nil {
		logError(p, stderr, EventStatusLine, sid, fmt.Errorf("read stdin: %w", err))
		return 0
	}
	if len(data) > maxStatusInput {
		logError(p, stderr, EventStatusLine, sid, fmt.Errorf("status line input exceeds %d bytes", maxStatusInput))
		return 0
	}
	in, err := parseStatusInput(data)
	if err != nil {
		logError(p, stderr, EventStatusLine, sid, fmt.Errorf("decode status line input: %w", err))
		return 0
	}
	sid = in.SessionID
	if _, err := os.Stat(p.DB); err != nil {
		return 0 // no database (yet): never create one from here
	}
	st, err := store.OpenWithTimeout(p.DB, dbBusyTimeout)
	if err != nil {
		logError(p, stderr, EventStatusLine, sid, err)
		return 0
	}
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), statusLineTimeout)
	defer cancel()

	task, state, err := statusTask(ctx, st, in, env, p)
	if err != nil {
		logError(p, stderr, EventStatusLine, sid, err)
	}
	if task == nil {
		return 0
	}
	fmt.Fprintln(stdout, statusLineText(task, state, env("NO_COLOR") == "", palette.DarkBackground(env("COLORFGBG"))))
	return 0
}

// parseStatusInput decodes Claude Code's status line JSON defensively:
// the input must be a JSON object, but fields that are missing or of an
// unexpected type are ignored.
func parseStatusInput(data []byte) (statusInput, error) {
	var in statusInput
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return in, err
	}
	if root == nil {
		return in, errors.New("not a JSON object")
	}
	in.SessionID = jsonString(root["session_id"])
	cwds := []string{jsonString(root["cwd"])}
	var ws map[string]json.RawMessage
	if raw, ok := root["workspace"]; ok && json.Unmarshal(raw, &ws) == nil {
		cwds = append(cwds, jsonString(ws["current_dir"]), jsonString(ws["project_dir"]))
	}
	for _, c := range cwds {
		if c != "" {
			in.Cwds = append(in.Cwds, c)
		}
	}
	return in, nil
}

// jsonString returns raw's value if it is a JSON string, else "".
func jsonString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// statusTask resolves the status line's task and the state its dot shows:
// the row with the input's claude session id (that session's own state),
// else the task from ATL_TASK or the cwd as the hook resolves it
// (resolveTask; the state of all its sessions, model.TaskState). A task
// with no match is nil. At most two queries on the common paths.
func statusTask(ctx context.Context, st *store.Store, in statusInput, env func(string) string, p config.Paths) (*model.Task, model.SessionState, error) {
	if in.SessionID != "" {
		sess, err := st.GetSessionByClaudeID(ctx, in.SessionID)
		if err == nil {
			t, err := st.GetTaskBrief(ctx, sess.TaskID)
			if err == nil {
				return t, sess.State, nil
			}
			if !errors.Is(err, store.ErrNotFound) {
				return nil, "", err
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, "", err
		}
	}
	t, err := resolveTask(statusCwd(in.Cwds, p), env, p, func(slug string) (*model.Task, error) {
		return st.GetTaskBriefBySlug(ctx, slug)
	})
	if err != nil || t == nil {
		return nil, "", err
	}
	ss, err := st.ListSessions(ctx, t.ID)
	if err != nil {
		return t, "", err // still show the title, with a plain dot
	}
	return t, model.TaskState(ss), nil
}

// statusCwd returns the first of cwds inside a task dir, else the first.
func statusCwd(cwds []string, p config.Paths) string {
	for _, c := range cwds {
		if slugFromCwd(c, p) != "" {
			return c
		}
	}
	if len(cwds) > 0 {
		return cwds[0]
	}
	return ""
}

// statusLineText renders "● <title>": the title made one line without
// control or format characters (the slug if that leaves nothing), cut to
// fit statusLineWidth columns; the dot coloured like the TUI draws state
// (palette.StateLook) when color is set.
func statusLineText(t *model.Task, state model.SessionState, color, dark bool) string {
	title := oneLine(t.Title)
	if title == "" {
		title = oneLine(t.Slug)
	}
	title = ansi.Truncate(title, statusLineWidth-2, "…")
	dot := "●"
	if color {
		dot = palette.StateLook(state).ANSI(dot, dark)
	}
	return dot + " " + title
}
