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
	// Attempt is the agent session the report comes from, read from
	// HRK_ATTEMPT. A report from another session than the task's current
	// one is refused; 0 skips the check, as a manual report does.
	Attempt int
}

// Report records a runner's result and returns at once. The slow follow-up
// work (starting dependents, notifications) runs in a detached child, so a
// report never waits on herdr or the network; it reads a local git
// snapshot of the task's checkout, capped at 2 s.
//
// A runner's done is held while its turn runs when the adapter says the
// agent may work on after it: the task stays running, and the done becomes
// final at the turn end (see FinishDone). Only a report from a runner's
// session (Attempt set) is held; a manual one never is.
func (r *Runner) Report(id string, o ReportOptions) (*task.Task, error) {
	if err := validateReport(&o); err != nil {
		return nil, err
	}
	return r.report(id, o, finishNone, nil)
}

// How report treats a held done.
const (
	finishNone       = iota // a report from the runner or the user
	finishHeld              // make the held done final, unless superseded
	finishSuperseded        // make the held done final, even if superseded
)

// errNothingHeld says the task holds no done to make final.
var errNothingHeld = errors.New("no held done")

// FinishDone makes t's held done final through the normal report path: a
// fresh git snapshot, the done event and the dependents. superseded also
// finishes a done a queued message superseded, and a non-nil judged only
// a held done of the same Since and TurnStart, so a caller that judged an
// older done, or one before a turn started, never finishes what it did not
// judge. It returns errNothingHeld when the task holds no such done, so two
// callers finish it once.
func (r *Runner) FinishDone(id string, superseded bool, judged *task.PendingDone) (*task.Task, error) {
	mode := finishHeld
	if superseded {
		mode = finishSuperseded
	}
	return r.report(id, ReportOptions{Status: task.Done}, mode, judged)
}

func (r *Runner) report(id string, o ReportOptions, finish int, judged *task.PendingDone) (*task.Task, error) {
	cur, err := r.Store.Get(id)
	if err != nil {
		return nil, err
	}
	hold := finish == finishNone && o.Status == task.Done && o.Attempt > 0 &&
		cur.Status == task.Running && doneAtTurnEnd(cur)
	// Read git before taking the task lock, so a slow repository never
	// holds it. A report the update refuses, or a held done, needs no
	// snapshot.
	var git *task.GitState
	if !hold && !cur.Status.Terminal() {
		git = gitState(cur)
	}
	question := ""
	if len(o.Questions) > 0 {
		question = task.JoinQuestions(o.Questions)
	}
	// A report while the runner's question is still open means the user
	// answered it in the runner's tab, which a locked tab never takes.
	var answered, held bool
	t, err := r.Store.Update(id, func(t *task.Task) error {
		if err := staleAttempt(t, o.Attempt); err != nil {
			return err
		}
		switch {
		case t.Status.Terminal():
			return fmt.Errorf("%s is already %s; report ignored", t.ID, t.Status)
		case t.Status == task.Queued || t.Status == task.Held:
			return fmt.Errorf("%s has not started (status %s)", t.ID, t.Status)
		}
		if finish != finishNone {
			p := t.PendingDone
			if p == nil || p.Superseded && finish != finishSuperseded ||
				judged != nil && (!p.Since.Equal(judged.Since) || !p.TurnStart.Equal(judged.TurnStart)) {
				return errNothingHeld
			}
			o.Summary, o.Decisions, o.Outputs = p.Summary, p.Decisions, p.Outputs
		}
		if held = hold && t.Status == task.Running; held {
			t.PendingDone = &task.PendingDone{Summary: o.Summary, Decisions: o.Decisions, Outputs: o.Outputs, Since: time.Now().UTC()}
			t.Unreported, t.APIErrors = 0, 0
			return nil
		}
		answered = t.Status == task.NeedsHuman && t.Question != "" && !InputLocked(t)
		t.Status = o.Status
		t.Summary, t.Outputs, t.Question, t.Decisions = o.Summary, o.Outputs, question, o.Decisions
		t.Unreported, t.APIErrors = 0, 0
		t.PendingDone = nil // a later report replaces a held done
		t.Git = git         // a failed snapshot clears an older one rather than show it as current
		if o.Status == task.Failed {
			t.Error = o.Summary
		}
		if o.Status.Terminal() {
			t.FinishedAt = time.Now().UTC()
			t.QueuedSend = time.Time{}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if held {
		// Progress, not news: the done reaches the main session when the
		// turn ends.
		r.emit(t, "done-pending", o.Summary, "")
		return t, nil
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
		Attempt: eventAttempt(t),
	}
	if git != nil {
		ev.Git = git.Short()
	}
	// The status is written, so the follow-up runs even when the log
	// refused the event.
	recErr := r.record(ev)
	if err := r.StartAfter(t.ID); err != nil {
		fmt.Fprintf(os.Stderr, "hrk: %s: start follow-up: %v\n", t.ID, err)
	}
	if recErr != nil {
		return t, fmt.Errorf("write event: %w", recErr)
	}
	return t, nil
}

// doneAtTurnEnd reports whether t's agent holds a done until its turn ends.
func doneAtTurnEnd(t *task.Task) bool {
	a, ok := adapterOf(t)
	return ok && a.DoneAtTurnEnd()
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
		if t.Review != nil {
			r.finishReview(ctx, t)
			if t, err = r.Store.Get(id); err != nil {
				return err
			}
		}
		if t.Plan == "" {
			return r.startDependents(ctx, t)
		}
		// A plan phase is checked before the next wave may start, and the
		// plan is done once its last phase is. A review or fixup task is no
		// phase and is not checked.
		r.acceptPhase(ctx, t)
		return errors.Join(r.startDependents(ctx, t), r.planDone(ctx, t))
	case task.Failed, task.Stopped:
		err := r.failDependents(t)
		if t.Plan != "" {
			// A final review or fixup that ended this way still ends the
			// run, and so does an older phase task a newer one replaced.
			err = errors.Join(err, r.planDone(ctx, t))
		}
		return err
	}
	return nil
}

// ended settles what waits on t, which a stop, a failed launch or a lost
// runner just ended: its waiting tasks fail here, and a plan task gets the
// follow-up a report would start, which checks whether the plan is done.
// That check runs in the detached child, never here, because plan run
// holds the plan's lock while it spawns, and a spawn can fail a task.
func (r *Runner) ended(t *task.Task) {
	_ = r.failDependents(t)
	if t.Plan == "" {
		return
	}
	if err := r.StartAfter(t.ID); err != nil {
		fmt.Fprintf(os.Stderr, "hrk: %s: start follow-up: %v\n", t.ID, err)
	}
}

// SettleWindow is how long the follow-up of a finished task has to settle
// the tasks that wait for it before the wait backstop's sweep does.
const SettleWindow = 2 * time.Minute

// SettleWaiting is the wait backstop's sweep for follow-ups that never ran,
// such as one a crash or a reboot lost. It settles every waiting task that
// waiting-task-has-a-path finds stuck, and checks the plan of every run that
// plan-run-finishes finds finished without its plan-done. What finished or
// was created within window is left to the follow-up or spawn on its way.
func (r *Runner) SettleWaiting(ctx context.Context, window time.Duration) error {
	tasks, err := r.Store.List()
	if err != nil {
		return err
	}
	markers, err := r.planMarkers(tasks)
	if err != nil {
		return err
	}
	c := newCheck(Snapshot{Tasks: tasks, PlanDone: markers, Now: time.Now(), Settle: window})
	var errs []error
	for _, v := range waitingTaskHasAPath(c) {
		errs = append(errs, r.Settle(ctx, v.Task))
	}
	for _, v := range planRunFinishes(c) {
		errs = append(errs, r.planDone(ctx, c.byID[v.Task]))
	}
	return errors.Join(errs...)
}

// startDependents settles every task that waits for parent, which just
// finished done, and launches in this process the ones it starts.
func (r *Runner) startDependents(ctx context.Context, parent *task.Task) error {
	tasks, err := r.Store.List()
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	var errs []error
	for _, q := range tasks {
		if !waiting(q) || !slices.Contains(q.After, parent.ID) {
			continue
		}
		started, err := r.settle(ctx, q.ID)
		if err != nil {
			errs = append(errs, err)
		}
		if !started {
			continue
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_ = r.Launch(ctx, id) // every outcome is recorded on the task
		}(q.ID)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// failDependents settles every task that waits for parent, which just
// failed or was stopped, so they fail, and their waiting tasks in turn.
func (r *Runner) failDependents(parent *task.Task) error {
	tasks, err := r.Store.List()
	if err != nil {
		return err
	}
	var errs []error
	for _, q := range tasks {
		if waiting(q) && slices.Contains(q.After, parent.ID) {
			errs = append(errs, r.Settle(context.Background(), q.ID))
		}
	}
	return errors.Join(errs...)
}

// failDependent fails a waiting task for reason, and settles what waits on
// it.
func (r *Runner) failDependent(id, reason string) {
	t, changed, err := r.Store.Transition(id, task.Failed, []task.Status{task.Queued, task.Held}, func(t *task.Task) {
		t.Error = reason
	})
	if err != nil || !changed {
		return
	}
	r.emit(t, "failed", reason, "")
	r.ended(t)
}

// Settle decides the waiting task id from its parents in the store. A
// parent that failed, was stopped or does not exist fails it, and the tasks
// that wait on it in turn. Once every parent is done, it starts the task
// through a detached launcher, holds it for release (the human gate, or a
// plan phase whose check holds), or closes a fixup the review found nothing
// for. Otherwise it changes nothing. Each move is a guarded transition from
// queued, or held for a failure, so any number of callers may settle the
// same task: a parent's follow-up, the spawn that created it and the wait
// backstop's sweep.
func (r *Runner) Settle(ctx context.Context, id string) error {
	started, err := r.settle(ctx, id)
	if err != nil || !started {
		return err
	}
	_, err = r.startLauncher(id)
	return err
}

// settle is Settle without the launch: it reports whether it moved the task
// to starting, for the caller to launch.
func (r *Runner) settle(ctx context.Context, id string) (bool, error) {
	t, err := r.Store.Get(id)
	if err != nil || !waiting(t) {
		return false, err
	}
	pending := false
	parents := make([]*task.Task, 0, len(t.After))
	for _, pid := range t.After {
		p, err := r.Store.Get(pid)
		switch {
		case errors.Is(err, state.ErrNotFound):
			r.failDependent(t.ID, "parent "+pid+" does not exist")
			return false, nil
		case err != nil:
			return false, err
		case p.Status == task.Failed || p.Status == task.Stopped:
			r.failDependent(t.ID, fmt.Sprintf("parent %s %s", p.ID, p.Status))
			return false, nil
		case p.Status != task.Done:
			pending = true
		}
		parents = append(parents, p)
	}
	if pending || t.Status != task.Queued {
		return false, nil
	}
	for _, p := range parents {
		if p.ID != t.FixupOf || p.Review == nil {
			continue
		}
		if p.Review.Verdict == "" {
			// Its own follow-up may still be reading the verdict.
			r.finishReview(ctx, p)
			if p, err = r.Store.Get(p.ID); err != nil {
				return false, err
			}
		}
		if p.Review.Verdict == task.ReviewPass {
			// A fixup closed as done gets the follow-up a report would
			// start, for what waits on it.
			r.skipFixup(t, p)
			return false, r.StartAfter(t.ID)
		}
	}
	inputs, base, err := r.parentInputs(t.After)
	if err != nil {
		return false, err
	}
	withInputs := func(t *task.Task) {
		t.Inputs = inputs
		if t.Worktree != nil && t.Worktree.Base == "" {
			t.Worktree.Base = base
		}
	}
	if reason := r.phaseHoldReason(ctx, t); t.Gate == task.GateHuman || reason != "" {
		hold := func(t *task.Task) {
			withInputs(t)
			if reason != "" {
				t.HoldReason = reason
			}
		}
		t, changed, err := r.Store.Transition(t.ID, task.Held, []task.Status{task.Queued}, hold)
		if err == nil && changed {
			r.emit(t, "held", task.HeldSummary(t), "")
		}
		return false, err
	}
	// The status change is the lock: when two callers settle the task at
	// once, only one moves it to starting and launches it.
	_, changed, err := r.beginLaunch(t.ID, withInputs)
	return changed, err
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
		if rv := p.Review; rv != nil && rv.File != "" {
			line += " Review report: " + rv.File + " (verdict " + rv.Verdict + ")."
			if rv.Note != "" {
				line += " Note: " + rv.Note + "."
			}
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
