package sandbox

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"unicode/utf8"
)

// The atelier assistant runs in a container of its own, one per workspace,
// next to the task containers: it sees its own dir, its own Claude config
// dir and its run dir, nothing else, and has no Docker access. Its gate
// (see Control.Workspace) answers its guard hook and runs, where atl runs,
// the atl commands the assistant may use (OpAtl, see CheckAssistantArgs);
// `atl` in its container forwards every command there.

// AssistantEnv is set (to 1) in the assistant's container: atl there sends
// its commands to the assistant's gate (OpAtl) instead of refusing them. It
// only picks what the client asks for: whether the gate runs them is the
// gate's to know, from how it was started.
const AssistantEnv = "ATL_ASSISTANT"

// assistantSlug stands for a task slug in the assistant's sandbox ID. Slugs
// are [a-z0-9-] (model.Slugify), so none starts with "_".
const assistantSlug = "_assistant"

// AssistantID names the assistant's sandbox on the Docker daemon (its
// container is ContainerName of it): "_assistant-<6 hex digits of the
// assistant dir>", so the assistants of two workspaces differ, and no task's
// ID (<slug>-<6 hex digits>) can be it.
func AssistantID(assistantDir string) string { return ID(assistantSlug, assistantDir) }

// InAssistant reports whether this process runs in the assistant's
// container.
func InAssistant() bool { return Inside() && os.Getenv(AssistantEnv) != "" }

// AssistantCommands are the atl commands the assistant's gate runs for it,
// in its workspace (see CheckAssistantArgs). Not done and cancel: they
// remove a task's worktrees, which the user does from the TUI or a terminal.
var AssistantCommands = []string{
	"ls", "show", "transcript", "repos", "diff", "new", "add", "status", "session", "resume",
	"pr", "note", "notes", "rename", "doctor", "help",
}

// ForAssistantFlag is the hidden global atl flag (--for-assistant) that the
// assistant's gate puts right after the command of every command line it
// runs, and that no request may carry (CheckAssistantArgs): with it, atl
// status only moves an open task between open statuses (no closing, no
// reopening, see app.SetOpenStatus), and atl notes --set and --clear first
// keep the notes they replace in a file only atl sees
// (app.ReplaceNotesKept).
const ForAssistantFlag = "for-assistant"

// MaxAssistantArgs bounds the arguments of one OpAtl request.
const MaxAssistantArgs = 256

// ErrAssistantOpen is the refusal of `atl open` from the assistant: it
// attaches a terminal, which only the user has.
var ErrAssistantOpen = errors.New("atl open attaches a terminal and does not run for the assistant: " +
	"open it in the atl TUI: select the task and press Enter, or run `atl open <task>` in a terminal")

// CheckAssistantArgs returns why the assistant's gate refuses to run atl
// with args (the command line without the binary), or nil. It runs only the
// commands of AssistantCommands, named first (help is `atl help <command>`
// or `atl <command> --help`), and refuses:
//   - a first argument that is a flag: cobra may take the next argument as
//     its value and run whatever command follows (`--help --help=false
//     token --paste`);
//   - any spelling of the global --workspace flag (-w, -w<x>, a -w in a
//     group of short flags, --workspace, --workspace=<x>): the gate picks
//     the workspace, its own;
//   - any spelling of --force (-f, in a group too, --force, --force=<x>):
//     no command the assistant may run has it (done and cancel, where it
//     discards uncommitted work, are the user's);
//   - done and cancel: they remove the task's worktrees;
//   - status with done or cancelled among its arguments (closing a task is
//     the user's; atl itself, run with ForAssistantFlag, also refuses to
//     close or reopen one);
//   - any spelling of ForAssistantFlag: the gate adds it, the requester
//     may not turn it off;
//   - --author with any value but assistant (--author <x>, --author=<x>),
//     and atl note without --author assistant: the assistant writes no note
//     that reads as the user's;
//   - `repos pin` (any argument after repos): it accepts a changed origin
//     URL, a decision for the user.
//
// Every argument is checked, also after "--" (a flag can take "--" as its
// value, and then a -w after it would count): an argument that is text for
// a command but looks like one of these is refused too. The environment the
// command runs with is the gate's, never the requester's.
func CheckAssistantArgs(args []string) error {
	if len(args) == 0 {
		return errors.New("atl without a command is the TUI, which does not run for the assistant")
	}
	if len(args) > MaxAssistantArgs {
		return fmt.Errorf("more than %d arguments", MaxAssistantArgs)
	}
	for _, a := range args {
		if strings.IndexByte(a, 0) >= 0 || !utf8.ValidString(a) {
			return errors.New("an argument is not valid text")
		}
	}
	for _, a := range args {
		switch {
		case a == "--workspace" || strings.HasPrefix(a, "--workspace="):
			return errWorkspaceFlag
		case a == "--force" || strings.HasPrefix(a, "--force="):
			return errForceFlag
		case a == "--"+ForAssistantFlag || strings.HasPrefix(a, "--"+ForAssistantFlag+"="):
			return errors.New("--" + ForAssistantFlag + " is not accepted: the gate sets it")
		case len(a) > 1 && a[0] == '-' && a[1] != '-':
			// Short flags, possibly grouped (-fw), possibly with a value
			// attached (-wother): a w or f anywhere may be one.
			if strings.ContainsRune(a[1:], 'w') {
				return errWorkspaceFlag
			}
			if strings.ContainsRune(a[1:], 'f') {
				return errForceFlag
			}
		}
	}
	// A note is the assistant's: --author assistant, never another author
	// (the default is the user), in any spelling.
	byAssistant := false
	for i, a := range args {
		v, eq := strings.CutPrefix(a, "--author=")
		if !eq {
			if a != "--author" {
				continue
			}
			if i+1 < len(args) {
				v = args[i+1]
			}
		}
		if v != "assistant" {
			return errors.New("--author " + v + " is not accepted: the assistant's notes are --author assistant")
		}
		byAssistant = true
	}
	if args[0] == "note" && !byAssistant {
		return errors.New("atl note needs --author assistant here: a note without it would read as the user's")
	}
	sub := args[0]
	switch {
	case sub == "open":
		return ErrAssistantOpen
	case sub == "done" || sub == "cancel":
		return errors.New("atl " + sub + " removes the task's worktrees and does not run for the assistant: " +
			"ask the user to close the task in the atl TUI (d done, c cancel) or with atl " + sub + " <task> in a terminal")
	case strings.HasPrefix(sub, "-"):
		return fmt.Errorf("flag %s before the command is not accepted: put the command first (help: atl help <command>, or atl <command> --help)", sub)
	case sub == "status" && (slices.Contains(args[1:], "done") || slices.Contains(args[1:], "cancelled")):
		return errors.New("atl status done/cancelled does not run for the assistant (closing a task is the user's): " +
			"ask the user to close it in the atl TUI (d done, c cancel) or with atl done/cancel <task> in a terminal")
	case sub == "repos":
		for _, a := range args[1:] {
			if !strings.HasPrefix(a, "-") {
				return errors.New("atl repos " + a + " does not run for the assistant (re-pinning a repo's origin is the user's decision): " +
					"ask the user to run it in a terminal")
			}
		}
	}
	for _, c := range AssistantCommands {
		if sub == c {
			return nil
		}
	}
	return fmt.Errorf("atl %s does not run for the assistant; it may run: %s (help: atl help <command>)", sub, strings.Join(AssistantCommands, ", "))
}

var (
	errWorkspaceFlag = errors.New("--workspace (-w) is not accepted: the assistant works in its own workspace only")
	errForceFlag     = errors.New("--force (-f) is not accepted: it discards uncommitted work; " +
		"ask the user to do it in the atl TUI or a terminal")
)

// AssistantCommandPath reports whether path, the CommandPath of the
// command cobra resolves an atl command line to ("atl ls", "atl repos pin"),
// is one the assistant's gate runs: "atl <c>" for c in AssistantCommands.
// The gate checks it as well as CheckAssistantArgs, on the command line it
// runs, so that no spelling the string check misreads (a flag that takes the
// next argument as its value, say) reaches another command.
func AssistantCommandPath(path string) bool {
	sub, ok := strings.CutPrefix(path, "atl ")
	return ok && slices.Contains(AssistantCommands, sub)
}
