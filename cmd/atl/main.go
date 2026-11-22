// Command atl groups Claude Code sessions by task.
package main

import (
	"os"
	"path/filepath"

	"github.com/andreujuanc/atelier/internal/cli"
	"github.com/andreujuanc/atelier/internal/hook"
	"github.com/andreujuanc/atelier/internal/sandbox"
)

func main() {
	// Run as wl-copy (or wl-paste): the copy relay of a task container,
	// linked to atl there (see sandbox.CopyMain).
	if name := filepath.Base(os.Args[0]); sandbox.CopyTool(name) {
		os.Exit(sandbox.CopyMain(name, os.Args[1:], os.Stdin, os.Stderr, os.Getenv))
	}
	// Fast path: Claude Code hooks must not pay for cobra or the CLI setup.
	// So must Claude Code's status line command.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "hook":
			os.Exit(hook.Main(os.Args[2:]))
		case "statusline":
			os.Exit(hook.StatusLineMain())
		}
	}
	os.Exit(cli.Execute())
}
