package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/herdr"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// GCAge is how long a finished runner stays open before gc closes it.
const GCAge = 30 * time.Minute

// Send types text into a live runner as a new prompt, and reports whether
// herdr saw the agent start a turn within a few seconds. A message to a
// finished runner is new work, so the task is reopened, but only after the
// prompt went in: a failed prompt leaves the task finished, and the backstop
// never mistakes its idle agent for a failure.
func (r *Runner) Send(ctx context.Context, id, text string) (bool, error) {
	t, err := r.liveTask(id)
	if err != nil {
		return false, err
	}
	if err := r.Herdr.AgentPrompt(ctx, t.Herdr.PaneID, text); err != nil {
		return false, err
	}
	errClosing := errors.New("closing")
	if t, err = r.Store.Update(id, func(t *task.Task) error {
		if t.Herdr.Closed || claimLive(t, time.Now()) {
			return errClosing
		}
		t.Unreported = 0
		// Reopen the task, so its next report counts, unreported turns are
		// nudged, and wait watches it.
		if t.Status.Terminal() {
			t.Status, t.FinishedAt, t.Error = task.Running, time.Time{}, ""
		}
		return nil
	}); errors.Is(err, errClosing) {
		return false, fmt.Errorf("%s was closing while the prompt was sent; it may not run", id)
	} else if err != nil {
		return false, err
	}
	r.emit(t, "sent", firstLine(text, 120), "")
	_, err = r.Herdr.AgentWait(ctx, t.Herdr.PaneID, 3*time.Second, "working")
	return err == nil, nil
}

// Answer sends the user's answer to a runner that reported needs-human and
// sets it running again. It reports whether herdr saw the turn start.
func (r *Runner) Answer(ctx context.Context, id, text string) (bool, error) {
	t, err := r.liveTask(id)
	if err != nil {
		return false, err
	}
	if t.Status != task.NeedsHuman {
		return false, fmt.Errorf("%s is %s, not waiting for an answer; use send", id, t.Status)
	}
	if t.Question == "" {
		return false, fmt.Errorf("%s waits on the %s dialog in its tab; answer it there (herdr-runner-kit focus %s)", id, t.Error, id)
	}
	if err := r.Herdr.AgentPrompt(ctx, t.Herdr.PaneID, text); err != nil {
		return false, err
	}
	nt, changed, err := r.Store.Transition(id, task.Running, []task.Status{task.NeedsHuman}, func(t *task.Task) {
		t.Question, t.Unreported = "", 0
	})
	if err != nil {
		return false, err
	}
	if changed {
		r.emit(nt, "answered", firstLine(text, 120), "")
	}
	_, err = r.Herdr.AgentWait(ctx, t.Herdr.PaneID, 3*time.Second, "working")
	return err == nil, nil
}

// Release opens a held task's human gate and launches it. A note is added
// to its prompt.
func (r *Runner) Release(id, note string) (*task.Task, error) {
	t, err := r.Store.Get(id)
	if err != nil {
		return nil, err
	}
	if t.Status != task.Held {
		return nil, fmt.Errorf("%s is %s, not held", id, t.Status)
	}
	if note = strings.TrimSpace(note); note != "" {
		if _, err := r.Store.Update(id, func(t *task.Task) error {
			t.Prompt = strings.TrimSpace(t.Prompt + "\n\nNote from the user: " + note)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return r.StartLaunch(id)
}

// Focus switches the herdr UI to the runner.
func (r *Runner) Focus(ctx context.Context, id string) error {
	t, err := r.liveTask(id)
	if err != nil {
		return err
	}
	return r.Herdr.AgentFocus(ctx, t.Herdr.PaneID)
}

func (r *Runner) liveTask(id string) (*task.Task, error) {
	t, err := r.Store.Get(id)
	if err != nil {
		return nil, err
	}
	if t.Herdr.PaneID == "" || t.Herdr.Closed {
		return nil, fmt.Errorf("%s has no open runner (status %s)", id, t.Status)
	}
	if claimLive(t, time.Now()) {
		return nil, fmt.Errorf("%s's tab is closing; spawn a new runner", id)
	}
	return t, nil
}

// Stop ends a task's agent and marks the task stopped. A worktree task keeps
// its workspace and branch unless removeWorktree is set; removal refuses a
// dirty worktree, and the branch is never deleted.
func (r *Runner) Stop(ctx context.Context, id string, removeWorktree bool) (*task.Task, error) {
	// Mark it stopped before the agent quits, or a running wait's backstop
	// sees the agent go first and reports a failure.
	t, changed, err := r.Store.SetStatus(id, task.Stopped)
	if err != nil {
		return nil, err
	}
	if changed {
		r.emit(t, "stopped", "stopped by the user", "")
		_ = r.failDependents(t)
	}
	if err := r.closeRunner(ctx, t, removeWorktree, closeCheck{}); err != nil {
		return nil, err
	}
	return r.Store.Get(id)
}

// Keep holds a runner's tab open after its task finishes, so gc leaves it
// until keep is turned off or the task is stopped. Turning keep off on a
// runner already past its close time closes it at once.
func (r *Runner) Keep(ctx context.Context, id string, on bool) (*task.Task, error) {
	t, err := r.Store.Update(id, func(t *task.Task) error {
		if on && t.Herdr.Closed {
			return fmt.Errorf("%s has no open runner (status %s)", id, t.Status)
		}
		t.Keep = on
		return nil
	})
	if err != nil {
		return nil, err
	}
	if at, ok := CloseAt(t); on || !ok || time.Now().Before(at) {
		return t, nil
	}
	if _, err := r.GC(ctx, t.Owner, GCAge, false); err != nil {
		return nil, fmt.Errorf("close %s: %w", id, err)
	}
	return r.Store.Get(id)
}

// GC closes runners whose tasks finished more than olderThan ago, so their
// agents stop using memory. Kept runners stay open unless removeWorktree is
// set. An empty owner means every owner's tasks. It returns the ids it closed.
func (r *Runner) GC(ctx context.Context, owner string, olderThan time.Duration, removeWorktree bool) ([]string, error) {
	tasks, err := r.Store.List()
	if err != nil {
		return nil, err
	}
	var closed []string
	var errs []error
	cutoff := time.Now().Add(-olderThan)
	for _, t := range tasks {
		if (owner != "" && t.Owner != owner) || !collectable(t, removeWorktree) || finishedAt(t).After(cutoff) {
			continue
		}
		err := r.closeRunner(ctx, t, removeWorktree, closeCheck{gc: true, cutoff: cutoff, keep: !removeWorktree})
		if errors.Is(err, errReopened) {
			continue // reopened, kept or busy since the list; try later
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.ID, err))
			continue
		}
		closed = append(closed, t.ID)
	}
	return closed, errors.Join(errs...)
}

// GCRetry is how long callers leave a runner that GC skipped or could not
// close before they try again.
const GCRetry = time.Minute

// inUse reports whether the agent in pane is busy: working, or blocked on a
// prompt someone may be answering.
func (r *Runner) inUse(ctx context.Context, pane string) bool {
	a, err := r.Herdr.AgentGet(ctx, pane)
	return err == nil && (a.Status == "working" || a.Status == "blocked")
}

// NextGC returns when GC can first close one of these tasks' finished
// runners, and false when none is open. A non-empty owner limits it to that
// owner's tasks.
func NextGC(tasks []*task.Task, owner string) (time.Time, bool) {
	var next time.Time
	for _, t := range tasks {
		if owner != "" && t.Owner != owner {
			continue
		}
		if due, ok := CloseAt(t); ok && (next.IsZero() || due.Before(next)) {
			next = due
		}
	}
	return next, !next.IsZero()
}

// CloseAt returns when GC closes t's runner, and false when it never will on
// its own: the task is not finished, its tab is closed, or it is kept.
func CloseAt(t *task.Task) (time.Time, bool) {
	if !collectable(t, false) {
		return time.Time{}, false
	}
	return finishedAt(t).Add(GCAge), true
}

// collectable reports whether t finished and still has something for GC
// to close: its runner unless it is kept, or with removeWorktree its runner
// or its worktree.
func collectable(t *task.Task, removeWorktree bool) bool {
	if !t.Status.Terminal() || t.Herdr.PaneID == "" {
		return false
	}
	if removeWorktree {
		return !t.Herdr.Closed || (t.Worktree != nil && !t.Worktree.Removed)
	}
	return !t.Herdr.Closed && !t.Keep
}

func finishedAt(t *task.Task) time.Time {
	if t.FinishedAt.IsZero() {
		return t.UpdatedAt
	}
	return t.FinishedAt
}

// closeClaimTTL is how long a close claim holds. Quitting an agent takes
// seconds, so a claim older than this was abandoned: its gc was cut off, or
// its process died.
const closeClaimTTL = time.Minute

var (
	// errAlreadyClosed means the runner was closed before; only its
	// worktree may be left to remove.
	errAlreadyClosed = errors.New("the runner is already closed")
	// errReopened means the runner may not be closed now: it is in use
	// again, kept, not due, or another close is under way.
	errReopened = errors.New("the runner is in use")
)

// closeCheck says what a close re-checks under the task lock. Stop and
// cleanup use the zero value: any finished runner closes.
type closeCheck struct {
	gc     bool      // also check that the agent is idle
	cutoff time.Time // close only a task that finished by then
	keep   bool      // leave a kept runner open
}

// claimLive reports whether a close claim on t is still held. The window
// spans both sides of now, so a clock that steps back cannot stretch it.
func claimLive(t *task.Task, now time.Time) bool {
	if t.Herdr.ClosingSince.IsZero() {
		return false
	}
	d := now.Sub(t.Herdr.ClosingSince)
	return d > -closeClaimTTL && d < closeClaimTTL
}

// claimClose marks id's runner as closing, after checking under the task
// lock that it may still close: the list the caller worked from may be stale.
func (r *Runner) claimClose(id string, c closeCheck) error {
	_, err := r.Store.Update(id, func(t *task.Task) error {
		now := time.Now()
		switch {
		case t.Herdr.Closed:
			return errAlreadyClosed
		case !t.Status.Terminal():
			return fmt.Errorf("%w: it is %s", errReopened, t.Status)
		case c.keep && t.Keep:
			return fmt.Errorf("%w: it is kept open", errReopened)
		case !c.cutoff.IsZero() && finishedAt(t).After(c.cutoff):
			return fmt.Errorf("%w: it finished again recently", errReopened)
		case claimLive(t, now):
			return fmt.Errorf("%w: its tab is already closing", errReopened)
		}
		t.Herdr.ClosingSince = now.UTC()
		return nil
	})
	return err
}

// endClaim drops the close claim on id, and with closed marks the runner
// closed in the same write.
func (r *Runner) endClaim(id string, closed bool) error {
	_, err := r.Store.Update(id, func(t *task.Task) error {
		t.Herdr.ClosingSince = time.Time{}
		t.Herdr.Closed = t.Herdr.Closed || closed
		return nil
	})
	return err
}

// closeRunner claims the runner, quits the agent gracefully, then closes its
// tab. A worktree runner keeps its workspace, whose pane drops back to a
// shell, unless removeWorktree is set. A runner closed before still has its
// worktree removed. It fails with errReopened when the runner may not close.
func (r *Runner) closeRunner(ctx context.Context, t *task.Task, removeWorktree bool, c closeCheck) error {
	if t.Herdr.PaneID == "" {
		return nil
	}
	switch err := r.claimClose(t.ID, c); {
	case errors.Is(err, errAlreadyClosed):
	case err != nil:
		return err
	default:
		if err := r.endAgent(ctx, t, c.gc); err != nil {
			if clearErr := r.endClaim(t.ID, false); clearErr != nil {
				return errors.Join(err, clearErr)
			}
			return err
		}
		// Closed is written only once the agent is gone.
		if err := r.endClaim(t.ID, true); err != nil {
			return err
		}
	}
	if t.Worktree == nil || !removeWorktree {
		return nil
	}
	current, err := r.Store.Get(t.ID)
	if err != nil {
		return err
	}
	if current.Worktree == nil || current.Worktree.Removed {
		return nil
	}
	if err := r.removeWorktree(ctx, current, false); err != nil {
		return fmt.Errorf("remove worktree (the branch is kept): %w", err)
	}
	_, err = r.Store.Update(t.ID, func(t *task.Task) error {
		t.Worktree.Removed = true
		return nil
	})
	return err
}

// endAgent quits the agent and closes its tab. With gc it first leaves an
// agent someone is still using, which keeps working after its report.
func (r *Runner) endAgent(ctx context.Context, t *task.Task, gc bool) error {
	if gc && r.inUse(ctx, t.Herdr.PaneID) {
		return fmt.Errorf("%w: its agent is busy", errReopened)
	}
	if err := r.quitAgent(ctx, t.Herdr.PaneID); err != nil {
		return err
	}
	if t.Worktree == nil {
		if err := r.Herdr.TabClose(ctx, t.Herdr.TabID); err != nil && !isGone(err) {
			return err
		}
	}
	return nil
}

// quitAgent asks the agent to exit with Ctrl+C pairs, the way a user would.
// Killing a Codex process directly can wedge the shared Codex daemon, so
// the agent is never signalled.
func (r *Runner) quitAgent(ctx context.Context, pane string) error {
	for range 3 {
		a, err := r.Herdr.AgentGet(ctx, pane)
		if isGone(err) || (err == nil && a.Kind == "") {
			return nil
		}
		if err != nil {
			return err
		}
		if err := r.Herdr.AgentSendKeys(ctx, pane, "ctrl+c", "ctrl+c"); err != nil && !isGone(err) {
			return err
		}
		_, err = r.Herdr.AgentWait(ctx, pane, 3*time.Second, "unknown")
		if isGone(err) {
			return nil
		}
	}
	return fmt.Errorf("the agent in %s did not exit after Ctrl+C", pane)
}

// isGone reports whether herdr says the agent or pane no longer exists.
func isGone(err error) bool {
	if err == nil {
		return false
	}
	return herdr.HasCode(err, herdr.CodeAgentNotRunning) || herdr.HasCode(err, herdr.CodeAgentNotFound) ||
		herdr.HasCode(err, "pane_not_found") || herdr.HasCode(err, "tab_not_found")
}

func firstLine(s string, n int) string {
	line := s
	for i, c := range s {
		if c == '\n' {
			line = s[:i]
			break
		}
	}
	if len(line) > n {
		return line[:n] + "…"
	}
	return line
}
