package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/config"
	"github.com/khuebt/herdr-runner-kit/internal/herdr"
	"github.com/khuebt/herdr-runner-kit/internal/planrun"
	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// planHerdr stands in for herdr in a plan run: worktree create makes a real
// git worktree, so phases commit and the kit checks them for real.
type planHerdr struct {
	mu        sync.Mutex
	t         *testing.T
	worktrees string
	checkout  map[string]string // workspace id -> checkout path
	tabs      [][]string
	panes     int
}

func (f *planHerdr) exec(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := strings.Join(args[:min(2, len(args))], " ")
	flag := func(name string) string {
		if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
			return args[i+1]
		}
		return ""
	}
	switch cmd {
	case "worktree create":
		branch := flag("--branch")
		path := filepath.Join(f.worktrees, strings.ReplaceAll(branch, "/", "-"))
		base := flag("--base")
		out, err := exec.Command("git", "-C", flag("--cwd"), "worktree", "add", "-q", "-b", branch, path, base).CombinedOutput()
		if err != nil {
			return nil, []byte(`{"error":{"code":"git","message":"` + strings.TrimSpace(string(out)) + `"}}`), err
		}
		f.checkout["w2"] = path
		return fmt.Appendf(nil, `{"result":{"root_pane":{"pane_id":"w2:p1","tab_id":"w2:t1","workspace_id":"w2"},"worktree":{"path":%q,"branch":%q}}}`, path, branch), nil, nil
	case "workspace get":
		return fmt.Appendf(nil, `{"result":{"workspace":{"workspace_id":%q,"worktree":{"checkout_path":%q,"is_linked_worktree":true}}}}`, args[2], f.checkout[args[2]]), nil, nil
	case "workspace list":
		return []byte(`{"result":{"workspaces":[]}}`), nil, nil
	case "tab create":
		f.tabs = append(f.tabs, args)
		f.panes++
		ws := flag("--workspace")
		return fmt.Appendf(nil, `{"result":{"root_pane":{"pane_id":"%s:p%d","tab_id":"%s:t%d","workspace_id":%q}}}`, ws, f.panes, ws, f.panes, ws), nil, nil
	case "agent start":
		return []byte(`{"result":{"agent":{"agent":"claude","agent_status":"idle"}}}`), nil, nil
	case "agent wait":
		return []byte(`{"result":{"agent":{"agent_status":"working"}}}`), nil, nil
	case "pane read":
		return []byte("❯ \n"), nil, nil
	}
	return []byte("{}"), nil, nil
}

// planRunner returns a runner that launches in process and runs report
// follow-ups at once, and a repository holding plans/demo with phases.
func planRunner(t *testing.T, phases map[string]string) (*runner.Runner, *planHerdr, string, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for k, v := range map[string]string{
		"GIT_AUTHOR_NAME": "Lan Pham", "GIT_COMMITTER_NAME": "Lan Pham",
		"GIT_AUTHOR_EMAIL": "lan@example.com", "GIT_COMMITTER_EMAIL": "lan@example.com",
	} {
		t.Setenv(k, v)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q", "-b", "main")
	gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	planDir := filepath.Join(repo, "plans", "demo")
	if err := os.MkdirAll(planDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := phases["plan.md"]; !ok {
		phases["plan.md"] = "---\ntitle: \"Demo plan\"\nstatus: pending\n---\n\n# Demo plan\n"
	}
	for name, body := range phases {
		if err := os.WriteFile(filepath.Join(planDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	worktrees, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fake := &planHerdr{t: t, worktrees: worktrees, checkout: map[string]string{}}
	r := runner.New(store, config.Default(), "/usr/local/bin/hrk")
	r.Herdr = &herdr.Client{Bin: "herdr", Exec: fake.exec}
	env := map[string]string{"HERDR_ENV": "1", "HERDR_WORKSPACE_ID": "w1", "CLAUDE_CONFIG_DIR": t.TempDir()}
	r.Getenv = func(k string) string { return env[k] }
	r.Sleep = func(time.Duration) {}
	r.Notify = nil
	r.ProcTable = nil
	r.StartLauncher = func(id string) (int, error) {
		return 4242, r.Launch(context.Background(), id)
	}
	r.StartAfter = func(id string) error { return r.After(context.Background(), id) }
	t.Chdir(repo)
	return r, fake, repo, planDir
}

func phaseFile(n int, deps, scope string) string {
	s := fmt.Sprintf("---\nphase: %d\ntitle: \"Part %d\"\nstatus: pending\n", n, n)
	if deps != "" {
		s += "dependencies: " + deps + "\n"
	}
	s += fmt.Sprintf("---\n\n# Phase %d\n\n## Files\n\n", n)
	for p := range strings.FieldsSeq(scope) {
		s += "- `" + p + "`\n"
	}
	return s
}

func runPlanCmd(t *testing.T, r *runner.Runner, planDir string, o planRunOptions) (string, error) {
	t.Helper()
	plan, err := planrun.Read(planDir)
	if err != nil {
		t.Fatal(err)
	}
	if o.Parallel == 0 {
		o.Parallel = 2
	}
	o.Kind, o.Skill = cmpOr(o.Kind, "flow"), cmpOr(o.Skill, "ak:cook")
	o.Gate = cmpOr(o.Gate, "auto")
	var out, warn bytes.Buffer
	o.Warn = func(s string) { warn.WriteString(s + "\n") }
	err = runPlan(t.Context(), &out, r, plan, runner.Toplevel(t.Context(), planDir), o)
	return out.String() + warn.String(), err
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// commitIn writes and commits files in dir, as a phase runner does.
func commitIn(t *testing.T, dir string, files ...string) {
	t.Helper()
	for _, rel := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(rel+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, dir, "add", "--", rel)
	}
	gitIn(t, dir, "commit", "-q", "-m", "feat: "+strings.Join(files, ", "))
}

func reportDone(t *testing.T, r *runner.Runner, id string) {
	t.Helper()
	if _, err := r.Report(id, runner.ReportOptions{Status: task.Done, Summary: "did " + id}); err != nil {
		t.Fatal(err)
	}
}

func getTask(t *testing.T, r *runner.Runner, id string) *task.Task {
	t.Helper()
	tk, err := r.Store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func eventTypes(t *testing.T, r *runner.Runner) []state.Event {
	t.Helper()
	evs, _, err := r.Store.ReadEventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestPlanRunDryRunSpawnsNothing(t *testing.T) {
	r, _, _, planDir := planRunner(t, map[string]string{
		"phase-01-a.md": phaseFile(1, "", "src/one.go"),
		"phase-02-b.md": phaseFile(2, "[1]", "src/two/"),
		"phase-03-c.md": phaseFile(3, "[1]", "docs/three.md"),
		"phase-04-d.md": phaseFile(4, "[2, 3]", ""),
	})
	out, err := runPlanCmd(t, r, planDir, planRunOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"plan Demo plan: 4 phases, 4 to run",
		"wave 1 · phase 1 Part 1 · no dependencies · scope 1 path(s) · alone: it creates the run's checkout",
		"wave 2 · phase 2 Part 2 · after 1 · scope 1 path(s)\n",
		"wave 2 · phase 3 Part 3 · after 1 · scope 1 path(s)\n",
		"wave 3 · phase 4 Part 4 · after 2, 3 · no declared scope · alone: it declares no write scope",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run lacks %q:\n%s", want, out)
		}
	}
	if tasks, _ := r.Store.List(); len(tasks) != 0 {
		t.Fatalf("dry run spawned %d tasks", len(tasks))
	}
	if out, _ := runPlanCmd(t, r, planDir, planRunOptions{DryRun: true, Parallel: 1}); !strings.Contains(out, "wave 4 · phase 4") {
		t.Errorf("--parallel 1 runs every phase alone:\n%s", out)
	}
}

func TestPlanRunSharedCheckoutHoldAndDone(t *testing.T) {
	r, fake, _, planDir := planRunner(t, map[string]string{
		"phase-01-a.md": phaseFile(1, "", "src/one.go"),
		"phase-02-b.md": phaseFile(2, "[1]", "src/two/"),
		"phase-03-c.md": phaseFile(3, "[1]", "docs/three.md"),
		"phase-04-d.md": phaseFile(4, "[2, 3]", "src/four.go"),
	})
	phase3Before, _ := os.ReadFile(filepath.Join(planDir, "phase-03-c.md"))
	out, err := runPlanCmd(t, r, planDir, planRunOptions{Branch: "feat/demo"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "t1 running") || !strings.Contains(out, "t2 queued") || !strings.Contains(out, "after t2,t3") {
		t.Fatalf("plan run output:\n%s", out)
	}
	anchor := getTask(t, r, "t1")
	wt := anchor.Worktree.Path
	if anchor.Worktree.Branch != "feat/demo" || anchor.Plan != planDir || anchor.Phase != 1 || anchor.PhaseStart == "" {
		t.Fatalf("anchor = %+v %+v", anchor, anchor.Worktree)
	}
	if !strings.HasPrefix(anchor.Prompt, filepath.Join(planDir, "phase-01-a.md")+"\n") ||
		!strings.Contains(anchor.Prompt, "You run phase 1 of plan "+filepath.Join(planDir, "plan.md")) {
		t.Errorf("anchor prompt = %q", anchor.Prompt)
	}
	for _, id := range []string{"t2", "t3"} {
		if q := getTask(t, r, id); q.CheckoutOf != "t1" || q.Worktree != nil || q.UseWorktree {
			t.Errorf("%s = %+v", id, q)
		}
	}
	if q := getTask(t, r, "t2"); !strings.Contains(q.Prompt, "Another runner works in this checkout at the same time on docs/three.md") {
		t.Errorf("parallel brief missing: %q", q.Prompt)
	}
	if fm, _ := planrun.Read(planDir); fm.Status != "in-progress" {
		t.Errorf("plan status = %q", fm.Status)
	}

	commitIn(t, wt, "src/one.go")
	reportDone(t, r, "t1")
	if c := getTask(t, r, "t1").PhaseCheck; c == nil || c.Verdict != task.PhaseAccepted || c.Commits != 1 || !c.Clean {
		t.Fatalf("t1 check = %+v", c)
	}
	for _, id := range []string{"t2", "t3"} {
		q := getTask(t, r, id)
		if q.Status != task.Running || q.Cwd != wt || q.PhaseStart == "" {
			t.Fatalf("%s = %s cwd %s", id, q.Status, q.Cwd)
		}
	}
	if tab := fake.tabs[0]; !slices.Contains(tab, "w2") || !slices.Contains(tab, wt) {
		t.Errorf("a sharer opens in the anchor's workspace and checkout: %q", tab)
	}

	// Phase 2 also commits outside its scope, and phase 3 is still at work.
	commitIn(t, wt, "src/two/a.go", "README.md")
	reportDone(t, r, "t2")
	if c := getTask(t, r, "t2").PhaseCheck; c == nil || c.Verdict != task.PhaseOutside || !slices.Equal(c.Outside, []string{"README.md"}) {
		t.Fatalf("t2 check = %+v", c)
	}
	if g := getTask(t, r, "t2").Git; g == nil || !g.Worktree || g.Branch != "feat/demo" || g.SinceStart != 1 {
		t.Errorf("a sharer's git line describes the run's worktree: %+v", g)
	}

	// The anchor's checkout cannot go while sharers work in it.
	if _, err := r.Stop(t.Context(), "t1", true); err == nil || !strings.Contains(err.Error(), "works in this checkout") {
		t.Errorf("remove the anchor's worktree: %v", err)
	}

	commitIn(t, wt, "docs/three.md")
	reportDone(t, r, "t3")
	held := getTask(t, r, "t4")
	if held.Status != task.Held || !strings.Contains(held.HoldReason, "phase 2 changed files outside its scope: README.md") ||
		!strings.HasSuffix(held.HoldReason, "release to continue") {
		t.Fatalf("t4 = %s, %q", held.Status, held.HoldReason)
	}
	if got, _ := os.ReadFile(filepath.Join(planDir, "phase-03-c.md")); string(got) != strings.Replace(string(phase3Before), "status: pending", "status: completed", 1) {
		t.Errorf("phase 3 file =\n%s", got)
	}

	if _, err := r.Release("t4", ""); err != nil {
		t.Fatal(err)
	}
	commitIn(t, wt, "src/four.go")
	reportDone(t, r, "t4")
	var done []state.Event
	for _, ev := range eventTypes(t, r) {
		if ev.Type == "plan-done" {
			done = append(done, ev)
		}
	}
	if len(done) != 1 || done[0].Summary != "plan Demo plan: 4/4 phases done, 3 verified in scope" || len(done[0].Outputs) != 1 {
		t.Fatalf("plan-done = %+v", done)
	}
	report, err := os.ReadFile(done[0].Outputs[0])
	if err != nil || filepath.Dir(done[0].Outputs[0]) != filepath.Join(planDir, "reports") {
		t.Fatalf("report %s: %v", done[0].Outputs[0], err)
	}
	for _, want := range []string{"| 2 Part 2 | t2 | claude", "did t2", "outside its scope: README.md", "phase 4 accepted: 1 commit"} {
		if !strings.Contains(string(report), want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	if fm, _ := planrun.Read(planDir); fm.Status != "completed" {
		t.Errorf("plan status = %q", fm.Status)
	}
	// A second follow-up of the last phase sends no second plan-done.
	if err := r.After(t.Context(), "t4"); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range eventTypes(t, r) {
		if ev.Type == "plan-done" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("plan-done sent %d times", n)
	}
	if out, err := runPlanCmd(t, r, planDir, planRunOptions{}); err != nil || !strings.Contains(out, "every phase of Demo plan is done") {
		t.Errorf("rerun of a done plan: %v\n%s", err, out)
	}
}

func TestPlanRunResumesAfterAFailedPhase(t *testing.T) {
	r, _, _, planDir := planRunner(t, map[string]string{
		"phase-01-a.md": phaseFile(1, "", "src/one.go"),
		"phase-02-b.md": phaseFile(2, "", "src/two.go"),
		"phase-03-c.md": phaseFile(3, "", "src/three.go"),
	})
	if _, err := runPlanCmd(t, r, planDir, planRunOptions{}); err != nil {
		t.Fatal(err)
	}
	wt := getTask(t, r, "t1").Worktree.Path
	commitIn(t, wt, "src/one.go")
	reportDone(t, r, "t1")
	if _, err := runPlanCmd(t, r, planDir, planRunOptions{}); err == nil || !strings.Contains(err.Error(), "t2 (phase 2) is running; wait or stop it") {
		t.Fatalf("rerun while a phase runs: %v", err)
	}
	if _, err := r.Report("t2", runner.ReportOptions{Status: task.Failed, Summary: "tests broke"}); err != nil {
		t.Fatal(err)
	}
	if q := getTask(t, r, "t3"); q.Status != task.Failed {
		t.Fatalf("t3 = %s", q.Status)
	}
	out, err := runPlanCmd(t, r, planDir, planRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "kept phase 1: t1 is done") || !strings.Contains(out, "checkout: "+wt) {
		t.Errorf("resume output:\n%s", out)
	}
	if tasks, _ := r.Store.List(); slices.ContainsFunc(tasks, func(q *task.Task) bool { return q.Phase == 1 && q.ID != "t1" }) {
		t.Error("phase 1 was replayed")
	}
	rerun := getTask(t, r, "t4")
	if rerun.Phase != 2 || rerun.CheckoutOf != "t1" || rerun.Cwd != wt || !slices.Equal(rerun.After, []string{"t1"}) ||
		!strings.Contains(rerun.Prompt, "An earlier attempt (t2) failed: tests broke.") {
		t.Fatalf("rerun of phase 2 = %+v", rerun)
	}
	if !strings.Contains(rerun.Inputs, "t1 (phase 1: Part 1): did t1") {
		t.Errorf("a done phase's result reaches the next prompt: %q", rerun.Inputs)
	}
	if q := getTask(t, r, "t5"); q.Phase != 3 || !slices.Equal(q.After, []string{"t4"}) || q.Status != task.Queued {
		t.Fatalf("phase 3 = %+v", q)
	}
	if _, err := r.Stop(t.Context(), "t4", false); err != nil {
		t.Fatal(err)
	}
	if _, err := runPlanCmd(t, r, planDir, planRunOptions{Branch: "other"}); err == nil || !strings.Contains(err.Error(), "drop --branch") {
		t.Errorf("--branch on a run with a checkout: %v", err)
	}
}

func TestPlanRunContinuesFromTheBranchOfAGoneCheckout(t *testing.T) {
	r, _, _, planDir := planRunner(t, map[string]string{
		"phase-01-a.md": phaseFile(1, "", "src/one.go"),
		"phase-02-b.md": phaseFile(2, "", "src/two.go"),
	})
	if _, err := runPlanCmd(t, r, planDir, planRunOptions{Branch: "feat/first"}); err != nil {
		t.Fatal(err)
	}
	commitIn(t, getTask(t, r, "t1").Worktree.Path, "src/one.go")
	reportDone(t, r, "t1")
	if _, err := r.Report("t2", runner.ReportOptions{Status: task.Failed, Summary: "broke"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Stop(t.Context(), "t1", true); err != nil {
		t.Fatal(err)
	}
	out, err := runPlanCmd(t, r, planDir, planRunOptions{Branch: "feat/second"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "checkout: a new worktree from feat/first, whose worktree is gone") {
		t.Errorf("output:\n%s", out)
	}
	fresh := getTask(t, r, "t3")
	if fresh.Phase != 2 || fresh.Worktree == nil || fresh.Worktree.Base != "feat/first" || fresh.Worktree.Branch != "feat/second" {
		t.Fatalf("new anchor = %+v %+v", fresh, fresh.Worktree)
	}
	if _, err := os.Stat(filepath.Join(fresh.Worktree.Path, "src/one.go")); err != nil {
		t.Errorf("the new checkout lacks the done phase's work: %v", err)
	}
}

func TestPlanRunHumanGateAndRefusals(t *testing.T) {
	r, _, _, planDir := planRunner(t, map[string]string{
		"phase-01-a.md": phaseFile(1, "", "a.go"),
		"phase-02-b.md": phaseFile(2, "", "b.go"),
	})
	t.Setenv("HRK_TASK", "t9")
	cmd := newPlanRunCommand()
	cmd.SetArgs([]string{planDir})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "runners cannot") {
		t.Errorf("inside a runner: %v", err)
	}
	t.Setenv("HRK_TASK", "")

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "plan.md"), []byte("# x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "phase-01-a.md"), []byte(phaseFile(1, "", "")), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = newPlanRunCommand()
	cmd.SetArgs([]string{outside})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "not in a git repository") {
		t.Errorf("outside git: %v", err)
	}

	if _, err := runPlanCmd(t, r, planDir, planRunOptions{Gate: "human"}); err != nil {
		t.Fatal(err)
	}
	if q := getTask(t, r, "t1"); q.Gate != task.GateAuto || q.Status != task.Running {
		t.Errorf("the first phase starts on its own: %s %s", q.Gate, q.Status)
	}
	commitIn(t, getTask(t, r, "t1").Worktree.Path, "a.go")
	reportDone(t, r, "t1")
	if q := getTask(t, r, "t2"); q.Status != task.Held {
		t.Fatalf("t2 = %s", q.Status)
	}
	if _, err := runPlanCmd(t, r, planDir, planRunOptions{}); err == nil || !strings.Contains(err.Error(), "release or stop t2 first") {
		t.Errorf("rerun while held: %v", err)
	}
}

const reviewedPlan = "---\ntitle: \"Demo plan\"\nstatus: pending\nreview: required\nreview_after: 1\nreview_agent: claude:claude-opus-5-5:high\n---\n\n# Demo plan\n"

func reviewedPhases(planMD string) map[string]string {
	return map[string]string{
		"plan.md":       planMD,
		"phase-01-a.md": phaseFile(1, "[]", "a/"),
		"phase-02-b.md": phaseFile(2, "[1]", "b/"),
		"phase-03-c.md": phaseFile(3, "[2]", "c/"),
	}
}

func TestPlanRunDryRunShowsReviews(t *testing.T) {
	r, _, _, planDir := planRunner(t, reviewedPhases(reviewedPlan))
	out, err := runPlanCmd(t, r, planDir, planRunOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"review: required · reviewer claude:claude-opus-5-5:high",
		"review · after wave 1 (phase 1)",
		"final review · after wave 3",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run lacks %q:\n%s", want, out)
		}
	}
	if tasks, _ := r.Store.List(); len(tasks) != 0 {
		t.Fatalf("dry run spawned %d tasks", len(tasks))
	}
}

func TestPlanRunChainsReviewsAndFixup(t *testing.T) {
	r, _, _, planDir := planRunner(t, reviewedPhases(reviewedPlan))
	if _, err := runPlanCmd(t, r, planDir, planRunOptions{}); err != nil {
		t.Fatal(err)
	}
	tasks, err := r.Store.List()
	if err != nil || len(tasks) != 6 {
		t.Fatalf("tasks = %d, %v", len(tasks), err)
	}
	p1, rev, p2, p3, final, fix := getTask(t, r, "t1"), getTask(t, r, "t2"), getTask(t, r, "t3"), getTask(t, r, "t4"), getTask(t, r, "t5"), getTask(t, r, "t6")
	if rev.Review == nil || rev.Review.Final || !slices.Equal(rev.After, []string{p1.ID}) || rev.CheckoutOf != p1.ID ||
		rev.Plan != planDir || rev.Agent != "claude:claude-opus-5-5:high" {
		t.Errorf("review = %+v %+v", rev, rev.Review)
	}
	if !slices.Contains(p2.After, p1.ID) || !slices.Contains(p2.After, rev.ID) || !strings.Contains(p2.Prompt, runner.ReviewFixNote(rev.ID)) {
		t.Errorf("phase 2 = after %v, prompt %q", p2.After, p2.Prompt)
	}
	if strings.Contains(p3.Prompt, runner.ReviewFixNote(rev.ID)) || !slices.Contains(p3.After, p2.ID) {
		t.Errorf("phase 3 = after %v, prompt %q", p3.After, p3.Prompt)
	}
	if final.Review == nil || !final.Review.Final || !slices.Equal(final.After, []string{p3.ID}) {
		t.Errorf("final review = %+v %+v", final, final.Review)
	}
	if fix.FixupOf != final.ID || !slices.Equal(fix.After, []string{final.ID}) || fix.CheckoutOf != p1.ID || fix.Kind != "flow" || fix.Skill != "" {
		t.Errorf("fixup = %+v", fix)
	}
	for _, q := range []*task.Task{rev, p2, p3, final, fix} {
		if q.Status != task.Queued {
			t.Errorf("%s is %s, want queued", q.ID, q.Status)
		}
	}
}

func TestPlanRunReviewFlag(t *testing.T) {
	r, _, _, planDir := planRunner(t, reviewedPhases(reviewedPlan))
	out, err := runPlanCmd(t, r, planDir, planRunOptions{Review: planrun.ReviewSkip})
	if err != nil || strings.Contains(out, "review") {
		t.Fatalf("--review skip: %v\n%s", err, out)
	}
	if tasks, _ := r.Store.List(); len(tasks) != 3 {
		t.Errorf("--review skip spawned %d tasks, want the 3 phases", len(tasks))
	}

	plain := reviewedPhases("---\ntitle: \"Demo plan\"\nstatus: pending\n---\n\n# Demo plan\n")
	r, _, _, planDir = planRunner(t, plain)
	r.Config.Review.Agent = "claude:claude-opus-5-5:high"
	if _, err := runPlanCmd(t, r, planDir, planRunOptions{Review: planrun.ReviewRequired}); err != nil {
		t.Fatal(err)
	}
	tasks, _ := r.Store.List()
	var reviews, fixups int
	for _, q := range tasks {
		switch {
		case q.Review != nil && q.Review.Final:
			reviews++
		case q.Review != nil:
			t.Errorf("%s is a first review without review_after", q.ID)
		case q.FixupOf != "":
			fixups++
		}
	}
	if len(tasks) != 5 || reviews != 1 || fixups != 1 {
		t.Errorf("--review required spawned %d tasks, %d final reviews, %d fixups", len(tasks), reviews, fixups)
	}

	t.Setenv("HRK_TASK", "")
	cmd := newPlanRunCommand()
	cmd.SetArgs([]string{planDir, "--review", "maybe"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), `invalid --review "maybe"`) {
		t.Errorf("--review maybe: %v", err)
	}
}

func TestPlanRunNoFirstReviewInTheLastWave(t *testing.T) {
	r, _, _, planDir := planRunner(t, reviewedPhases(strings.Replace(reviewedPlan, "review_after: 1", "review_after: 3", 1)))
	out, err := runPlanCmd(t, r, planDir, planRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no review after phase 3") {
		t.Errorf("output lacks the note:\n%s", out)
	}
	tasks, _ := r.Store.List()
	if len(tasks) != 5 {
		t.Fatalf("spawned %d tasks, want 3 phases, a final review and a fixup", len(tasks))
	}
	if q := tasks[3]; q.Review == nil || !q.Review.Final {
		t.Errorf("task 4 = %+v", q)
	}
	if q := tasks[4]; q.FixupOf == "" {
		t.Errorf("task 5 = %+v", q)
	}
}
