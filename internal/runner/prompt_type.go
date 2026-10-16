package runner

import (
	"context"
	"fmt"
	"strings"

	"github.com/khuebt/herdr-runner-kit/internal/harness"
	"github.com/khuebt/herdr-runner-kit/internal/redact"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// InputLocked reports whether t's runner takes prompts only from the kit:
// it was spawned with runnerInput locked, and its current agent can lock
// its input.
func InputLocked(t *task.Task) bool {
	a, ok := adapterOf(t)
	return t.InputLocked && ok && a.LocksInput()
}

// typePrompt types text into t's agent in pane as a prompt; every prompt
// the kit types goes through it. Into a runner with locked input it first
// clears the input box, where the agent puts back a prompt the gate
// blocked, and then lets one prompt through the gate. The box is left as
// it is while an earlier kit prompt never reached the gate, as when its
// Enter was lost: that prompt still sits there, and goes in with this one.
// A prompt that fails takes its token back, so no window stays open for a
// prompt typed in the tab.
func (r *Runner) typePrompt(ctx context.Context, t *task.Task, pane, text string) error {
	if !InputLocked(t) {
		return r.Herdr.AgentPrompt(ctx, pane, text)
	}
	adapter, _ := adapterOf(t)
	if keys := adapter.ClearInputKeys(); len(keys) > 0 && !r.promptPending(t.ID) {
		// Best effort: a prompt joined to leftover text is blocked and
		// stays visible in the tab.
		_ = r.Herdr.AgentSendKeys(ctx, pane, keys...)
	}
	if err := r.Store.AddPromptToken(t.ID); err != nil {
		return fmt.Errorf("let the prompt through the input lock: %w", err)
	}
	if err := r.Herdr.AgentPrompt(ctx, pane, text); err != nil {
		_ = r.Store.ReturnPromptToken(t.ID)
		return err
	}
	return nil
}

// promptPending reports whether task id's last kit prompt never reached
// the gate. An error counts as none, so the box is cleared as usual.
func (r *Runner) promptPending(id string) bool {
	pending, err := r.Store.PromptPending(id)
	return err == nil && pending
}

// blockedHead is how many characters of a blocked prompt the input-blocked
// event keeps.
const blockedHead = 60

// GatePrompt is the prompt gate of task id's locked runner, fed the
// agent's prompt-submit hook input. It lets the prompt through when the
// agent says it is its own, or when the kit left a token for it; otherwise
// it records an input-blocked event and returns the adapter's block reply,
// which tells the human where to ask instead. An error lets the prompt
// through: the lock prevents accidents, and must never cut the kit off from
// its own runner.
func (r *Runner) GatePrompt(id string, adapter harness.Adapter, payload []byte) (string, error) {
	prompt, synthetic, err := adapter.GatePrompt(payload)
	if err != nil || synthetic {
		return "", err
	}
	t, err := r.Store.Get(id)
	if err != nil {
		return "", err
	}
	if taken, err := r.Store.TakePromptToken(t.ID); err != nil || taken {
		return "", err
	}
	head := []rune(strings.Join(strings.Fields(redact.Secrets(prompt)), " "))
	summary := string(head[:min(blockedHead, len(head))])
	if len(head) > blockedHead {
		summary += "…"
	}
	r.emit(t, "input-blocked", summary, "")
	return adapter.GateBlock(fmt.Sprintf("%s is a runner: it takes prompts only from the main session. Ask there, or run: hrk send %s \"…\"", t.ID, t.ID)), nil
}
