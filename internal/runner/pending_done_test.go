package runner

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
	if got.Status != task.Done || got.PendingDone != nil || !got.QueuedSend.IsZero() || got.Git == nil {
		t.Fatalf("claude done: %+v", got)
	}
	if len(doneEvents(t, r, tk.ID)) != 1 || !slices.Equal(after, []string{tk.ID}) {
		t.Errorf("done events %d, dependents %q", len(doneEvents(t, r, tk.ID)), after)
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
	if _, err := r.FinishDone(tk.ID, true); err == nil || !strings.Contains(err.Error(), errNothingHeld.Error()) {
		t.Errorf("finish with nothing held = %v", err)
	}
}
