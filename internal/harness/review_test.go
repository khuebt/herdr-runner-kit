package harness

import (
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func TestDefaultReviewer(t *testing.T) {
	bin := func(h task.Harness) string {
		a, err := For(h)
		if err != nil {
			t.Fatal(err)
		}
		return a.Checks().Bin
	}
	both := map[string]bool{bin(task.Claude): true, bin(task.Codex): true}
	onlyClaude := map[string]bool{bin(task.Claude): true}
	onlyCodex := map[string]bool{bin(task.Codex): true}
	none := map[string]bool{}
	for _, tc := range []struct {
		name   string
		writer string
		have   map[string]bool
		want   string
	}{
		{"claude sonnet with codex", "claude:claude-sonnet-5-5:high", both, "codex::high"},
		{"claude opus without codex", "claude:claude-opus-5-5:high", onlyClaude, "claude:claude-sonnet-5-5:high"},
		{"claude sonnet without codex", "claude:claude-sonnet-5-5:high", none, "claude:claude-opus-5-5:high"},
		{"codex with claude", "codex", onlyClaude, "claude:claude-opus-5-5:high"},
		{"codex without claude", "codex", onlyCodex, "codex::high"},
		{"pi with claude", "pi:openai-codex/gpt-6-luna:low", onlyClaude, "claude:claude-opus-5-5:high"},
		{"pi without claude", "pi:openai-codex/gpt-6-luna:low", none, "pi:openai-codex/gpt-6-luna:high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writer, err := task.ParseAgentSpec(tc.writer)
			if err != nil {
				t.Fatal(err)
			}
			got := DefaultReviewer(writer, func(b string) bool { return tc.have[b] })
			if got.String() != tc.want {
				t.Fatalf("DefaultReviewer(%s) = %s, want %s", tc.writer, got, tc.want)
			}
		})
	}
}
