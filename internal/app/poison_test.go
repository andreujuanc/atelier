package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/sandbox"
)

// markerDir collects the markers planted programs leave when they run.
type markerDir string

// prog writes an executable that leaves the marker name and exits.
func (m markerDir) prog(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(filepath.Dir(string(m)), "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\ntouch '"+filepath.Join(string(m), name)+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// ran returns and clears the markers left so far.
func (m markerDir) ran(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(string(m))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
		os.Remove(filepath.Join(string(m), e.Name()))
	}
	return out
}

var poisonHooks = []string{"pre-commit", "post-checkout", "post-merge", "pre-push", "pre-receive", "update", "post-receive",
	"post-update", "reference-transaction", "push-to-checkout", "pre-auto-gc", "post-rewrite", "post-index-change"}

// poisonRepo plants, in the canonical clone at repo (shared by the task
// worktrees), what a task could write to make atl's git run a program or go
// to decoy. With refusing, also what atl can only refuse.
func poisonRepo(t *testing.T, m markerDir, repo, decoy string, refusing bool) {
	t.Helper()
	base := filepath.Base(repo)
	hooks := filepath.Join(repo, ".git", "hooks")
	for _, h := range poisonHooks {
		p := m.prog(t, base+"-hook-"+h)
		if err := os.Rename(p, filepath.Join(hooks, h)); err != nil {
			t.Fatal(err)
		}
		git(t, repo, "config", "hook.evil-"+h+".command", m.prog(t, base+"-cfghook-"+h))
		git(t, repo, "config", "hook.evil-"+h+".event", h)
	}
	kv := []string{
		"core.fsmonitor", m.prog(t, base+"-fsmonitor"),
		"core.sshCommand", m.prog(t, base+"-ssh"),
		"core.askPass", m.prog(t, base+"-askpass"),
		"core.gitProxy", m.prog(t, base+"-git-proxy"),
		"core.pager", m.prog(t, base+"-pager"),
		"core.editor", m.prog(t, base+"-editor"),
		"core.alternateRefsCommand", m.prog(t, base+"-alternate-refs"),
		"credential.helper", m.prog(t, base+"-credential"),
		"diff.external", m.prog(t, base+"-diff-external"),
		"diff.evil.command", m.prog(t, base+"-diff-command"),
		"diff.evil.textconv", m.prog(t, base+"-textconv"),
		"gpg.program", m.prog(t, base+"-gpg"),
		"log.showSignature", "true",
		"submodule.recurse", "true",
		"status.submoduleSummary", "true",
		"fetch.bundleURI", "file://" + filepath.Join(string(m), base+"-bundle-uri"),
		"trace2.normalTarget", filepath.Join(string(m), base+"-trace2"),
		"protocol.allow", "always",
		"remote.origin.uploadpack", m.prog(t, base+"-upload-pack") + "; git-upload-pack",
		"remote.origin.receivepack", m.prog(t, base+"-receive-pack") + "; git-receive-pack",
		"remote.atl.url", decoy,
		"remote.pushDefault", "atl",
		"gc.auto", "1",
	}
	if refusing {
		kv = append(kv,
			"filter.evil.clean", m.prog(t, base+"-filter-clean"),
			"filter.evil.smudge", m.prog(t, base+"-filter-smudge"),
			"url."+decoy+".pushInsteadOf", "https://github.com/o/"+base+".git",
		)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		git(t, repo, "config", "--add", kv[i], kv[i+1])
	}
	writeAttr := filepath.Join(repo, ".git", "info", "attributes")
	if err := os.MkdirAll(filepath.Dir(writeAttr), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(writeAttr, []byte("* filter=evil diff=evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPoisonedRepoApp: every app operation that runs git in a repo, against
// clones a task poisoned: nothing planted runs, and what atl refuses it
// says, warns about and logs where no task sees it.
func TestPoisonedRepoApp(t *testing.T) {
	for _, refusing := range []bool{false, true} {
		t.Run(map[bool]string{false: "overridable", true: "refusing"}[refusing], func(t *testing.T) {
			testPoisonedRepoApp(t, refusing)
		})
	}
}

func testPoisonedRepoApp(t *testing.T, refusing bool) {
	e := newTaskEnv(t)
	ctx := context.Background()
	gh := installGH(t)
	for _, r := range []string{"api", "lib", "docs"} {
		e.addRepo(t, r, "main")
	}
	e.githubOrigins(t, "api", "lib", "docs")
	m := markerDir(filepath.Join(e.root, "markers"))
	if err := os.MkdirAll(string(m), 0o755); err != nil {
		t.Fatal(err)
	}
	task, err := e.app.NewTask(ctx, NewTaskOpts{Title: "Poison", Repos: []RepoArg{{"api", model.ModeRW}, {"docs", model.ModeRO}}})
	if err != nil {
		t.Fatal(err)
	}
	api := e.app.Paths.WorktreePath(task.Slug, "api")
	commitIn(t, api, "work.txt", "work\n")
	git(t, api, "push", "-q", "origin", task.Branch)
	if err := os.WriteFile(filepath.Join(api, "README"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(api, ".gitattributes"), []byte("* filter=evil diff=evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"api", "lib", "docs"} {
		poisonRepo(t, m, e.canonical(r), e.decoy(t, r), refusing)
	}
	e.app.Out.(*bytes.Buffer).Reset()
	refusedLog := e.app.Paths.RefusalLog()

	type op struct {
		name   string
		run    func() error
		refuse string // with refusing: the error must name this key; "" → must succeed
	}
	var other *model.Task
	ops := []op{
		{"Get (status)", func() error {
			v, err := e.app.Get(ctx, task.Slug)
			if err == nil && (!v.RepoStatus[0].Dirty || v.RepoStatus[0].Err != "") {
				err = errors.New("api not dirty: " + v.RepoStatus[0].Err)
			}
			return err
		}, ""},
		{"Show", func() error {
			d, err := e.app.Show(ctx, task.Slug)
			if err == nil && (!d.Repos[0].Dirty || d.Repos[0].Err != "" || d.Repos[1].Err != "" || !d.Repos[0].Pushed) {
				err = errors.New("show: " + d.Repos[0].Err + d.Repos[1].Err)
			}
			return err
		}, ""},
		{"DirtyTasks", func() error {
			d, err := e.app.DirtyTasks(ctx)
			if err == nil && !d[task.ID] {
				err = errors.New("not dirty")
			}
			return err
		}, ""},
		{"AvailableRepos", func() error { _, err := e.app.AvailableRepos(ctx); return err }, ""},
		{"Diff", func() error {
			var b bytes.Buffer
			err := e.app.Diff(ctx, task.Slug, &b, false)
			if err == nil && !strings.Contains(b.String(), "+work") {
				err = errors.New("diff lacks the work: " + b.String())
			}
			return err
		}, "filter.evil."},
		{"Fetch", func() error { return firstErr(e.app.Fetch(ctx, task.Slug, FetchOpts{}, nil)) }, ""},
		{"Push", func() error { return firstErr(e.app.Push(ctx, task.Slug, PushOpts{}, nil)) }, "url."},
		{"PR", func() error {
			res, err := e.app.PR(ctx, task.Slug)
			if err == nil && res[0].Skipped {
				err = errors.New(res[0].Reason)
			}
			return err
		}, ""},
		{"sandboxSpec (git config)", func() error {
			e.app.Sandbox = &sandbox.Config{Image: "x"}
			defer func() { e.app.Sandbox = nil }()
			_, err := e.app.sandboxSpec(ctx, task)
			return err
		}, ""},
		{"pinOrigins", func() error { return e.app.pinOrigins(ctx, task) }, ""},
		{"RepinOrigin", func() error { _, _, _, err := e.app.RepinOrigin(ctx, "docs"); return err }, ""},
		{"AddRepo (fetch, worktree add)", func() error {
			_, err := e.app.AddRepo(ctx, task.Slug, RepoArg{"lib", model.ModeRW}, false)
			return err
		}, "filter.evil."},
		{"NewTask (fetch, worktree add)", func() error {
			var err error
			other, err = e.app.NewTask(ctx, NewTaskOpts{Title: "Other", Repos: []RepoArg{{"api", model.ModeRW}}})
			return err
		}, "filter.evil."},
		{"Doctor", func() error {
			for _, c := range e.app.Doctor(ctx) {
				if strings.HasPrefix(c.Name, "repo ") && c.Warn && !refusing {
					return errors.New("doctor warns about a repo: " + c.Detail)
				}
			}
			return nil
		}, ""},
		{"Done without force (changes)", func() error {
			if _, err := e.app.Done(ctx, task.Slug, false); err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
				return errors.New("done without force: " + errorString(err))
			}
			return nil
		}, ""},
		{"Cancel (clean)", func() error {
			if other == nil {
				return nil
			}
			_, err := e.app.Cancel(ctx, other.Slug, false)
			return err
		}, ""},
		{"Done --force", func() error { _, err := e.app.Done(ctx, task.Slug, true); return err }, ""},
		// The pushed branch was deleted: it is made again from origin's.
		{"Reopen (fetch, worktree add)", func() error {
			return e.app.SetStatus(ctx, task.Slug, model.StatusActive)
		}, "filter.evil."},
	}
	// With refusing: Diff refuses (filter); fetches are fine (pushInsteadOf
	// only rewrites push URLs, but -c cannot remove it: every transport
	// refuses), so Fetch, Push, PR and the fetch of AddRepo/NewTask refuse
	// with the URL rule.
	if refusing {
		for i := range ops {
			switch ops[i].name {
			case "Fetch", "Push", "AddRepo (fetch, worktree add)", "NewTask (fetch, worktree add)":
				ops[i].refuse = "url."
			case "PR":
				ops[i].refuse = "url."
			}
		}
	}
	for _, o := range ops {
		err := o.run()
		if ran := m.ran(t); len(ran) > 0 {
			t.Errorf("%s ran %v", o.name, ran)
		}
		switch {
		case refusing && o.refuse != "":
			if err == nil || !strings.Contains(err.Error(), "refused") || !strings.Contains(err.Error(), o.refuse) {
				t.Errorf("%s: %v; want a refusal naming %s", o.name, err, o.refuse)
			}
		case err != nil:
			t.Errorf("%s: %v", o.name, err)
		}
	}
	if gh.calls() == 0 && !refusing {
		t.Error("gh never ran")
	}
	log, _ := os.ReadFile(refusedLog)
	out := e.outString()
	if !refusing {
		if len(log) > 0 || strings.Contains(out, "did not run") {
			t.Errorf("refusals without refusing settings:\n%s\n%s", log, out)
		}
		return
	}
	// The user hears of it: a warning, a log line, doctor.
	for _, want := range []string{"refused git diff in ", "filter.evil.clean = ", "refused git fetch in ", "url."} {
		if !strings.Contains(string(log), want) {
			t.Errorf("refusal log lacks %q:\n%s", want, log)
		}
	}
	if !strings.Contains(out, "warning: repo api: atl did not run git diff: its own config (local) has filter.evil.clean = ") ||
		!strings.Contains(out, refusedLog) {
		t.Errorf("no warning:\n%s", out)
	}
	found := false
	for _, c := range e.app.Doctor(ctx) {
		if c.Name == "repo api" && c.Warn && strings.Contains(c.Detail, "filter.evil.clean") && strings.Contains(c.Detail, "atl diff") &&
			strings.Contains(c.Detail, "pushinsteadof") {
			found = true
		}
	}
	if !found {
		t.Errorf("doctor does not list api's settings: %+v", e.app.Doctor(ctx))
	}
}

func firstErr(res []RemoteResult, err error) error {
	if err != nil {
		return err
	}
	for _, r := range res {
		if r.Err != nil {
			return r.Err
		}
	}
	return nil
}

func errorString(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}
