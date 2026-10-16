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
	"github.com/khuebt/herdr-runner-kit/internal/state"
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
			if deadline, ok := r.selfResolveDeadline(t); ok {
				// A prompt the agent answers itself, seen by this wait or an
				// earlier one: news only once it outlasts its timeout.
				if err := r.awaitSelfResolve(ctx, t, deadline); isGone(err) && !r.agentGone(ctx, t) {
					return
				}
				continue
			}
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
			if !r.agentGone(ctx, t) {
				return
			}
			continue
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

// agentGone handles a runner whose agent herdr no longer has, and reports
// whether to keep watching: a fallback is taking over the pane. Otherwise
// the task fails.
func (r *Runner) agentGone(ctx context.Context, t *task.Task) bool {
	if cur, err := r.Store.Get(t.ID); err == nil && cur.Status == task.Starting {
		return true // the agent was quit for a fallback
	}
	if r.switchable(t) && r.usageLimited(ctx, t) && r.switchAgent(t, false) == nil {
		return true
	}
	left, psErr := r.scopeLeft(ctx, t)
	if failed, ok := r.fail(t.ID, r.goneReason(ctx, t)); ok {
		r.reportScopeLeft(failed, left, psErr)
	}
	return false
}

// selfResolveGrace is how long past its own timeout a self-resolving prompt
// may stay before it is news. A variable so tests can shorten it.
var selfResolveGrace = 30 * time.Second

// onBlocked handles an agent that blocked on a prompt. A prompt the agent
// answers itself is recorded as the progress event blocked, which WatchAgent
// turns into stalled news only when it outlasts its timeout; any other
// prompt is stalled news at once. Both carry the prompt's text.
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
	r.markStalled(t.ID, "blocked", note)
}

// selfResolveDeadline returns when a stalled task's self-resolving prompt
// becomes news: its blocked event plus the longest timeout of the agent's
// self-resolving prompts and selfResolveGrace. ok is false when the task's
// latest block is already news, or its agent has no such prompts.
func (r *Runner) selfResolveDeadline(t *task.Task) (time.Time, bool) {
	adapter, ok := adapterOf(t)
	if !ok {
		return time.Time{}, false
	}
	var timeout time.Duration
	for _, p := range adapter.SelfResolvingPrompts() {
		timeout = max(timeout, p.Timeout)
	}
	ev, ok := r.lastBlock(t.ID)
	if !ok || timeout == 0 || ev.Type != "blocked" {
		return time.Time{}, false
	}
	return ev.TS.Add(timeout + selfResolveGrace), true
}

// awaitSelfResolve waits until deadline for a blocked agent to move on, and
// clears the block when it does. A herdr error only retries; once the
// deadline passed with the agent still blocked, the block becomes stalled
// news with the prompt's text, once. It returns herdr's error when the agent
// is gone.
func (r *Runner) awaitSelfResolve(ctx context.Context, t *task.Task, deadline time.Time) error {
wait:
	for remaining := time.Until(deadline); remaining > 0; remaining = time.Until(deadline) {
		status, err := r.Herdr.AgentWait(ctx, t.Herdr.PaneID, remaining, "working", "idle", "done")
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case isGone(err):
			return err
		case err == nil && status != "blocked":
			r.unstall(t.ID)
			return nil
		case herdr.HasCode(err, herdr.CodeTimeout):
			break wait
		case !sleepCtx(ctx, time.Second):
			return ctx.Err()
		}
	}
	screen, _ := r.Herdr.PaneRead(ctx, t.Herdr.PaneID, 40)
	cur, err := r.Store.Get(t.ID)
	if ev, ok := r.lastBlock(t.ID); err == nil && ok && ev.Type == "blocked" && cur.Stalled && !cur.Status.Terminal() {
		r.emit(cur, "stalled", blockedSummary(screen), "")
	}
	return nil
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
	if ev, ok := r.lastBlock(id); ok {
		summary += " after " + shortSpan(time.Since(ev.TS))
	}
	r.emit(t, "unstalled", summary, "")
}

// lastBlock returns the event that began the task's latest block, or made
// it news: its last stalled or blocked event.
func (r *Runner) lastBlock(id string) (state.Event, bool) {
	all, _, err := r.Store.ReadEventsSince(0)
	if err != nil {
		return state.Event{}, false
	}
	for i := len(all) - 1; i >= 0; i-- {
		if ev := all[i]; ev.Task == id && (ev.Type == "stalled" || ev.Type == "blocked") {
			return ev, true
		}
	}
	return state.Event{}, false
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
	lines := screenLines(screen)
	text := redact.Secrets(strings.Join(lines[max(0, len(lines)-excerptLines):], " / "))
	if r := []rune(text); len(r) > maxExcerpt {
		text = string(r[:maxExcerpt-1]) + "…"
	}
	return text
}

// screenLines returns a screen's non-empty lines, with runs of spaces
// collapsed.
func screenLines(screen string) []string {
	var lines []string
	for _, l := range strings.Split(screen, "\n") {
		if l = strings.Join(strings.Fields(l), " "); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
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
		r.checkRestarted(ctx, t)
		return
	}
	if r.switchable(t) && r.usageLimited(ctx, t) && r.switchAgent(t, false) == nil {
		return
	}
	r.failLost(ctx, t.ID, reason)
}

// checkRestarted reports, once per herdr restart, the processes still
// running in the memory scopes of a runner whose pane herdr restored: its
// agent came back outside them, so they would otherwise go unnoticed until
// the runner closes. A pane shell other than the one the latest scope was
// made under means herdr restarted the pane. It never signals them.
func (r *Runner) checkRestarted(ctx context.Context, t *task.Task) {
	if r.ProcTable == nil || len(t.Scopes) == 0 || t.ShellPID == 0 {
		return
	}
	info, err := r.Herdr.PaneProcessInfo(ctx, t.Herdr.PaneID)
	if err != nil || info.ShellPID <= 0 || info.ShellPID == t.ShellPID {
		return
	}
	left, err := r.scopeLeft(ctx, t)
	if err != nil {
		return // looked at again in a minute
	}
	cur, err := r.Store.Update(t.ID, func(cur *task.Task) error {
		if cur.ShellPID != t.ShellPID {
			return errors.New("another check took this restart")
		}
		cur.ShellPID = info.ShellPID
		return nil
	})
	if err == nil && len(left) > 0 {
		r.emit(cur, "processes-left", survivorSummary(left, "after herdr restarted its pane"), "")
	}
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
	case a.Name != "" && shellOnly(a) && r.stillGone(ctx, t):
		// herdr still names the pane's agent but sees only a shell in it, as
		// after a resume that found no conversation.
		return r.goneReason(ctx, t), true
	}
	return "", false
}

// shellOnly reports whether herdr looked at a pane and found no agent in it.
// Right after a restore herdr reports a live agent with no kind and the
// status unknown until it detects it again, which took 35 s on a loaded
// machine.
func shellOnly(a herdr.Agent) bool {
	return a.Kind == "" && a.Status != "unknown"
}

// goneConfirm is how long a pane herdr sees only a shell in must stay so
// before its runner counts as lost, so that a relaunch that started in
// between, or a detection that is still settling, keeps the runner.
const goneConfirm = 15 * time.Second

// stillGone waits goneConfirm, then reports whether t is still running or
// waiting on the same attempt in the same pane, and herdr still sees no
// agent there.
func (r *Runner) stillGone(ctx context.Context, t *task.Task) bool {
	if !r.pause(ctx, goneConfirm) {
		return false
	}
	cur, err := r.Store.Get(t.ID)
	if err != nil || cur.Status != task.Running && cur.Status != task.NeedsHuman ||
		task.AttemptOf(cur) != task.AttemptOf(t) || cur.Herdr.PaneID != t.Herdr.PaneID {
		return false
	}
	a, err := r.Herdr.AgentGet(ctx, t.Herdr.PaneID)
	return isGone(err) || err == nil && shellOnly(a)
}

// goneReason says why t's agent is gone: its whole pane is gone, or the
// agent exited and left its pane. The latter quotes the agent's last line
// on the screen: the one above the shell prompt it left behind.
func (r *Runner) goneReason(ctx context.Context, t *task.Task) string {
	screen, err := r.Herdr.PaneRead(ctx, t.Herdr.PaneID, 40)
	if herdr.HasCode(err, "pane_not_found") || herdr.HasCode(err, "tab_not_found") {
		return "runner lost: herdr pane gone"
	}
	reason := "the agent exited without reporting"
	if lines := screenLines(screen); err == nil && len(lines) >= 2 {
		reason += ": " + firstLine(redact.Secrets(lines[len(lines)-2]), 160)
	}
	return reason
}

// failLost fails a running or waiting task whose runner is gone, and its
// dependents, and names the processes still running in its memory scopes.
func (r *Runner) failLost(ctx context.Context, id, reason string) {
	var left []proc
	var psErr error
	if cur, err := r.Store.Get(id); err == nil {
		left, psErr = r.scopeLeft(ctx, cur)
	}
	t, changed, err := r.Store.Transition(id, task.Failed, []task.Status{task.Running, task.NeedsHuman}, func(t *task.Task) {
		t.Error = reason
	})
	if err != nil || !changed {
		return
	}
	r.emit(t, "failed", reason, "")
	r.reportScopeLeft(t, left, psErr)
	_ = r.failDependents(t)
}

// errIfLost returns an error naming where t's work is when its runner is
// gone, and fails the task if it was still running or waiting.
func (r *Runner) errIfLost(ctx context.Context, t *task.Task) error {
	reason, lost := r.lostReason(ctx, t)
	if !lost {
		return nil
	}
	r.failLost(ctx, t.ID, reason)
	return fmt.Errorf("%s's runner is gone (%s); its worktree and branch are in hrk show %s", t.ID, reason, t.ID)
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

// pause waits d through Sleep, which tests replace, or until ctx ends, and
// reports whether ctx is still live.
func (r *Runner) pause(ctx context.Context, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Sleep(d)
	}()
	select {
	case <-done:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
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
