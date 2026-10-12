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
// herdr saw the agent start a turn within a few seconds.
func (r *Runner) Send(ctx context.Context, id, text string) (bool, error) {
	t, err := r.liveTask(id)
	if err != nil {
		return false, err
	}
	if err := r.Herdr.AgentPrompt(ctx, t.Herdr.PaneID, text); err != nil {
		return false, err
	}
	if t, err = r.Store.Update(id, func(t *task.Task) error {
		t.Unreported = 0
		return nil
	}); err != nil {
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
	if err := r.closeRunner(ctx, t, removeWorktree); err != nil {
		return nil, err
	}
	return r.Store.Get(id)
}

// GC closes runners whose tasks finished more than olderThan ago, so their
// agents stop using memory. An empty owner means every owner's tasks. It
// returns the ids it closed.
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
		if !t.Herdr.Closed && r.inUse(ctx, t.Herdr.PaneID) {
			continue // someone kept working in the tab after the report; try later
		}
		if err := r.closeRunner(ctx, t, removeWorktree); err != nil {
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
		if (owner != "" && t.Owner != owner) || !collectable(t, false) {
			continue
		}
		if due := finishedAt(t).Add(GCAge); next.IsZero() || due.Before(next) {
			next = due
		}
	}
	return next, !next.IsZero()
}

// collectable reports whether t finished and still has something for GC
// to close: its runner, or with removeWorktree its worktree.
func collectable(t *task.Task, removeWorktree bool) bool {
	return t.Status.Terminal() && t.Herdr.PaneID != "" &&
		(!t.Herdr.Closed || (removeWorktree && t.Worktree != nil && !t.Worktree.Removed))
}

func finishedAt(t *task.Task) time.Time {
	if t.FinishedAt.IsZero() {
		return t.UpdatedAt
	}
	return t.FinishedAt
}

// closeRunner quits the agent gracefully, then closes its tab. A worktree
// runner keeps its workspace, whose pane drops back to a shell, unless
// removeWorktree is set.
func (r *Runner) closeRunner(ctx context.Context, t *task.Task, removeWorktree bool) error {
	if t.Herdr.PaneID == "" {
		return nil
	}
	if !t.Herdr.Closed {
		if err := r.quitAgent(ctx, t.Herdr.PaneID); err != nil {
			return err
		}
		if t.Worktree == nil {
			if err := r.Herdr.TabClose(ctx, t.Herdr.TabID); err != nil && !isGone(err) {
				return err
			}
		}
	}
	removed := false
	if t.Worktree != nil && removeWorktree && !t.Worktree.Removed {
		if err := r.Herdr.WorktreeRemove(ctx, t.Herdr.WorkspaceID, false); err != nil {
			return fmt.Errorf("remove worktree (the branch is kept): %w", err)
		}
		removed = true
	}
	_, err := r.Store.Update(t.ID, func(t *task.Task) error {
		t.Herdr.Closed = true
		if removed {
			t.Worktree.Removed = true
		}
		return nil
	})
	return err
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
