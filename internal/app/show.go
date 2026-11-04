package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/andreujuanc/atelier/internal/gitx"
	"github.com/andreujuanc/atelier/internal/model"
)

// TaskDetail is everything known about one task (atl show): the task, its
// repos with git state, its sessions and its notes.
type TaskDetail struct {
	Task     model.Task
	TaskDir  string
	Repos    []RepoDetail
	Sessions []model.Session // created_at asc
	Notes    []model.Note    // newest first
}

// RepoDetail is the git state of one repo of a task. For rw repos it
// describes the task worktree; for ro repos the canonical clone (Ahead,
// Unpushed and Pushed are then always zero).
type RepoDetail struct {
	Name string
	Mode model.Mode
	Path string // worktree (rw) or canonical repo (ro)
	// BaseBranch is the origin branch the task branch started from (rw;
	// "" for ro).
	BaseBranch string
	Dirty      bool
	// Ahead is the number of commits on HEAD not in the task's base commit (rw).
	Ahead int
	// LastCommit is HEAD's commit; nil if it could not be read.
	LastCommit *Commit
	// Pushed: the local remote-tracking ref refs/remotes/origin/<branch>
	// exists, i.e. the branch was pushed (as of the last fetch/push; no
	// network is used). rw only.
	Pushed bool
	// Unpushed is the number of commits on the worktree's HEAD not in
	// refs/remotes/origin/<branch> when Pushed, else equal to Ahead (rw;
	// zero when the worktree is missing).
	Unpushed int
	// Err is "missing" if the repo directory is gone (closed tasks have
	// their worktrees removed), else the first git error, or "".
	Err string
}

// Commit identifies a commit.
type Commit struct {
	SHA     string
	Subject string
	Date    time.Time // committer date
}

// Show returns the full detail of a task. Git problems in one repo are
// reported in that repo's Err; only lookup and database errors fail.
func (a *App) Show(ctx context.Context, ref string) (*TaskDetail, error) {
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return nil, err
	}
	sessions, err := a.Store.ListSessions(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	notes, err := a.Store.ListNotes(ctx, t.ID, 0)
	if err != nil {
		return nil, err
	}
	d := &TaskDetail{Task: *t, TaskDir: a.Paths.TaskDir(t.Slug), Sessions: sessions, Notes: notes,
		Repos: make([]RepoDetail, 0, len(t.Repos))}
	for _, r := range t.Repos {
		d.Repos = append(d.Repos, a.repoDetail(ctx, t, r))
	}
	return d, nil
}

func (a *App) repoDetail(ctx context.Context, t *model.Task, r model.TaskRepo) RepoDetail {
	rd := RepoDetail{Name: r.Name, Mode: r.Mode, Path: a.RepoPathFor(t, r), BaseBranch: r.BaseBranch}
	var errs []string
	note := func(err error) {
		if err != nil {
			errs = append(errs, firstLineOf(err.Error()))
		}
	}
	if r.Mode == model.ModeRW {
		// Refs are shared by all worktrees of a repo: ask the canonical
		// clone, so a closed task (worktree removed) still answers.
		pushed, err := gitx.RefExists(ctx, a.Paths.RepoPath(r.Name), "refs/remotes/origin/"+t.Branch)
		rd.Pushed = pushed
		note(err)
	}
	if _, err := os.Stat(rd.Path); errors.Is(err, os.ErrNotExist) {
		rd.Err = "missing"
		return rd
	}
	var err error
	rd.Dirty, err = gitx.IsDirty(ctx, rd.Path)
	note(err)
	if sha, subject, when, err := gitx.LastCommit(ctx, rd.Path); err == nil {
		rd.LastCommit = &Commit{SHA: sha, Subject: subject, Date: when}
	} else {
		note(err)
	}
	if r.Mode == model.ModeRW && r.BaseCommit != "" {
		rd.Ahead, err = gitx.CommitsAhead(ctx, rd.Path, r.BaseCommit)
		note(err)
	}
	if r.Mode == model.ModeRW {
		rd.Unpushed = rd.Ahead
		if rd.Pushed {
			rd.Unpushed, err = gitx.CommitsAhead(ctx, rd.Path, "refs/remotes/origin/"+t.Branch)
			note(err)
		}
	}
	if len(errs) > 0 {
		rd.Err = errs[0]
	}
	return rd
}

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// AddNote adds a note to a task (open or closed). The body is trimmed and
// must not be empty; author is model.AuthorUser ("" means user) or
// model.AuthorAssistant.
func (a *App) AddNote(ctx context.Context, ref, body, author string) (*model.Note, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, errors.New("note is empty")
	}
	if author == "" {
		author = model.AuthorUser
	}
	if !model.ValidAuthor(author) {
		return nil, fmt.Errorf("invalid note author %q (want user or assistant)", author)
	}
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return nil, err
	}
	n := &model.Note{TaskID: t.ID, Body: body, Author: author, CreatedAt: a.now()}
	if err := a.Store.AddNote(ctx, n); err != nil {
		return nil, err
	}
	return n, nil
}

// Notes returns a task's notes, newest first.
func (a *App) Notes(ctx context.Context, ref string) (*model.Task, []model.Note, error) {
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	notes, err := a.Store.ListNotes(ctx, t.ID, 0)
	if err != nil {
		return nil, nil, err
	}
	return t, notes, nil
}
