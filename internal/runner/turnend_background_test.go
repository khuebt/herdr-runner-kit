package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/harness"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func claudeAdapter(t *testing.T) harness.Adapter {
	t.Helper()
	a, err := harness.For(task.Claude)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// claudeTurnEnd ends one turn of a fresh Claude runner whose pane shows
// screen, and returns the nudge, empty when there is none.
func claudeTurnEnd(t *testing.T, payload []byte, screen string) string {
	t.Helper()
	r, fake, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	fake.on("pane read", ok(screen))
	block, err := r.TurnEnd(context.Background(), tk.ID, task.Claude, payload)
	if err != nil {
		t.Fatal(err)
	}
	return block
}

func TestTurnEndTrustsTheStopInputOverTheModeLine(t *testing.T) {
	running := []byte(harnessFixture(t, "stop-background-running-claude.json"))
	none := []byte(harnessFixture(t, "stop-background-none-claude.json"))
	// Right after the launch the mode line draws no count for the agents.
	early := harnessFixture(t, "background-agents-claude-1.txt")
	shells := harnessFixture(t, "background-agents-claude-2.txt")
	if busy, _ := claudeAdapter(t).WorksInBackground(func(int) (string, error) { return early, nil }); busy {
		t.Fatal("the early screen shows a count; the fixture no longer shows the miss")
	}
	if block := claudeTurnEnd(t, running, early); block != "" {
		t.Errorf("nudged with 2 agents running: %q", block)
	}
	if block := claudeTurnEnd(t, none, shells); block == "" {
		t.Error("no nudge once every background task finished")
	}
}

func TestTurnEndFallsBackToTheModeLine(t *testing.T) {
	shells := harnessFixture(t, "background-agents-claude-2.txt")
	early := harnessFixture(t, "background-agents-claude-1.txt")
	for name, payload := range map[string][]byte{"no list": []byte(`{"hook_event_name":"Stop"}`), "malformed": []byte(`{"background_tasks": 3`)} {
		if block := claudeTurnEnd(t, payload, shells); block != "" {
			t.Errorf("%s: nudged while the mode line counts shells: %q", name, block)
		}
		if block := claudeTurnEnd(t, payload, early); block == "" {
			t.Errorf("%s: no nudge without a count", name)
		}
	}
}

func TestNudgeNeverAsksToWaitInTheTurn(t *testing.T) {
	r, _, _ := newTestRunner(t)
	nudge := r.nudgeText(task.Claude)
	if strings.Contains(nudge, "wait for it") || !strings.Contains(nudge, "Never start sleep or heartbeat commands") ||
		!strings.Contains(nudge, "end your turn now without reporting: you are woken when they finish") {
		t.Errorf("nudge = %q", nudge)
	}
}

// Only Claude is woken when its background work ends: every other harness
// is told to wait for it in the same turn.
func TestNudgeAsksOtherHarnessesToWaitInTheTurn(t *testing.T) {
	r, _, _ := newTestRunner(t)
	for _, h := range []task.Harness{task.Codex, task.Pi, task.Omp} {
		for _, nudge := range []string{r.nudgeText(h), r.idleNudgeText(h, 10)} {
			if strings.Contains(nudge, "you are woken") || strings.Contains(nudge, "end your turn now") ||
				!strings.Contains(nudge, "wait for them in this same turn") || !strings.Contains(nudge, "Never end your turn to wait") ||
				!strings.Contains(nudge, r.Bin+" report done") {
				t.Errorf("%s nudge = %q", h, nudge)
			}
		}
	}
}
