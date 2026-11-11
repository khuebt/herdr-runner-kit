package runner

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/harness"
	"github.com/khuebt/herdr-runner-kit/internal/herdr"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// promptFate is what the screen and herdr say became of a prompt the kit
// typed into an agent.
type promptFate int

const (
	// fateUnsure: the evidence does not say, so the kit changes nothing.
	fateUnsure promptFate = iota
	// fateTaken: the agent works, the kit's gate saw the prompt, or the
	// conversation shows it.
	fateTaken
	// fateHolds: the prompt still sits in the input; its Enter was lost.
	fateHolds
	// fateLost: the input is empty, the agent is not working and nothing
	// shows the prompt, so the text never reached the agent.
	fateLost
)

// promptEchoPasted is how a pasted prompt shows in the conversation:
// "[Pasted text #1 +12 lines]" in Claude, "[Pasted Content 2337 chars]" in
// Codex.
var promptEchoPasted = regexp.MustCompile(`\[Pasted (?:text #\d+|Content \d+ chars)`)

// promptEchoed reports whether the conversation above the input shows prompt
// as the agent echoes a submitted one.
// ponytail: only the last progressScreenLines lines are read, so a turn that
// scrolls the echo away before this runs reads as lost; a first turn does
// not finish in the seconds that took.
func promptEchoed(adapter harness.Adapter, screen, prompt string) bool {
	above := adapter.ProgressText(screen)
	if promptEchoPasted.MatchString(above) {
		return true
	}
	head := []rune(strings.Join(strings.Fields(prompt), " "))
	head = head[:min(40, len(head))]
	return len(head) > 0 && strings.Contains(above, string(head))
}

// promptFate reads what became of prompt in pane. Codex can report a stale
// agent state, so besides herdr's state it reads the agent's own status
// line and the input.
func (r *Runner) promptFate(ctx context.Context, t *task.Task, adapter harness.Adapter, pane, prompt string) promptFate {
	a, err := r.Herdr.AgentGet(ctx, pane)
	if err != nil {
		return fateUnsure
	}
	screen, err := r.Herdr.PaneRead(ctx, pane, progressScreenLines)
	if err != nil {
		return fateUnsure
	}
	if a.Status == "working" || adapter.ShowsWorking(screen) {
		return fateTaken
	}
	if _, open := harness.DetectDialog(adapter, screen); open || a.Status == "blocked" {
		return fateUnsure
	}
	switch adapter.InputState(screen, prompt) {
	case harness.InputHolds:
		return fateHolds
	case harness.InputEmpty:
		if r.gateSawPrompt(t, adapter) || promptEchoed(adapter, screen, prompt) {
			return fateTaken
		}
		return fateLost
	}
	return fateUnsure
}

// gateSawPrompt reports whether the prompt gate of a runner with locked
// input consumed the kit's token: the agent submitted the prompt.
func (r *Runner) gateSawPrompt(t *task.Task, adapter harness.Adapter) bool {
	if !t.InputLocked || !adapter.LocksInput() {
		return false
	}
	pending, err := r.Store.PromptPending(t.ID)
	return err == nil && !pending
}

// confirmFirstPrompt handles a first prompt herdr saw no turn start for,
// acting only on evidence of what became of it:
//
//   - the agent works, the gate saw the prompt or the conversation shows it:
//     nothing, the turn is only slow to show;
//   - the input still holds the prompt: one more Enter, never the text again;
//   - the input is empty and the agent is not working: the text was lost,
//     as when a dialog took the keys; it is typed once more;
//   - anything else: nothing is sent, and when herdr still sees no turn
//     start the runner is stalled news.
//
// Each of the two sends waits 5 s once more, and a prompt still not taken
// after it is stalled news, with the runner left open.
func (r *Runner) confirmFirstPrompt(ctx context.Context, t *task.Task, adapter harness.Adapter, pane, prompt string) {
	switch r.promptFate(ctx, t, adapter, pane, prompt) {
	case fateHolds:
		if err := r.Herdr.AgentSendKeys(ctx, pane, "enter"); err != nil {
			return
		}
		if r.awaitTurn(ctx, t, adapter, pane, prompt, "prompt not submitted: the first prompt is still in the input after a second Enter; the runner is left open") {
			r.emit(t, "prompt-resubmitted", "the first prompt was still in the input; sent Enter again", "")
		}
	case fateLost:
		if err := r.retypeFirstPrompt(ctx, t, pane, prompt); err != nil {
			return
		}
		if r.awaitTurn(ctx, t, adapter, pane, prompt, "first prompt not taken: it was typed again and the agent still did not take it; the runner is left open") {
			r.emit(t, "prompt-retyped", "the input was empty and the agent idle; typed the first prompt once more", "")
		}
	case fateUnsure:
		if _, err := r.Herdr.AgentWait(ctx, pane, 5*time.Second, "working"); herdr.HasCode(err, herdr.CodeTimeout) {
			r.markStalled(t, "stalled", "first prompt not confirmed: herdr saw no turn start and the input could not be read; check the runner's tab")
		}
	}
}

// awaitTurn waits 5 s for the turn to start after the kit sent the prompt
// again. A loaded machine can take longer to show the turn: only a prompt
// still in the input, or gone, is stalled news with the summary stalled. It
// reports whether the runner is not stalled.
func (r *Runner) awaitTurn(ctx context.Context, t *task.Task, adapter harness.Adapter, pane, prompt, stalled string) bool {
	if _, err := r.Herdr.AgentWait(ctx, pane, 5*time.Second, "working"); err == nil {
		return true
	}
	if f := r.promptFate(ctx, t, adapter, pane, prompt); f == fateHolds || f == fateLost {
		r.markStalled(t, "stalled", stalled)
		return false
	}
	return true
}

// retypeFirstPrompt types the first prompt again after evidence that the
// first copy was lost. The lost copy's token goes first, so a locked
// runner's gate lets this one through and typePrompt does not join the
// two. t is typed as a runner still starting, so a Codex gate that has not
// confirmed yet is probed again.
func (r *Runner) retypeFirstPrompt(ctx context.Context, t *task.Task, pane, prompt string) error {
	if err := errors.Join(r.Store.DropPromptTokens(t.ID), r.Store.SetPendingPrompt(t.ID, "")); err != nil {
		return err
	}
	first := *t
	first.Status = task.Starting
	return r.typePrompt(ctx, &first, pane, prompt)
}
