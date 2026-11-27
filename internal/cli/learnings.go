package cli

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/andreujuanc/atelier/internal/app"
	"github.com/andreujuanc/atelier/internal/learn"
	"github.com/andreujuanc/atelier/internal/store"
)

// learningsCmd is `atl learnings`: review what atl learn proposes. Like
// atl learn it runs only where atelier runs: not in a task container
// (insideContainer) and not for the assistant (not in
// sandbox.AssistantCommands; the commands that change a learning refuse
// --for-assistant too).
func (c *cli) learningsCmd() *cobra.Command {
	var all, asJSON bool
	cmd := &cobra.Command{
		Use:   "learnings [ls|show|accept|reject|edit|rm]",
		Short: "Review the learnings atl learn proposes (default: ls)",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return c.learningsList(cmd, all, asJSON)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "also the candidates (below the propose threshold) and the rejected")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")

	var lsAll, lsJSON bool
	ls := &cobra.Command{
		Use:   "ls [--all] [--json]",
		Short: "List the proposed and accepted learnings (--all: also candidates and rejected)",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return c.learningsList(cmd, lsAll, lsJSON)
		},
	}
	ls.Flags().BoolVar(&lsAll, "all", false, "also the candidates (below the propose threshold) and the rejected")
	ls.Flags().BoolVar(&lsJSON, "json", false, "print JSON")

	var showJSON bool
	show := &cobra.Command{
		Use:   "show <id> [--json]",
		Short: "A learning and its occurrences (the quotes), newest first",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			id, err := learningID(a[0])
			if err != nil {
				return err
			}
			ap, err := c.App()
			if err != nil {
				return err
			}
			v, occs, err := ap.Learning(cmd.Context(), id)
			if err != nil {
				return err
			}
			sort.SliceStable(occs, func(i, j int) bool { return occs[i].At.After(occs[j].At) })
			if showJSON {
				j := toJSONLearning(v)
				j.Occurrences = make([]jsonOccurrence, len(occs))
				for i, o := range occs {
					j.Occurrences[i] = jsonOccurrence{At: o.At, Task: o.TaskSlug, Session: o.Session, Strength: o.Strength,
						Polarity: o.Polarity, Quote: o.Quote, Text: o.Text, Unrelated: !learn.SharesWord(o.Quote, v.Text)}
				}
				return c.printJSON(j)
			}
			fmt.Fprintf(c.stdout, "learning %d: %s\n", v.ID, oneLine(v.Text))
			fmt.Fprintf(c.stdout, "status %s, score %.1f (reinforce %.1f, contradict %.1f), trend %s %+.1f, %d task(s), last %s\n",
				v.Status, v.Score.Score, v.Reinforce, v.Contradict, trendMark(v.Trend), v.Trend, v.Tasks, date(v.Last))
			if f := learningFlags(v); f != "-" {
				fmt.Fprintf(c.stdout, "flags: %s\n", f)
			}
			if !v.DecidedAt.IsZero() {
				fmt.Fprintf(c.stdout, "decided %s\n", date(v.DecidedAt))
			}
			if v.Edited {
				fmt.Fprintln(c.stdout, "edited by you")
			}
			if len(occs) == 0 {
				fmt.Fprintln(c.stdout, "no occurrences")
				return nil
			}
			unrelated := 0
			tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "DATE\tTASK\tSTRENGTH\tPOLARITY\t?\tQUOTE")
			for _, o := range occs {
				mark := ""
				if !learn.SharesWord(o.Quote, v.Text) {
					mark = "?"
					unrelated++
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", date(o.At), o.TaskSlug, o.Strength, o.Polarity, mark, oneLine(o.Quote))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if unrelated > 0 {
				fmt.Fprintf(c.stdout, "?: %d quote(s) share no word with the text: read them before you accept\n", unrelated)
			}
			return nil
		},
	}
	show.Flags().BoolVar(&showJSON, "json", false, "print JSON")

	decide := func(name string, accept bool) *cobra.Command {
		short := "Accept a proposed (or candidate) learning"
		if !accept {
			short = "Reject a learning: it stays, so it is not proposed again"
		}
		return &cobra.Command{
			Use:   name + " <id>",
			Short: short,
			Args:  args(cobra.ExactArgs(1)),
			RunE: func(cmd *cobra.Command, a []string) error {
				ap, id, err := c.learningChange(a[0])
				if err != nil {
					return err
				}
				if err := ap.DecideLearning(cmd.Context(), id, accept); err != nil {
					if !errors.Is(err, store.ErrLearningStatus) {
						return err
					}
					l, gerr := ap.Store.GetLearning(cmd.Context(), id)
					if gerr != nil {
						return gerr
					}
					if accept {
						return fmt.Errorf("learning %d is %s: only a proposed or candidate learning can be accepted", id, l.Status)
					}
					return fmt.Errorf("learning %d is %s already", id, l.Status)
				}
				fmt.Fprintf(c.stdout, "learning %d %sed\n", id, name)
				return nil
			},
		}
	}

	edit := &cobra.Command{
		Use:   "edit <id>",
		Short: "Reword a learning with the text read from stdin (one line, at most 200 characters)",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			ap, id, err := c.learningChange(a[0])
			if err != nil {
				return err
			}
			// A bad id fails before stdin is read.
			if _, err := ap.Store.GetLearning(cmd.Context(), id); err != nil {
				return err
			}
			in := cmd.InOrStdin()
			if isTerminal(in) {
				fmt.Fprintln(c.stderr, "atl: reading the new text from stdin; end with ctrl-d")
			}
			data, err := io.ReadAll(io.LimitReader(in, 8*learn.MaxLearningText+1))
			if err != nil {
				return fmt.Errorf("read stdin: %w", err)
			}
			if len(data) > 8*learn.MaxLearningText {
				return fmt.Errorf("the text is over %d characters", learn.MaxLearningText)
			}
			if _, err := ap.EditLearning(cmd.Context(), id, string(data)); err != nil {
				return err
			}
			fmt.Fprintf(c.stdout, "learning %d edited\n", id)
			return nil
		},
	}

	rm := &cobra.Command{
		Use:   "rm <id>",
		Short: "Forget a learning and its occurrences (those still in a task are analysed again by the next atl learn; reject keeps it from coming back)",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			ap, id, err := c.learningChange(a[0])
			if err != nil {
				return err
			}
			n, err := ap.DeleteLearning(cmd.Context(), id)
			if err != nil {
				return err
			}
			fmt.Fprintf(c.stdout, "learning %d deleted (%d occurrence(s))\n", id, n)
			return nil
		},
	}

	cmd.AddCommand(ls, show, decide("accept", true), decide("reject", false), edit, rm)
	return cmd
}

// learningChange readies a command that changes learning ref: refused for
// the assistant, ref an id, the app open.
func (c *cli) learningChange(ref string) (*app.App, int64, error) {
	if c.forAssistant {
		return nil, 0, errors.New("learnings are changed only by you, where atelier runs; not for the assistant")
	}
	id, err := learningID(ref)
	if err != nil {
		return nil, 0, err
	}
	ap, err := c.App()
	return ap, id, err
}

// learningID parses a learning's id.
func learningID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, usagef("%q is not a learning id (atl learnings ls --all lists them)", s)
	}
	return id, nil
}

// statusOrder is the order atl learnings lists statuses in: what to
// review first.
var statusOrder = map[string]int{learn.StatusProposed: 0, learn.StatusAccepted: 1, learn.StatusCandidate: 2, learn.StatusRejected: 3}

// learningsList is `atl learnings [ls]`: the proposed and accepted
// learnings (all: every one), proposed first, then by score.
func (c *cli) learningsList(cmd *cobra.Command, all, asJSON bool) error {
	ap, err := c.App()
	if err != nil {
		return err
	}
	vs, err := ap.Learnings(cmd.Context())
	if err != nil {
		return err
	}
	var shown []app.LearningView
	for _, v := range vs {
		if all || v.Status == learn.StatusProposed || v.Status == learn.StatusAccepted {
			shown = append(shown, v)
		}
	}
	sort.SliceStable(shown, func(i, j int) bool {
		a, b := shown[i], shown[j]
		if statusOrder[a.Status] != statusOrder[b.Status] {
			return statusOrder[a.Status] < statusOrder[b.Status]
		}
		if a.Score.Score != b.Score.Score {
			return a.Score.Score > b.Score.Score
		}
		return a.ID < b.ID
	})
	if asJSON {
		out := make([]jsonLearning, len(shown))
		for i, v := range shown {
			out[i] = toJSONLearning(v)
		}
		return c.printJSON(out)
	}
	if len(shown) == 0 {
		if all {
			fmt.Fprintln(c.stdout, "no learnings yet: atl learn makes them")
		} else {
			fmt.Fprintln(c.stdout, "no proposed or accepted learnings (atl learnings ls --all shows the candidates and rejected)")
		}
		return nil
	}
	tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tSCORE\tTREND\tTASKS\tLAST\tFLAGS\tTEXT")
	for _, v := range shown {
		fmt.Fprintf(tw, "%d\t%s\t%.1f\t%s\t%d\t%s\t%s\t%s\n", v.ID, v.Status, v.Score.Score, trendMark(v.Trend), v.Tasks, date(v.Last), learningFlags(v), oneLine(v.Text))
	}
	return tw.Flush()
}

// trendMark is a trend as an arrow: ↑ rising, ↓ fading, – about flat.
func trendMark(t float64) string {
	switch {
	case t > 0.05:
		return "↑"
	case t < -0.05:
		return "↓"
	}
	return "–"
}

// learningFlags is what review must look at in an accepted learning:
// "outdated", "still-corrected"; "-" if nothing.
func learningFlags(v app.LearningView) string {
	var f []string
	if v.Outdated {
		f = append(f, "outdated")
	}
	if v.StillCorrected {
		f = append(f, "still-corrected")
	}
	if len(f) == 0 {
		return "-"
	}
	return strings.Join(f, ",")
}

// date is t's local date; "-" if zero.
func date(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02")
}

type jsonLearning struct {
	ID             int64            `json:"id"`
	Text           string           `json:"text"`
	Status         string           `json:"status"`
	Edited         bool             `json:"edited"`
	Score          float64          `json:"score"`
	Reinforce      float64          `json:"reinforce"`
	Contradict     float64          `json:"contradict"`
	Trend          float64          `json:"trend"`
	Tasks          int              `json:"tasks"`
	Last           *time.Time       `json:"last,omitempty"`
	Outdated       bool             `json:"outdated"`
	StillCorrected bool             `json:"still_corrected"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
	DecidedAt      *time.Time       `json:"decided_at,omitempty"`
	Occurrences    []jsonOccurrence `json:"occurrences,omitempty"`
}

type jsonOccurrence struct {
	At       time.Time `json:"at"`
	Task     string    `json:"task"`
	Session  int64     `json:"session,omitempty"`
	Strength string    `json:"strength"`
	Polarity string    `json:"polarity"`
	Quote    string    `json:"quote"`
	Text     string    `json:"text"`
	// Unrelated: the quote shares no word with the learning's text
	// (learn.SharesWord): read it.
	Unrelated bool `json:"unrelated"`
}

func toJSONLearning(v app.LearningView) jsonLearning {
	j := jsonLearning{ID: v.ID, Text: v.Text, Status: v.Status, Edited: v.Edited, Score: round(v.Score.Score), Reinforce: round(v.Reinforce),
		Contradict: round(v.Contradict), Trend: round(v.Trend), Tasks: v.Tasks, Outdated: v.Outdated, StillCorrected: v.StillCorrected,
		CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt}
	if !v.Last.IsZero() {
		j.Last = &v.Last
	}
	if !v.DecidedAt.IsZero() {
		j.DecidedAt = &v.DecidedAt
	}
	return j
}

// round is v to 3 decimals.
func round(v float64) float64 {
	r, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'f', 3, 64), 64)
	return r
}
