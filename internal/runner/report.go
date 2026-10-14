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
	"unicode/utf8"

	"github.com/khuebt/herdr-runner-kit/internal/redact"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// Limits of one report.
const (
	// MaxQuestions is the most questions one needs-human round asks, and
	// MaxQuestionRunes the longest question of a round. A single question
	// longer than that is cut, so a report written for the first contract
	// is never refused.
	MaxQuestions     = 5
	MaxQuestionRunes = 400
	// MaxDecisions is the most decisions one done report lists, and
	// MaxDecisionRunes the longest one.
	MaxDecisions     = 7
	MaxDecisionRunes = 200
	// MaxSummaryRunes is the longest summary wait and Telegram show whole.
	// A longer one is accepted, with a note.
	MaxSummaryRunes = 400
)

// answeredInTab is the summary of the answered event a report emits when the
// runner's question was answered in its tab rather than with answer.
const answeredInTab = "answered in the runner's tab"

// ReportOptions are the inputs of `report`.
type ReportOptions struct {
	Status  task.Status // Done, NeedsHuman or Failed
	Summary string
	// Questions are one question, or a round of up to MaxQuestions, for
	// needs-human.
	Questions []string
	// Decisions are the open questions a done runner settled with the
	// recommended option, each as "<question> -> <choice>".
	Decisions []string
	Outputs   []string
	// Manual is set when the task id came from --task rather than HRK_TASK.
	Manual bool
}

// Report records a runner's result and returns at once. The slow follow-up
// work (starting dependents, notifications) runs in a detached child, so a
// report never waits on herdr or the network; it reads a local git
// snapshot of the task's checkout, capped at 2 s.
func (r *Runner) Report(id string, o ReportOptions) (*task.Task, error) {
	if err := validateReport(&o); err != nil {
		return nil, err
	}
	cur, err := r.Store.Get(id)
	if err != nil {
		return nil, err
	}
	// Read git before taking the task lock, so a slow repository never
	// holds it. A report the update refuses needs no snapshot.
	var git *task.GitState
	if !cur.Status.Terminal() {
		git = gitState(cur)
	}
	question := ""
	if len(o.Questions) > 0 {
		question = task.JoinQuestions(o.Questions)
	}
	// A report while the runner's question is still open means the user
	// answered it in the runner's tab.
	var answered bool
	t, err := r.Store.Update(id, func(t *task.Task) error {
		switch {
		case t.Status.Terminal():
			return fmt.Errorf("%s is already %s; report ignored", t.ID, t.Status)
		case t.Status == task.Queued || t.Status == task.Held:
			return fmt.Errorf("%s has not started (status %s)", t.ID, t.Status)
		}
		answered = t.Status == task.NeedsHuman && t.Question != ""
		t.Status = o.Status
		t.Summary, t.Outputs, t.Question, t.Decisions = o.Summary, o.Outputs, question, o.Decisions
		t.Unreported = 0
		t.Git = git // a failed snapshot clears an older one rather than show it as current
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
	if answered {
		r.emit(t, "answered", answeredInTab, "")
	}
	summary := o.Summary
	if o.Manual {
		summary = "[manual] " + summary
	}
	ev := state.Event{
		Task: t.ID, Owner: t.Owner, Type: string(o.Status), Status: t.Status,
		Summary: summary, Question: question, Outputs: o.Outputs, Decisions: o.Decisions,
	}
	if git != nil {
		ev.Git = git.Short()
	}
	if err := r.record(ev); err != nil {
		return t, fmt.Errorf("write event: %w", err)
	}
	if err := r.StartAfter(t.ID); err != nil {
		fmt.Fprintf(os.Stderr, "herdr-runner-kit: %s: start follow-up: %v\n", t.ID, err)
	}
	return t, nil
}

// OutDir returns the folder for a task's files meant for the main session.
func (r *Runner) OutDir(id string) string { return filepath.Join(r.Store.TaskDir(id), "out") }

// ReportNotes returns advice for a runner about its report: a summary that
// wait and Telegram cut, and outputs outside the task's out folder. They
// never refuse a report, and are neither events nor stored.
func (r *Runner) ReportNotes(id string, o ReportOptions) []string {
	out := r.OutDir(id)
	var notes []string
	if utf8.RuneCountInString(strings.TrimSpace(o.Summary)) > MaxSummaryRunes {
		notes = append(notes, fmt.Sprintf("note: summaries over %d characters are cut in wait and Telegram; keep one or two sentences and put detail in an --output file under %s", MaxSummaryRunes, out))
	}
	for _, p := range o.Outputs {
		if !within(out, p) {
			notes = append(notes, fmt.Sprintf("note: %s is outside %s; files for the main session belong there", p, out))
		}
	}
	return notes
}

// within reports whether path is inside dir, symlinks resolved where they
// exist.
func within(dir, path string) bool {
	resolve := func(p string) string {
		abs, err := filepath.Abs(p)
		if err != nil {
			return p
		}
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			return resolved
		}
		return abs
	}
	rel, err := filepath.Rel(resolve(dir), resolve(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// validateReport checks a report and hides values that look like secrets
// in its text, before the text is stored, logged or sent to Telegram.
func validateReport(o *ReportOptions) error {
	o.Summary = redact.Secrets(strings.TrimSpace(o.Summary))
	o.Questions = redactAll(trimAll(o.Questions))
	o.Decisions = redactAll(trimAll(o.Decisions))
	switch o.Status {
	case task.Done, task.Failed:
		if o.Summary == "" {
			return fmt.Errorf("report %s needs --summary", o.Status)
		}
		if len(o.Questions) > 0 {
			return fmt.Errorf("--question is only for needs-human")
		}
	case task.NeedsHuman:
		if len(o.Questions) == 0 {
			return errors.New("report needs-human needs --question")
		}
	default:
		return fmt.Errorf("unknown report %q: want done, needs-human or failed", o.Status)
	}
	if len(o.Decisions) > 0 && o.Status != task.Done {
		return errors.New("--decision is only for done")
	}
	if n := len(o.Questions); n > MaxQuestions {
		return fmt.Errorf("%d questions: ask at most %d in one round", n, MaxQuestions)
	}
	if len(o.Questions) == 1 {
		if q := []rune(o.Questions[0]); len(q) > MaxQuestionRunes {
			o.Questions[0] = strings.TrimSpace(string(q[:MaxQuestionRunes-1])) + "…"
		}
	}
	for i, q := range o.Questions {
		if n := utf8.RuneCountInString(q); n > MaxQuestionRunes {
			return fmt.Errorf("question %d has %d characters; keep each question of a round under %d", i+1, n, MaxQuestionRunes+1)
		}
	}
	if n := len(o.Decisions); n > MaxDecisions {
		return fmt.Errorf("%d decisions: list at most %d", n, MaxDecisions)
	}
	for i, d := range o.Decisions {
		if n := utf8.RuneCountInString(d); n > MaxDecisionRunes {
			return fmt.Errorf("decision %d has %d characters; keep each under %d", i+1, n, MaxDecisionRunes+1)
		}
		if q, c, ok := strings.Cut(d, "->"); !ok || strings.TrimSpace(q) == "" || strings.TrimSpace(c) == "" {
			return fmt.Errorf("decision %d: want \"<question> -> <choice>\", got %q", i+1, d)
		}
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

func redactAll(values []string) []string {
	for i, v := range values {
		values[i] = redact.Secrets(v)
	}
	return values
}

// trimAll trims each value and drops the empty ones, so an empty flag reads
// as no flag.
func trimAll(values []string) []string {
	var out []string
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
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
				r.emit(t, "held", task.HeldSummary(t), "")
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
