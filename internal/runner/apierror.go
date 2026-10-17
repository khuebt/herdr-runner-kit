package runner

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// apiRetryFloor and apiRetryCeil bound the wait before the kit continues a
// runner whose turn ended on a transient API error.
const (
	apiRetryFloor = 30 * time.Second
	apiRetryCeil  = 60 * time.Second
)

// apiContinue is the prompt that continues a runner after an API error.
const apiContinue = "herdr-runner-kit: your last turn ended on an API error, such as a lost connection or an overloaded server. " +
	"Continue the task from where it stopped; check `git status` first if you were editing files. Report as usual when you finish."

// apiError handles a turn that ended on a transient API error: the first
// starts the detached continue, the second becomes api-error news, later
// ones wait for a reset.
func (r *Runner) apiError(t *task.Task, message string) error {
	if t.Status != task.Running || t.Herdr.PaneID == "" {
		return nil
	}
	t, err := r.Store.Update(t.ID, func(t *task.Task) error {
		if t.Status != task.Running {
			return errNotRunning
		}
		t.APIErrors++
		return nil
	})
	if errors.Is(err, errNotRunning) {
		return nil
	}
	if err != nil {
		return err
	}
	message = clip(message, 160)
	switch t.APIErrors {
	case 1:
		r.emit(t, "api-retry", "turn ended on an API error ("+message+"); continuing it within a minute", "")
		return r.StartAPIRetry(t.ID)
	case 2:
		r.emit(t, "api-error", "the turn failed again on an API error after one automatic continue ("+message+"); the runner waits at its prompt", "")
	}
	return nil
}

// ContinueAfterAPIError runs in the detached __api-retry child: after a
// random 30 to 60 s it types apiContinue into the runner, unless the task
// no longer runs, its turn ended normally or was prompted since, or its
// agent is working.
func (r *Runner) ContinueAfterAPIError(ctx context.Context, id string) error {
	r.Sleep(apiRetryFloor + rand.N(apiRetryCeil-apiRetryFloor))
	t, err := r.Store.Get(id)
	if err != nil {
		return err
	}
	if t.Status != task.Running || t.APIErrors != 1 || t.Herdr.PaneID == "" {
		return nil
	}
	pane := t.Herdr.PaneID
	if a, err := r.Herdr.AgentGet(ctx, pane); err != nil || a.Status == "working" {
		return nil
	}
	if err := r.typePrompt(ctx, t, pane, apiContinue); err != nil {
		r.emit(t, "api-retry-failed", "could not continue the runner after the API error: "+err.Error(), "")
		return err
	}
	r.emit(t, "api-continued", "continued the runner after the API error", "")
	return nil
}
