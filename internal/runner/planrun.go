package runner

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/khuebt/herdr-runner-kit/internal/planrun"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// phaseGitTimeout caps each git command of a phase check. The check runs in
// the detached follow-up, so a slow repository only delays the next phase.
const phaseGitTimeout = 10 * time.Second

// maxCheckedFiles is the most changed files a phase check stores.
const maxCheckedFiles = 100

// PlanLock is the store lock that serialises the runs of the plan in dir
// and the news that one of them is done.
func PlanLock(dir string) string {
	sum := sha1.Sum([]byte(filepath.Clean(dir)))
	return "plan-" + hex.EncodeToString(sum[:])[:12]
}

// PlanTasks returns the tasks of the plan in dir, oldest first.
func PlanTasks(tasks []*task.Task, dir string) []*task.Task {
	var out []*task.Task
	for _, t := range tasks {
		if t.Plan == dir {
			out = append(out, t)
		}
	}
	slices.SortStableFunc(out, func(a, b *task.Task) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out
}

// LatestPhaseTasks maps each phase number to its newest task among the
// tasks of one plan.
func LatestPhaseTasks(planTasks []*task.Task) map[int]*task.Task {
	latest := map[int]*task.Task{}
	for _, t := range planTasks {
		latest[t.Phase] = t // oldest first, so the newest wins
	}
	return latest
}

// PhaseDone reports whether phase ph counts as done: its newest task is
// done, or no task ran it and its file already says completed.
func PhaseDone(ph planrun.Phase, latest map[int]*task.Task) bool {
	if t, ok := latest[ph.Number]; ok {
		return t.Status == task.Done
	}
	return FileSaysDone(ph.Status)
}

// FileSaysDone reports whether a status line marks a phase finished.
func FileSaysDone(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "complete", "done":
		return true
	}
	return false
}

// PlanAnchor returns the newest task of a plan whose worktree it created
// and that still exists, or nil.
func PlanAnchor(planTasks []*task.Task) *task.Task {
	for _, t := range slices.Backward(planTasks) {
		w := t.Worktree
		if w != nil && w.BranchCreated != nil && *w.BranchCreated && w.Path != "" && !w.Removed && isDir(w.Path) {
			return t
		}
	}
	return nil
}

// PhasePrompt is the brief of the runner of phase ph, which follows the
// skill and the phase file. parallel are the phases that run in the same
// checkout at the same time, and earlier the phase's last task when it
// failed or was stopped.
func PhasePrompt(plan *planrun.Plan, ph planrun.Phase, parallel []planrun.Phase, earlier *task.Task) string {
	lines := []string{
		ph.File,
		"",
		fmt.Sprintf("You run phase %d of plan %s in this session; do only this phase.", ph.Number, plan.File),
	}
	if len(ph.Scope) > 0 {
		lines = append(lines, "Its write scope: "+strings.Join(ph.Scope, ", ")+".")
	} else {
		lines = append(lines, "It declares no write scope; change only what the phase asks for.")
	}
	lines = append(lines,
		"Do not edit plan.md or other phase files; the kit records phase status.",
		"Commit your work on this branch before you report done.")
	if len(parallel) > 0 {
		var scopes []string
		for _, p := range parallel {
			scopes = append(scopes, strings.Join(p.Scope, ", "))
		}
		lines = append(lines, "Another runner works in this checkout at the same time on "+strings.Join(scopes, "; ")+
			". Stage only your own paths (`git add -- <paths>`, never `git add -A`). If git reports `index.lock`, wait and retry. "+
			"A failing check caused by files outside your scope is not yours to fix; say so in your report.")
	}
	// An attempt that never launched, such as one failed with its parent,
	// left nothing in the checkout.
	launched := earlier != nil && (earlier.PhaseStart != "" || earlier.Herdr.PaneID != "")
	if launched && (earlier.Status == task.Failed || earlier.Status == task.Stopped) {
		what := string(earlier.Status)
		if earlier.Error != "" && earlier.Status == task.Failed {
			what += ": " + earlier.Error
		}
		lines = append(lines, fmt.Sprintf("An earlier attempt (%s) %s. The checkout may hold its partial work; inspect it and continue.", earlier.ID, what))
	}
	return strings.Join(lines, "\n")
}

// acceptPhase checks a done plan phase once: what changed in its checkout
// since it started, against its declared scope widened by the scopes of
// the phases that ran beside it. It records the check, emits it as
// phase-accepted, and marks the phase file completed.
func (r *Runner) acceptPhase(ctx context.Context, t *task.Task) {
	unlock, err := r.Store.LockFile(t.ID, "phase-check.lock")
	if err != nil {
		return
	}
	defer unlock()
	t, err = r.Store.Get(t.ID)
	if err != nil || t.PhaseCheck != nil {
		return
	}
	check := r.checkPhase(ctx, t)
	t, err = r.Store.Update(t.ID, func(t *task.Task) error {
		t.PhaseCheck = check
		return nil
	})
	if err != nil {
		return
	}
	r.emit(t, "phase-accepted", phaseEvidence(t.Phase, check), "")
	if plan, err := planrun.Read(t.Plan); err == nil {
		if ph, ok := plan.Phase(t.Phase); ok {
			if err := planrun.SetStatus(ph.File, "completed"); err != nil {
				r.emit(t, "plan-status-failed", err.Error(), "")
			}
		}
	}
}

func (r *Runner) checkPhase(ctx context.Context, t *task.Task) *task.PhaseCheck {
	unknown := func(note string) *task.PhaseCheck {
		return &task.PhaseCheck{Verdict: task.PhaseUnknown, Note: note}
	}
	plan, err := planrun.Read(t.Plan)
	if err != nil {
		return unknown("read the plan: " + err.Error())
	}
	ph, ok := plan.Phase(t.Phase)
	if !ok {
		return unknown(fmt.Sprintf("the plan has no phase %d", t.Phase))
	}
	dir, _ := checkoutDir(t)
	if dir == "" || t.PhaseStart == "" {
		return unknown("the phase's starting commit is unknown")
	}
	tasks, err := r.Store.List()
	if err != nil {
		return unknown(err.Error())
	}
	// Files an earlier phase left uncommitted were judged with that phase.
	var inherited []string
	for _, id := range t.After {
		if p, err := r.Store.Get(id); err == nil && p.PhaseCheck != nil {
			inherited = append(inherited, p.PhaseCheck.Dirty...)
		}
	}
	scope := slices.Clone(ph.Scope)
	var siblingScope []string
	for _, s := range phaseSiblings(t, tasks) {
		if sp, ok := plan.Phase(s.Phase); ok {
			scope = append(scope, sp.Scope...)
			siblingScope = append(siblingScope, sp.Scope...)
		}
	}

	c, cancel := context.WithTimeout(ctx, phaseGitTimeout)
	log, err := gitStrict(c, dir, "log", "--format=%x00%H", "--name-only", t.PhaseStart+"..HEAD")
	cancel()
	if err != nil {
		return unknown(err.Error())
	}
	c, cancel = context.WithTimeout(ctx, phaseGitTimeout)
	st, err := readStatus(c, dir)
	cancel()
	if err != nil {
		return unknown(err.Error())
	}

	check := &task.PhaseCheck{}
	var changed, dirty, fresh []string
	add := func(f string) {
		if f != "" && !slices.Contains(changed, f) {
			changed = append(changed, f)
		}
	}
	for _, files := range commitFiles(log) {
		// The phases of a wave commit on one branch. A commit that touches
		// a sibling's scope and none of this phase's is the sibling's.
		mine := slices.ContainsFunc(files, func(f string) bool { return planrun.InScope(ph.Scope, f) })
		theirs := slices.ContainsFunc(files, func(f string) bool { return planrun.InScope(siblingScope, f) })
		if theirs && !mine {
			continue
		}
		check.Commits++
		for _, f := range files {
			add(f)
		}
	}
	_, untracked := splitUntracked(st.Untracked)
	for _, f := range append(slices.Clone(st.Changed), untracked...) {
		// A sibling still at work leaves its own files uncommitted.
		if planrun.InScope(siblingScope, f) {
			continue
		}
		dirty = append(dirty, f)
		if !slices.Contains(inherited, f) {
			add(f)
			fresh = append(fresh, f)
		}
	}
	for _, f := range changed {
		// Plan scratch, such as a runner's reports, belongs to no scope.
		if len(ph.Scope) > 0 && !planrun.InScope(scope, f) && !strings.HasPrefix(f, scratchDir) {
			check.Outside = append(check.Outside, f)
		}
	}
	check.Clean = len(dirty) == 0
	check.Changed = changed[:min(len(changed), maxCheckedFiles)]
	check.Outside = check.Outside[:min(len(check.Outside), maxCheckedFiles)]
	check.Dirty = dirty[:min(len(dirty), maxCheckedFiles)]
	switch {
	case len(fresh) > 0:
		check.Note = "uncommitted: " + listPaths(fresh)
	case len(dirty) > 0:
		check.Note = "uncommitted, left by an earlier phase: " + listPaths(dirty)
	}
	switch {
	case len(check.Outside) > 0:
		check.Verdict = task.PhaseOutside
	case len(fresh) > 0:
		check.Verdict = task.PhaseDirty
	case check.Commits == 0 && len(changed) == 0:
		check.Verdict = task.PhaseNoChanges
	case len(ph.Scope) == 0:
		check.Verdict = task.PhaseNoDeclaredScope
	default:
		check.Verdict = task.PhaseAccepted
	}
	return check
}

// commitFiles splits `git log --format=%x00%H --name-only` output into the
// files of each commit.
func commitFiles(log string) [][]string {
	var commits [][]string
	for _, chunk := range strings.Split(log, "\x00") {
		lines := strings.Split(strings.TrimSpace(chunk), "\n")
		if len(lines) == 0 || lines[0] == "" {
			continue
		}
		var files []string
		for _, f := range lines[1:] {
			if f = strings.TrimSpace(f); f != "" {
				files = append(files, f)
			}
		}
		commits = append(commits, files)
	}
	return commits
}

// phaseSiblings returns the plan tasks that worked in t's checkout at the
// same time as t: launched, not among t's ancestors, and not finished
// before t was created. Earlier waves are t's ancestors, and earlier runs
// finished before this one spawned t.
func phaseSiblings(t *task.Task, tasks []*task.Task) []*task.Task {
	byID := map[string]*task.Task{}
	for _, q := range tasks {
		byID[q.ID] = q
	}
	ancestors := map[string]bool{}
	var walk func(ids []string)
	walk = func(ids []string) {
		for _, id := range ids {
			if ancestors[id] {
				continue
			}
			ancestors[id] = true
			if p := byID[id]; p != nil {
				walk(p.After)
			}
		}
	}
	walk(t.After)
	var out []*task.Task
	for _, q := range tasks {
		if q.ID == t.ID || q.Plan != t.Plan || q.PhaseStart == "" || ancestors[q.ID] {
			continue
		}
		if !q.FinishedAt.IsZero() && q.FinishedAt.Before(t.CreatedAt) {
			continue
		}
		out = append(out, q)
	}
	return out
}

// gitStrict runs git in dir and returns trimmed stdout, or an error that
// names a timeout.
func gitStrict(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks", "-c", "core.quotePath=false", "-C", dir}, args...)...)
	cmd.WaitDelay = gitWaitDelay
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("git %s timed out after %s", args[0], phaseGitTimeout)
	}
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}

// phaseEvidence is the one-line summary of a phase check.
func phaseEvidence(phase int, c *task.PhaseCheck) string {
	head := fmt.Sprintf("phase %d %s", phase, c.Verdict)
	if c.Verdict == task.PhaseUnknown {
		return head + ": " + c.Note
	}
	parts := []string{plural(strconv.Itoa(c.Commits), "commit"), plural(strconv.Itoa(len(c.Changed)), "file") + " changed"}
	if len(c.Outside) > 0 {
		parts = append(parts, "outside its scope: "+listPaths(c.Outside))
	}
	if c.Clean {
		parts = append(parts, "clean")
	} else {
		parts = append(parts, c.Note)
	}
	return head + ": " + strings.Join(parts, ", ")
}

// phaseHoldReason says why q, a plan task whose parents are all done, must
// wait for release: a parent of this run whose check holds. A parent that
// finished before q was created belongs to an earlier run, whose hold was
// already released.
func (r *Runner) phaseHoldReason(ctx context.Context, q *task.Task) string {
	if q.Plan == "" {
		return ""
	}
	for _, id := range q.After {
		p, err := r.Store.Get(id)
		if err != nil || p.Plan == "" || p.Status != task.Done || p.FinishedAt.Before(q.CreatedAt) {
			continue
		}
		if p.PhaseCheck == nil {
			// Its own follow-up may still be checking it.
			r.acceptPhase(ctx, p)
			if p, err = r.Store.Get(id); err != nil {
				continue
			}
		}
		c := p.PhaseCheck
		if !c.Holds() {
			continue
		}
		var reason string
		if c.Verdict == task.PhaseOutside {
			reason = fmt.Sprintf("phase %d changed files outside its scope: %s", p.Phase, listPaths(c.Outside))
		} else {
			reason = fmt.Sprintf("phase %d left its checkout dirty (%s)", p.Phase, c.Note)
		}
		return clip(reason, maxHoldReason-len("; release to continue")) + "; release to continue"
	}
	return ""
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// planDone emits plan-done once every phase of t's plan is done and no
// task of it is left unfinished: it writes the run report into the plan's
// reports folder and marks plan.md completed.
func (r *Runner) planDone(ctx context.Context, t *task.Task) error {
	unlock, err := r.Store.Lock(PlanLock(t.Plan))
	if err != nil {
		return err
	}
	defer unlock()
	plan, err := planrun.Read(t.Plan)
	if err != nil {
		return err
	}
	tasks, err := r.Store.List()
	if err != nil {
		return err
	}
	own := PlanTasks(tasks, t.Plan)
	for _, q := range own {
		if !q.Status.Terminal() {
			return nil
		}
	}
	latest := LatestPhaseTasks(own)
	var ids []string
	for _, ph := range plan.Phases {
		if !PhaseDone(ph, latest) {
			return nil
		}
		if q := latest[ph.Number]; q != nil {
			ids = append(ids, q.ID)
		}
	}
	// A phase that finished beside this one may still be in its own
	// follow-up's check; the report waits for it.
	for _, q := range latest {
		if q.Status == task.Done && q.PhaseCheck == nil {
			r.acceptPhase(ctx, q)
			if cur, err := r.Store.Get(q.ID); err == nil {
				latest[q.Phase] = cur
			}
		}
	}
	// One plan-done per set of finished phase tasks, however many
	// follow-ups see the plan done.
	marker := filepath.Join(r.Store.Dir(), "plans", PlanLock(t.Plan)+".done")
	key := strings.Join(ids, ",")
	if prev, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(prev)) == key {
		return nil
	}
	report, verified, err := writePlanReport(plan, latest, time.Now())
	if err != nil {
		r.emit(t, "plan-status-failed", "write the plan run report: "+err.Error(), "")
	}
	if err := planrun.SetStatus(plan.File, "completed"); err != nil {
		r.emit(t, "plan-status-failed", err.Error(), "")
	}
	ev := state.Event{
		Task: t.ID, Owner: t.Owner, Type: "plan-done", Status: t.Status,
		Summary: fmt.Sprintf("plan %s: %d/%d phases done, %d verified in scope", plan.Title, len(plan.Phases), len(plan.Phases), verified),
	}
	if report != "" {
		ev.Outputs = []string{report}
	}
	if err := r.record(ev); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		return err
	}
	return os.WriteFile(marker, []byte(key+"\n"), 0o600)
}

// writePlanReport writes <plan dir>/reports/plan-run-<YYMMDD-HHMM>.md: one
// row per phase with the runner's claim next to the kit's check. It
// returns the file and how many phases were verified in scope.
func writePlanReport(plan *planrun.Plan, latest map[int]*task.Task, now time.Time) (string, int, error) {
	verified := 0
	var b strings.Builder
	fmt.Fprintf(&b, "# Plan run: %s\n\n", plan.Title)
	fmt.Fprintf(&b, "- Plan: %s\n- Finished: %s\n\n", plan.File, now.Format("2006-01-02 15:04 MST"))
	b.WriteString("Each row puts the runner's claim next to what the kit verified in the checkout.\n\n")
	b.WriteString("| Phase | Task | Agent | Units | Claim | Verified |\n|---|---|---|---|---|---|\n")
	var total task.Usage
	for _, ph := range plan.Phases {
		t := latest[ph.Number]
		name := fmt.Sprintf("%d %s", ph.Number, ph.Title)
		if t == nil {
			fmt.Fprintf(&b, "| %s | none | | | | status %s in its file, not run by the kit |\n", cell(name), cell(ph.Status))
			continue
		}
		units := "-"
		if t.Usage != nil {
			units = task.Count(t.Usage.Units)
			total.Add(t.Usage)
		}
		claim := t.Summary
		if len(t.Decisions) > 0 {
			claim += " Decided: " + strings.Join(t.Decisions, "; ")
		}
		verification := "not checked"
		if c := t.PhaseCheck; c != nil {
			if c.Verdict == task.PhaseAccepted {
				verified++
			}
			verification = phaseEvidence(ph.Number, c)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", cell(name), t.ID, cell(t.Agent), units, cell(claim), cell(verification))
	}
	if total.Calls > 0 {
		fmt.Fprintf(&b, "| Total | | | %s | %s | |\n", task.Count(total.Units), cell(total.String()))
	}
	dir := filepath.Join(plan.Dir, "reports")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", verified, err
	}
	base := "plan-run-" + now.Format("060102-1504")
	for i := 1; ; i++ {
		name := base + ".md"
		if i > 1 {
			name = fmt.Sprintf("%s-%d.md", base, i)
		}
		path := filepath.Join(dir, name)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", verified, err
		}
		_, err = f.WriteString(b.String())
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return path, verified, err
	}
}

// cell makes text safe for one markdown table cell.
func cell(s string) string {
	return strings.ReplaceAll(strings.Join(strings.Fields(s), " "), "|", `\|`)
}
