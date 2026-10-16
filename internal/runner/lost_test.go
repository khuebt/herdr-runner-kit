package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// waitingRunner returns a task waiting on a question, with a queued
// dependent, whose agent is named the way spawn names it.
func waitingRunner(t *testing.T) (*Runner, *fakeHerdr, *task.Task, *task.Task) {
	t.Helper()
	r, fake, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error {
		t.Herdr.AgentName = "hrk-" + t.ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Report(tk.ID, ReportOptions{Status: task.NeedsHuman, Questions: []string{"Drop it?"}}); err != nil {
		t.Fatal(err)
	}
	child := &task.Task{Kind: "fix", Agent: "claude", Prompt: "y", Owner: tk.Owner, Gate: task.GateAuto, Status: task.Queued, After: []string{tk.ID}}
	if err := r.Store.Create(child); err != nil {
		t.Fatal(err)
	}
	tk, err := r.Store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	return r, fake, tk, child
}

func TestAnswerToALostRunnerSaysItIsGone(t *testing.T) {
	r, fake, tk, child := waitingRunner(t)
	fake.on("agent get", herdrErr("agent_not_found"))
	fake.on("pane read", herdrErr("pane_not_found"))
	_, err := r.Answer(context.Background(), tk.ID, "yes")
	if err == nil || !strings.Contains(err.Error(), "runner is gone (runner lost: herdr pane gone)") || !strings.Contains(err.Error(), "show "+tk.ID) {
		t.Fatalf("err = %v", err)
	}
	if fake.count("agent", "prompt") != 0 {
		t.Error("the answer was typed into a lost pane")
	}
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Failed || got.Error != "runner lost: herdr pane gone" {
		t.Errorf("task = %s %q", got.Status, got.Error)
	}
	if dep, _ := r.Store.Get(child.ID); dep.Status != task.Failed {
		t.Errorf("dependent = %s", dep.Status)
	}
	if _, err := r.Send(context.Background(), tk.ID, "more"); err == nil || !strings.Contains(err.Error(), "runner is gone") {
		t.Errorf("send = %v", err)
	}
}

func TestPaneHoldingAnotherAgentIsLost(t *testing.T) {
	r, fake, tk, _ := waitingRunner(t)
	// A restart gave the pane id to another agent.
	fake.on("agent get", ok(`{"result":{"agent":{"agent":"claude","agent_status":"idle","name":"hrk-t99"}}}`))
	r.CheckLost(context.Background(), tk)
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Failed || !strings.Contains(got.Error, "now holds another agent") {
		t.Errorf("task = %s %q", got.Status, got.Error)
	}
}

func TestLostCheckKeepsALiveOrUnreachableRunner(t *testing.T) {
	for name, get := range map[string]func([]string) (string, error){
		"same agent":      ok(`{"result":{"agent":{"agent":"claude","agent_status":"idle","name":"hrk-t1"}}}`),
		"unnamed agent":   ok(`{"result":{"agent":{"agent":"claude","agent_status":"idle"}}}`),
		"herdr is down":   func([]string) (string, error) { return "", context.DeadlineExceeded },
		"herdr timed out": herdrErr("timeout"),
	} {
		r, fake, tk, _ := waitingRunner(t)
		fake.on("agent get", get)
		r.CheckLost(context.Background(), tk)
		if got, _ := r.Store.Get(tk.ID); got.Status != task.NeedsHuman {
			t.Errorf("%s: task = %s %q", name, got.Status, got.Error)
		}
	}
}

func TestAgentGoneFromItsPaneKeepsTheExitReason(t *testing.T) {
	r, fake, tk, _ := waitingRunner(t)
	fake.on("agent get", herdrErr("agent_not_found"))
	r.CheckLost(context.Background(), tk)
	if got, _ := r.Store.Get(tk.ID); got.Status != task.Failed || got.Error != "the agent exited without reporting" {
		t.Errorf("task = %s %q", got.Status, got.Error)
	}
}

// Agent states herdr reports for a runner's pane: its agent exited and left
// the shell, keeping the agent's name; a restored agent herdr has yet to
// detect; a live agent.
const (
	shellLeft = `{"result":{"agent":{"agent":null,"agent_status":"idle","name":"hrk-t1"}}}`
	detecting = `{"result":{"agent":{"agent":null,"agent_status":"unknown","name":"hrk-t1"}}}`
	liveAgent = `{"result":{"agent":{"agent":"claude","agent_status":"idle","name":"hrk-t1"}}}`
)

// agentGets answers herdr's agent get with outs in turn, repeating the last.
func agentGets(outs ...string) func([]string) (string, error) {
	n := 0
	return func([]string) (string, error) {
		out := outs[min(n, len(outs)-1)]
		n++
		return out, nil
	}
}

// failedEvents counts the task's failed events.
func failedEvents(t *testing.T, r *Runner, id string) int {
	t.Helper()
	all, _, err := r.Store.ReadEventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range all {
		if ev.Task == id && ev.Type == "failed" {
			n++
		}
	}
	return n
}

func TestAgentThatDidNotResumeFailsWithItsLastLine(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	tk, err := r.Store.Update(tk.ID, func(t *task.Task) error { t.Herdr.AgentName = "hrk-" + t.ID; return nil })
	if err != nil {
		t.Fatal(err)
	}
	fake.on("agent get", agentGets(shellLeft))
	fake.on("pane read", ok("$ claude --resume abc\nNo conversation found with session ID: abc\n\n❯ \n"))
	var slept []time.Duration
	r.Sleep = func(d time.Duration) { slept = append(slept, d) }
	r.CheckLost(context.Background(), tk)
	got, _ := r.Store.Get(tk.ID)
	if want := "the agent exited without reporting: No conversation found with session ID: abc"; got.Status != task.Failed || got.Error != want {
		t.Errorf("task = %s %q, want failed %q", got.Status, got.Error, want)
	}
	if n := failedEvents(t, r, tk.ID); n != 1 {
		t.Errorf("failed events = %d, want 1", n)
	}
	if len(slept) != 1 || slept[0] != goneConfirm {
		t.Errorf("slept %v, want one wait of %v", slept, goneConfirm)
	}
}

func TestWaitingRunnerWhoseAgentLeftItsShellFails(t *testing.T) {
	r, fake, tk, child := waitingRunner(t)
	fake.on("agent get", agentGets(shellLeft))
	r.CheckLost(context.Background(), tk)
	if got, _ := r.Store.Get(tk.ID); got.Status != task.Failed || got.Error != "the agent exited without reporting" {
		t.Errorf("task = %s %q", got.Status, got.Error)
	}
	if dep, _ := r.Store.Get(child.ID); dep.Status != task.Failed {
		t.Errorf("dependent = %s", dep.Status)
	}
}

func TestAgentBackWithinTheConfirmationKeepsTheRunner(t *testing.T) {
	r, fake, tk, _ := waitingRunner(t)
	fake.on("agent get", agentGets(shellLeft, liveAgent))
	r.CheckLost(context.Background(), tk)
	if got, _ := r.Store.Get(tk.ID); got.Status != task.NeedsHuman {
		t.Errorf("task = %s %q", got.Status, got.Error)
	}
}

func TestRestoredAgentHerdrHasYetToDetectIsKept(t *testing.T) {
	r, fake, tk, _ := waitingRunner(t)
	fake.on("agent get", agentGets(detecting))
	r.CheckLost(context.Background(), tk)
	if got, _ := r.Store.Get(tk.ID); got.Status != task.NeedsHuman {
		t.Errorf("task = %s %q", got.Status, got.Error)
	}
}

func TestDetectionStillSettlingAfterTheConfirmationKeepsTheRunner(t *testing.T) {
	r, fake, tk, _ := waitingRunner(t)
	fake.on("agent get", agentGets(shellLeft, detecting))
	r.CheckLost(context.Background(), tk)
	if got, _ := r.Store.Get(tk.ID); got.Status != task.NeedsHuman {
		t.Errorf("task = %s %q", got.Status, got.Error)
	}
}

func TestRelaunchDuringTheConfirmationKeepsTheRunner(t *testing.T) {
	for name, change := range map[string]func(*task.Task){
		"starting":    func(t *task.Task) { t.Status = task.Starting },
		"new attempt": func(t *task.Task) { t.Attempt = 2 },
	} {
		r, fake, tk, _ := waitingRunner(t)
		fake.on("agent get", agentGets(shellLeft))
		r.Sleep = func(time.Duration) {
			if _, err := r.Store.Update(tk.ID, func(t *task.Task) error { change(t); return nil }); err != nil {
				t.Fatal(err)
			}
		}
		r.CheckLost(context.Background(), tk)
		if got, _ := r.Store.Get(tk.ID); got.Status == task.Failed {
			t.Errorf("%s: task failed: %q", name, got.Error)
		}
	}
}
