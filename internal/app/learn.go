package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/learn"
	"github.com/andreujuanc/atelier/internal/model"
)

// LearnConfig is the workspace's learning settings: atelier.toml's [learn]
// over learn.Defaults.
func (a *App) LearnConfig() learn.Config {
	if a.Workspace != nil && a.Workspace.Config.Learn != nil {
		return *a.Workspace.Config.Learn
	}
	return learn.Defaults()
}

// TaskExtract is one task's learning extract, with what could not be read
// for it (a transcript or input history that could not be opened or read).
type TaskExtract struct {
	learn.Extract
	Warnings []string
}

// LearnExtracts builds the learning extract (learn.Build) of task ref, or
// of every task in the workspace, open or closed, when ref is "" (by id).
// Prompts come from every transcript file each session has had
// (session_transcripts, and transcript_path), each read once, opened as
// openTranscript does, with Claude Code's input history (history.jsonl
// in the same Claude dir) for prompts with pasted content. A file that
// cannot be opened or read is a warning, not an error.
func (a *App) LearnExtracts(ctx context.Context, ref string) ([]TaskExtract, error) {
	var tasks []model.Task
	if ref != "" {
		t, err := a.Store.ResolveTask(ctx, ref)
		if err != nil {
			return nil, err
		}
		tasks = []model.Task{*t}
	} else {
		var err error
		if tasks, err = a.Store.ListTasks(ctx); err != nil {
			return nil, err
		}
		sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	}
	ids := make([]int64, len(tasks))
	for i, t := range tasks {
		ids[i] = t.ID
	}
	sessions, err := a.Store.ListSessionsByTask(ctx, ids)
	if err != nil {
		return nil, err
	}
	var sessIDs []int64
	for _, ss := range sessions {
		for _, s := range ss {
			sessIDs = append(sessIDs, s.ID)
		}
	}
	paths, err := a.Store.ListSessionTranscripts(ctx, sessIDs)
	if err != nil {
		return nil, err
	}
	cfg := a.LearnConfig()
	out := make([]TaskExtract, 0, len(tasks))
	for i := range tasks {
		t := &tasks[i]
		te, err := a.taskExtract(ctx, t, sessions[t.ID], paths, cfg)
		if err != nil {
			return nil, err
		}
		out = append(out, te)
	}
	return out, nil
}

// learnFile is one transcript file of a task and the first session
// that had it.
type learnFile struct {
	session int64
	path    string
}

func (a *App) taskExtract(ctx context.Context, t *model.Task, sessions []model.Session, paths map[int64][]string, cfg learn.Config) (TaskExtract, error) {
	var te TaskExtract
	warn := func(format string, args ...any) {
		te.Warnings = append(te.Warnings, fmt.Sprintf("task %s, ", t.Slug)+fmt.Sprintf(format, args...))
	}
	var files []learnFile
	seen := map[string]bool{}
	claudeIDs := map[string]bool{} // Claude session ids: the files' names
	roots := map[string]bool{}     // the Claude dirs they are in
	for _, s := range sessions {
		for _, p := range append(paths[s.ID], s.TranscriptPath) {
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			files = append(files, learnFile{s.ID, p})
			claudeIDs[strings.TrimSuffix(filepath.Base(p), ".jsonl")] = true
			if r := a.claudeRootOf(t, p); r != "" {
				roots[r] = true
			}
		}
	}
	pastes := &learn.Pastes{}
	rootList := make([]string, 0, len(roots))
	for r := range roots {
		rootList = append(rootList, r)
	}
	sort.Strings(rootList)
	for _, r := range rootList {
		f, err := a.openTranscript(t, filepath.Join(r, "history.jsonl"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			warn("input history: %v", err)
			continue
		}
		err = learn.ReadHistory(f, claudeIDs, pastes)
		f.Close()
		if err != nil {
			warn("input history: %v", err)
		}
	}
	c := learn.NewCollector(cfg, pastes)
	for _, tf := range files {
		f, err := a.openTranscript(t, tf.path)
		if err != nil {
			warn("session %d: transcript: %v", tf.session, err)
			continue
		}
		err = c.Parse(f, tf.session)
		f.Close()
		if err != nil {
			warn("session %d: transcript %s: %v", tf.session, tf.path, err)
		}
	}
	blame, err := a.Store.NotesBlame(ctx, t.ID)
	if err != nil {
		return te, err
	}
	prompts, dropped := c.Prompts()
	te.Extract = learn.Build(learn.TaskInfoOf(t), prompts, dropped, blame, cfg)
	return te, nil
}

// claudeRootOf returns the Claude dir (the task's or the user's, as
// openTranscript allows) that path is in, "" if neither.
func (a *App) claudeRootOf(t *model.Task, path string) string {
	for _, root := range []string{a.Paths.ClaudeDir(t.Slug), config.ClaudeConfigDir()} {
		if root == "" {
			continue
		}
		forms := []string{root}
		if real, err := filepath.EvalSymlinks(root); err == nil {
			forms = append(forms, real)
		}
		for _, form := range forms {
			rel, err := filepath.Rel(form, path)
			if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return root
			}
		}
	}
	return ""
}
