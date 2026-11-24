# Git

atl never edits the canonical clones' checkouts. Each rw repo of a task is a worktree at
`<task dir>/<repo>` on branch `atl/<slug>`. atl pushes only when asked.

## Base branch

`atl new --base` (TUI form: "auto" or "production patch"), after `git fetch origin` unless
`--no-fetch`:

| value | branch starts from |
|---|---|
| `auto` (default) | `origin/dev`, else `origin/develop`, else origin's default branch |
| `default` or `production` | origin's default branch |
| any other name | `origin/<name>`; must exist in every rw repo or nothing is created |

The choice is stored on the task: repos added later resolve the same way. Each repo records
its base: `atl pr` targets it, `atl diff` compares against the commit it started from,
`atl show` prints it. A branch literally named `auto`, `default` or `production` cannot be
chosen.

## Commits, push and fetch

Commits work in the task container. Your git config is not given to it; only `user.name` and
`user.email` (as `GIT_AUTHOR_*` / `GIT_COMMITTER_*`).

The task has no push/fetch credentials, so plain `git push`, `git fetch` and `gh` fail there.
Sessions use:

```sh
atl push [--repo R] [--force]   # push atl/<slug> of each rw repo to origin (--force-with-lease)
atl fetch [--repo R]            # git fetch origin in each repo (rw and ro)
```

In the container these ask the task's gate, which runs them on the host with your SSH agent,
`~/.ssh/config` and credentials. The request can only name the operation, one of the task's
repos and `--force`; the task is always the gate's own. So an agent can push its own branch,
never `main`, tags or another task's branch. Timeout: 10 minutes.

A push from a task runs without a terminal: an SSH key that needs a typed PIN or passphrase
fails (push from your shell instead).

## atl pr

Run on the host. Per rw repo with pushed commits: `gh pr create --repo <origin's GitHub
repo> --base <the repo's base branch>`, then the task moves to `review`; a done or cancelled
task keeps its status (atl says so), and the assistant cannot run it on one. Push first.

- The PR opens in the pinned origin's repository: with a fork as `origin`, in the fork.
- An ssh alias (`git@github-work:org/api.git`) counts as github.com when `ssh -G` resolves it
  to github.com.
- `gh` runs outside the worktree, so it neither reads the repo's remotes nor runs git there.

## Origin pin

atl pins each repo's origin URLs (fetch and push, before `insteadOf` rewriting) in
`<workspace>/run/origins/<repo>.json` the first time it needs them (`atl new`/`add`, first
container start, first push/fetch). Push and fetch always go to the pin, through a
command-line remote; your own global `insteadOf` rules still apply. `refs/remotes/origin/*`
and `--force-with-lease` work as usual.

Trust on first use: if the repo's config later names another URL, atl uses the pin and
warns. To accept the new URL, on the host:

```bash
atl repos pin <repo>
```

## How atl runs git safely

A repo's config can make git run programs (hooks, `core.fsmonitor`, filters, ssh commands),
and tasks can write that config. atl's own git on the host (status, diff, worktree add/remove,
branch cleanup, push, fetch) defends in three ways:

1. **Disabled on every call:** pager, fsmonitor, hooks, submodule recursion, auto gc, editor,
   promisor lazy fetch, and all transports except for push/fetch/remote checks, which use your
   credential helpers and ssh command, never the repo's. `atl diff` runs no external diff or
   textconv.
2. **Done without git:** the dirty check compares worktree and index itself, so no clean
   filter runs. A converted file (filter, `eol`, `working-tree-encoding`) whose stat changed
   reads as dirty.
3. **Checked and refused** when the repo's own config (`.git/config`, `config.worktree`) has:

| setting | refuses |
|---|---|
| a filter program (except the exact ones `git lfs install` writes) | `atl diff`, checkout in `atl new`/`add` |
| git-lfs settings, `http.*`, URL rewrites, or an origin other than the pin | checkout |
| `url.*.insteadOf` / `pushInsteadOf`; `http.*` for an https origin | push, fetch, remote check |
| `include.path` / `includeIf.*.path` | all of the above |

A refusal names the repo, setting and value, and is logged to `<workspace>/state/refused.log`.
`atl doctor` lists, per repo, what it would refuse now. Fix: remove the setting and rerun.

The same commands, and `atl show`'s last commit, are also refused while git would read another
repository: `.git/objects/info/alternates` (a clone made with `--reference`: run
`git repack -a -d` in it, then remove the file), a symlink in `.git/objects/`, or a `commondir`
file in the clone's `.git`. `atl show` reads HEAD and its branch without following a symlink.

Worktree checks: before running git or a project script in a worktree, atl checks that it is
a real directory (not a symlink) and a worktree of that repo's canonical clone. If not, the
command fails for that repo; `atl done`/`cancel` refuse (with `--force`, close the task and
leave the path alone). Push, fetch and the PR remote check run in the canonical clone.

Limits:

- A task rewriting its config (or git dir) in a tight loop could slip a setting in between the
  check and git's read.
- A partial clone (`clone --filter`) cannot be an rw repo: the checkout fails.
