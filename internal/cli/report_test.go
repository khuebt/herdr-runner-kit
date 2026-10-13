package cli

import (
	"slices"
	"testing"
)

// A round repeats --question; every one is kept, not just the last.
func TestReportFlagsRepeat(t *testing.T) {
	cmd := newReportCommand()
	if err := cmd.ParseFlags([]string{
		"--question", "Which database?", "--question", "Which host?",
		"--decision", "CSV or JSON? -> CSV", "--decision", "Page size? -> 50",
	}); err != nil {
		t.Fatal(err)
	}
	for flag, want := range map[string][]string{
		"question": {"Which database?", "Which host?"},
		"decision": {"CSV or JSON? -> CSV", "Page size? -> 50"},
	} {
		if got, err := cmd.Flags().GetStringArray(flag); err != nil || !slices.Equal(got, want) {
			t.Errorf("--%s = %q, %v", flag, got, err)
		}
	}
}
