// Package hook implements `atl hook <event>`: the Claude Code hook handler
// that tracks session state in the database and enforces read-only repos.
//
// This path must stay fast (<50ms): no cobra, one DB open, no tmux calls.
package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/store"
)

// Input is the JSON Claude Code passes to a hook on stdin (fields used by atelier).
type Input struct {
	SessionID        string          `json:"session_id"`
	Cwd              string          `json:"cwd"`
	HookEventName    string          `json:"hook_event_name"`
	Source           string          `json:"source"`            // SessionStart
	Message          string          `json:"message"`           // Notification
	NotificationType string          `json:"notification_type"` // Notification: permission_prompt | idle_prompt | ...
	ToolName         string          `json:"tool_name"`
	ToolInput        json.RawMessage `json:"tool_input"` // file_path / notebook_path
}

// Hook event names handled by atelier.
const (
	EventSessionStart     = "SessionStart"
	EventUserPromptSubmit = "UserPromptSubmit"
	EventNotification     = "Notification"
	EventStop             = "Stop"
	EventSessionEnd       = "SessionEnd"
	EventPreToolUse       = "PreToolUse"
	EventPostToolUse      = "PostToolUse"
)

func knownEvent(e string) bool {
	switch e {
	case EventSessionStart, EventUserPromptSubmit, EventNotification, EventStop,
		EventSessionEnd, EventPreToolUse, EventPostToolUse:
		return true
	}
	return false
}

// maxInput caps the hook JSON read from stdin. It is far above anything
// Claude Code sends (a Write of a very large file); a var so tests can
// lower it.
var maxInput = 64 << 20

const (
	hookTimeout = 10 * time.Second
	// dbBusyTimeout bounds how long a hook waits on a locked database, so a
	// stuck writer cannot stall Claude Code.
	dbBusyTimeout = 500 * time.Millisecond
)

// Main runs `atl hook <args...>`: args[0] is the event name. It reads the
// hook JSON from stdin and returns the process exit code: 2 only when the
// ro guard blocks a tool call, 0 otherwise. It never writes to stdout;
// errors are appended to Paths.HookLog().
func Main(args []string) int {
	return run(args, os.Stdin, os.Stderr, os.Getenv)
}

func run(args []string, stdin io.Reader, stderr io.Writer, env func(string) string) (code int) {
	event := ""
	if len(args) > 0 {
		event = args[0]
	}
	var p config.Paths
	var sid string
	defer func() {
		if r := recover(); r != nil {
			logError(p, stderr, event, sid, fmt.Errorf("panic: %v\n%s", r, debug.Stack()))
			code = 0
		}
	}()

	p, err := config.Resolve()
	if err != nil {
		logError(p, stderr, event, sid, err)
		return 0
	}
	if event == "" {
		logError(p, stderr, event, sid, errors.New("missing event name (usage: atl hook <event>)"))
		return 0
	}
	data, err := io.ReadAll(io.LimitReader(stdin, int64(maxInput)+1))
	if err != nil {
		logError(p, stderr, event, sid, fmt.Errorf("read stdin: %w", err))
		if event == EventPreToolUse && len(data) > 0 {
			return guardPartial(p, stderr, env, event, data, false)
		}
		return 0
	}
	if len(data) > maxInput {
		logError(p, stderr, event, sid, fmt.Errorf("hook input exceeds %d bytes", maxInput))
		if event == EventPreToolUse {
			return guardPartial(p, stderr, env, event, data, true)
		}
		return 0
	}
	var in Input
	if err := json.Unmarshal(data, &in); err != nil {
		logError(p, stderr, event, sid, fmt.Errorf("decode hook input: %w", err))
		if event == EventPreToolUse {
			return guardPartial(p, stderr, env, event, data, false)
		}
		return 0
	}
	sid = in.SessionID
	if !knownEvent(event) {
		return 0
	}
	return handleMain(p, stderr, env, event, in)
}

// handleMain opens the store and runs Handle, mapping its result to the
// process exit code.
func handleMain(p config.Paths, stderr io.Writer, env func(string) string, event string, in Input) int {
	st, err := store.OpenWithTimeout(p.DB, dbBusyTimeout)
	if err != nil {
		logError(p, stderr, event, in.SessionID, err)
		return 0
	}
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
	defer cancel()

	exit, msg, err := Handle(ctx, event, in, env, st, p, time.Now())
	if err != nil {
		logError(p, stderr, event, in.SessionID, err)
	}
	if exit == 2 {
		fmt.Fprintln(stderr, msg)
		return 2
	}
	return 0
}

// guardPartial runs the ro guard on PreToolUse input that could not be
// read or decoded in full (truncated, oversized or malformed), using
// whatever fields a streaming decode recovers from its prefix. If a target
// path is recovered the guard decides as usual; otherwise an oversized
// input is refused (exit 2: the guard cannot see where the write goes) and
// any other undecodable input fails open.
func guardPartial(p config.Paths, stderr io.Writer, env func(string) string, event string, data []byte, oversized bool) int {
	in, ok := partialInput(data)
	if ok {
		code := handleMain(p, stderr, env, event, in)
		if code == 2 || !oversized {
			return code
		}
	} else if !oversized {
		return 0
	}
	fmt.Fprintf(stderr, "atelier: hook input exceeds %d bytes; refusing the %s tool call because the read-only repo guard cannot inspect it\n",
		maxInput, orDash(in.ToolName))
	return 2
}

// partialInput recovers session_id, cwd, tool_name and tool_input's
// file_path/notebook_path from a possibly truncated or malformed hook JSON
// object by decoding it token by token and stopping at the first error.
// Other values (e.g. a huge "content" string) are skipped without being
// kept. As with json.Unmarshal, a duplicate key's last value wins (as far
// as the input could be read). ok reports whether a target path was found.
func partialInput(data []byte) (in Input, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var targets map[string]string
	_ = walkObject(dec, func(key string) error {
		switch key {
		case "session_id":
			return dec.Decode(&in.SessionID)
		case "cwd":
			return dec.Decode(&in.Cwd)
		case "tool_name":
			return dec.Decode(&in.ToolName)
		case "hook_event_name":
			return dec.Decode(&in.HookEventName)
		case "tool_input":
			targets = map[string]string{} // a later tool_input replaces an earlier one
			return walkObject(dec, func(key string) error {
				if key != "file_path" && key != "notebook_path" {
					return skipValue(dec)
				}
				var raw json.RawMessage
				if err := dec.Decode(&raw); err != nil {
					return err
				}
				delete(targets, key)
				var v string
				if json.Unmarshal(raw, &v) == nil && v != "" {
					targets[key] = v
				}
				return nil
			})
		}
		return skipValue(dec)
	})
	if len(targets) == 0 {
		return in, false
	}
	in.ToolInput, _ = json.Marshal(targets)
	return in, true
}

// walkObject reads a JSON object from dec, calling field for each key with
// the decoder positioned at its value (field must consume the value).
func walkObject(dec *json.Decoder, field func(key string) error) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return errors.New("not a JSON object")
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := tok.(string)
		if !ok {
			return errors.New("object key is not a string")
		}
		if err := field(key); err != nil {
			return err
		}
	}
	_, err = dec.Token()
	return err
}

// skipValue consumes one JSON value from dec.
func skipValue(dec *json.Decoder) error {
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			default:
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}

// logError appends one line to the hook log. If the log cannot be written
// the error goes to stderr instead (never stdout).
func logError(p config.Paths, stderr io.Writer, event, sid string, err error) {
	line := fmt.Sprintf("%s event=%s session_id=%s error=%s\n", time.Now().Format(time.RFC3339),
		orDash(event), orDash(sid), strings.ReplaceAll(err.Error(), "\n", "\\n"))
	if p.StateDir != "" {
		if mkErr := os.MkdirAll(p.StateDir, 0o755); mkErr == nil {
			f, ferr := os.OpenFile(p.HookLog(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if ferr == nil {
				_, werr := f.WriteString(line)
				if cerr := f.Close(); werr == nil && cerr == nil {
					return
				}
			}
		}
	}
	fmt.Fprint(stderr, "atl hook: "+line)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Handle is the testable core of `atl hook`. It returns the exit code (2
// only for an ro-guard block), the stderr message for exit 2, and an error
// to log (which never changes the exit code from 0).
func Handle(ctx context.Context, event string, in Input, env func(string) string, st *store.Store,
	p config.Paths, now time.Time) (exit int, msg string, err error) {
	switch event {
	case EventPreToolUse:
		return guard(ctx, in, env, st, p)
	case EventSessionStart, EventUserPromptSubmit, EventNotification, EventStop,
		EventSessionEnd, EventPostToolUse:
		return 0, "", transition(ctx, event, in, env, st, p, now)
	default:
		return 0, "", nil
	}
}

func transition(ctx context.Context, event string, in Input, env func(string) string, st *store.Store,
	p config.Paths, now time.Time) error {
	sess, err := findSession(ctx, event, in, env, st)
	if err != nil {
		return err
	}
	if sess == nil {
		if event != EventSessionStart {
			return nil // not a session atelier knows about
		}
		task, err := findTask(ctx, in, env, st, p)
		if err != nil || task == nil {
			return err
		}
		return st.CreateSession(ctx, &model.Session{TaskID: task.ID, ClaudeSessionID: in.SessionID,
			State: model.StateIdle, CreatedAt: now, LastEventAt: now})
	}

	id := sess.ID
	switch event {
	case EventSessionStart:
		if in.Source == "compact" {
			// Auto-compaction can happen mid-turn: the session keeps
			// running, only its id may change.
			if in.SessionID == "" {
				return st.TouchSession(ctx, id, now)
			}
			return st.SetClaudeSessionID(ctx, id, in.SessionID, now)
		}
		if in.SessionID == "" {
			return st.SetSessionState(ctx, id, model.StateIdle, now)
		}
		return st.SetClaudeSession(ctx, id, in.SessionID, now)
	case EventUserPromptSubmit:
		return st.SetSessionState(ctx, id, model.StateRunning, now)
	case EventNotification:
		if isPermissionPrompt(in) {
			return st.SetSessionState(ctx, id, model.StateWaiting, now)
		}
		return st.TouchSession(ctx, id, now)
	case EventPostToolUse:
		ok, err := st.SetStateIf(ctx, id, model.StateWaiting, model.StateRunning, now)
		if err != nil || ok {
			return err
		}
		return st.TouchSession(ctx, id, now)
	case EventStop:
		return st.SetSessionState(ctx, id, model.StateIdle, now)
	case EventSessionEnd:
		return st.SetSessionState(ctx, id, model.StateEnded, now)
	}
	return nil
}

// isPermissionPrompt reports whether a Notification asks the user for a
// permission decision (the only notification that means "waiting").
func isPermissionPrompt(in Input) bool {
	if in.NotificationType != "" {
		return in.NotificationType == "permission_prompt"
	}
	return strings.Contains(strings.ToLower(in.Message), "permission")
}

// findSession resolves the session row for a hook event. It returns nil,
// nil if nothing matches.
//
// ATL_SESSION (the row atl launched this Claude process for; it must exist
// and belong to the ATL_TASK task if that is set) is used when its
// claude_session_id is empty, equals the event's session id, or the event
// carries none. Because the variable is inherited, a nested `claude` run
// from inside the session also sees it, so on an id mismatch it is trusted
// only for a SessionStart that legitimately changes the id (source clear,
// resume or compact). Otherwise — a nested process's startup, or any other
// event from it — the row is looked up by claude session id instead, and
// the parent row is never touched (a nested SessionStart then creates its
// own row). The ATL_SESSION row with an empty claude id is also only a
// fallback for non-SessionStart events when no row has the event's id.
func findSession(ctx context.Context, event string, in Input, env func(string) string, st *store.Store) (*model.Session, error) {
	envSess, err := envSession(ctx, env, st)
	if err != nil {
		return nil, err
	}
	if envSess != nil {
		switch {
		case in.SessionID == "" || envSess.ClaudeSessionID == in.SessionID:
			return envSess, nil
		case envSess.ClaudeSessionID == "":
			if event == EventSessionStart {
				return envSess, nil // first start of a session atl launched
			}
		case event == EventSessionStart && idChangingSource(in.Source):
			return envSess, nil
		default:
			envSess = nil // another (nested) Claude process: not this row
		}
	}
	sess, err := st.GetSessionByClaudeID(ctx, in.SessionID)
	if errors.Is(err, store.ErrNotFound) {
		return envSess, nil
	}
	return sess, err
}

// idChangingSource reports whether a SessionStart source means the same
// Claude process continues under a new session id.
func idChangingSource(source string) bool {
	switch source {
	case "clear", "resume", "compact":
		return true
	}
	return false
}

// envSession returns the ATL_SESSION row if it is set, parses, exists and
// belongs to the ATL_TASK task (when set); nil, nil otherwise.
func envSession(ctx context.Context, env func(string) string, st *store.Store) (*model.Session, error) {
	v := env("ATL_SESSION")
	if v == "" {
		return nil, nil
	}
	id, perr := strconv.ParseInt(v, 10, 64)
	if perr != nil {
		return nil, nil
	}
	sess, err := st.GetSession(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if belongs, err := sessionInEnvTask(ctx, sess, env, st); err != nil || !belongs {
		return nil, err
	}
	return sess, nil
}

func sessionInEnvTask(ctx context.Context, sess *model.Session, env func(string) string, st *store.Store) (bool, error) {
	slug := env("ATL_TASK")
	if slug == "" {
		return true, nil
	}
	task, err := st.GetTaskBySlug(ctx, slug)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return task.ID == sess.TaskID, nil
}

// findTask resolves the task from ATL_TASK, else from cwd being inside
// Paths.Tasks/<slug>. It returns nil, nil if neither matches.
func findTask(ctx context.Context, in Input, env func(string) string, st *store.Store, p config.Paths) (*model.Task, error) {
	if slug := env("ATL_TASK"); slug != "" {
		task, err := st.GetTaskBySlug(ctx, slug)
		if err == nil || !errors.Is(err, store.ErrNotFound) {
			return task, err
		}
	}
	slug := slugFromCwd(in.Cwd, p)
	if slug == "" {
		return nil, nil
	}
	task, err := st.GetTaskBySlug(ctx, slug)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	return task, err
}

// slugFromCwd returns <slug> if cwd is Tasks/<slug> or below, else "".
func slugFromCwd(cwd string, p config.Paths) string {
	if cwd == "" || p.Tasks == "" || !filepath.IsAbs(cwd) {
		return ""
	}
	for _, c := range []string{realPath(filepath.Clean(cwd)), filepath.Clean(cwd)} {
		for _, root := range []string{realPath(p.Tasks), filepath.Clean(p.Tasks)} {
			if rel, ok := within(root, c); ok && rel != "" {
				return strings.SplitN(rel, string(filepath.Separator), 2)[0]
			}
		}
	}
	return ""
}

// realPath returns EvalSymlinks(path), or path if that fails.
func realPath(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	return path
}

// within reports whether path is root or inside it (component-wise), and
// returns path relative to root ("" for root itself). Both must be clean.
func within(root, path string) (string, bool) {
	if path == root {
		return "", true
	}
	prefix := root
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	if strings.HasPrefix(path, prefix) {
		return path[len(prefix):], true
	}
	return "", false
}
