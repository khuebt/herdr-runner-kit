package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
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

// A runner session that handed off can no longer report, answer or hand
// off for its task.
func TestReportAndAnswerRefuseAnEndedAttempt(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	t.Setenv("HRK_STATE_DIR", stateDir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	store, err := state.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	tk := &task.Task{Status: task.NeedsHuman, Question: "q?", Owner: "/o", Cwd: t.TempDir(), Attempt: 2, Handoffs: []string{"/h.md"}}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "handoff.md")
	if err := os.WriteFile(file, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HRK_TASK", tk.ID)
	t.Setenv("HRK_ATTEMPT", "")
	if _, err := runCLI("report", "done", "--summary", "old"); err == nil || !strings.Contains(err.Error(), "is on attempt 2; a report from attempt 1 is ignored") {
		t.Errorf("report from attempt 1: %v", err)
	}
	if _, err := runCLI("report", "handoff", "--summary", "s", "--output", file); err == nil || !strings.Contains(err.Error(), "is on attempt 2") {
		t.Errorf("handoff from attempt 1: %v", err)
	}
	t.Setenv("HRK_ATTEMPT", "2")
	if _, err := runCLI("report", "handoff", "--summary", "s", "--output", file); err == nil || !strings.Contains(err.Error(), "already used its one context replacement") {
		t.Errorf("second handoff: %v", err)
	}

	t.Setenv("HRK_TASK", "")
	if _, err := runCLI("report", "handoff", "--task", tk.ID, "--summary", "s", "--output", file); err == nil || !strings.Contains(err.Error(), "only inside a runner") {
		t.Errorf("manual handoff: %v", err)
	}
	if _, err := runCLI("answer", tk.ID+"a1", "use A"); err == nil || err.Error() != "that question was for attempt 1 of "+tk.ID+", which ended" {
		t.Errorf("answer for attempt 1: %v", err)
	}
	if got, _ := store.Get(tk.ID); got.Status != task.NeedsHuman || got.Question != "q?" {
		t.Errorf("a refused call changed the task: %+v", got)
	}
}
