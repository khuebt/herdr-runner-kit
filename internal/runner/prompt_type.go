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
// it is only while it provably holds the text of an earlier kit prompt that
// never reached the gate, as when its Enter was lost: that prompt goes in
// with this one. In every other case the box is cleared, and such a prompt
// is typed again ahead of this one, so neither a kit prompt is lost nor text
// typed in the tab goes through. A prompt that fails takes its token back,
// so no window stays open for a prompt typed in the tab.
func (r *Runner) typePrompt(ctx context.Context, t *task.Task, pane, text string) error {
	if !InputLocked(t) {
		return r.Herdr.AgentPrompt(ctx, pane, text)
	}
	adapter, _ := adapterOf(t)
	// stored is what the box holds once text is typed, for the next prompt
	// to compare it with.
	stored, restore := text, false
	if keys := adapter.ClearInputKeys(); len(keys) > 0 {
		pending := r.pendingPrompt(t.ID)
		if pending != "" && r.boxIs(ctx, adapter, pane, pending) {
			stored = pending + "\n\n" + text
		} else {
			// Best effort: a prompt joined to leftover text is blocked and
			// stays visible in the tab.
			_ = r.Herdr.AgentSendKeys(ctx, pane, keys...)
			if pending != "" {
				text = pending + "\n\n" + text
				stored = text
			}
		}
		if pending != "" {
			// One submit carries both prompts, so one token answers for
			// them; a token left over would make a prompt that reached the
			// gate look pending.
			restore = r.Store.DropPromptTokens(t.ID) == nil
		}
	}
	if err := r.Store.AddPromptToken(t.ID); err != nil {
		return fmt.Errorf("let the prompt through the input lock: %w", err)
	}
	if err := r.Herdr.AgentPrompt(ctx, pane, text); err != nil {
		_ = r.Store.ReturnPromptToken(t.ID)
		if restore {
			// The cleared prompt is still pending: its text was not typed.
			_ = r.Store.AddPromptToken(t.ID)
		}
		return err
	}
	_ = r.Store.SetPendingPrompt(t.ID, stored)
	return nil
}

// pendingPrompt returns the text of task id's last kit prompt that never
// reached the gate, empty when there is none or its text was not kept.
func (r *Runner) pendingPrompt(id string) string {
	if pending, err := r.Store.PromptPending(id); err != nil || !pending {
		return ""
	}
	text, _ := r.Store.PendingPrompt(id)
	return text
}

// boxIs reports whether the input box in pane provably holds text. A box the
// adapter cannot read, or an error, counts as not.
func (r *Runner) boxIs(ctx context.Context, adapter harness.Adapter, pane, text string) bool {
	screen, err := r.Herdr.PaneRead(ctx, pane, progressScreenLines)
	return err == nil && adapter.InputIs(screen, text)
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
// its own runner. The session's transcript is recorded from the first
// prompt on, so usage covers a session that hands off before its first
// turn ends.
func (r *Runner) GatePrompt(id string, adapter harness.Adapter, payload []byte) (string, error) {
	t, err := r.Store.Get(id)
	if err != nil {
		return "", err
	}
	r.recordTranscript(t, adapter.Harness(), adapter.HookTranscript(payload))
	prompt, synthetic, err := adapter.GatePrompt(payload)
	if err != nil || synthetic {
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
