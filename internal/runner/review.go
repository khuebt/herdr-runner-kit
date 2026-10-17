package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/config"
	"github.com/khuebt/herdr-runner-kit/internal/harness"
	"github.com/khuebt/herdr-runner-kit/internal/planrun"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// planHelper reports whether t is a plan's review or fixup task, which is
// no phase: it is never checked, held for or counted as one.
func planHelper(t *task.Task) bool { return t.Review != nil || t.FixupOf != "" }

// WantsReview reports whether a spawn of kind chains a review runner: flag
// when given, else the kind's review default. Kinds fix, research and
// report are never reviewed.
func (r *Runner) WantsReview(kind string, flag *bool) (bool, error) {
	if kind == "" {
		kind = r.Config.DefaultKind
	}
	k, ok := r.Config.Kinds[kind]
	if !ok {
		return false, fmt.Errorf("unknown kind %q", kind)
	}
	want := k.Review
	if flag != nil {
		want = *flag
	}
	if want && slices.Contains(config.UnreviewedKinds, kind) {
		return false, fmt.Errorf("--review applies to flow and code tasks, not kind %s", kind)
	}
	return want, nil
}

// ReviewerAgent returns the agent spec of the runner that reviews the work
// of writerAgent: override, else the config's review.agent, else the
// default reviewer on another harness.
func (r *Runner) ReviewerAgent(writerAgent, override string) (string, error) {
	for _, a := range []string{override, r.Config.Review.Agent} {
		if a == "" {
			continue
		}
		spec, err := task.ParseAgentSpec(a)
		if err != nil {
			return "", fmt.Errorf("review agent: %w", err)
		}
		return spec.String(), nil
	}
	writer, err := task.ParseAgentSpec(writerAgent)
	if err != nil {
		return "", err
	}
	installed := func(bin string) bool {
		_, err := r.LookPath(bin)
		return err == nil
	}
	return harness.DefaultReviewer(writer, installed).String(), nil
}

// ReviewSpawn says what a review task reviews and where it runs.
type ReviewSpawn struct {
	// Writer is the reviewed task or, for a plan review, one of the plan's
	// phase tasks: the reviewer's quota fallback never takes its agent.
	Writer *task.Task
	// Plan makes it a plan review that works in the run's checkout
	// CheckoutOf after the tasks After; Final marks the review before
	// plan-done.
	Plan       *planrun.Plan
	CheckoutOf string
	After      []string
	Final      bool
	Agent      string // reviewer agent spec; empty asks ReviewerAgent
	Warn       func(string)
}

// SpawnReview records a review task queued after the work it reviews.
func (r *Runner) SpawnReview(ctx context.Context, o ReviewSpawn) (*task.Task, error) {
	w := o.Writer
	agent := o.Agent
	if agent == "" {
		var err error
		if agent, err = r.ReviewerAgent(w.Agent, ""); err != nil {
			return nil, err
		}
	}
	so := SpawnOptions{
		Kind: w.Kind, Agent: agent, Worktree: new(false), Skill: r.Config.Review.Skill,
		Cwd: w.Cwd, Owner: w.Owner, Project: w.Project, Language: w.Language, Warn: o.Warn,
	}
	writer := agentKey(w.Agent, w.ClaudeConfigDir)
	var subject string
	if p := o.Plan; p == nil {
		subject = fmt.Sprintf("the work of task %s (%s). The spec to review it against is its brief:\n\n%s", w.ID, w.Title, w.Prompt)
		if w.Skill != "" {
			subject += "\n\nIt ran the skill " + w.Skill + "."
		}
		so.Title = "review of " + w.ID + ": " + w.Title
		so.After = []string{w.ID}
		if w.UseWorktree {
			so.CheckoutOf = w.ID
		}
		so.Review = &task.Review{Of: w.ID, Writer: writer}
	} else {
		subject = fmt.Sprintf("the work of the plan run %q on this branch. The spec to review it against is %s and its phase files in %s.", p.Title, p.File, p.Dir)
		so.Title = "review: " + p.Title
		if o.Final {
			so.Title = "final review: " + p.Title
		}
		so.After, so.CheckoutOf, so.Plan = o.After, o.CheckoutOf, p.Dir
		so.Review = &task.Review{Final: o.Final, Writer: writer}
	}
	so.Prompt = ReviewPrompt(subject)
	return r.Spawn(ctx, so)
}

// ReviewPrompt is the brief of a review runner; subject names the work and
// the spec it is reviewed against.
func ReviewPrompt(subject string) string {
	return fmt.Sprintf(`You are an independent reviewer. Review %s

Look for what tests miss: bugs, regressions, cases the change did not consider, races, drift from the spec, security problems, and tests that are weak, wrong or cover the wrong scenario.

Rules:
- You are read-only. Never edit, create, stage or commit project files, never switch branches, never push. The one file you write is the review report named below.
- Verify each finding before you list it: cite the file and line, and show the input or state that fails; run a focused test or command when it settles the question. Mark each finding verified or plausible.
- Write the report in this layout:
  # Review: <what you reviewed>
  Verdict: pass | fix
  Reviewed: <base>..<head sha>
  ## Findings
  1. [High|Medium|Low] path:line. What is wrong, the failing scenario, the fix you suggest. verified|plausible
- Write Verdict: fix when at least one verified finding should be fixed before merge, otherwise Verdict: pass.
- Report done with a summary that starts with "Verdict: pass" or "Verdict: fix" and the number of verified findings, and pass the report with --output.`, subject)
}

// ReviewFixNote follows the brief of a plan phase that runs after review
// reviewID: it fixes the verified findings inside its own scope first.
func ReviewFixNote(reviewID string) string {
	return "Task " + reviewID + " reviewed the work before this phase; its report is named in your inputs below (Review report:). " +
		"Before your phase work, fix its verified findings that lie inside your write scope, in their own fix: commit, " +
		"and name in your done summary each finding you fixed and each you left because it lies outside your scope."
}

// FixupPrompt is the brief of the fixup runner after a plan's final review.
func FixupPrompt(planFile, reviewID string) string {
	return "Task " + reviewID + " reviewed the plan run of " + planFile + " and asked for fixes; its report is named in your inputs below (Review report:). " +
		"Fix each verified finding in this checkout, with a focused test where the finding names a missing or wrong test. " +
		"Leave plausible findings unless the fix is small and clear, and say which you left. " +
		"Run the checks the plan names, commit the fixes on this branch as fix: commits, and name in your done summary each finding you fixed."
}

// prepareReview sets what a review's launch needs: the commit its diff
// starts from, its report file and, for a plan, the earlier reports. A
// relaunch keeps them.
func (r *Runner) prepareReview(t *task.Task) (*task.Task, error) {
	if t.Review == nil || (t.Review.Base != "" && t.Review.File != "") {
		return t, nil
	}
	var base, dir string
	var earlier []string
	if t.Plan == "" {
		dir = r.OutDir(t.ID)
		if of, err := r.Store.Get(t.Review.Of); err == nil {
			base = of.PhaseStart
		}
	} else {
		dir = filepath.Join(t.Plan, "reports")
		list, err := r.Store.List()
		if err != nil {
			return nil, err
		}
		own := PlanTasks(list, t.Plan)
		for _, q := range slices.Backward(own) {
			if q.ID != t.ID && q.Review != nil && q.Status == task.Done && base == "" {
				base = q.PhaseStart
			}
		}
		if base == "" {
			for _, q := range own {
				if !planHelper(q) && q.PhaseStart != "" {
					base = q.PhaseStart
					break
				}
			}
		}
		for _, q := range own {
			if q.ID != t.ID && q.Review != nil && q.Status == task.Done && q.Review.File != "" {
				earlier = append(earlier, q.Review.File)
			}
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	stem := filepath.Join(dir, "review-"+time.Now().Format("060102-1504"))
	file := stem + ".md"
	for i := 2; fileExists(file); i++ {
		file = stem + "-" + strconv.Itoa(i) + ".md"
	}
	return r.Store.Update(t.ID, func(t *task.Task) error {
		if t.Review.Base == "" {
			t.Review.Base = base
		}
		if t.Review.File == "" {
			t.Review.File = file
			t.Review.Earlier = earlier
		}
		return nil
	})
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// reviewRange is the part of a review's first prompt that names the diff,
// the report file and the earlier reports.
func reviewRange(t *task.Task) string {
	rv := t.Review
	var s string
	if rv.Base != "" {
		s = "Review range: run git log --oneline " + rv.Base + "..HEAD and git diff " + rv.Base + "..HEAD in " + t.Cwd + "."
	} else {
		s = "The starting commit is unknown: review this branch against its merge-base with the repository's default branch."
	}
	s += " Write the review report to " + rv.File + "; it is the only file you write."
	if len(rv.Earlier) > 0 {
		s += " Earlier reviews of this plan: " + strings.Join(rv.Earlier, ", ") +
			". Check whether their verified findings were fixed, and list each one still open as a finding."
	}
	return s
}

var verdictRe = regexp.MustCompile(`(?mi)^[\s>*_#-]*verdict[\s*_]*:[\s*_]*(pass|fix)\b`)

// ReadVerdict reads the verdict of the review report file.
func ReadVerdict(file string) (verdict, note string) {
	if file == "" {
		return task.ReviewUnknown, "no review report was named"
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return task.ReviewUnknown, "no review report at " + file
	}
	m := verdictRe.FindSubmatch(data)
	if m == nil {
		return task.ReviewUnknown, "the report has no Verdict: pass or Verdict: fix line"
	}
	return strings.ToLower(string(m[1])), ""
}

// checkoutChanges names what changed in t's checkout since the review
// started: a read-only reviewer should leave nothing. It is empty when
// nothing changed or git cannot say.
func checkoutChanges(ctx context.Context, t *task.Task) string {
	dir, _ := checkoutDir(t)
	if dir == "" || t.PhaseStart == "" {
		return ""
	}
	c, cancel := context.WithTimeout(ctx, phaseGitTimeout)
	count, err := gitStrict(c, dir, "rev-list", "--count", t.PhaseStart+"..HEAD")
	cancel()
	if err != nil {
		return ""
	}
	c, cancel = context.WithTimeout(ctx, phaseGitTimeout)
	st, err := readStatus(c, dir)
	cancel()
	if err != nil {
		return ""
	}
	var parts []string
	if count != "0" {
		parts = append(parts, plural(count, "commit"))
	}
	if len(st.Changed) > 0 {
		parts = append(parts, "files "+listPaths(st.Changed))
	}
	if len(parts) == 0 {
		return ""
	}
	return "the checkout changed during the review: " + strings.Join(parts, ", ")
}

// finishReview records the verdict of a done review task, and a note when
// the report is missing or the checkout changed, and emits review-verdict.
func (r *Runner) finishReview(ctx context.Context, t *task.Task) {
	if t.Review.Verdict != "" {
		return // a second follow-up of the same done report
	}
	verdict, note := ReadVerdict(t.Review.File)
	notes := []string{}
	if note != "" {
		notes = append(notes, note)
	}
	if c := checkoutChanges(ctx, t); c != "" {
		notes = append(notes, c)
	}
	note = strings.Join(notes, "; ")
	t, err := r.Store.Update(t.ID, func(t *task.Task) error {
		t.Review.Verdict, t.Review.Note = verdict, note
		return nil
	})
	if err != nil {
		return
	}
	summary := "verdict " + verdict + "; report " + t.Review.File
	if note != "" {
		summary += "; " + note
	}
	r.emit(t, "review-verdict", summary, "")
}

// skipFixup closes the queued fixup task q as done without a runner, since
// review found nothing to fix.
func (r *Runner) skipFixup(q, review *task.Task) {
	summary := "skipped: review " + review.ID + " found nothing to fix"
	t, changed, err := r.Store.Transition(q.ID, task.Done, []task.Status{task.Queued}, func(t *task.Task) { t.Summary = summary })
	if err != nil || !changed {
		return
	}
	r.emit(t, "fixup-skipped", summary, "")
}
