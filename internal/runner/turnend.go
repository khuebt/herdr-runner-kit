package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/khuebt/herdr-runner-kit/internal/harness"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// codexTitlePrompt starts the input of Codex's internal title-generation
// turn, which also fires notify.
const codexTitlePrompt = "Generate a concise, single-line task title"

// backgroundWork matches the count Claude shows on its mode line ("⏵⏵
// bypass permissions on · 1 shell") while background shells or agents still
// run. Claude ends its turn then and wakes on its own later.
var backgroundWork = regexp.MustCompile(`(?i)\b\d+\s+(background\s+)?(shells?|bash(es)?|tasks?|agents?)\b`)

// hasBackgroundWork looks only at Claude's mode line, so text in the
// conversation can never match.
func hasBackgroundWork(screen string) bool {
	for line := range strings.SplitSeq(screen, "\n") {
		if strings.Contains(line, "⏵") && backgroundWork.MatchString(line) {
			return true
		}
	}
	return false
}

// TurnEnd handles a runner's turn-end hook. A turn-end is a hint, never a
// verdict: the first turn that ends without a report is nudged, the second
// becomes news, and later ones are ignored until the main session prompts
// the runner again. For Claude, block holds the nudge text that the Stop
// hook returns; for Codex the nudge is typed into the pane.
//
// A usage limit switches the runner to its next fallback agent. Claude
// confirms it through its StopFailure hook, and with no fallback left the
// task fails. Codex only shows it on screen, so a limit seen there switches
// when a fallback is left and otherwise becomes usage-limit news next to
// the nudge.
func (r *Runner) TurnEnd(ctx context.Context, id string, h task.Harness, payload []byte) (block string, err error) {
	t, err := r.Store.Get(id)
	if err != nil {
		return "", err
	}
	if h == task.Claude {
		if message, stopFailure := harness.ClaudeUsageLimit(payload); stopFailure {
			// Any other API failure ends the turn with nothing the kit can
			// act on, and Claude ignores a StopFailure hook's output.
			if message == "" || t.Herdr.PaneID == "" {
				return "", nil
			}
			return "", r.switchAgent(t, true)
		}
	}
	if h == task.Codex {
		if ignore, err := r.ignoreCodexTurn(t, payload); ignore || err != nil {
			return "", err
		}
	}
	if t.Status != task.Running || t.Herdr.PaneID == "" {
		return "", nil
	}
	limited := h == task.Codex && r.usageLimited(ctx, t)
	if limited {
		// An agent out of usage cannot act on a nudge.
		if err := r.switchAgent(t, false); !errors.Is(err, errNoFallback) {
			return "", err
		}
	}
	if h == task.Claude {
		if screen, err := r.Herdr.PaneRead(ctx, t.Herdr.PaneID, 8); err == nil && hasBackgroundWork(screen) {
			return "", nil
		}
	}
	t, err = r.Store.Update(id, func(t *task.Task) error {
		if t.Status != task.Running {
			return errNotRunning
		}
		t.Unreported++
		return nil
	})
	if errors.Is(err, errNotRunning) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	switch t.Unreported {
	case 1:
		nudge := r.nudgeText()
		if h == task.Codex {
			if err := r.Herdr.AgentPrompt(ctx, t.Herdr.PaneID, nudge); err != nil {
				return "", fmt.Errorf("nudge: %w", err)
			}
		}
		r.emit(t, "nudged", "turn ended without a report; asked the runner to report", "")
		if limited {
			r.emit(t, "usage-limit", limitNews, "")
		}
		return nudge, nil
	case 2:
		summary := "ended its turn again without reporting; it may wait for background work or need input"
		if limited {
			summary = "ended its turn again without reporting; " + limitNews
		}
		r.emit(t, "turn-ended-unreported", summary, "")
	}
	return "", nil
}

// limitNews tells the main session about a usage limit seen on a runner's
// screen that no fallback agent can take over.
const limitNews = "the runner appears to be at its usage limit, and no quotaFallback agent is left to switch to"

var errNotRunning = errors.New("not running")

// ignoreCodexTurn filters notify calls that are not the runner's own turns.
// The first real turn records its thread.
func (r *Runner) ignoreCodexTurn(t *task.Task, payload []byte) (bool, error) {
	var n struct {
		Type   string   `json:"type"`
		Thread string   `json:"thread-id"`
		Input  []string `json:"input-messages"`
	}
	if err := json.Unmarshal(payload, &n); err != nil {
		return true, fmt.Errorf("parse codex notify: %w", err)
	}
	if n.Type != "agent-turn-complete" {
		return true, nil
	}
	if len(n.Input) > 0 && strings.HasPrefix(strings.TrimSpace(n.Input[0]), codexTitlePrompt) {
		return true, nil
	}
	if n.Thread == "" || t.CodexThread == n.Thread {
		return false, nil
	}
	if t.CodexThread != "" {
		return true, nil
	}
	_, err := r.Store.Update(t.ID, func(t *task.Task) error {
		if t.CodexThread == "" {
			t.CodexThread = n.Thread
		}
		return nil
	})
	return false, err
}

func (r *Runner) nudgeText() string {
	return fmt.Sprintf("You ended your turn without reporting to the main session. "+
		"If the task is finished, run `%[1]s report done --summary \"...\"`. "+
		"If you need a decision, run `%[1]s report needs-human --question \"...\"`. "+
		"If it failed, run `%[1]s report failed --summary \"...\"`. "+
		"If you are waiting for background work, wait for it, then report.", r.Bin)
}
