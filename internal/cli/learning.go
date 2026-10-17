package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/runner"
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
	// locked says the runner's tab takes no prompts, so it never counts an
	// answer given there.
	locked     bool
	lastStatus task.Status
	// active sums the working rounds, from a start, send or answer to the
	// next report or stop; roundSince is the open round's start.
	active     time.Duration
	roundSince time.Time
	// stalled sums the blocked and stalled spans; stallSince is the open
	// span's start.
	stalled    time.Duration
	stallSince time.Time
	// idleKept is the time from the finish to the closing of a kept tab.
	idleKept time.Duration
	kept     bool
}

// observe folds one of the task's events, in log order, into its numbers.
func (l *learning) observe(ev state.Event) {
	switch ev.Type {
	case "started", "sent", "answered":
		if l.roundSince.IsZero() && (ev.Status == "" || !ev.Status.Terminal()) {
			l.roundSince = ev.TS
		}
	case "done", "needs-human", "failed", "stopped":
		l.endRound(ev.TS)
	case "stalled", "blocked":
		if l.stallSince.IsZero() {
			l.stallSince = ev.TS
		}
	case "unstalled":
		l.endStall(ev.TS)
	case "closed":
		if runner.KeptClosed(ev) && !l.ended.IsZero() && ev.TS.After(l.ended) {
			l.idleKept += ev.TS.Sub(l.ended)
			l.kept = true
		}
	}
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

// endRound closes the open working round, and any stall in it, at ts.
func (l *learning) endRound(ts time.Time) {
	if !l.roundSince.IsZero() {
		l.active += ts.Sub(l.roundSince)
		l.roundSince = time.Time{}
	}
	l.endStall(ts)
}

// endStall closes the open stall span at ts.
func (l *learning) endStall(ts time.Time) {
	if !l.stallSince.IsZero() {
		l.stalled += ts.Sub(l.stallSince)
		l.stallSince = time.Time{}
	}
}

// tabAnswer counts a question the user answered in the runner's tab: the
// runner moved on after needs-human with no answer or send from the kit.
// A locked tab takes no answer, so its runner moved on by itself.
func (l *learning) tabAnswer() {
	if l.awaitingAnswer && !l.locked {
		l.answers++
	}
	l.awaitingAnswer = false
}

// printLearning prints, per task with events, how much steering it took:
// attempts (starts plus sends that reopened a finished task), nudges,
// sends, answers (from the main session or in the tab), needs-human
// reports, unreported turns, agent switches, the time from spawn to its last
// finish, the time it worked in rounds (ACTIVE, a "time on the task" that
// includes turns idled without a report), the time blocked or stalled, the
// time a kept tab stayed open after the finish, the time it waited on a
// human after its spawn, and the cost units of its token usage. Open rounds and stalls count up to now. Every number
// comes from the events given; tasks only add the kind, agents and current
// status. ids, when set, limits the rows, and so does project: to the tasks
// of that project.
func printLearning(out io.Writer, events []state.Event, tasks []*task.Task, ids []string, project string, now time.Time) error {
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
		if t := byID[ev.Task]; project != "" && (t == nil || t.Project != project) {
			continue
		}
		l := rows[ev.Task]
		if l == nil {
			l = &learning{}
			if t := byID[ev.Task]; t != nil {
				l.locked = runner.InputLocked(t)
			}
			rows[ev.Task] = l
			order = append(order, ev.Task)
		}
		l.observe(ev)
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TASK\tPROJECT\tKIND\tAGENTS\tSTATUS\tATTEMPTS\tNUDGES\tSENDS\tANSWERS\tNEEDS-HUMAN\tUNREPORTED\tSWITCHES\tDURATION\tACTIVE\tSTALLED\tIDLE-KEPT\tWAITED\tUNITS")
	for _, id := range order {
		l := rows[id]
		proj, kind, agents, status, units := "-", "-", "-", l.lastStatus, "-"
		if t := byID[id]; t != nil {
			proj, kind, status = orDash(t.Project), t.Kind, t.Status
			agents = strings.Join(append(slices.Clone(t.FallbackFrom), t.Agent), "->")
			if t.Usage != nil {
				units = task.Count(t.Usage.Units)
			}
		}
		waited := l.waited
		if !l.waitingSince.IsZero() && (status == task.NeedsHuman || status == task.Held) {
			waited += now.Sub(l.waitingSince)
		}
		active, stalled := l.active, l.stalled
		if !status.Terminal() {
			if !l.roundSince.IsZero() {
				active += now.Sub(l.roundSince)
			}
			if !l.stallSince.IsZero() {
				stalled += now.Sub(l.stallSince)
			}
		}
		idleKept := "-"
		if l.kept {
			idleKept = rounded(l.idleKept)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			id, proj, kind, agents, orDash(string(status)), l.attempts, l.nudges, l.sends, l.answers,
			l.needsHuman, l.unreported, l.switches, roundedSpan(l.spawned, l.ended),
			rounded(active), rounded(stalled), idleKept, rounded(waited), units)
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
