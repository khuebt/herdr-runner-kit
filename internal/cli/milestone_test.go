package cli

import (
	"bytes"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/project"
	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

const roadmapProposal = "# Roadmap proposal\n\n```yaml\nmilestones:\n" +
	"  - {id: M0, title: Scaffold}\n" +
	"  - id: M1\n    title: Lists\n    features:\n      - {id: F1, title: Add a list}\n```\n"

// approvedRunner returns a stage runner whose project has the spec and the
// pack approved, with the roadmap M0, M1 imported.
func approvedRunner(t *testing.T) (*runner.Runner, state.Project) {
	t.Helper()
	r, dir := stageRunner(t)
	gitIn(t, dir, "branch", "-M", "main")
	commitFiles(t, dir, specFile("1"))
	if _, err := gateCmd(t, r, "approve", "spec", ""); err != nil {
		t.Fatal(err)
	}
	commitFiles(t, dir, map[string]string{
		project.PackDir(1) + "/pack.html": "<html></html>\n", project.RoadmapProposalPath: roadmapProposal,
	})
	if _, err := gateCmd(t, r, "approve", "pack", ""); err != nil {
		t.Fatal(err)
	}
	p, err := r.Store.ProjectByName("demo-app")
	if err != nil {
		t.Fatal(err)
	}
	return r, p
}

func projectTasks(t *testing.T, r *runner.Runner, prefix string) []*task.Task {
	t.Helper()
	all, err := r.Store.List()
	if err != nil {
		t.Fatal(err)
	}
	var out []*task.Task
	for _, tk := range all {
		if strings.HasPrefix(tk.Stage, prefix) {
			out = append(out, tk)
		}
	}
	return out
}

func setTask(t *testing.T, r *runner.Runner, id string, status task.Status, created bool) {
	t.Helper()
	if _, err := r.Store.Update(id, func(tk *task.Task) error {
		tk.Status, tk.FinishedAt = status, time.Now()
		if tk.Worktree != nil {
			tk.Worktree.BranchCreated = &created
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMilestoneStartSpawnsTheFirstRunner(t *testing.T) {
	r, p := approvedRunner(t)
	if _, err := runStage(t.Context(), r, stageRun{Project: "demo-app", Stage: "m0", Cwd: p.Path}); err == nil ||
		!strings.Contains(err.Error(), "milestone start|chain") {
		t.Fatalf("project run m0: %v", err)
	}
	var out bytes.Buffer
	if err := startMilestone(t.Context(), &out, r, p, "M1", milestoneRun{}); err == nil || !strings.Contains(err.Error(), "starts after M0") {
		t.Fatalf("M1 first: %v", err)
	}
	if err := startMilestone(t.Context(), &out, r, p, "M0", milestoneRun{}); err != nil {
		t.Fatal(err)
	}
	m0 := projectTasks(t, r, "M0/")
	if len(m0) != 2 {
		t.Fatalf("M0 tasks = %v", m0)
	}
	s, closer := m0[0], m0[1]
	if closer.Stage != "M0/close" || closer.Worktree == nil || closer.Worktree.Branch != "release/M0" || closer.Worktree.Base != "feat/M0-scaffold" ||
		!slices.Equal(closer.After, []string{s.ID}) || closer.Status != task.Queued {
		t.Errorf("M0 close task = %+v %+v", closer, closer.Worktree)
	}
	for _, want := range []string{"/ak:docs update", "/ak:retro 20", "events --learning --project demo-app", project.ReleaseNotesPath("M0"), "Do not tag, merge, push"} {
		if !strings.Contains(closer.Prompt, want) {
			t.Errorf("close prompt lacks %q:\n%s", want, closer.Prompt)
		}
	}
	if s.Stage != "M0/scaffold" || s.Kind != "flow" || s.Worktree == nil || s.Worktree.Branch != "feat/M0-scaffold" ||
		s.Worktree.Base != "main" || s.Owner != task.ProjectOwner("demo-app") {
		t.Errorf("scaffold task = %+v %+v", s, s.Worktree)
	}
	for _, want := range []string{"/ak:cook", "/ak:docs init", "@AGENTS.md"} {
		if !strings.Contains(s.Prompt, want) {
			t.Errorf("scaffold prompt lacks %q:\n%s", want, s.Prompt)
		}
	}
	if !strings.Contains(out.String(), "M0 Scaffold is in progress since ") {
		t.Errorf("output:\n%s", out.String())
	}
	if err := startMilestone(t.Context(), &out, r, p, "M0", milestoneRun{}); err == nil || !strings.Contains(err.Error(), "is starting") {
		t.Fatalf("start twice: %v", err)
	}

	// A failed scaffold that created its branch runs again on a new one,
	// and so does the close runner that failed with it.
	gitIn(t, p.Path, "branch", "feat/M0-scaffold")
	setTask(t, r, s.ID, task.Failed, true)
	setTask(t, r, closer.ID, task.Failed, false)
	out.Reset()
	if err := startMilestone(t.Context(), &out, r, p, "M0", milestoneRun{}); err != nil {
		t.Fatal(err)
	}
	m0 = projectTasks(t, r, "M0/")
	if len(m0) != 4 {
		t.Fatalf("M0 tasks after the run again = %v", m0)
	}
	again := m0[2]
	if again.Worktree.Branch != "feat/M0-scaffold-r1" || again.Worktree.Base != "feat/M0-scaffold" {
		t.Errorf("run again = %+v", again.Worktree)
	}
	if c := m0[3]; c.Stage != "M0/close" || c.Worktree.Branch != "release/M0" || c.Worktree.Base != "feat/M0-scaffold-r1" || !slices.Equal(c.After, []string{again.ID}) {
		t.Errorf("close again = %+v %+v", c, c.Worktree)
	}
	if !strings.Contains(out.String(), "runs m0 again after "+s.ID+" failed") {
		t.Errorf("output:\n%s", out.String())
	}

	// M1 starts with its plan runner in the main checkout.
	if _, err := project.Update(r.Store, "demo-app", func(rec *project.Record) error {
		rec.Milestones[0].Status = project.MilestoneDone
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := startMilestone(t.Context(), &out, r, p, "M1", milestoneRun{}); err != nil {
		t.Fatal(err)
	}
	plan := projectTasks(t, r, "M1/")
	if len(plan) != 1 || plan[0].Stage != "M1/plan" || plan[0].Kind != "research" || plan[0].Worktree != nil || !plan[0].Keep {
		t.Fatalf("plan task = %+v", plan)
	}
	if !strings.Contains(plan[0].Prompt, "--parallel --tdd --skip-journal") || !strings.Contains(plan[0].Prompt, "- F1 Add a list") {
		t.Errorf("plan prompt:\n%s", plan[0].Prompt)
	}
}

func TestMilestoneChain(t *testing.T) {
	r, p := approvedRunner(t)
	if _, err := project.Update(r.Store, "demo-app", func(rec *project.Record) error {
		rec.Milestones[0].Status = project.MilestoneDone
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := project.AddBacklog(r.Store, "demo-app", "dark mode", time.Now()); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := chainMilestone(t.Context(), &out, r, p, "M1", milestoneRun{}); err == nil || !strings.Contains(err.Error(), "start it first") {
		t.Fatalf("chain before start: %v", err)
	}
	if err := startMilestone(t.Context(), &out, r, p, "M1", milestoneRun{}); err != nil {
		t.Fatal(err)
	}
	planTask := projectTasks(t, r, "M1/plan")[0]
	if err := chainMilestone(t.Context(), &out, r, p, "M1", milestoneRun{}); err == nil || !strings.Contains(err.Error(), "is starting, not done") {
		t.Fatalf("chain before the plan is done: %v", err)
	}
	setTask(t, r, planTask.ID, task.Done, false)
	if err := chainMilestone(t.Context(), &out, r, p, "M1", milestoneRun{}); err == nil || !strings.Contains(err.Error(), "holds no "+project.MilestonePlanPath("M1")) {
		t.Fatalf("chain without a committed plan: %v", err)
	}
	plan := func(answer string) map[string]string {
		return map[string]string{project.MilestonePlanPath("M1"): "# M1 plan\n\n```yaml\nnodes:\n" +
			"  - {id: F1, type: feature, title: Contract}\n" +
			"  - {id: F2, type: feature, title: List, dependsOn: [F1]}\n" +
			"  - {id: F3, type: feature, title: Settings}\n" +
			"openQuestions:\n  - {q: \"Paging?\", answer: \"" + answer + "\"}\n" +
			"backlog:\n  - {id: B1, to: F3}\n```\n"}
	}
	commitFiles(t, p.Path, plan(""))
	if err := chainMilestone(t.Context(), &out, r, p, "M1", milestoneRun{}); err == nil || !strings.Contains(err.Error(), "1 open question(s) unanswered:\n- Paging?") {
		t.Fatalf("chain with an unanswered question: %v", err)
	}
	if n := len(projectTasks(t, r, "M1/")); n != 1 {
		t.Fatalf("a refused chain spawned %d tasks", n-1)
	}
	commitFiles(t, p.Path, plan("paging"))
	out.Reset()
	if err := chainMilestone(t.Context(), &out, r, p, "M1", milestoneRun{}); err != nil {
		t.Fatal(err)
	}
	nodes := projectTasks(t, r, "M1/")[1:]
	byStage := map[string]*task.Task{}
	for _, n := range nodes {
		byStage[n.Stage] = n
	}
	f1, f2, f3, in, cl := byStage["M1/F1"], byStage["M1/F2"], byStage["M1/F3"], byStage["M1/integrate"], byStage["M1/close"]
	if len(nodes) != 5 || f1 == nil || f2 == nil || f3 == nil || in == nil || cl == nil {
		t.Fatalf("nodes = %v", nodes)
	}
	if got := []string{nodes[0].Stage, nodes[1].Stage, nodes[2].Stage, nodes[3].Stage, nodes[4].Stage}; strings.Join(got, ",") != "M1/F1,M1/F2,M1/F3,M1/integrate,M1/close" {
		t.Errorf("spawn order = %v", got)
	}
	for _, c := range []struct {
		tk           *task.Task
		branch, base string
		after        []string
	}{
		{f1, "feat/M1-F1-contract", "main", nil},
		{f2, "feat/M1-F2-list", "", []string{f1.ID}},
		{f3, "feat/M1-F3-settings", "main", nil},
		{in, "integrate/M1-integrate", "main", []string{f2.ID, f3.ID}},
		{cl, "release/M1", "integrate/M1-integrate", []string{in.ID}},
	} {
		if c.tk.Worktree == nil || c.tk.Worktree.Branch != c.branch || c.tk.Worktree.Base != c.base || !slices.Equal(c.tk.After, c.after) || c.tk.Kind != "flow" {
			t.Errorf("%s = %+v after %v", c.tk.Stage, c.tk.Worktree, c.tk.After)
		}
	}
	if !strings.Contains(f2.Prompt, `/ak:cook "node F2 of handbook/product/milestones/M1/plan.md`) || !strings.Contains(in.Prompt, "git merge --no-edit") {
		t.Errorf("prompts:\n%s\n%s", f2.Prompt, in.Prompt)
	}
	for _, want := range []string{"backlog: 1 item(s) planned into M1", "added M1/integrate", "worktree feat/M1-F1-contract"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("chain output lacks %q:\n%s", want, out.String())
		}
	}
	rec, err := project.Load(r.Store, "demo-app")
	if err != nil {
		t.Fatal(err)
	}
	if b := rec.Backlog[0]; b.Status != project.BacklogPlanned || b.To != "M1/F3" {
		t.Errorf("backlog = %+v", b)
	}

	// While every node is unfinished, running the chain again keeps them all.
	out.Reset()
	if err := chainMilestone(t.Context(), &out, r, p, "M1", milestoneRun{}); err != nil {
		t.Fatalf("chain while nodes run: %v", err)
	}
	if n := len(projectTasks(t, r, "M1/")); n != 6 || !strings.Contains(out.String(), "no node of M1 failed; nothing to run again") {
		t.Fatalf("chain while nodes run spawned %d tasks:\n%s", n-6, out.String())
	}

	// F1 is done; F2 failed after it created its branch; F3 and the
	// integrate node failed with it.
	gitIn(t, p.Path, "branch", "feat/M1-F1-contract")
	gitIn(t, p.Path, "branch", "feat/M1-F2-list")
	setTask(t, r, f1.ID, task.Done, true)
	setTask(t, r, f2.ID, task.Failed, true)
	setTask(t, r, f3.ID, task.Failed, false)
	setTask(t, r, in.ID, task.Failed, false)
	setTask(t, r, cl.ID, task.Failed, false)

	// A branch no runner of the node created stops the chain before any spawn.
	gitIn(t, p.Path, "branch", "feat/M1-F3-settings")
	before := len(projectTasks(t, r, "M1/"))
	if err := chainMilestone(t.Context(), &out, r, p, "M1", milestoneRun{}); err == nil || !strings.Contains(err.Error(), "node F3: branch feat/M1-F3-settings already exists") {
		t.Fatalf("foreign branch: %v", err)
	}
	if n := len(projectTasks(t, r, "M1/")); n != before {
		t.Fatalf("a refused chain spawned %d tasks", n-before)
	}
	gitIn(t, p.Path, "branch", "-D", "feat/M1-F3-settings")

	out.Reset()
	if err := chainMilestone(t.Context(), &out, r, p, "M1", milestoneRun{}); err != nil {
		t.Fatal(err)
	}
	again := projectTasks(t, r, "M1/")[before:]
	if len(again) != 4 {
		t.Fatalf("run again spawned %v", again)
	}
	if again[0].Stage != "M1/F2" || again[0].Worktree.Branch != "feat/M1-F2-list-r1" || again[0].Worktree.Base != "feat/M1-F2-list" ||
		!slices.Equal(again[0].After, []string{f1.ID}) || again[0].Status == task.Queued {
		t.Errorf("F2 again = %+v %+v", again[0], again[0].Worktree)
	}
	if again[1].Stage != "M1/F3" || again[1].Worktree.Branch != "feat/M1-F3-settings" || again[1].Worktree.Base != "main" {
		t.Errorf("F3 again = %+v", again[1].Worktree)
	}
	if again[2].Stage != "M1/integrate" || !slices.Equal(again[2].After, []string{again[0].ID, again[1].ID}) {
		t.Errorf("integrate again = %+v", again[2])
	}
	if again[3].Stage != "M1/close" || again[3].Worktree.Branch != "release/M1" || !slices.Equal(again[3].After, []string{again[2].ID}) {
		t.Errorf("close again = %+v %+v", again[3], again[3].Worktree)
	}
	if !strings.Contains(out.String(), "kept M1/F1: "+f1.ID+" is done") {
		t.Errorf("run again output:\n%s", out.String())
	}

	// F3 failed to launch while F2 runs: F3 runs again next to F2, and the
	// queued integrate node waits for the new F3 instead of the failed one.
	f2r, f3r, inr, clr := again[0], again[1], again[2], again[3]
	setTask(t, r, f2r.ID, task.Running, true)
	setTask(t, r, f3r.ID, task.Failed, false)
	out.Reset()
	if err := chainMilestone(t.Context(), &out, r, p, "M1", milestoneRun{}); err != nil {
		t.Fatalf("chain with a failed node next to a running one: %v", err)
	}
	third := projectTasks(t, r, "M1/")[before+len(again):]
	if len(third) != 1 || third[0].Stage != "M1/F3" || third[0].Worktree.Branch != "feat/M1-F3-settings" || third[0].Worktree.Base != "main" {
		t.Fatalf("respawned %+v", third)
	}
	for _, c := range []struct {
		id     string
		status task.Status
		after  []string
	}{
		{f2r.ID, task.Running, []string{f1.ID}},
		{inr.ID, task.Queued, []string{f2r.ID, third[0].ID}},
		{clr.ID, task.Queued, []string{inr.ID}},
	} {
		if tk, err := r.Store.Get(c.id); err != nil || tk.Status != c.status || !slices.Equal(tk.After, c.after) {
			t.Errorf("%s = %v after %v (%v), want %s after %v", c.id, tk.Status, tk.After, err, c.status, c.after)
		}
	}
	for _, want := range []string{
		"kept M1/F2: " + f2r.ID + " is running",
		"kept M1/integrate: " + inr.ID + " is queued, now after " + f2r.ID + ", " + third[0].ID,
		"kept M1/close: " + clr.ID + " is queued",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("respawn output lacks %q:\n%s", want, out.String())
		}
	}

	// The integrate node created its branch, then failed while the close
	// runner stayed queued: the integrate node runs again on -r1, and the
	// close runner starts from that branch instead of the old one.
	gitIn(t, p.Path, "branch", "integrate/M1-integrate")
	setTask(t, r, inr.ID, task.Failed, true)
	out.Reset()
	if err := chainMilestone(t.Context(), &out, r, p, "M1", milestoneRun{}); err != nil {
		t.Fatalf("chain with a failed integrate node: %v", err)
	}
	all := projectTasks(t, r, "M1/")
	in2 := all[len(all)-1]
	if in2.Stage != "M1/integrate" || in2.Worktree.Branch != "integrate/M1-integrate-r1" || in2.Worktree.Base != "integrate/M1-integrate" {
		t.Fatalf("integrate again = %+v %+v", in2, in2.Worktree)
	}
	if tk, err := r.Store.Get(clr.ID); err != nil || tk.Status != task.Queued || !slices.Equal(tk.After, []string{in2.ID}) || tk.Worktree.Base != "integrate/M1-integrate-r1" {
		t.Errorf("close = %+v after %v (%v)", tk.Worktree, tk.After, err)
	}
	if want := "kept M1/close: " + clr.ID + " is queued, now after " + in2.ID + ", from integrate/M1-integrate-r1"; !strings.Contains(out.String(), want) {
		t.Errorf("output lacks %q:\n%s", want, out.String())
	}

	// A chain run that stopped before the relink leaves the close runner
	// waiting for the failed runner after the new one finished: the next
	// chain points it at the done runner and starts it, since no finish
	// will.
	var started []string
	r.StartAfter = func(id string) error { started = append(started, id); return nil }
	if _, err := r.Store.Update(clr.ID, func(tk *task.Task) error { tk.After = []string{inr.ID}; return nil }); err != nil {
		t.Fatal(err)
	}
	setTask(t, r, in2.ID, task.Done, true)
	out.Reset()
	if err := chainMilestone(t.Context(), &out, r, p, "M1", milestoneRun{}); err != nil {
		t.Fatal(err)
	}
	if tk, err := r.Store.Get(clr.ID); err != nil || !slices.Equal(tk.After, []string{in2.ID}) || !slices.Equal(started, []string{in2.ID}) ||
		!strings.Contains(out.String(), "its parents are done, so it starts") {
		t.Errorf("close after a done parent = %v, started %v (%v):\n%s", tk.After, started, err, out.String())
	}
}

// lanePlan is a milestone plan with a backend lane (F8, F9, then F10 and
// F11) beside a UI lane (F33, then U1 and F7), both after C2. F9 waits for
// F8 unless f9Root.
func lanePlan(f9Root bool) map[string]string {
	f9 := "  - {id: F9, type: feature, title: Sync jobs, dependsOn: [F8]}\n"
	if f9Root {
		f9 = "  - {id: F9, type: feature, title: Sync jobs}\n"
	}
	return map[string]string{project.MilestonePlanPath("M1"): "# M1 plan\n\n```yaml\nnodes:\n" +
		"  - {id: C2, type: feature, title: Core}\n" +
		"  - {id: F33, type: feature, title: Shell, dependsOn: [C2]}\n" +
		"  - {id: F8, type: feature, title: Sync core, dependsOn: [C2]}\n" +
		"  - {id: U1, type: feature, title: Board, dependsOn: [F33]}\n" +
		"  - {id: F7, type: feature, title: Import, dependsOn: [F33]}\n" +
		f9 +
		"  - {id: F10, type: feature, title: Retry, dependsOn: [F9]}\n" +
		"  - {id: F11, type: feature, title: Metrics, dependsOn: [F9]}\n" +
		"  - {id: I2, type: integrate, title: Integrate, dependsOn: [F10, F11, U1, F7]}\n```\n"}
}

// laneChain chains lanePlan, then finishes C2 and F33, fails F8 before it
// created its branch, and sets U1 to u1 and F7 to running, while every node
// after them stays queued. It returns the node tasks by stage.
func laneChain(t *testing.T, u1 task.Status) (*runner.Runner, state.Project, map[string]*task.Task) {
	t.Helper()
	r, p := approvedRunner(t)
	if _, err := project.Update(r.Store, "demo-app", func(rec *project.Record) error {
		rec.Milestones[0].Status = project.MilestoneDone
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := startMilestone(t.Context(), &out, r, p, "M1", milestoneRun{}); err != nil {
		t.Fatal(err)
	}
	setTask(t, r, projectTasks(t, r, "M1/plan")[0].ID, task.Done, false)
	commitFiles(t, p.Path, lanePlan(false))
	if err := chainMilestone(t.Context(), &out, r, p, "M1", milestoneRun{}); err != nil {
		t.Fatal(err)
	}
	byStage := map[string]*task.Task{}
	for _, tk := range projectTasks(t, r, "M1/")[1:] {
		byStage[tk.Stage] = tk
	}
	if len(byStage) != 10 {
		t.Fatalf("chain spawned %v", byStage)
	}
	for stage, status := range map[string]task.Status{"M1/C2": task.Done, "M1/F33": task.Done, "M1/U1": u1, "M1/F7": task.Running} {
		gitIn(t, p.Path, "branch", byStage[stage].Worktree.Branch)
		setTask(t, r, byStage[stage].ID, status, true)
	}
	setTask(t, r, byStage["M1/F8"].ID, task.Failed, false)
	return r, p, byStage
}

func TestChainRespawnsAFailedNodeWhileSiblingsRun(t *testing.T) {
	for _, u1 := range []task.Status{task.Starting, task.Running} {
		t.Run(string(u1), func(t *testing.T) {
			r, p, n := laneChain(t, u1)
			before := len(projectTasks(t, r, "M1/"))
			var out bytes.Buffer
			if err := chainMilestone(t.Context(), &out, r, p, "M1", milestoneRun{}); err != nil {
				t.Fatalf("chain while U1 is %s and F7 runs: %v", u1, err)
			}
			again := projectTasks(t, r, "M1/")[before:]
			if len(again) != 1 || again[0].Stage != "M1/F8" {
				t.Fatalf("respawned %v", again)
			}
			f8 := again[0]
			if f8.Worktree.Branch != n["M1/F8"].Worktree.Branch || f8.Worktree.Base != n["M1/C2"].Worktree.Branch ||
				!slices.Equal(f8.After, []string{n["M1/C2"].ID}) || f8.Status != task.Starting {
				t.Errorf("F8 again = %s after %v, %+v", f8.Status, f8.After, f8.Worktree)
			}
			for _, c := range []struct {
				stage, base string
				status      task.Status
				after       []string
			}{
				{"M1/U1", "", u1, []string{n["M1/F33"].ID}},
				{"M1/F7", "", task.Running, []string{n["M1/F33"].ID}},
				{"M1/F9", "", task.Queued, []string{f8.ID}},
				{"M1/F10", "", task.Queued, []string{n["M1/F9"].ID}},
				{"M1/F11", "", task.Queued, []string{n["M1/F9"].ID}},
				{"M1/I2", "main", task.Queued, []string{n["M1/F10"].ID, n["M1/F11"].ID, n["M1/U1"].ID, n["M1/F7"].ID}},
				{"M1/close", n["M1/I2"].Worktree.Branch, task.Queued, []string{n["M1/I2"].ID}},
			} {
				tk, err := r.Store.Get(n[c.stage].ID)
				if err != nil || tk.Status != c.status || !slices.Equal(tk.After, c.after) || tk.Worktree.Base != c.base {
					t.Errorf("%s = %s after %v from %q (%v), want %s after %v from %q", c.stage, tk.Status, tk.After, tk.Worktree.Base, err, c.status, c.after, c.base)
				}
			}
			for _, want := range []string{
				"kept M1/U1: " + n["M1/U1"].ID + " is " + string(u1),
				"kept M1/F7: " + n["M1/F7"].ID + " is running",
				"kept M1/F9: " + n["M1/F9"].ID + " is queued, now after " + f8.ID,
				"kept M1/close: " + n["M1/close"].ID + " is queued",
			} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output lacks %q:\n%s", want, out.String())
				}
			}
		})
	}
}

func TestChainStartsAQueuedNodeNoParentWillStart(t *testing.T) {
	r, p, n := laneChain(t, task.Running)
	var out bytes.Buffer
	if err := chainMilestone(t.Context(), &out, r, p, "M1", milestoneRun{}); err != nil {
		t.Fatal(err)
	}
	all := projectTasks(t, r, "M1/")
	f8, f9 := all[len(all)-1], n["M1/F9"]

	// A chain run stopped before it relinked F9, and the new F8 finishes
	// while the next run respawns U1, after that run read the tasks: F9 is
	// relinked to a parent that is done already, so the chain starts it.
	if _, err := r.Store.Update(f9.ID, func(tk *task.Task) error { tk.After = []string{n["M1/F8"].ID}; return nil }); err != nil {
		t.Fatal(err)
	}
	gitIn(t, p.Path, "branch", "-D", n["M1/U1"].Worktree.Branch)
	setTask(t, r, n["M1/U1"].ID, task.Failed, false)
	r.StartLauncher = func(string) (int, error) {
		setTask(t, r, f8.ID, task.Done, true)
		return 4242, nil
	}
	var started []string
	r.StartAfter = func(id string) error { started = append(started, id); return nil }
	out.Reset()
	if err := chainMilestone(t.Context(), &out, r, p, "M1", milestoneRun{}); err != nil {
		t.Fatal(err)
	}
	want := "kept M1/F9: " + f9.ID + " is queued, now after " + f8.ID + "; its parents are done, so it starts"
	if !slices.Equal(started, []string{f8.ID}) || !strings.Contains(out.String(), want) {
		t.Errorf("started %v, output lacks %q:\n%s", started, want, out.String())
	}

	// F9 still waits, since that start did not reach it: the next chain,
	// with nothing to relink, starts it again.
	r.StartLauncher = func(string) (int, error) { return 4242, nil }
	out.Reset()
	if err := chainMilestone(t.Context(), &out, r, p, "M1", milestoneRun{}); err != nil {
		t.Fatal(err)
	}
	want = "kept M1/F9: " + f9.ID + " is queued; its parents are done, so it starts"
	if !slices.Equal(started, []string{f8.ID, f8.ID}) || !strings.Contains(out.String(), want) {
		t.Errorf("started %v, output lacks %q:\n%s", started, want, out.String())
	}

	// The plan no longer has F9 wait for F8: F9 is relinked to no task and
	// starts from the default branch on its own.
	commitFiles(t, p.Path, lanePlan(true))
	out.Reset()
	if err := chainMilestone(t.Context(), &out, r, p, "M1", milestoneRun{}); err != nil {
		t.Fatalf("chain after F9 lost its parent: %v", err)
	}
	if tk, err := r.Store.Get(f9.ID); err != nil || tk.Status != task.Starting || len(tk.After) != 0 || tk.Worktree.Base != "main" {
		t.Errorf("F9 = %s after %v from %q (%v)", tk.Status, tk.After, tk.Worktree.Base, err)
	}
	if want := "kept M1/F9: " + f9.ID + " is queued, now after no task, from main; it waits for no task, so it starts"; !strings.Contains(out.String(), want) {
		t.Errorf("output lacks %q:\n%s", want, out.String())
	}
}

// commitOnBranch commits files on a new branch made from base, and goes
// back to main.
func commitOnBranch(t *testing.T, dir, branch, base string, files map[string]string) {
	t.Helper()
	gitIn(t, dir, "checkout", "-q", "-b", branch, base)
	commitFiles(t, dir, files)
	gitIn(t, dir, "checkout", "-q", "main")
}

func TestMilestoneDoneReopenAndClose(t *testing.T) {
	r, p := approvedRunner(t)
	var out bytes.Buffer
	if err := startMilestone(t.Context(), &out, r, p, "M0", milestoneRun{}); err != nil {
		t.Fatal(err)
	}
	m0 := projectTasks(t, r, "M0/")
	scaffold, closer := m0[0], m0[1]
	commitOnBranch(t, p.Path, "feat/M0-scaffold", "main", map[string]string{"app.txt": "app\n"})
	commitOnBranch(t, p.Path, "release/M0", "feat/M0-scaffold", map[string]string{project.ReleaseNotesPath("M0"): "# M0\n"})
	setTask(t, r, scaffold.ID, task.Done, true)
	setTask(t, r, closer.ID, task.Done, true)
	var warn []string
	o := milestoneRun{Warn: func(line string) { warn = append(warn, line) }}

	if err := finishMilestone(t.Context(), &out, r, p, "M0", "", o); err == nil || !strings.Contains(err.Error(), "release/M0 is not merged into main") {
		t.Fatalf("done before the merge: %v", err)
	}
	gitIn(t, p.Path, "merge", "-q", "--no-ff", "--no-edit", "release/M0")
	out.Reset()
	if err := finishMilestone(t.Context(), &out, r, p, "M0", "", o); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"M0 Scaffold is done: release/M0 is merged into main", "tag: v0.0.0 on ", "local only"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("done output lacks %q:\n%s", want, out.String())
		}
	}
	if gitIn(t, p.Path, "tag", "-l") != "v0.0.0" || len(warn) != 0 {
		t.Errorf("tags = %q, warnings %v", gitIn(t, p.Path, "tag", "-l"), warn)
	}

	if err := recloseMilestone(t.Context(), &out, r, p, "M0", o); err == nil || !strings.Contains(err.Error(), "only a milestone in progress") {
		t.Fatalf("close a done milestone: %v", err)
	}
	out.Reset()
	if err := reopenMilestone(t.Context(), &out, r, p, "M0", "the app does not start"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"M0 Scaffold is in progress again", "deleted the local tag v0.0.0", "revert that merge through a pull request", "milestone close M0 --project demo-app"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("reopen output lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if err := recloseMilestone(t.Context(), &out, r, p, "M0", o); err != nil {
		t.Fatal(err)
	}
	again := projectTasks(t, r, "M0/close")
	c := again[len(again)-1]
	if len(again) != 2 || c.Worktree == nil || c.Worktree.Branch != "release/M0-r1" || c.Worktree.Base != "feat/M0-scaffold" ||
		!slices.Equal(c.After, []string{scaffold.ID}) || c.Status == task.Queued || !strings.Contains(c.Prompt, "/ak:retro") {
		t.Errorf("close again = %+v %+v", c, c.Worktree)
	}
	if err := recloseMilestone(t.Context(), &out, r, p, "M0", o); err == nil || !strings.Contains(err.Error(), "of M0/close is starting") {
		t.Fatalf("close twice: %v", err)
	}
}

func TestParallelNoteAfterProjectSpawns(t *testing.T) {
	r, _ := approvedRunner(t)
	var notes []string
	warn := func(line string) { notes = append(notes, line) }
	spawn := func(after ...string) *task.Task {
		t.Helper()
		tk, err := r.Spawn(t.Context(), runner.SpawnOptions{Kind: "research", Project: "demo-app", Prompt: "check " + strconv.Itoa(len(notes)), After: after})
		if err != nil {
			t.Fatal(err)
		}
		parallelNote(r.Store, "demo-app", warn)
		return tk
	}
	first := spawn()
	spawn()
	spawn()
	if q := spawn(first.ID); q.Status != task.Queued || len(notes) != 0 {
		t.Fatalf("three active and one queued: %s, notes %v", q.Status, notes)
	}
	spawn()
	if len(notes) != 1 || notes[0] != "note: 4 runners of demo-app are active (warning above 3); nothing was blocked" {
		t.Errorf("notes = %v", notes)
	}
}

func TestMilestoneStartOnALiteProjectWithThePackWaived(t *testing.T) {
	r, dir := stageRunner(t)
	gitIn(t, dir, "branch", "-M", "main")
	commitFiles(t, dir, map[string]string{project.SpecPath: "---\nversion: 1\ntype: cli\ntrack: lite\n---\n# Spec\n"})
	p, err := r.Store.ProjectByName("demo-app")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := project.Decide(t.Context(), r.Store, "demo-app", project.DecideOptions{
		Gate: project.GateSpec, Outcome: "approve", By: "Lan", WaivePack: true,
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := startMilestone(t.Context(), &out, r, p, "M0", milestoneRun{}); err != nil {
		t.Fatal(err)
	}
	if m0 := projectTasks(t, r, "M0/"); len(m0) != 2 || m0[0].Stage != "M0/scaffold" {
		t.Fatalf("M0 tasks = %v", m0)
	}
	if strings.Contains(out.String(), "imported the roadmap") {
		t.Errorf("output without a proposal:\n%s", out.String())
	}
	if _, err := project.Update(r.Store, "demo-app", func(rec *project.Record) error {
		rec.Milestones[0].Status = project.MilestoneDone
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := startMilestone(t.Context(), &out, r, p, "M1", milestoneRun{}); err == nil || !strings.Contains(err.Error(), "project run roadmap") {
		t.Fatalf("M1 without a roadmap: %v", err)
	}
	commitFiles(t, dir, map[string]string{project.RoadmapProposalPath: roadmapProposal})
	out.Reset()
	if err := startMilestone(t.Context(), &out, r, p, "M1", milestoneRun{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "imported the roadmap from "+project.RoadmapProposalPath+" (2 milestones)") {
		t.Errorf("output:\n%s", out.String())
	}
	if plan := projectTasks(t, r, "M1/"); len(plan) != 1 || !strings.Contains(plan[0].Prompt, "- F1 Add a list") {
		t.Fatalf("plan task = %+v", plan)
	}
}

func TestSpawnProjectTaskPrintsItsWriteRule(t *testing.T) {
	r, _ := approvedRunner(t)
	r.StartLauncher = func(string) (int, error) { return 4242, nil }
	for kind, want := range map[string]string{
		"flow":     "may change code (kind flow)",
		"fix":      "may change code (kind fix)",
		"research": "may write only the handbook files its task names (kind research)",
	} {
		var out, errOut bytes.Buffer
		o := runner.SpawnOptions{Kind: kind, Project: "demo-app", Prompt: "do " + kind, Warn: func(string) {}}
		if err := spawnTask(t.Context(), r, o, nil, &out, &errOut); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("kind %s: notes %q, want %q", kind, errOut.String(), want)
		}
	}
}
