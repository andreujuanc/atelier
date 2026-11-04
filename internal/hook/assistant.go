package hook

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/andreujuanc/atelier/internal/config"
)

// assistantGuardMain runs the AssistantGuard event: no database, no store.
// Unlike the ro guard it fails closed: input it cannot read or decode is
// refused (exit 2).
func assistantGuardMain(p config.Paths, stdin io.Reader, stderr io.Writer) int {
	data, err := io.ReadAll(io.LimitReader(stdin, int64(maxInput)+1))
	if err != nil {
		logError(p, stderr, EventAssistantGuard, "", fmt.Errorf("read stdin: %w", err))
		return assistantRefuse(stderr, "", "cannot read the hook input")
	}
	if len(data) > maxInput {
		logError(p, stderr, EventAssistantGuard, "", fmt.Errorf("hook input exceeds %d bytes", maxInput))
		return assistantRefuse(stderr, "", fmt.Sprintf("hook input exceeds %d bytes", maxInput))
	}
	var in Input
	if err := json.Unmarshal(data, &in); err != nil {
		logError(p, stderr, EventAssistantGuard, "", fmt.Errorf("decode hook input: %w", err))
		return assistantRefuse(stderr, "", "cannot decode the hook input")
	}
	exit, msg, err := AssistantGuard(in, p)
	if err != nil {
		logError(p, stderr, EventAssistantGuard, in.SessionID, err)
	}
	if exit == 2 {
		fmt.Fprintln(stderr, msg)
		return 2
	}
	return 0
}

// assistantRefuse prints the refusal for a tool call the guard could not
// inspect and returns exit code 2.
func assistantRefuse(stderr io.Writer, tool, reason string) int {
	what := "this tool call"
	if tool != "" {
		what = "the " + tool + " tool call"
	}
	fmt.Fprintf(stderr, "atelier assistant: %s is not allowed (%s) — the assistant only manages tasks\n", what, reason)
	return 2
}

// assistantGuardedTool reports whether the assistant guard checks a tool.
// An empty name is checked too (fail closed); other tools (e.g. TodoWrite,
// which an unanchored matcher could route here) are not file edits.
func assistantGuardedTool(name string) bool {
	switch name {
	case "", "Edit", "Write", "MultiEdit", "NotebookEdit":
		return true
	}
	return false
}

// AssistantGuard is the testable core of the AssistantGuard event. It allows
// (exit 0) an Edit/Write/MultiEdit/NotebookEdit only when every form of its
// target path (relative to cwd, ~-expanded, resolved through symlinks the
// way the kernel would) lands inside Paths.AssistantDir(), but not in its
// generated CLAUDE.md or .claude/ (the assistant must not loosen its own
// permissions). Anything else, including a target it cannot resolve, is
// blocked (exit 2 with msg); err carries details for the log.
func AssistantGuard(in Input, p config.Paths) (exit int, msg string, err error) {
	if !assistantGuardedTool(in.ToolName) {
		return 0, "", nil
	}
	tool := orDash(in.ToolName)
	refuse := func(reason string, err error) (int, string, error) {
		return 2, fmt.Sprintf("atelier assistant: the %s tool call is not allowed (%s) — the assistant only manages tasks",
			tool, reason), err
	}
	blocked := func(target string, err error) (int, string, error) {
		return 2, fmt.Sprintf("atelier assistant: editing %s is not allowed — the assistant only manages tasks", target), err
	}

	targets, err := guardTargets(in.ToolInput)
	if err != nil {
		return refuse("cannot decode its target path", err)
	}
	if len(targets) == 0 {
		return refuse("no target path", errors.New("assistant guard: no file_path or notebook_path in tool_input"))
	}

	dir := filepath.Clean(p.AssistantDir())
	if !filepath.IsAbs(dir) {
		return refuse("assistant dir unknown", fmt.Errorf("assistant guard: assistant dir %q is not absolute", dir))
	}
	roots := []string{dir}
	if r, err := resolvePath(dir); err == nil && r != dir {
		roots = append(roots, r)
	}
	allowed := func(path string) bool {
		for _, root := range roots {
			rel, ok := within(root, path)
			if !ok {
				continue
			}
			// Case-insensitive: on a case-insensitive file system
			// "claude.md" or ".Claude/" is the same file.
			first, _, _ := strings.Cut(rel, string(filepath.Separator))
			if strings.EqualFold(rel, "CLAUDE.md") || strings.EqualFold(first, ".claude") {
				return false
			}
			return true
		}
		return false
	}

	for _, target := range targets {
		forms, ferr := targetForms(target, in.Cwd)
		if ferr != nil {
			return blocked(target, fmt.Errorf("assistant guard: %w", ferr))
		}
		for _, raw := range forms {
			// Where the write lands decides, so only resolved forms count.
			// raw keeps ".." unapplied: the kernel follows a symlink before
			// applying a later "..", so both forms are resolved.
			for _, path := range []string{filepath.Clean(raw), raw} {
				r, rerr := resolvePath(path)
				if rerr != nil {
					return blocked(target, fmt.Errorf("assistant guard: resolve %s: %w", path, rerr))
				}
				if !allowed(r) {
					return blocked(target, nil)
				}
			}
		}
	}
	return 0, "", nil
}
