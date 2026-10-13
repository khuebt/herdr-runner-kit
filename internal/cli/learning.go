package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// learning holds one task's numbers, derived from its events.
type learning struct {
	attempts, nudges, sends, answers int
	needsHuman, unreported, switches int
	spawned, ended                   time.Time
	waited                           time.Duration
	waitingSince                     time.Time
	awaitingAnswer                   bool
	lastStatus                       task.Status
}

// observe folds one of the task's events, in log order, into its numbers.
func (l *learning) observe(ev state.Event) {
	switch ev.Type {
	case "spawned":
		if l.spawned.IsZero() {
			l.spawned = ev.TS
		}
	case "started":
		l.attempts++
		l.tabAnswer()
	case "sent":
		l.sends++
		if l.lastStatus.Terminal() && ev.Status != "" && !ev.Status.Terminal() {
			// The send reopened a finished task, which has not finished
			// again yet.
			l.attempts++
			l.ended = time.Time{}
		}
		l.awaitingAnswer = false
	case "answered":
		l.answers++
		l.awaitingAnswer = false
	case "needs-human":
		l.needsHuman++
		l.tabAnswer()
		l.awaitingAnswer = true
	case "done":
		l.tabAnswer()
		l.ended = ev.TS
	case "failed", "stopped":
		l.awaitingAnswer = false
		l.ended = ev.TS
	case "nudged":
		l.nudges++
	case "turn-ended-unreported":
		l.unreported++
	case "agent-switched":
		l.switches++
		l.awaitingAnswer = false
	}
	if ev.Status == "" {
		return
	}
	// Waiting counts from the spawn on, so it stays within the duration:
	// time held or queued before it is not the runner's.
	waiting := !l.spawned.IsZero() && (ev.Status == task.NeedsHuman || ev.Status == task.Held)
	switch {
	case waiting && l.waitingSince.IsZero():
		l.waitingSince = ev.TS
	case !waiting && !l.waitingSince.IsZero():
		l.waited += ev.TS.Sub(l.waitingSince)
		l.waitingSince = time.Time{}
	}
	l.lastStatus = ev.Status
}

// tabAnswer counts a question the user answered in the runner's tab: the
// runner moved on after needs-human with no answer or send from the kit.
func (l *learning) tabAnswer() {
	if l.awaitingAnswer {
		l.answers++
		l.awaitingAnswer = false
	}
}

// printLearning prints, per task with events, how much steering it took:
// attempts (starts plus sends that reopened a finished task), nudges,
// sends, answers (from the main session or in the tab), needs-human
// reports, unreported turns, agent switches, the time from spawn to its last
// finish, and the time it waited on a human after its spawn. Every number
// comes from the events given; tasks only add the kind, agents and current
// status. ids, when set, limits the rows.
func printLearning(out io.Writer, events []state.Event, tasks []*task.Task, ids []string, now time.Time) error {
	byID := map[string]*task.Task{}
	for _, t := range tasks {
		byID[t.ID] = t
	}
	var order []string
	rows := map[string]*learning{}
	for _, ev := range events {
		if ev.Task == "" || (len(ids) > 0 && !slices.Contains(ids, ev.Task)) {
			continue
		}
		l := rows[ev.Task]
		if l == nil {
			l = &learning{}
			rows[ev.Task] = l
			order = append(order, ev.Task)
		}
		l.observe(ev)
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TASK\tKIND\tAGENTS\tSTATUS\tATTEMPTS\tNUDGES\tSENDS\tANSWERS\tNEEDS-HUMAN\tUNREPORTED\tSWITCHES\tDURATION\tWAITED")
	for _, id := range order {
		l := rows[id]
		kind, agents, status := "-", "-", l.lastStatus
		if t := byID[id]; t != nil {
			kind, status = t.Kind, t.Status
			agents = strings.Join(append(slices.Clone(t.FallbackFrom), t.Agent), "->")
		}
		waited := l.waited
		if !l.waitingSince.IsZero() && (status == task.NeedsHuman || status == task.Held) {
			waited += now.Sub(l.waitingSince)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\n",
			id, kind, agents, orDash(string(status)), l.attempts, l.nudges, l.sends, l.answers,
			l.needsHuman, l.unreported, l.switches, roundedSpan(l.spawned, l.ended), rounded(waited))
	}
	return w.Flush()
}

func roundedSpan(from, to time.Time) string {
	if from.IsZero() || to.IsZero() || to.Before(from) {
		return "-"
	}
	return rounded(to.Sub(from))
}

func rounded(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	return d.Round(time.Second).String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
