package cli

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/andreujuanc/atelier/internal/app"
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
		Use:   "learn [--task <task>] [--dry-run [--json]]",
		Short: "Learn your preferences across tasks (--dry-run: print each task's extract, call no model)",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if asJSON && !dryRun {
				return usagef("--json goes with --dry-run")
			}
			ap, err := c.App()
			if err != nil {
				return err
			}
			if !dryRun {
				return c.learnRun(cmd, ap, task)
			}
			exs, err := ap.LearnExtracts(cmd.Context(), task)
			if err != nil {
				return err
			}
			c.extractWarnings(exs)
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
	cmd.Flags().BoolVar(&asJSON, "json", false, "with --dry-run: print the extracts as JSON")
	return cmd
}

// extractWarnings prints what could not be read for the extracts.
func (c *cli) extractWarnings(exs []app.TaskExtract) {
	for _, ex := range exs {
		for _, w := range ex.Warnings {
			fmt.Fprintf(c.stderr, "atl: warning: %s\n", sanitize(w))
		}
	}
}

// learnRun is `atl learn` without --dry-run: the pre-clean of each task
// (app.Preclean), one row each: cached, ran (occurrences kept, dropped by
// reason, cost), failed (why) or empty; then the totals. Analyse is not
// there yet. A task that failed makes it exit 1.
func (c *cli) learnRun(cmd *cobra.Command, ap *app.App, task string) error {
	pts, err := ap.Preclean(cmd.Context(), task)
	if err != nil {
		return err
	}
	exs := make([]app.TaskExtract, len(pts))
	for i, pt := range pts {
		exs[i] = pt.TaskExtract
	}
	c.extractWarnings(exs)
	tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TASK\tPRE-CLEAN\tOCCURRENCES\tCOST\tDETAIL")
	var kept int
	var cost float64
	status := map[string]int{}
	for _, pt := range pts {
		status[pt.Status]++
		kept += len(pt.Occurrences)
		cost += pt.CostUSD
		occ, detail := fmt.Sprint(len(pt.Occurrences)), ""
		switch pt.Status {
		case app.PrecleanFailed:
			occ, detail = "-", oneLine(pt.Err.Error())
		case app.PrecleanRan:
			detail = droppedBy(pt.Dropped)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", pt.Task.Slug, pt.Status, occ, usd(pt.CostUSD, pt.Status == app.PrecleanCached || pt.Status == app.PrecleanEmpty), detail)
	}
	var parts []string
	for _, s := range []string{app.PrecleanRan, app.PrecleanCached, app.PrecleanEmpty, app.PrecleanFailed} {
		if status[s] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", status[s], s))
		}
	}
	fmt.Fprintf(tw, "total (%d tasks)\t%s\t%d\t%s\t\n", len(pts), strings.Join(parts, ", "), kept, usd(cost, false))
	if err := tw.Flush(); err != nil {
		return err
	}
	if n := status[app.PrecleanFailed]; n > 0 {
		return fmt.Errorf("learn: the pre-clean of %d task(s) failed; run atl learn again to retry them (the others are cached)", n)
	}
	fmt.Fprintln(c.stdout, "analyse: not implemented yet")
	return nil
}

// usd is a cost in dollars; "-" when none was spent in this run.
func usd(v float64, none bool) string {
	if none {
		return "-"
	}
	return fmt.Sprintf("$%.4f", v)
}

// droppedBy is the occurrences dropped by reason: "dropped 2 quote not
// found, 1 bad enum"; "" if none.
func droppedBy(n map[string]int) string {
	reasons := make([]string, 0, len(n))
	for r := range n {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	var parts []string
	for _, r := range reasons {
		parts = append(parts, fmt.Sprintf("%d %s", n[r], r))
	}
	if len(parts) == 0 {
		return ""
	}
	return "dropped " + strings.Join(parts, ", ")
}

// oneLine is s sanitized on one line.
func oneLine(s string) string { return strings.Join(strings.Fields(sanitize(s)), " ") }

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
