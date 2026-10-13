package cli

import (
	"bytes"
	"slices"
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
	if len(m0) != 1 {
		t.Fatalf("M0 tasks = %v", m0)
	}
	s := m0[0]
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

	// A failed scaffold that created its branch runs again on a new one.
	gitIn(t, p.Path, "branch", "feat/M0-scaffold")
	setTask(t, r, s.ID, task.Failed, true)
	out.Reset()
	if err := startMilestone(t.Context(), &out, r, p, "M0", milestoneRun{}); err != nil {
		t.Fatal(err)
	}
	again := projectTasks(t, r, "M0/")[1]
	if again.Worktree.Branch != "feat/M0-scaffold-r1" || again.Worktree.Base != "feat/M0-scaffold" {
		t.Errorf("run again = %+v", again.Worktree)
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
	f1, f2, f3, in := byStage["M1/F1"], byStage["M1/F2"], byStage["M1/F3"], byStage["M1/integrate"]
	if len(nodes) != 4 || f1 == nil || f2 == nil || f3 == nil || in == nil {
		t.Fatalf("nodes = %v", nodes)
	}
	if got := []string{nodes[0].Stage, nodes[1].Stage, nodes[2].Stage, nodes[3].Stage}; strings.Join(got, ",") != "M1/F1,M1/F2,M1/F3,M1/integrate" {
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
		{in, "feat/M1-integrate-integrate-every-feature", "main", []string{f2.ID, f3.ID}},
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

	// While a node runs, the chain is refused.
	if err := chainMilestone(t.Context(), &out, r, p, "M1", milestoneRun{}); err == nil || !strings.Contains(err.Error(), "before chaining again") {
		t.Fatalf("chain while nodes run: %v", err)
	}

	// F1 is done; F2 failed after it created its branch; F3 and the
	// integrate node failed with it.
	gitIn(t, p.Path, "branch", "feat/M1-F1-contract")
	gitIn(t, p.Path, "branch", "feat/M1-F2-list")
	setTask(t, r, f1.ID, task.Done, true)
	setTask(t, r, f2.ID, task.Failed, true)
	setTask(t, r, f3.ID, task.Failed, false)
	setTask(t, r, in.ID, task.Failed, false)

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
	if len(again) != 3 {
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
	if !strings.Contains(out.String(), "kept M1/F1: "+f1.ID+" is done") {
		t.Errorf("run again output:\n%s", out.String())
	}
}
