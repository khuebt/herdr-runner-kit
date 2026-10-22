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
	}{{claude{}, "working-claude"}, {claude{}, "working-claude-minute"}, {claude{}, "working-claude-blink"}, {codex{}, "working-codex"}} {
		// The -minute screens cross a minute: "45s", then "1m 5s"; in the
		// -blink screens the running tool's bullet blinked off.
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

// Claude's mode line counts background shells and agents, but its agent
// switcher hint, drawn all the time, is no background work.
func TestClaudeModeLineIgnoresTheAgentsHint(t *testing.T) {
	for line, want := range map[string]bool{
		"  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← 2 agents":   false,
		"  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents": false,
		"  ⏵⏵ auto mode on · 1 shell · ← 2 agents":                       true,
		"  ⏵⏵ bypass permissions on · 2 shells · ← for agents":           true,
	} {
		got, err := claude{}.WorksInBackground(func(int) (string, error) { return "❯ \n" + line + "\n", nil })
		if err != nil || got != want {
			t.Errorf("%q: %v, %v", line, got, err)
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

func TestClaudeBackgroundEnding(t *testing.T) {
	in := `{"background_tasks":[{"type":"shell","status":"running"},{"type":"subagent","status":"completed"},{"type":"subagent","status":"running"}]}`
	if n, known := ClaudeBackgroundEnding([]byte(in)); !known || n != 1 {
		t.Errorf("one subagent running beside a shell: %d %v", n, known)
	}
	shell := `{"background_tasks":[{"type":"shell","status":"running"}]}`
	if n, known := ClaudeBackgroundEnding([]byte(shell)); !known || n != 0 {
		t.Errorf("a running shell counted: %d %v", n, known)
	}
	for _, kind := range []string{"workflow", "MCP task", "cloud session", "teammate"} {
		in := `{"background_tasks":[{"type":"` + kind + `","status":"running"}]}`
		if n, known := ClaudeBackgroundEnding([]byte(in)); !known || n != 1 {
			t.Errorf("a running %s did not count: %d %v", kind, n, known)
		}
	}
	for _, kind := range []string{"monitor", "dream", "auto-mode scan", "memory import"} {
		in := `{"background_tasks":[{"type":"` + kind + `","status":"running"}]}`
		if n, _ := ClaudeBackgroundEnding([]byte(in)); n != 0 {
			t.Errorf("a running %s counted: %d", kind, n)
		}
	}
	if _, known := ClaudeBackgroundEnding([]byte(`{}`)); known {
		t.Error("no list is known")
	}
}

func TestPiProgressText(t *testing.T) {
	progress := func(a Adapter, name string) string { return a.ProgressText(readFixture(t, name)) }
	// The model ran its command between these two frames.
	if progress(piAgent, "working-pi-1.txt") == progress(piAgent, "working-pi-2.txt") {
		t.Error("pi: a command that started is not progress")
	}
	working := readFixture(t, "working-pi-3.txt")
	spun := strings.Replace(working, "── ⠦ Working", "── ⠙ Working", 1)
	if spun == working {
		t.Fatal("the fixture no longer holds the spinner")
	}
	if a, b := piAgent.ProgressText(working), piAgent.ProgressText(spun); a != b {
		t.Errorf("pi: a spinner frame looks like progress:\n%s\n---\n%s", a, b)
	}
	// Only the running command's "Elapsed" counter changed.
	first, second := progress(piAgent, "longcmd-pi-1.txt"), progress(piAgent, "longcmd-pi-2.txt")
	if first != second {
		t.Errorf("pi: a running command looks like progress:\n%s\n---\n%s", first, second)
	}
	if !strings.Contains(second, "$ sleep 12; echo slept") {
		t.Errorf("pi progress text lost the conversation:\n%s", second)
	}
	for _, name := range []string{"longcmd-pi-2.txt", "compacting-pi.txt", "working-pi-3.txt"} {
		got := progress(piAgent, name)
		for _, gone := range []string{"Working", "Compacting context", "probe-repo (master)", "gpt-6-luna"} {
			if strings.Contains(got, gone) {
				t.Errorf("%s: progress text keeps %q:\n%s", name, gone, got)
			}
		}
	}
	if progress(piAgent, "compacting-pi.txt") == progress(piAgent, "compacted-pi.txt") {
		t.Error("pi: a finished compaction is not progress")
	}
}

func TestOmpProgressText(t *testing.T) {
	progress := func(name string) string { return ompAgent.ProgressText(readFixture(t, name)) }
	// Only the band's spinner and elapsed time changed.
	first, second := progress("longcmd-omp-1.txt"), progress("longcmd-omp-2.txt")
	if first != second {
		t.Errorf("omp: a running command looks like progress:\n%s\n---\n%s", first, second)
	}
	for _, kept := range []string{"$ sleep 12; echo slept", "⤵ 28K"} {
		if !strings.Contains(second, kept) {
			t.Errorf("omp progress text lost %q:\n%s", kept, second)
		}
	}
	if second == progress("longcmd-omp-6.txt") {
		t.Error("omp: command output is not progress")
	}
	if progress("async-job-omp-1.txt") == progress("async-job-omp-5.txt") {
		t.Error("omp: a finished background job is not progress")
	}
	for _, name := range []string{"longcmd-omp-2.txt", "autocompact-omp-1.txt", "autocompact-omp-3.txt"} {
		got := progress(name)
		for _, gone := range []string{"⎋", "Running requested shell commands", "Auto server compaction", "GPT-6-Luna"} {
			if strings.Contains(got, gone) {
				t.Errorf("%s: progress text keeps %q:\n%s", name, gone, got)
			}
		}
	}
}

func TestPiShowsRetry(t *testing.T) {
	piRetrying := strings.Replace(readFixture(t, "longcmd-pi-2.txt"), "── ⠹ Working", "── ⠹ Retrying (1/3) in 2s... (esc to cancel)", 1)
	ompRetrying := strings.Replace(readFixture(t, "longcmd-omp-2.txt"), "\n ⠦ 4s >", "\n ⠦ Retrying (1/3) in 2s… (esc to cancel)\n ⠦ 4s >", 1)
	for name, c := range map[string]struct {
		a      Adapter
		screen string
	}{"pi": {piAgent, piRetrying}, "omp": {ompAgent, ompRetrying}} {
		if !c.a.ShowsRetry(c.screen) {
			t.Errorf("%s retry line not detected", name)
		}
		if strings.Contains(c.a.ProgressText(c.screen), "Retrying") {
			t.Errorf("%s progress text keeps the retry line", name)
		}
	}
	for _, name := range []string{"working-pi-1.txt", "working-pi-2.txt", "working-pi-3.txt", "longcmd-omp-1.txt", "longcmd-omp-2.txt", "longcmd-omp-6.txt"} {
		screen := readFixture(t, name)
		if piAgent.ShowsRetry(screen) || ompAgent.ShowsRetry(screen) {
			t.Errorf("%s: detected a retry", name)
		}
	}
}

func TestClaudeInputState(t *testing.T) {
	prompt := "/ak:research queues in the billing service and report which ones can drop messages"
	cases := []struct {
		name, screen string
		want         InputState
	}{
		{"pasted", readFixture(t, "input-pasted-claude.txt"), InputHolds},
		{"typed", readFixture(t, "input-typed-claude.txt"), InputHolds},
		{"empty", readFixture(t, "working-claude-1.txt"), InputEmpty},
		{"no box", "❯ \n", InputUnknown},
		{"other text", strings.Replace(readFixture(t, "input-typed-claude.txt"), "/ak:research queues", "something else", 1), InputUnknown},
	}
	for _, c := range cases {
		if got := (claude{}).InputState(c.screen, prompt); got != c.want {
			t.Errorf("%s: InputState = %v, want %v", c.name, got, c.want)
		}
	}
	for _, a := range []Adapter{codex{}, piAgent, ompAgent} {
		if got := a.InputState(readFixture(t, "input-pasted-claude.txt"), prompt); got != InputUnknown {
			t.Errorf("%s: InputState = %v, want unknown", a.Harness(), got)
		}
	}
}

func TestClaudeInputIsOnlyTheWholeText(t *testing.T) {
	prompt := "/ak:research queues in the billing service and report which ones can drop messages"
	typed := readFixture(t, "input-typed-claude.txt")
	cases := []struct {
		name, screen, text string
		want               bool
	}{
		{"exact", typed, prompt, true},
		{"pasted placeholder", readFixture(t, "input-pasted-claude.txt"), prompt, false},
		{"same prefix, other tail", typed, prompt[:50] + " and delete everything", false},
		{"text is a prefix of the box", typed, prompt[:50], false},
		{"empty box", readFixture(t, "working-claude-1.txt"), prompt, false},
		{"no box", "❯ \n", prompt, false},
	}
	for _, c := range cases {
		if got := (claude{}).InputIs(c.screen, c.text); got != c.want {
			t.Errorf("%s: InputIs = %v, want %v", c.name, got, c.want)
		}
	}
	for _, a := range []Adapter{codex{}, piAgent, ompAgent} {
		if a.InputIs(typed, prompt) {
			t.Errorf("%s: InputIs = true for an input that is not read", a.Harness())
		}
	}
}
