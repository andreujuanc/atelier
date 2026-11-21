// Package hook implements `atl hook <event>`: the Claude Code hook handler
// that tracks session state in the database and enforces read-only repos.
//
// The hook never writes to stdout, with exactly one exception: a
// SessionStart inside a task prints one JSON object
// {"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":...}}
// carrying fresh task context (see context.go). Errors go to hook.log.
//
// This path must stay fast (<50ms): no cobra, one DB open, no tmux calls.
// In a sandboxed task's container it opens nothing: the input goes to the
// task's gate, which handles it with an open database, limited to that task
// (see gate.go).
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
	"github.com/andreujuanc/atelier/internal/sandbox"
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
	ToolInput        json.RawMessage `json:"tool_input"`      // file_path / notebook_path
	TranscriptPath   string          `json:"transcript_path"` // every event: the session's transcript JSONL

	// landings are where the tool targets land as the task container
	// resolves them (PreToolUse through a sandboxed task's gate): the ro
	// guard checks them besides its own resolution.
	landings []candidate
}

// db is the database a hook works on, and how much of it it may touch.
type db struct {
	st *store.Store // nil: opened per event from p.DB
	p  config.Paths
	// scope is 0 for atl running next to the sessions: any task's rows. A
	// sandboxed task's gate sets it to the task's id: whatever the event or
	// its environment say, only that task and its sessions are read or
	// written (see TaskServer).
	scope int64
	// claudeDir is, with scope, the task's own Claude config dir: the only
	// place a transcript path from the task may point into (see
	// transcriptIn), since atl reads that file later where it runs.
	claudeDir string
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
	// EventAssistantGuard is the atelier assistant's PreToolUse guard (see
	// assistant.go); it never opens the database.
	EventAssistantGuard = "AssistantGuard"
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
// ro guard blocks a tool call, 0 otherwise. Errors are appended to
// Paths.HookLog(). The only stdout output is the SessionStart task context
// JSON (one object, only for a session resolved to a task).
//
// In a task container (sandbox.InEnv set) it opens nothing: the task's gate
// handles the event (see viaGate and TaskServer).
func Main(args []string) int {
	return run(args, os.Stdin, os.Stdout, os.Stderr, os.Getenv)
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, env func(string) string) (code int) {
	event := ""
	if len(args) > 0 {
		event = args[0]
	}
	if event != EventAssistantGuard && env(sandbox.InEnv) != "" {
		return viaGate(event, stdin, stdout, stderr, env)
	}
	var p config.Paths
	defer func() {
		if r := recover(); r != nil {
			logError(p, stderr, event, "", fmt.Errorf("panic: %v\n%s", r, debug.Stack()))
			code = 0
			if event == EventAssistantGuard {
				code = assistantRefuse(stderr, "", "internal error")
			}
		}
	}()

	p, err := config.Resolve()
	if err != nil {
		logError(p, stderr, event, "", err)
		if event == EventAssistantGuard {
			return assistantRefuse(stderr, "", "cannot resolve atelier paths")
		}
		return 0
	}
	if event == EventAssistantGuard {
		return assistantGuardMain(p, stdin, stderr)
	}
	if event == "" {
		return db{p: p}.serve(event, nil, nil, nil, stdout, stderr, env)
	}
	data, err := io.ReadAll(io.LimitReader(stdin, int64(maxInput)+1))
	return db{p: p}.serve(event, data, err, nil, stdout, stderr, env)
}

// serve handles one hook event whose input is data; readErr says reading it
// failed after those bytes. landings come from a task container (see
// Input.landings). It returns the exit code.
func (d db) serve(event string, data []byte, readErr error, landings []candidate, stdout, stderr io.Writer, env func(string) string) int {
	if event == "" {
		logError(d.p, stderr, event, "", errors.New("missing event name (usage: atl hook <event>)"))
		return 0
	}
	if readErr != nil {
		logError(d.p, stderr, event, "", fmt.Errorf("read stdin: %w", readErr))
		if event == EventPreToolUse && len(data) > 0 {
			return d.guardPartial(stderr, env, event, data, false, landings)
		}
		return 0
	}
	if len(data) > maxInput {
		logError(d.p, stderr, event, "", fmt.Errorf("hook input exceeds %d bytes", maxInput))
		if event == EventPreToolUse {
			return d.guardPartial(stderr, env, event, data, true, landings)
		}
		return 0
	}
	var in Input
	if err := json.Unmarshal(data, &in); err != nil {
		logError(d.p, stderr, event, "", fmt.Errorf("decode hook input: %w", err))
		if event == EventPreToolUse {
			return d.guardPartial(stderr, env, event, data, false, landings)
		}
		return 0
	}
	if !knownEvent(event) {
		return 0
	}
	in.landings = landings
	return d.handleMain(stdout, stderr, env, event, in)
}

// handleMain opens the store (unless d has one) and runs Handle, mapping its
// result to the process exit code. For a SessionStart resolved to a task it
// then writes the task context JSON to stdout (stdout is nil for callers
// that must never print it).
func (d db) handleMain(stdout, stderr io.Writer, env func(string) string, event string, in Input) int {
	if d.st == nil {
		st, err := store.OpenWithTimeout(d.p.DB, dbBusyTimeout)
		if err != nil {
			logError(d.p, stderr, event, in.SessionID, err)
			return 0
		}
		defer st.Close()
		d.st = st
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
	defer cancel()

	now := time.Now()
	exit, msg, taskID, err := d.handle(ctx, event, in, env, now)
	if err != nil {
		logError(d.p, stderr, event, in.SessionID, err)
	}
	if event == EventSessionStart && taskID != 0 && stdout != nil {
		sandboxed := d.scope != 0 || env(sandbox.InEnv) != ""
		if err := writeSessionContext(ctx, stdout, d.st, d.p, taskID, env(envContextVar), sandboxed, now); err != nil {
			logError(d.p, stderr, event, in.SessionID, fmt.Errorf("session context: %w", err))
		}
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
func (d db) guardPartial(stderr io.Writer, env func(string) string, event string, data []byte, oversized bool, landings []candidate) int {
	in, ok := partialInput(data)
	if ok {
		in.landings = landings
		code := d.handleMain(nil, stderr, env, event, in)
		if code == 2 || !oversized {
			return code
		}
	} else if !oversized {
		return 0
	}
	return refuseOversized(stderr, in.ToolName)
}

// refuseOversized prints the refusal of a PreToolUse whose input is too
// large for the ro guard and returns exit code 2.
func refuseOversized(stderr io.Writer, tool string) int {
	fmt.Fprintf(stderr, "atelier: hook input exceeds %d bytes; refusing the %s tool call because the read-only repo guard cannot inspect it\n",
		maxInput, orDash(tool))
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
	// One line per error: the event name and session id come from the hook
	// input (in a sandboxed task, from the task), so they cannot add lines.
	oneLineField := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return '?'
			}
			return r
		}, s)
	}
	line := fmt.Sprintf("%s event=%s session_id=%s error=%s\n", time.Now().Format(time.RFC3339),
		oneLineField(orDash(event)), oneLineField(orDash(sid)), strings.ReplaceAll(err.Error(), "\n", "\\n"))
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
	exit, msg, _, err = db{st: st, p: p}.handle(ctx, event, in, env, now)
	return exit, msg, err
}

// handle is Handle that also returns the id of the task the event's
// session was resolved to (0 if none, or for events that do not resolve a
// session).
func (d db) handle(ctx context.Context, event string, in Input, env func(string) string, now time.Time) (exit int, msg string, taskID int64, err error) {
	switch event {
	case EventPreToolUse:
		exit, msg, err = d.guard(ctx, in, env)
		return exit, msg, 0, err
	case EventSessionStart, EventUserPromptSubmit, EventNotification, EventStop,
		EventSessionEnd, EventPostToolUse:
		taskID, err = d.transition(ctx, event, in, env, now)
		return 0, "", taskID, err
	default:
		return 0, "", 0, nil
	}
}

// transition applies event to its session's row and returns the session's
// task id (0 if the event matched no session or task).
func (d db) transition(ctx context.Context, event string, in Input, env func(string) string, now time.Time) (int64, error) {
	st := d.st
	if event == EventSessionStart && d.scope != 0 {
		// A Claude session id is how the hooks and the status line of every
		// task find a session row (GetSessionByClaudeID): a sandboxed task
		// must not put another task's id on a row of its own.
		taken, err := st.ClaudeIDInOtherTask(ctx, in.SessionID, d.scope)
		if err != nil {
			return 0, err
		}
		if taken {
			return 0, fmt.Errorf("claude session id %q belongs to another task's session; ignored", in.SessionID)
		}
	}
	if d.scope != 0 && in.TranscriptPath != "" {
		if err := transcriptIn(d.claudeDir, in.TranscriptPath); err != nil {
			logError(d.p, io.Discard, event, in.SessionID, fmt.Errorf("transcript path ignored: %w", err))
			in.TranscriptPath = ""
		}
	}
	sess, err := d.findSession(ctx, event, in, env)
	if err != nil {
		return 0, err
	}
	if sess == nil {
		if event != EventSessionStart {
			return 0, nil // not a session atelier knows about
		}
		task, err := d.findTask(ctx, in, env)
		if err != nil || task == nil {
			return 0, err
		}
		return task.ID, st.CreateSession(ctx, &model.Session{TaskID: task.ID, ClaudeSessionID: in.SessionID,
			State: model.StateIdle, CreatedAt: now, LastEventAt: now, TranscriptPath: in.TranscriptPath})
	}
	if err := transitionState(ctx, event, in, sess.ID, st, now); err != nil {
		return sess.TaskID, err
	}
	return sess.TaskID, recordTranscript(ctx, event, in, sess, st)
}

// recordTranscript stores the event's transcript path: SessionStart (any
// source: /clear, resume and compaction may start a new file) overwrites,
// other events only fill an empty one. At most one statement, and none
// when the row already has the value.
func recordTranscript(ctx context.Context, event string, in Input, sess *model.Session, st *store.Store) error {
	if in.TranscriptPath == "" || in.TranscriptPath == sess.TranscriptPath {
		return nil
	}
	if event == EventSessionStart {
		return st.SetTranscriptPath(ctx, sess.ID, in.TranscriptPath)
	}
	if sess.TranscriptPath != "" {
		return nil
	}
	return st.SetTranscriptPathIfEmpty(ctx, sess.ID, in.TranscriptPath)
}

// transcriptIn returns nil if path, with its symlinks resolved here (where
// the gate runs, at the same paths as in the task container), lies inside
// dir (resolved too); else why not. A task reports its transcript path
// itself, and atl reads that file later (atl show, atl transcript, the
// picker), so a path elsewhere could make it read another task's or the
// user's files.
func transcriptIn(dir, path string) error {
	if dir == "" {
		return errors.New("the task has no Claude config dir")
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("the task's Claude config dir: %w", err)
	}
	real, err := resolvePath(path)
	if err != nil {
		return fmt.Errorf("%q: %w", path, err)
	}
	if rel, ok := within(root, real); !ok || rel == "" {
		return fmt.Errorf("%q is not inside the task's Claude config dir %s", path, dir)
	}
	return nil
}

// transitionState applies event's state change to session id.
func transitionState(ctx context.Context, event string, in Input, id int64, st *store.Store, now time.Time) error {
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
// only for a SessionStart that legitimately changes the id: source compact
// (which can happen mid-turn), or clear/resume while the row is not
// mid-turn (a real /clear or /resume ends the session first; a nested
// `claude --resume` runs during the parent's turn). Otherwise — a nested process's startup, or any other
// event from it — the row is looked up by claude session id instead, and
// the parent row is never touched (a nested SessionStart then creates its
// own row). The ATL_SESSION row with an empty claude id is also only a
// fallback for non-SessionStart events when no row has the event's id.
//
// With a scope, both lookups only ever return a row of the scope's task.
func (d db) findSession(ctx context.Context, event string, in Input, env func(string) string) (*model.Session, error) {
	envSess, err := d.envSession(ctx, env)
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
		case event == EventSessionStart && idChangingSource(in.Source) &&
			(in.Source == "compact" || !midTurn(envSess.State)):
			return envSess, nil
		default:
			envSess = nil // another (nested) Claude process: not this row
		}
	}
	sess, err := d.sessionByClaudeID(ctx, in.SessionID)
	if errors.Is(err, store.ErrNotFound) {
		return envSess, nil
	}
	return sess, err
}

// sessionByClaudeID is Store.GetSessionByClaudeID within d's scope.
func (d db) sessionByClaudeID(ctx context.Context, cid string) (*model.Session, error) {
	if d.scope != 0 {
		return d.st.GetSessionByClaudeIDInTask(ctx, cid, d.scope)
	}
	return d.st.GetSessionByClaudeID(ctx, cid)
}

// midTurn reports whether a session in state s is in the middle of a turn.
func midTurn(s model.SessionState) bool {
	return s == model.StateRunning || s == model.StateWaiting
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
// belongs to the ATL_TASK task (when set; with a scope, to the scope's task
// whatever ATL_TASK says); nil, nil otherwise.
func (d db) envSession(ctx context.Context, env func(string) string) (*model.Session, error) {
	v := env("ATL_SESSION")
	if v == "" {
		return nil, nil
	}
	id, perr := strconv.ParseInt(v, 10, 64)
	if perr != nil {
		return nil, nil
	}
	sess, err := d.st.GetSession(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if d.scope != 0 {
		if sess.TaskID != d.scope {
			return nil, nil
		}
		return sess, nil
	}
	if belongs, err := sessionInEnvTask(ctx, sess, env, d.st); err != nil || !belongs {
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
// Paths.Tasks/<slug>. It returns nil, nil if neither matches. With a scope
// it is always the scope's task (nil if that is gone).
func (d db) findTask(ctx context.Context, in Input, env func(string) string) (*model.Task, error) {
	if d.scope != 0 {
		return d.scopeTask(ctx)
	}
	return resolveTask(in.Cwd, env, d.p, func(slug string) (*model.Task, error) { return d.st.GetTaskBySlug(ctx, slug) })
}

// scopeTask returns the scope's task, or nil, nil if it is gone.
func (d db) scopeTask(ctx context.Context) (*model.Task, error) {
	t, err := d.st.GetTask(ctx, d.scope)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	return t, err
}

// resolveTask is the task resolution order shared by the hook and `atl
// statusline`: the ATL_TASK slug if get finds it, else the slug of cwd
// under Paths.Tasks. get returns store.ErrNotFound for a missing slug; the
// zero T and a nil error mean no match. Any other error is returned as is.
func resolveTask[T any](cwd string, env func(string) string, p config.Paths, get func(slug string) (T, error)) (T, error) {
	var zero T
	if slug := env("ATL_TASK"); slug != "" {
		t, err := get(slug)
		if err == nil || !errors.Is(err, store.ErrNotFound) {
			return t, err
		}
	}
	slug := slugFromCwd(cwd, p)
	if slug == "" {
		return zero, nil
	}
	t, err := get(slug)
	if errors.Is(err, store.ErrNotFound) {
		return zero, nil
	}
	return t, err
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
