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
	var dryRun, asJSON, retrySkipped bool
	var task string
	cmd := &cobra.Command{
		Use:   "learn [--task <task>] [--retry-skipped] [--dry-run [--json]]",
		Short: "Learn your preferences across tasks (--dry-run: print each task's extract, call no model)",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if asJSON && !dryRun {
				return usagef("--json goes with --dry-run")
			}
			if retrySkipped && dryRun {
				return usagef("--retry-skipped does not go with --dry-run")
			}
			ap, err := c.App()
			if err != nil {
				return err
			}
			if !dryRun {
				return c.learnRun(cmd, ap, task, retrySkipped)
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
	cmd.Flags().BoolVar(&retrySkipped, "retry-skipped", false, "first forget which occurrences Analyse skipped (left out in 2 runs), so they are sent again")
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

// learnRun is `atl learn` without --dry-run (app.Learn): the pre-clean of
// each task, one row each: cached, ran (occurrences kept, dropped by
// reason, cost), failed (why) or empty; then the totals; then what
// Analyse did and the status changes. A task whose pre-clean failed, or
// a failed Analyse, makes it exit 1.
func (c *cli) learnRun(cmd *cobra.Command, ap *app.App, task string, retrySkipped bool) error {
	run, err := ap.Learn(cmd.Context(), task, retrySkipped)
	if err != nil {
		return err
	}
	if retrySkipped {
		fmt.Fprintf(c.stderr, "learn: %d skipped occurrence(s) analysed again\n", run.RetriedSkips)
	}
	pts := run.Tasks
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
	c.printAnalyse(run.Analyse)
	var errs []string
	if n := status[app.PrecleanFailed]; n > 0 {
		errs = append(errs, fmt.Sprintf("the pre-clean of %d task(s) failed; run atl learn again to retry them (the others are cached)", n))
	}
	if run.Analyse.Err != nil {
		errs = append(errs, "analyse failed: "+oneLine(run.Analyse.Err.Error())+"; run atl learn again (the pre-cleans are cached)")
	}
	if len(errs) > 0 {
		return fmt.Errorf("learn: %s", strings.Join(errs, "; "))
	}
	return nil
}

// printAnalyse prints what Analyse did and the status changes at the end
// of the run.
func (c *cli) printAnalyse(r app.AnalyseResult) {
	switch {
	case r.New == 0:
		fmt.Fprintln(c.stdout, "analyse: no new occurrences")
	default:
		line := fmt.Sprintf("analyse: %d new occurrence(s): %d assigned", r.New, r.Assigned)
		if r.ToNew > 0 || r.NewLearnings > 0 {
			line += fmt.Sprintf(" (%d to %d new learning(s))", r.ToNew, r.NewLearnings)
		}
		if r.Joined > 0 {
			line += fmt.Sprintf(", %d joined to their item's learning", r.Joined)
		}
		if r.Skipped > 0 {
			line += fmt.Sprintf(", %d skipped (left out twice)", r.Skipped)
		}
		if r.Unassigned > 0 {
			line += fmt.Sprintf(", %d left for the next run", r.Unassigned)
		}
		if r.DroppedLabels > 0 {
			line += fmt.Sprintf(", %d new learning(s) dropped (bad label or text)", r.DroppedLabels)
		}
		line += fmt.Sprintf("; %d call(s), %s", r.Calls, usd(r.CostUSD, r.Calls == 0))
		fmt.Fprintln(c.stdout, line)
	}
	var proposed, faded int
	for _, t := range r.Transitions {
		switch t.To {
		case learn.StatusProposed:
			proposed++
		case learn.StatusCandidate:
			faded++
		}
	}
	if proposed > 0 {
		fmt.Fprintf(c.stdout, "learnings: %d new proposal(s): atl learnings\n", proposed)
	}
	if faded > 0 {
		fmt.Fprintf(c.stdout, "learnings: %d proposal(s) faded back to candidate\n", faded)
	}
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
