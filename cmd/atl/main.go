// Command atl groups Claude Code sessions by task.
package main

import (
	"os"

	"github.com/andreujuanc/atelier/internal/cli"
	"github.com/andreujuanc/atelier/internal/hook"
)

func main() {
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
