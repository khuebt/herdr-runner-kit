package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/config"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// The agents of a pi and an omp runner.
var piAgents = map[task.Harness]string{
	task.Pi:  "pi:openai-codex/gpt-6-luna:low",
	task.Omp: "omp:openai-codex/gpt-6-luna:low",
}

// piTurnEnd ends one turn of task id with the named fixture's payload.
func piTurnEnd(t *testing.T, r *Runner, id string, h task.Harness, name string) {
	t.Helper()
	if _, err := r.TurnEnd(context.Background(), id, h, []byte(fixture(t, name))); err != nil {
		t.Fatalf("TurnEnd(%s): %v", name, err)
	}
}

func TestPiAbortedTurnIsIgnored(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Agent: piAgents[task.Pi], Prompt: "x", Cwd: plainDir(t)})
	before := len(events(t, r, tk.ID))
	piTurnEnd(t, r, tk.ID, task.Pi, "turnend-pi-aborted.json")
	got, _ := r.Store.Get(tk.ID)
	if got.Unreported != 0 || len(events(t, r, tk.ID)) != before || fake.count("agent", "prompt") != 0 {
		t.Errorf("unreported %d, events %v, prompts %d", got.Unreported, events(t, r, tk.ID), fake.count("agent", "prompt"))
	}
}

func TestPiUsageLimitSwitchesAgent(t *testing.T) {
	r, _, launched := limitRunner(t, "", config.Fallback{Agent: "codex"})
	tk := running(t, r, SpawnOptions{Kind: "research", Agent: piAgents[task.Pi], Prompt: "x", Cwd: plainDir(t)})
	*launched = nil
	piTurnEnd(t, r, tk.ID, task.Pi, "turnend-pi-usage-limit.json")
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Starting || got.Agent != "codex" || len(*launched) != 1 {
		t.Errorf("task = %s %s, launchers %v", got.Status, got.Agent, *launched)
	}
	if ev := lastEvent(t, r); ev.Type != "agent-switched" || !strings.HasPrefix(ev.Summary, piAgents[task.Pi]+" -> codex") {
		t.Errorf("last event = %+v", ev)
	}
}

func TestPiUsageLimitWithoutFallbackFails(t *testing.T) {
	r, _, launched := limitRunner(t, "")
	tk := running(t, r, SpawnOptions{Kind: "research", Agent: piAgents[task.Pi], Prompt: "x", Cwd: plainDir(t)})
	*launched = nil
	piTurnEnd(t, r, tk.ID, task.Pi, "turnend-pi-usage-limit.json")
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Failed || !strings.HasPrefix(got.Error, "usage limit reached for "+piAgents[task.Pi]) || len(*launched) != 0 {
		t.Errorf("task = %s %q, launchers %v", got.Status, got.Error, *launched)
	}
}

func TestPiTurnWithoutReportIsNudgedInThePane(t *testing.T) {
	for h, payload := range map[task.Harness]string{task.Pi: "turnend-pi-error.json", task.Omp: "turnend-omp-completed.json"} {
		r, fake, _ := newTestRunner(t)
		tk := running(t, r, SpawnOptions{Agent: piAgents[h], Prompt: "x", Cwd: plainDir(t)})
		block, err := r.TurnEnd(context.Background(), tk.ID, h, []byte(fixture(t, payload)))
		if err != nil {
			t.Fatal(err)
		}
		// The nudge is typed: the extension's hook takes no reply.
		if p := lastCall(fake, "agent", "prompt"); p == nil || p[2] != tk.Herdr.PaneID || p[3] != r.nudgeText() || block != p[3] {
			t.Errorf("%s: prompt = %q, block %q", h, p, block)
		}
		if ev := lastEvent(t, r); ev.Type != "nudged" {
			t.Errorf("%s: event = %+v", h, ev)
		}
		if _, err := r.TurnEnd(context.Background(), tk.ID, h, []byte(fixture(t, payload))); err != nil {
			t.Fatal(err)
		}
		if ev := lastEvent(t, r); ev.Type != "turn-ended-unreported" || fake.count("agent", "prompt") != 1 {
			t.Errorf("%s: second turn: event %+v, prompts %d", h, ev, fake.count("agent", "prompt"))
		}
	}
}

func TestPiHighContextAsksForAHandoffOnce(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	r.Config.ContextHandoffAt = 15
	tk := running(t, r, SpawnOptions{Agent: piAgents[task.Pi], Prompt: "x", Cwd: plainDir(t)})
	piTurnEnd(t, r, tk.ID, task.Pi, "turnend-pi-context-high.json")
	if p := lastCall(fake, "agent", "prompt"); p == nil || p[3] != r.handoffRequest(tk) {
		t.Fatalf("prompt = %q", p)
	}
	if ev := lastEvent(t, r); ev.Type != "context-low" || !strings.HasPrefix(ev.Summary, "10% of its context left") {
		t.Errorf("event = %+v", ev)
	}
	// Asked once: the next unreported turn is nudged with the pending request.
	piTurnEnd(t, r, tk.ID, task.Pi, "turnend-pi-context-high.json")
	if countEvents(t, r, tk.ID, "context-low") != 1 || countEvents(t, r, tk.ID, "nudged") != 1 {
		t.Errorf("events = %v", events(t, r, tk.ID))
	}
}

func TestPiTurnOfAnEndedAttemptIsIgnored(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := handedOff(t, r, SpawnOptions{Agent: piAgents[task.Omp], Prompt: "x", Cwd: plainDir(t)})
	before := len(events(t, r, tk.ID))
	piTurnEnd(t, r, tk.ID, task.Omp, "turnend-omp-completed.json")
	got, _ := r.Store.Get(tk.ID)
	if got.Unreported != 0 || len(events(t, r, tk.ID)) != before || fake.count("agent", "prompt") != 0 {
		t.Errorf("attempt 1's turn counted: unreported %d, events %v", got.Unreported, events(t, r, tk.ID))
	}
}

// piCompact runs the context hook of task id with the named fixture and
// fails when the hook prints anything.
func piCompact(t *testing.T, r *Runner, id string, h task.Harness, name string) {
	t.Helper()
	out, err := r.ContextHook(context.Background(), id, h, []byte(fixture(t, name)))
	if err != nil || out != "" {
		t.Fatalf("ContextHook(%s) = %q, %v", name, out, err)
	}
}

func TestPiCompactionAsksOnceAndManualNever(t *testing.T) {
	for h, agent := range piAgents {
		r, fake, _ := newTestRunner(t)
		tk := running(t, r, SpawnOptions{Agent: agent, Prompt: "x", Cwd: plainDir(t)})
		for range 2 {
			piCompact(t, r, tk.ID, h, "precompact-auto.json")
			piCompact(t, r, tk.ID, h, "sessionstart-compact.json")
		}
		// No session context to carry it, so the request is typed.
		if fake.count("agent", "prompt") != 1 || lastCall(fake, "agent", "prompt")[3] != r.handoffRequest(tk) {
			t.Errorf("%s: prompts = %d", h, fake.count("agent", "prompt"))
		}
		if countEvents(t, r, tk.ID, "context-low") != 2 || r.Compactions(tk.ID) != 2 {
			t.Errorf("%s: events = %v, compactions %d", h, events(t, r, tk.ID), r.Compactions(tk.ID))
		}

		manual := running(t, r, SpawnOptions{Agent: agent, Prompt: "y", Cwd: plainDir(t)})
		piCompact(t, r, manual.ID, h, "precompact-manual.json")
		piCompact(t, r, manual.ID, h, "sessionstart-compact.json")
		if countEvents(t, r, manual.ID, "context-low") != 0 || r.Compactions(manual.ID) != 1 {
			t.Errorf("%s: manual events = %v", h, events(t, r, manual.ID))
		}
	}
}
