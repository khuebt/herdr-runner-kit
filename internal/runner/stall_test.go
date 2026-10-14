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
	sampleEvery(t, r, tk.ID, time.Now(), 0, 30)
	if p := kitPrompts(fake); len(p) != 0 || fake.count("pane", "read") != 0 {
		t.Errorf("an idle agent was sampled: prompts %q", p)
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
