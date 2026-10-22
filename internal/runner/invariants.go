package runner

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/project"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// Snapshot is what CheckInvariants reads: the tasks, the records of the
// registered projects, and which parents still have a follow-up on its way.
type Snapshot struct {
	Tasks    []*task.Task // in creation order, as Store.List returns them
	Projects map[string]project.Record
	// PlanDone maps a plan folder to the task ids its plan-done marker
	// holds, for the plans that have one.
	PlanDone map[string][]string
	Now      time.Time
	Settle   time.Duration   // liveness rules skip parents finished more recently
	InFlight map[string]bool // parent ids whose follow-up has not run yet (tests)
	// Prev, when set, is an earlier snapshot of the same store; the rules
	// on transitions compare against it.
	Prev *Snapshot
}

// Violation is one broken rule.
type Violation struct{ Rule, Task, Project, Detail string }

func (v Violation) String() string {
	return strings.Join(slices.DeleteFunc([]string{v.Rule, v.Project, v.Task, v.Detail}, func(s string) bool { return s == "" }), ": ")
}

// Snapshot reads the store: every task, the record of every registered
// project, and every plan-done marker. Now is the current time; Settle and
// InFlight are the caller's to set.
func (r *Runner) Snapshot() (Snapshot, error) {
	tasks, err := r.Store.List()
	if err != nil {
		return Snapshot{}, err
	}
	s := Snapshot{Tasks: tasks, Projects: map[string]project.Record{}, Now: time.Now()}
	projects, err := r.Store.Projects()
	if err != nil {
		return Snapshot{}, err
	}
	for _, p := range projects {
		rec, err := project.Load(r.Store, p.Name)
		if errors.Is(err, project.ErrNoRecord) {
			continue // reserved, or registered by a version before records
		}
		if err != nil {
			return Snapshot{}, err
		}
		s.Projects[p.Name] = rec
	}
	if s.PlanDone, err = r.planMarkers(tasks); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}

// planMarkers maps the plan folder of each plan task to the task ids of the
// plan-done marker planDone wrote for it, for the plans that have one.
func (r *Runner) planMarkers(tasks []*task.Task) (map[string][]string, error) {
	out := map[string][]string{}
	read := map[string]bool{}
	for _, t := range tasks {
		if t.Plan == "" || read[t.Plan] {
			continue
		}
		read[t.Plan] = true
		data, err := os.ReadFile(filepath.Join(r.Store.Dir(), "plans", PlanLock(t.Plan)+".done"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[t.Plan] = strings.Split(strings.TrimSpace(string(data)), ",")
	}
	return out, nil
}

// CheckInvariants returns every rule of docs/state-model.md that s breaks,
// sorted by rule, then project, then task. It reads nothing but s.
func CheckInvariants(s Snapshot) []Violation {
	c := newCheck(s)
	var out []Violation
	for _, rule := range rules {
		for _, v := range rule.check(c) {
			v.Rule = rule.name
			out = append(out, v)
		}
	}
	slices.SortStableFunc(out, func(a, b Violation) int {
		return cmp.Or(strings.Compare(a.Rule, b.Rule), strings.Compare(a.Project, b.Project), compareIDs(a.Task, b.Task))
	})
	return out
}

// compareIDs orders task ids by number, so t9 comes before t10.
func compareIDs(a, b string) int {
	return cmp.Or(cmp.Compare(len(a), len(b)), strings.Compare(a, b))
}

var rules = []struct {
	name  string
	check func(*check) []Violation
}{
	{"waiting-task-has-a-path", waitingTaskHasAPath},
	{"worktree-recorded", worktreeRecorded},
	{"stopped-gets-no-agent", stoppedGetsNoAgent},
	{"terminal-stays-terminal", terminalStaysTerminal},
	{"fields-match-status", fieldsMatchStatus},
	{"links-resolve", linksResolve},
	{"one-live-runner-per-step", oneLiveRunnerPerStep},
	{"plan-run-finishes", planRunFinishes},
	{"child-follows-current-parents", childFollowsCurrentParents},
	{"base-is-a-parent-branch", baseIsAParentBranch},
	{"done-milestone-frozen", doneMilestoneFrozen},
	{"milestone-order", milestoneOrder},
	{"feature-in-one-milestone", featureInOneMilestone},
	{"done-milestone-has-release", doneMilestoneHasRelease},
	{"done-milestone-has-no-live-runner", doneMilestoneHasNoLiveRunner},
	{"cleanup-spares-live-work", cleanupSparesLiveWork},
	{"live-task-has-checkout", liveTaskHasCheckout},
}

// check is a snapshot with the lookups the rules share.
type check struct {
	Snapshot
	byID map[string]*task.Task
	// newest maps "<project>\x00<stage>" to the newest task of that stage.
	newest map[string]*task.Task
}

func newCheck(s Snapshot) *check {
	c := &check{Snapshot: s, byID: map[string]*task.Task{}, newest: map[string]*task.Task{}}
	for _, t := range s.Tasks {
		c.byID[t.ID] = t
		if t.Project != "" && t.Stage != "" {
			c.newest[stageKey(t.Project, t.Stage)] = t // creation order, so the newest wins
		}
	}
	return c
}

func stageKey(name, stage string) string { return name + "\x00" + stage }

// settling reports whether t may still have a follow-up on its way: it is
// in flight, or it finished within the settle window.
func (c *check) settling(t *task.Task) bool {
	return c.InFlight[t.ID] || t.Status.Terminal() && c.Now.Sub(t.FinishedAt) < c.Settle
}

// newestOfStage reports whether t is the newest task of its project stage.
func (c *check) newestOfStage(t *task.Task) bool {
	return c.newest[stageKey(t.Project, t.Stage)] == t
}

func waiting(t *task.Task) bool { return t.Status == task.Queued || t.Status == task.Held }

// chainTask reports whether t runs a step of a milestone of its project.
func chainTask(t *task.Task) bool {
	m, _, ok := strings.Cut(t.Stage, "/")
	return t.Project != "" && ok && m != ""
}

func taskViolation(t *task.Task, format string, args ...any) Violation {
	return Violation{Task: t.ID, Project: t.Project, Detail: fmt.Sprintf(format, args...)}
}

// waitingTaskHasAPath: a queued task has a parent that is not terminal, and
// no waiting task has a parent that failed, stopped or does not exist.
func waitingTaskHasAPath(c *check) []Violation {
	var out []Violation
	for _, t := range c.Tasks {
		if !waiting(t) {
			continue
		}
		if t.Status == task.Queued && len(t.After) == 0 {
			if c.Now.Sub(t.CreatedAt) >= c.Settle {
				out = append(out, taskViolation(t, "queued with no parent"))
			}
			continue
		}
		live, settling, problem := false, false, ""
		for _, id := range t.After {
			p := c.byID[id]
			switch {
			case p == nil:
				problem = cmp.Or(problem, fmt.Sprintf("parent %s does not exist", id))
			case !p.Status.Terminal():
				live = true
			case c.settling(p):
				settling = true
			case p.Status != task.Done:
				problem = cmp.Or(problem, fmt.Sprintf("parent %s is %s", id, p.Status))
			}
		}
		switch {
		case problem != "" && !settling:
			out = append(out, taskViolation(t, "%s %s", t.Status, problem))
		case t.Status == task.Queued && !live && !settling && problem == "":
			out = append(out, taskViolation(t, "queued, but every parent is done"))
		}
	}
	return out
}

// worktreeRecorded: a task that wants a worktree has one with a branch.
func worktreeRecorded(c *check) []Violation {
	var out []Violation
	for _, t := range c.Tasks {
		if t.UseWorktree && (t.Worktree == nil || t.Worktree.Branch == "") {
			out = append(out, taskViolation(t, "wants a worktree but has no branch recorded"))
		}
	}
	return out
}

// stoppedGetsNoAgent: a stopped task holds no agent: once its pane is
// recorded, its runner was closed or is being closed.
func stoppedGetsNoAgent(c *check) []Violation {
	var out []Violation
	for _, t := range c.Tasks {
		if t.Status == task.Stopped && t.Herdr.PaneID != "" && !t.Herdr.Closed && t.Herdr.ClosingSince.IsZero() && !c.settling(t) {
			out = append(out, taskViolation(t, "stopped, but its agent in %s was never closed", t.Herdr.PaneID))
		}
	}
	return out
}

// terminalStaysTerminal: a terminal task leaves its status only when send
// reopens a done or failed one. A reopen makes it running, from where it can
// need a human or start a fallback, but it never waits again.
func terminalStaysTerminal(c *check) []Violation {
	if c.Prev == nil {
		return nil
	}
	var out []Violation
	for _, before := range c.Prev.Tasks {
		t := c.byID[before.ID]
		if t == nil || !before.Status.Terminal() || t.Status == before.Status {
			continue
		}
		reopened := t.Status == task.Running || t.Status == task.NeedsHuman || t.Status == task.Starting
		if reopened && (before.Status == task.Done || before.Status == task.Failed) {
			continue // reopened by send
		}
		out = append(out, taskViolation(t, "went from %s to %s", before.Status, t.Status))
	}
	return out
}

// fieldsMatchStatus: finishedAt is set exactly when the task is terminal;
// a live task holds a done only while running and a question only while it
// needs a human.
func fieldsMatchStatus(c *check) []Violation {
	var out []Violation
	for _, t := range c.Tasks {
		terminal := t.Status.Terminal()
		switch {
		case terminal && t.FinishedAt.IsZero():
			out = append(out, taskViolation(t, "%s with no finishedAt", t.Status))
		case !terminal && !t.FinishedAt.IsZero():
			out = append(out, taskViolation(t, "%s with finishedAt set", t.Status))
		case !terminal && t.PendingDone != nil && t.Status != task.Running:
			out = append(out, taskViolation(t, "%s while it holds a done", t.Status))
		case !terminal && t.Question != "" && t.Status != task.NeedsHuman:
			out = append(out, taskViolation(t, "%s while it holds a question", t.Status))
		}
	}
	return out
}

// linksResolve: every task a task names exists, and after names neither
// the task itself nor a task that waits for it.
func linksResolve(c *check) []Violation {
	var out []Violation
	for _, t := range c.Tasks {
		links := [][2]string{{"checkoutOf", t.CheckoutOf}, {"fixupOf", t.FixupOf}}
		if t.Review != nil {
			links = append(links, [2]string{"review.of", t.Review.Of})
		}
		for _, id := range t.After {
			links = append(links, [2]string{"after", id})
		}
		var missing []string
		for _, l := range links {
			if l[1] != "" && c.byID[l[1]] == nil {
				missing = append(missing, l[0]+" "+l[1])
			}
		}
		switch {
		case len(missing) > 0:
			out = append(out, taskViolation(t, "names missing tasks: %s", strings.Join(missing, ", ")))
		case slices.Contains(t.After, t.ID):
			out = append(out, taskViolation(t, "waits for itself"))
		case c.waitsFor(t.After, t.ID, map[string]bool{}):
			out = append(out, taskViolation(t, "waits for a task that waits for it"))
		}
	}
	return out
}

// waitsFor reports whether one of ids, or a task they wait for, is target.
func (c *check) waitsFor(ids []string, target string, seen map[string]bool) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		if p := c.byID[id]; p != nil && c.waitsFor(p.After, target, seen) {
			return true
		}
	}
	return false
}

// oneLiveRunnerPerStep: at most one task is live per project stage, per
// plan phase and per plan's final review.
func oneLiveRunnerPerStep(c *check) []Violation {
	type step struct{ key, name string }
	live := map[step][]*task.Task{}
	var order []step
	add := func(st step, t *task.Task) {
		if live[st] == nil {
			order = append(order, st)
		}
		live[st] = append(live[st], t)
	}
	for _, t := range c.Tasks {
		if t.Status.Terminal() {
			continue
		}
		if t.Project != "" && t.Stage != "" {
			add(step{"stage\x00" + stageKey(t.Project, t.Stage), "stage " + t.Stage}, t)
		}
		switch {
		case t.Plan == "":
		case t.Review != nil && t.Review.Final:
			add(step{"review\x00" + t.Plan, "the final review of " + t.Plan}, t)
		case !planHelper(t):
			add(step{fmt.Sprintf("phase\x00%s\x00%d", t.Plan, t.Phase), fmt.Sprintf("phase %d of %s", t.Phase, t.Plan)}, t)
		}
	}
	var out []Violation
	for _, st := range order {
		ts := live[st]
		if len(ts) < 2 {
			continue
		}
		ids := make([]string, len(ts))
		for i, t := range ts {
			ids[i] = t.ID
		}
		out = append(out, taskViolation(ts[len(ts)-1], "%s are all live for %s", strings.Join(ids, ", "), st.name))
	}
	return out
}

// planRunFinishes: once every task of a plan run is terminal and every
// phase is done, the plan-done marker holds that set of tasks.
func planRunFinishes(c *check) []Violation {
	plans := map[string]bool{}
	var out []Violation
	for _, t := range c.Tasks {
		if t.Plan == "" || plans[t.Plan] {
			continue
		}
		plans[t.Plan] = true
		own := PlanTasks(c.Tasks, t.Plan)
		if slices.ContainsFunc(own, func(q *task.Task) bool { return !q.Status.Terminal() || c.settling(q) }) {
			continue
		}
		latest := LatestPhaseTasks(own)
		var want []string
		for _, q := range latest {
			if q.Status != task.Done {
				want = nil
				break
			}
			want = append(want, q.ID)
		}
		if len(want) == 0 {
			continue // a phase did not finish, so the run is not done
		}
		var review, fixup *task.Task
		for _, q := range own { // oldest first, so the newest wins, as in planDone
			if q.Review != nil && q.Review.Final {
				review = q
			}
			if q.FixupOf != "" {
				fixup = q
			}
		}
		for _, q := range []*task.Task{review, fixup} {
			if q != nil {
				want = append(want, q.ID)
			}
		}
		got := slices.Clone(c.PlanDone[t.Plan])
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(want, got) {
			last := own[len(own)-1]
			out = append(out, taskViolation(last, "every task of plan %s finished, but no plan-done names %s", t.Plan, strings.Join(want, ", ")))
		}
	}
	return out
}

// childFollowsCurrentParents: a queued chain task waits for the newest
// runner of each milestone step it waits for.
func childFollowsCurrentParents(c *check) []Violation {
	var out []Violation
	for _, t := range c.Tasks {
		if t.Status != task.Queued || !chainTask(t) {
			continue
		}
		for _, id := range t.After {
			p := c.byID[id]
			if p == nil || p.Project != t.Project || !chainTask(p) || c.newestOfStage(p) {
				continue
			}
			out = append(out, taskViolation(t, "waits for %s, but %s is the newest runner of %s", p.ID, c.newest[stageKey(p.Project, p.Stage)].ID, p.Stage))
			break
		}
	}
	return out
}

// baseIsAParentBranch: when a waiting chain task starts from the branch of
// another task of its project, it waits for that task, the newest runner
// of its stage.
func baseIsAParentBranch(c *check) []Violation {
	var out []Violation
	for _, t := range c.Tasks {
		if !waiting(t) || !chainTask(t) || t.Worktree == nil || t.Worktree.Base == "" {
			continue
		}
		var owners []*task.Task
		for _, o := range c.Tasks {
			if o != t && o.Project == t.Project && o.Worktree != nil && o.Worktree.Branch == t.Worktree.Base {
				owners = append(owners, o)
			}
		}
		if len(owners) == 0 {
			continue // not a kit task's branch, such as the default branch
		}
		if !slices.ContainsFunc(owners, func(o *task.Task) bool { return slices.Contains(t.After, o.ID) && c.newestOfStage(o) }) {
			out = append(out, taskViolation(t, "starts from %s of %s, which is not a current parent", t.Worktree.Base, owners[len(owners)-1].ID))
		}
	}
	return out
}

// projectNames returns the names of the snapshot's projects, sorted.
func (c *check) projectNames() []string {
	names := make([]string, 0, len(c.Projects))
	for name := range c.Projects {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// doneMilestoneFrozen: a done milestone changes only by a reopen, which
// sets it in progress. A feature it lost to a later milestone that is not
// done counts as moved, which Load does on its own.
func doneMilestoneFrozen(c *check) []Violation {
	if c.Prev == nil {
		return nil
	}
	var out []Violation
	for _, name := range c.projectNames() {
		before, ok := c.Prev.Projects[name]
		if !ok {
			continue
		}
		rec := c.Projects[name]
		for _, old := range before.Milestones {
			if old.Status != project.MilestoneDone {
				continue
			}
			i := slices.IndexFunc(rec.Milestones, func(m project.Milestone) bool { return m.ID == old.ID })
			if i < 0 {
				out = append(out, Violation{Project: name, Detail: fmt.Sprintf("done milestone %s is gone", old.ID)})
				continue
			}
			m := rec.Milestones[i]
			if m.Status == project.MilestoneInProgress {
				continue // reopened
			}
			if changed := frozenChanges(old, m, rec.Milestones[i+1:]); len(changed) > 0 {
				out = append(out, Violation{Project: name, Detail: fmt.Sprintf("done milestone %s changed %s", old.ID, strings.Join(changed, ", "))})
			}
		}
	}
	return out
}

// frozenChanges names the fields of done milestone old that m changed;
// later are the milestones after it.
func frozenChanges(old, m project.Milestone, later []project.Milestone) []string {
	var changed []string
	if m.Status != old.Status {
		changed = append(changed, "status")
	}
	if m.Title != old.Title {
		changed = append(changed, "title")
	}
	if !m.Planned.Equal(old.Planned) {
		changed = append(changed, "planned")
	}
	if m.Tag != old.Tag {
		changed = append(changed, "tag")
	}
	if !m.Closed.Equal(old.Closed) {
		changed = append(changed, "closed")
	}
	if m.Branch != old.Branch {
		changed = append(changed, "branch")
	}
	moved := func(f project.Feature) bool {
		return slices.ContainsFunc(later, func(l project.Milestone) bool {
			return l.Status != project.MilestoneDone && slices.ContainsFunc(l.Features, func(lf project.Feature) bool { return lf.ID == f.ID })
		})
	}
	kept := slices.DeleteFunc(slices.Clone(old.Features), moved)
	if !slices.EqualFunc(kept, m.Features, func(a, b project.Feature) bool {
		return a.ID == b.ID && a.Title == b.Title && slices.Equal(a.DependsOn, b.DependsOn)
	}) {
		changed = append(changed, "features")
	}
	return changed
}

// milestoneOrder: every milestone is planned, in progress or done, at
// most one is in progress, and every milestone before one in progress or
// done is done.
func milestoneOrder(c *check) []Violation {
	var out []Violation
	for _, name := range c.projectNames() {
		ms := c.Projects[name].Milestones
		bad := func(format string, args ...any) {
			out = append(out, Violation{Project: name, Detail: fmt.Sprintf(format, args...)})
		}
		var running []string
		last := -1
		for i, m := range ms {
			switch m.Status {
			case project.MilestoneInProgress:
				running = append(running, m.ID)
				last = i
			case project.MilestoneDone:
				last = i
			case project.MilestonePlanned:
			default:
				bad("milestone %s has status %q", m.ID, m.Status)
			}
		}
		if len(running) > 1 {
			bad("milestones %s are all in progress", strings.Join(running, ", "))
		}
		for _, m := range ms[:max(last, 0)] {
			if m.Status != project.MilestoneDone {
				bad("milestone %s is %s, but %s after it is %s", m.ID, m.Status, ms[last].ID, ms[last].Status)
			}
		}
	}
	return out
}

// featureInOneMilestone: a feature id appears in one milestone at most.
func featureInOneMilestone(c *check) []Violation {
	var out []Violation
	for _, name := range c.projectNames() {
		where := map[string]string{}
		for _, m := range c.Projects[name].Milestones {
			for _, f := range m.Features {
				if first, ok := where[f.ID]; ok && first != m.ID {
					out = append(out, Violation{Project: name, Detail: fmt.Sprintf("feature %s is in %s and %s", f.ID, first, m.ID)})
					continue
				}
				where[f.ID] = m.ID
			}
		}
	}
	return out
}

// doneMilestoneHasRelease: a done milestone has a tag, a closed date,
// released notes and a done close runner.
func doneMilestoneHasRelease(c *check) []Violation {
	var out []Violation
	for _, name := range c.projectNames() {
		for _, m := range c.Projects[name].Milestones {
			if m.Status != project.MilestoneDone {
				continue
			}
			var missing []string
			if m.Tag == "" {
				missing = append(missing, "a tag")
			}
			if m.Closed.IsZero() {
				missing = append(missing, "a closed date")
			}
			if len(m.Released) == 0 {
				missing = append(missing, "released notes")
			}
			if closer := c.newest[stageKey(name, project.CloseStage(m.ID))]; closer == nil || closer.Status != task.Done {
				missing = append(missing, "a done close runner")
			}
			if len(missing) > 0 {
				out = append(out, Violation{Project: name, Detail: fmt.Sprintf("done milestone %s has no %s", m.ID, strings.Join(missing, ", "))})
			}
		}
	}
	return out
}

// doneMilestoneHasNoLiveRunner: no task of a done milestone's steps is live.
func doneMilestoneHasNoLiveRunner(c *check) []Violation {
	var out []Violation
	for _, t := range c.Tasks {
		if t.Status.Terminal() || !chainTask(t) {
			continue
		}
		rec, ok := c.Projects[t.Project]
		if !ok {
			continue
		}
		m, _, _ := strings.Cut(t.Stage, "/")
		if slices.ContainsFunc(rec.Milestones, func(ms project.Milestone) bool { return ms.ID == m && ms.Status == project.MilestoneDone }) {
			out = append(out, taskViolation(t, "%s, but milestone %s is done", t.Status, m))
		}
	}
	return out
}

// cleanupSparesLiveWork: cleanup has not removed the checkout a live task
// shares, nor deleted the branch a waiting task will start from.
func cleanupSparesLiveWork(c *check) []Violation {
	// The newest task that owns each branch of a repository.
	owner := map[[2]string]*task.Task{}
	for _, t := range c.Tasks {
		if t.Worktree != nil && t.Worktree.Branch != "" {
			owner[[2]string{t.RepoRoot, t.Worktree.Branch}] = t
		}
	}
	var out []Violation
	for _, t := range c.Tasks {
		if t.Status.Terminal() {
			continue
		}
		if o := c.byID[t.CheckoutOf]; o != nil && o.Worktree != nil && o.Worktree.Removed {
			out = append(out, taskViolation(t, "%s, but the worktree of %s it works in was removed", t.Status, o.ID))
			continue
		}
		if !waiting(t) || t.Worktree == nil {
			continue
		}
		from := t.Worktree.Base
		if p := c.byID[firstOf(t.After)]; from == "" && len(t.After) == 1 && p != nil && p.Worktree != nil {
			from = p.Worktree.Branch
		}
		if o := owner[[2]string{t.RepoRoot, from}]; from != "" && o != nil && o.Worktree.BranchDeleted {
			out = append(out, taskViolation(t, "%s to start from %s, which cleanup deleted with %s", t.Status, from, o.ID))
		}
	}
	return out
}

func firstOf(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

// liveTaskHasCheckout: the worktree of a live task was not removed.
func liveTaskHasCheckout(c *check) []Violation {
	var out []Violation
	for _, t := range c.Tasks {
		if !t.Status.Terminal() && t.Worktree != nil && t.Worktree.Removed {
			out = append(out, taskViolation(t, "%s, but its worktree was removed", t.Status))
		}
	}
	return out
}
