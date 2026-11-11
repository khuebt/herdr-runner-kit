package runner

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

const piAgentSpec = "pi:openai-codex/gpt-6-luna"

const codexEmpty = "• Booting\n\n› Ask Codex to do anything\n\n  GPT-6-Luna low · ~/Work/proj\n"

func stalledNews(t *testing.T, r *Runner, id string) string {
	t.Helper()
	evs, _, _ := r.Store.ReadEventsSince(0)
	var summary string
	for _, ev := range evs {
		if ev.Task == id && ev.Type == "stalled" {
			summary = ev.Summary
		}
	}
	return summary
}

func TestFirstPromptLostIsTypedOnceMore(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		t.Run(agent, func(t *testing.T) {
			screen := codexEmpty
			if agent == "claude" {
				screen = fixture(t, "working-claude-1.txt")
			}
			r, fake, got := unconfirmedLaunchOf(t, agent, func(*fakeHerdr) string { return screen }, "idle", false, true)
			if n, k := fake.count("agent", "prompt"), fake.count("agent", "send-keys"); n != 2 || k != 0 {
				t.Errorf("%d prompts, %d Enters sent, want 2 and 0", n, k)
			}
			if ev := events(t, r, got.ID); !slices.Contains(ev, "prompt-retyped") || slices.Contains(ev, "stalled") || got.Stalled {
				t.Errorf("events = %v, stalled %v", ev, got.Stalled)
			}
		})
	}
}

func TestFirstPromptLostAgainIsStalledNewsAfterOneRetype(t *testing.T) {
	r, fake, got := unconfirmedLaunch(t, "codex", codexEmpty, "idle", false, false)
	if n := fake.count("agent", "prompt"); n != 2 {
		t.Errorf("%d prompts sent, want 2", n)
	}
	if s := stalledNews(t, r, got.ID); !strings.Contains(s, "first prompt not taken") || !got.Stalled || got.Status != task.Running {
		t.Errorf("stalled news %q, stalled %v, status %s", s, got.Stalled, got.Status)
	}
}

func TestFirstPromptCodexInputHoldingTextGetsOneEnter(t *testing.T) {
	r, fake, got := unconfirmedLaunch(t, "codex", fixture(t, "codex-input-pasted.txt"), "idle", false, true)
	if n, k := fake.count("agent", "prompt"), fake.count("agent", "send-keys"); n != 1 || k != 1 {
		t.Errorf("%d prompts, %d Enters sent, want 1 and 1", n, k)
	}
	if ev := events(t, r, got.ID); !slices.Contains(ev, "prompt-resubmitted") || got.Stalled {
		t.Errorf("events = %v, stalled %v", ev, got.Stalled)
	}
}

// A dialog on screen, or an input that cannot be read, proves nothing about
// the prompt: nothing is sent, and a runner herdr still sees no turn in is
// news.
func TestFirstPromptUnsureSendsNothingAndIsNews(t *testing.T) {
	// The dialog opens late: it is not there when the prompt is typed.
	late := func(f *fakeHerdr) string {
		if f.call("agent", "prompt") == nil {
			return codexEmpty
		}
		return fixture(t, "codex-hooks-review.txt")
	}
	cases := []struct {
		name, agent string
		screen      func(*fakeHerdr) string
	}{
		{"hooks review opens after the prompt", "codex", late},
		{"input not read", piAgentSpec, func(*fakeHerdr) string { return "some output\n" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, fake, got := unconfirmedLaunchOf(t, c.agent, c.screen, "idle", false, false)
			if n, k := fake.count("agent", "prompt"), fake.count("agent", "send-keys"); n != 1 || k != 0 {
				t.Errorf("%d prompts, %d Enters sent, want 1 and 0", n, k)
			}
			if s := stalledNews(t, r, got.ID); !strings.Contains(s, "not confirmed") || !got.Stalled {
				t.Errorf("stalled news %q, stalled %v", s, got.Stalled)
			}
		})
	}
	t.Run("turn starts late", func(t *testing.T) {
		r, _, got := unconfirmedLaunch(t, piAgentSpec, "some output\n", "idle", false, true)
		if ev := events(t, r, got.ID); slices.Contains(ev, "stalled") || got.Stalled {
			t.Errorf("events = %v, stalled %v", ev, got.Stalled)
		}
	})
}

// launched returns a running task whose runner shows screen.
func launched(t *testing.T, agent string, screen func(*fakeHerdr) string) (*Runner, *fakeHerdr, *task.Task) {
	t.Helper()
	r, fake, _ := newTestRunner(t)
	tk := spawnStarting(t, r, SpawnOptions{Kind: "research", Skill: "ak:research", Agent: agent, Prompt: "queues", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	fake.on("pane read", func([]string) (string, error) { return screen(fake), nil })
	got, _ := r.Store.Get(tk.ID)
	return r, fake, got
}

func TestSendRefusedWhileStartingOrAtAStartupDialog(t *testing.T) {
	setups := map[string]func(*task.Task){
		"starting": func(t *task.Task) { t.Status = task.Starting },
		"dialog with answers": func(t *task.Task) {
			t.Status, t.Error, t.DialogAnswers = task.NeedsHuman, "codex-hooks-review", map[string][]string{"skip": {"esc"}}
		},
		"unknown dialog": func(t *task.Task) { t.Status, t.Error = task.NeedsHuman, "unknown startup" },
	}
	for name, set := range setups {
		t.Run(name, func(t *testing.T) {
			r, fake, tk := launched(t, "codex", func(*fakeHerdr) string { return codexEmpty })
			if _, err := r.Store.Update(tk.ID, func(t *task.Task) error { set(t); return nil }); err != nil {
				t.Fatal(err)
			}
			before := fake.count("agent", "prompt")
			if _, err := r.Send(context.Background(), tk.ID, "more"); err == nil || !strings.Contains(err.Error(), "still starting") {
				t.Errorf("send error = %v", err)
			}
			if n := fake.count("agent", "prompt"); n != before {
				t.Errorf("%d prompts typed", n-before)
			}
		})
	}
}

func TestSendWorkingNeedsProofTheAgentTookTheText(t *testing.T) {
	cases := []struct {
		name, screen string
		want         bool
	}{
		{"input empty", codexEmpty, true},
		{"input still holds the text", "› more\n\n  GPT-6-Luna low\n", false},
		{"input holds a paste", fixture(t, "codex-input-pasted.txt"), false},
		{"startup dialog open", fixture(t, "codex-hooks-review.txt"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _, tk := launched(t, "codex", func(*fakeHerdr) string { return c.screen })
			working, err := r.Send(context.Background(), tk.ID, "more")
			if err != nil || working != c.want {
				t.Errorf("Send = %v, %v; want %v", working, err, c.want)
			}
		})
	}
}

func TestAnswerWorkingNeedsProofTheAgentTookTheText(t *testing.T) {
	for _, c := range []struct {
		screen string
		want   bool
	}{{codexEmpty, true}, {"› use A\n\n  GPT-6-Luna low\n", false}} {
		r, _, tk := launched(t, "codex", func(*fakeHerdr) string { return c.screen })
		if _, err := r.Store.Update(tk.ID, func(t *task.Task) error {
			t.Status, t.Question = task.NeedsHuman, "A or B?"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if working, err := r.Answer(context.Background(), tk.ID, "use A"); err != nil || working != c.want {
			t.Errorf("Answer on %q = %v, %v; want %v", c.screen, working, err, c.want)
		}
	}
}
