package runner

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/config"
	"github.com/khuebt/herdr-runner-kit/internal/harness"
	"github.com/khuebt/herdr-runner-kit/internal/herdr"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "harness", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func adapterFor(t *testing.T, h task.Harness) harness.Adapter {
	t.Helper()
	a, err := harness.For(h)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestDetectUsageLimitFixtures(t *testing.T) {
	claude, codex := adapterFor(t, task.Claude), adapterFor(t, task.Codex)
	if !detectUsageLimit(claude, fixture(t, "usage-limit-claude.txt")) {
		t.Error("claude usage-limit screen not detected")
	}
	if !detectUsageLimit(codex, fixture(t, "usage-limit-codex.txt")) {
		t.Error("codex usage-limit screen not detected")
	}
	negative := fixture(t, "usage-limit-negative.txt")
	if detectUsageLimit(claude, negative) || detectUsageLimit(codex, negative) {
		t.Error("phrases quoted above the last lines were detected")
	}
	// Trailing blank rows below the agent's output do not push it out.
	if !detectUsageLimit(claude, fixture(t, "usage-limit-claude.txt")+strings.Repeat("   \n", 30)) {
		t.Error("claude screen with blank rows below not detected")
	}
	if detectUsageLimit(claude, "❯ \n") || detectUsageLimit(codex, "") {
		t.Error("an ordinary screen was detected")
	}
}

// limitRunner returns a runner whose panes show screen, with the given
// fallbacks configured, and a record of the launchers it started.
func limitRunner(t *testing.T, screen string, fallbacks ...config.Fallback) (*Runner, *fakeHerdr, *[]string) {
	t.Helper()
	r, fake, _ := newTestRunner(t)
	r.Config.QuotaFallback = fallbacks
	fake.on("pane read", ok(screen))
	var mu sync.Mutex
	launched := &[]string{}
	r.StartLauncher = func(id string) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		*launched = append(*launched, id)
		return 5151, nil
	}
	return r, fake, launched
}

func TestNextFallbackSelection(t *testing.T) {
	r, _, _ := newTestRunner(t)
	dir := t.TempDir()
	r.Config.QuotaFallback = []config.Fallback{
		{Agent: "codex"},
		{Agent: "claude:claude-sonnet-5-5", ClaudeConfig: filepath.Join(dir, "missing")},
		{Agent: "claude:claude-sonnet-5-5", ClaudeConfig: dir},
		{Agent: "claude"},
	}
	tests := []struct {
		agent, dir string
		tried      []string
		want       string
	}{
		{"claude", "", nil, "codex"},
		{"codex", "", []string{"claude"}, "claude:claude-sonnet-5-5@" + dir},
		// An entry without a config folder keeps the runner's own.
		{"claude:claude-sonnet-5-5", dir, []string{"claude@" + dir, "codex"}, ""},
		{"claude:claude-sonnet-5-5", dir, []string{"codex"}, "claude@" + dir},
		{"claude:x", "", []string{"codex", "claude:claude-sonnet-5-5@" + dir}, "claude"},
	}
	for _, tt := range tests {
		agent, gotDir, found := r.nextFallback(&task.Task{Agent: tt.agent, ClaudeConfigDir: tt.dir, FallbackFrom: tt.tried})
		got := ""
		if found {
			got = agentKey(agent, gotDir)
		}
		if got != tt.want {
			t.Errorf("next after %s (tried %v) = %q, want %q", tt.agent, tt.tried, got, tt.want)
		}
	}
}

func TestTurnEndSwitchesCodexOnUsageLimit(t *testing.T) {
	r, fake, launched := limitRunner(t, fixture(t, "usage-limit-codex.txt"), config.Fallback{Agent: "claude:claude-sonnet-5-5"})
	tk := running(t, r, SpawnOptions{Kind: "research", Agent: "codex", Prompt: "x", Cwd: plainDir(t)})
	*launched = nil
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error {
		t.CodexThread, t.Unreported = "main", 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	turn := `{"type":"agent-turn-complete","thread-id":"main","input-messages":["x"]}`
	block, err := r.TurnEnd(context.Background(), tk.ID, task.Codex, []byte(turn))
	if err != nil || block != "" {
		t.Fatalf("TurnEnd = %q, %v", block, err)
	}
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Starting || got.Agent != "claude:claude-sonnet-5-5" || got.CodexThread != "" ||
		got.Unreported != 0 || got.LauncherPID != 5151 || !slices.Equal(got.FallbackFrom, []string{"codex"}) {
		t.Errorf("task = %+v", got)
	}
	if !slices.Equal(*launched, []string{tk.ID}) || fake.count("agent", "prompt") != 0 {
		t.Errorf("launchers = %v, prompts = %d", *launched, fake.count("agent", "prompt"))
	}
	if ev := lastEvent(t, r); ev.Type != "agent-switched" || ev.Summary != "codex -> claude:claude-sonnet-5-5@"+tk.ClaudeConfigDir {
		t.Errorf("last event = %+v", ev)
	}
}

func TestUsageLimitWithoutFallbackFails(t *testing.T) {
	r, _, launched := limitRunner(t, fixture(t, "usage-limit-claude.txt"))
	tk := running(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: plainDir(t)})
	child := spawnQueued(t, r, tk.ID)
	*launched = nil
	if block, err := r.TurnEnd(context.Background(), tk.ID, task.Claude, nil); err != nil || block != "" {
		t.Fatalf("TurnEnd = %q, %v", block, err)
	}
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Failed || got.Error != "usage limit reached for "+agentKey(tk.Agent, tk.ClaudeConfigDir)+"; no fallback configured" {
		t.Errorf("task = %s %q", got.Status, got.Error)
	}
	if c, _ := r.Store.Get(child); c.Status != task.Failed {
		t.Errorf("dependent = %s, want failed", c.Status)
	}
	if len(*launched) != 0 {
		t.Errorf("launchers = %v", *launched)
	}
}

func TestUsageLimitExhaustedFallbacksFail(t *testing.T) {
	r, _, launched := limitRunner(t, fixture(t, "usage-limit-codex.txt"), config.Fallback{Agent: "claude"}, config.Fallback{Agent: "codex"})
	tk := running(t, r, SpawnOptions{Agent: "codex", Prompt: "x", Cwd: plainDir(t)})
	*launched = nil
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error {
		t.FallbackFrom = []string{agentKey("claude", t.ClaudeConfigDir)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	tk, _ = r.Store.Get(tk.ID)
	if err := r.switchAgent(tk); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Failed || got.Error != "usage limit reached for codex; every quotaFallback agent was tried" {
		t.Errorf("task = %s %q", got.Status, got.Error)
	}
	if len(*launched) != 0 {
		t.Errorf("launchers = %v", *launched)
	}
}

func TestConcurrentDetectorsSwitchOnce(t *testing.T) {
	r, _, launched := limitRunner(t, fixture(t, "usage-limit-claude.txt"), config.Fallback{Agent: "codex"}, config.Fallback{Agent: "claude:claude-sonnet-5-5"})
	tk := running(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: plainDir(t)})
	*launched = nil
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.switchAgent(tk); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, _ := r.Store.Get(tk.ID)
	if got.Agent != "codex" || len(got.FallbackFrom) != 1 || len(*launched) != 1 {
		t.Errorf("agent %s, fallbackFrom %v, launchers %v", got.Agent, got.FallbackFrom, *launched)
	}
	if n := countEvents(t, r, tk.ID, "agent-switched"); n != 1 {
		t.Errorf("agent-switched events = %d, want 1", n)
	}
}

func TestLaunchSwitchedTaskReusesPane(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	cfgDir := t.TempDir()
	r.Config.QuotaFallback = []config.Fallback{{Agent: "claude:claude-sonnet-5-5", ClaudeConfig: cfgDir}}
	tk := spawnStarting(t, r, SpawnOptions{Agent: "codex", Skill: "ak:cook", Prompt: "plan.md", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	r.StartLauncher = func(string) (int, error) { return 5151, nil }
	tk, _ = r.Store.Get(tk.ID)
	if err := r.switchAgent(tk); err != nil {
		t.Fatal(err)
	}

	// The launcher quits the codex agent and starts Claude in its pane.
	fake.on("agent get", ok(`{"result":{"agent":{"agent":"codex","agent_status":"idle"}}}`))
	fake.on("agent wait", func(args []string) (string, error) {
		if slices.Contains(args, "unknown") {
			fake.on("agent get", ok(`{"result":{"agent":{"agent":"claude","agent_status":"idle"}}}`))
			return herdrErr(herdr.CodeAgentNotRunning)(args)
		}
		return `{"result":{"agent":{"agent_status":"working"}}}`, nil
	})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	if n := fake.count("tab", "create"); n != 1 {
		t.Errorf("tab create calls = %d, want 1", n)
	}
	if keys := fake.call("agent", "send-keys"); !slices.Equal(keys, []string{"agent", "send-keys", "w1:p9", "ctrl+c", "ctrl+c"}) {
		t.Errorf("send-keys = %q", keys)
	}
	if run := fake.call("pane", "run"); len(run) != 4 || run[2] != "w1:p9" || !strings.Contains(run[3], "CLAUDE_CONFIG_DIR="+cfgDir) {
		t.Errorf("pane run = %q", run)
	}
	if fake.count("agent", "start") != 2 || !strings.Contains(strings.Join(lastCall(fake, "agent", "start"), " "), "--kind claude --pane w1:p9") {
		t.Errorf("agent start = %q", lastCall(fake, "agent", "start"))
	}
	prompt := lastCall(fake, "agent", "prompt")
	if !strings.HasPrefix(prompt[3], "/ak:cook plan.md\n\n") || !strings.HasSuffix(prompt[3], resumeNote) {
		t.Errorf("prompt = %q", prompt[3])
	}
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Running || got.ClaudeConfigDir != cfgDir || got.Herdr.PaneID != "w1:p9" {
		t.Errorf("task = %+v", got)
	}
}

func TestLaunchSwitchesOnUsageLimitAtStartup(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	r.Config.QuotaFallback = []config.Fallback{{Agent: "codex"}}
	starts := 0
	fake.on("agent start", func([]string) (string, error) {
		starts++
		screen := "❯ \n"
		if starts == 1 {
			screen = fixture(t, "usage-limit-claude.txt")
		}
		fake.on("pane read", ok(screen))
		return `{"result":{"agent":{"agent_status":"idle"}}}`, nil
	})
	tk := spawnStarting(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Running || got.Agent != "codex" || len(got.FallbackFrom) != 1 || starts != 2 {
		t.Errorf("task = %s %s %v after %d starts", got.Status, got.Agent, got.FallbackFrom, starts)
	}
	if !strings.Contains(lastCall(fake, "agent", "prompt")[3], resumeNote) {
		t.Error("the resent prompt lacks the resume note")
	}
	ev := events(t, r, tk.ID)
	if i := slices.Index(ev, "agent-switched"); i < 0 || i > slices.Index(ev, "started") {
		t.Errorf("events = %v", ev)
	}
}

func TestBackstopSwitchesBlockedAgentAtUsageLimit(t *testing.T) {
	r, fake, launched := limitRunner(t, fixture(t, "usage-limit-claude.txt"), config.Fallback{Agent: "codex"})
	tk := running(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: plainDir(t)})
	*launched = nil
	fake.on("agent wait", ok(`{"result":{"agent":{"agent_status":"blocked"}}}`))
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	r.WatchAgent(ctx, tk.ID)
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Starting || got.Agent != "codex" || got.Stalled || len(*launched) != 1 {
		t.Errorf("task = %s %s stalled %v, launchers %v", got.Status, got.Agent, got.Stalled, *launched)
	}
}

func TestBackstopSwitchesExitedAgentAtUsageLimit(t *testing.T) {
	r, fake, launched := limitRunner(t, fixture(t, "usage-limit-codex.txt"), config.Fallback{Agent: "claude"})
	tk := running(t, r, SpawnOptions{Agent: "codex", Prompt: "x", Cwd: plainDir(t)})
	*launched = nil
	fake.on("agent wait", herdrErr(herdr.CodeAgentNotRunning))
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	r.WatchAgent(ctx, tk.ID)
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Starting || !strings.HasPrefix(agentKey(got.Agent, got.ClaudeConfigDir), "claude") || len(*launched) != 1 {
		t.Errorf("task = %s %s, launchers %v", got.Status, got.Agent, *launched)
	}
}

func spawnQueued(t *testing.T, r *Runner, parent string) string {
	t.Helper()
	c, err := r.Spawn(context.Background(), SpawnOptions{Prompt: "next", Cwd: plainDir(t), After: []string{parent}})
	if err != nil || c.Status != task.Queued {
		t.Fatalf("child = %+v, %v", c, err)
	}
	return c.ID
}

func countEvents(t *testing.T, r *Runner, id, typ string) int {
	t.Helper()
	n := 0
	for _, ev := range events(t, r, id) {
		if ev == typ {
			n++
		}
	}
	return n
}

// lastCall returns the last recorded call starting with prefix.
func lastCall(f *fakeHerdr, prefix ...string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if c := f.calls[i]; len(c) >= len(prefix) && slices.Equal(c[:len(prefix)], prefix) {
			return c
		}
	}
	return nil
}
