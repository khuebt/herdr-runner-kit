package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// gateEnv makes a state dir holding one locked Claude task, and points the
// gate at it.
func gateEnv(t *testing.T) (*state.Store, string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "state")
	t.Setenv("HRK_STATE_DIR", dir)
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	tk := &task.Task{Kind: "code", Agent: "claude", Prompt: "p", Cwd: "/repo", Owner: "/repo", Gate: task.GateAuto, Status: task.Running, InputLocked: true}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRK_TASK", tk.ID)
	return store, tk.ID
}

// runGate runs the hook with the probe payload in file, and returns what it
// printed to stdout.
func runGate(t *testing.T, file string) string {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("..", "harness", "testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	root := NewRootCommand()
	var out, errOut bytes.Buffer
	root.SetIn(bytes.NewReader(payload))
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"__prompt-gate", "--harness", "claude"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("the gate failed the hook: %v", err)
	}
	return out.String()
}

func blockedEvents(t *testing.T, store *state.Store) []state.Event {
	t.Helper()
	events, _, err := store.ReadEventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	var blocked []state.Event
	for _, ev := range events {
		if ev.Type == "input-blocked" {
			blocked = append(blocked, ev)
		}
	}
	return blocked
}

func TestPromptGateBlocksATypedPrompt(t *testing.T) {
	store, id := gateEnv(t)
	out := runGate(t, "userpromptsubmit-claude.json")
	if !strings.Contains(out, `"decision":"block"`) || !strings.Contains(out, id+" is a runner") || !strings.Contains(out, "hrk send "+id) {
		t.Errorf("block reply = %q", out)
	}
	blocked := blockedEvents(t, store)
	if len(blocked) != 1 || blocked[0].Summary != "human typed text" || blocked[0].Task != id || blocked[0].Owner != "/repo" {
		t.Errorf("events = %+v", blocked)
	}
}

func TestPromptGateLetsAKitPromptThroughOnce(t *testing.T) {
	store, id := gateEnv(t)
	if err := store.AddPromptToken(id); err != nil {
		t.Fatal(err)
	}
	if out := runGate(t, "userpromptsubmit-claude.json"); out != "" {
		t.Errorf("a prompt with a token was blocked: %q", out)
	}
	if out := runGate(t, "userpromptsubmit-claude.json"); !strings.Contains(out, `"decision":"block"`) {
		t.Errorf("the token let a second prompt through: %q", out)
	}
}

func TestPromptGateBlocksOnAnExpiredToken(t *testing.T) {
	store, id := gateEnv(t)
	expired := strconv.FormatInt(time.Now().Add(-time.Second).UnixNano(), 10) + "\n"
	if err := os.WriteFile(filepath.Join(store.TaskDir(id), "prompt-tokens"), []byte(expired), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := runGate(t, "userpromptsubmit-claude.json"); !strings.Contains(out, `"decision":"block"`) {
		t.Errorf("an expired token let a prompt through: %q", out)
	}
}

func TestPromptGateLetsATaskNotificationThrough(t *testing.T) {
	store, _ := gateEnv(t)
	if out := runGate(t, "userpromptsubmit-task-notification-claude.json"); out != "" {
		t.Errorf("Claude's own task notification was blocked: %q", out)
	}
	if blocked := blockedEvents(t, store); len(blocked) != 0 {
		t.Errorf("events = %+v", blocked)
	}
}

func TestPromptGateFailsOpen(t *testing.T) {
	gateEnv(t)
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRK_STATE_DIR", file)
	if out := runGate(t, "userpromptsubmit-claude.json"); out != "" {
		t.Errorf("an unreadable state dir blocked the prompt: %q", out)
	}
}
