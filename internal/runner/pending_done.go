package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// The safety net for a held done. Variables so tests can shorten them.
var (
	// pendingDoneAge is how old a held done must be before the safety net
	// looks at it.
	pendingDoneAge = 2 * time.Minute
	// pendingDoneEvery is how often the safety net reads the task.
	pendingDoneEvery = 15 * time.Second
	// pendingDoneRecheck is how far apart the two reads of a settled agent
	// are.
	pendingDoneRecheck = 5 * time.Second
)

// WatchPendingDone makes a held done final when the turn end that should
// have done so never came: the agent's notify failed, or it crashed. Once
// the done is older than pendingDoneAge and herdr reports the agent idle or
// done on two reads pendingDoneRecheck apart, with no working status line
// on its screen, the done becomes final, superseded or not. A gone agent is
// left to WatchAgent, which finishes the done (see finishHeldDone). A done held for running subagents waits for the turn end their result brings. It returns when the task
// finishes or its runner closes, and at once for an agent whose done is
// never held.
func (r *Runner) WatchPendingDone(ctx context.Context, id string) {
	for sleepCtx(ctx, pendingDoneEvery) {
		t, err := r.Store.Get(id)
		if err != nil || t.Status.Terminal() || t.Herdr.PaneID == "" || t.Herdr.Closed || !doneAtTurnEnd(t) {
			return
		}
		if t.PendingDone == nil || t.PendingDone.Subagents || time.Since(t.PendingDone.Since) < pendingDoneAge {
			continue
		}
		if !r.agentSettled(ctx, t) || !sleepCtx(ctx, pendingDoneRecheck) || !r.agentSettled(ctx, t) {
			continue
		}
		if _, err := r.FinishDone(id, true); err != nil && !errors.Is(err, errNothingHeld) {
			fmt.Fprintf(os.Stderr, "hrk: %s: finish held done: %v\n", id, err)
		}
	}
}

// finishHeldDone makes id's held done final when the task is running and
// holds one no queued message superseded, and reports whether it did. The
// runner finished its work, so an agent that is gone or at its usage limit
// after the done neither fails the task nor hands the work to a fallback.
func (r *Runner) finishHeldDone(id string) bool {
	cur, err := r.Store.Get(id)
	if err != nil || cur.Status != task.Running || cur.PendingDone == nil || cur.PendingDone.Superseded {
		return false
	}
	t, err := r.FinishDone(id, false)
	if err != nil && t == nil {
		return false // a report or a turn end took it first, or the task changed
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "hrk: %s: finish held done: %v\n", id, err)
	}
	return true
}

// agentSettled reports whether herdr sees t's agent idle or done, and its
// screen shows no running turn.
func (r *Runner) agentSettled(ctx context.Context, t *task.Task) bool {
	a, err := r.Herdr.AgentGet(ctx, t.Herdr.PaneID)
	if err != nil || a.Status != "idle" && a.Status != "done" {
		return false
	}
	adapter, ok := adapterOf(t)
	if !ok {
		return false
	}
	screen, err := r.Herdr.PaneRead(ctx, t.Herdr.PaneID, 40)
	return err == nil && !adapter.ShowsWorking(screen)
}
