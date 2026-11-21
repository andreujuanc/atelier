package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
)

// tokenCmd gets the Claude Code token of sandboxed sessions: it runs
// `claude setup-token` in tmux for the user and stores the token it prints
// where only atl reads it (see app.SetupToken). With --paste it stores a
// token the user already has.
func (c *cli) tokenCmd() *cobra.Command {
	var paste bool
	cmd := &cobra.Command{
		Use:   "token [--paste]",
		Short: "Get a Claude Code token for sandboxed sessions (runs claude setup-token) and store it for atl",
		Long: "Run `claude setup-token` in tmux: approve access in the browser (paste the code back if it\n" +
			"asks), then press Enter to come back. atl reads the token from that screen and stores it in\n" +
			"a file only atl reads (mode 0600, in no task container); if it cannot find it there it\n" +
			"asks you to paste it. Sandboxed sessions use the stored token while neither\n" +
			"CLAUDE_CODE_OAUTH_TOKEN nor ANTHROPIC_API_KEY is set where atl runs. The token lasts a\n" +
			"year: run atl token again then (atl doctor warns a month before).\n\n" +
			"--paste stores a token you already have, read from the terminal without echo (or from\n" +
			"standard input).",
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ap, err := c.doctorApp() // the database is not needed
			if err != nil {
				return err
			}
			in := cmd.InOrStdin()
			ask := func(prompt string) (string, error) { return readSecret(in, c.stderr, prompt) }
			if paste {
				err = ap.PasteToken(ask, "Paste the token from `claude setup-token` (it is not shown) and press Enter: ")
			} else {
				if !isTerminal(os.Stdin) || !isTerminal(c.stdout) {
					return errors.New("atl token runs claude setup-token in tmux and needs a terminal; " +
						"to store a token you already have, use atl token --paste")
				}
				var auto bool
				auto, err = ap.SetupToken(cmd.Context(), func(attach *exec.Cmd) error {
					attach.Stdin, attach.Stdout, attach.Stderr = os.Stdin, c.stdout, c.stderr
					return attach.Run()
				}, ask)
				if err == nil && auto {
					fmt.Fprintln(c.stdout, "Read the token from claude setup-token's screen.")
				}
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(c.stdout, "Claude Code token stored in %s. Sandboxed sessions use it while CLAUDE_CODE_OAUTH_TOKEN "+
				"and ANTHROPIC_API_KEY are unset; it lasts a year (atl doctor shows its age).\n", ap.Paths.ClaudeToken())
			return nil
		},
	}
	cmd.Flags().BoolVar(&paste, "paste", false, "store a token you already have instead of running claude setup-token")
	return cmd
}

// readSecret prints prompt to w and reads one line from in: from a terminal
// without echo, else as it comes (at most 64 KiB).
func readSecret(in io.Reader, w io.Writer, prompt string) (string, error) {
	fmt.Fprint(w, prompt)
	if f, ok := in.(*os.File); ok && isTerminal(f) {
		b, err := term.ReadPassword(f.Fd())
		fmt.Fprintln(w)
		return string(b), err
	}
	line, err := bufio.NewReader(io.LimitReader(in, 64<<10)).ReadString('\n')
	fmt.Fprintln(w)
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		if errors.Is(err, io.EOF) {
			return "", errors.New("no token given")
		}
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
