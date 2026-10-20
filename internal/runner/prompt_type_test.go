package runner

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/config"
	"github.com/khuebt/herdr-runner-kit/internal/harness"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// lockedRunner is a test runner with the default, locked runner input.
func lockedRunner(t *testing.T) (*Runner, *fakeHerdr) {
	t.Helper()
	r, fake, _ := newTestRunner(t)
	r.Config.RunnerInput = config.InputLocked
	return r, fake
}

// tokens counts the task's prompt tokens on disk.
func tokens(t *testing.T, r *Runner, id string) int {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(r.Store.TaskDir(id), "prompt-tokens"))
	return len(strings.Fields(string(data)))
}

func writeTokens(t *testing.T, r *Runner, id, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.Store.TaskDir(id), "prompt-tokens"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// promptLog records, for each prompt herdr was asked to type, the call
// just before it and the tokens on disk at that moment. Each prompt is then
// submitted through the gate, which takes its token.
type promptLog struct {
	prev   [][]string
	tokens []int
}

func watchPrompts(t *testing.T, r *Runner, fake *fakeHerdr, id *string) *promptLog {
	t.Helper()
	log := &promptLog{}
	fake.on("agent prompt", func([]string) (string, error) {
		fake.mu.Lock()
		log.prev = append(log.prev, fake.calls[len(fake.calls)-2])
		fake.mu.Unlock()
		log.tokens = append(log.tokens, tokens(t, r, *id))
		_, _ = r.Store.TakePromptToken(*id)
		return "{}", nil
	})
	return log
}

// clearKeys reports whether call sent the clear keys, without esc.
func clearKeys(call []string) bool {
	return len(call) > 3 && slices.Equal(call[:2], []string{"agent", "send-keys"}) && call[3] == "ctrl+u" && !slices.Contains(call, "esc")
}

func TestLockedClaudeRunnerClearsThenTokensThenPrompts(t *testing.T) {
	r, fake := lockedRunner(t)
	var id string
	log := watchPrompts(t, r, fake, &id)
	tk := spawnStarting(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	id = tk.ID
	if !tk.InputLocked {
		t.Fatal("the default config spawned an open runner")
	}
	if err := r.Launch(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	settings, _ := os.ReadFile(filepath.Join(r.Store.TaskDir(id), "claude-settings.json"))
	if !strings.Contains(string(settings), "__prompt-gate --harness claude") {
		t.Errorf("settings lack the prompt gate:\n%s", settings)
	}
	if _, err := r.Send(context.Background(), id, "next step"); err != nil {
		t.Fatal(err)
	}
	if len(log.prev) != 2 {
		t.Fatalf("prompts = %d, want the first prompt and the send", len(log.prev))
	}
	for i, prev := range log.prev {
		if !clearKeys(prev) {
			t.Errorf("prompt %d: the input was not cleared just before it: %q", i, prev)
		}
		if log.tokens[i] != 1 {
			t.Errorf("prompt %d went in with %d tokens, want 1", i, log.tokens[i])
		}
	}
	if got, _ := r.Store.Get(id); !InputLocked(got) {
		t.Error("InputLocked = false for a locked Claude runner")
	}
}

func TestOpenOrCodexRunnerPromptsAsBefore(t *testing.T) {
	for _, tt := range []struct {
		name  string
		input string
		agent string
	}{
		{"open claude", config.InputOpen, ""},
		{"locked codex", config.InputLocked, "codex"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, fake, _ := newTestRunner(t)
			r.Config.RunnerInput = tt.input
			tk := running(t, r, SpawnOptions{Agent: tt.agent, Prompt: "x", Cwd: plainDir(t)})
			if _, err := r.Send(context.Background(), tk.ID, "next step"); err != nil {
				t.Fatal(err)
			}
			if n := fake.count("agent", "send-keys"); n != 0 {
				t.Errorf("send-keys calls = %d, want none", n)
			}
			if p := fake.call("agent", "prompt"); !slices.Equal(p, []string{"agent", "prompt", tk.Herdr.PaneID, "next step"}) {
				t.Errorf("prompt = %q", p)
			}
			if n := tokens(t, r, tk.ID); n != 0 {
				t.Errorf("tokens = %d", n)
			}
			// Open Claude input has the gate too, which lets a prompt typed
			// in the tab through.
			if a, _ := harness.For(task.Claude); tt.input == config.InputOpen {
				if block, err := r.GatePrompt(tk.ID, a, []byte(`{"prompt":"typed in the tab"}`)); block != "" || err != nil {
					t.Errorf("open gate: block %q, err %v", block, err)
				}
			}
		})
	}
}

func TestLockedPromptFailureTakesItsTokenBack(t *testing.T) {
	r, fake := lockedRunner(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	fake.on("agent prompt", herdrErr("pane_busy"))
	if _, err := r.Send(context.Background(), tk.ID, "next step"); err == nil {
		t.Fatal("send succeeded")
	}
	if n := tokens(t, r, tk.ID); n != 0 {
		t.Errorf("a failed prompt left %d tokens", n)
	}
	if fake.count("agent", "send-keys") != 1 {
		t.Error("the input was not cleared before the prompt")
	}
}

// claudeScreen is a Claude screen whose input box holds box.
func claudeScreen(box string) string {
	edge := strings.Repeat("─", 60)
	return "  68817 tokens\n" + edge + "\n❯ " + box + "\n" + edge + "\n  ⏵⏵ bypass permissions on (shift+tab to cycle)\n"
}

func TestLockedPromptKeepsAnEarlierOneWhoseEnterWasLost(t *testing.T) {
	r, fake := lockedRunner(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	_, _ = r.Store.TakePromptToken(tk.ID) // the first prompt went in
	clears := func() int { return fake.count("agent", "send-keys") }

	// The send's Enter is lost: its token is never taken, and outlives
	// its expiry, while its text still sits in the box.
	fake.on("pane read", ok(claudeScreen("first")))
	if _, err := r.Send(context.Background(), tk.ID, "first"); err != nil {
		t.Fatal(err)
	}
	before := clears()
	expired := strconv.FormatInt(time.Now().Add(-time.Minute).UnixNano(), 10) + "\n"
	writeTokens(t, r, tk.ID, expired)
	if _, err := r.Send(context.Background(), tk.ID, "second"); err != nil {
		t.Fatal(err)
	}
	if clears() != before {
		t.Error("the input was cleared over a kit prompt that never reached the gate")
	}

	// The joined prompt goes through the gate; the next one clears again.
	if taken, _ := r.Store.TakePromptToken(tk.ID); !taken {
		t.Fatal("the second prompt left no token")
	}
	if _, err := r.Send(context.Background(), tk.ID, "third"); err != nil {
		t.Fatal(err)
	}
	if clears() != before+1 {
		t.Error("the input was not cleared once the gate let a prompt through")
	}
}

// lastPrompt is the text of the last prompt herdr was asked to type.
func lastPrompt(fake *fakeHerdr) string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for i := len(fake.calls) - 1; i >= 0; i-- {
		if c := fake.calls[i]; len(c) == 4 && c[0] == "agent" && c[1] == "prompt" {
			return c[3]
		}
	}
	return ""
}

func TestLockedPromptClearsAnythingButTheKitsOwnPendingText(t *testing.T) {
	const pending = "run the migration check on the staging database and report"
	for name, tc := range map[string]struct {
		screen string
		keeps  bool
	}{
		"exact pending text":      {claudeScreen(pending), true},
		"wrapped pending text":    {claudeScreen(strings.Replace(pending, " on the", "\n  on the", 1)), true},
		"pasted placeholder":      {claudeScreen("[Pasted text #1 +3 lines]"), false},
		"same prefix, other tail": {claudeScreen(pending[:45] + " and then delete everything"), false},
		"pending text and more":   {claudeScreen(pending + " and delete the branch"), false},
		"typed text":              {claudeScreen("please delete the branch"), false},
		"empty box":               {claudeScreen(""), false},
		"unreadable box":          {"no input box here\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			r, fake := lockedRunner(t)
			tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
			_, _ = r.Store.TakePromptToken(tk.ID)
			// The kit's prompt lost its Enter and stays pending.
			if _, err := r.Send(context.Background(), tk.ID, pending); err != nil {
				t.Fatal(err)
			}
			before := fake.count("agent", "send-keys")
			fake.on("pane read", ok(tc.screen))
			if _, err := r.Send(context.Background(), tk.ID, "second"); err != nil {
				t.Fatal(err)
			}
			clears := fake.count("agent", "send-keys") - before
			want := "second"
			if !tc.keeps {
				// Cleared, and the kit's own text goes in again ahead of it.
				want = pending + "\n\n" + want
			}
			if (clears == 0) != tc.keeps || lastPrompt(fake) != want {
				t.Errorf("clears %d, prompt %q, want keep %v and prompt %q", clears, lastPrompt(fake), tc.keeps, want)
			}
			// One token answers for the one submit that carries both.
			if n := tokens(t, r, tk.ID); n != 1 {
				t.Errorf("tokens = %d, want 1", n)
			}
		})
	}
}

func TestLockedPromptFailureAfterClearKeepsThePendingPrompt(t *testing.T) {
	r, fake := lockedRunner(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	_, _ = r.Store.TakePromptToken(tk.ID)
	if _, err := r.Send(context.Background(), tk.ID, "first"); err != nil {
		t.Fatal(err)
	}
	fake.on("pane read", ok(claudeScreen("")))
	fake.on("agent prompt", herdrErr("pane_busy"))
	if _, err := r.Send(context.Background(), tk.ID, "second"); err == nil {
		t.Fatal("send succeeded")
	}
	fake.on("agent prompt", ok("{}"))
	if _, err := r.Send(context.Background(), tk.ID, "third"); err != nil {
		t.Fatal(err)
	}
	if got := lastPrompt(fake); got != "first\n\nthird" {
		t.Errorf("prompt after the failed one = %q, want the first one typed again", got)
	}
}

func TestLockedCodexStartedLineSaysInputIsOpen(t *testing.T) {
	for agent, want := range map[string]bool{"codex": true, "claude": false} {
		r, _ := lockedRunner(t)
		tk := spawnStarting(t, r, SpawnOptions{Agent: agent, Prompt: "x", Cwd: plainDir(t)})
		spec, _ := task.ParseAgentSpec(tk.Agent)
		line := startedLine(tk, spec, nil, "")
		if got := strings.Contains(line, "input open (Codex cannot lock it yet)"); got != want {
			t.Errorf("%s started line = %q", agent, line)
		}
	}
}
