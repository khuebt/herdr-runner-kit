package runner

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/herdr"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func harnessFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "harness", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestPromptExcerpt(t *testing.T) {
	screen := "\n● Bash(curl -H 'Authorization: Bearer abcdefghijklmnop' …)\n\n Allow it?\n" +
		strings.Repeat("filler line\n", 3) + " ❯ 1. Yes\n   2. No\n\n"
	got := promptExcerpt(screen)
	if got != "Allow it? / filler line / filler line / filler line / ❯ 1. Yes / 2. No" {
		t.Errorf("excerpt = %q", got)
	}
	long := promptExcerpt(strings.Repeat("word ", 100) + "\nBearer abcdefghijklmnopqrstuvwxyz0123\n")
	if n := len([]rune(long)); n > maxExcerpt || !strings.HasSuffix(long, "…") {
		t.Errorf("long excerpt: %d runes %q", n, long)
	}
	if got := promptExcerpt("token=abcd1234efgh5678\n"); strings.Contains(got, "abcd1234") {
		t.Errorf("excerpt shows a secret: %q", got)
	}
	if got := blockedSummary("\n\n"); got != "blocked on a prompt in its pane" {
		t.Errorf("empty screen = %q", got)
	}
}

// blockedRunner returns a running Claude task and a fake herdr whose agent
// waits answer in turn from waits; once they run out, the task finishes, so
// WatchAgent returns.
func blockedRunner(t *testing.T, screen string, waits ...func([]string) (string, error)) (*Runner, *task.Task) {
	t.Helper()
	r, fake, _ := newTestRunner(t)
	selfResolveGrace = 0
	t.Cleanup(func() { selfResolveGrace = 30 * time.Second })
	tk := running(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: plainDir(t)})
	fake.on("pane read", ok(screen))
	var n atomic.Int32
	fake.on("agent wait", func(args []string) (string, error) {
		i := int(n.Add(1)) - 1
		if i < len(waits) {
			return waits[i](args)
		}
		if _, _, err := r.Store.SetStatus(tk.ID, task.Done); err != nil {
			t.Error(err)
		}
		return herdrErr(herdr.CodeTimeout)(args)
	})
	return r, tk
}

func status(s string) func([]string) (string, error) {
	return ok(`{"result":{"agent":{"agent_status":"` + s + `"}}}`)
}

func TestSelfResolvingPromptThatClearsIsQuiet(t *testing.T) {
	r, tk := blockedRunner(t, harnessFixture(t, "rm-safety-prompt-claude.txt"), status("blocked"), status("working"))
	r.WatchAgent(context.Background(), tk.ID)
	ev := events(t, r, tk.ID)
	if slices.Contains(ev, "stalled") || !slices.Contains(ev, "blocked") || !slices.Contains(ev, "unstalled") {
		t.Fatalf("events = %v", ev)
	}
	if got := eventsOf(t, r, tk.ID, "blocked")[0].Summary; got != "Claude's safety prompt; auto-denies in 2 min" {
		t.Errorf("blocked summary = %q", got)
	}
	if got := eventsOf(t, r, tk.ID, "unstalled")[0].Summary; !strings.HasPrefix(got, "unblocked after ") {
		t.Errorf("unstalled summary = %q", got)
	}
	if got, _ := r.Store.Get(tk.ID); got.Stalled {
		t.Error("the task stays stalled")
	}
}

func TestSelfResolvingPromptThatStaysIsNews(t *testing.T) {
	var waitedFor string
	r, tk := blockedRunner(t, harnessFixture(t, "rm-safety-prompt-claude.txt"), status("blocked"),
		func(args []string) (string, error) {
			waitedFor = args[4]
			return herdrErr(herdr.CodeTimeout)(args)
		}, status("working"))
	r.WatchAgent(context.Background(), tk.ID)
	stalled := eventsOf(t, r, tk.ID, "stalled")
	if len(stalled) != 1 || !strings.Contains(stalled[0].Summary, "blocked on a prompt: ") ||
		!strings.Contains(stalled[0].Summary, "Do you want to proceed? / ❯ 1. Yes / 2. No / Esc to cancel · Tab to amend") {
		t.Fatalf("stalled = %+v", stalled)
	}
	if ms, err := strconv.Atoi(waitedFor); err != nil || ms < 119000 || ms > 120000 {
		t.Errorf("waited %s ms for the prompt to clear", waitedFor)
	}
	if ev := events(t, r, tk.ID); !slices.Contains(ev, "unstalled") {
		t.Errorf("events = %v", ev)
	}
}

func TestOtherPromptIsNewsAtOnce(t *testing.T) {
	r, tk := blockedRunner(t, "\n Trust this MCP server?\n ❯ 1. Yes\n   2. No\n", status("blocked"), status("idle"))
	r.WatchAgent(context.Background(), tk.ID)
	stalled := eventsOf(t, r, tk.ID, "stalled")
	if len(stalled) != 1 || stalled[0].Summary != "blocked on a prompt: Trust this MCP server? / ❯ 1. Yes / 2. No" {
		t.Fatalf("stalled = %+v", stalled)
	}
	if unstalled := eventsOf(t, r, tk.ID, "unstalled"); len(unstalled) != 1 {
		t.Errorf("unstalled = %+v", unstalled)
	}
}

func TestSelfResolvingPromptIsNewsForALaterWait(t *testing.T) {
	// The wait that saw the prompt ended; a later one finds the task still
	// blocked past the prompt's timeout.
	r, tk := blockedRunner(t, harnessFixture(t, "rm-safety-prompt-claude.txt"), status("working"))
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error {
		t.Stalled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.Append(state.Event{TS: time.Now().Add(-3 * time.Minute), Task: tk.ID, Owner: tk.Owner, Type: "blocked", Summary: "Claude's safety prompt; auto-denies in 2 min"}); err != nil {
		t.Fatal(err)
	}
	r.WatchAgent(context.Background(), tk.ID)
	if stalled := eventsOf(t, r, tk.ID, "stalled"); len(stalled) != 1 || !strings.Contains(stalled[0].Summary, "Do you want to proceed?") {
		t.Fatalf("stalled = %+v", stalled)
	}
	if unstalled := eventsOf(t, r, tk.ID, "unstalled"); len(unstalled) != 1 {
		t.Errorf("unstalled = %+v", unstalled)
	}
}

func TestSelfResolvingPromptRidesOutAHerdrError(t *testing.T) {
	r, tk := blockedRunner(t, harnessFixture(t, "rm-safety-prompt-claude.txt"),
		status("blocked"), herdrErr("server_unavailable"), status("working"))
	r.WatchAgent(context.Background(), tk.ID)
	if ev := events(t, r, tk.ID); slices.Contains(ev, "stalled") || !slices.Contains(ev, "unstalled") {
		t.Errorf("events = %v", ev)
	}
}

func TestSelfResolvingTimeoutDoesNotCrossHandoff(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: plainDir(t)})
	fake.on("pane read", ok("Claude Code will automatically deny this request in 1:42"))
	r.onBlocked(context.Background(), tk)
	file := handoffFile(t, r, tk.ID)
	fake.on("pane read", func([]string) (string, error) {
		if _, err := r.Handoff(tk.ID, ReportOptions{Summary: "continue", Outputs: []string{file}}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := r.Store.SetStatus(tk.ID, task.Running); err != nil {
			t.Fatal(err)
		}
		return "Claude Code will automatically block this request in 0:00", nil
	})
	before := countEvents(t, r, tk.ID, "stalled")
	if err := r.awaitSelfResolve(context.Background(), tk, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if countEvents(t, r, tk.ID, "stalled") != before {
		t.Fatal("stale countdown escalated")
	}
}

func TestAutoBlockPromptGetsGrace(t *testing.T) {
	screen := strings.ReplaceAll(harnessFixture(t, "rm-safety-prompt-narrow-claude.txt"), "deny", "block")
	r, tk := blockedRunner(t, screen, status("blocked"), status("working"))
	r.WatchAgent(context.Background(), tk.ID)
	if countEvents(t, r, tk.ID, "stalled") != 0 || countEvents(t, r, tk.ID, "blocked") != 1 {
		t.Fatal("auto-block did not get grace")
	}
}
