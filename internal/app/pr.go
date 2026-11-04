package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

// PR opens a pull request (`gh pr create`, run in the worktree) for each rw
// repo of the task that has commits on its branch and whose branch is on
// origin; if any PR was created (or already existed) the task moves to
// review. atelier never pushes: a repo whose branch is not on origin is
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
	wt := a.Paths.WorktreePath(t.Slug, r.Name)
	if _, err := os.Stat(wt); errors.Is(err, os.ErrNotExist) {
		return skip("worktree missing")
	}
	n, err := gitx.CommitsAhead(ctx, wt, r.BaseCommit)
	if err != nil {
		return skip(err.Error())
	}
	if n == 0 {
		return skip("no commits")
	}
	pushed, err := gitx.RemoteBranchExists(ctx, wt, t.Branch)
	if err != nil {
		return skip(err.Error())
	}
	if !pushed {
		return skip("push " + t.Branch + " first")
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "gh", "pr", "create",
		"--head", t.Branch, "--base", r.BaseBranch, "--title", t.Title, "--body", body)
	cmd.Dir = wt
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1")
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
