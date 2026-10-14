package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/harness"
	"github.com/khuebt/herdr-runner-kit/internal/herdr"
	"github.com/khuebt/herdr-runner-kit/internal/redact"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// WatchAgent follows a live runner's agent through herdr until ctx ends or
// the task finishes. An agent that disappears fails the task, and an agent
// blocked on a prompt in its pane emits stalled once per block, with the
// prompt's text, unless its screen shows the agent's usage-limit line and a
// fallback agent is left: then the task switches to it. A prompt the agent
// answers itself after a timeout is news only if it outlasts that timeout.
// A block that clears emits unstalled.
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
		until := []string{"blocked"}
		if t.Stalled {
			until = []string{"working", "idle", "done"}
		}
		timeout := time.Hour
		if dl, ok := ctx.Deadline(); ok {
			timeout = max(time.Until(dl), time.Second)
		}
		status, err := r.Herdr.AgentWait(ctx, t.Herdr.PaneID, timeout, until...)
		switch {
		case ctx.Err() != nil:
			return
		case isGone(err):
			if cur, err := r.Store.Get(id); err == nil && cur.Status == task.Starting {
				continue // the agent was quit for a fallback
			}
			if r.switchable(t) && r.usageLimited(ctx, t) && r.switchAgent(t, false) == nil {
				continue
			}
			r.fail(id, r.goneReason(ctx, t))
			return
		case err != nil:
			// herdr was briefly unavailable, or the wait timed out.
			if !sleepCtx(ctx, time.Second) {
				return
			}
			continue
		}
		if status == "blocked" && r.switchable(t) && r.usageLimited(ctx, t) && r.switchAgent(t, false) == nil {
			continue
		}
		if status == "blocked" {
			r.onBlocked(ctx, t)
		} else {
			r.unstall(id)
		}
	}
}

// selfResolveGrace is how long past its own timeout a self-resolving prompt
// may stay before it is news. A variable so tests can shorten it.
var selfResolveGrace = 30 * time.Second

// onBlocked handles an agent that blocked on a prompt. A prompt the agent
// answers itself is recorded as progress, and becomes stalled news only when
// it outlasts its timeout; any other prompt is stalled news at once. Both
// carry the prompt's text.
func (r *Runner) onBlocked(ctx context.Context, t *task.Task) {
	screen, _ := r.Herdr.PaneRead(ctx, t.Herdr.PaneID, 40)
	adapter, ok := adapterOf(t)
	p, self := harness.SelfResolvingPrompt{}, false
	if ok {
		p, self = harness.MatchSelfResolving(adapter, screen)
	}
	if !self {
		r.markStalled(t.ID, "stalled", blockedSummary(screen))
		return
	}
	note := fmt.Sprintf("%s prompt; auto-denies in %d min", p.Name, int(p.Timeout.Round(time.Minute).Minutes()))
	if !r.markStalled(t.ID, "blocked", note) {
		return // another watcher saw it first
	}
	status, err := r.Herdr.AgentWait(ctx, t.Herdr.PaneID, p.Timeout+selfResolveGrace, "working", "idle", "done")
	switch {
	case ctx.Err() != nil || isGone(err):
		return
	case err == nil && status != "blocked":
		r.unstall(t.ID)
		return
	}
	screen, _ = r.Herdr.PaneRead(ctx, t.Herdr.PaneID, 40)
	if cur, err := r.Store.Get(t.ID); err == nil && cur.Stalled && !cur.Status.Terminal() {
		r.emit(cur, "stalled", blockedSummary(screen), "")
	}
}

// markStalled marks the task blocked and emits typ with summary. It reports
// whether this call marked it: false when the task was marked already or
// has finished.
func (r *Runner) markStalled(id, typ, summary string) bool {
	errSame := errors.New("unchanged")
	t, err := r.Store.Update(id, func(t *task.Task) error {
		if t.Stalled || t.Status.Terminal() {
			return errSame
		}
		t.Stalled = true
		return nil
	})
	if err != nil {
		return false
	}
	r.emit(t, typ, summary, "")
	return true
}

// unstall clears a block and emits unstalled with how long it lasted.
func (r *Runner) unstall(id string) {
	errSame := errors.New("unchanged")
	t, err := r.Store.Update(id, func(t *task.Task) error {
		if !t.Stalled || t.Status.Terminal() {
			return errSame
		}
		t.Stalled = false
		return nil
	})
	if err != nil {
		return
	}
	summary := "unblocked"
	if since, ok := r.blockedSince(id); ok {
		summary += " after " + shortSpan(time.Since(since))
	}
	r.emit(t, "unstalled", summary, "")
}

// blockedSince returns when the task's latest block began: its last stalled
// or blocked event.
func (r *Runner) blockedSince(id string) (time.Time, bool) {
	all, _, err := r.Store.ReadEventsSince(0)
	if err != nil {
		return time.Time{}, false
	}
	for i := len(all) - 1; i >= 0; i-- {
		if ev := all[i]; ev.Task == id && (ev.Type == "stalled" || ev.Type == "blocked") {
			return ev.TS, true
		}
	}
	return time.Time{}, false
}

// shortSpan prints a duration as 45s or 2m01s.
func shortSpan(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// Excerpt bounds: the last lines of the visible pane a stalled event quotes,
// and its most characters.
const (
	excerptLines = 6
	maxExcerpt   = 240
)

// blockedSummary says the agent is blocked on a prompt and quotes it.
func blockedSummary(screen string) string {
	if excerpt := promptExcerpt(screen); excerpt != "" {
		return "blocked on a prompt: " + excerpt
	}
	return "blocked on a prompt in its pane"
}

// promptExcerpt quotes the last few non-empty lines of a screen on one
// line, with anything that looks like a secret hidden.
func promptExcerpt(screen string) string {
	var lines []string
	for _, l := range strings.Split(screen, "\n") {
		if l = strings.Join(strings.Fields(l), " "); l != "" {
			lines = append(lines, l)
		}
	}
	text := redact.Secrets(strings.Join(lines[max(0, len(lines)-excerptLines):], " / "))
	if r := []rune(text); len(r) > maxExcerpt {
		text = string(r[:maxExcerpt-1]) + "…"
	}
	return text
}

// CheckLost fails a running or waiting task whose runner herdr no longer
// has, as after a herdr or machine restart: its pane is gone, its agent left
// the pane, or the pane now holds another agent. An agent that exited on a
// usage limit switches to its fallback instead. The task's dependents fail
// with it. An unreachable herdr gives no verdict.
func (r *Runner) CheckLost(ctx context.Context, t *task.Task) {
	if t.Status != task.Running && t.Status != task.NeedsHuman || t.Herdr.PaneID == "" || t.Herdr.Closed {
		return
	}
	reason, lost := r.lostReason(ctx, t)
	if !lost {
		return
	}
	if r.switchable(t) && r.usageLimited(ctx, t) && r.switchAgent(t, false) == nil {
		return
	}
	r.failLost(t.ID, reason)
}

// lostReason reports whether t's runner is gone from herdr, and why.
func (r *Runner) lostReason(ctx context.Context, t *task.Task) (string, bool) {
	a, err := r.Herdr.AgentGet(ctx, t.Herdr.PaneID)
	switch {
	case isGone(err):
		return r.goneReason(ctx, t), true
	case err != nil:
		return "", false
	case t.Herdr.AgentName != "" && a.Name != "" && a.Name != t.Herdr.AgentName:
		return "runner lost: herdr pane " + t.Herdr.PaneID + " now holds another agent", true
	}
	return "", false
}

// goneReason says why t's agent is gone: its whole pane is gone, or the
// agent exited and left its pane.
func (r *Runner) goneReason(ctx context.Context, t *task.Task) string {
	if _, err := r.Herdr.PaneRead(ctx, t.Herdr.PaneID, 1); herdr.HasCode(err, "pane_not_found") || herdr.HasCode(err, "tab_not_found") {
		return "runner lost: herdr pane gone"
	}
	return "the agent exited without reporting"
}

// failLost fails a running or waiting task whose runner is gone, and its
// dependents.
func (r *Runner) failLost(id, reason string) {
	t, changed, err := r.Store.Transition(id, task.Failed, []task.Status{task.Running, task.NeedsHuman}, func(t *task.Task) {
		t.Error = reason
	})
	if err != nil || !changed {
		return
	}
	r.emit(t, "failed", reason, "")
	_ = r.failDependents(t)
}

// errIfLost returns an error naming where t's work is when its runner is
// gone, and fails the task if it was still running or waiting.
func (r *Runner) errIfLost(ctx context.Context, t *task.Task) error {
	reason, lost := r.lostReason(ctx, t)
	if !lost {
		return nil
	}
	r.failLost(t.ID, reason)
	return fmt.Errorf("%s's runner is gone (%s); its worktree and branch are in herdr-runner-kit show %s", t.ID, reason, t.ID)
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
