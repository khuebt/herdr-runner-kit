package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/planrun"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func headOf(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// planTask records a done phase task of a one-phase plan whose scope is
// src/, working in repo since its current HEAD.
func planTask(t *testing.T, r *Runner, repo string, phaseBody string) (*task.Task, string) {
	t.Helper()
	planDir := filepath.Join(t.TempDir(), "plan")
	writeFiles(t, planDir, map[string]string{
		"plan.md":       "---\ntitle: P\nstatus: in-progress\n---\n",
		"phase-01-a.md": phaseBody,
	})
	tk := &task.Task{
		Kind: "flow", Agent: "claude", Prompt: "x", Cwd: repo, RepoRoot: repo, Owner: repo,
		Plan: planDir, Phase: 1, PhaseStart: headOf(t, repo), Status: task.Done,
	}
	if err := r.Store.Create(tk); err != nil {
		t.Fatal(err)
	}
	return tk, planDir
}

const srcPhase = "---\nphase: 1\nstatus: pending\n---\n# Phase 1\n\n## Files\n\n- `src/`\n"

func TestAcceptPhaseVerdicts(t *testing.T) {
	cases := []struct {
		name    string
		work    func(t *testing.T, repo string)
		verdict string
		clean   bool
	}{
		{"accepted", func(t *testing.T, repo string) {
			// A non-ASCII name stays in scope, and committed plan scratch
			// is in no one's way.
			writeFiles(t, repo, map[string]string{"src/a.go": "a", "src/ghi-chú.md": "b", "plans/reports/r.md": "r"})
			git(t, repo, "add", "-A")
			git(t, repo, "commit", "-q", "-m", "a")
		}, task.PhaseAccepted, true},
		{"outside", func(t *testing.T, repo string) {
			writeFiles(t, repo, map[string]string{"src/a.go": "a", "main.go": "m"})
			git(t, repo, "add", "-A")
			git(t, repo, "commit", "-q", "-m", "a")
		}, task.PhaseOutside, true},
		{"dirty", func(t *testing.T, repo string) {
			writeFiles(t, repo, map[string]string{"src/a.go": "a", "plans/r.md": "scratch"})
		}, task.PhaseDirty, false},
		{"no changes", func(t *testing.T, repo string) {
			writeFiles(t, repo, map[string]string{"plans/reports/r.md": "scratch"})
		}, task.PhaseNoChanges, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _, _ := newTestRunner(t)
			repo := gitRepo(t)
			tk, planDir := planTask(t, r, repo, srcPhase)
			c.work(t, repo)
			r.acceptPhase(context.Background(), tk)
			got, _ := r.Store.Get(tk.ID)
			if got.PhaseCheck == nil || got.PhaseCheck.Verdict != c.verdict || got.PhaseCheck.Clean != c.clean {
				t.Fatalf("check = %+v", got.PhaseCheck)
			}
			if !slices.Contains(events(t, r, tk.ID), "phase-accepted") {
				t.Error("no phase-accepted event")
			}
			plan, _ := planrun.Read(planDir)
			if plan.Phases[0].Status != "completed" {
				t.Errorf("phase status = %q", plan.Phases[0].Status)
			}
		})
	}
}

func TestAcceptPhaseRunsOnceAndReportsUnknowns(t *testing.T) {
	r, _, _ := newTestRunner(t)
	repo := gitRepo(t)
	tk, _ := planTask(t, r, repo, "---\nphase: 1\n---\n# no status line\n")
	r.acceptPhase(context.Background(), tk)
	r.acceptPhase(context.Background(), tk)
	if ev := events(t, r, tk.ID); !slices.Equal(ev, []string{"phase-accepted", "plan-status-failed"}) {
		t.Errorf("events = %v", ev)
	}
	got, _ := r.Store.Get(tk.ID)
	if got.PhaseCheck.Verdict != task.PhaseNoChanges {
		t.Errorf("check = %+v", got.PhaseCheck)
	}

	tk2, _ := planTask(t, r, repo, srcPhase)
	if _, err := r.Store.Update(tk2.ID, func(t *task.Task) error {
		t.PhaseStart = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.acceptPhase(context.Background(), tk2)
	got, _ = r.Store.Get(tk2.ID)
	if got.PhaseCheck.Verdict != task.PhaseUnknown || got.PhaseCheck.Holds() {
		t.Errorf("an unknown start: %+v", got.PhaseCheck)
	}
}

func TestAcceptPhaseLeavesAnEarlierPhasesLeftoversToIt(t *testing.T) {
	r, _, _ := newTestRunner(t)
	repo := gitRepo(t)
	parent, _ := planTask(t, r, repo, srcPhase)
	writeFiles(t, repo, map[string]string{"src/gen.txt": "left"})
	r.acceptPhase(context.Background(), parent)
	got, _ := r.Store.Get(parent.ID)
	if got.PhaseCheck.Verdict != task.PhaseDirty || !slices.Equal(got.PhaseCheck.Dirty, []string{"src/gen.txt"}) {
		t.Fatalf("parent check = %+v", got.PhaseCheck)
	}

	child, _ := planTask(t, r, repo, srcPhase)
	if _, err := r.Store.Update(child.ID, func(t *task.Task) error {
		t.After = []string{parent.ID}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, repo, map[string]string{"src/b.go": "b"})
	git(t, repo, "add", "src/b.go")
	git(t, repo, "commit", "-q", "-m", "b")
	r.acceptPhase(context.Background(), child)
	c, _ := r.Store.Get(child.ID)
	if c.PhaseCheck.Verdict != task.PhaseAccepted || c.PhaseCheck.Clean || c.PhaseCheck.Holds() ||
		!strings.Contains(c.PhaseCheck.Note, "left by an earlier phase: src/gen.txt") || slices.Contains(c.PhaseCheck.Changed, "src/gen.txt") {
		t.Fatalf("child check = %+v", c.PhaseCheck)
	}
}

func TestPlanDoneWaitsForASiblingsCheck(t *testing.T) {
	r, _, _ := newTestRunner(t)
	repo := gitRepo(t)
	planDir := filepath.Join(t.TempDir(), "plan")
	writeFiles(t, planDir, map[string]string{
		"plan.md":       "---\ntitle: P\nstatus: in-progress\n---\n",
		"phase-01-a.md": "---\nphase: 1\nstatus: pending\ndependencies: []\n---\n## Files\n- `a/`\n",
		"phase-02-b.md": "---\nphase: 2\nstatus: pending\ndependencies: []\n---\n## Files\n- `b/`\n",
	})
	start := headOf(t, repo)
	var tasks []*task.Task
	for n := 1; n <= 2; n++ {
		tk := &task.Task{
			Kind: "flow", Agent: "claude", Prompt: "x", Cwd: repo, RepoRoot: repo, Owner: repo,
			Plan: planDir, Phase: n, PhaseStart: start, Status: task.Done, FinishedAt: time.Now(),
		}
		if err := r.Store.Create(tk); err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, tk)
	}
	writeFiles(t, repo, map[string]string{"a/x.go": "x", "b/y.go": "y", "z.go": "z"})
	git(t, repo, "add", "a/x.go")
	git(t, repo, "commit", "-q", "-m", "a")
	git(t, repo, "add", "b/y.go", "z.go")
	git(t, repo, "commit", "-q", "-m", "b")
	// Phase 1's follow-up checks it and finds the plan done while phase 2's
	// own follow-up has not checked phase 2 yet.
	r.acceptPhase(context.Background(), tasks[0])
	if err := r.planDone(context.Background(), tasks[0]); err != nil {
		t.Fatal(err)
	}
	all, _, _ := r.Store.ReadEventsSince(0)
	var done []string
	for _, ev := range all {
		if ev.Type == "plan-done" {
			done = append(done, ev.Summary)
		}
	}
	if len(done) != 1 || done[0] != "plan P: 2/2 phases done, 1 verified in scope" {
		t.Fatalf("plan-done = %q", done)
	}
	if c, _ := r.Store.Get(tasks[1].ID); c.PhaseCheck == nil || c.PhaseCheck.Verdict != task.PhaseOutside {
		t.Fatalf("phase 2 check = %+v", c.PhaseCheck)
	}
}

func TestPhaseSiblingsAreTheRunnersOfTheSameWave(t *testing.T) {
	now := time.Now()
	mk := func(id string, after []string, started bool, finished time.Time, created time.Time) *task.Task {
		tk := &task.Task{ID: id, Plan: "/p", After: after, FinishedAt: finished, CreatedAt: created}
		if started {
			tk.PhaseStart = "abc"
		}
		return tk
	}
	earlier := mk("t1", nil, true, now.Add(-time.Hour), now.Add(-2*time.Hour))
	anchor := mk("t2", nil, true, now.Add(-time.Minute), now.Add(-10*time.Minute))
	me := mk("t3", []string{"t2"}, true, time.Time{}, now.Add(-10*time.Minute))
	sib := mk("t4", []string{"t2"}, true, time.Time{}, now.Add(-10*time.Minute))
	next := mk("t5", []string{"t3", "t4"}, false, time.Time{}, now.Add(-10*time.Minute))
	other := &task.Task{ID: "t6", Plan: "/q", PhaseStart: "x", CreatedAt: now}
	got := phaseSiblings(me, []*task.Task{earlier, anchor, me, sib, next, other})
	if len(got) != 1 || got[0].ID != "t4" {
		t.Fatalf("siblings = %v", got)
	}
}

func TestCommitFiles(t *testing.T) {
	log := "\x00aaa\n\na.go\nb/c.go\n\x00bbb\n\n\x00ccc\n\nd.md\n"
	got := commitFiles(log)
	if len(got) != 3 || !slices.Equal(got[0], []string{"a.go", "b/c.go"}) || len(got[1]) != 0 || !slices.Equal(got[2], []string{"d.md"}) {
		t.Fatalf("commits = %q", got)
	}
}

func TestPhasePrompt(t *testing.T) {
	plan := &planrun.Plan{File: "/p/plan.md"}
	ph := planrun.Phase{Number: 2, File: "/p/phase-02-x.md", Scope: []string{"a/", "b.go"}}
	got := PhasePrompt(plan, ph, nil, nil)
	for _, want := range []string{
		"/p/phase-02-x.md\n\nYou run phase 2 of plan /p/plan.md in this session; do only this phase.",
		"Its write scope: a/, b.go.",
		"Do not edit plan.md or other phase files; the kit records phase status.",
		"Commit your work on this branch before you report done.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Another runner") || strings.Contains(got, "earlier attempt") {
		t.Errorf("a lone first attempt gets no extra rules:\n%s", got)
	}
	sib := planrun.Phase{Number: 3, Scope: []string{"c/"}}
	never := &task.Task{ID: "t6", Status: task.Failed, Error: "parent t5 stopped"}
	if got := PhasePrompt(plan, ph, nil, never); strings.Contains(got, "earlier attempt") {
		t.Errorf("an attempt that never launched left no work:\n%s", got)
	}
	earlier := &task.Task{ID: "t7", Status: task.Stopped, PhaseStart: "abc"}
	got = PhasePrompt(plan, planrun.Phase{Number: 2, File: "/p/x.md"}, []planrun.Phase{sib}, earlier)
	for _, want := range []string{
		"It declares no write scope",
		"Another runner works in this checkout at the same time on c/. Stage only your own paths",
		"An earlier attempt (t7) stopped. The checkout may hold its partial work",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q:\n%s", want, got)
		}
	}
}

func TestSharedCheckoutGoneFailsTheLaunch(t *testing.T) {
	r, _, _ := newTestRunner(t)
	repo := gitRepo(t)
	anchor := &task.Task{
		Kind: "flow", Agent: "claude", Prompt: "x", Cwd: repo, RepoRoot: repo, Owner: repo, Status: task.Done,
		Worktree: &task.Worktree{Branch: "b", Path: filepath.Join(repo, "gone"), BranchCreated: new(true)},
	}
	if err := r.Store.Create(anchor); err != nil {
		t.Fatal(err)
	}
	tk := spawnStarting(t, r, SpawnOptions{Kind: "flow", Prompt: "phase", Cwd: repo, CheckoutOf: anchor.ID, Plan: "/p", Phase: 2})
	if tk.UseWorktree || tk.CheckoutOf != anchor.ID {
		t.Fatalf("task = %+v", tk)
	}
	if err := r.Launch(context.Background(), tk.ID); err == nil || !strings.Contains(err.Error(), "the run's checkout "+filepath.Join(repo, "gone")+" is gone") {
		t.Fatalf("launch: %v", err)
	}
	if _, err := r.Spawn(context.Background(), SpawnOptions{Prompt: "x", Cwd: repo, CheckoutOf: anchor.ID, Branch: "x"}); err == nil {
		t.Error("--branch with a shared checkout must be refused")
	}
}

func TestPlanReportShowsUnitsAndTheirTotal(t *testing.T) {
	dir := t.TempDir()
	plan := &planrun.Plan{Title: "Demo", Dir: dir, File: filepath.Join(dir, "plan.md"), Phases: []planrun.Phase{
		{Number: 1, Title: "Claude phase"}, {Number: 2, Title: "Codex phase"}, {Number: 3, Title: "Not run", Status: "pending"},
	}}
	latest := map[int]*task.Task{
		1: {ID: "t1", Agent: "claude", Summary: "did one", Usage: &task.Usage{Calls: 40, MaxContext: 120_000, Units: 1_250_000}},
		2: {ID: "t2", Agent: "codex", Summary: "did two"},
	}
	path, _, err := writePlanReport(plan, latest, time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	report := string(data)
	for _, want := range []string{
		"| Phase | Task | Agent | Units | Claim | Verified |",
		"| 1 Claude phase | t1 | claude | 1.2M | did one |",
		"| 2 Codex phase | t2 | codex | - | did two |",
		"| 3 Not run | none | | | | status pending",
		"| Total | | | 1.2M | 40 calls, max context 120k, 1.2M units | |",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
}
