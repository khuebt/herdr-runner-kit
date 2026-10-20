package runner

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/config"
	"github.com/khuebt/herdr-runner-kit/internal/harness"
	"github.com/khuebt/herdr-runner-kit/internal/herdr"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

const codexWorkingScreen = "• Working (14s • esc to interrupt)\n\n› Ask Codex to do anything\n"

// codexRunning starts a Codex runner in repo, with its dependents recorded
// in after.
func codexRunning(t *testing.T, r *Runner, repo string, after *[]string) *task.Task {
	t.Helper()
	r.StartAfter = func(id string) error { *after = append(*after, id); return nil }
	return running(t, r, SpawnOptions{Kind: "fix", Agent: "codex", Prompt: "x", Cwd: repo})
}

func codexTurnEnd(t *testing.T, r *Runner, id string) {
	t.Helper()
	payload := []byte(`{"type":"agent-turn-complete","thread-id":"th1","input-messages":["x"]}`)
	if _, err := r.TurnEnd(context.Background(), id, task.Codex, payload); err != nil {
		t.Fatal(err)
	}
}

// runnerDone reports done from the runner's own session, as `hrk report`
// inside a runner does.
func runnerDone(t *testing.T, r *Runner, id, summary string) *task.Task {
	t.Helper()
	got, err := r.Report(id, ReportOptions{Status: task.Done, Summary: summary, Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func doneEvents(t *testing.T, r *Runner, id string) []state.Event {
	t.Helper()
	all, _, err := r.Store.ReadEventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	var out []state.Event
	for _, ev := range all {
		if ev.Task == id && ev.Type == "done" {
			out = append(out, ev)
		}
	}
	return out
}

func TestCodexDoneIsFinalAtTurnEnd(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	repo := gitRepo(t)
	var after []string
	tk := codexRunning(t, r, repo, &after)
	writeFile(t, filepath.Join(repo, "notes.txt"))

	got := runnerDone(t, r, tk.ID, "fixed it")
	if got.Status != task.Running || got.PendingDone == nil || got.PendingDone.Summary != "fixed it" || got.Git != nil {
		t.Fatalf("after done in the turn: %+v", got)
	}
	if evs := events(t, r, tk.ID); evs[len(evs)-1] != "done-pending" || len(doneEvents(t, r, tk.ID)) != 0 || len(after) != 0 {
		t.Fatalf("events %q, dependents %q", evs, after)
	}

	// The runner commits before its turn ends.
	git(t, repo, "add", "notes.txt")
	git(t, repo, "commit", "-q", "-m", "feat: notes")
	prompts := fake.count("agent", "prompt")
	codexTurnEnd(t, r, tk.ID)

	got, _ = r.Store.Get(tk.ID)
	if got.Status != task.Done || got.Summary != "fixed it" || got.PendingDone != nil || got.FinishedAt.IsZero() {
		t.Fatalf("after the turn end: %+v", got)
	}
	dones := doneEvents(t, r, tk.ID)
	// The git line is read at the turn end: the commit, and no untracked file.
	if len(dones) != 1 || dones[0].Summary != "fixed it" || strings.Contains(dones[0].Git, "untracked") ||
		!strings.HasSuffix(dones[0].Git, "head "+gitHead(t, repo)) {
		t.Fatalf("done events = %+v", dones)
	}
	if !slices.Equal(after, []string{tk.ID}) {
		t.Errorf("dependents started for %q", after)
	}
	if n := fake.count("agent", "prompt"); n != prompts {
		t.Errorf("the turn end typed %d prompts", n-prompts)
	}
	// A later turn end changes nothing.
	codexTurnEnd(t, r, tk.ID)
	if len(doneEvents(t, r, tk.ID)) != 1 || len(after) != 1 {
		t.Error("a second turn end finished the done again")
	}
}

func TestSendWhileWorkingSupersedesAHeldDone(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	var after []string
	tk := codexRunning(t, r, gitRepo(t), &after)
	runnerDone(t, r, tk.ID, "first")

	fake.on("pane read", ok(codexWorkingScreen))
	if _, err := r.Send(context.Background(), tk.ID, "not done yet"); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Store.Get(tk.ID); got.QueuedSend.IsZero() {
		t.Fatal("a send to a working agent was not marked")
	}
	// The turn ends with no report after the message: the done is
	// superseded, and the turn is nudged as unreported.
	prompts := fake.count("agent", "prompt")
	codexTurnEnd(t, r, tk.ID)
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Running || !got.QueuedSend.IsZero() || got.PendingDone == nil || !got.PendingDone.Superseded || got.Unreported != 1 {
		t.Fatalf("after the turn end: %+v", got)
	}
	if evs := events(t, r, tk.ID); !slices.Contains(evs, "done-superseded") || evs[len(evs)-1] != "nudged" ||
		len(doneEvents(t, r, tk.ID)) != 0 || len(after) != 0 {
		t.Fatalf("events %q, dependents %q", evs, after)
	}
	if fake.count("agent", "prompt") != prompts+1 {
		t.Error("the unreported turn was not nudged")
	}
	// A superseded done stays held through later turn ends.
	codexTurnEnd(t, r, tk.ID)
	if got, _ := r.Store.Get(tk.ID); got.Status != task.Running || len(doneEvents(t, r, tk.ID)) != 0 {
		t.Fatalf("a superseded done was finished: %s", got.Status)
	}

	// The runner reports on the message, and that turn ends.
	runnerDone(t, r, tk.ID, "second")
	codexTurnEnd(t, r, tk.ID)
	got, _ = r.Store.Get(tk.ID)
	if dones := doneEvents(t, r, tk.ID); got.Status != task.Done || len(dones) != 1 || dones[0].Summary != "second" {
		t.Fatalf("status %s, done events %+v", got.Status, dones)
	}
	if !slices.Equal(after, []string{tk.ID}) {
		t.Errorf("dependents started for %q", after)
	}
}

func TestDoneAfterASendWhileWorkingAnswersIt(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	var after []string
	tk := codexRunning(t, r, gitRepo(t), &after)
	fake.on("agent get", ok(`{"result":{"agent":{"agent":"codex","agent_status":"working"}}}`))
	if _, err := r.Send(context.Background(), tk.ID, "also update the docs"); err != nil {
		t.Fatal(err)
	}
	// Codex reads the message within its running turn and reports on it.
	if got := runnerDone(t, r, tk.ID, "code and docs fixed"); got.Status != task.Running {
		t.Fatalf("status = %s, want running", got.Status)
	}
	codexTurnEnd(t, r, tk.ID)
	got, _ := r.Store.Get(tk.ID)
	if dones := doneEvents(t, r, tk.ID); got.Status != task.Done || !got.QueuedSend.IsZero() || len(dones) != 1 || dones[0].Summary != "code and docs fixed" {
		t.Fatalf("after the turn end: %+v, done events %+v", got, dones)
	}
	if evs := events(t, r, tk.ID); slices.Contains(evs, "done-superseded") || slices.Contains(evs, "nudged") {
		t.Errorf("events = %q", evs)
	}
	if !slices.Equal(after, []string{tk.ID}) {
		t.Errorf("dependents started for %q", after)
	}
}

func TestSendToAnIdleCodexIsNotQueued(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	var after []string
	tk := codexRunning(t, r, gitRepo(t), &after)
	fake.on("agent get", ok(`{"result":{"agent":{"agent":"codex","agent_status":"idle"}}}`))
	if _, err := r.Send(context.Background(), tk.ID, "go on"); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Store.Get(tk.ID); !got.QueuedSend.IsZero() {
		t.Error("a send to an idle agent was marked queued")
	}
}

func TestWatcherFinishesADoneWhoseTurnEndWasLost(t *testing.T) {
	defer func(age, every, recheck time.Duration) {
		pendingDoneAge, pendingDoneEvery, pendingDoneRecheck = age, every, recheck
	}(pendingDoneAge, pendingDoneEvery, pendingDoneRecheck)
	pendingDoneAge, pendingDoneEvery, pendingDoneRecheck = 0, time.Millisecond, time.Millisecond

	r, fake, _ := newTestRunner(t)
	var after []string
	tk := codexRunning(t, r, gitRepo(t), &after)
	runnerDone(t, r, tk.ID, "fixed")
	fake.on("agent get", ok(`{"result":{"agent":{"agent":"codex","agent_status":"idle"}}}`))
	fake.on("pane read", ok(codexWorkingScreen)) // herdr's status is stale

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	go func() { r.WatchPendingDone(ctx, tk.ID); close(stopped) }()
	time.Sleep(50 * time.Millisecond)
	if got, _ := r.Store.Get(tk.ID); got.Status != task.Running {
		t.Fatalf("finished while the screen showed a running turn: %s", got.Status)
	}

	fake.on("pane read", ok("› Ask Codex to do anything\n"))
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher never finished the done")
	}
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Done || len(doneEvents(t, r, tk.ID)) != 1 || !slices.Equal(after, []string{tk.ID}) {
		t.Errorf("status %s, done events %d, dependents %q", got.Status, len(doneEvents(t, r, tk.ID)), after)
	}
}

func TestWatcherLeavesAYoungDoneAlone(t *testing.T) {
	defer func(every, recheck time.Duration) {
		pendingDoneEvery, pendingDoneRecheck = every, recheck
	}(pendingDoneEvery, pendingDoneRecheck)
	pendingDoneEvery, pendingDoneRecheck = time.Millisecond, time.Millisecond

	r, fake, _ := newTestRunner(t)
	var after []string
	tk := codexRunning(t, r, gitRepo(t), &after)
	runnerDone(t, r, tk.ID, "fixed")
	fake.on("agent get", ok(`{"result":{"agent":{"agent":"codex","agent_status":"idle"}}}`))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	r.WatchPendingDone(ctx, tk.ID)
	if got, _ := r.Store.Get(tk.ID); got.Status != task.Running {
		t.Errorf("a done younger than %s was finished", pendingDoneAge)
	}
}

func TestClaudeDoneIsFinalAtOnce(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	var after []string
	r.StartAfter = func(id string) error { after = append(after, id); return nil }
	tk := running(t, r, SpawnOptions{Kind: "fix", Agent: "claude", Prompt: "x", Cwd: gitRepo(t)})
	fake.on("agent get", ok(`{"result":{"agent":{"agent":"claude","agent_status":"working"}}}`))
	if _, err := r.Send(context.Background(), tk.ID, "go on"); err != nil {
		t.Fatal(err)
	}
	got := runnerDone(t, r, tk.ID, "fixed")
	if got.Status != task.Running || got.PendingDone == nil {
		t.Fatalf("claude done: %+v", got)
	}
	claudeStop(t, r, tk.ID, `{"hook_event_name":"Stop","background_tasks":[]}`)
	got, _ = r.Store.Get(tk.ID)
	if got.Status != task.Done || got.PendingDone != nil || got.Git == nil {
		t.Fatalf("claude done at the turn end: %+v", got)
	}
	if len(doneEvents(t, r, tk.ID)) != 1 || !slices.Equal(after, []string{tk.ID}) {
		t.Errorf("done events %d, dependents %q", len(doneEvents(t, r, tk.ID)), after)
	}
}

func claudeStop(t *testing.T, r *Runner, id, payload string) {
	t.Helper()
	if _, err := r.TurnEnd(context.Background(), id, task.Claude, []byte(payload)); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeDoneWaitsForBackgroundSubagents(t *testing.T) {
	subagent := `{"hook_event_name":"Stop","background_tasks":[{"type":"subagent","status":"running"},{"type":"shell","status":"running"}]}`
	shell := `{"hook_event_name":"Stop","background_tasks":[{"type":"shell","status":"running"}]}`
	for name, tc := range map[string]struct {
		stops []string
		final bool
	}{
		"a running subagent holds it":                   {[]string{subagent}, false},
		"until a turn end with none":                    {[]string{subagent, subagent, shell}, true},
		"a workflow holds it like a subagent":           {[]string{`{"hook_event_name":"Stop","background_tasks":[{"type":"workflow","status":"running"}]}`}, false},
		"a monitor never holds it":                      {[]string{`{"hook_event_name":"Stop","background_tasks":[{"type":"monitor","status":"running"}]}`}, true},
		"a dev server never holds it":                   {[]string{shell}, true},
		"an input without a list, as from older Claude": {[]string{`{"hook_event_name":"Stop"}`}, true},
	} {
		r, fake, _ := newTestRunner(t)
		fake.on("pane read", ok(""))
		tk := running(t, r, SpawnOptions{Kind: "fix", Agent: "claude", Prompt: "x", Cwd: gitRepo(t)})
		runnerDone(t, r, tk.ID, "fixed")
		for _, stop := range tc.stops {
			claudeStop(t, r, tk.ID, stop)
		}
		got, _ := r.Store.Get(tk.ID)
		if final := got.Status == task.Done; final != tc.final {
			t.Errorf("%s: status %s, want final %v", name, got.Status, tc.final)
		}
		if !tc.final && (got.PendingDone == nil || !got.PendingDone.Subagents || got.Unreported != 0) {
			t.Errorf("%s: %+v", name, got.PendingDone)
		}
	}
}

func TestManualDoneIsFinalAtOnce(t *testing.T) {
	r, _, _ := newTestRunner(t)
	var after []string
	tk := codexRunning(t, r, gitRepo(t), &after)
	got, err := r.Report(tk.ID, ReportOptions{Status: task.Done, Summary: "recovered", Manual: true})
	if err != nil || got.Status != task.Done {
		t.Fatalf("manual done = %+v, %v", got, err)
	}
}

func TestLaterReportReplacesAHeldDone(t *testing.T) {
	r, _, _ := newTestRunner(t)
	var after []string
	tk := codexRunning(t, r, gitRepo(t), &after)
	runnerDone(t, r, tk.ID, "fixed")
	got, err := r.Report(tk.ID, ReportOptions{Status: task.NeedsHuman, Questions: []string{"Push it?"}, Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.NeedsHuman || got.PendingDone != nil {
		t.Fatalf("after needs-human: %+v", got)
	}
	codexTurnEnd(t, r, tk.ID)
	if got, _ := r.Store.Get(tk.ID); got.Status != task.NeedsHuman || len(doneEvents(t, r, tk.ID)) != 0 {
		t.Errorf("status %s after the turn end", got.Status)
	}
	if _, err := r.FinishDone(tk.ID, true, nil); err == nil || !strings.Contains(err.Error(), errNothingHeld.Error()) {
		t.Errorf("finish with nothing held = %v", err)
	}
}

func TestHeldDoneOfAGoneAgentIsFinished(t *testing.T) {
	for name, gone := range map[string]func(r *Runner, fake *fakeHerdr, id string){
		"lost check": func(r *Runner, fake *fakeHerdr, id string) {
			fake.on("agent get", herdrErr("agent_not_found"))
			tk, _ := r.Store.Get(id)
			r.CheckLost(context.Background(), tk)
		},
		"agent watch": func(r *Runner, fake *fakeHerdr, id string) {
			fake.on("agent wait", herdrErr("agent_not_found"))
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			r.WatchAgent(ctx, id)
		},
	} {
		r, fake, _ := newTestRunner(t)
		var after []string
		tk := codexRunning(t, r, gitRepo(t), &after)
		runnerDone(t, r, tk.ID, "fixed")
		gone(r, fake, tk.ID)
		got, _ := r.Store.Get(tk.ID)
		if dones := doneEvents(t, r, tk.ID); got.Status != task.Done || len(dones) != 1 || dones[0].Summary != "fixed" {
			t.Errorf("%s: status %s %q, done events %+v", name, got.Status, got.Error, dones)
		}
		if !slices.Equal(after, []string{tk.ID}) {
			t.Errorf("%s: dependents started for %q", name, after)
		}
	}
}

func TestHeldDoneAtAUsageLimitIsNotRedone(t *testing.T) {
	r, fake, launched := limitRunner(t, fixture(t, "usage-limit-codex.txt"), config.Fallback{Agent: "claude"})
	var after []string
	tk := codexRunning(t, r, gitRepo(t), &after)
	*launched = nil
	runnerDone(t, r, tk.ID, "fixed")
	fake.on("agent wait", herdrErr(herdr.CodeAgentNotRunning))
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	r.WatchAgent(ctx, tk.ID)
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Done || got.Agent != "codex" || len(*launched) != 0 || len(doneEvents(t, r, tk.ID)) != 1 {
		t.Errorf("task = %s %s, launchers %v, done events %d", got.Status, got.Agent, *launched, len(doneEvents(t, r, tk.ID)))
	}
	if slices.Contains(events(t, r, tk.ID), "agent-switched") {
		t.Error("the finished work was handed to a fallback")
	}
}

func TestSupersededDoneOfAGoneAgentFails(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	var after []string
	tk := codexRunning(t, r, gitRepo(t), &after)
	runnerDone(t, r, tk.ID, "first")
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error { t.PendingDone.Superseded = true; return nil }); err != nil {
		t.Fatal(err)
	}
	fake.on("agent get", herdrErr("agent_not_found"))
	tk, _ = r.Store.Get(tk.ID)
	r.CheckLost(context.Background(), tk)
	if got, _ := r.Store.Get(tk.ID); got.Status != task.Failed || len(doneEvents(t, r, tk.ID)) != 0 {
		t.Errorf("status %s, done events %d", got.Status, len(doneEvents(t, r, tk.ID)))
	}
}

func TestSendIsQueuedBeforeThePromptIsTyped(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	var after []string
	tk := codexRunning(t, r, gitRepo(t), &after)
	fake.on("pane read", ok(codexWorkingScreen))
	var atPrompt time.Time
	fake.on("agent prompt", func([]string) (string, error) {
		cur, _ := r.Store.Get(tk.ID)
		atPrompt = cur.QueuedSend
		return "{}", nil
	})
	if _, err := r.Send(context.Background(), tk.ID, "also the docs"); err != nil {
		t.Fatal(err)
	}
	if atPrompt.IsZero() {
		t.Error("the send was not recorded before the prompt was typed")
	}
}

func TestSendToAnIdleCodexHoldingADoneSupersedesIt(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	var after []string
	tk := codexRunning(t, r, gitRepo(t), &after)
	runnerDone(t, r, tk.ID, "first") // its turn end was lost
	fake.on("agent get", ok(`{"result":{"agent":{"agent":"codex","agent_status":"idle"}}}`))
	if _, err := r.Send(context.Background(), tk.ID, "one more thing"); err != nil {
		t.Fatal(err)
	}
	codexTurnEnd(t, r, tk.ID)
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Running || got.PendingDone == nil || !got.PendingDone.Superseded || len(doneEvents(t, r, tk.ID)) != 0 || len(after) != 0 {
		t.Fatalf("the old done answered the message: %+v", got)
	}
}

func TestFailedSendTakesItsQueuedMarkBack(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	var after []string
	tk := codexRunning(t, r, gitRepo(t), &after)
	runnerDone(t, r, tk.ID, "first")
	fake.on("agent prompt", herdrErr("pane_busy"))
	if _, err := r.Send(context.Background(), tk.ID, "lost"); err == nil {
		t.Fatal("send succeeded")
	}
	if got, _ := r.Store.Get(tk.ID); !got.QueuedSend.IsZero() {
		t.Error("a send that never went in left the held done to be superseded")
	}
}

// heldForSubagent returns a Claude runner whose done waits for a running
// subagent, with herdr seeing its agent working, as it does while Claude
// waits for one.
func heldForSubagent(t *testing.T) (*Runner, *fakeHerdr, *task.Task) {
	t.Helper()
	r, fake, _ := newTestRunner(t)
	return r, fake, holdForSubagent(t, r, fake)
}

// holdForSubagent runs a Claude task on r whose done is held for a running
// subagent, with herdr reporting it working.
func holdForSubagent(t *testing.T, r *Runner, fake *fakeHerdr) *task.Task {
	t.Helper()
	fake.on("pane read", ok(""))
	fake.on("agent get", ok(`{"result":{"agent":{"agent":"claude","agent_status":"working"}}}`))
	tk := running(t, r, SpawnOptions{Kind: "fix", Agent: "claude", Prompt: "x", Cwd: gitRepo(t)})
	runnerDone(t, r, tk.ID, "fixed")
	claudeStop(t, r, tk.ID, subagentStop)
	return tk
}

// subagentWakes submits the note Claude gets when a subagent ends, which
// starts a turn.
func subagentWakes(t *testing.T, r *Runner, id string) {
	t.Helper()
	gate, _ := harness.For(task.Claude)
	if _, err := r.GatePrompt(id, gate, []byte(`{"prompt":"<task-notification>agent done</task-notification>"}`)); err != nil {
		t.Fatal(err)
	}
}

const subagentStop = `{"hook_event_name":"Stop","background_tasks":[{"type":"subagent","status":"running"}]}`

func watchBriefly(r *Runner, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	r.WatchPendingDone(ctx, id)
}

// shortWatch makes the watcher fast, with a done held for subagents until
// ceiling, and restores it when t ends.
func shortWatch(t *testing.T, ceiling time.Duration) {
	t.Helper()
	every, recheck, age, old := pendingDoneEvery, pendingDoneRecheck, pendingDoneAge, pendingDoneCeiling
	t.Cleanup(func() {
		pendingDoneEvery, pendingDoneRecheck, pendingDoneAge, pendingDoneCeiling = every, recheck, age, old
	})
	pendingDoneEvery, pendingDoneRecheck, pendingDoneAge, pendingDoneCeiling = time.Millisecond, time.Millisecond, 0, ceiling
}

func TestWatcherFinishesADoneHeldForAHungSubagentAtTheCeiling(t *testing.T) {
	shortWatch(t, time.Hour)
	r, _, tk := heldForSubagent(t)
	watchBriefly(r, tk.ID)
	if got, _ := r.Store.Get(tk.ID); got.Status != task.Running || got.PendingDone == nil {
		t.Fatalf("a done waiting for a subagent was finished before the ceiling: %s", got.Status)
	}

	pendingDoneCeiling = 0
	watchBriefly(r, tk.ID)
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Done || len(doneEvents(t, r, tk.ID)) != 1 || countEvents(t, r, tk.ID, "done-hold-timeout") != 1 {
		t.Errorf("status %s, done events %d, timeouts %d", got.Status, len(doneEvents(t, r, tk.ID)), countEvents(t, r, tk.ID, "done-hold-timeout"))
	}
}

func TestTurnInProgressKeepsADoneHeldPastTheCeiling(t *testing.T) {
	shortWatch(t, 0)
	r, _, tk := heldForSubagent(t)
	// The subagent's result wakes Claude for a turn.
	subagentWakes(t, r, tk.ID)
	watchBriefly(r, tk.ID)
	if got, _ := r.Store.Get(tk.ID); got.Status != task.Running || countEvents(t, r, tk.ID, "done-hold-timeout") != 0 {
		t.Fatalf("a done was finished while a turn ran: %s", got.Status)
	}

	// The turn ends with another subagent running, which hangs.
	claudeStop(t, r, tk.ID, subagentStop)
	watchBriefly(r, tk.ID)
	if got, _ := r.Store.Get(tk.ID); got.Status != task.Done || countEvents(t, r, tk.ID, "done-hold-timeout") != 1 {
		t.Errorf("status %s once the turn ended", got.Status)
	}
}

func TestWatcherNeverFinishesANewerDoneThanTheOneItJudged(t *testing.T) {
	shortWatch(t, 0)
	r, _, tk := heldForSubagent(t)
	judged, _ := r.Store.Get(tk.ID)
	runnerDone(t, r, tk.ID, "newer")
	r.finishStaleDone(context.Background(), judged)
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Running || got.PendingDone == nil || got.PendingDone.Summary != "newer" {
		t.Fatalf("the newer done was finished: %s %+v", got.Status, got.PendingDone)
	}
	if n := countEvents(t, r, tk.ID, "done-hold-timeout"); n != 0 || len(doneEvents(t, r, tk.ID)) != 0 {
		t.Errorf("timeouts %d, done events %d for a done the watcher never finished", n, len(doneEvents(t, r, tk.ID)))
	}
}

func TestSupersededDoneHeldForSubagentsIsFinishedOnceTheAgentSettles(t *testing.T) {
	shortWatch(t, time.Hour)
	r, fake, tk := heldForSubagent(t)
	// A message sent after the done superseded it at a turn end with no
	// subagent left, so its subagent flag is stale.
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error {
		t.PendingDone.Superseded = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fake.on("agent get", ok(`{"result":{"agent":{"agent":"claude","agent_status":"idle"}}}`))
	watchBriefly(r, tk.ID)
	if got, _ := r.Store.Get(tk.ID); got.Status != task.Done || countEvents(t, r, tk.ID, "done-hold-timeout") != 0 {
		t.Errorf("status %s, timeouts %d", got.Status, countEvents(t, r, tk.ID, "done-hold-timeout"))
	}
}

func TestInterruptedTurnFinishesAHeldDoneOnceTheAgentSettles(t *testing.T) {
	shortWatch(t, 0)
	r, fake, tk := heldForSubagent(t)
	subagentWakes(t, r, tk.ID)
	// esc ends the turn, and no Stop reports it.
	fake.on("agent get", ok(`{"result":{"agent":{"agent":"claude","agent_status":"idle"}}}`))
	watchBriefly(r, tk.ID)
	if got, _ := r.Store.Get(tk.ID); got.Status != task.Done || countEvents(t, r, tk.ID, "done-hold-timeout") != 1 {
		t.Errorf("status %s, timeouts %d", got.Status, countEvents(t, r, tk.ID, "done-hold-timeout"))
	}
}

func TestWatcherNeverFinishesADoneWhoseTurnStartedAfterItsRead(t *testing.T) {
	for _, superseded := range []bool{false, true} {
		shortWatch(t, 0)
		r, fake, tk := heldForSubagent(t)
		if superseded {
			// The 2-minute branch, which waits for the agent to settle.
			fake.on("agent get", ok(`{"result":{"agent":{"agent":"claude","agent_status":"idle"}}}`))
			if _, err := r.Store.Update(tk.ID, func(t *task.Task) error {
				t.PendingDone.Superseded = true
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		judged, _ := r.Store.Get(tk.ID)
		subagentWakes(t, r, tk.ID)
		r.finishStaleDone(context.Background(), judged)
		if got, _ := r.Store.Get(tk.ID); got.Status != task.Running || countEvents(t, r, tk.ID, "done-hold-timeout") != 0 {
			t.Errorf("superseded %v: a done was finished while a turn ran: %s", superseded, got.Status)
		}
	}
}

func TestLockedRunnersTurnFromTheKitKeepsADoneHeldPastTheCeiling(t *testing.T) {
	shortWatch(t, 0)
	r, fake := lockedRunner(t)
	tk := holdForSubagent(t, r, fake)
	if !InputLocked(tk) {
		t.Fatal("the runner's input is open")
	}
	// A prompt the kit typed, which takes the token the kit left.
	if err := r.Store.AddPromptToken(tk.ID); err != nil {
		t.Fatal(err)
	}
	gate, _ := harness.For(task.Claude)
	if block, err := r.GatePrompt(tk.ID, gate, []byte(`{"prompt":"next step"}`)); block != "" || err != nil {
		t.Fatalf("the kit's prompt was blocked: %q, %v", block, err)
	}
	watchBriefly(r, tk.ID)
	if got, _ := r.Store.Get(tk.ID); got.Status != task.Running || countEvents(t, r, tk.ID, "done-hold-timeout") != 0 {
		t.Fatalf("a done was finished while the kit's turn ran: %s", got.Status)
	}

	claudeStop(t, r, tk.ID, `{"hook_event_name":"Stop"}`)
	if got, _ := r.Store.Get(tk.ID); got.Status != task.Done {
		t.Errorf("status %s once the turn ended", got.Status)
	}
}
