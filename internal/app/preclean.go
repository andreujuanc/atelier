package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/andreujuanc/atelier/internal/learn"
	"github.com/andreujuanc/atelier/internal/sandbox"
	"github.com/andreujuanc/atelier/internal/store"
)

// What became of a task in a pre-clean (PrecleanTask.Status).
const (
	PrecleanCached = "cached" // the stored result was for the same input, prompt, model and effort
	PrecleanRan    = "ran"
	PrecleanFailed = "failed"
	PrecleanEmpty  = "empty" // nothing a preference can come from (learn.HasInput): no call
)

// PrecleanTask is one task's pre-clean.
type PrecleanTask struct {
	TaskExtract
	Status      string
	Occurrences []learn.Occurrence // kept (learn.Check); also from the cache
	Dropped     map[string]int     // by reason (learn.Drop*); a run only
	CostUSD     float64            // of this run's call (also when it failed after the model ran)
	Err         error              // why it failed
}

// precleanTimeout bounds one pre-clean call.
var precleanTimeout = 5 * time.Minute

// learnLockWait bounds how long Preclean waits for another atl learn of
// the workspace (the learner's container is one per workspace).
var learnLockWait = 2 * time.Second

// maxPrecleanOutput bounds what a pre-clean call may print on stdout;
// maxPrecleanStderr what is kept of its stderr.
const (
	maxPrecleanOutput = 8 << 20
	maxPrecleanStderr = 64 << 10
)

// Preclean runs the pre-clean (docs/learning.md, "1b. Pre-clean") of task
// ref, or of every task when ref is "": each task's extract
// (LearnExtracts) goes to one `claude -p` (learn.PrecleanArgs), unless the
// stored digest is for the same learn.CacheKey (cached) or the extract has
// nothing to read (empty). The calls run in the learner (startLearner), at
// most Config.Parallelism at a time, each within precleanTimeout; their
// occurrences are checked (learn.Check) and stored with the cost. A call
// that fails stores nothing, and the digest a task had goes when its call
// fails or it has nothing to read any more. The learner is started only when a task needs
// a call, and removed before Preclean returns, also when ctx is cancelled.
// Runs in a workspace are serialised (state/learn.lock).
func (a *App) Preclean(ctx context.Context, ref string) ([]PrecleanTask, error) {
	unlock, err := a.lockLearn(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	ls := &learnerSession{a: a, ctx: ctx}
	defer ls.close()
	return a.preclean(ctx, ref, ls)
}

// lockLearn takes the workspace's learn lock (state/learn.lock), waiting
// at most learnLockWait; atl learn runs only where atelier runs.
func (a *App) lockLearn(ctx context.Context) (func(), error) {
	if err := outsideSandbox("run atl learn"); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(a.Paths.StateDir, 0o755); err != nil {
		return nil, err
	}
	return lockFile(ctx, a.learnLock(), syscall.LOCK_EX, learnLockWait, "another atl learn runs in this workspace")
}

// learnerSession starts the learner (startLearner) when a call first
// needs it and keeps it for the rest of the run: pre-clean and Analyse
// share it. close removes it.
type learnerSession struct {
	a       *App
	ctx     context.Context
	started bool
	run     learnerRun
	stop    func()
	err     error
}

// get returns how to run a call in the learner, starting it the first
// time; a learner that failed to start fails every get.
func (s *learnerSession) get() (learnerRun, error) {
	if !s.started {
		s.started = true
		s.run, s.stop, s.err = s.a.startLearner(s.ctx)
		if s.err != nil {
			s.err = fmt.Errorf("learn: %w", s.err)
		}
	}
	return s.run, s.err
}

func (s *learnerSession) close() {
	if s.stop != nil {
		s.stop()
		s.stop = nil
	}
}

// preclean is Preclean under the learn lock, with ls's learner.
func (a *App) preclean(ctx context.Context, ref string, ls *learnerSession) ([]PrecleanTask, error) {
	exs, err := a.LearnExtracts(ctx, ref)
	if err != nil {
		return nil, err
	}
	cfg := a.LearnConfig()
	out := make([]PrecleanTask, len(exs))
	var todo []int
	for i, ex := range exs {
		pt := &out[i]
		pt.TaskExtract = ex
		if !learn.HasInput(ex.Extract) {
			pt.Status, pt.Occurrences = PrecleanEmpty, []learn.Occurrence{}
			if err := a.Store.DeleteLearnDigest(ctx, ex.TaskID); err != nil {
				return nil, err
			}
			continue
		}
		occ, ok, err := a.currentDigest(ctx, ex, cfg)
		if err != nil {
			return nil, err
		}
		if ok {
			pt.Status, pt.Occurrences = PrecleanCached, occ
			continue
		}
		todo = append(todo, i)
	}
	if len(todo) == 0 {
		return out, nil
	}
	fmt.Fprintf(a.out(), "learn: pre-clean of %d task(s) with %s (effort %s), %d at a time\n",
		len(todo), cfg.PrecleanModel, cfg.PrecleanEffort, cfg.Parallelism)
	run, err := ls.get()
	if err != nil {
		return nil, err
	}
	argv := learn.PrecleanArgs(cfg.PrecleanModel, cfg.PrecleanEffort, cfg.MaxCallUSD)
	sem := make(chan struct{}, max(cfg.Parallelism, 1))
	var wg sync.WaitGroup
	for _, i := range todo {
		wg.Add(1)
		sem <- struct{}{}
		go func(pt *PrecleanTask) {
			defer wg.Done()
			defer func() { <-sem }()
			a.precleanTask(ctx, run, argv, cfg, pt)
		}(&out[i])
	}
	wg.Wait()
	return out, nil
}

// precleanTask runs pt's pre-clean call and stores its result.
func (a *App) precleanTask(ctx context.Context, run learnerRun, argv []string, cfg learn.Config, pt *PrecleanTask) {
	fail := func(err error) {
		pt.Status, pt.Err = PrecleanFailed, err
		// What it had is for another input, prompt, model or effort.
		if derr := a.Store.DeleteLearnDigest(context.WithoutCancel(ctx), pt.TaskID); derr != nil {
			a.warnf("learn: task %s: drop its old pre-clean: %v", pt.Task.Slug, derr)
		}
	}
	var res learn.PrecleanResult
	if err := learnerCall(ctx, run, argv, learn.PrecleanInput(pt.Extract), func(stdout []byte) (err error) {
		res, err = learn.ParsePreclean(stdout)
		pt.CostUSD = res.CostUSD
		return err
	}); err != nil {
		fail(err)
		return
	}
	pt.Occurrences, pt.Dropped = learn.Check(pt.Extract, res.Occurrences)
	occ, err := json.Marshal(pt.Occurrences)
	if err != nil {
		fail(err)
		return
	}
	if err := a.Store.PutLearnDigest(ctx, store.LearnDigest{
		TaskID: pt.TaskID, InputHash: pt.InputHash, CacheKey: learn.CacheKey(pt.InputHash, cfg.PrecleanModel, cfg.PrecleanEffort),
		OccurrencesJSON: string(occ), CreatedAt: a.now(), CostUSD: res.CostUSD,
	}); err != nil {
		fail(fmt.Errorf("store the result: %w", err))
		return
	}
	pt.Status = PrecleanRan
}

// learnerCall runs the learner call argv (run) with stdin within
// precleanTimeout and reads its stdout with parse, which keeps what it
// needs. The error is ctx's when ctx ended; a timeout; parse's error
// starting "claude: " (the result said it was an error); else the
// call's failure with the end of its stderr, or parse's error.
func learnerCall(ctx context.Context, run learnerRun, argv []string, stdin []byte, parse func(stdout []byte) error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	cctx, cancel := context.WithTimeout(ctx, precleanTimeout)
	defer cancel()
	start := time.Now()
	stdout, stderr, runErr := run(cctx, argv, stdin)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(cctx.Err(), context.DeadlineExceeded), innerTimeout(runErr, time.Since(start)):
		return fmt.Errorf("timed out after %s", precleanTimeout)
	}
	if err := parse(stdout); err != nil {
		if runErr != nil && !strings.HasPrefix(err.Error(), "claude: ") {
			err = runFailure(runErr, stderr)
		}
		return err
	}
	if runErr != nil {
		return runFailure(runErr, stderr)
	}
	return nil
}

// currentDigest returns the occurrences of ex's task's stored pre-clean
// if it is current: made from this extract with this prompt, model and
// effort (learn.CacheKey). ok is false when there is none such.
func (a *App) currentDigest(ctx context.Context, ex TaskExtract, cfg learn.Config) (occ []learn.Occurrence, ok bool, err error) {
	d, ok, err := a.Store.GetLearnDigest(ctx, ex.TaskID)
	if err != nil || !ok || d.CacheKey != learn.CacheKey(ex.InputHash, cfg.PrecleanModel, cfg.PrecleanEffort) {
		return nil, false, err
	}
	if err := json.Unmarshal([]byte(d.OccurrencesJSON), &occ); err != nil {
		return nil, false, nil
	}
	if occ == nil {
		occ = []learn.Occurrence{}
	}
	return occ, true, nil
}

// LearnDigests returns, by task id, the occurrences of the current
// pre-cleans (currentDigest) of task ref, or of every task when ref is "":
// what Analyse reads. A task whose pre-clean is missing or stale (its
// input, the prompt, model or effort changed since) is left out; Preclean
// makes it current.
func (a *App) LearnDigests(ctx context.Context, ref string) (map[int64][]learn.Occurrence, error) {
	exs, err := a.LearnExtracts(ctx, ref)
	if err != nil {
		return nil, err
	}
	cfg := a.LearnConfig()
	out := map[int64][]learn.Occurrence{}
	for _, ex := range exs {
		if !learn.HasInput(ex.Extract) {
			continue
		}
		occ, ok, err := a.currentDigest(ctx, ex, cfg)
		if err != nil {
			return nil, err
		}
		if ok {
			out[ex.TaskID] = occ
		}
	}
	return out, nil
}

// runFailure is a failed call's error with the last line of its stderr.
func runFailure(err error, stderr []byte) error {
	lines := strings.Split(strings.TrimSpace(string(stderr)), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		if len(last) > 300 {
			last = last[:300] + "…"
		}
		return fmt.Errorf("%v: %s", err, last)
	}
	return err
}

// learnLock serialises atl learn runs of the workspace: in the state dir,
// which no container sees.
func (a *App) learnLock() string { return filepath.Join(a.Paths.StateDir, "learn.lock") }

// learnerRun runs the claude command argv in the learner with stdin and
// returns its stdout (at most maxPrecleanOutput) and the end of its stderr.
type learnerRun func(ctx context.Context, argv []string, stdin []byte) (stdout, stderr []byte, err error)

// startLearner readies where pre-clean calls run and returns how to run
// one there and how to tear it down (stop, which ignores ctx's end).
//
// With the sandbox on, that is the learner's container (learnerSpec),
// made for this run (one left by a run that died is removed first), its
// Claude dir with none of the user's setup (seedLearnerClaudeDir), and
// removed by stop (looked for once more after an interrupt); each call
// runs there under timeout(1) (learnerTimeoutArgv). It needs Docker, the
// task image (never pulled) and a Claude Code token, which each call gets
// by name (docker exec -e). With
// the sandbox off ($ATL_SANDBOX=off), as the assistant there: claude from
// the host, with the user's Claude config, in a new empty dir that stop
// removes.
func (a *App) startLearner(ctx context.Context) (learnerRun, func(), error) {
	if a.Sandbox == nil {
		if _, err := exec.LookPath("claude"); err != nil {
			return nil, nil, errors.New("claude is not on PATH")
		}
		dir, err := os.MkdirTemp("", "atl-learn-")
		if err != nil {
			return nil, nil, err
		}
		run := func(ctx context.Context, argv []string, stdin []byte) ([]byte, []byte, error) {
			return runPiped(ctx, argv, dir, nil, stdin)
		}
		return run, func() { os.RemoveAll(dir) }, nil
	}
	if _, err := a.checkDocker(ctx); err != nil {
		return nil, nil, err
	}
	auth, err := a.sandboxAuth()
	if err != nil {
		return nil, nil, err
	}
	if err := a.removeLearnerContainer(ctx); err != nil {
		return nil, nil, fmt.Errorf("remove the learner's container of an earlier run: %w", err)
	}
	stop := func() {
		err := a.removeLearnerContainer(ctx)
		if ctx.Err() != nil {
			// Interrupted: a docker run ended meanwhile may still have
			// made the container; look for it once more.
			time.Sleep(learnerRetryWait)
			err = errors.Join(err, a.removeLearnerContainer(ctx))
		}
		if err != nil {
			a.warnf("learn: remove the learner's container %s: %v", sandbox.ContainerName(a.learnerID()), err)
		}
	}
	spec, err := a.learnerSpec()
	if err != nil {
		return nil, nil, err
	}
	fresh := func() error {
		if missing, err := sandbox.ImageMissing(ctx, a.docker(), spec.Image); err == nil && missing {
			return unpulledImageMissing(spec.Image, "atl learn")
		}
		return a.seedLearnerClaudeDir(a.Paths.LearnerClaudeDir(), spec.Workdir)
	}
	container, _, err := sandbox.Ensure(ctx, a.docker(), spec, nil, fresh)
	if err != nil {
		stop()
		if errors.Is(err, sandbox.ErrNotAgent) {
			return nil, nil, err
		}
		if missing, ierr := sandbox.ImageMissing(ctx, a.docker(), spec.Image); ierr == nil && missing {
			return nil, nil, unpulledImageMissing(spec.Image, "atl learn")
		}
		return nil, nil, fmt.Errorf("the learner's container (image %s): %w", spec.Image, err)
	}
	keys := make([]string, 0, len(auth))
	env := os.Environ()
	for k, v := range auth {
		keys = append(keys, k)
		env = append(env, k+"="+v)
	}
	sort.Strings(keys)
	run := func(ctx context.Context, argv []string, stdin []byte) ([]byte, []byte, error) {
		return runPiped(ctx, sandbox.PipeExecArgv(container, keys, learnerTimeoutArgv(argv)), "", env, stdin)
	}
	return run, stop, nil
}

// learnerRetryWait is how long an interrupted run waits before it looks
// for its container once more (see startLearner).
var learnerRetryWait = 2 * time.Second

// learnerLimit is how long timeout(1) lets a call run in the learner's
// container: 10 s less than precleanTimeout, at least 1 s.
func learnerLimit() time.Duration {
	return max((precleanTimeout - 10*time.Second).Truncate(time.Second), time.Second)
}

// innerTimeout reports whether a call that failed with err after
// elapsed was ended by timeout(1) in the container (learnerTimeoutArgv):
// it ran past learnerLimit, or ended with timeout's status (124, or
// 137 or 143 for KILL or TERM) within a second of it.
func innerTimeout(err error, elapsed time.Duration) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	limit := learnerLimit()
	switch ee.ExitCode() {
	case 124, 137, 143:
		return elapsed >= limit-time.Second
	}
	return elapsed >= limit
}

// learnerTimeoutArgv wraps argv, run in the learner's container, in
// timeout(1) where the image has it: ended (TERM, KILL 10 s later) 10 s
// before precleanTimeout (at least 1 s), so a call atl gives up on does
// not run on in the container, spending, next to the next one. Without
// timeout(1), argv runs as it is (the container goes at the run's end).
func learnerTimeoutArgv(argv []string) []string {
	secs := int(learnerLimit() / time.Second)
	return append([]string{"sh", "-c", learnerTimeoutScript, strconv.Itoa(secs)}, argv...)
}

// learnerTimeoutScript runs "$@" under timeout(1) for $0 seconds, if the
// image has timeout(1) (plain seconds: GNU and busybox read them alike).
const learnerTimeoutScript = `if command -v timeout > /dev/null 2>&1; then exec timeout -k 10 "$0" "$@"; fi; exec "$@"`

// learnerWorkdir is the learner's working directory in its container: an
// empty one docker makes, no mount.
const learnerWorkdir = "/tmp/atl-learner"

// learnerID names the learner's sandbox (sandbox.LearnerID).
func (a *App) learnerID() string { return sandbox.LearnerID(a.Paths.LearnerClaudeDir()) }

// learnerSpec describes the learner's container: from the workspace's task
// image ($ATL_SANDBOX_IMAGE, else the default; never a project's), never
// pulled. It mounts its own Claude config dir (Paths.LearnerClaudeDir, as
// $CLAUDE_CONFIG_DIR) and nothing else: no task dir, repo, run dir (no
// gate, no atl binary, no Docker), other part of the workspace, nor
// $ATL_SANDBOX_MOUNTS. It works in learnerWorkdir, an empty dir. What it
// reads comes on stdin, what it says goes to stdout.
func (a *App) learnerSpec() (sandbox.Spec, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return sandbox.Spec{}, err
	}
	claude := a.Paths.LearnerClaudeDir()
	if err := os.MkdirAll(claude, 0o700); err != nil {
		return sandbox.Spec{}, err
	}
	id := a.learnerID()
	spec := sandbox.Spec{
		ID:      id,
		Image:   a.Sandbox.Image,
		User:    sandboxUser(),
		Workdir: learnerWorkdir,
		NoPull:  true,
		Env: map[string]string{
			"HOME":              home,
			sandbox.InEnv:       id,
			"CLAUDE_CONFIG_DIR": claude,
		},
		Mounts: []sandbox.Mount{{Src: claude, Dst: claude}},
	}
	for _, k := range []string{"LANG", "LC_ALL", "TZ"} {
		if v := os.Getenv(k); v != "" {
			spec.Env[k] = v
		}
	}
	return spec, nil
}

// removeLearnerContainer removes the learner's container (only atl's own,
// see sandbox.Agent); one that is not there is fine. It runs on even when
// ctx has ended (an interrupted atl learn).
func (a *App) removeLearnerContainer(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	c, err := sandbox.Agent(ctx, a.docker(), a.learnerID())
	if sandbox.IsNoSuch(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := a.docker()(ctx, "rm", "-f", c.ID); err != nil && !sandbox.IsNoSuch(err) {
		return err
	}
	return nil
}

// runPiped runs argv in dir ("" for atl's) with env (nil for atl's), stdin
// as its input, and returns its stdout (an error past maxPrecleanOutput)
// and the last maxPrecleanStderr bytes of its stderr. ctx's end kills it.
func runPiped(ctx context.Context, argv []string, dir string, env []string, stdin []byte) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdin = bytes.NewReader(stdin)
	stdout := &capWriter{max: maxPrecleanOutput}
	stderr := &tailWriter{max: maxPrecleanStderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if err == nil && stdout.over {
		err = fmt.Errorf("more than %d bytes of output", maxPrecleanOutput)
	}
	return stdout.buf.Bytes(), stderr.buf, err
}

// capWriter keeps the first max bytes written to it and notes more.
type capWriter struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); len(p) > room {
		w.buf.Write(p[:max(room, 0)])
		w.over = true
		return len(p), nil
	}
	return w.buf.Write(p)
}

// tailWriter keeps the last max bytes written to it.
type tailWriter struct {
	buf []byte
	max int
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = append([]byte(nil), w.buf[len(w.buf)-w.max:]...)
	}
	return len(p), nil
}
