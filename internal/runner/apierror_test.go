package runner

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

const apiFailure = `{"hook_event_name":"StopFailure","error":"server_error","last_assistant_message":"API Error: Connection lost mid-response."}`

// apiRetryRunner returns a runner on a running Claude task, and the ids its
// StartAPIRetry was called with.
func apiRetryRunner(t *testing.T) (*Runner, *fakeHerdr, *task.Task, *[]string) {
	t.Helper()
	r, fake, _ := limitRunner(t, "❯ \n")
	tk := running(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: plainDir(t)})
	started := &[]string{}
	r.StartAPIRetry = func(id string) error {
		*started = append(*started, id)
		return nil
	}
	return r, fake, tk, started
}

func turnEnd(t *testing.T, r *Runner, id, payload string) {
	t.Helper()
	if _, err := r.TurnEnd(context.Background(), id, task.Claude, []byte(payload)); err != nil {
		t.Fatal(err)
	}
}

func TestAPIErrorContinuesOnceThenIsNews(t *testing.T) {
	r, _, tk, started := apiRetryRunner(t)
	turnEnd(t, r, tk.ID, apiFailure)
	got, _ := r.Store.Get(tk.ID)
	if got.APIErrors != 1 || got.Status != task.Running || got.Unreported != 0 {
		t.Errorf("task = %s apiErrors %d unreported %d", got.Status, got.APIErrors, got.Unreported)
	}
	if !slices.Equal(*started, []string{tk.ID}) || countEvents(t, r, tk.ID, "api-retry") != 1 || countEvents(t, r, tk.ID, "nudged") != 0 {
		t.Errorf("started %v, events %v", *started, events(t, r, tk.ID))
	}
	turnEnd(t, r, tk.ID, apiFailure)
	if len(*started) != 1 || countEvents(t, r, tk.ID, "api-error") != 1 {
		t.Errorf("after the second: started %v, events %v", *started, events(t, r, tk.ID))
	}
	turnEnd(t, r, tk.ID, apiFailure)
	if len(*started) != 1 || countEvents(t, r, tk.ID, "api-error") != 1 || countEvents(t, r, tk.ID, "api-retry") != 1 {
		t.Errorf("after the third: started %v, events %v", *started, events(t, r, tk.ID))
	}
}

func TestAPIErrorCountResets(t *testing.T) {
	const stop = `{"hook_event_name":"Stop"}`
	tests := []struct {
		name  string
		reset func(*Runner, *task.Task)
	}{
		{"normal turn end", func(r *Runner, tk *task.Task) { turnEnd(t, r, tk.ID, stop) }},
		{"send", func(r *Runner, tk *task.Task) {
			if _, err := r.Send(context.Background(), tk.ID, "go on"); err != nil {
				t.Fatal(err)
			}
		}},
		{"needs-human", func(r *Runner, tk *task.Task) {
			if _, err := r.Report(tk.ID, ReportOptions{Status: task.NeedsHuman, Questions: []string{"go on? (recommended: yes)"}}); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		r, fake, tk, started := apiRetryRunner(t)
		fake.on("agent prompt", ok("{}"))
		turnEnd(t, r, tk.ID, apiFailure)
		tt.reset(r, tk)
		if got, _ := r.Store.Get(tk.ID); got.APIErrors != 0 {
			t.Errorf("%s: apiErrors = %d", tt.name, got.APIErrors)
			continue
		}
		if got, _ := r.Store.Get(tk.ID); got.Status == task.NeedsHuman {
			if _, err := r.Answer(context.Background(), tk.ID, "yes"); err != nil {
				t.Fatal(err)
			}
		}
		turnEnd(t, r, tk.ID, apiFailure)
		if len(*started) != 2 || countEvents(t, r, tk.ID, "api-retry") != 2 || countEvents(t, r, tk.ID, "api-error") != 0 {
			t.Errorf("%s: started %v, events %v", tt.name, *started, events(t, r, tk.ID))
		}
	}
}

func TestContinueAfterAPIError(t *testing.T) {
	r, fake, tk, _ := apiRetryRunner(t)
	turnEnd(t, r, tk.ID, apiFailure)
	var slept time.Duration
	r.Sleep = func(d time.Duration) { slept = d }
	fake.on("agent get", ok(`{"result":{"agent":{"agent":"claude","agent_status":"idle"}}}`))
	if err := r.ContinueAfterAPIError(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	if slept < apiRetryFloor || slept >= apiRetryCeil {
		t.Errorf("slept %s, want [%s, %s)", slept, apiRetryFloor, apiRetryCeil)
	}
	if fake.count("agent", "prompt") != 1 {
		t.Fatalf("prompts = %d, want 1", fake.count("agent", "prompt"))
	}
	if p := lastCall(fake, "agent", "prompt"); p[2] != tk.Herdr.PaneID || p[3] != apiContinue {
		t.Errorf("prompt = %v", p)
	}
	if countEvents(t, r, tk.ID, "api-continued") != 1 {
		t.Errorf("events = %v", events(t, r, tk.ID))
	}
}

func TestContinueAfterAPIErrorSkips(t *testing.T) {
	tests := []struct {
		name  string
		agent string
		undo  func(*Runner, *task.Task)
	}{
		{"working", "working", func(*Runner, *task.Task) {}},
		{"done", "idle", func(r *Runner, tk *task.Task) {
			if _, _, err := r.Store.Transition(tk.ID, task.Done, nil, func(*task.Task) {}); err != nil {
				t.Fatal(err)
			}
		}},
		{"counter reset", "idle", func(r *Runner, tk *task.Task) {
			if _, err := r.Store.Update(tk.ID, func(t *task.Task) error { t.APIErrors = 0; return nil }); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		r, fake, tk, _ := apiRetryRunner(t)
		turnEnd(t, r, tk.ID, apiFailure)
		tt.undo(r, tk)
		fake.on("agent get", ok(`{"result":{"agent":{"agent":"claude","agent_status":"`+tt.agent+`"}}}`))
		if err := r.ContinueAfterAPIError(context.Background(), tk.ID); err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if fake.count("agent", "prompt") != 0 || countEvents(t, r, tk.ID, "api-continued") != 0 {
			t.Errorf("%s: prompts %d, events %v", tt.name, fake.count("agent", "prompt"), events(t, r, tk.ID))
		}
	}
}

// A send that lands after the retry checked the runner, but before it
// typed, means no continue is typed.
func TestContinueAfterAPIErrorYieldsToASend(t *testing.T) {
	r, fake, tk, _ := apiRetryRunner(t)
	turnEnd(t, r, tk.ID, apiFailure)
	r.Sleep = func(time.Duration) {}
	fake.on("agent prompt", ok("{}"))
	sent := false
	fake.on("agent get", func([]string) (string, error) {
		if !sent {
			sent = true
			if _, err := r.Send(context.Background(), tk.ID, "go on"); err != nil {
				t.Error(err)
			}
		}
		return `{"result":{"agent":{"agent":"claude","agent_status":"idle"}}}`, nil
	})
	if err := r.ContinueAfterAPIError(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	if fake.count("agent", "prompt") != 1 || lastCall(fake, "agent", "prompt")[3] != "go on" || countEvents(t, r, tk.ID, "api-continued") != 0 {
		t.Errorf("prompts %d, last %v, events %v", fake.count("agent", "prompt"), lastCall(fake, "agent", "prompt"), events(t, r, tk.ID))
	}
}

// A confirmed usage limit stays on the quota fallback path and never starts
// an API retry.
func TestUsageLimitStopFailureDoesNotRetry(t *testing.T) {
	r, _, tk, started := apiRetryRunner(t)
	turnEnd(t, r, tk.ID, fixture(t, "stop-failure-claude.json"))
	if len(*started) != 0 || countEvents(t, r, tk.ID, "api-retry") != 0 {
		t.Errorf("started %v, events %v", *started, events(t, r, tk.ID))
	}
}
