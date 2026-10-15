package harness

import (
	"strings"
	"testing"
	"time"
)

func TestProgressTextIgnoresAHangingTool(t *testing.T) {
	for _, c := range []struct {
		a      Adapter
		prefix string
	}{{claude{}, "working-claude"}, {claude{}, "working-claude-minute"}, {codex{}, "working-codex"}} {
		// The -minute screens cross a minute: "45s", then "1m 5s".
		first := c.a.ProgressText(readFixture(t, c.prefix+"-1.txt"))
		second := c.a.ProgressText(readFixture(t, c.prefix+"-2.txt"))
		if first != second {
			t.Errorf("%s: a hanging tool looks like progress:\n%s\n---\n%s", c.prefix, first, second)
		}
		for _, gone := range []string{"esc to interrupt", "Tip:", "bypass permissions", "Context 6% used", "ctx █"} {
			if strings.Contains(second, gone) {
				t.Errorf("%s: progress text keeps %q:\n%s", c.prefix, gone, second)
			}
		}
	}
	if got := (claude{}).ProgressText(readFixture(t, "working-claude-1.txt")); !strings.Contains(got, "curl -sS --max-time 100 http://10.255.255.1/") ||
		!strings.Contains(got, "spinner 156 tokens") {
		t.Errorf("claude progress text lost the conversation:\n%s", got)
	}
}

func TestProgressTextIgnoresWhereATimerWraps(t *testing.T) {
	// A long command line wraps, and its timer with it: "(3m" at the end of
	// one line and "0s)" on the next. Before it reached a minute the timer
	// fit on the first line.
	wrapped := readFixture(t, "working-claude-wrapped.txt")
	fits := strings.Replace(wrapped, "exited\"; fi (3m\n     0s)", "exited\"; fi (59s)", 1)
	if fits == wrapped {
		t.Fatal("the fixture no longer holds the wrapped timer")
	}
	if a, b := (claude{}).ProgressText(wrapped), (claude{}).ProgressText(fits); a != b {
		t.Errorf("the wrap of a timer looks like progress:\n%s\n---\n%s", a, b)
	}
}

func TestProgressTextSeesStreaming(t *testing.T) {
	for _, c := range []struct {
		a      Adapter
		prefix string
	}{{claude{}, "streaming-claude"}, {codex{}, "streaming-codex"}} {
		if c.a.ProgressText(readFixture(t, c.prefix+"-1.txt")) == c.a.ProgressText(readFixture(t, c.prefix+"-2.txt")) {
			t.Errorf("%s: streaming text is not progress", c.prefix)
		}
	}
	// A spinner whose token count grew is progress too.
	a := "● Thinking\n✽ Pondering… (1m 4s · ↓ 1.2k tokens)\n"
	b := "● Thinking\n✻ Pondering… (1m 9s · ↓ 1.4k tokens)\n"
	if (claude{}).ProgressText(a) == (claude{}).ProgressText(b) {
		t.Error("a growing token count is not progress")
	}
	if (claude{}).ProgressText(a) != (claude{}).ProgressText(strings.Replace(a, "1m 4s", "2m 0s", 1)) {
		t.Error("an elapsed timer counts as progress")
	}
}

func TestShowsRetry(t *testing.T) {
	if !(claude{}).ShowsRetry(readFixture(t, "retry-claude.txt")) {
		t.Error("claude retry screen not detected")
	}
	codexRetrying := "• Reconnecting... 2/5 (12s • esc to interrupt)\n\n› Ask Codex to do anything\n"
	if !(codex{}).ShowsRetry(codexRetrying) {
		t.Error("codex reconnecting line not detected")
	}
	// The phrase in the conversation above the status area is not a retry.
	quoted := "• The log said: stream disconnected - retrying sampling request\n" + strings.Repeat("  └ line\n", 12) + "\n› Ask Codex to do anything\n"
	if (codex{}).ShowsRetry(quoted) {
		t.Error("codex: a quoted retry line far above the status counts")
	}
	for _, name := range []string{"working-claude-1.txt", "working-codex-2.txt", "streaming-claude-1.txt", "streaming-codex-1.txt"} {
		screen := readFixture(t, name)
		if (claude{}).ShowsRetry(screen) || (codex{}).ShowsRetry(screen) {
			t.Errorf("%s: detected a retry", name)
		}
	}
}

func TestSelfResolvingPrompt(t *testing.T) {
	for _, name := range []string{"rm-safety-prompt-claude.txt", "rm-safety-prompt-glob-claude.txt", "rm-safety-prompt-narrow-claude.txt"} {
		p, ok := MatchSelfResolving(claude{}, readFixture(t, name))
		if !ok || p.Timeout != 2*time.Minute || p.Name == "" {
			t.Errorf("%s: %+v, %v", name, p, ok)
		}
		if _, ok := MatchSelfResolving(codex{}, readFixture(t, name)); ok {
			t.Errorf("%s: matched for codex", name)
		}
	}
	for _, name := range []string{"working-claude-1.txt", "usage-limit-claude.txt"} {
		if _, ok := MatchSelfResolving(claude{}, readFixture(t, name)); ok {
			t.Errorf("%s: matched a self-resolving prompt", name)
		}
	}
}

func TestClaudeBackgroundWork(t *testing.T) {
	for name, want := range map[string]int{"stop-background-running-claude.json": 2, "stop-background-none-claude.json": 0} {
		if n, known := ClaudeBackgroundWork([]byte(readFixture(t, name))); !known || n != want {
			t.Errorf("%s: %d %v", name, n, known)
		}
	}
	finished := `{"background_tasks":[{"type":"shell","status":"completed"},{"type":"subagent","status":"running"}]}`
	if n, known := ClaudeBackgroundWork([]byte(finished)); !known || n != 1 {
		t.Errorf("one of two running: %d %v", n, known)
	}
	for _, in := range []string{`{}`, `not json`, `{"background_tasks":null}`} {
		if _, known := ClaudeBackgroundWork([]byte(in)); known {
			t.Errorf("%s: known", in)
		}
	}
}
