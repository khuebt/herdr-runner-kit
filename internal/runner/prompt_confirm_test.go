package runner

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/herdr"
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
		named       string
	}{
		{"hooks review opens after the prompt", "codex", late, "codex-hooks-review dialog is open"},
		{"input not read", piAgentSpec, func(*fakeHerdr) string { return "some output\n" }, "input could not be read"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, fake, got := unconfirmedLaunchOf(t, c.agent, c.screen, "idle", false, false)
			if n, k := fake.count("agent", "prompt"), fake.count("agent", "send-keys"); n != 1 || k != 0 {
				t.Errorf("%d prompts, %d Enters sent, want 1 and 0", n, k)
			}
			if s := stalledNews(t, r, got.ID); !strings.Contains(s, c.named) || !got.Stalled {
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

// An echo of the prompt that was on screen before it was typed belongs to an
// earlier session of the pane: it does not show the new prompt was taken.
func TestFirstPromptEchoOfAnEarlierSessionIsNotProof(t *testing.T) {
	earlier := "• Earlier session\n› [Pasted Content 2337 chars]\n\n" + codexEmpty
	r, fake, got := unconfirmedLaunchOf(t, "codex", func(*fakeHerdr) string { return earlier }, "idle", false, true)
	if n := fake.count("agent", "prompt"); n != 2 {
		t.Errorf("%d prompts typed, want 2", n)
	}
	if ev := events(t, r, got.ID); !slices.Contains(ev, "prompt-retyped") || got.Stalled {
		t.Errorf("events = %v, stalled %v", ev, got.Stalled)
	}
	// The same echo, drawn after the prompt was typed, is proof.
	drawn := func(f *fakeHerdr) string {
		if f.call("agent", "prompt") == nil {
			return codexEmpty
		}
		return earlier
	}
	if _, fake, _ := unconfirmedLaunchOf(t, "codex", drawn, "idle", false, true); fake.count("agent", "prompt") != 1 {
		t.Error("an echo drawn after the prompt was typed was not taken as proof")
	}
}

// A runner herdr sees as blocked is the backstop's news, with the screen's
// text, not a stalled prompt.
func TestFirstPromptBlockedLeftToTheBackstop(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	fake.on("pane read", ok(codexEmpty))
	fake.on("agent get", func([]string) (string, error) {
		if fake.call("agent", "prompt") == nil {
			return `{"result":{"agent":{"agent_status":"idle"}}}`, nil
		}
		return `{"result":{"agent":{"agent_status":"blocked"}}}`, nil
	})
	fake.on("agent wait", func([]string) (string, error) {
		if fake.call("agent", "prompt") == nil {
			return `{"result":{"agent":{"agent_status":"idle"}}}`, nil
		}
		return "", herdrError(herdr.CodeTimeout)
	})
	tk := spawnStarting(t, r, SpawnOptions{Kind: "research", Skill: "ak:research", Agent: "codex", Prompt: "queues", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Store.Get(tk.ID)
	if n, k := fake.count("agent", "prompt"), fake.count("agent", "send-keys"); n != 1 || k != 0 || got.Stalled {
		t.Errorf("%d prompts, %d Enters, stalled %v", n, k, got.Stalled)
	}
}

// The first prompt of a runner whose hooks review the user answered is lost
// in the input that Codex did not take yet: it is typed once more.
func TestFirstPromptLostAfterAnsweredHooksReviewIsTypedOnce(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	review := fixture(t, "codex-hooks-review.txt")
	answered := false
	fake.on("agent send-keys", func([]string) (string, error) {
		answered = true
		return "{}", nil
	})
	fake.on("pane read", func([]string) (string, error) {
		if answered {
			return codexEmpty, nil
		}
		return review, nil
	})
	fake.on("agent wait", func([]string) (string, error) {
		// The prompt typed first is lost; the one typed again starts a turn.
		if fake.count("agent", "prompt") == 1 {
			return "", herdrError(herdr.CodeTimeout)
		}
		return `{"result":{"agent":{"agent_status":"working"}}}`, nil
	})
	r.Sleep = func(time.Duration) {
		if got, _ := r.Store.Get("t1"); got.Status == task.NeedsHuman && !answered {
			if _, err := r.Answer(context.Background(), "t1", "skip"); err != nil {
				t.Errorf("answer skip: %v", err)
			}
		}
	}
	tk := spawnStarting(t, r, SpawnOptions{Kind: "research", Skill: "ak:research", Agent: "codex", Prompt: "queues", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Store.Get(tk.ID)
	if n := fake.count("agent", "prompt"); n != 2 {
		t.Errorf("%d prompts typed, want 2", n)
	}
	if ev := events(t, r, tk.ID); !slices.Contains(ev, "answered") || !slices.Contains(ev, "prompt-retyped") || got.Stalled || got.Status != task.Running {
		t.Errorf("events = %v, stalled %v, status %s", ev, got.Stalled, got.Status)
	}
}

// A runner that asked a question is waiting for a message, not starting up:
// send still reaches it.
func TestSendReachesARunnerThatAskedAQuestion(t *testing.T) {
	r, fake, tk := launched(t, "codex", func(*fakeHerdr) string { return codexEmpty })
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error {
		t.Status, t.Question = task.NeedsHuman, "A or B?"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := fake.count("agent", "prompt")
	if _, err := r.Send(context.Background(), tk.ID, "use A"); err != nil {
		t.Errorf("send error = %v", err)
	}
	if n := fake.count("agent", "prompt"); n != before+1 {
		t.Errorf("%d prompts typed, want 1", n-before)
	}
}

// AgentWait can return before the agent drew what the Enter did: an input
// that holds the text is read again before the send is called untaken.
func TestSendReadsTheInputAgainBeforeCallingItUntaken(t *testing.T) {
	for name, settles := range map[string]bool{"input empties": true, "input keeps the text": false} {
		t.Run(name, func(t *testing.T) {
			// The input holds the text until the second read after the send.
			reads, typed := 0, 0
			var fake *fakeHerdr
			r, fake, tk := launched(t, "codex", func(f *fakeHerdr) string {
				if f.count("agent", "prompt") == typed {
					return codexEmpty
				}
				if reads++; reads > 1 && settles {
					return codexEmpty
				}
				return "› more\n\n  GPT-6-Luna low\n"
			})
			typed = fake.count("agent", "prompt")
			working, err := r.Send(context.Background(), tk.ID, "more")
			if err != nil || working != settles {
				t.Errorf("Send = %v, %v; want %v", working, err, settles)
			}
		})
	}
}

// Codex draws its composer a moment after a dialog closes: the first prompt
// waits for it to show empty instead of being typed before it takes input.
func TestFirstPromptWaitsForCodexComposer(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	const ready = 8
	readsAtPrompt := 0
	fake.on("pane read", func([]string) (string, error) {
		if fake.count("pane", "read") < ready {
			return "• Booting\n", nil
		}
		return codexEmpty, nil
	})
	fake.on("agent prompt", func([]string) (string, error) {
		readsAtPrompt = fake.count("pane", "read")
		return "{}", nil
	})
	tk := spawnStarting(t, r, SpawnOptions{Kind: "research", Skill: "ak:research", Agent: "codex", Prompt: "queues", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	if readsAtPrompt < ready {
		t.Errorf("the prompt was typed after %d reads, before the composer showed at read %d", readsAtPrompt, ready)
	}
}
