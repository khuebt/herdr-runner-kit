package runner

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/herdr"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// unconfirmedLaunch launches a task whose first prompt herdr saw no turn
// start for. waits holds the answers of the first prompt's wait and, when
// the launcher gets that far, of the one after the extra Enter.
func unconfirmedLaunch(t *testing.T, agent, screen, status string, waits ...bool) (*Runner, *fakeHerdr, *task.Task) {
	t.Helper()
	r, fake, _ := newTestRunner(t)
	fake.on("pane read", ok(screen))
	fake.on("agent get", ok(`{"result":{"agent":{"agent_status":"`+status+`"}}}`))
	fake.on("agent wait", func([]string) (string, error) {
		started := true
		if len(waits) > 0 {
			started, waits = waits[0], waits[1:]
		}
		if !started {
			return "", herdrError(herdr.CodeTimeout)
		}
		return `{"result":{"agent":{"agent_status":"working"}}}`, nil
	})
	tk := spawnStarting(t, r, SpawnOptions{Kind: "research", Skill: "ak:research", Agent: agent, Prompt: "queues", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Store.Get(tk.ID)
	return r, fake, got
}

func TestFirstPromptStillInInputGetsOneEnter(t *testing.T) {
	r, fake, got := unconfirmedLaunch(t, "claude", fixture(t, "input-pasted-claude.txt"), "idle", false, true)
	if keys := fake.call("agent", "send-keys"); !slices.Equal(keys, []string{"agent", "send-keys", "w1:p9", "enter"}) {
		t.Errorf("send-keys = %q", keys)
	}
	if n := fake.count("agent", "prompt"); n != 1 {
		t.Errorf("the prompt was sent %d times", n)
	}
	if ev := events(t, r, got.ID); !slices.Contains(ev, "prompt-resubmitted") || slices.Contains(ev, "stalled") || got.Stalled {
		t.Errorf("events = %v, stalled %v", ev, got.Stalled)
	}
}

func TestFirstPromptTypedInInputGetsOneEnter(t *testing.T) {
	_, fake, _ := unconfirmedLaunch(t, "claude", fixture(t, "input-typed-claude.txt"), "idle", false, true)
	if n := fake.count("agent", "send-keys"); n != 1 {
		t.Errorf("%d Enters sent", n)
	}
}

func TestFirstPromptNotSubmittedAfterRetryIsStalledNews(t *testing.T) {
	r, fake, got := unconfirmedLaunch(t, "claude", fixture(t, "input-pasted-claude.txt"), "idle", false, false)
	if n := fake.count("agent", "send-keys"); n != 1 {
		t.Errorf("%d Enters sent, want 1", n)
	}
	if n := fake.count("agent", "prompt"); n != 1 {
		t.Errorf("the prompt was sent %d times", n)
	}
	evs, _, _ := r.Store.ReadEventsSince(0)
	var stalled string
	for _, ev := range evs {
		if ev.Task == got.ID && ev.Type == "stalled" {
			stalled = ev.Summary
		}
	}
	if !strings.Contains(stalled, "prompt not submitted") || !got.Stalled || got.Status != task.Running {
		t.Errorf("stalled news %q, stalled %v, status %s", stalled, got.Stalled, got.Status)
	}
}

func TestFirstPromptNotResentWhenInputEmptyOrAgentWorking(t *testing.T) {
	cases := []struct{ name, agent, screen, status string }{
		{"empty input", "claude", fixture(t, "working-claude-1.txt"), "idle"},
		{"agent working", "claude", fixture(t, "input-pasted-claude.txt"), "working"},
		{"codex is never read", "codex", fixture(t, "input-pasted-claude.txt"), "idle"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, fake, got := unconfirmedLaunch(t, c.agent, c.screen, c.status, false)
			if n := fake.count("agent", "send-keys"); n != 0 {
				t.Errorf("%d Enters sent", n)
			}
			if ev := events(t, r, got.ID); slices.Contains(ev, "stalled") || got.Stalled || !slices.Contains(ev, "prompt-unconfirmed") {
				t.Errorf("events = %v, stalled %v", ev, got.Stalled)
			}
		})
	}
}

func TestFirstPromptSubmittedButSlowToStartIsNotStalled(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	held, empty := fixture(t, "input-pasted-claude.txt"), fixture(t, "working-claude-1.txt")
	// The second Enter submits the prompt, but herdr is slow to see the turn.
	fake.on("pane read", func([]string) (string, error) {
		if fake.count("agent", "send-keys") > 0 {
			return empty, nil
		}
		return held, nil
	})
	fake.on("agent get", ok(`{"result":{"agent":{"agent_status":"idle"}}}`))
	fake.on("agent wait", herdrErr(herdr.CodeTimeout))
	tk := spawnStarting(t, r, SpawnOptions{Kind: "research", Skill: "ak:research", Agent: "claude", Prompt: "queues", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Store.Get(tk.ID)
	if ev := events(t, r, got.ID); slices.Contains(ev, "stalled") || got.Stalled || !slices.Contains(ev, "prompt-resubmitted") || fake.count("agent", "send-keys") != 1 {
		t.Errorf("events = %v, stalled %v", ev, got.Stalled)
	}
}
