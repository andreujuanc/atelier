package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/sandbox"
	"github.com/andreujuanc/atelier/internal/store"
)

// A sandboxed task's container has no atelier database. There, `atl hook`,
// `atl statusline`, `atl note` and `atl notes` send their input to the task's
// gate (sandbox.Control), and the gate answers them in its own process with
// a TaskServer: same code as next to atl, with the store limited to the
// gate's task (db.scope). The hook log is written where the gate runs.

// gateTimeout bounds a hook's or the status line's exchange with the gate.
// A healthy gate answers in milliseconds; the database's busy timeout is
// far below this.
const gateTimeout = 20 * time.Second

// maxLandingBytes and maxEnvValue bound what viaGate puts on the request
// line (at most 64 KiB): the container's resolution of the tool targets and
// each variable (ATL_CONTEXT is cut to 1000 bytes anyway, after sanitising).
const (
	maxLandingBytes = 32 << 10
	maxEnvValue     = 8 << 10
)

// viaGate is `atl hook <event>` in a task container. It reads the input as
// run does and sends it to the task's gate, with the few variables of its
// environment the hook uses and, for PreToolUse, where the tool's targets
// land in this container. The gate's stdout, stderr and exit code are
// passed on as they are. When the gate cannot answer, the hook behaves like
// one that fails: exit 0 with the error on stderr (there is no hook log in
// here), except that an oversized PreToolUse input is still refused, and
// that the assistant's guard (AssistantGuard, from the assistant's
// container) refuses: it fails closed.
func viaGate(event string, stdin io.Reader, stdout, stderr io.Writer, env func(string) string) int {
	var data []byte
	var readErr error
	if event != "" {
		data, readErr = io.ReadAll(io.LimitReader(stdin, int64(maxInput)+1))
	}
	req := sandbox.ControlRequest{Op: sandbox.OpHook, Event: event, Env: pickEnv(env, sandbox.HookEnv)}
	if readErr != nil {
		req.ReadErr = readErr.Error()
	}
	if event == EventPreToolUse || event == EventAssistantGuard {
		req.Landings = containerLandings(data)
	}
	// JSON escaping can grow odd paths and text past the line limit: then
	// the guard goes without the landings (it still resolves the targets
	// itself), and the session context without ATL_CONTEXT.
	if event == EventAssistantGuard && lineLen(req) > sandbox.MaxRequestLine {
		// The assistant's guard does not go without the landings: it fails closed.
		return assistantRefuse(stderr, "", "its targets are too long to check")
	}
	if lineLen(req) > sandbox.MaxRequestLine {
		req.Landings = nil
	}
	if lineLen(req) > sandbox.MaxRequestLine {
		delete(req.Env, envContextVar)
	}
	var out, errb bytes.Buffer
	code, ran, err := sandbox.Ask(env("ATL_CONTROL_SOCK"), req, data, &out, &errb, gateTimeout)
	if err == nil && ran {
		stdout.Write(out.Bytes())
		stderr.Write(errb.Bytes())
		return code
	}
	if err == nil {
		err = fmt.Errorf("the task's gate refused the request: %s", strings.TrimSpace(errb.String()))
	}
	logError(config.Paths{}, stderr, event, "", err)
	if event == EventAssistantGuard {
		// The assistant's guard fails closed, here too.
		return assistantRefuse(stderr, "", "its gate did not answer")
	}
	if event == EventPreToolUse && len(data) > maxInput {
		in, _ := partialInput(data)
		return refuseOversized(stderr, in.ToolName)
	}
	return 0
}

// statusLineViaGate is `atl statusline` in a task container (see viaGate).
// It always exits 0; without a gate it prints nothing.
func statusLineViaGate(stdin io.Reader, stdout, stderr io.Writer, env func(string) string) int {
	data, err := io.ReadAll(io.LimitReader(stdin, maxStatusInput+1))
	if err != nil {
		logError(config.Paths{}, stderr, EventStatusLine, "", fmt.Errorf("read stdin: %w", err))
		return 0
	}
	req := sandbox.ControlRequest{Op: sandbox.OpStatusLine, Env: pickEnv(env, sandbox.StatusLineEnv)}
	var out, errb bytes.Buffer
	_, ran, err := sandbox.Ask(env("ATL_CONTROL_SOCK"), req, data, &out, &errb, gateTimeout)
	if err == nil && ran {
		stdout.Write(out.Bytes())
		stderr.Write(errb.Bytes())
		return 0
	}
	if err == nil {
		err = fmt.Errorf("the task's gate refused the request: %s", strings.TrimSpace(errb.String()))
	}
	logError(config.Paths{}, stderr, EventStatusLine, "", err)
	return 0
}

// lineLen is the length of req's request line (see sandbox.Ask).
func lineLen(req sandbox.ControlRequest) int {
	b, _ := json.Marshal(req)
	return len(b) + 1
}

// pickEnv returns the non-empty values of keys in env, each cut to
// maxEnvValue bytes.
func pickEnv(env func(string) string, keys []string) map[string]string {
	m := map[string]string{}
	for _, k := range keys {
		if v := env(k); v != "" {
			m[k] = truncateBytes(v, maxEnvValue)
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// containerLandings resolves the targets of a PreToolUse input here, in the
// task container, the way the ro guard does (targetForms, resolvePath): a
// symlink that exists only in the container is followed by the guard too.
// The input is read with partialInput, which takes the same keys
// json.Unmarshal would and skips large values without keeping them.
func containerLandings(data []byte) []sandbox.Landing {
	in, ok := partialInput(data)
	if !ok {
		return nil
	}
	targets, _ := guardTargets(in.ToolInput)
	var out []sandbox.Landing
	seen := map[sandbox.Landing]bool{}
	size := 0
	for _, target := range targets {
		forms, _ := targetForms(target, in.Cwd)
		for _, raw := range forms {
			for _, path := range []string{filepath.Clean(raw), raw} {
				r, err := resolvePath(path)
				l := sandbox.Landing{Given: path, Path: r}
				if err != nil || seen[l] || size+len(path)+len(r) > maxLandingBytes {
					continue
				}
				seen[l] = true
				size += len(path) + len(r)
				out = append(out, l)
			}
		}
	}
	return out
}

// TaskServer answers, in a sandboxed task's gate, what atl in the task's
// container asks for: hooks, the status line and the task's notes. It reads
// and writes the rows of its own task only, which it knows by database id:
// a request names no task, and the session ids, Claude session ids,
// working directories and ATL_TASK it carries can only select among that
// task's sessions, never reach another task's (see db.scope). A transcript
// path an event reports is recorded only if it lies in the task's own
// Claude config dir (see transcriptIn).
type TaskServer struct {
	d    db
	slug string
}

var _ sandbox.Task = (*TaskServer)(nil)

// OpenTaskServer opens the database at p.DB (with the hook's short busy
// timeout, once for the gate's lifetime) for task id, which must still have
// the given slug.
func OpenTaskServer(p config.Paths, id int64, slug string) (*TaskServer, error) {
	st, err := store.OpenWithTimeout(p.DB, dbBusyTimeout)
	if err != nil {
		return nil, err
	}
	s := &TaskServer{d: db{st: st, p: p, scope: id, claudeDir: p.ClaudeDir(slug)}, slug: slug}
	if gone, err := s.Gone(context.Background()); err != nil || gone {
		st.Close()
		if err == nil {
			err = fmt.Errorf("task %d is not %s (any more)", id, slug)
		}
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *TaskServer) Close() error { return s.d.st.Close() }

// OnSessionEnd sets f to be told, in the request's goroutine, the id of
// each session of the task a SessionEnd hook is for, whether or not it set
// the session ended (a session atl stopped, or launched again since, keeps
// its state: see store.EndLive). The gate has it end what the session
// leaves running in the container once its Claude is gone
// (sandbox.EndSessionCmd). Claude also reports a SessionEnd for /clear and
// goes on, a killed Claude's can come after a resume has started the next
// one, and the task can send one for any session: f must leave a session
// alone while it runs.
func (s *TaskServer) OnSessionEnd(f func(session int64)) { s.d.ended = f }

// SetROMounts sets the host paths the task has read-only (the gate's
// sandbox.Gate.ROMounts), which its sessions' start context names.
func (s *TaskServer) SetROMounts(paths []string) { s.d.roMounts = paths }

// Gone reports whether the server's task no longer exists (as the id and
// slug it was opened for).
func (s *TaskServer) Gone(ctx context.Context) (bool, error) {
	t, err := s.d.st.GetTaskBrief(ctx, s.d.scope)
	if errors.Is(err, store.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return t.Slug != s.slug, nil
}

// Do answers one request (see sandbox.Task). Fields of req other than the
// op's own are empty (sandbox.Control checks them).
func (s *TaskServer) Do(req sandbox.ControlRequest, body []byte, stdout, stderr io.Writer) (code int) {
	defer func() {
		if r := recover(); r != nil {
			logError(s.d.p, io.Discard, req.Op, "", fmt.Errorf("gate: panic: %v\n%s", r, debug.Stack()))
			code = 0
			if req.Op == sandbox.OpNote || req.Op == sandbox.OpNotes {
				fmt.Fprintln(stderr, "atl: internal error in the task's gate")
				code = 1
			}
		}
	}()
	switch req.Op {
	case sandbox.OpHook:
		return s.hook(req, body, stdout, stderr)
	case sandbox.OpStatusLine:
		s.d.statusLine(body, stdout, stderr, envMap(req.Env, sandbox.StatusLineEnv))
		return 0
	case sandbox.OpNote:
		return s.note(req, body, stdout, stderr)
	case sandbox.OpNotes:
		return s.notes(req, body, stdout, stderr)
	}
	fmt.Fprintf(stderr, "atl: unknown operation %q\n", req.Op)
	return 1
}

func (s *TaskServer) hook(req sandbox.ControlRequest, body []byte, stdout, stderr io.Writer) int {
	if req.Event == EventAssistantGuard {
		// The assistant never runs in a task container.
		return assistantRefuse(stderr, "", "the assistant guard does not run for a task container")
	}
	var readErr error
	if req.ReadErr != "" {
		readErr = errors.New(req.ReadErr)
	}
	var landings []candidate
	for _, l := range req.Landings {
		if filepath.IsAbs(l.Path) && filepath.Clean(l.Path) == l.Path {
			landings = append(landings, candidate{given: l.Given, landing: l.Path})
		}
	}
	// ATL_TASK and the rest of the container's environment are not passed:
	// the scope decides the task.
	env := envMap(req.Env, sandbox.HookEnv)
	return s.d.serve(req.Event, body, readErr, landings, stdout, stderr, env)
}

// envMap is the environment of a request: the values of keys in m, nothing
// else.
func envMap(m map[string]string, keys []string) func(string) string {
	return func(k string) string {
		for _, allowed := range keys {
			if k == allowed {
				return m[k]
			}
		}
		return ""
	}
}

func (s *TaskServer) fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "atl: %v\n", err)
	return 1
}

// note appends body as a note line (see app.AppendNote).
func (s *TaskServer) note(req sandbox.ControlRequest, body []byte, stdout, stderr io.Writer) int {
	text := strings.TrimSpace(model.SanitizeNotes(string(body)))
	if text == "" {
		return s.fail(stderr, errors.New("note is empty"))
	}
	author := req.Author
	if author == "" {
		author = model.AuthorUser
	}
	if !model.ValidAuthor(author) {
		return s.fail(stderr, fmt.Errorf("invalid note author %q (want user or assistant)", author))
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
	defer cancel()
	if _, err := s.d.st.AppendNote(ctx, s.d.scope, author, text); err != nil {
		return s.fail(stderr, err)
	}
	fmt.Fprintln(stdout, "note added")
	return 0
}

// notes prints the notes, or with Set replaces them with body, or with
// Clear alone clears them, printing what `atl notes` prints.
func (s *TaskServer) notes(req sandbox.ControlRequest, body []byte, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
	defer cancel()
	if !req.Set && !req.Clear {
		text, err := s.d.st.GetNotes(ctx, s.d.scope)
		if err != nil {
			return s.fail(stderr, err)
		}
		if text = model.SanitizeNotes(text); text != "" {
			fmt.Fprintln(stdout, text)
		}
		return 0
	}
	text := ""
	if req.Set {
		text = string(body)
		if !req.Clear && model.SanitizeNotes(text) == "" {
			return s.fail(stderr, errors.New("stdin is empty: nothing to set (use --clear to clear the notes)"))
		}
	}
	text, err := s.d.st.ReplaceNotes(ctx, s.d.scope, text)
	if err != nil {
		return s.fail(stderr, err)
	}
	if text == "" {
		fmt.Fprintln(stdout, "notes cleared")
	} else {
		fmt.Fprintf(stdout, "notes replaced (%d bytes)\n", len(text))
	}
	return 0
}
