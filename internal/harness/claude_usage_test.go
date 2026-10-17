package harness

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// The fixture's counts, by hand: call m1 is two rows of one message id (the
// first with 5 output tokens, the second with 50) writing 1000 tokens to the
// 1-hour cache; m2 reads 1000 and writes 200 to the 5-minute cache; the
// synthetic row, the request blob and the broken line count nothing. The
// subagent and the workflow agent under subagents/workflows/wf_x make one
// call each. Units: m1 10 + 1000×2 + 50×5 = 2260, m2 5 + 1000×0.1 +
// 200×1.25 + 20×5 = 455, the subagent 3 + 500×0.1 + 100×1.25 + 10×5 = 228,
// the workflow agent 2 + 4×5 = 22.
var fixtureUsage = task.Usage{
	Calls: 4, Input: 20, CacheRead: 1500, CacheWrite: 1300, CacheWrite1h: 1000, Output: 84,
	MaxContext: 1205, Subagents: 2, Units: 2965, SubagentUnits: 250,
}

func fixtureTranscript() string { return filepath.Join("testdata", "transcript-claude", "s1.jsonl") }

func TestClaudeUsage(t *testing.T) {
	u, err := claude{}.Usage([]string{fixtureTranscript()}, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if u.At.IsZero() {
		t.Error("At is not set")
	}
	u.At = time.Time{}
	if *u != fixtureUsage {
		t.Errorf("usage = %+v, want %+v", *u, fixtureUsage)
	}
}

func TestClaudeUsageWindow(t *testing.T) {
	from := time.Date(2026, 10, 17, 1, 5, 0, 0, time.UTC)
	u, err := ClaudeUsage([]string{fixtureTranscript()}, from, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// Only m2 is after from; the subagents' calls are before it.
	if u.Calls != 1 || u.Units != 455 || u.Subagents != 0 {
		t.Errorf("usage from %s = %+v, want m2 only", from, *u)
	}
}

func TestClaudeUsageMissingTranscript(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone.jsonl")
	if u, err := ClaudeUsage([]string{missing}, time.Time{}, time.Time{}); err == nil || u != nil {
		t.Errorf("only a missing transcript: usage %+v, err %v; want nil and an error", u, err)
	}
	u, err := ClaudeUsage([]string{missing, fixtureTranscript()}, time.Time{}, time.Time{})
	if err != nil || u.Units != fixtureUsage.Units {
		t.Errorf("a missing and a readable transcript: usage %+v, err %v; want the readable one's", u, err)
	}
}

func TestClaudeTurnWritten(t *testing.T) {
	const (
		toolCall = `{"type":"assistant","message":{"id":"m1","stop_reason":"tool_use","content":[{"type":"tool_use"}]}}` + "\n"
		result   = `{"type":"user","message":{"content":[{"type":"tool_result"}]}}` + "\n"
		reply    = `{"type":"assistant","message":{"id":"m2","stop_reason":"end_turn","content":[{"type":"text"}]}}` + "\n"
		blob     = `{"type":"api-request","message":{"stop_reason":"tool_use"},"x":"assistant"}` + "\n"
	)
	path := filepath.Join(t.TempDir(), "s.jsonl")
	tests := []struct {
		name, rows string
		want       bool
	}{
		{"tool call still last", toolCall + result + blob, false},
		{"closing reply written", toolCall + result + reply, true},
		{"no call", result, true},
	}
	for _, tt := range tests {
		if err := os.WriteFile(path, []byte(tt.rows), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := (claude{}).TurnWritten(path); got != tt.want {
			t.Errorf("%s: TurnWritten = %v, want %v", tt.name, got, tt.want)
		}
	}
	if !(claude{}).TurnWritten(filepath.Join(t.TempDir(), "gone.jsonl")) {
		t.Error("a missing transcript waits")
	}
}

func TestOtherHarnessesHaveNoUsage(t *testing.T) {
	for _, a := range All() {
		if a.Harness() == task.Claude {
			continue
		}
		if u, err := a.Usage([]string{fixtureTranscript()}, time.Time{}, time.Time{}); u != nil || err != nil {
			t.Errorf("%s: usage %+v, err %v; want nil", a.Harness(), u, err)
		}
	}
}

func TestUsageString(t *testing.T) {
	u := task.Usage{Calls: 53, MaxContext: 187_000, Units: 1_400_000, Subagents: 2, SubagentUnits: 300_000}
	if got, want := u.String(), "53 calls, max context 187k, 1.4M units (2 subagents, 300k)"; got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
}
