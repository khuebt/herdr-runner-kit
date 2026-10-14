package runner

import (
	"context"
	"strings"
	"testing"

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
