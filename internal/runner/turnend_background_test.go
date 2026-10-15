package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

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
	if hasBackgroundWork(early) {
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
	nudge := r.nudgeText()
	if strings.Contains(nudge, "wait for it") || !strings.Contains(nudge, "Never start sleep or heartbeat commands") ||
		!strings.Contains(nudge, "end your turn now without reporting: you are woken when they finish") {
		t.Errorf("nudge = %q", nudge)
	}
}
