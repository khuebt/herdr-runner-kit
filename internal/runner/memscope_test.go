package runner

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/harness"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func TestScopeCommand(t *testing.T) {
	unit := scopeUnit("t7", 2, time.Unix(1760000000, 0))
	if unit != "hrk-t7-2-1760000000" {
		t.Errorf("unit = %q", unit)
	}
	want := `exec systemd-run --user --scope --quiet --collect '--unit=hrk-t7-2-1760000000' ` +
		`-p 'MemoryMax=25%' -p 'MemorySwapMax=0' -p 'OOMPolicy=continue' -- "$SHELL"`
	if got := scopeCommand(unit, "25%"); got != want {
		t.Errorf("scopeCommand =\n%s\nwant\n%s", got, want)
	}
}

// scopedLaunch launches a runner on agent and returns the herdr calls, the
// agent's argv as herdr got it, the argv its adapter builds, and the
// started event.
func scopedLaunch(t *testing.T, agent string, scope func(context.Context) string) (calls [][]string, argv, plain []string, started string) {
	t.Helper()
	r, fake, _ := newTestRunner(t)
	r.Scope = scope
	tk := spawnStarting(t, r, SpawnOptions{Kind: "research", Agent: agent, Prompt: "x", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	start := fake.call("agent", "start")
	sep := slices.Index(start, "--")
	if sep < 0 {
		t.Fatalf("agent start = %q", start)
	}
	spec, _ := task.ParseAgentSpec(agent)
	adapter, _ := harness.For(spec.Harness)
	plain = adapter.LaunchArgs(harness.LaunchOptions{Spec: spec, Bin: r.Bin, Dir: r.Store.TaskDir(tk.ID)})
	all, _, _ := r.Store.ReadEventsSince(0)
	for _, ev := range all {
		if ev.Type == "started" {
			started = ev.Summary
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return slices.Clone(fake.calls), start[sep+1:], plain, started
}

// scopeRun returns the pane run that starts a memory scope, and its index.
func scopeRun(calls [][]string) ([]string, int) {
	for i, c := range calls {
		if len(c) == 4 && c[0] == "pane" && c[1] == "run" && strings.Contains(c[3], "systemd-run") {
			return c, i
		}
	}
	return nil, -1
}

func TestLaunchMemoryScope(t *testing.T) {
	for _, agent := range []string{"claude", "codex", piAgents[task.Pi]} {
		for _, scope := range []func(context.Context) string{nil, func(context.Context) string { return "" }} {
			calls, argv, plain, started := scopedLaunch(t, agent, scope)
			if run, _ := scopeRun(calls); run != nil || !slices.Equal(argv, plain) || strings.Contains(started, "memory scope") {
				t.Errorf("%s without a cap: scope run %q, argv %q, want %q; started %q", agent, run, argv, plain, started)
			}
		}

		calls, argv, plain, started := scopedLaunch(t, agent, func(context.Context) string { return "512M" })
		if !slices.Equal(argv, plain) {
			t.Errorf("%s scoped argv = %q, want %q", agent, argv, plain)
		}
		run, at := scopeRun(calls)
		if run == nil {
			t.Fatalf("%s: no scope run in %q", agent, calls)
		}
		unit, _, _ := strings.Cut(strings.TrimPrefix(run[3], "exec systemd-run --user --scope --quiet --collect '--unit="), "'")
		if !strings.HasPrefix(unit, "hrk-t1-1-") || run[2] != "w1:p9" || run[3] != scopeCommand(unit, "512M") {
			t.Errorf("%s scope run = %q", agent, run)
		}
		if start := slices.IndexFunc(calls, func(c []string) bool { return len(c) > 1 && c[0] == "agent" && c[1] == "start" }); start < at {
			t.Errorf("%s: agent start (%d) before the scope run (%d)", agent, start, at)
		}
		if !strings.Contains(started, "memory scope "+unit+".scope") {
			t.Errorf("%s started = %q", agent, started)
		}
	}
}

func TestLaunchWaitsForTheScopedShell(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	r.Scope = func(context.Context) string { return "25%" }
	busy := 3
	fake.on("agent start", func([]string) (string, error) {
		if busy > 0 {
			busy--
			return "", herdrError(codePaneBusy)
		}
		return `{"result":{"agent":{"agent":"claude","agent_status":"idle"}}}`, nil
	})
	tk := spawnStarting(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Store.Get(tk.ID); got.Status != task.Running || fake.count("agent", "start") != 4 {
		t.Errorf("status %s after %d starts", got.Status, fake.count("agent", "start"))
	}
}
