package runner

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/harness"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// contextState is what the kit knows about a runner's context, kept in
// tasks/<id>/progress.json next to the stall ladder, so it never moves the
// task's UpdatedAt. Armed and Asked hold the attempt they belong to, so a
// new attempt starts clear.
type contextState struct {
	// LowAt is when Claude last announced an automatic compaction.
	LowAt time.Time `json:"contextLowAt,omitzero"`
	// Armed is the attempt whose automatic compaction Claude announced and
	// has not run yet. A manual compaction cancels it.
	Armed int `json:"compactArmed,omitempty"`
	// Asked is the attempt the kit asked to hand off. It asks once per
	// attempt.
	Asked int `json:"handoffAsked,omitempty"`
	// Compactions counts the compactions the runner's sessions ran.
	Compactions int `json:"compactions,omitempty"`
}

// Compactions returns how many times the sessions of task id compacted
// their context.
func (r *Runner) Compactions(id string) int { return r.loadProgress(id).Compactions }

// ContextHook handles Claude's PreCompact and SessionStart(compact) hooks
// of a runner. An automatic compaction that ran asks the runner, once per
// attempt, to hand off to a fresh session: out is then the SessionStart
// output that adds the request to its context, and the request is also
// queued in its pane as a message, which a model busy with its task heeds
// where it may pass over added context. A manual /compact never asks. Hooks
// of an attempt that already ended are ignored.
func (r *Runner) ContextHook(ctx context.Context, id string, payload []byte) (out string, err error) {
	t, err := r.Store.Get(id)
	if err != nil {
		return "", err
	}
	attempt := task.AttemptOf(t)
	if r.callerAttempt() != attempt {
		return "", nil
	}
	c, err := harness.ParseClaudeCompaction(payload)
	if err != nil {
		return "", err
	}
	compacted := c.Event == "SessionStart" && c.Source == "compact"
	if c.Event != "PreCompact" && !compacted {
		return "", nil
	}
	unlock, err := r.Store.LockFile(id, "progress.lock")
	if err != nil {
		return "", err
	}
	st := r.loadProgress(id)
	var auto, ask bool
	switch {
	case c.Event == "PreCompact" && c.Trigger == "auto":
		st.LowAt, st.Armed = time.Now().UTC(), attempt
	case c.Event == "PreCompact":
		st.Armed = 0
	default:
		st.Compactions++
		auto, st.Armed = st.Armed == attempt, 0
		ask = auto && st.Asked != attempt && r.mayHandOff(t)
		if ask {
			st.Asked = attempt
		}
	}
	r.saveProgress(id, st)
	unlock()
	if !auto {
		return "", nil
	}
	if !ask {
		r.emit(t, "context-low", "compacted automatically near its context limit; "+r.noHandoffReason(t, st.Asked == attempt), "")
		return "", nil
	}
	r.emit(t, "context-low", "compacted automatically near its context limit; asked the runner to hand off to a fresh session", "")
	request := r.handoffRequest(t)
	if err := r.Herdr.AgentPrompt(ctx, t.Herdr.PaneID, request); err != nil {
		// The added context still carries the request.
		err = fmt.Errorf("queue the handoff request: %w", err)
		return harness.ClaudeSessionContext(request), err
	}
	return harness.ClaudeSessionContext(request), nil
}

// handoffPending reports whether t was asked to hand off in its current
// attempt and still may, so a turn that ends without it is reminded of the
// request rather than given the plain nudge.
func (r *Runner) handoffPending(t *task.Task) bool {
	return r.mayHandOff(t) && r.loadProgress(t.ID).Asked == task.AttemptOf(t)
}

// codexContextLow asks a Codex runner whose footer shows little context
// left to hand off, in place of the turn-end nudge, once per attempt. It
// reports whether it asked.
func (r *Runner) codexContextLow(ctx context.Context, t *task.Task) (bool, error) {
	if !r.mayHandOff(t) {
		return false, nil
	}
	adapter, ok := adapterOf(t)
	if !ok {
		return false, nil
	}
	screen, err := r.Herdr.PaneRead(ctx, t.Herdr.PaneID, 40)
	if err != nil {
		return false, nil
	}
	left, ok := adapter.ContextLeft(screen)
	if !ok || left > r.Config.ContextHandoffAt {
		return false, nil
	}
	unlock, err := r.Store.LockFile(t.ID, "progress.lock")
	if err != nil {
		return false, err
	}
	st := r.loadProgress(t.ID)
	attempt := task.AttemptOf(t)
	if st.Asked == attempt {
		unlock()
		return false, nil
	}
	st.Asked = attempt
	r.saveProgress(t.ID, st)
	unlock()
	if err := r.Herdr.AgentPrompt(ctx, t.Herdr.PaneID, r.handoffRequest(t)); err != nil {
		return false, fmt.Errorf("ask for a handoff: %w", err)
	}
	r.emit(t, "context-low", fmt.Sprintf("%d%% of its context left; asked the runner to hand off to a fresh session", left), "")
	return true, nil
}

// mayHandOff reports whether t may still be asked to hand off: the feature
// is on, the task runs in a pane, and its one replacement is unused.
func (r *Runner) mayHandOff(t *task.Task) bool {
	return r.Config.ContextHandoff && t.Status == task.Running && t.Herdr.PaneID != "" && len(t.Handoffs) < MaxHandoffs
}

// noHandoffReason says why an automatic compaction asked for no handoff.
func (r *Runner) noHandoffReason(t *task.Task, asked bool) string {
	switch {
	case !r.Config.ContextHandoff:
		return "contextHandoff is off"
	case len(t.Handoffs) >= MaxHandoffs:
		return "it already used its one context replacement"
	case asked:
		return "it was already asked to hand off"
	}
	return "it is " + string(t.Status)
}

// handoffRequest asks the runner to hand off, naming the file and the exact
// command, and repeats the contract's rule.
func (r *Runner) handoffRequest(t *task.Task) string {
	file := filepath.Join(r.OutDir(t.ID), fmt.Sprintf("handoff-a%d.md", task.AttemptOf(t)+1))
	return fmt.Sprintf("herdr-runner-kit: your context is nearly full. Task %[1]s may continue once in a fresh session. "+
		"At a stable point, commit or save your work, then write a handoff to %[2]s naming what is done, what is left, "+
		"the checks you ran and any background process you started (command and pid). Take it from the checkout and your notes, "+
		"not from memory alone. Then run `%[3]s report handoff --output %[2]s --summary \"<what is done and what is left>\"` and stop: "+
		"a fresh session of %[1]s reads the handoff first and continues the work.", t.ID, file, r.Bin)
}
