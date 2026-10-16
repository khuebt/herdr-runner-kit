package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// stateWithTask opens a fresh state folder for the CLI and stores one
// finished task with a report, a long prompt and a few events.
func stateWithTask(t *testing.T) (*state.Store, *task.Task) {
	t.Helper()
	stateDir := filepath.Join(t.TempDir(), "state")
	t.Setenv("HRK_STATE_DIR", stateDir)
	t.Setenv("HRK_TASK", "")
	store, err := state.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	tk := &task.Task{
		Kind: "code", Agent: "claude", Title: "fix login", Owner: "/repo", Cwd: "/repo", Gate: task.GateAuto,
		Prompt: "PROMPT-TEXT " + strings.Repeat("p", 2000), Inputs: "INPUTS-TEXT",
		Summary: "SUMMARY " + strings.Repeat("s", 600), Status: task.NeedsHuman,
		Question:  task.JoinQuestions([]string{"Which database?", "Which host?"}),
		Decisions: []string{"cache -> none"},
		Git:       &task.GitState{Branch: "main", Head: "abc1234", SinceStart: 2},
	}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"spawned", "started", "prompted", "sent", "nudged", "needs-human"} {
		if err := store.Append(state.Event{Task: tk.ID, Owner: tk.Owner, Type: typ, Summary: typ + " line"}); err != nil {
			t.Fatal(err)
		}
	}
	return store, tk
}

func TestShowIsCompact(t *testing.T) {
	_, tk := stateWithTask(t)
	out, err := runCLI("show", tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		tk.ID + " · needs-human · code · claude\n",
		"title: fix login\n",
		"git: repo state, on main, 2 commits since start, head abc1234",
		"summary: " + tk.Summary + "\n",
		"decisions:\n  cache -> none\n",
		"question:\n  1. Which database?\n  2. Which host?\n",
		"events (last 5):\n",
		"nudged · nudged line\n",
		"/out\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	for _, gone := range []string{"PROMPT-TEXT", "INPUTS-TEXT", "spawned line", "{"} {
		if strings.Contains(out, gone) {
			t.Errorf("show has %q:\n%s", gone, out)
		}
	}
	out, err = runCLI("show", tk.ID, "--prompt")
	if err != nil || !strings.Contains(out, "prompt:\nPROMPT-TEXT") || !strings.Contains(out, "inputs:\nINPUTS-TEXT") {
		t.Errorf("show --prompt = %q %v", out, err)
	}
}

func TestShowJSONParses(t *testing.T) {
	_, tk := stateWithTask(t)
	out, err := runCLI("show", tk.ID, "--json")
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(out))
	var got task.Task
	if err := dec.Decode(&got); err != nil || got.ID != tk.ID || got.Prompt != tk.Prompt {
		t.Fatalf("decode = %+v, %v", got, err)
	}
	if rest, _ := io.ReadAll(dec.Buffered()); len(bytes.TrimSpace(rest)) != 0 || dec.More() {
		t.Errorf("text after the JSON: %q", rest)
	}

	out, err = runCLI("show", tk.ID, "--json", "--events", "2")
	if err != nil {
		t.Fatal(err)
	}
	var both struct {
		Task   task.Task     `json:"task"`
		Events []state.Event `json:"events"`
	}
	if err := json.Unmarshal([]byte(out), &both); err != nil || both.Task.ID != tk.ID || len(both.Events) != 2 || both.Events[1].Type != "needs-human" {
		t.Errorf("show --json --events = %+v, %v", both, err)
	}
}

func TestListHasLastEventAge(t *testing.T) {
	_, tk := stateWithTask(t)
	t.Chdir(t.TempDir())
	out, err := runCLI("list", "--all")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[0], "AGE  LAST  TITLE") || !strings.HasPrefix(lines[1], tk.ID+" ") {
		t.Fatalf("list = %q", out)
	}
	// The title has two words, so LAST is the third field from the end.
	if f := strings.Fields(lines[1]); !regexp.MustCompile(`^\d+s$`).MatchString(f[len(f)-3]) {
		t.Errorf("LAST = %q in %q", f[len(f)-3], lines[1])
	}
}

func TestShowAndListCountCompactions(t *testing.T) {
	store, tk := stateWithTask(t)
	t.Chdir(t.TempDir())
	if _, err := store.Update(tk.ID, func(t *task.Task) error {
		t.Attempt, t.Handoffs = 2, []string{"/out/handoff-a2.md"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.TaskDir(tk.ID), "progress.json"), []byte(`{"compactions":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI("show", tk.ID)
	if err != nil || !strings.Contains(out, "\ncontext: attempt 2 (handed off from /out/handoff-a2.md) · 3 compactions\n") {
		t.Errorf("show = %q, %v", out, err)
	}
	out, err = runCLI("list", "--all")
	if err != nil || !regexp.MustCompile(`\d+s, 3 compactions\s+fix login`).MatchString(out) {
		t.Errorf("list = %q, %v", out, err)
	}
}

// TestShowAndListSharedWorktree names the plan task whose worktree a later
// phase works in.
func TestShowAndListSharedWorktree(t *testing.T) {
	store, tk := stateWithTask(t)
	t.Chdir(t.TempDir())
	if _, err := store.Update(tk.ID, func(t *task.Task) error {
		t.Worktree = &task.Worktree{Path: "/wt/t1-plan", Branch: "t1-plan", Removed: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	phase := &task.Task{
		Kind: "code", Agent: "claude", Title: "phase two", Owner: "/repo", Cwd: "/wt/t1-plan",
		Status: task.Running, CheckoutOf: tk.ID,
	}
	if err := store.Create(phase); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI("show", phase.ID)
	if err != nil || !strings.Contains(out, "\nworktree: /wt/t1-plan on t1-plan (removed) (of "+tk.ID+")\n") || strings.Contains(out, "cwd:") {
		t.Errorf("show = %q, %v", out, err)
	}
	out, err = runCLI("show", tk.ID)
	if err != nil || !strings.Contains(out, "\nworktree: /wt/t1-plan on t1-plan (removed)\n") {
		t.Errorf("show of the anchor = %q, %v", out, err)
	}
	out, err = runCLI("list", "--all")
	if err != nil || !strings.Contains(out, "phase two (in "+tk.ID+"'s worktree)\n") || strings.Contains(out, "fix login (in") {
		t.Errorf("list = %q, %v", out, err)
	}

	// An anchor that cannot be read is still named.
	if _, err := store.Update(phase.ID, func(t *task.Task) error { t.CheckoutOf = "t99"; return nil }); err != nil {
		t.Fatal(err)
	}
	out, err = runCLI("show", phase.ID)
	if err != nil || !strings.Contains(out, "\nworktree: shared with t99\ncwd: /wt/t1-plan\n") {
		t.Errorf("show without the anchor = %q, %v", out, err)
	}
}
