package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/andreujuanc/atelier/internal/gitx"
	"github.com/andreujuanc/atelier/internal/model"
)

// PRResult is the outcome of PR for one rw repo.
type PRResult struct {
	Repo, URL string
	Skipped   bool
	Reason    string // why it was skipped, or a note (e.g. PR already existed)
}

// prFooter ends every PR body.
const prFooter = "Created by atelier"

// PR opens a pull request (`gh pr create --repo <the pinned origin's>`) for
// each rw repo of the task that has commits on its branch and whose branch
// is on origin; if any PR was created (or already existed) the task moves
// to review. atelier never pushes: a repo whose branch is not on origin is
// skipped with reason "push <branch> first". Per-repo failures (including
// gh's) are reported as skipped results, never abort the other repos.
func (a *App) PR(ctx context.Context, ref string) ([]PRResult, error) {
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return nil, err
	}
	body := prFooter
	if d := strings.TrimSpace(t.Description); d != "" {
		body = d + "\n\n" + prFooter
	}
	var res []PRResult
	var loopErr error
	created := 0
	for _, r := range t.Repos {
		if r.Mode != model.ModeRW {
			continue
		}
		if loopErr = ctx.Err(); loopErr != nil {
			break
		}
		pr := a.prRepo(ctx, t, r, body)
		if !pr.Skipped {
			created++
		}
		res = append(res, pr)
	}
	if created > 0 && t.Status != model.StatusReview {
		// Record PRs already opened even if interrupted afterwards.
		if err := a.Store.SetTaskStatus(context.WithoutCancel(ctx), t.ID, model.StatusReview); err != nil {
			return res, err
		}
	}
	return res, loopErr
}

func (a *App) prRepo(ctx context.Context, t *model.Task, r model.TaskRepo, body string) PRResult {
	skip := func(reason string) PRResult { return PRResult{Repo: r.Name, Skipped: true, Reason: reason} }
	wt, err := a.checkWorktree(ctx, t, r)
	if errors.Is(err, os.ErrNotExist) {
		return skip("worktree missing")
	} else if err != nil {
		return skip(err.Error())
	}
	n, err := gitx.CommitsAhead(ctx, wt, r.BaseCommit)
	if err != nil {
		return skip(err.Error())
	}
	if n == 0 {
		return skip("no commits")
	}
	// The remote check runs in the canonical clone, as push and fetch do.
	clone := a.Paths.RepoPath(r.Name)
	origin, err := a.remoteOrigin(ctx, r.Name, clone, a.out())
	if err != nil {
		return skip(err.Error())
	}
	pushed, err := gitx.RemoteBranchExists(ctx, clone, origin, t.Branch)
	if err != nil {
		return skip(a.noteRefusal(r.Name, "git ls-remote", err).Error())
	}
	if !pushed {
		return skip("push " + t.Branch + " first")
	}
	repo, ok := gitx.GitHubRepo(ctx, clone, origin)
	if !ok {
		return skip(fmt.Sprintf("origin %s is not a GitHub repository URL", origin))
	}

	// gh is told the repository (the pinned origin's) and runs in an empty
	// directory: it picks the repository from the remotes of the repo it
	// runs in, and runs git there (whose config is the task's), otherwise.
	tmp, err := os.MkdirTemp("", "atl-gh-")
	if err != nil {
		return skip(err.Error())
	}
	defer os.RemoveAll(tmp)
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "gh", "pr", "create", "--repo", repo,
		"--head", t.Branch, "--base", r.BaseBranch, "--title", t.Title, "--body", body)
	cmd.Dir = tmp
	cmd.Env = append(gitx.GuardedEnv(), "GH_PROMPT_DISABLED=1", "GIT_CEILING_DIRECTORIES="+filepath.Dir(tmp))
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		combined := stdout.String() + "\n" + stderr.String()
		if strings.Contains(combined, "already exists") {
			res := PRResult{Repo: r.Name, URL: lastURL(combined)}
			if res.URL == "" {
				res.Reason = "PR already exists"
			}
			return res
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if cerr := ctx.Err(); cerr != nil {
			msg = cerr.Error()
		}
		return skip("gh: " + msg)
	}
	return PRResult{Repo: r.Name, URL: lastLine(stdout.String())}
}

// lastLine returns the last non-empty line of s, trimmed.
func lastLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

// lastURL returns the last line of s that is an http(s) URL, or "".
func lastURL(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") {
			return l
		}
	}
	return ""
}

// String formats r for display: the URL, or the reason it was skipped.
func (r PRResult) String() string {
	switch {
	case r.Skipped:
		return fmt.Sprintf("skipped: %s", r.Reason)
	case r.URL != "":
		return r.URL
	default:
		return r.Reason
	}
}
