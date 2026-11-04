// Command atl groups Claude Code sessions by task.
package main

import (
	"os"

	"github.com/andreujuanc/atelier/internal/cli"
	"github.com/andreujuanc/atelier/internal/hook"
)

func main() {
	// Fast path: Claude Code hooks must not pay for cobra or the CLI setup.
	if len(os.Args) > 1 && os.Args[1] == "hook" {
		os.Exit(hook.Main(os.Args[2:]))
	}
	os.Exit(cli.Execute())
}
