package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// ReportOptions are the inputs of `report`.
type ReportOptions struct {
	Status   task.Status // Done, NeedsHuman or Failed
	Summary  string
	Question string
	Outputs  []string
	// Manual is set when the task id came from --task rather than HRK_TASK.
	Manual bool
}

// Report records a runner's result and returns at once. The slow follow-up
// work (starting dependents, notifications) runs in a detached child, so a
// report never waits on herdr or the network.
func (r *Runner) Report(id string, o ReportOptions) (*task.Task, error) {
	if err := validateReport(&o); err != nil {
		return nil, err
	}
	t, err := r.Store.Update(id, func(t *task.Task) error {
		switch {
		case t.Status.Terminal():
			return fmt.Errorf("%s is already %s; report ignored", t.ID, t.Status)
		case t.Status == task.Queued || t.Status == task.Held:
			return fmt.Errorf("%s has not started (status %s)", t.ID, t.Status)
		}
		t.Status = o.Status
		t.Summary, t.Outputs, t.Question = o.Summary, o.Outputs, o.Question
		t.Unreported = 0
		if o.Status == task.Failed {
			t.Error = o.Summary
		}
		if o.Status.Terminal() {
			t.FinishedAt = time.Now().UTC()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	summary := o.Summary
	if o.Manual {
		summary = "[manual] " + summary
	}
	ev := state.Event{
		Task: t.ID, Owner: t.Owner, Type: string(o.Status), Status: t.Status,
		Summary: summary, Question: o.Question, Outputs: o.Outputs,
	}
	if err := r.Store.Append(ev); err != nil {
		return t, fmt.Errorf("write event: %w", err)
	}
	if err := r.StartAfter(t.ID); err != nil {
		fmt.Fprintf(os.Stderr, "herdr-runner-kit: %s: start follow-up: %v\n", t.ID, err)
	}
	return t, nil
}

func validateReport(o *ReportOptions) error {
	o.Summary = strings.TrimSpace(o.Summary)
	o.Question = strings.TrimSpace(o.Question)
	switch o.Status {
	case task.Done, task.Failed:
		if o.Summary == "" {
			return fmt.Errorf("report %s needs --summary", o.Status)
		}
		if o.Question != "" {
			return fmt.Errorf("--question is only for needs-human")
		}
	case task.NeedsHuman:
		if o.Question == "" {
			return errors.New("report needs-human needs --question")
		}
	default:
		return fmt.Errorf("unknown report %q: want done, needs-human or failed", o.Status)
	}
	outputs := make([]string, 0, len(o.Outputs))
	for _, p := range o.Outputs {
		abs, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		if _, err := os.Stat(abs); err != nil {
			return fmt.Errorf("--output %s: %w", p, err)
		}
		outputs = append(outputs, abs)
	}
	o.Outputs = outputs
	return nil
}

// After runs the follow-up of a task's latest status: a done task starts or
// holds the tasks that waited for it, and a failed or stopped task fails
// them. It runs in the detached child that report starts.
func (r *Runner) After(ctx context.Context, id string) error {
	t, err := r.Store.Get(id)
	if err != nil {
		return err
	}
	switch t.Status {
	case task.Done:
		return r.startDependents(ctx, t)
	case task.Failed, task.Stopped:
		return r.failDependents(t)
	}
	return nil
}

// startDependents moves every queued task whose parents are now all done
// to held (human gate) or starts it in this process.
func (r *Runner) startDependents(ctx context.Context, parent *task.Task) error {
	tasks, err := r.Store.List()
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	var errs []error
	for _, q := range tasks {
		if q.Status != task.Queued || !slices.Contains(q.After, parent.ID) {
			continue
		}
		pending, err := r.pendingParents(q.After)
		if err != nil {
			r.failDependent(q.ID, err.Error())
			continue
		}
		if len(pending) > 0 {
			continue
		}
		inputs, base, err := r.parentInputs(q.After)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		withInputs := func(t *task.Task) {
			t.Inputs = inputs
			if t.Worktree != nil && t.Worktree.Base == "" {
				t.Worktree.Base = base
			}
		}
		if q.Gate == task.GateHuman {
			if t, changed, err := r.Store.Transition(q.ID, task.Held, []task.Status{task.Queued}, withInputs); err != nil {
				errs = append(errs, err)
			} else if changed {
				r.emit(t, "held", "held for release: "+t.Title, "")
			}
			continue
		}
		// The status change is the lock: when two parents finish at once,
		// only one follow-up moves the child to starting and launches it.
		t, changed, err := r.beginLaunch(q.ID, withInputs)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !changed {
			continue
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_ = r.Launch(ctx, id) // every outcome is recorded on the task
		}(t.ID)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// failDependents fails every waiting task that depends on parent, and their
// dependents in turn.
func (r *Runner) failDependents(parent *task.Task) error {
	tasks, err := r.Store.List()
	if err != nil {
		return err
	}
	for _, q := range tasks {
		if slices.Contains(q.After, parent.ID) {
			r.failDependent(q.ID, fmt.Sprintf("parent %s %s", parent.ID, parent.Status))
		}
	}
	return nil
}

func (r *Runner) failDependent(id, reason string) {
	t, changed, err := r.Store.Transition(id, task.Failed, []task.Status{task.Queued, task.Held}, func(t *task.Task) {
		t.Error = reason
	})
	if err != nil || !changed {
		return
	}
	r.emit(t, "failed", reason, "")
	_ = r.failDependents(t)
}

// parentInputs describes the results of done parents for the child's first
// prompt. base is the worktree branch of the only parent that has one, the
// default starting point for a child worktree that builds on its code.
func (r *Runner) parentInputs(after []string) (inputs, base string, err error) {
	var lines []string
	var branches []string
	for _, id := range after {
		p, err := r.Store.Get(id)
		if err != nil {
			return "", "", err
		}
		line := fmt.Sprintf("- %s (%s): %s", p.ID, p.Title, p.Summary)
		if len(p.Outputs) > 0 {
			line += " Outputs: " + strings.Join(p.Outputs, ", ") + "."
		}
		if p.Worktree != nil && p.Worktree.Branch != "" {
			line += " Branch: " + p.Worktree.Branch + "."
			branches = append(branches, p.Worktree.Branch)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "", "", nil
	}
	if len(branches) == 1 {
		base = branches[0]
	}
	return "Inputs from the tasks this one waited for:\n" + strings.Join(lines, "\n"), base, nil
}
