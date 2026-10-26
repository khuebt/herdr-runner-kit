package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/khuebt/herdr-runner-kit/internal/harness"
	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func newUsageCommand() *cobra.Command {
	var since, until, owner, by string
	var all, withMain, asJSON bool
	cmd := &cobra.Command{
		Use:   "usage [id...]",
		Short: "Total the token usage of tasks and main sessions, per task or per owner",
		Long: "usage reads the token usage of each task's agent sessions and their subagents from " +
			"their transcripts, so a running task counts too; a task whose transcript is gone shows " +
			"the usage stored at its finish. Only Claude runners have usage; the others show -. " +
			"Tasks of the current folder's main session and every project are listed, as by list, " +
			"and --since and --until keep those that finished in the window, or started in it while " +
			"running. --main adds the recorded main sessions, counting their calls in the window. " +
			"UNITS weigh tokens by list-price ratios to an input token: input 1, cache read 0.1, " +
			"5-minute cache write 1.25, 1-hour cache write 2, output 5. They compare runs; they are " +
			"not money. MAX-CTX is the largest context one call of the task's own sessions carried.",
		RunE: func(cmd *cobra.Command, args []string) error {
			now := time.Now()
			var o usageOptions
			var err error
			if o.from, err = parseWhen(since, now); err != nil {
				return fmt.Errorf("--since: %w", err)
			}
			if o.to, err = parseWhen(until, now); err != nil {
				return fmt.Errorf("--until: %w", err)
			}
			if by != "task" && by != "owner" {
				return fmt.Errorf("--by takes task or owner, not %q", by)
			}
			for _, a := range args {
				id, err := parseTaskID(a)
				if err != nil {
					return err
				}
				o.ids = append(o.ids, id)
			}
			r, err := openRunner()
			if err != nil {
				return err
			}
			tasks, err := r.Store.List()
			if err != nil {
				return err
			}
			o.all, o.owner, o.projects = all, owner, owner == ""
			if !all && owner == "" {
				o.owner = currentOwner(cmd)
			}
			rows := taskUsage(tasks, o)
			if withMain {
				rows = append(rows, mainUsage(r.Store.MainSessions(), o)...)
			}
			if by == "owner" {
				rows = byOwner(rows)
			}
			if asJSON {
				return printUsageJSON(cmd.OutOrStdout(), rows)
			}
			return printUsage(cmd.OutOrStdout(), rows, by == "owner")
		},
	}
	f := cmd.Flags()
	f.StringVar(&since, "since", "", "only tasks and main calls from this time on: an age such as 24h or 7d, or a time such as 2026-10-17 or 2026-10-17 09:30")
	f.StringVar(&until, "until", "", "only tasks and main calls up to this time, in --since's forms")
	f.StringVar(&owner, "owner", "", "only tasks of this main session (default: the main repository of the current folder, plus the tasks of every project)")
	f.BoolVar(&all, "all", false, "tasks of every main session")
	f.StringVar(&by, "by", "task", "one row per task, or per owner")
	f.BoolVar(&withMain, "main", false, "add the recorded main sessions")
	f.BoolVar(&asJSON, "json", false, "print the rows and the total as JSON")
	return cmd
}

type usageOptions struct {
	ids           []string
	owner         string
	all, projects bool
	from, to      time.Time
}

// usageRow is one line of usage: a task, a main session, or an owner's
// sum. Usage is nil when none could be read.
type usageRow struct {
	Task   string      `json:"task,omitempty"`
	Kind   string      `json:"kind,omitempty"`
	Agent  string      `json:"agent,omitempty"`
	Status string      `json:"status,omitempty"`
	Owner  string      `json:"owner"`
	Tasks  int         `json:"tasks,omitempty"` // in a per-owner row
	Usage  *task.Usage `json:"usage"`
}

// mainOwner is the owner of main session rows: one main session serves
// every repository.
const mainOwner = "main"

// taskUsage reads the usage of the tasks o selects.
func taskUsage(tasks []*task.Task, o usageOptions) []usageRow {
	var rows []usageRow
	for _, t := range tasks {
		switch {
		case len(o.ids) > 0:
			if !slices.Contains(o.ids, t.ID) {
				continue
			}
		case !o.all && t.Owner != o.owner && (!o.projects || !task.IsProjectOwner(t.Owner)):
			continue
		}
		at := t.FinishedAt
		if !t.Status.Terminal() || at.IsZero() {
			at = t.CreatedAt
		}
		if (!o.from.IsZero() && at.Before(o.from)) || (!o.to.IsZero() && at.After(o.to)) {
			continue
		}
		u := runner.ReadUsage(t.Transcripts, time.Time{}, time.Time{})
		if u == nil {
			u = t.Usage
		}
		rows = append(rows, usageRow{Task: t.ID, Kind: t.Kind, Agent: t.Agent, Status: string(t.Status), Owner: t.Owner, Usage: u})
	}
	return rows
}

// mainUsage reads the calls in o's window of each recorded main session.
// hrk main starts Claude, so its transcripts are Claude's.
func mainUsage(sessions []state.MainSession, o usageOptions) []usageRow {
	var rows []usageRow
	for _, m := range sessions {
		if m.Transcript == "" {
			continue // recorded before transcripts were kept
		}
		row := usageRow{Task: "main " + m.ID[:min(8, len(m.ID))], Kind: "main", Owner: mainOwner}
		row.Usage, _ = harness.ClaudeUsage([]string{m.Transcript}, o.from, o.to)
		if row.Usage != nil && row.Usage.Calls == 0 {
			continue // no call in the window
		}
		rows = append(rows, row)
	}
	return rows
}

// byOwner sums rows per owner, in the order owners first appear.
func byOwner(rows []usageRow) []usageRow {
	var out []usageRow
	index := map[string]int{}
	for _, r := range rows {
		i, ok := index[r.Owner]
		if !ok {
			i = len(out)
			index[r.Owner] = i
			out = append(out, usageRow{Owner: r.Owner})
		}
		out[i].Tasks++
		if r.Usage != nil {
			if out[i].Usage == nil {
				out[i].Usage = &task.Usage{}
			}
			out[i].Usage.Add(r.Usage)
		}
	}
	return out
}

func usageTotal(rows []usageRow) task.Usage {
	var total task.Usage
	for _, r := range rows {
		if r.Usage != nil {
			total.Add(r.Usage)
		}
	}
	return total
}

func printUsage(out io.Writer, rows []usageRow, owners bool) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	numbers := func(u *task.Usage) string {
		if u == nil {
			return "-\t-\t-\t-"
		}
		return fmt.Sprintf("%d\t%s\t%d\t%s", u.Calls, task.Count(float64(u.MaxContext)), u.Subagents, task.Count(u.Units))
	}
	if owners {
		fmt.Fprintln(w, "OWNER\tTASKS\tCALLS\tMAX-CTX\tSUBAGENTS\tUNITS")
		for _, r := range rows {
			fmt.Fprintf(w, "%s\t%d\t%s\n", r.Owner, r.Tasks, numbers(r.Usage))
		}
	} else {
		fmt.Fprintln(w, "TASK\tKIND\tAGENT\tSTATUS\tCALLS\tMAX-CTX\tSUBAGENTS\tUNITS")
		for _, r := range rows {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Task, r.Kind, orDash(r.Agent), orDash(r.Status), numbers(r.Usage))
		}
	}
	total := usageTotal(rows)
	if owners {
		fmt.Fprintf(w, "total\t\t%s\n", numbers(&total))
	} else {
		fmt.Fprintf(w, "total\t\t\t\t%s\n", numbers(&total))
	}
	return w.Flush()
}

func printUsageJSON(out io.Writer, rows []usageRow) error {
	if rows == nil {
		rows = []usageRow{}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		Rows  []usageRow `json:"rows"`
		Total task.Usage `json:"total"`
	}{rows, usageTotal(rows)})
}

// parseWhen reads a --since or --until value: empty for none, an age such
// as 90m, 24h or 7d before now, or a local date or time.
func parseWhen(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.ParseFloat(days, 64); err == nil {
			return now.Add(-time.Duration(n * float64(24*time.Hour))), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is neither an age such as 24h or 7d nor a time such as 2026-10-17 09:30", s)
}
