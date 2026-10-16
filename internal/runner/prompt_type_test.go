package runner

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/config"
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

// promptLog records, for each prompt herdr was asked to type, the call
// just before it and the tokens on disk at that moment.
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
		// The fake runs no gate, so tokens add up: one per prompt.
		if log.tokens[i] != i+1 {
			t.Errorf("prompt %d went in with %d tokens, want %d", i, log.tokens[i], i+1)
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
