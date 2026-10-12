package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func newEventsCommand() *cobra.Command {
	var since time.Duration
	var follow, timings bool
	var ids []string
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Show the event log, or the latencies computed from it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := openRunner()
			if err != nil {
				return err
			}
			for i, a := range ids {
				if ids[i], err = parseTaskID(a); err != nil {
					return err
				}
			}
			all, offset, err := r.Store.ReadEventsSince(0)
			if err != nil {
				return err
			}
			keep := func(ev state.Event) bool {
				if since > 0 && time.Since(ev.TS) > since {
					return false
				}
				return len(ids) == 0 || slices.Contains(ids, ev.Task)
			}
			var events []state.Event
			for _, ev := range all {
				if keep(ev) {
					events = append(events, ev)
				}
			}
			out := cmd.OutOrStdout()
			if timings {
				tasks, err := r.Store.List()
				if err != nil {
					return err
				}
				return printTimings(out, events, tasks)
			}
			for _, ev := range events {
				fmt.Fprintln(out, eventLine(ev))
			}
			for follow {
				select {
				case <-cmd.Context().Done():
					return nil
				case <-time.After(250 * time.Millisecond):
				}
				var more []state.Event
				if more, offset, err = r.Store.ReadEventsSince(offset); err != nil {
					return err
				}
				for _, ev := range more {
					if len(ids) == 0 || slices.Contains(ids, ev.Task) {
						fmt.Fprintln(out, eventLine(ev))
					}
				}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.DurationVar(&since, "since", 0, "only events newer than this, such as 1h")
	f.StringSliceVar(&ids, "task", nil, "only these tasks")
	f.BoolVar(&follow, "follow", false, "keep printing new events")
	f.BoolVar(&timings, "timings", false, "print latencies per task, in milliseconds")
	return cmd
}

func eventLine(ev state.Event) string {
	parts := []string{ev.TS.Local().Format("15:04:05.000"), ev.Task, ev.Type}
	if ev.Summary != "" {
		parts = append(parts, ev.Summary)
	}
	if ev.Question != "" {
		parts = append(parts, "Q: "+ev.Question)
	}
	if len(ev.Outputs) > 0 {
		parts = append(parts, "out: "+strings.Join(ev.Outputs, ", "))
	}
	return strings.Join(parts, "  ")
}

// printTimings prints, per task, the budget latencies that the event log can
// measure: spawn to first prompt, parent done to child launch, and report to
// the main session's wake-up.
func printTimings(out io.Writer, events []state.Event, tasks []*task.Task) error {
	first := map[string]map[string]time.Time{} // task -> type -> first time
	for _, ev := range events {
		if first[ev.Task] == nil {
			first[ev.Task] = map[string]time.Time{}
		}
		if _, ok := first[ev.Task][ev.Type]; !ok {
			first[ev.Task][ev.Type] = ev.TS
		}
	}
	var prompted, chained, woke []time.Duration
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TASK\tSPAWN→PROMPTED\tPARENT DONE→SPAWNED\tREPORT→WOKE")
	for _, t := range tasks {
		ev := first[t.ID]
		if ev == nil {
			continue
		}
		p := span(ev["spawned"], ev["prompted"])
		c := chainSpan(t, first)
		k := wokeSpan(t.ID, events)
		prompted, chained, woke = collect(prompted, p), collect(chained, c), collect(woke, k)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.ID, ms(p), ms(c), ms(k))
	}
	fmt.Fprintf(w, "p50/p95\t%s\t%s\t%s\n", percentiles(prompted), percentiles(chained), percentiles(woke))
	return w.Flush()
}

// chainSpan is the time from the last parent's done to the child's spawn.
func chainSpan(t *task.Task, first map[string]map[string]time.Time) time.Duration {
	var last time.Time
	for _, p := range t.After {
		done := first[p]["done"]
		if done.IsZero() {
			return -1
		}
		if done.After(last) {
			last = done
		}
	}
	if last.IsZero() || t.CreatedAt.After(last) {
		return -1 // the parents were done before the child was spawned
	}
	return span(last, first[t.ID]["spawned"])
}

// wokeSpan is the time from the task's first report to the next wake-up
// that included it.
func wokeSpan(id string, events []state.Event) time.Duration {
	var reported time.Time
	for _, ev := range events {
		switch {
		case reported.IsZero() && ev.Task == id && (ev.Type == "done" || ev.Type == "needs-human" || ev.Type == "failed"):
			reported = ev.TS
		case !reported.IsZero() && ev.Type == "woke" && slices.Contains(strings.Split(ev.Summary, ","), id):
			return ev.TS.Sub(reported)
		}
	}
	return -1
}

func span(from, to time.Time) time.Duration {
	if from.IsZero() || to.IsZero() || to.Before(from) {
		return -1
	}
	return to.Sub(from)
}

func collect(list []time.Duration, d time.Duration) []time.Duration {
	if d < 0 {
		return list
	}
	return append(list, d)
}

func ms(d time.Duration) string {
	if d < 0 {
		return "-"
	}
	return fmt.Sprintf("%d", d.Milliseconds())
}

func percentiles(list []time.Duration) string {
	if len(list) == 0 {
		return "-"
	}
	s := slices.Clone(list)
	slices.Sort(s)
	at := func(p float64) time.Duration { return s[min(len(s)-1, int(p*float64(len(s))))] }
	return ms(at(0.5)) + "/" + ms(at(0.95))
}
