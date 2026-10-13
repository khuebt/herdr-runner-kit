package runner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/harness"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// usageLimitLines is how many of the pane's last non-blank lines are
// searched for a usage-limit phrase. The agent draws the limit at the end of
// its output, so the same words quoted higher up in the conversation never
// count.
const usageLimitLines = 12

// resumeNote follows the first prompt when a runner continues on a
// fallback agent.
const resumeNote = "The previous agent hit its usage limit. The checkout may hold its partial work: " +
	"inspect `git status` and `git log` and continue."

var (
	// errUsageLimit means the agent's screen shows a usage limit.
	errUsageLimit = errors.New("usage limit reached")
	// errSwitched means the task no longer runs the agent the caller saw:
	// another detector switched it, or it moved on.
	errSwitched = errors.New("the agent was already switched")
)

// detectUsageLimit reports whether the last lines of screen show one of the
// adapter's usage-limit phrases.
func detectUsageLimit(a harness.Adapter, screen string) bool {
	lines := strings.Split(strings.TrimRight(screen, " \t\r\n"), "\n")
	if len(lines) > usageLimitLines {
		lines = lines[len(lines)-usageLimitLines:]
	}
	text := harness.Normalize(strings.Join(lines, "\n"))
	for _, p := range a.UsageLimit() {
		if phrase := harness.Normalize(p); phrase != "" && strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

// usageLimited reads t's pane and reports whether its agent shows a usage
// limit. A screen that cannot be read counts as no limit.
func (r *Runner) usageLimited(ctx context.Context, t *task.Task) bool {
	spec, err := task.ParseAgentSpec(t.Agent)
	if err != nil {
		return false
	}
	adapter, err := harness.For(spec.Harness)
	if err != nil {
		return false
	}
	screen, err := r.Herdr.PaneRead(ctx, t.Herdr.PaneID, 40)
	return err == nil && detectUsageLimit(adapter, screen)
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
// Claude config folder no longer exists is skipped.
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
		if key == current || slices.Contains(t.FallbackFrom, key) {
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
// fallback left the task fails with a usage-limit reason, its dependents
// fail too, and claimSwitch returns errHandled.
func (r *Runner) claimSwitch(seen *task.Task, from []task.Status, toStarting bool) (*task.Task, error) {
	errNoFallback := errors.New("no fallback")
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
		// A new agent starts a new Codex thread and a fresh turn count.
		t.CodexThread, t.Unreported, t.Stalled, t.Question, t.Error = "", 0, false, "", ""
		t.LaunchDeadline = time.Now().UTC().Add(LaunchBudget)
		if toStarting {
			t.Status, t.LauncherPID = task.Starting, 0
		}
		return nil
	})
	switch {
	case errors.Is(err, errNoFallback):
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
	r.emit(nt, "agent-switched", prev+" -> "+next, "")
	return nt, nil
}

// switchAgent handles a usage limit seen on a running runner: it claims the
// switch and hands the task to a detached launcher, which quits the agent
// and starts the fallback in the same pane and checkout. It never quits the
// agent itself, because it may run inside that agent's turn-end hook. A
// switch another detector made first, or a task failed for want of a
// fallback, returns nil.
func (r *Runner) switchAgent(t *task.Task) error {
	nt, err := r.claimSwitch(t, []task.Status{task.Running, task.NeedsHuman}, true)
	if errors.Is(err, errSwitched) || errors.Is(err, errHandled) {
		return nil
	}
	if err != nil {
		return err
	}
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

// clearPane quits the agent in a switched runner's pane and sets the
// environment the next agent needs, such as its Claude config folder.
func (r *Runner) clearPane(ctx context.Context, t *task.Task) error {
	quitCtx, cancel := context.WithTimeout(ctx, quitBudget)
	err := r.quitAgent(quitCtx, t.Herdr.PaneID)
	cancel()
	if err != nil {
		return fmt.Errorf("quit the agent that hit its usage limit: %w", err)
	}
	return r.exportEnv(ctx, t.Herdr.PaneID, t, true)
}
