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

	"github.com/khuebt/herdr-runner-kit/internal/config"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func TestReadVerdict(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cases := []struct{ name, file, verdict, note string }{
		{"plain", write("a.md", "# Review: x\nVerdict: pass\n"), task.ReviewPass, ""},
		{"bold", write("b.md", "# Review: x\n**Verdict:** fix\n"), task.ReviewFix, ""},
		{"quoted upper case", write("c.md", "> verdict: FIX\n"), task.ReviewFix, ""},
		{"no line", write("d.md", "# Review: x\nThe verdict is fine.\n"), task.ReviewUnknown, "the report has no Verdict: pass or Verdict: fix line"},
		{"missing", filepath.Join(dir, "none.md"), task.ReviewUnknown, "no review report at " + filepath.Join(dir, "none.md")},
		{"no name", "", task.ReviewUnknown, "no review report was named"},
	}
	for _, c := range cases {
		if v, n := ReadVerdict(c.file); v != c.verdict || n != c.note {
			t.Errorf("%s: ReadVerdict = %q, %q", c.name, v, n)
		}
	}
}

func TestWantsReview(t *testing.T) {
	r, _, _ := newTestRunner(t)
	yes, no := true, false
	if got, err := r.WantsReview("code", nil); err != nil || got {
		t.Errorf("code, no flag = %v, %v", got, err)
	}
	if got, err := r.WantsReview("", &yes); err != nil || !got {
		t.Errorf("default kind, flag = %v, %v", got, err)
	}
	k := r.Config.Kinds["code"]
	k.Review = true
	r.Config.Kinds["code"] = k
	if got, err := r.WantsReview("code", nil); err != nil || !got {
		t.Errorf("code with kind default = %v, %v", got, err)
	}
	if got, err := r.WantsReview("code", &no); err != nil || got {
		t.Errorf("code with --review=false = %v, %v", got, err)
	}
	if _, err := r.WantsReview("fix", &yes); err == nil || !strings.Contains(err.Error(), "fix") {
		t.Errorf("fix with flag: %v", err)
	}
	if got, err := r.WantsReview("fix", nil); err != nil || got {
		t.Errorf("fix, no flag = %v, %v", got, err)
	}
	if _, err := r.WantsReview("nope", nil); err == nil {
		t.Error("unknown kind accepted")
	}
}

func TestReviewerAgent(t *testing.T) {
	r, _, _ := newTestRunner(t)
	const writer = "claude:claude-sonnet-5-5:medium"
	r.LookPath = func(string) (string, error) { return "/bin/x", nil }
	if got, err := r.ReviewerAgent("claude:claude-opus-5-5:high", ""); err != nil || got != "codex::high" {
		t.Errorf("claude writer, everything installed = %q, %v", got, err)
	}
	r.LookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	if got, err := r.ReviewerAgent(writer, ""); err != nil || got != "claude:claude-opus-5-5:high" {
		t.Errorf("claude writer, nothing installed = %q, %v", got, err)
	}
	r.Config.Review.Agent = "codex:gpt-x:low"
	if got, err := r.ReviewerAgent(writer, ""); err != nil || got != "codex:gpt-x:low" {
		t.Errorf("config agent = %q, %v", got, err)
	}
	if got, err := r.ReviewerAgent(writer, "pi:m:high"); err != nil || got != "pi:m:high" {
		t.Errorf("override = %q, %v", got, err)
	}
	if _, err := r.ReviewerAgent(writer, "nope"); err == nil || !strings.Contains(err.Error(), "review agent") {
		t.Errorf("bad override: %v", err)
	}
}

// reviewWriter records a running writer task in repo.
func reviewWriter(t *testing.T, r *Runner, repo string, worktree bool) *task.Task {
	t.Helper()
	w := &task.Task{
		Kind: "code", Agent: "claude:claude-sonnet-5-5:medium", Prompt: "build the thing", Skill: "ak:cook", Title: "build",
		Cwd: repo, RepoRoot: repo, Owner: repo, UseWorktree: worktree, Status: task.Running,
		PhaseStart: "aaa", Language: "Vietnamese",
	}
	if err := r.Store.Create(w); err != nil {
		t.Fatal(err)
	}
	return w
}

func TestSpawnReviewQueuesAfterTheWriter(t *testing.T) {
	r, _, _ := newTestRunner(t)
	repo := gitRepo(t)
	w := reviewWriter(t, r, repo, true)
	rv, err := r.SpawnReview(context.Background(), ReviewSpawn{Writer: w, Agent: "claude:claude-opus-5-5:high"})
	if err != nil {
		t.Fatal(err)
	}
	if rv.Status != task.Queued || !slices.Equal(rv.After, []string{w.ID}) || rv.CheckoutOf != w.ID || rv.UseWorktree {
		t.Fatalf("review = %s after %v checkout %q worktree %v", rv.Status, rv.After, rv.CheckoutOf, rv.UseWorktree)
	}
	if rv.Review == nil || rv.Review.Of != w.ID || rv.Review.Writer != agentKey(w.Agent, w.ClaudeConfigDir) {
		t.Errorf("review = %+v", rv.Review)
	}
	if rv.Skill != "ak:code-review" || rv.Owner != w.Owner || rv.Language != "Vietnamese" || rv.Agent != "claude:claude-opus-5-5:high" {
		t.Errorf("skill %q owner %q language %q agent %q", rv.Skill, rv.Owner, rv.Language, rv.Agent)
	}
	for _, want := range []string{"build the thing", "It ran the skill ak:cook.", "Verdict: pass | fix", "review of " + w.ID} {
		if !strings.Contains(rv.Prompt+rv.Title, want) {
			t.Errorf("prompt and title lack %q", want)
		}
	}

	plain := reviewWriter(t, r, repo, false)
	rv, err = r.SpawnReview(context.Background(), ReviewSpawn{Writer: plain, Agent: "codex::high"})
	if err != nil {
		t.Fatal(err)
	}
	if rv.CheckoutOf != "" || rv.Cwd != plain.Cwd {
		t.Errorf("no worktree: checkout %q cwd %q", rv.CheckoutOf, rv.Cwd)
	}
}

func TestPrepareReviewBaseAndFile(t *testing.T) {
	r, _, _ := newTestRunner(t)
	repo := gitRepo(t)
	w := reviewWriter(t, r, repo, false)
	if _, err := r.Store.Update(w.ID, func(t *task.Task) error { t.PhaseStart = "aaa"; return nil }); err != nil {
		t.Fatal(err)
	}
	rv, err := r.SpawnReview(context.Background(), ReviewSpawn{Writer: w, Agent: "codex::high"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.prepareReview(rv)
	if err != nil {
		t.Fatal(err)
	}
	if got.Review.Base != "aaa" || filepath.Dir(got.Review.File) != r.OutDir(rv.ID) || !strings.HasPrefix(filepath.Base(got.Review.File), "review-") {
		t.Fatalf("single review = %+v", got.Review)
	}
	again, err := r.prepareReview(got)
	if err != nil || again.Review.File != got.Review.File || again.Review.Base != "aaa" {
		t.Errorf("a second call = %+v, %v", again.Review, err)
	}

	// A plan: two phase tasks, a first review, then a final one.
	planDir := filepath.Join(t.TempDir(), "plan")
	mk := func(tk *task.Task) *task.Task {
		tk.Kind, tk.Agent, tk.Prompt, tk.Cwd, tk.RepoRoot, tk.Owner, tk.Plan = "code", "claude", "x", repo, repo, repo, planDir
		if err := r.Store.Create(tk); err != nil {
			t.Fatal(err)
		}
		return tk
	}
	mk(&task.Task{Phase: 1, PhaseStart: "s1", Status: task.Done})
	mk(&task.Task{Phase: 2, PhaseStart: "s2", Status: task.Done})
	first := mk(&task.Task{Review: &task.Review{}, Status: task.Queued})
	first, err = r.prepareReview(first)
	if err != nil {
		t.Fatal(err)
	}
	if first.Review.Base != "s1" || filepath.Dir(first.Review.File) != filepath.Join(planDir, "reports") || len(first.Review.Earlier) != 0 {
		t.Fatalf("first plan review = %+v", first.Review)
	}
	if err := os.WriteFile(first.Review.File, []byte("Verdict: fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.Update(first.ID, func(t *task.Task) error { t.Status, t.PhaseStart = task.Done, "r1"; return nil }); err != nil {
		t.Fatal(err)
	}
	final := mk(&task.Task{Review: &task.Review{Final: true}, Status: task.Queued})
	final, err = r.prepareReview(final)
	if err != nil {
		t.Fatal(err)
	}
	if final.Review.Base != "r1" || !slices.Equal(final.Review.Earlier, []string{first.Review.File}) {
		t.Fatalf("final review = %+v", final.Review)
	}
	if final.Review.File == first.Review.File {
		t.Errorf("both reviews write %s", final.Review.File)
	}
	if _, err := os.Stat(first.Review.File); err != nil {
		t.Errorf("the earlier report is gone: %v", err)
	}
}

func TestReviewRangeText(t *testing.T) {
	tk := &task.Task{Cwd: "/work/repo", Review: &task.Review{Base: "abc", File: "/r/review.md", Earlier: []string{"/r/old.md"}}}
	got := reviewRange(tk)
	for _, want := range []string{"git diff abc..HEAD in /work/repo", "git log --oneline abc..HEAD", "/r/review.md; it is the only file you write", "Earlier reviews of this plan: /r/old.md"} {
		if !strings.Contains(got, want) {
			t.Errorf("range lacks %q:\n%s", want, got)
		}
	}
	tk.Review = &task.Review{File: "/r/review.md"}
	got = reviewRange(tk)
	if !strings.Contains(got, "merge-base with the repository's default branch") || strings.Contains(got, "git diff") || strings.Contains(got, "Earlier") {
		t.Errorf("no base: %s", got)
	}
}

func TestFinishReviewRecordsVerdictAndChanges(t *testing.T) {
	r, _, _ := newTestRunner(t)
	repo := gitRepo(t)
	report := filepath.Join(t.TempDir(), "review.md")
	if err := os.WriteFile(report, []byte("Verdict: fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mk := func() *task.Task {
		tk := &task.Task{
			Kind: "code", Agent: "claude", Prompt: "x", Cwd: repo, RepoRoot: repo, Owner: repo, Status: task.Done,
			PhaseStart: headOf(t, repo), Review: &task.Review{File: report},
		}
		if err := r.Store.Create(tk); err != nil {
			t.Fatal(err)
		}
		return tk
	}
	tk := mk()
	r.finishReview(context.Background(), tk)
	got, _ := r.Store.Get(tk.ID)
	if got.Review.Verdict != task.ReviewFix || got.Review.Note != "" {
		t.Fatalf("review = %+v", got.Review)
	}
	if n := countEvents(t, r, tk.ID, "review-verdict"); n != 1 {
		t.Errorf("review-verdict events = %d", n)
	}
	r.finishReview(context.Background(), got)
	if n := countEvents(t, r, tk.ID, "review-verdict"); n != 1 {
		t.Errorf("a second finish emitted again: %d", n)
	}

	tk2 := mk()
	writeFiles(t, repo, map[string]string{"a.go": "a"})
	git(t, repo, "add", "a.go")
	git(t, repo, "commit", "-q", "-m", "reviewer commit")
	r.finishReview(context.Background(), tk2)
	got, _ = r.Store.Get(tk2.ID)
	if !strings.Contains(got.Review.Note, "1 commit") || got.Review.Verdict != task.ReviewFix {
		t.Errorf("note = %q", got.Review.Note)
	}
	writeFiles(t, repo, map[string]string{"a.go": "changed"})
	if c := checkoutChanges(context.Background(), tk2); !strings.Contains(c, "files a.go") {
		t.Errorf("changes = %q", c)
	}
}

// finalReviewFixture records a one-phase plan whose phase is done, a running
// final review and a fixup queued after it.
func finalReviewFixture(t *testing.T, r *Runner, report string) (review, fixup *task.Task) {
	t.Helper()
	repo := gitRepo(t)
	phase, planDir := planTask(t, r, repo, srcPhase)
	if _, err := r.Store.Update(phase.ID, func(t *task.Task) error { t.FinishedAt = time.Now(); return nil }); err != nil {
		t.Fatal(err)
	}
	review = &task.Task{
		Kind: "code", Agent: "codex::high", Prompt: "review", Cwd: repo, RepoRoot: repo, Owner: repo,
		Plan: planDir, PhaseStart: headOf(t, repo), Status: task.Running,
		Review: &task.Review{Final: true, File: report},
	}
	if err := r.Store.Create(review); err != nil {
		t.Fatal(err)
	}
	fixup, err := r.Spawn(context.Background(), SpawnOptions{
		Kind: "code", Prompt: "fix", Cwd: repo, Worktree: new(false), After: []string{review.ID}, Plan: planDir, FixupOf: review.ID,
	})
	if err != nil || fixup.Status != task.Queued {
		t.Fatalf("fixup = %+v, %v", fixup, err)
	}
	return review, fixup
}

func finishTask(t *testing.T, r *Runner, id string, status task.Status, summary string) {
	t.Helper()
	if _, _, err := r.Store.Transition(id, status, nil, func(t *task.Task) { t.Summary = summary }); err != nil {
		t.Fatal(err)
	}
}

func planDoneSummaries(t *testing.T, r *Runner) []string {
	t.Helper()
	all, _, err := r.Store.ReadEventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range all {
		if ev.Type == "plan-done" {
			out = append(out, ev.Summary)
		}
	}
	return out
}

func writeReport(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "review.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFinalReviewPassSkipsTheFixup(t *testing.T) {
	r, _, _ := newTestRunner(t)
	review, fixup := finalReviewFixture(t, r, writeReport(t, "Verdict: pass\n"))
	finishTask(t, r, review.ID, task.Done, "Verdict: pass, 0 verified findings")
	if err := r.After(context.Background(), review.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Store.Get(fixup.ID)
	if got.Status != task.Done || !strings.HasPrefix(got.Summary, "skipped:") {
		t.Fatalf("fixup = %s %q", got.Status, got.Summary)
	}
	ev := events(t, r, fixup.ID)
	if countEvents(t, r, fixup.ID, "fixup-skipped") != 1 || slices.Contains(ev, "done") || slices.Contains(ev, "spawned") {
		t.Errorf("fixup events = %v", ev)
	}
	if done := planDoneSummaries(t, r); len(done) != 1 || !strings.HasSuffix(done[0], ", review pass") {
		t.Fatalf("plan-done = %q", done)
	}
}

func TestFinalReviewFixRunsTheFixup(t *testing.T) {
	r, _, _ := newTestRunner(t)
	review, fixup := finalReviewFixture(t, r, writeReport(t, "Verdict: fix\n"))
	finishTask(t, r, review.ID, task.Done, "Verdict: fix, 1 verified finding")
	if err := r.After(context.Background(), review.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Store.Get(fixup.ID)
	if got.Status == task.Queued || got.Status == task.Done {
		t.Fatalf("fixup = %s, want it started", got.Status)
	}
	if done := planDoneSummaries(t, r); len(done) != 0 {
		t.Fatalf("plan-done before the fixup finished: %q", done)
	}
	finishTask(t, r, fixup.ID, task.Done, "fixed it")
	if err := r.After(context.Background(), fixup.ID); err != nil {
		t.Fatal(err)
	}
	done := planDoneSummaries(t, r)
	want := ", review fix, fixup " + fixup.ID + " done"
	if len(done) != 1 || !strings.HasSuffix(done[0], want) {
		t.Fatalf("plan-done = %q, want suffix %q", done, want)
	}
	all, _, _ := r.Store.ReadEventsSince(0)
	for _, ev := range all {
		if ev.Type != "plan-done" {
			continue
		}
		data, err := os.ReadFile(ev.Outputs[0])
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range []string{"## Reviews", review.ID, fixup.ID, "final review", "fixed it"} {
			if !strings.Contains(string(data), w) {
				t.Errorf("run report lacks %q:\n%s", w, data)
			}
		}
	}
}

func TestFailedFinalReviewStillEndsThePlan(t *testing.T) {
	r, _, _ := newTestRunner(t)
	review, fixup := finalReviewFixture(t, r, "")
	finishTask(t, r, review.ID, task.Failed, "")
	if err := r.After(context.Background(), review.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Store.Get(fixup.ID); got.Status != task.Failed {
		t.Errorf("fixup = %s", got.Status)
	}
	if done := planDoneSummaries(t, r); len(done) != 1 || !strings.Contains(done[0], ", review failed") {
		t.Fatalf("plan-done = %q", done)
	}
}

func TestReviewerNeverFallsBackToTheWriter(t *testing.T) {
	r, _, _ := newTestRunner(t)
	writer := "claude:claude-opus-5-5:high"
	r.Config.QuotaFallback = []config.Fallback{{Agent: writer}, {Agent: "claude:claude-sonnet-5-5"}}
	rv := &task.Task{Agent: "codex::high", Review: &task.Review{Writer: agentKey(writer, "")}}
	if agent, _, ok := r.nextFallback(rv); !ok || agent != "claude:claude-sonnet-5-5" {
		t.Errorf("fallback = %q, %v", agent, ok)
	}
	r.Config.QuotaFallback = r.Config.QuotaFallback[:1]
	if agent, _, ok := r.nextFallback(rv); ok {
		t.Errorf("fell back to the writer: %q", agent)
	}
	if _, _, ok := r.nextFallback(&task.Task{Agent: "codex::high"}); !ok {
		t.Error("a non-review task keeps the writer's entry")
	}
}

func TestParentInputsNameTheReviewReport(t *testing.T) {
	r, _, _ := newTestRunner(t)
	rv := &task.Task{
		Kind: "code", Agent: "codex", Prompt: "x", Cwd: plainDir(t), Owner: "o", Status: task.Done, Summary: "Verdict: fix",
		Review: &task.Review{File: "/r/review.md", Verdict: task.ReviewFix, Note: "the checkout changed"},
	}
	if err := r.Store.Create(rv); err != nil {
		t.Fatal(err)
	}
	inputs, _, err := r.parentInputs([]string{rv.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Review report: /r/review.md (verdict fix).", "Note: the checkout changed."} {
		if !strings.Contains(inputs, want) {
			t.Errorf("inputs lack %q:\n%s", want, inputs)
		}
	}
}

func TestLaunchRecordsTheStartOfEveryTask(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	repo := gitRepo(t)
	tk := spawnStarting(t, r, SpawnOptions{Kind: "code", Prompt: "plain", Cwd: repo, Worktree: new(false)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Store.Get(tk.ID)
	if got.PhaseStart == "" || got.PhaseStart != headOf(t, repo) {
		t.Errorf("PhaseStart = %q, want %q; round %q root %q status %s err %q", got.PhaseStart, headOf(t, repo), got.RoundHead, got.RepoRoot, got.Status, got.Error)
	}

	w := reviewWriter(t, r, repo, false)
	if _, err := r.Store.Update(w.ID, func(t *task.Task) error { t.Status = task.Done; return nil }); err != nil {
		t.Fatal(err)
	}
	rv, err := r.SpawnReview(context.Background(), ReviewSpawn{Writer: w, Agent: "claude:claude-opus-5-5:high"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Launch(context.Background(), rv.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = r.Store.Get(rv.ID)
	if got.Review.Base != "aaa" || got.Review.File == "" {
		t.Errorf("review = %+v", got.Review)
	}
	prompt := lastCall(fake, "agent", "prompt")
	if len(prompt) < 4 || !strings.Contains(prompt[3], "Review range: run git log --oneline aaa..HEAD") || !strings.Contains(prompt[3], got.Review.File) {
		t.Errorf("first prompt = %q", prompt)
	}
}
