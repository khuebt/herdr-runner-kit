package runner

import (
	"context"
	"errors"
	"syscall"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// WatchAgent follows a live runner's agent through herdr until ctx ends or
// the task finishes. An agent that disappears fails the task, and an agent
// blocked on a prompt in its pane emits stalled once per block, unless its
// screen shows a usage limit: then the task switches to its next fallback
// agent.
func (r *Runner) WatchAgent(ctx context.Context, id string) {
	for ctx.Err() == nil {
		t, err := r.Store.Get(id)
		if err != nil || t.Status.Terminal() || t.Herdr.PaneID == "" || t.Herdr.Closed {
			return
		}
		if t.Status == task.Starting {
			// A launcher is restarting the agent after a usage limit.
			if !sleepCtx(ctx, time.Second) {
				return
			}
			continue
		}
		until := "blocked"
		if t.Stalled {
			until = "working"
		}
		timeout := time.Hour
		if dl, ok := ctx.Deadline(); ok {
			timeout = max(time.Until(dl), time.Second)
		}
		status, err := r.Herdr.AgentWait(ctx, t.Herdr.PaneID, timeout, until)
		switch {
		case ctx.Err() != nil:
			return
		case isGone(err):
			if cur, err := r.Store.Get(id); err == nil && cur.Status == task.Starting {
				continue // the agent was quit for a fallback
			}
			if r.usageLimited(ctx, t) && r.switchAgent(t) == nil {
				continue
			}
			r.fail(id, "the agent exited without reporting")
			return
		case err != nil:
			// herdr was briefly unavailable, or the wait timed out.
			if !sleepCtx(ctx, time.Second) {
				return
			}
			continue
		}
		if status == "blocked" && r.usageLimited(ctx, t) && r.switchAgent(t) == nil {
			continue
		}
		r.setStalled(id, status == "blocked")
	}
}

func (r *Runner) setStalled(id string, stalled bool) {
	errSame := errors.New("unchanged")
	t, err := r.Store.Update(id, func(t *task.Task) error {
		if t.Stalled == stalled || t.Status.Terminal() {
			return errSame
		}
		t.Stalled = stalled
		return nil
	})
	if err == nil && stalled {
		r.emit(t, "stalled", "blocked on a prompt in its pane", "")
	}
}

// CheckLaunch fails a task stuck in starting: its launch deadline passed, or
// the launcher process is gone.
func (r *Runner) CheckLaunch(t *task.Task) {
	if t.Status != task.Starting {
		return
	}
	switch {
	case !t.LaunchDeadline.IsZero() && time.Now().After(t.LaunchDeadline):
		r.fail(t.ID, "the launch did not finish in time")
	case t.LauncherPID > 0 && errors.Is(syscall.Kill(t.LauncherPID, 0), syscall.ESRCH):
		r.fail(t.ID, "the launcher exited before the agent started")
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
