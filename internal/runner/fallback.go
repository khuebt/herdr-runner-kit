package runner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/harness"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// resumeNote follows the first prompt when a runner continues on a
// fallback agent.
const resumeNote = "The previous agent hit its usage limit. The checkout may hold its partial work: " +
	"inspect `git status` and `git log` and continue."

var (
	// errUsageLimit means the agent's screen shows a usage limit and a
	// fallback agent is left to switch to.
	errUsageLimit = errors.New("usage limit reached")
	// errSwitched means the task no longer runs the agent the caller saw:
	// another detector switched it, or it moved on.
	errSwitched = errors.New("the agent was already switched")
	// errNoFallback means no fallback agent is left for a limit seen only
	// on the screen, which never fails the task.
	errNoFallback = errors.New("no fallback agent left")
)

// usageLimited reads t's pane and reports whether its agent shows its own
// usage-limit line. A screen that cannot be read counts as no limit.
func (r *Runner) usageLimited(ctx context.Context, t *task.Task) bool {
	adapter, ok := adapterOf(t)
	if !ok {
		return false
	}
	screen, err := r.Herdr.PaneRead(ctx, t.Herdr.PaneID, 40)
	return err == nil && adapter.ShowsUsageLimit(screen)
}

// adapterOf returns the adapter of t's agent, and false for an agent the
// kit cannot read.
func adapterOf(t *task.Task) (harness.Adapter, bool) {
	spec, err := task.ParseAgentSpec(t.Agent)
	if err != nil {
		return nil, false
	}
	adapter, err := harness.For(spec.Harness)
	return adapter, err == nil
}

// switchable reports whether t has a fallback agent left to switch to.
func (r *Runner) switchable(t *task.Task) bool {
	_, _, ok := r.nextFallback(t)
	return ok
}

// agentKey names an agent as FallbackFrom records it: the agent spec, and
// for Claude its config folder when it is not the default one.
func agentKey(agent, claudeDir string) string {
	if spec, err := task.ParseAgentSpec(agent); err == nil && spec.Harness == task.Claude && claudeDir != "" {
		return agent + "@" + claudeDir
	}
	return agent
}

// nextFallback returns the first configured fallback that is neither t's
// current agent nor one that already hit its limit on t. An entry whose
// Claude config folder no longer exists is skipped, and so is the writer's
// agent for a review task.
func (r *Runner) nextFallback(t *task.Task) (agent, claudeDir string, ok bool) {
	current := agentKey(t.Agent, t.ClaudeConfigDir)
	for _, f := range r.Config.QuotaFallback {
		spec, err := task.ParseAgentSpec(f.Agent)
		if err != nil {
			continue
		}
		dir := t.ClaudeConfigDir
		if f.ClaudeConfig != "" {
			if dir, err = ClaudeConfigDir(f.ClaudeConfig); err != nil {
				continue
			}
		}
		key := agentKey(spec.String(), dir)
		if key == current || slices.Contains(t.FallbackFrom, key) || (t.Review != nil && key == t.Review.Writer) {
			continue
		}
		return spec.String(), dir, true
	}
	return "", "", false
}

// claimSwitch moves the task seen by a detector to its next fallback agent,
// under the task lock. Only the first detector that saw a given agent
// switches it; later ones get errSwitched. With toStarting the task goes
// back to starting for a launcher to restart the agent in its pane. With no
// fallback left, a limit the agent confirmed fails the task with a
// usage-limit reason, fails its dependents too and returns errHandled; an
// unconfirmed one returns errNoFallback and changes nothing. A running task
// that holds a done finishes it instead and returns errHandled.
func (r *Runner) claimSwitch(seen *task.Task, from []task.Status, toStarting, confirmed bool) (*task.Task, error) {
	if slices.Contains(from, task.Running) && r.finishHeldDone(seen.ID) {
		return nil, errHandled
	}
	var prev, next string
	nt, err := r.Store.Update(seen.ID, func(t *task.Task) error {
		if !slices.Contains(from, t.Status) || t.Agent != seen.Agent ||
			t.ClaudeConfigDir != seen.ClaudeConfigDir || len(t.FallbackFrom) != len(seen.FallbackFrom) {
			return errSwitched
		}
		prev = agentKey(t.Agent, t.ClaudeConfigDir)
		agent, dir, ok := r.nextFallback(t)
		if !ok {
			return errNoFallback
		}
		next = agentKey(agent, dir)
		t.FallbackFrom = append(t.FallbackFrom, prev)
		t.Agent, t.ClaudeConfigDir = agent, dir
		t.Relaunched = task.RelaunchUsageLimit
		// A new agent starts a new Codex thread and a fresh turn count.
		t.CodexThread, t.Unreported, t.APIErrors, t.Stalled, t.Question, t.Error = "", 0, 0, false, "", ""
		t.Background = nil
		t.LaunchDeadline = time.Now().UTC().Add(LaunchBudget)
		if toStarting {
			t.Status, t.LauncherPID = task.Starting, 0
		}
		return nil
	})
	switch {
	case errors.Is(err, errNoFallback) && confirmed:
		reason := "usage limit reached for " + prev + "; no fallback configured"
		if len(r.Config.QuotaFallback) > 0 {
			reason = "usage limit reached for " + prev + "; every quotaFallback agent was tried"
		}
		r.fail(seen.ID, reason)
		if failed, err := r.Store.Get(seen.ID); err == nil && failed.Status == task.Failed {
			_ = r.failDependents(failed)
		}
		return nil, errHandled
	case err != nil:
		return nil, err
	}
	// The fallback is a fresh session: a handoff request the previous one
	// got does not apply to it.
	r.clearHandoffAsk(nt.ID)
	r.emit(nt, "agent-switched", prev+" -> "+next, "")
	return nt, nil
}

// switchAgent handles a usage limit seen on a running runner: it claims the
// switch and hands the task to a detached launcher, which quits the agent
// and starts the fallback in the same pane and checkout. It never quits the
// agent itself, because it may run inside that agent's turn-end hook. A
// switch another detector made first, or a task failed for want of a
// fallback, returns nil. A limit only seen on the screen (confirmed false)
// with no fallback left returns errNoFallback.
func (r *Runner) switchAgent(t *task.Task, confirmed bool) error {
	nt, err := r.claimSwitch(t, []task.Status{task.Running, task.NeedsHuman}, true, confirmed)
	if errors.Is(err, errSwitched) || errors.Is(err, errHandled) {
		return nil
	}
	if err != nil {
		return err
	}
	return r.relaunch(nt)
}

// relaunch hands nt, which a switch or a handoff just moved back to
// starting, to a detached launcher that quits the agent in its pane and
// starts the next session there.
func (r *Runner) relaunch(nt *task.Task) error {
	pid, err := r.StartLauncher(nt.ID)
	if err != nil {
		r.fail(nt.ID, "start launcher: "+err.Error())
		return fmt.Errorf("start launcher: %w", err)
	}
	_, err = r.Store.Update(nt.ID, func(t *task.Task) error {
		if t.Status == task.Starting && t.LauncherPID == 0 {
			t.LauncherPID = pid
		}
		return nil
	})
	return err
}

// clearPane quits the agent in a relaunched runner's pane and sets the
// environment the next agent needs, such as its Claude config folder and
// its attempt. It returns the processes of the previous session that still
// run after the quit, which it never signals.
func (r *Runner) clearPane(ctx context.Context, t *task.Task) ([]proc, error) {
	before, _ := r.snapshotProcs(ctx, t)
	quitCtx, cancel := context.WithTimeout(ctx, quitBudget)
	err := r.quitAgent(quitCtx, t.Herdr.PaneID)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("quit the previous agent session: %w", err)
	}
	if err := r.exportEnv(ctx, t.Herdr.PaneID, t, true); err != nil {
		return nil, err
	}
	left := r.waitSurvivors(ctx, t, before)
	if len(left) > 0 {
		r.emit(t, "processes-carried", fmt.Sprintf("%d of the previous session's processes still run; the next session was told to stop or reuse them: %s", len(left), procList(left)), "")
	}
	return left, nil
}
