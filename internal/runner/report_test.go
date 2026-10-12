package runner

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// running spawns a task and marks it running, as a finished launch would.
func running(t *testing.T, r *Runner, o SpawnOptions) *task.Task {
	t.Helper()
	tk := spawnStarting(t, r, o)
	got, _, err := r.Store.Transition(tk.ID, task.Running, nil, func(t *task.Task) { t.Herdr.PaneID = "w1:p" + t.ID })
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestReportValidationAndState(t *testing.T) {
	r, _, _ := newTestRunner(t)
	dir := plainDir(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: dir})
	var after []string
	r.StartAfter = func(id string) error { after = append(after, id); return nil }

	for _, o := range []ReportOptions{
		{Status: task.Done},
		{Status: task.NeedsHuman, Summary: "s"},
		{Status: task.Done, Summary: "s", Question: "q"},
		{Status: "maybe", Summary: "s"},
		{Status: task.Done, Summary: "s", Outputs: []string{filepath.Join(dir, "missing.md")}},
	} {
		if _, err := r.Report(tk.ID, o); err == nil {
			t.Errorf("Report(%+v) succeeded", o)
		}
	}

	out := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(out, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := r.Report(tk.ID, ReportOptions{Status: task.NeedsHuman, Question: "Drop it?"})
	if err != nil || got.Status != task.NeedsHuman || got.Question != "Drop it?" {
		t.Fatalf("needs-human: %+v, %v", got, err)
	}
	got, err = r.Report(tk.ID, ReportOptions{Status: task.Done, Summary: " Fixed. ", Outputs: []string{out}})
	if err != nil || got.Status != task.Done || got.Summary != "Fixed." || got.Question != "" ||
		!slices.Equal(got.Outputs, []string{out}) || got.FinishedAt.IsZero() {
		t.Fatalf("done: %+v, %v", got, err)
	}
	if _, err := r.Report(tk.ID, ReportOptions{Status: task.Failed, Summary: "late"}); err == nil ||
		!strings.Contains(err.Error(), "already done") {
		t.Errorf("report after done: %v", err)
	}
	if !slices.Equal(after, []string{tk.ID, tk.ID}) {
		t.Errorf("follow-ups = %v", after)
	}
	all, _, _ := r.Store.ReadEventsSince(0)
	last := all[len(all)-1]
	if last.Type != "done" || last.Summary != "Fixed." || !slices.Equal(last.Outputs, []string{out}) {
		t.Errorf("event = %+v", last)
	}
}

func TestReportBeforeStartIsRefused(t *testing.T) {
	r, _, _ := newTestRunner(t)
	held := spawnStarting(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t), Gate: task.GateHuman})
	if _, err := r.Report(held.ID, ReportOptions{Status: task.Done, Summary: "s"}); err == nil {
		t.Fatal("a held task reported done")
	}
}

func TestChainStartsChildWhenAllParentsDone(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	dir := plainDir(t)
	p1 := running(t, r, SpawnOptions{Prompt: "one", Title: "first", Cwd: dir})
	p2 := running(t, r, SpawnOptions{Prompt: "two", Title: "second", Cwd: dir})
	child := spawnStarting(t, r, SpawnOptions{Prompt: "join them", Cwd: dir, After: []string{p1.ID, p2.ID}})

	if _, err := r.Report(p1.ID, ReportOptions{Status: task.Done, Summary: "one done"}); err != nil {
		t.Fatal(err)
	}
	if err := r.After(context.Background(), p1.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Store.Get(child.ID); got.Status != task.Queued {
		t.Fatalf("child started with a parent pending: %s", got.Status)
	}
	if _, err := r.Report(p2.ID, ReportOptions{Status: task.Done, Summary: "two done"}); err != nil {
		t.Fatal(err)
	}
	if err := r.After(context.Background(), p2.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Store.Get(child.ID)
	if got.Status != task.Running {
		t.Fatalf("child = %s, want running", got.Status)
	}
	if !strings.Contains(got.Inputs, "t1 (first): one done") || !strings.Contains(got.Inputs, "t2 (second): two done") {
		t.Errorf("inputs = %q", got.Inputs)
	}
	var prompt string
	for _, c := range fake.calls {
		if len(c) == 4 && c[0] == "agent" && c[1] == "prompt" {
			prompt = c[3]
		}
	}
	if !strings.HasPrefix(prompt, "join them\n\nInputs from the tasks this one waited for:") {
		t.Errorf("first prompt = %q", prompt)
	}
	// A second follow-up for the same parent must not launch the child again.
	starts := fake.count("agent", "start")
	if err := r.After(context.Background(), p2.ID); err != nil || fake.count("agent", "start") != starts {
		t.Errorf("child launched twice: %v", err)
	}
}

func TestChainHumanGateHoldsChild(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	dir := plainDir(t)
	p := running(t, r, SpawnOptions{Prompt: "plan", Cwd: dir})
	child := spawnStarting(t, r, SpawnOptions{Prompt: "cook", Cwd: dir, After: []string{p.ID}, Gate: task.GateHuman})
	if _, err := r.Report(p.ID, ReportOptions{Status: task.Done, Summary: "planned"}); err != nil {
		t.Fatal(err)
	}
	if err := r.After(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Store.Get(child.ID)
	if got.Status != task.Held || !strings.Contains(got.Inputs, "planned") || fake.count("agent", "start") != 0 {
		t.Fatalf("child = %+v", got)
	}
	var launched []string
	r.StartLauncher = func(id string) (int, error) { launched = append(launched, id); return 7, nil }
	got, err := r.Release(child.ID, "use the small variant")
	if err != nil || got.Status != task.Starting || !strings.HasSuffix(got.Prompt, "Note from the user: use the small variant") {
		t.Fatalf("release: %+v, %v", got, err)
	}
	if !slices.Equal(launched, []string{child.ID}) {
		t.Errorf("launched = %v", launched)
	}
	if _, err := r.Release(child.ID, ""); err == nil {
		t.Error("released a task that is not held")
	}
}

func TestChainFailedParentFailsDependents(t *testing.T) {
	r, _, _ := newTestRunner(t)
	dir := plainDir(t)
	p := running(t, r, SpawnOptions{Prompt: "a", Cwd: dir})
	c1 := spawnStarting(t, r, SpawnOptions{Prompt: "b", Cwd: dir, After: []string{p.ID}})
	c2 := spawnStarting(t, r, SpawnOptions{Prompt: "c", Cwd: dir, After: []string{c1.ID}, Gate: task.GateHuman})
	if _, err := r.Report(p.ID, ReportOptions{Status: task.Failed, Summary: "broke"}); err != nil {
		t.Fatal(err)
	}
	if err := r.After(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{c1.ID, c2.ID} {
		got, _ := r.Store.Get(id)
		if got.Status != task.Failed || !strings.HasPrefix(got.Error, "parent ") {
			t.Errorf("%s = %s (%s)", id, got.Status, got.Error)
		}
	}
}

func TestStopFailsDependents(t *testing.T) {
	r, _, _ := newTestRunner(t)
	dir := plainDir(t)
	p := running(t, r, SpawnOptions{Prompt: "a", Cwd: dir})
	c := spawnStarting(t, r, SpawnOptions{Prompt: "b", Cwd: dir, After: []string{p.ID}})
	if _, err := r.Stop(context.Background(), p.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Store.Get(c.ID); got.Status != task.Failed || got.Error != "parent t1 stopped" {
		t.Errorf("child = %s (%s)", got.Status, got.Error)
	}
}

func TestSpawnAfterDoneParentCarriesInputsAndBase(t *testing.T) {
	r, _, _ := newTestRunner(t)
	repo := gitRepo(t)
	p := running(t, r, SpawnOptions{Kind: "code", Prompt: "add parser", Cwd: repo})
	if _, err := r.Report(p.ID, ReportOptions{Status: task.Done, Summary: "parser added"}); err != nil {
		t.Fatal(err)
	}
	c := spawnStarting(t, r, SpawnOptions{Kind: "code", Prompt: "test parser", Cwd: repo, After: []string{p.ID}})
	if !strings.Contains(c.Inputs, "parser added") || !strings.Contains(c.Inputs, "Branch: hrk/t1-add-parser") {
		t.Errorf("inputs = %q", c.Inputs)
	}
	if c.Worktree == nil || c.Worktree.Base != "hrk/t1-add-parser" {
		t.Errorf("worktree = %+v", c.Worktree)
	}
}

func TestAnswerResumesNeedsHuman(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	if _, err := r.Answer(context.Background(), tk.ID, "yes"); err == nil {
		t.Error("answered a running task")
	}
	if _, err := r.Report(tk.ID, ReportOptions{Status: task.NeedsHuman, Question: "Drop it?"}); err != nil {
		t.Fatal(err)
	}
	working, err := r.Answer(context.Background(), tk.ID, "yes, drop it")
	if err != nil || !working {
		t.Fatalf("answer: %v %v", working, err)
	}
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Running || got.Question != "" {
		t.Errorf("task = %+v", got)
	}
	if p := fake.call("agent", "prompt"); len(p) != 4 || p[3] != "yes, drop it" {
		t.Errorf("prompt = %q", p)
	}
	if ev := events(t, r, tk.ID); ev[len(ev)-1] != "answered" {
		t.Errorf("events = %v", ev)
	}
}

func TestAnswerRefusesStartupDialog(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	if _, _, err := r.Store.Transition(tk.ID, task.NeedsHuman, nil, func(t *task.Task) { t.Error = "codex-trust" }); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Answer(context.Background(), tk.ID, "yes"); err == nil || !strings.Contains(err.Error(), "codex-trust") {
		t.Errorf("err = %v", err)
	}
}

func TestTurnEndNudgesThenReports(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	ctx := context.Background()

	block, err := r.TurnEnd(ctx, tk.ID, task.Claude, []byte(`{}`))
	if err != nil || !strings.Contains(block, "report done") {
		t.Fatalf("first turn-end: %q, %v", block, err)
	}
	for range 3 {
		if block, err = r.TurnEnd(ctx, tk.ID, task.Claude, []byte(`{}`)); err != nil || block != "" {
			t.Fatalf("later turn-end: %q, %v", block, err)
		}
	}
	if ev := events(t, r, tk.ID); !slices.Equal(ev[len(ev)-2:], []string{"nudged", "turn-ended-unreported"}) {
		t.Errorf("events = %v", ev)
	}
	// A prompt from the main session starts a new count.
	if _, err := r.Send(ctx, tk.ID, "continue"); err != nil {
		t.Fatal(err)
	}
	if block, _ = r.TurnEnd(ctx, tk.ID, task.Claude, nil); block == "" {
		t.Error("no nudge after send")
	}
}

func TestTurnEndSkipsBackgroundWorkAndReportedTasks(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	fake.on("pane read", ok("I started 2 tasks earlier.\n❯ \n  ⏵⏵ bypass permissions on · 1 shell · ← for agents\n"))
	if block, err := r.TurnEnd(context.Background(), tk.ID, task.Claude, nil); block != "" || err != nil {
		t.Errorf("nudged while a shell runs: %q %v", block, err)
	}
	fake.on("pane read", ok("I started 2 tasks earlier.\n❯ \n  ⏵⏵ bypass permissions on · ← for agents\n"))
	if block, _ := r.TurnEnd(context.Background(), tk.ID, task.Claude, nil); block == "" {
		t.Error("conversation text was taken for background work")
	}
	if _, err := r.Report(tk.ID, ReportOptions{Status: task.Done, Summary: "ok"}); err != nil {
		t.Fatal(err)
	}
	if block, _ := r.TurnEnd(context.Background(), tk.ID, task.Claude, nil); block != "" {
		t.Error("nudged a task that reported")
	}
}

func TestTurnEndCodexFiltersTitleAndOtherThreads(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Kind: "research", Agent: "codex", Prompt: "x", Cwd: plainDir(t)})
	ctx := context.Background()
	title := `{"type":"agent-turn-complete","thread-id":"title","input-messages":["Generate a concise, single-line task title for this"]}`
	if _, err := r.TurnEnd(ctx, tk.ID, task.Codex, []byte(title)); err != nil || fake.count("agent", "prompt") != 0 {
		t.Fatalf("title turn nudged: %v", err)
	}
	turn := `{"type":"agent-turn-complete","thread-id":"main","input-messages":["x"]}`
	if _, err := r.TurnEnd(ctx, tk.ID, task.Codex, []byte(turn)); err != nil || fake.count("agent", "prompt") != 1 {
		t.Fatalf("real turn not nudged: %v", err)
	}
	other := `{"type":"agent-turn-complete","thread-id":"other","input-messages":["y"]}`
	if _, err := r.TurnEnd(ctx, tk.ID, task.Codex, []byte(other)); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Store.Get(tk.ID); got.CodexThread != "main" || got.Unreported != 1 {
		t.Errorf("task = thread %q unreported %d", got.CodexThread, got.Unreported)
	}
}
