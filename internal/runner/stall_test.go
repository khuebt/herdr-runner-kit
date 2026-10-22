package runner

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// stallRunner returns a running Claude task whose agent is working and
// whose pane shows screen until the test changes it.
func stallRunner(t *testing.T, screen string) (*Runner, *fakeHerdr, *task.Task, *atomic.Value) {
	t.Helper()
	r, fake, _ := newTestRunner(t)
	r.Config.StallAfter = 10 * time.Minute
	tk := running(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: plainDir(t)})
	var shown atomic.Value
	shown.Store(screen)
	fake.on("pane read", func([]string) (string, error) { return shown.Load().(string), nil })
	fake.on("agent get", ok(`{"result":{"agent":{"agent":"claude","agent_status":"working","revision":4,"state_change_seq":385}}}`))
	return r, fake, tk, &shown
}

// sampleEvery runs checkProgress once a minute from start, for minutes from
// through to.
func sampleEvery(t *testing.T, r *Runner, id string, start time.Time, from, to int) {
	t.Helper()
	for m := from; m <= to; m++ {
		tk, err := r.Store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		r.checkProgress(context.Background(), tk, start.Add(time.Duration(m)*time.Minute))
	}
}

// kitPrompts returns the text of every agent prompt typed into a runner.
func kitPrompts(fake *fakeHerdr) []string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var out []string
	for _, c := range fake.calls {
		if len(c) == 4 && c[0] == "agent" && c[1] == "prompt" {
			out = append(out, c[3])
		}
	}
	return out
}

func TestStallLadder(t *testing.T) {
	r, fake, tk, _ := stallRunner(t, harnessFixture(t, "working-claude-1.txt"))
	start := time.Now()
	sampleEvery(t, r, tk.ID, start, 0, 9)
	if ev := events(t, r, tk.ID); slices.Contains(ev, "nudged") {
		t.Fatalf("nudged before stallAfter: %v", ev)
	}
	sampleEvery(t, r, tk.ID, start, 10, 10)
	if p := kitPrompts(fake); len(p) != 1 || !strings.HasPrefix(p[0], "No visible progress for 10 minutes.") {
		t.Fatalf("nudge prompts = %q", p)
	}
	sampleEvery(t, r, tk.ID, start, 11, 20)
	if fake.count("agent", "send-keys", tk.Herdr.PaneID, "esc") != 1 || fake.count("agent", "send-keys", tk.Herdr.PaneID, "ctrl+c") != 1 {
		t.Fatalf("interrupt keys: esc %d, ctrl+c %d", fake.count("agent", "send-keys", tk.Herdr.PaneID, "esc"), fake.count("agent", "send-keys", tk.Herdr.PaneID, "ctrl+c"))
	}
	if p := kitPrompts(fake); len(p) != 2 || !strings.HasPrefix(p[1], "You were interrupted after 20 minutes") {
		t.Fatalf("resume prompts = %q", p)
	}
	sampleEvery(t, r, tk.ID, start, 21, 30)
	stalled := eventsOf(t, r, tk.ID, "stalled")
	if len(stalled) != 1 || stalled[0].Summary != "no progress for 30 min after a nudge and an interrupt; the runner is left open" {
		t.Fatalf("stalled = %+v", stalled)
	}
	// Nothing more happens: the runner is left open.
	sampleEvery(t, r, tk.ID, start, 31, 60)
	if n := len(eventsOf(t, r, tk.ID, "stalled")); n != 1 || len(kitPrompts(fake)) != 2 || fake.count("agent", "send-keys") != 2 {
		t.Errorf("after the last step: %d stalled, %d prompts, %d keys", n, len(kitPrompts(fake)), fake.count("agent", "send-keys"))
	}
	if ev := events(t, r, tk.ID); !slices.Contains(ev, "nudged") || !slices.Contains(ev, "interrupted") {
		t.Errorf("events = %v", ev)
	}
	if got, _ := r.Store.Get(tk.ID); got.Status != task.Running || !got.UpdatedAt.Equal(tk.UpdatedAt) {
		t.Errorf("sampling changed the task: %s, updated %v", got.Status, got.UpdatedAt)
	}
}

func TestStallInterruptSettlesWithEsc(t *testing.T) {
	r, fake, tk, _ := stallRunner(t, harnessFixture(t, "working-claude-1.txt"))
	fake.on("agent wait", ok(`{"result":{"agent":{"agent_status":"idle"}}}`))
	start := time.Now()
	sampleEvery(t, r, tk.ID, start, 0, 20)
	if fake.count("agent", "send-keys", tk.Herdr.PaneID, "ctrl+c") != 0 {
		t.Error("ctrl+c sent although esc stopped the agent")
	}
	// The turn-end hook already nudged the runner: no resume prompt.
	r2, fake2, tk2, _ := stallRunner(t, harnessFixture(t, "working-claude-1.txt"))
	fake2.on("agent send-keys", func([]string) (string, error) {
		_, err := r2.Store.Update(tk2.ID, func(t *task.Task) error { t.Unreported++; return nil })
		return "{}", err
	})
	sampleEvery(t, r2, tk2.ID, start, 0, 20)
	if p := kitPrompts(fake2); len(p) != 1 {
		t.Errorf("prompts after a turn-end nudge = %q", p)
	}
	if ev := eventsOf(t, r2, tk2.ID, "interrupted"); len(ev) != 1 || strings.Contains(ev[0].Summary, "resumed") {
		t.Errorf("interrupted = %+v", ev)
	}
}

func TestStallResets(t *testing.T) {
	r, fake, tk, shown := stallRunner(t, "● Working\n")
	start := time.Now()
	// The screen changes every minute: progress, so no step.
	for m := range 30 {
		shown.Store(fmt.Sprintf("● step %c\n", 'a'+m))
		sampleEvery(t, r, tk.ID, start, m, m)
	}
	if p := kitPrompts(fake); len(p) != 0 {
		t.Fatalf("a changing screen was nudged: %q", p)
	}
	// After a nudge, a report restarts the ladder: the next stall is nudged
	// again, not interrupted.
	sampleEvery(t, r, tk.ID, start, 30, 40)
	if len(kitPrompts(fake)) != 1 {
		t.Fatalf("prompts = %q", kitPrompts(fake))
	}
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error { t.Unreported = 0; return nil }); err != nil {
		t.Fatal(err)
	}
	sampleEvery(t, r, tk.ID, start, 41, 51)
	if p := kitPrompts(fake); len(p) != 2 || !strings.HasPrefix(p[1], "No visible progress") || fake.count("agent", "send-keys") != 0 {
		t.Errorf("after a report: prompts %q, keys %d", p, fake.count("agent", "send-keys"))
	}
}

func TestStallRetryNeverEscalates(t *testing.T) {
	r, fake, tk, _ := stallRunner(t, harnessFixture(t, "retry-claude.txt"))
	start := time.Now()
	sampleEvery(t, r, tk.ID, start, 0, 45)
	if p := kitPrompts(fake); len(p) != 0 || fake.count("agent", "send-keys") != 0 {
		t.Fatalf("a retrying agent was nudged: %q", p)
	}
	stalled := eventsOf(t, r, tk.ID, "stalled")
	if len(stalled) != 1 || stalled[0].Summary != "retrying for 30 min; the runner is left open" {
		t.Errorf("stalled = %+v", stalled)
	}
}

func TestStallSkipsAnAgentThatIsNotWorking(t *testing.T) {
	r, fake, tk, _ := stallRunner(t, harnessFixture(t, "working-claude-1.txt"))
	fake.on("agent get", ok(`{"result":{"agent":{"agent":"claude","agent_status":"idle"}}}`))
	// Below stallAfter an idle agent is not looked at; past it, checkIdle
	// takes over, never the progress ladder.
	sampleEvery(t, r, tk.ID, time.Now(), 0, 9)
	if p := kitPrompts(fake); len(p) != 0 || fake.count("pane", "read") != 0 {
		t.Errorf("an idle agent was sampled: prompts %q", p)
	}
	if ev := events(t, r, tk.ID); slices.Contains(ev, "interrupted") {
		t.Errorf("an idle agent was interrupted: %v", ev)
	}
}

// idleRunner returns a running task of agent whose agent sits idle at its
// prompt, with the given unreported turns, and whose pane shows screen.
func idleRunner(t *testing.T, agent string, unreported int, screen string) (*Runner, *fakeHerdr, *task.Task) {
	t.Helper()
	r, fake, _ := newTestRunner(t)
	r.Config.StallAfter = 10 * time.Minute
	tk := running(t, r, SpawnOptions{Kind: "research", Agent: agent, Prompt: "x", Cwd: plainDir(t)})
	tk, err := r.Store.Update(tk.ID, func(t *task.Task) error { t.Unreported = unreported; return nil })
	if err != nil {
		t.Fatal(err)
	}
	fake.on("pane read", ok(screen))
	fake.on("agent get", ok(`{"result":{"agent":{"agent":"codex","agent_status":"idle"}}}`))
	return r, fake, tk
}

// A Codex runner that ended its turn to wait, was nudged by the turn end
// and was never woken, as a missed turn end leaves it, becomes stalled news
// after stallAfter idle, once.
func TestIdleRunnerAfterATurnEndNudgeIsStalled(t *testing.T) {
	r, fake, tk := idleRunner(t, "codex", 1, "› \n")
	start := time.Now()
	sampleEvery(t, r, tk.ID, start, 0, 9)
	if ev := eventsOf(t, r, tk.ID, "stalled"); len(ev) != 0 {
		t.Fatalf("stalled before stallAfter: %+v", ev)
	}
	sampleEvery(t, r, tk.ID, start, 10, 120)
	stalled := eventsOf(t, r, tk.ID, "stalled")
	if len(stalled) != 1 || stalled[0].Summary != "idle at its prompt for 10 min without a report, after a nudge; the runner is left open" {
		t.Fatalf("stalled = %+v", stalled)
	}
	if p := kitPrompts(fake); len(p) != 0 {
		t.Errorf("prompts = %q", p)
	}
	if got, _ := r.Store.Get(tk.ID); got.Status != task.Running || got.Unreported != 2 {
		t.Errorf("task = %s, unreported %d", got.Status, got.Unreported)
	}
	checkStore(t, r)
}

// A runner idle without a report, whose turn end the kit never saw, gets
// one nudge that tells it to wait in its turn, then stalled news.
func TestIdleRunnerIsNudgedThenStalled(t *testing.T) {
	r, fake, tk := idleRunner(t, "codex", 0, "› \n")
	start := time.Now()
	sampleEvery(t, r, tk.ID, start, 0, 10)
	p := kitPrompts(fake)
	if len(p) != 1 || !strings.HasPrefix(p[0], "You have been idle at your prompt for 10 minutes") || !strings.Contains(p[0], "wait for them in this same turn") {
		t.Fatalf("prompts = %q", p)
	}
	if ev := eventsOf(t, r, tk.ID, "nudged"); len(ev) != 1 || ev[0].Summary != "idle at its prompt for 10 min without a report; nudged" {
		t.Fatalf("nudged = %+v", ev)
	}
	// The nudge changed the task, so the idle clock starts again.
	sampleEvery(t, r, tk.ID, start, 11, 20)
	if ev := eventsOf(t, r, tk.ID, "stalled"); len(ev) != 0 {
		t.Fatalf("stalled too early: %+v", ev)
	}
	sampleEvery(t, r, tk.ID, start, 21, 60)
	if ev := eventsOf(t, r, tk.ID, "stalled"); len(ev) != 1 {
		t.Fatalf("stalled = %+v", ev)
	}
	if len(kitPrompts(fake)) != 1 {
		t.Errorf("prompts = %q", kitPrompts(fake))
	}
	// A message from the main session starts the ladder over.
	if _, err := r.Send(context.Background(), tk.ID, "go on"); err != nil {
		t.Fatal(err)
	}
	sampleEvery(t, r, tk.ID, start, 61, 71)
	if n := len(eventsOf(t, r, tk.ID, "nudged")); n != 2 {
		t.Errorf("nudges after a send = %d, want 2", n)
	}
	checkStore(t, r)
}

// withBackground records what the runner's last turn-end hook listed.
func withBackground(t *testing.T, r *Runner, id string, running bool) {
	t.Helper()
	if _, err := r.Store.Update(id, func(t *task.Task) error { t.Background = &running; return nil }); err != nil {
		t.Fatal(err)
	}
}

// A Claude runner waiting on background work of its own is woken by it,
// so it is nudged only after three times stallAfter. The turn-end hook's
// list says so even when the screen draws no count, as with subagents only;
// without a list the mode line decides.
func TestIdleClaudeWaitingOnBackgroundWorkIsNudgedLater(t *testing.T) {
	for name, tc := range map[string]struct {
		screen string
		listed bool
	}{
		"subagents listed, no count on screen": {harnessFixture(t, "background-agents-claude-1.txt"), true},
		"no list, shells counted on screen":    {harnessFixture(t, "background-agents-claude-2.txt"), false},
	} {
		t.Run(name, func(t *testing.T) {
			r, fake, tk := idleRunner(t, "claude", 0, tc.screen)
			if tc.listed {
				withBackground(t, r, tk.ID, true)
			}
			start := time.Now()
			sampleEvery(t, r, tk.ID, start, 0, 29)
			if p := kitPrompts(fake); len(p) != 0 {
				t.Fatalf("nudged a runner waiting on its background work: %q", p)
			}
			sampleEvery(t, r, tk.ID, start, 30, 30)
			p := kitPrompts(fake)
			if len(p) != 1 || !strings.Contains(p[0], "you are woken when they finish") {
				t.Fatalf("prompts = %q", p)
			}
			if ev := eventsOf(t, r, tk.ID, "nudged"); len(ev) != 1 || ev[0].Summary != "idle at its prompt for 30 min without a report, with background work of its own still running; nudged" {
				t.Errorf("nudged = %+v", ev)
			}
		})
	}
}

// Claude's agent switcher hint ("← 2 agents") is no background work, and a
// turn end that listed none beats a count on screen.
func TestIdleClaudeWithNothingInTheBackgroundIsNudgedAtStallAfter(t *testing.T) {
	hint := "❯ \n  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← 2 agents\n"
	for name, tc := range map[string]struct {
		screen string
		listed bool
	}{
		"agents hint, no list":          {hint, false},
		"shells on screen, none listed": {harnessFixture(t, "background-agents-claude-2.txt"), true},
	} {
		t.Run(name, func(t *testing.T) {
			r, fake, tk := idleRunner(t, "claude", 0, tc.screen)
			if tc.listed {
				withBackground(t, r, tk.ID, false)
			}
			sampleEvery(t, r, tk.ID, time.Now(), 0, 10)
			if len(kitPrompts(fake)) != 1 {
				t.Fatalf("prompts = %q", kitPrompts(fake))
			}
			if ev := eventsOf(t, r, tk.ID, "nudged"); len(ev) != 1 || ev[0].Summary != "idle at its prompt for 10 min without a report; nudged" {
				t.Errorf("nudged = %+v", ev)
			}
		})
	}
}

// A nudge that cannot be typed is news of its own, does not count as a
// nudge, and is tried again after another stallAfter, so a later stalled
// line never claims a nudge that did not happen.
func TestIdleNudgeThatCannotBeTypedIsNotCounted(t *testing.T) {
	r, fake, tk := idleRunner(t, "codex", 0, "› \n")
	fake.on("agent prompt", herdrErr("pane_busy"))
	start := time.Now()
	sampleEvery(t, r, tk.ID, start, 0, 15)
	if got, _ := r.Store.Get(tk.ID); got.Unreported != 0 {
		t.Errorf("unreported = %d after a failed nudge", got.Unreported)
	}
	failed := eventsOf(t, r, tk.ID, "nudge-failed")
	if len(failed) != 1 || !strings.HasPrefix(failed[0].Summary, "idle at its prompt for 10 min without a report; the nudge could not be typed: ") {
		t.Fatalf("nudge-failed = %+v", failed)
	}
	if n := len(eventsOf(t, r, tk.ID, "nudged")); n != 0 {
		t.Errorf("%d nudged events", n)
	}
	fake.on("agent prompt", ok("{}"))
	sampleEvery(t, r, tk.ID, start, 16, 20)
	if n := len(eventsOf(t, r, tk.ID, "nudged")); n != 1 || len(eventsOf(t, r, tk.ID, "stalled")) != 0 {
		t.Errorf("after prompts work again: %v", events(t, r, tk.ID))
	}
}

// A held done is a report: its turn end finishes it, so no nudge.
func TestIdleRunnerWithAHeldDoneIsLeftAlone(t *testing.T) {
	r, fake, tk := idleRunner(t, "codex", 0, "› \n")
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error {
		t.PendingDone = &task.PendingDone{Since: time.Now().UTC()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sampleEvery(t, r, tk.ID, time.Now(), 0, 60)
	if p := kitPrompts(fake); len(p) != 0 || len(eventsOf(t, r, tk.ID, "stalled")) != 0 {
		t.Errorf("prompts %q, events %v", p, events(t, r, tk.ID))
	}
}

func TestStallTwoWatchersActOnce(t *testing.T) {
	r, fake, tk, _ := stallRunner(t, harnessFixture(t, "working-claude-1.txt"))
	start := time.Now()
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { sampleEvery(t, r, tk.ID, start, 0, 10) })
	}
	wg.Wait()
	if p := kitPrompts(fake); len(p) != 1 {
		t.Errorf("two watchers nudged %d times", len(p))
	}
}

func TestStallAfterZeroStartsNoSampler(t *testing.T) {
	r, fake, tk, _ := stallRunner(t, "x")
	r.Config.StallAfter = 0
	defer func(d time.Duration) { progressEvery = d }(progressEvery)
	progressEvery = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	r.WatchProgress(ctx, tk.ID)
	if ctx.Err() != nil || fake.count("agent", "get") != 0 {
		t.Errorf("stallAfter 0 sampled: ctx %v, agent get %d", ctx.Err(), fake.count("agent", "get"))
	}
}

func TestStallLadderAfterRecoveredWork(t *testing.T) {
	// A reply to the nudge does not undo it: the next stall interrupts.
	r, fake, tk, shown := stallRunner(t, "● Working\n")
	start := time.Now()
	sampleEvery(t, r, tk.ID, start, 0, 10)
	shown.Store("● Working\n● I am waiting for the build.\n")
	sampleEvery(t, r, tk.ID, start, 11, 21)
	if fake.count("agent", "send-keys", tk.Herdr.PaneID, "esc") != 1 {
		t.Fatalf("a reply to the nudge undid it: prompts %q", kitPrompts(fake))
	}

	// Work for a whole stallAfter after the nudge makes the next stall a
	// new one: it is nudged first, and measured from the last progress.
	r, fake, tk, shown = stallRunner(t, "● Working\n")
	sampleEvery(t, r, tk.ID, start, 0, 10)
	for m := 11; m <= 30; m++ {
		shown.Store(fmt.Sprintf("● step %d\n", m))
		sampleEvery(t, r, tk.ID, start, m, m)
	}
	sampleEvery(t, r, tk.ID, start, 31, 40)
	p := kitPrompts(fake)
	if fake.count("agent", "send-keys") != 0 || len(p) != 2 || !strings.HasPrefix(p[1], "No visible progress for 10 minutes.") {
		t.Errorf("after recovered work: prompts %q, keys %d", p, fake.count("agent", "send-keys"))
	}
}

func TestStallSkipsAWaitForAHeavySlot(t *testing.T) {
	r, fake, tk, _ := stallRunner(t, harnessFixture(t, "working-claude-1.txt"))
	release, _, err := r.Store.AcquireHeavy(context.Background(), 1, state.HeavyRecord{Task: "main"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	waited := make(chan struct{})
	go func() {
		defer close(waited)
		_, _, _ = r.Store.AcquireHeavy(ctx, 1, state.HeavyRecord{Task: tk.ID}, nil)
	}()
	for deadline := time.Now().Add(5 * time.Second); !r.Store.HeavyWaiting(tk.ID); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the runner never queued")
		}
	}
	start := time.Now()
	sampleEvery(t, r, tk.ID, start, 0, 40)
	if p := kitPrompts(fake); len(p) != 0 || fake.count("agent", "send-keys") != 0 {
		t.Fatalf("a queued runner was nudged: prompts %q, keys %d", p, fake.count("agent", "send-keys"))
	}
	cancel()
	<-waited
	release()
	// Out of the queue, the same quiet screen climbs the ladder again.
	sampleEvery(t, r, tk.ID, start, 41, 51)
	if p := kitPrompts(fake); len(p) != 1 {
		t.Errorf("prompts after the queue = %q", p)
	}
}
