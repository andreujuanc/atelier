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
	Notes    string          // the task's notes text ("" if none)
	// SessionTitles maps a session id to its conversation title, read from
	// its transcript: Claude Code's title, else the first user prompt.
	// Sessions without one (no or unreadable transcript) are absent.
	SessionTitles map[int64]string
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
	notes, err := a.Store.GetNotes(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	d := &TaskDetail{Task: *t, TaskDir: a.Paths.TaskDir(t.Slug), Sessions: sessions, Notes: notes,
		Repos: make([]RepoDetail, 0, len(t.Repos))}
	for _, r := range t.Repos {
		d.Repos = append(d.Repos, a.repoDetail(ctx, t, r))
	}
	d.SessionTitles = map[int64]string{}
	for _, s := range sessions {
		if s.TranscriptPath == "" {
			continue
		}
		if info, err := readTranscriptInfo(s.TranscriptPath); err == nil {
			if title := info.Title; title != "" {
				d.SessionTitles[s.ID] = title
			} else if info.FirstPrompt != "" {
				d.SessionTitles[s.ID] = info.FirstPrompt
			}
		}
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

// AppendNote appends the line "- YYYY-MM-DD [author] body" to a task's
// notes (open or closed) and returns it. The body is sanitised and trimmed
// and must not be empty; author is model.AuthorUser ("" means user) or
// model.AuthorAssistant.
func (a *App) AppendNote(ctx context.Context, ref, body, author string) (string, error) {
	body = strings.TrimSpace(model.SanitizeNotes(body))
	if body == "" {
		return "", errors.New("note is empty")
	}
	if author == "" {
		author = model.AuthorUser
	}
	if !model.ValidAuthor(author) {
		return "", fmt.Errorf("invalid note author %q (want user or assistant)", author)
	}
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return "", err
	}
	return a.Store.AppendNote(ctx, t.ID, author, body)
}

// Notes returns a task's notes text ("" if none).
func (a *App) Notes(ctx context.Context, ref string) (*model.Task, string, error) {
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return nil, "", err
	}
	text, err := a.Store.GetNotes(ctx, t.ID)
	if err != nil {
		return nil, "", err
	}
	return t, text, nil
}

// SetNotes replaces a task's notes with text if they are still base, the
// text the edit started from (compare-and-set); else it fails with a
// *model.NotesConflictError holding the current text. It returns the text
// as stored (model.SanitizeNotes).
func (a *App) SetNotes(ctx context.Context, ref, text, base string) (string, error) {
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return "", err
	}
	return a.Store.SetNotes(ctx, t.ID, text, base)
}

// ReplaceNotes replaces a task's notes with text unconditionally ("" clears
// them) and returns the text as stored.
func (a *App) ReplaceNotes(ctx context.Context, ref, text string) (string, error) {
	t, err := a.Store.ResolveTask(ctx, ref)
	if err != nil {
		return "", err
	}
	return a.Store.ReplaceNotes(ctx, t.ID, text)
}
