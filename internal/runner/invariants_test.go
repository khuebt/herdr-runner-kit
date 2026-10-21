package runner

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/project"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

var invT0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// invTask is a task as the store holds it: a terminal one has finishedAt.
func invTask(id string, st task.Status, edit ...func(*task.Task)) *task.Task {
	t := &task.Task{ID: id, Status: st, CreatedAt: invT0}
	if st.Terminal() {
		t.FinishedAt = invT0
	}
	for _, e := range edit {
		e(t)
	}
	return t
}

func after(ids ...string) func(*task.Task) { return func(t *task.Task) { t.After = ids } }

func stage(name, st string) func(*task.Task) {
	return func(t *task.Task) { t.Project, t.Stage = name, st }
}

func branch(b, base string) func(*task.Task) {
	return func(t *task.Task) { t.UseWorktree, t.Worktree = true, &task.Worktree{Branch: b, Base: base} }
}

func invSnap(tasks ...*task.Task) Snapshot {
	return Snapshot{Tasks: tasks, Projects: map[string]project.Record{}, Now: invT0.Add(time.Hour)}
}

func withRecord(s Snapshot, ms ...project.Milestone) Snapshot {
	rec := project.NewRecord("p")
	rec.Milestones = ms
	s.Projects = map[string]project.Record{"p": rec}
	return s
}

func milestone(id, status string, features ...string) project.Milestone {
	m := project.Milestone{ID: id, Title: "milestone " + id, Status: status}
	for _, f := range features {
		m.Features = append(m.Features, project.Feature{ID: f, Title: f})
	}
	if status == project.MilestoneDone {
		m.Tag, m.Closed, m.Released, m.Branch = "v0."+id+".0", invT0, []string{"notes"}, "release/"+id
	}
	return m
}

// closer is the done close runner every done milestone of these tests has.
func closer(id, m string) *task.Task { return invTask(id, task.Done, stage("p", m+"/close")) }

func rulesOf(vs []Violation) []string {
	var out []string
	for _, v := range vs {
		out = append(out, v.Rule)
	}
	return out
}

func TestCheckInvariants(t *testing.T) {
	recent := func(s Snapshot) Snapshot {
		s.Tasks[0].FinishedAt = s.Now.Add(-time.Minute)
		s.Settle = 2 * time.Minute
		return s
	}
	inFlight := func(s Snapshot) Snapshot { s.InFlight = map[string]bool{"t1": true}; return s }
	prev := func(s Snapshot, p Snapshot) Snapshot { s.Prev = &p; return s }
	withPlan := func(plan string, phase int) func(*task.Task) {
		return func(t *task.Task) { t.Plan, t.Phase = plan, phase }
	}
	planDone := func(s Snapshot, ids ...string) Snapshot { s.PlanDone = map[string][]string{"/plan": ids}; return s }

	cases := []struct {
		name string
		snap Snapshot
		want []string
	}{
		// waiting-task-has-a-path
		{
			"child of a failed parent", invSnap(invTask("t1", task.Failed), invTask("t2", task.Queued, after("t1"))),
			[]string{"waiting-task-has-a-path"},
		},
		{
			"held child of a stopped parent", invSnap(invTask("t1", task.Stopped), invTask("t2", task.Held, after("t1"))),
			[]string{"waiting-task-has-a-path"},
		},
		{
			"queued child of done parents", invSnap(invTask("t1", task.Done), invTask("t2", task.Queued, after("t1"))),
			[]string{"waiting-task-has-a-path"},
		},
		{"queued with no parent", invSnap(invTask("t1", task.Queued)), []string{"waiting-task-has-a-path"}},
		{
			"child of a missing parent", invSnap(invTask("t2", task.Queued, after("t1"))),
			[]string{"links-resolve", "waiting-task-has-a-path"},
		},
		{"child of a running parent", invSnap(invTask("t1", task.Running), invTask("t2", task.Queued, after("t1"))), nil},
		{"held child of a done parent", invSnap(invTask("t1", task.Done), invTask("t2", task.Held, after("t1"))), nil},
		{"child of a parent that just failed", recent(invSnap(invTask("t1", task.Failed), invTask("t2", task.Queued, after("t1")))), nil},
		{"child of a parent whose follow-up is on its way", inFlight(invSnap(invTask("t1", task.Done), invTask("t2", task.Queued, after("t1")))), nil},

		// worktree-recorded
		{
			"worktree task with no worktree", invSnap(invTask("t1", task.Running, func(t *task.Task) { t.UseWorktree = true })),
			[]string{"worktree-recorded"},
		},
		{"worktree task with its branch", invSnap(invTask("t1", task.Running, branch("b", ""))), nil},

		// stopped-gets-no-agent
		{
			"stopped task with an open agent", invSnap(invTask("t1", task.Stopped, func(t *task.Task) { t.Herdr.PaneID = "w1:p1" })),
			[]string{"stopped-gets-no-agent"},
		},
		{"stopped task whose agent was closed", invSnap(invTask("t1", task.Stopped, func(t *task.Task) { t.Herdr.PaneID, t.Herdr.Closed = "w1:p1", true })), nil},

		// terminal-stays-terminal
		{
			"stopped task running again", prev(invSnap(invTask("t1", task.Running)), invSnap(invTask("t1", task.Stopped))),
			[]string{"terminal-stays-terminal"},
		},
		{
			"done task failed later", prev(invSnap(invTask("t1", task.Failed)), invSnap(invTask("t1", task.Done))),
			[]string{"terminal-stays-terminal"},
		},
		{"done task reopened by send", prev(invSnap(invTask("t1", task.Running)), invSnap(invTask("t1", task.Done))), nil},

		// fields-match-status
		{
			"running task with a question", invSnap(invTask("t1", task.Running, func(t *task.Task) { t.Question = "q" })),
			[]string{"fields-match-status"},
		},
		{
			"done task with no finishedAt", invSnap(invTask("t1", task.Done, func(t *task.Task) { t.FinishedAt = time.Time{} })),
			[]string{"fields-match-status"},
		},
		{
			"starting task holding a done", invSnap(invTask("t1", task.Starting, func(t *task.Task) { t.PendingDone = &task.PendingDone{} })),
			[]string{"fields-match-status"},
		},
		{"needs-human task with a question", invSnap(invTask("t1", task.NeedsHuman, func(t *task.Task) { t.Question = "q" })), nil},
		{"stopped task keeps its last question", invSnap(invTask("t1", task.Stopped, func(t *task.Task) { t.Question = "q" })), nil},

		// links-resolve
		{
			"fixup of a missing review", invSnap(invTask("t1", task.Done, func(t *task.Task) { t.FixupOf = "t9" })),
			[]string{"links-resolve"},
		},
		{"task waiting for itself", invSnap(invTask("t1", task.Queued, after("t1"))), []string{"links-resolve"}},
		{
			"two tasks waiting for each other", invSnap(invTask("t1", task.Queued, after("t2")), invTask("t2", task.Queued, after("t1"))),
			[]string{"links-resolve", "links-resolve"},
		},
		{"review of an existing task", invSnap(invTask("t1", task.Done), invTask("t2", task.Done, func(t *task.Task) { t.Review = &task.Review{Of: "t1"} })), nil},

		// one-live-runner-per-step
		{
			"two live runners of a stage", invSnap(invTask("t1", task.Running, stage("p", "spec")), invTask("t2", task.Running, stage("p", "spec"))),
			[]string{"one-live-runner-per-step"},
		},
		{
			"two live runners of a plan phase", invSnap(invTask("t1", task.Running, withPlan("/plan", 1)), invTask("t2", task.Starting, withPlan("/plan", 1))),
			[]string{"one-live-runner-per-step"},
		},
		{"a failed runner and its respawn", invSnap(invTask("t1", task.Failed, stage("p", "spec")), invTask("t2", task.Running, stage("p", "spec"))), nil},
		{"two phases of a plan at once", invSnap(invTask("t1", task.Running, withPlan("/plan", 1)), invTask("t2", task.Running, withPlan("/plan", 2))), nil},

		// plan-run-finishes
		{"finished plan with no plan-done", invSnap(invTask("t1", task.Done, withPlan("/plan", 1))), []string{"plan-run-finishes"}},
		{"plan-done of an earlier set", planDone(invSnap(
			invTask("t1", task.Done, withPlan("/plan", 1)),
			invTask("t2", task.Stopped, withPlan("/plan", 0), func(t *task.Task) { t.Review = &task.Review{Final: true} }),
		), "t1"), []string{"plan-run-finishes"}},
		{"finished plan with its plan-done", planDone(invSnap(
			invTask("t1", task.Done, withPlan("/plan", 1)),
			invTask("t2", task.Done, withPlan("/plan", 0), func(t *task.Task) { t.Review = &task.Review{Final: true} }),
			invTask("t3", task.Done, withPlan("/plan", 0), func(t *task.Task) { t.FixupOf = "t2" }),
		), "t1", "t2", "t3"), nil},
		{"plan with a failed phase", invSnap(invTask("t1", task.Failed, withPlan("/plan", 1))), nil},
		{"plan still running", invSnap(invTask("t1", task.Done, withPlan("/plan", 1)), invTask("t2", task.Running, withPlan("/plan", 2))), nil},

		// child-follows-current-parents
		{"child of a replaced runner", invSnap(
			invTask("t1", task.Failed, stage("p", "M1/F1")),
			invTask("t2", task.Running, stage("p", "M1/F1")),
			invTask("t3", task.Running, stage("p", "M1/F2")),
			invTask("t4", task.Queued, stage("p", "M1/integrate"), after("t1", "t3")),
		), []string{"child-follows-current-parents", "waiting-task-has-a-path"}},
		{"child of a replaced done runner", invSnap(
			invTask("t1", task.Done, stage("p", "M1/F1")),
			invTask("t2", task.Running, stage("p", "M1/F1")),
			invTask("t3", task.Running, stage("p", "M1/F2")),
			invTask("t4", task.Queued, stage("p", "M1/integrate"), after("t1", "t3")),
		), []string{"child-follows-current-parents"}},
		{"child of the current runners", invSnap(
			invTask("t1", task.Failed, stage("p", "M1/F1")),
			invTask("t2", task.Running, stage("p", "M1/F1")),
			invTask("t3", task.Running, stage("p", "M1/F2")),
			invTask("t4", task.Queued, stage("p", "M1/integrate"), after("t2", "t3")),
		), nil},

		// base-is-a-parent-branch
		{"child from the branch of a task it does not wait for", invSnap(
			invTask("t1", task.Done, stage("p", "M1/F1"), branch("f1", "")),
			invTask("t2", task.Running, stage("p", "M1/F2"), branch("f2", "")),
			invTask("t3", task.Queued, stage("p", "M1/close"), after("t2"), branch("release/M1", "f1")),
		), []string{"base-is-a-parent-branch"}},
		{"child from its parent's branch", invSnap(
			invTask("t1", task.Done, stage("p", "M1/F1"), branch("f1", "")),
			invTask("t2", task.Running, stage("p", "M1/F2"), branch("f2", "")),
			invTask("t3", task.Queued, stage("p", "M1/close"), after("t2"), branch("release/M1", "f2")),
		), nil},
		{"child from the default branch", invSnap(
			invTask("t2", task.Running, stage("p", "M1/F2"), branch("f2", "")),
			invTask("t3", task.Queued, stage("p", "M1/close"), after("t2"), branch("release/M1", "main")),
		), nil},

		// done-milestone-frozen
		{"done milestone renamed", prev(
			withRecord(invSnap(closer("t1", "M1")), func() project.Milestone { m := milestone("M1", project.MilestoneDone); m.Title = "renamed"; return m }()),
			withRecord(invSnap(closer("t1", "M1")), milestone("M1", project.MilestoneDone)),
		), []string{"done-milestone-frozen"}},
		{"done milestone lost a feature", prev(
			withRecord(invSnap(closer("t1", "M1")), milestone("M1", project.MilestoneDone, "F1")),
			withRecord(invSnap(closer("t1", "M1")), milestone("M1", project.MilestoneDone, "F1", "F2")),
		), []string{"done-milestone-frozen"}},
		{"done milestone reopened", prev(
			withRecord(invSnap(closer("t1", "M1")), milestone("M1", project.MilestoneInProgress)),
			withRecord(invSnap(closer("t1", "M1")), milestone("M1", project.MilestoneDone)),
		), nil},
		{"feature moved to a later milestone", prev(
			withRecord(invSnap(closer("t1", "M1")), milestone("M1", project.MilestoneDone, "F1"), milestone("M2", project.MilestonePlanned, "F2")),
			withRecord(invSnap(closer("t1", "M1")), milestone("M1", project.MilestoneDone, "F1", "F2"), milestone("M2", project.MilestonePlanned)),
		), nil},

		// milestone-order
		{"planned milestone before a done one", withRecord(invSnap(closer("t1", "M2")),
			milestone("M1", project.MilestonePlanned), milestone("M2", project.MilestoneDone)), []string{"milestone-order"}},
		{
			"two milestones in progress", withRecord(invSnap(closer("t1", "M1")),
				milestone("M1", project.MilestoneDone), milestone("M2", project.MilestoneInProgress), milestone("M3", project.MilestoneInProgress)),
			[]string{"milestone-order"},
		},
		{"unknown milestone status", withRecord(invSnap(), milestone("M1", "")), []string{"milestone-order"}},
		{"milestones in order", withRecord(invSnap(closer("t1", "M1")),
			milestone("M1", project.MilestoneDone), milestone("M2", project.MilestoneInProgress), milestone("M3", project.MilestonePlanned)), nil},

		// feature-in-one-milestone
		{"feature in two milestones", withRecord(invSnap(),
			milestone("M1", project.MilestonePlanned, "F1"), milestone("M2", project.MilestonePlanned, "F1")), []string{"feature-in-one-milestone"}},
		{"features in one milestone each", withRecord(invSnap(),
			milestone("M1", project.MilestonePlanned, "F1"), milestone("M2", project.MilestonePlanned, "F2")), nil},

		// done-milestone-has-release
		{"done milestone with no tag", withRecord(invSnap(closer("t1", "M1")),
			func() project.Milestone { m := milestone("M1", project.MilestoneDone); m.Tag = ""; return m }()), []string{"done-milestone-has-release"}},
		{"done milestone whose close runner failed", withRecord(invSnap(invTask("t1", task.Failed, stage("p", "M1/close"))),
			milestone("M1", project.MilestoneDone)), []string{"done-milestone-has-release"}},
		{"done milestone with its release", withRecord(invSnap(closer("t1", "M1")), milestone("M1", project.MilestoneDone)), nil},

		// done-milestone-has-no-live-runner
		{"live runner of a done milestone", withRecord(invSnap(closer("t1", "M1"), invTask("t2", task.Running, stage("p", "M1/F1"))),
			milestone("M1", project.MilestoneDone)), []string{"done-milestone-has-no-live-runner"}},
		{"live runner of the milestone in progress", withRecord(invSnap(closer("t1", "M1"), invTask("t2", task.Running, stage("p", "M2/F1"))),
			milestone("M1", project.MilestoneDone), milestone("M2", project.MilestoneInProgress)), nil},

		// cleanup-spares-live-work
		{"live task in a removed shared checkout", invSnap(
			invTask("t1", task.Done, branch("a", ""), func(t *task.Task) { t.Worktree.Removed = true }),
			invTask("t2", task.Running, func(t *task.Task) { t.CheckoutOf = "t1" }),
		), []string{"cleanup-spares-live-work"}},
		{"waiting task from a deleted branch", invSnap(
			invTask("t1", task.Done, branch("a", ""), func(t *task.Task) { t.Worktree.BranchDeleted = true }),
			invTask("t2", task.Running),
			invTask("t3", task.Queued, after("t2"), branch("b", "a")),
		), []string{"cleanup-spares-live-work"}},
		{"waiting task from a deleted parent branch", invSnap(
			invTask("t1", task.Held, branch("a", ""), func(t *task.Task) { t.Worktree.BranchDeleted = true }),
			invTask("t2", task.Queued, after("t1"), branch("b", "")),
		), []string{"cleanup-spares-live-work"}},
		{"waiting task from a kept branch", invSnap(
			invTask("t1", task.Done, branch("a", ""), func(t *task.Task) { t.Worktree.Removed = true }),
			invTask("t2", task.Running),
			invTask("t3", task.Queued, after("t2"), branch("b", "a")),
		), nil},

		// live-task-has-checkout
		{
			"live task whose worktree was removed", invSnap(invTask("t1", task.Running, branch("a", ""), func(t *task.Task) { t.Worktree.Removed = true })),
			[]string{"live-task-has-checkout"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CheckInvariants(c.snap)
			if !slices.Equal(rulesOf(got), c.want) {
				t.Errorf("rules = %q, want %q; violations: %v", rulesOf(got), c.want, got)
			}
		})
	}
}

func TestCheckInvariantsSortsViolations(t *testing.T) {
	got := CheckInvariants(invSnap(
		invTask("t10", task.Running, func(t *task.Task) { t.Question = "q" }),
		invTask("t9", task.Running, func(t *task.Task) { t.Question = "q" }),
		invTask("t2", task.Queued),
	))
	var ids []string
	for _, v := range got {
		ids = append(ids, v.Rule+" "+v.Task)
	}
	want := []string{"fields-match-status t9", "fields-match-status t10", "waiting-task-has-a-path t2"}
	if !slices.Equal(ids, want) {
		t.Errorf("order = %q, want %q", ids, want)
	}
}

// checkStore asserts that the store r's flows left behind breaks no rule.
func checkStore(t *testing.T, r *Runner) {
	t.Helper()
	s, err := r.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := CheckInvariants(s); len(got) > 0 {
		t.Errorf("violations: %v", got)
	}
}

func TestInvariantsHoldAfterDoneParentStartsChild(t *testing.T) {
	r, _, _ := newTestRunner(t)
	p := running(t, r, SpawnOptions{Prompt: "a", Cwd: plainDir(t)})
	c := spawnQueued(t, r, p.ID)
	if _, err := r.Report(p.ID, ReportOptions{Status: task.Done, Summary: "did a"}); err != nil {
		t.Fatal(err)
	}
	// The follow-up has not run yet: the child waits on a done parent.
	s, err := r.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := rulesOf(CheckInvariants(s)); !slices.Equal(got, []string{"waiting-task-has-a-path"}) {
		t.Fatalf("before the follow-up: %q", got)
	}
	s.InFlight = map[string]bool{p.ID: true}
	if got := CheckInvariants(s); len(got) > 0 {
		t.Fatalf("with the follow-up in flight: %v", got)
	}
	if err := r.After(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Store.Get(c); got.Status == task.Queued {
		t.Fatalf("child still queued")
	}
	checkStore(t, r)
}

func TestInvariantsHoldAfterStopFailsDependents(t *testing.T) {
	r, _, _ := newTestRunner(t)
	dir := plainDir(t)
	p := running(t, r, SpawnOptions{Prompt: "a", Cwd: dir})
	c := spawnStarting(t, r, SpawnOptions{Prompt: "b", Cwd: dir, After: []string{p.ID}})
	if _, err := r.Stop(context.Background(), p.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Store.Get(c.ID); got.Status != task.Failed {
		t.Fatalf("child = %s", got.Status)
	}
	checkStore(t, r)
}

func TestInvariantsHoldAfterPlanReviewAndFixup(t *testing.T) {
	r, _, _ := newTestRunner(t)
	review, fixup := finalReviewFixture(t, r, writeReport(t, "Verdict: fix\n"))
	finishTask(t, r, review.ID, task.Done, "Verdict: fix, 1 verified finding")
	if err := r.After(context.Background(), review.ID); err != nil {
		t.Fatal(err)
	}
	finishTask(t, r, fixup.ID, task.Done, "fixed it")
	if err := r.After(context.Background(), fixup.ID); err != nil {
		t.Fatal(err)
	}
	if len(planDoneSummaries(t, r)) != 1 {
		t.Fatal("no plan-done")
	}
	checkStore(t, r)
}

func TestInvariantSnapshotReadsProjectRecords(t *testing.T) {
	r, _, _ := newTestRunner(t)
	if _, err := r.Store.ReserveProject("p", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.CommitProject("p"); err != nil {
		t.Fatal(err)
	}
	rec := project.NewRecord("p")
	m := milestone("M1", project.MilestoneDone)
	m.Tag = ""
	rec.Milestones = []project.Milestone{m}
	if err := project.Save(r.Store, rec); err != nil {
		t.Fatal(err)
	}
	s, err := r.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	got := CheckInvariants(s)
	if len(got) != 1 || got[0].Rule != "done-milestone-has-release" || got[0].Project != "p" {
		t.Fatalf("violations = %v", got)
	}
}
