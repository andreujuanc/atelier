package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/andreujuanc/atelier/internal/learn"
	"github.com/andreujuanc/atelier/internal/model"
)

// noteAuthors is the order notes lines are counted in, by author.
var noteAuthors = []string{model.AuthorUser, model.AuthorAgent, model.AuthorAssistant, model.AuthorUnknown}

// learnCmd is `atl learn`. It runs where atelier runs: not in a task
// container (insideContainer) and not for the assistant (not in
// sandbox.AssistantCommands).
func (c *cli) learnCmd() *cobra.Command {
	var dryRun, asJSON bool
	var task string
	cmd := &cobra.Command{
		Use:   "learn --dry-run [--task <task>] [--json]",
		Short: "Learn your preferences across tasks (--dry-run: print each task's extract, call no model)",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !dryRun {
				return usagef("only --dry-run is implemented yet")
			}
			ap, err := c.App()
			if err != nil {
				return err
			}
			exs, err := ap.LearnExtracts(cmd.Context(), task)
			if err != nil {
				return err
			}
			for _, ex := range exs {
				for _, w := range ex.Warnings {
					fmt.Fprintf(c.stderr, "atl: warning: %s\n", sanitize(w))
				}
			}
			if asJSON {
				out := make([]learn.Extract, len(exs))
				for i, ex := range exs {
					out[i] = ex.Extract
				}
				return c.printJSON(out)
			}
			tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "TASK\tPROMPTS\tDROPPED\tNOTES\tBYTES")
			var kept, dropped, droppedNotes, size int
			total := map[string]int{}
			for _, ex := range exs {
				n := map[string]int{}
				for _, l := range ex.Notes {
					n[l.Author]++
					total[l.Author]++
				}
				b := len(learn.Encode(ex.Extract))
				kept, dropped, droppedNotes, size = kept+len(ex.Prompts), dropped+ex.DroppedPrompts, droppedNotes+ex.DroppedNotes, size+b
				fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%d\n", ex.Task.Slug, len(ex.Prompts), ex.DroppedPrompts, byAuthor(n, ex.DroppedNotes), b)
			}
			fmt.Fprintf(tw, "total (%d tasks)\t%d\t%d\t%s\t%d\n", len(exs), kept, dropped, byAuthor(total, droppedNotes), size)
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "build and print the extracts only; no model is called")
	cmd.Flags().StringVar(&task, "task", "", "only this task (default: every task, open or closed)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the extracts as JSON")
	return cmd
}

// byAuthor is notes line counts by author and the lines dropped: "3 user,
// 1 agent, 2 dropped"; "-" if none.
func byAuthor(n map[string]int, dropped int) string {
	var parts []string
	for _, a := range noteAuthors {
		if n[a] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n[a], a))
		}
	}
	if dropped > 0 {
		parts = append(parts, fmt.Sprintf("%d dropped", dropped))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}
