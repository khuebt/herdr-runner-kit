package cli

import (
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func TestPeekRefusesAClosedRunner(t *testing.T) {
	store, tk := stateWithTask(t)
	if _, err := store.Update(tk.ID, func(t *task.Task) error {
		t.Herdr.PaneID, t.Herdr.Closed = "w1:p1", true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI("peek", tk.ID); err == nil || !strings.Contains(err.Error(), "has no open runner") {
		t.Errorf("peek = %v", err)
	}
	if _, err := runCLI("peek", tk.ID, "--lines", "500"); err == nil || !strings.Contains(err.Error(), "between 1 and 200") {
		t.Errorf("peek --lines 500 = %v", err)
	}
}
