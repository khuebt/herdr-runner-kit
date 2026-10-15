package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/config"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// compactHook runs the context hook with the named fixture's input.
func compactHook(t *testing.T, r *Runner, id, name string) string {
	t.Helper()
	out, err := r.ContextHook(context.Background(), id, task.Claude, []byte(fixture(t, name)))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// sessionContext returns the additional context of a SessionStart output.
func sessionContext(t *testing.T, out string) string {
	t.Helper()
	var o struct {
		HookSpecificOutput struct{ HookEventName, AdditionalContext string }
	}
	if err := json.Unmarshal([]byte(out), &o); err != nil || o.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Fatalf("hook output %q: %v", out, err)
	}
	return o.HookSpecificOutput.AdditionalContext
}

func TestAutomaticCompactionAsksForAHandoffOnce(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: plainDir(t)})
	// Claude may announce an automatic compaction more than once before it
	// runs one.
	for range 2 {
		if out := compactHook(t, r, tk.ID, "precompact-auto.json"); out != "" {
			t.Fatalf("PreCompact output = %q", out)
		}
	}
	if n := countEvents(t, r, tk.ID, "context-low"); n != 0 {
		t.Fatalf("context-low before any compaction ran: %d", n)
	}
	request := sessionContext(t, compactHook(t, r, tk.ID, "sessionstart-compact.json"))
	file := r.OutDir(tk.ID) + "/handoff-a2.md"
	for _, want := range []string{"your context is nearly full", file, r.Bin + " report handoff --output " + file, "background process you started (command and pid)"} {
		if !strings.Contains(request, want) {
			t.Errorf("request lacks %q:\n%s", want, request)
		}
	}
	if ev := lastEvent(t, r); ev.Type != "context-low" || !strings.Contains(ev.Summary, "asked the runner to hand off") {
		t.Errorf("event = %+v", ev)
	}
	// The request is queued in the pane too, as a message.
	if p := lastCall(fake, "agent", "prompt"); p == nil || p[2] != tk.Herdr.PaneID || p[3] != request {
		t.Errorf("queued prompt = %q", p)
	}
	st := r.loadProgress(tk.ID)
	if st.Compactions != 1 || st.Asked != 1 || st.Armed != 0 || st.LowAt.IsZero() {
		t.Errorf("progress = %+v", st)
	}

	// A later automatic compaction of the same attempt is only noted.
	compactHook(t, r, tk.ID, "precompact-auto.json")
	if out := compactHook(t, r, tk.ID, "sessionstart-compact.json"); out != "" {
		t.Errorf("second request = %q", out)
	}
	if ev := lastEvent(t, r); ev.Type != "context-low" || !strings.HasSuffix(ev.Summary, "it was already asked to hand off") {
		t.Errorf("event = %+v", ev)
	}
	if r.Compactions(tk.ID) != 2 {
		t.Errorf("compactions = %d", r.Compactions(tk.ID))
	}
}

func TestManualCompactionNeverAsks(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	compactHook(t, r, tk.ID, "precompact-manual.json")
	if out := compactHook(t, r, tk.ID, "sessionstart-compact.json"); out != "" {
		t.Errorf("manual compaction asked: %q", out)
	}
	// An automatic compaction announced but not run, then a manual one.
	compactHook(t, r, tk.ID, "precompact-auto.json")
	compactHook(t, r, tk.ID, "precompact-manual.json")
	if out := compactHook(t, r, tk.ID, "sessionstart-compact.json"); out != "" {
		t.Errorf("manual compaction after an announced automatic one asked: %q", out)
	}
	if n := countEvents(t, r, tk.ID, "context-low"); n != 0 {
		t.Errorf("context-low events = %d", n)
	}
	if r.Compactions(tk.ID) != 2 {
		t.Errorf("compactions = %d", r.Compactions(tk.ID))
	}
}

func TestCompactionAsksNothingWhenTheHandoffIsUnavailable(t *testing.T) {
	r, _, e := newTestRunner(t)
	used := handedOff(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	e["HRK_ATTEMPT"] = "2"
	compactHook(t, r, used.ID, "precompact-auto.json")
	if out := compactHook(t, r, used.ID, "sessionstart-compact.json"); out != "" {
		t.Errorf("a task that used its replacement was asked: %q", out)
	}
	if ev := lastEvent(t, r); !strings.HasSuffix(ev.Summary, "it already used its one context replacement") {
		t.Errorf("event = %+v", ev)
	}

	delete(e, "HRK_ATTEMPT")
	r.Config.ContextHandoff = false
	off := running(t, r, SpawnOptions{Prompt: "y", Cwd: plainDir(t)})
	compactHook(t, r, off.ID, "precompact-auto.json")
	if out := compactHook(t, r, off.ID, "sessionstart-compact.json"); out != "" {
		t.Errorf("contextHandoff off, asked: %q", out)
	}
	if ev := lastEvent(t, r); !strings.HasSuffix(ev.Summary, "contextHandoff is off") {
		t.Errorf("event = %+v", ev)
	}
}

func TestContextHookOfAnEndedAttemptIsIgnored(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk := handedOff(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	tk, err := r.Store.Update(tk.ID, func(t *task.Task) error { t.Handoffs = nil; return nil })
	if err != nil {
		t.Fatal(err)
	}
	compactHook(t, r, tk.ID, "precompact-auto.json")
	if out := compactHook(t, r, tk.ID, "sessionstart-compact.json"); out != "" {
		t.Errorf("attempt 1's hook asked: %q", out)
	}
	if st := r.loadProgress(tk.ID); st.Compactions != 0 || st.Armed != 0 {
		t.Errorf("attempt 1's hook changed the progress: %+v", st)
	}
}

func TestStallLadderKeepsTheContextState(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	compactHook(t, r, tk.ID, "precompact-manual.json")
	compactHook(t, r, tk.ID, "sessionstart-compact.json")
	tk, _ = r.Store.Update(tk.ID, func(*task.Task) error { return nil }) // the runner moved on
	r.checkProgress(context.Background(), tk, time.Now())
	if st := r.loadProgress(tk.ID); st.Compactions != 1 || !st.UpdatedAt.Equal(tk.UpdatedAt) {
		t.Errorf("progress after a ladder reset = %+v", st)
	}
}

// codexTurn is the notify input of a Codex runner's turn.
const codexTurn = `{"type":"agent-turn-complete","thread-id":"th1","input-messages":["go on"]}`

func TestCodexLowContextAsksInsteadOfNudging(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Agent: "codex", Prompt: "x", Cwd: plainDir(t)})
	fake.on("pane read", ok(fixture(t, "codex-context-low.txt")))
	if _, err := r.TurnEnd(context.Background(), tk.ID, task.Codex, []byte(codexTurn)); err != nil {
		t.Fatal(err)
	}
	prompt := lastCall(fake, "agent", "prompt")
	if prompt == nil || !strings.Contains(prompt[3], "report handoff --output "+r.OutDir(tk.ID)+"/handoff-a2.md") {
		t.Fatalf("prompt = %q", prompt)
	}
	got, _ := r.Store.Get(tk.ID)
	if got.Unreported != 0 || countEvents(t, r, tk.ID, "nudged") != 0 {
		t.Errorf("unreported %d, nudges %d", got.Unreported, countEvents(t, r, tk.ID, "nudged"))
	}
	if ev := lastEvent(t, r); ev.Type != "context-low" || !strings.HasPrefix(ev.Summary, "13% of its context left") {
		t.Errorf("event = %+v", ev)
	}
	// Asked once: the next turn end without a report is nudged.
	if _, err := r.TurnEnd(context.Background(), tk.ID, task.Codex, []byte(codexTurn)); err != nil {
		t.Fatal(err)
	}
	if countEvents(t, r, tk.ID, "nudged") != 1 || countEvents(t, r, tk.ID, "context-low") != 1 {
		t.Errorf("events = %v", events(t, r, tk.ID))
	}
}

func TestCodexHighContextIsNudged(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Agent: "codex", Prompt: "x", Cwd: plainDir(t)})
	fake.on("pane read", ok(fixture(t, "codex-context-high.txt")))
	if _, err := r.TurnEnd(context.Background(), tk.ID, task.Codex, []byte(codexTurn)); err != nil {
		t.Fatal(err)
	}
	if countEvents(t, r, tk.ID, "nudged") != 1 || countEvents(t, r, tk.ID, "context-low") != 0 {
		t.Errorf("events = %v", events(t, r, tk.ID))
	}
	// The threshold is the config's.
	r.Config.ContextHandoffAt = 60
	other := running(t, r, SpawnOptions{Agent: "codex", Prompt: "y", Cwd: plainDir(t)})
	if _, err := r.TurnEnd(context.Background(), other.ID, task.Codex, []byte(codexTurn)); err != nil {
		t.Fatal(err)
	}
	if countEvents(t, r, other.ID, "context-low") != 1 {
		t.Errorf("events at 60%% = %v", events(t, r, other.ID))
	}
}

func TestRelaunchListsThePreviousSessionsProcesses(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	fake.on("pane process-info", ok(`{"result":{"process_info":{"shell_pid":100}}}`))
	tk := handedOff(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: plainDir(t)})
	if _, _, err := r.Store.Transition(tk.ID, task.Starting, nil, nil); err != nil {
		t.Fatal(err)
	}
	after := []proc{{PID: 100, PPID: 1, Start: "a", Command: "zsh"}, {PID: 102, PPID: 1, Start: "a", Command: "python3 -m http.server 8765"}}
	r.ProcTable = procTables(runnerTree, after)
	quitsThenIdle(fake)
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	prompt := lastCall(fake, "agent", "prompt")[3]
	if !strings.HasSuffix(prompt, "Still running from the previous session: 102 `python3 -m http.server 8765`. Stop them or reuse them; do not start duplicates.") {
		t.Errorf("prompt = %q", prompt)
	}
	carried := eventsOf(t, r, tk.ID, "processes-carried")
	if len(carried) != 1 || !strings.Contains(carried[0].Summary, "102 `python3 -m http.server 8765`") || carried[0].Attempt != 2 {
		t.Errorf("processes-carried = %+v", carried)
	}
	if len(eventsOf(t, r, tk.ID, "processes-left")) != 0 {
		t.Error("a relaunch reported processes-left news")
	}
}

func TestTurnEndRemindsOfAPendingHandoff(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	compactHook(t, r, tk.ID, "precompact-auto.json")
	request := sessionContext(t, compactHook(t, r, tk.ID, "sessionstart-compact.json"))
	block, err := r.TurnEnd(context.Background(), tk.ID, task.Claude, []byte(`{}`))
	if err != nil || block != request {
		t.Errorf("nudge = %q, %v; want the handoff request", block, err)
	}
}

func TestQuotaSwitchClearsThePendingRequest(t *testing.T) {
	r, _, _ := newTestRunner(t)
	r.Config.QuotaFallback = []config.Fallback{{Agent: "codex"}}
	r.StartLauncher = func(string) (int, error) { return 5151, nil }
	tk := running(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: plainDir(t)})
	compactHook(t, r, tk.ID, "precompact-auto.json")
	compactHook(t, r, tk.ID, "sessionstart-compact.json")
	if err := r.switchAgent(tk, true); err != nil {
		t.Fatal(err)
	}
	if st := r.loadProgress(tk.ID); st.Asked != 0 || st.Armed != 0 || st.Compactions != 1 {
		t.Fatalf("progress after the switch = %+v", st)
	}
	// The fallback session's first unreported turn gets the plain nudge.
	got, _, err := r.Store.Transition(tk.ID, task.Running, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.handoffPending(got) {
		t.Error("the fallback session inherited the handoff request")
	}
}
