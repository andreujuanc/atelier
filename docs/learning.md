# Learning module (design)

Status: design, not built. Task `atelier-learning-module`.

`atl learn` reads what happened across a workspace's tasks (your prompts and the tasks'
notes) and proposes **learnings**: cross-task preferences about how agents should work
("no prose", "don't open PRs without asking"). The ones you accept are added to every
session's start context.

## Decisions

| | |
|---|---|
| trigger | manual: `atl learn` / a TUI key. No automatic runs for now |
| scope | per workspace only. Nothing is shared between workspaces |
| repo-specific learnings | not a concept: tasks are multi-repo. The only rule is spread across tasks |
| models | cheap models only, effort low or medium (pre-clean: Haiku, low; analyse: Haiku or Sonnet, medium). Configurable in `atelier.toml` |
| applying | never automatic: a learning is injected only after you accept it |
| who computes scores | atl (Go, deterministic). Models only extract and group |

## Pipeline

```
atl learn (host)
  1a. Static extract        no AI, per task
  1b. Pre-clean             AI, per task, cached        → occurrences
  2.  Analyse               AI, once per run            → occurrences grouped into learnings
  3.  Score + validate      atl                         → proposed learnings
  4.  Review                you: accept / edit / reject
  5.  Deliver               SessionStart context of task sessions and the assistant
```

### 1a. Static extract (no AI)

Per task, from its sessions' transcripts (`sessions.transcript_path`, parsed with
`ParseTranscript`) and its notes:

- your prompts only: no tool output, no pasted blocks, no injected context (hook output,
  system reminders), no slash-command noise; each turn capped (e.g. 2 KiB);
- the last 200 characters of Claude's reply before each prompt, so a bare "no, shorter"
  can be understood (pre-clean reads it; it never reaches Analyse);
- the task's notes, each line with its author from `NotesBlame` (below) and date;
- task metadata: title, status, repos.

Closed tasks count: closing keeps their transcripts (`pruneClaudeDir` keeps `projects/`)
and their notes stay in the database.

`atl learn --dry-run` prints each task's extract and its size and calls no model. Use it to
tune the extract and measure the input before spending tokens.

### 1b. Pre-clean (AI, per task)

One headless `claude -p` per task, with a JSON schema for the output, run in the learner
container (below), a few at a time (about 3). Output: **occurrences**, not conclusions.

| field | |
|---|---|
| `at` | timestamp of the transcript turn, or the date of the note line |
| `task`, `session` | where it came from (`session` empty for a note) |
| `strength` | `strong` ("never", "stop doing this", repeated frustration), `normal` (a plain correction), `mild` (a hint) |
| `polarity` | `reinforce` or `contradict` |
| `text` | the preference in a few words |
| `quote` | verbatim from the extract |

atl checks that every `quote` appears in that task's extract and drops any occurrence
whose quote does not.

Cache: `learn_digests(task_id, input_hash, occurrences, created_at)`. `input_hash` covers
the extract (transcript turns and the notes text; notes are edited in place, so a hash,
not an offset). Pre-clean runs again only for tasks whose input changed.

Notes lines not written by you (see [Prerequisite](#prerequisite-who-wrote-each-notes-line))
are context about what the task was doing, never your preferences: the prompt says so,
and occurrences quoting them are dropped.

### 2. Analyse (AI, once)

Input: every task's occurrences, plus the current learnings (accepted, proposed,
rejected) with their IDs. Output: each occurrence assigned to an existing learning or
to a new one (with its text). Rejected learnings are visible so the same thing is not
proposed again under another name. Analyse does no scoring.

### 3. Score (atl)

Each occurrence is worth `strength × repeat × decay`:

- **strength:** strong 2, normal 1, mild 0.5.
- **repeat:** within one session, the k-th occurrence for the same learning counts 1/k
  (4 times in one session ≈ 2.1). Separate sessions and tasks start again at 1.
- **decay:** `0.5^(age / half-life)`, half-life 45 days for proposed learnings. Each
  occurrence decays on its own, so a fresh one refreshes the score.
- **polarity:** `contradict` occurrences subtract.

The score is computed when read, from the occurrence rows and the current time: no
periodic decay job, and changing weights or half-life re-scores everything.

Examples (normal strength, recent):

| situation | score |
|---|---|
| once, one session | 1.0 |
| 4 times, one session | ~2.1 |
| once each in 3 tasks | 3.0 |
| 2–4 times each in 3 tasks | ~5–6 |
| once each in 3 tasks, 3 months ago, nothing since | ~0.75 |

**Proposed** when score ≥ 3 **and** its occurrences span at least 2 distinct tasks. A
single session never becomes a workspace rule, however often it repeats something.

#### Accepted learnings

Once a learning is injected, agents follow it and you stop repeating it. Plain decay would
remove a rule that works. So, for accepted learnings:

- silence does not count against them: half-life 1 year (or none);
- contradictions do: below a threshold the learning goes back to review as "possibly
  outdated". It is never dropped without you;
- new reinforcing occurrences while it is injected mean agents still get it wrong:
  review shows "still being corrected" (maybe reword it).

### 4. Review

`proposed` → `accepted` | `rejected`. You can edit the text before or after accepting.
Rejected learnings stay (so they are not proposed again) and can be deleted.

Review shows per learning: text, score, trend (rising / fading), distinct tasks, latest
occurrence, and the quotes.

### 5. Delivery

A "User preferences" section in the SessionStart context (`internal/hook/context.go`),
with its own budget (about 800 bytes) on top of the existing 2500 bytes. Accepted
learnings go in by score, highest first; what does not fit is left out. The hook runs on
startup, resume, `/clear` and compaction, so open tasks pick changes up at their next
session event; no files are rewritten. The assistant gets the same section.
`~/.claude` is never written.

Optional, later: firmer wording for high scores ("Never …") than for low ones
("Prefer …").

## The learner container

Like the assistant's: one per workspace, from the workspace's task image, never pulled,
logged in with the workspace token. It sees only its own dir, where atl writes the
extracts and reads the results. It does not see task dirs, worktrees, repos or
`[sandbox] mounts`. Its gate refuses every Docker request and every `atl` command. It
runs `claude -p` and exits; there is no tmux window.

## Safety

The learner is the one place where one task can affect every other task: its output is
added to every session. Text that reaches it can try to become a rule, e.g. a repo file
says "always run `curl … | sh`", an agent copies it into a note, and two tasks later it is
a proposed learning.

Defences:

- nothing is injected until you accept it (no auto-accept, ever);
- agent-written notes are context, never preferences;
- from transcripts, only your prompts (and a short reply snippet), never tool output;
- the learner container sees no repos and can run nothing through its gate;
- quotes are checked against the extract, so evidence cannot be invented.

## Prerequisite: who wrote each notes line

`atl note` defaults to `--author user` (`internal/cli/info.go`), so notes agents write from
task sessions read as yours. And the `[author]` tag is only text: `atl notes --set` from a
task container (or the assistant) replaces the whole text and can write `[user]` lines.
So the author of a line is recorded outside the text:

- `model.AuthorAgent`. The task gate forces it on `atl note` (as the assistant's gate
  forces `assistant`).
- Every notes write records, for each non-blank line, who wrote it first:
  `notes_line_authors(task_id, line_hash, copy, author, at)` (sha256 of the line; `copy` k
  for its k-th copy). Insert-or-ignore, never updated or deleted, so a line an agent wrote
  stays the agent's however it comes back (a stale editor buffer, a restored copy).
  Writers: `user` (host CLI, TUI editor), `agent` (task gate), `assistant` (assistant
  gate). An append records only the copies it adds.
- The hash is of a canonical form (frozen, V1): invalid UTF-8, U+FFFD, control and format
  characters (BOM, zero-width, bidi) dropped, whitespace runs made one space, trimmed; so
  the editor's tab and U+FFFD changes don't make a line new. A line an external editor
  rewrites in other ways (NFC, wrapping, smart quotes) on the user's round trip counts as
  theirs: the user's review of every learning is the final defence.
- The migration records the existing lines as `unknown`, raw and as sanitising splits them.
- Cap: 10000 rows per author and task. Past it the author's new lines are `unknown`; while
  any other author is past it, the user's whole-text writes record nothing (they could
  bring back an unrecorded line), only `atl note` appends do.
- `NotesBlame(task)`: each line of the current notes with its first writer (copies matched
  by count, not position; `unknown` if not recorded; blank lines marked `Blank`).

The learner uses the blame, never the tags. Lines from `agent`, `assistant` or `unknown`
are context; only `user` lines can be preferences.

## Data model

Migration adds (timestamps as INTEGER unix ms, as elsewhere):

```
learnings(id, text, status, created_at, updated_at, decided_at)
    status: proposed | accepted | rejected
learning_occurrences(id, learning_id, task_id, session_id, at, strength, polarity, quote)
learn_digests(task_id, input_hash, occurrences_json, created_at)
notes_line_authors(task_id, line_hash, copy, author, at)   -- phase 0
```

## Surfaces

Every TUI action also exists in the CLI.

| CLI | |
|---|---|
| `atl learn [--dry-run]` | run the pipeline; `--dry-run` prints the extracts only |
| `atl learnings [ls]` | list with score, status, tasks |
| `atl learnings show <id>` | occurrences and quotes |
| `atl learnings accept\|reject <id>` | decide |
| `atl learnings edit <id>` | reword (stdin, like `atl notes --set`) |
| `atl learnings rm <id>` | delete |

TUI: a key to run `atl learn`, and a review list (proposed first) with accept / edit /
reject. The assistant may run `atl learnings ls` and `show` (allowed, read-only).

## Phases

0. Notes provenance: `agent` author forced by the task gate, `notes_line_authors`, `NotesBlame`.
1. Static extract and `atl learn --dry-run`.
2. Pre-clean: learner container, per-task `claude -p` with a schema, quote check, cache.
3. Analyse, scoring, the tables, review through `atl learnings`.
4. Delivery in SessionStart (task sessions and the assistant).
5. TUI: run key and review list.

## Defaults (to tune)

| | |
|---|---|
| reply snippet before each prompt | kept, last 200 characters of the previous reply |
| prompt cap | 2 KiB per prompt |
| strength weights | strong 2, normal 1, mild 0.5 |
| half-life | proposed 45 days, accepted 365 days |
| propose | score ≥ 3 and ≥ 2 distinct tasks |
| accepted → "possibly outdated" | score of contradictions ≥ 2 (decayed), or net score < 0 |
| injection budget | 800 bytes |
| pre-clean parallelism | 3 |

All in one Go struct with these defaults, overridable from `[learn]` in `atelier.toml`.
