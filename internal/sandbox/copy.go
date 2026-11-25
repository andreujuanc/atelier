package sandbox

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"time"
)

// The copy relay. Claude Code copies a selection (its fullscreen renderer
// selects with the mouse itself and copies on release) with a clipboard tool
// when it finds one, on Linux wl-copy, xclip or xsel, and also through tmux
// (load-buffer -w) when $TMUX is set. Next to atl it ran in tmux, which
// forwarded the copy to the terminal; in a task container there is neither
// tmux nor a clipboard tool, so a copy went nowhere. (The OSC 52 sequence it
// writes as well reaches tmux, which ignores it from an application unless
// set-clipboard is on.)
//
// So the container gets a wl-copy: atl itself, linked as wl-copy next to it
// in the run dir's bin (InstallBin), which is first on PATH there (ExecArgv).
// Claude Code 2.1 uses wl-copy only with $WAYLAND_DISPLAY set, and runs it
// as `wl-copy` (stdin: the text) and, unless another copy came meanwhile,
// `wl-copy --primary` with the same text. ExecArgv sets $WAYLAND_DISPLAY to
// CopyDisplay, the name of no socket (it is relative, and a task container
// has no $XDG_RUNTIME_DIR): a Wayland client in there fails to connect, as
// it did without a display. Claude Code also takes a display to mean that it
// can open a browser: for a link or an MCP server's sign-in it now runs
// xdg-open (not in the default image), and says it could not open the
// browser and shows the URL, where it said it was headless before.
//
// wl-copy sends the text to the task's gate (OpCopy), which puts it in the
// clipboard of the terminal attached to the task's tmux session. It is
// write-only: wl-paste (Claude Code tries it, and xclip -o, for a pasted
// image) is refused, and atl never offers a tool that reads the clipboard.

// CopyDisplay is the $WAYLAND_DISPLAY of a session in a task container: it
// makes Claude Code use wl-copy (see above), and names no socket.
const CopyDisplay = "atl-copy-relay"

// copyTools are the names atl answers to as the copy relay (CopyMain): it
// is linked as each next to the atl of a task container.
var copyTools = []string{"wl-copy", "wl-paste"}

// CopyTool reports whether atl runs as one of the copy relay's tools: name
// is the base name it was run as (argv[0]).
func CopyTool(name string) bool {
	for _, t := range copyTools {
		if name == t {
			return true
		}
	}
	return false
}

// copyClientTimeout bounds wl-copy's exchange with the gate. Claude Code
// kills a clipboard tool after 2 seconds anyway; a gate that has the text
// by then copies it all the same.
const copyClientTimeout = 10 * time.Second

// wlCopyArgs is a wl-copy command line as the relay takes it.
type wlCopyArgs struct {
	primary bool     // --primary: the primary selection, not the clipboard
	trim    bool     // --trim-newline: without a final newline
	help    bool     // --help
	text    []string // the text to copy, if given as arguments (else stdin)
}

// parseWlCopy reads a wl-copy command line: the options Claude Code passes
// (none, or --primary) and the few others a script is likely to use. Others
// (--type, --clear, --paste-once, --seat...) are refused rather than
// ignored: a copy that does not do what was asked is worse than none.
func parseWlCopy(args []string) (wlCopyArgs, error) {
	var a wlCopyArgs
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			a.text = append(a.text, args[i+1:]...)
			return a, nil
		case arg == "--primary":
			a.primary = true
		case arg == "--trim-newline":
			a.trim = true
		case arg == "--help":
			a.help = true
		case strings.HasPrefix(arg, "--"):
			return a, fmt.Errorf("option %s is not supported", arg)
		case strings.HasPrefix(arg, "-") && len(arg) > 1:
			for _, f := range arg[1:] {
				switch f {
				case 'p':
					a.primary = true
				case 'n':
					a.trim = true
				case 'h':
					a.help = true
				default:
					return a, fmt.Errorf("option -%c is not supported", f)
				}
			}
		default:
			a.text = append(a.text, arg)
		}
	}
	return a, nil
}

const wlCopyUsage = `usage: wl-copy [--primary] [--trim-newline] [text...]
In a task container, wl-copy is atl's copy relay: it puts the text (the
arguments, else standard input; at most 1 MiB) in the clipboard of the
terminal attached to the task's tmux session, through the task's gate.
--primary is accepted and copies nothing (Claude Code copies the same text to
the clipboard first). Nothing in a task container reads the clipboard.
`

// CopyMain is atl run as one of the copy relay's tools (name, see CopyTool)
// in a task container, with args (argv without the tool), and returns the
// exit status. It never writes to standard output, and wl-paste reads
// nothing at all: the task may write the user's clipboard, never read it.
// env is the process environment (os.Getenv).
func CopyMain(name string, args []string, stdin io.Reader, stderr io.Writer, env func(string) string) int {
	if name != "wl-copy" {
		fmt.Fprintf(stderr, "%s: refused: atl's copy relay only writes to your clipboard; nothing in a task container reads it\n", name)
		return 1
	}
	if env(InEnv) == "" {
		fmt.Fprintln(stderr, "wl-copy: this is atl's copy relay, which works in a task container only")
		return 1
	}
	a, err := parseWlCopy(args)
	if err != nil {
		fmt.Fprintf(stderr, "wl-copy: %v (atl's copy relay)\n%s", err, wlCopyUsage)
		return 2
	}
	if a.help {
		io.WriteString(stderr, wlCopyUsage)
		return 0
	}
	var text []byte
	if a.text != nil {
		text = []byte(strings.Join(a.text, " "))
	} else {
		// Up to two bytes over the cap: one more than it, also once a
		// final newline is trimmed (else a text cut at a newline passes).
		text, err = io.ReadAll(io.LimitReader(stdin, MaxCopy+2))
		if err != nil {
			fmt.Fprintf(stderr, "wl-copy: read stdin: %v\n", err)
			return 1
		}
	}
	if a.primary {
		// Claude Code sends the selection to the clipboard, then to the
		// primary selection: one copy is enough.
		return 0
	}
	if a.trim {
		text = bytes.TrimSuffix(text, []byte("\n"))
	}
	if len(text) > MaxCopy {
		fmt.Fprintf(stderr, "wl-copy: more than %d bytes; atl's copy relay takes at most that, nothing was copied\n", MaxCopy)
		return 1
	}
	if len(text) == 0 {
		return 0
	}
	code, _, err := Ask(env("ATL_CONTROL_SOCK"), ControlRequest{Op: OpCopy}, text, stderr, stderr, copyClientTimeout)
	if err != nil {
		fmt.Fprintf(stderr, "wl-copy: %v\n", err)
		return 1
	}
	return code
}
