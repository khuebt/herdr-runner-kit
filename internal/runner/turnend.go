package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

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
func (r *Runner) TurnEnd(ctx context.Context, id string, h task.Harness, payload []byte) (block string, err error) {
	t, err := r.Store.Get(id)
	if err != nil {
		return "", err
	}
	if h == task.Codex {
		if ignore, err := r.ignoreCodexTurn(t, payload); ignore || err != nil {
			return "", err
		}
	}
	if t.Status != task.Running || t.Herdr.PaneID == "" {
		return "", nil
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
		return nudge, nil
	case 2:
		r.emit(t, "turn-ended-unreported",
			"ended its turn again without reporting; it may wait for background work or need input", "")
	}
	return "", nil
}

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
