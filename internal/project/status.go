package project

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// statusListCap bounds the runners and waiting tasks a status lists, so the
// whole report stays about one screen.
const statusListCap = 5

// Report is a read-only view of where a project stands, built from the
// kit's record and its tasks. It never reads herdr, so it works with herdr
// down.
type Report struct {
	Name, Path string
	// Record is nil when the kit holds no record for the project.
	Record *Record
	// Remote is the URL of the repository's origin, when it has one.
	Remote string
	// PathMissing is set when the project's folder no longer exists.
	PathMissing bool
	// Reserved is set while new has not finished setting the project up,
	// or was interrupted.
	Reserved bool
	// Stage is the stage of the project's latest staged task, else intake.
	Stage string
	// Runners are the project's tasks at work or about to start; Waiting
	// are those waiting on the user.
	Runners, Waiting []*task.Task
	// Active counts the runners that may be working now, which the
	// record's parallelWarning compares against.
	Active int
	// Latest holds the latest task of each stage. Tasks only hint at the
	// next step; gates are decided from committed artifacts.
	Latest map[string]*task.Task
	// Decide is the first open change request whose impact runner is done,
	// so it waits for the user's decision.
	Decide *Change
	// Running is the milestone in progress, if any; Nodes are the latest
	// tasks of its plan nodes, in the order they were first spawned.
	Running *Milestone
	Nodes   []*task.Task
	// PlanDone is set when the running milestone's plan runner is done.
	// The caller then reads the committed plan into Unanswered, or the
	// reason it cannot be read into PlanErr.
	PlanDone   bool
	Unanswered []string
	PlanErr    string
	// Close is the latest close runner of the running milestone, and
	// CloseStale is set when the release it prepared was merged before the
	// milestone was reopened.
	Close      *task.Task
	CloseStale bool
	Now        time.Time
}

// Status builds the report of a registered project from its record (nil
// when there is none) and the tasks of every project.
func Status(rec *Record, reg state.Project, tasks []*task.Task, now time.Time) Report {
	r := Report{
		Name: reg.Name, Path: reg.Path, Record: rec, Reserved: reg.Reserved, Stage: "intake",
		Latest: map[string]*task.Task{}, Now: now,
	}
	for _, t := range tasks {
		if t.Project != reg.Name {
			continue
		}
		if t.Stage != "" {
			r.Stage = t.Stage // tasks come in creation order: the last one wins
			r.Latest[t.Stage] = t
		}
		if runnerActive(t.Status) {
			r.Active++
		}
		switch t.Status {
		case task.NeedsHuman, task.Held:
			r.Waiting = append(r.Waiting, t)
		case task.Queued, task.Starting, task.Running:
			r.Runners = append(r.Runners, t)
		}
	}
	if rec != nil {
		done := map[string]bool{}
		for _, t := range tasks {
			if t.Project == reg.Name && t.Status == task.Done {
				done[t.ID] = true
			}
		}
		for i, c := range rec.Changes {
			if c.Status == ChangeOpen && done[c.Task] {
				r.Decide = &rec.Changes[i]
				break
			}
		}
		r.milestoneTasks(*rec, tasks)
	}
	return r
}

// milestoneTasks fills the running milestone and the latest task of each
// of its plan nodes.
func (r *Report) milestoneTasks(rec Record, tasks []*task.Task) {
	if r.Running = RunningMilestone(rec); r.Running == nil {
		return
	}
	first := FirstStage(r.Running.ID)
	if t := r.Latest[first]; t != nil && t.Status == task.Done && r.Running.ID != ScaffoldMilestone {
		r.PlanDone = true
	}
	r.Close = r.Latest[CloseStage(r.Running.ID)]
	r.CloseStale = CloseStale(*r.Running, r.Close)
	at := map[string]int{}
	for _, t := range tasks {
		if t.Project != r.Name || t.Stage == first || !strings.HasPrefix(t.Stage, r.Running.ID+"/") {
			continue
		}
		if i, seen := at[t.Stage]; seen {
			r.Nodes[i] = t
			continue
		}
		at[t.Stage] = len(r.Nodes)
		r.Nodes = append(r.Nodes, t)
	}
}

// Text renders the report: a header, one line per fact and one
// recommended next step.
func (r Report) Text() string {
	var b strings.Builder
	rec := r.Record
	head := []string{r.Name}
	if rec != nil {
		head = append(head, rec.Type+"/"+rec.Track)
	}
	head = append(head, r.Path)
	if rec != nil && rec.Graduated != nil {
		head = append(head, "graduated "+day(rec.Graduated.At))
	}
	b.WriteString(strings.Join(head, " · ") + "\n")
	if r.Reserved {
		b.WriteString("setup: not finished\n")
	}
	if r.PathMissing {
		b.WriteString("folder: missing\n")
	}
	if r.Remote != "" {
		fmt.Fprintf(&b, "remote: %s\n", RedactURL(r.Remote))
	}
	if rec == nil {
		b.WriteString("record: missing\n")
		fmt.Fprintf(&b, "next: %s\n", r.Next())
		return b.String()
	}
	if rec.ReferenceRepo != "" {
		fmt.Fprintf(&b, "reference: %s\n", RedactURL(rec.ReferenceRepo))
	}
	fmt.Fprintf(&b, "stage: %s\n", r.Stage)
	fmt.Fprintf(&b, "gates: spec %s · pack %s\n", gateLine(rec.Gates.Spec), gateLine(rec.Gates.Pack))
	if rec.Reapproval.Needed {
		fmt.Fprintf(&b, "re-approval needed before the next milestone: %s\n", orDash(oneLine(rec.Reapproval.Reason)))
	}
	b.WriteString("milestone: " + r.Milestone() + "\n")
	if r.Running != nil && len(r.Nodes) > 0 {
		parts := make([]string, 0, len(r.Nodes))
		for _, t := range r.Nodes {
			parts = append(parts, fmt.Sprintf("%s %s (%s)", strings.TrimPrefix(t.Stage, r.Running.ID+"/"), t.Status, t.ID))
		}
		b.WriteString("features: " + strings.Join(parts, " · ") + "\n")
	}
	b.WriteString("changes: " + changesLine(rec.Changes) + "\n")
	b.WriteString("blockers: " + blockersLine(rec.Dependencies) + "\n")
	r.writeTasks(&b, "runners", r.Runners)
	r.writeTasks(&b, "waiting on you", r.Waiting)
	if rec.ParallelWarning > 0 {
		fmt.Fprintf(&b, "active runners: %d (warning above %d)\n", r.Active, rec.ParallelWarning)
	} else {
		fmt.Fprintf(&b, "active runners: %d\n", r.Active)
	}
	fmt.Fprintf(&b, "records not yet committed: %d\n", rec.Pending)
	fmt.Fprintf(&b, "next: %s\n", r.Next())
	return b.String()
}

// changesLine lists the change requests not yet done, rejected or moved
// to the backlog: "CR-1 open (t7) · CR-2 scope next M2".
func changesLine(changes []Change) string {
	var parts []string
	for _, c := range changes {
		if c.Status != ChangeOpen && c.Status != ChangeNow && c.Status != ChangeNext {
			continue
		}
		words := []string{c.ID}
		if c.Class != "" {
			words = append(words, c.Class)
		}
		words = append(words, c.Status)
		if c.Blocking {
			words = append(words, "blocking")
		}
		if c.Milestone != "" {
			words = append(words, c.Milestone)
		}
		if c.Status == ChangeOpen && c.Task != "" {
			words = append(words, "("+c.Task+")")
		}
		parts = append(parts, strings.Join(words, " "))
	}
	return capped(parts)
}

// blockersLine lists the dependencies that are not ready.
func blockersLine(deps []Dependency) string {
	var parts []string
	for _, d := range deps {
		if d.Status == DependencyReady {
			continue
		}
		notes := []string{orDash(d.Status)}
		if d.Owner != "" {
			notes = append(notes, oneLine(d.Owner))
		}
		if d.Milestone != "" {
			notes = append(notes, "needed by "+d.Milestone)
		}
		parts = append(parts, fmt.Sprintf("%s %s (%s)", d.ID, oneLine(d.Name), strings.Join(notes, ", ")))
	}
	return capped(parts)
}

// capped joins at most statusListCap parts on one line, or says none.
func capped(parts []string) string {
	switch n := len(parts); {
	case n == 0:
		return "none"
	case n > statusListCap:
		return strings.Join(parts[:statusListCap], " · ") + fmt.Sprintf(" · and %d more", n-statusListCap)
	}
	return strings.Join(parts, " · ")
}

// Milestone describes the current milestone in a few words.
func (r Report) Milestone() string {
	if r.Record == nil {
		return "-"
	}
	m := currentMilestone(*r.Record)
	if m == nil {
		return "none yet"
	}
	return fmt.Sprintf("%s %s (%s)", m.ID, oneLine(m.Title), orDash(m.Status))
}

func (r Report) writeTasks(b *strings.Builder, label string, list []*task.Task) {
	if len(list) == 0 {
		fmt.Fprintf(b, "%s: none\n", label)
		return
	}
	fmt.Fprintf(b, "%s:\n", label)
	for i, t := range list {
		if i == statusListCap {
			fmt.Fprintf(b, "  and %d more\n", len(list)-i)
			break
		}
		fmt.Fprintf(b, "  %s\n", r.taskLine(t))
	}
}

// taskLine is "t3 needs-human 12m · write spec · Q: A or B?".
func (r Report) taskLine(t *task.Task) string {
	parts := []string{t.ID + " " + string(t.Status)}
	if t.Status == task.NeedsHuman || t.Status == task.Held {
		if !t.UpdatedAt.IsZero() && r.Now.After(t.UpdatedAt) {
			parts[0] += " " + r.Now.Sub(t.UpdatedAt).Round(time.Minute).String()
		}
	}
	if t.Stage != "" {
		parts = append(parts, "stage "+t.Stage)
	}
	if t.Title != "" {
		parts = append(parts, oneLine(t.Title))
	}
	if t.Status == task.Held && t.HoldReason != "" {
		parts = append(parts, "for: "+oneLine(t.HoldReason))
	}
	switch {
	case t.Status == task.NeedsHuman && t.Question != "":
		parts = append(parts, "Q: "+oneLine(t.Question))
	case t.Status == task.NeedsHuman && t.Error != "":
		parts = append(parts, "waits on the "+t.Error+" dialog in its tab")
	}
	return strings.Join(parts, " · ")
}

// Next is the one step the report recommends. The rules are checked in
// order and the first that applies wins.
func (r Report) Next() string {
	rec := r.Record
	switch {
	case r.Reserved:
		return fmt.Sprintf("wait for herdr-runner-kit new to finish; if it was interrupted, drop the project with: herdr-runner-kit project forget %s, then run new again", r.Name)
	case rec == nil:
		return fmt.Sprintf("the kit has no record of %s; import it again with: herdr-runner-kit project forget %s && herdr-runner-kit project register %s --name %s",
			r.Name, r.Name, shellCommand([]string{r.Path}), r.Name)
	case r.PathMissing:
		return fmt.Sprintf("the folder %s is gone; move it back, or drop the project with: herdr-runner-kit project forget %s", r.Path, r.Name)
	case len(r.Waiting) > 0:
		t := r.Waiting[0]
		more := ""
		if n := len(r.Waiting) - 1; n > 0 {
			more = fmt.Sprintf(" (%d more waiting)", n)
		}
		switch {
		case t.Status == task.Held && t.HoldReason != "":
			return fmt.Sprintf("release %s once this is settled: %s; then: herdr-runner-kit release %s%s", t.ID, oneLine(t.HoldReason), t.ID, more)
		case t.Status == task.Held:
			return fmt.Sprintf("release %s when it may start: herdr-runner-kit release %s%s", t.ID, t.ID, more)
		case t.Question != "":
			return fmt.Sprintf("answer %s: herdr-runner-kit answer %s \"<answer>\"%s", t.ID, t.ID, more)
		default:
			return fmt.Sprintf("answer the dialog of %s in its tab: herdr-runner-kit focus %s%s", t.ID, t.ID, more)
		}
	case r.Decide != nil:
		return fmt.Sprintf("%s's impact is in %s: ask the user, then record the decision: %s",
			r.Decide.ID, orDash(r.Decide.Path), r.command("cr decide "+strings.TrimPrefix(r.Decide.ID, "CR-")+" <now|next|backlog|reject>", ""))
	case len(r.Runners) > 0:
		return fmt.Sprintf("wait for the %d runner(s) at work: herdr-runner-kit wait", len(r.Runners))
	case rec.Gates.Spec.Status != GateApproved:
		return r.specNext()
	case rec.Gates.Pack.Status != GateApproved && !rec.Gates.Pack.Waived:
		return r.packNext()
	}
	if next := r.milestoneNext(); next != "" {
		return next
	}
	return fmt.Sprintf("spawn a runner for the next piece of work: herdr-runner-kit spawn --project %s --stdin", r.Name)
}

// milestoneNext is the next step once the pack is approved or waived, or
// "" when the roadmap holds nothing to do.
func (r Report) milestoneNext() string {
	rec := r.Record
	m := r.Running
	if m == nil {
		for _, next := range rec.Milestones {
			if next.Status == MilestoneDone {
				continue
			}
			if rec.Reapproval.Needed {
				return fmt.Sprintf("the pack needs a new approval before %s starts: %s, then approve it", next.ID, r.command("run pack", ""))
			}
			return fmt.Sprintf("start %s %s: %s", next.ID, oneLine(next.Title), r.command("milestone start "+next.ID, ""))
		}
		return ""
	}
	first := r.Latest[FirstStage(m.ID)]
	switch {
	case first == nil:
		return fmt.Sprintf("%s is in progress without a runner; start it: %s", m.ID, r.command("milestone start "+m.ID, ""))
	case first.Status != task.Done:
		return fmt.Sprintf("%s's runner %s %s; run it again: %s", m.ID, first.ID, first.Status, r.command("milestone start "+m.ID, ""))
	case m.ID == ScaffoldMilestone && r.Close == nil:
		branch := "its branch"
		if first.Worktree != nil {
			branch = first.Worktree.Branch
		}
		return fmt.Sprintf("%s's scaffold is done in %s: review %s", m.ID, first.ID, branch)
	case m.ID == ScaffoldMilestone:
	case r.PlanErr != "":
		return fmt.Sprintf("the plan of %s cannot be chained (%s); ask %s to fix it: herdr-runner-kit send %s \"<what to fix>\"",
			m.ID, oneLine(r.PlanErr), first.ID, first.ID)
	case len(r.Unanswered) > 0:
		return fmt.Sprintf("the plan of %s leaves %d open question(s) unanswered; ask %s to answer each with the recommended option: herdr-runner-kit send %s \"answer every open question in %s with its recommended option, commit and report again\"",
			m.ID, len(r.Unanswered), first.ID, first.ID, MilestonePlanPath(m.ID))
	case len(r.Nodes) == 0:
		return fmt.Sprintf("plan of %s done: read its decisions in %s, then chain its features: %s",
			m.ID, MilestonePlanPath(m.ID), r.command("milestone chain "+m.ID, ""))
	}
	for _, t := range r.Nodes {
		if t != r.Close && (t.Status == task.Failed || t.Status == task.Stopped) {
			return fmt.Sprintf("%s of %s %s; fix the cause, then run the chain again: %s", t.ID, t.Stage, t.Status, r.command("milestone chain "+m.ID, ""))
		}
	}
	if c := r.Close; c != nil {
		switch {
		case r.CloseStale:
			return fmt.Sprintf("%s was reopened after %s was merged; once the fixes are on the branch the release came from, prepare a new release: %s",
				m.ID, m.Branch, r.command("milestone close "+m.ID, ""))
		case c.Status == task.Done:
			return fmt.Sprintf("the release of %s is ready on %s: merge it into the default branch through your pull request, pull, then: %s",
				m.ID, orDash(taskBranch(c)), r.command("milestone done "+m.ID, ""))
		case c.Status == task.Failed || c.Status == task.Stopped:
			return fmt.Sprintf("the close runner %s of %s %s; fix the cause, then run it again: %s", c.ID, m.ID, c.Status, r.command("milestone close "+m.ID, ""))
		}
	}
	return fmt.Sprintf("every feature runner of %s is done", m.ID)
}

// ActiveRunners counts the runners of project name that may be working
// right now: starting, running, or waiting on a human in their tab.
// Queued and held tasks do not count.
func ActiveRunners(tasks []*task.Task, name string) int {
	n := 0
	for _, t := range tasks {
		if t.Project == name && runnerActive(t.Status) {
			n++
		}
	}
	return n
}

// ParallelNote is the soft warning printed after a project spawn when more
// of its runners are active than its record's parallelWarning, or "" when
// there are not, or the warning is off (0). It never blocks anything.
func ParallelNote(rec Record, tasks []*task.Task) string {
	n := ActiveRunners(tasks, rec.Name)
	if rec.ParallelWarning <= 0 || n <= rec.ParallelWarning {
		return ""
	}
	return fmt.Sprintf("note: %d runners of %s are active (warning above %d); nothing was blocked", n, rec.Name, rec.ParallelWarning)
}

// laterStages run between the spec and the pack, in the order status
// suggests them.
var laterStages = []string{"ux", "design", "architecture", "roadmap"}

// specNext is the next step before the spec is approved.
func (r Report) specNext() string {
	spec := r.Latest["spec"]
	switch {
	case spec != nil && spec.Status == task.Done && r.revised(spec, r.Record.Gates.Spec):
		return fmt.Sprintf("spec done: ask the user to approve it, then record the outcome: %s (or %s --stdin with the feedback)",
			r.command("approve spec", "--by NAME"), r.command("changes spec", ""))
	case spec != nil && spec.Status == task.Done:
		return fmt.Sprintf("revise the spec with the feedback: %s", r.command("run spec", "--stdin"))
	case spec != nil && spec.Status.Terminal():
		return fmt.Sprintf("the spec runner %s %s; run it again: %s", spec.ID, spec.Status, r.command("run spec", ""))
	case r.done("intake"):
		return fmt.Sprintf("write the spec from the brief summary: %s", r.command("run spec", ""))
	}
	return fmt.Sprintf("start discovery from the brief in %s: %s", BriefDir, r.command("run intake", ""))
}

// packNext is the next step after the spec is approved and before the pack
// is approved or waived.
func (r Report) packNext() string {
	pack := r.Latest["pack"]
	switch {
	case pack != nil && pack.Status == task.Done && r.revised(pack, r.Record.Gates.Pack):
		return fmt.Sprintf("pack done: share pack.html / pack.pdf from %s/ with the stakeholders, then record the outcome: %s (or %s --stdin, or %s --note WHY)",
			ApprovalDir, r.command("approve pack", "--by NAME"), r.command("changes pack", ""), r.command("reject pack", ""))
	case pack != nil && pack.Status == task.Done:
		return fmt.Sprintf("revise the pack with the feedback: %s", r.command("run pack", "--stdin"))
	}
	for _, stage := range laterStages {
		if !r.done(stage) {
			return fmt.Sprintf("run the next discovery stage: %s (chain the others with --after)", r.command("run "+stage, ""))
		}
	}
	return fmt.Sprintf("assemble the stakeholder approval pack: %s", r.command("run pack", ""))
}

// revised reports whether t finished after the gate's last outcome, so it
// holds a version still waiting for one.
func (r Report) revised(t *task.Task, g Gate) bool {
	return g.Status == "" || g.At.IsZero() || t.FinishedAt.After(g.At)
}

func (r Report) done(stage string) bool {
	t := r.Latest[stage]
	return t != nil && t.Status == task.Done
}

// command spells a project subcommand for this project.
func (r Report) command(sub, extra string) string {
	c := "herdr-runner-kit project " + sub + " --project " + r.Name
	if extra != "" {
		c += " " + extra
	}
	return c
}

// Origin returns the URL of the repository's origin remote, or "" when it
// has none or cannot be read. Print it through RedactURL.
func Origin(ctx context.Context, dir string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
