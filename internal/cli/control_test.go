package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func TestPromptTextRefusesTooLong(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(strings.Repeat("x", maxPromptBytes-1) + "\n"))
	if got, err := promptText(cmd, nil, true); err != nil || len(got) != maxPromptBytes-1 {
		t.Fatalf("at the limit: %d bytes, %v", len(got), err)
	}
	cmd.SetIn(strings.NewReader(strings.Repeat("x", maxPromptBytes+1)))
	if _, err := promptText(cmd, nil, true); err == nil {
		t.Fatal("over the limit: no error")
	}
}

func TestTabState(t *testing.T) {
	now := time.Date(2026, 10, 13, 16, 0, 0, 0, time.Local)
	open := task.Herdr{PaneID: "w1:p1"}
	for _, c := range []struct {
		t    task.Task
		want string
	}{
		{task.Task{Status: task.Queued}, "-"},
		{task.Task{Status: task.Running, Herdr: open}, "open"},
		{task.Task{Status: task.Done, FinishedAt: now.Add(-15 * time.Minute), Herdr: open}, "open, closes 16:15"},
		{task.Task{Status: task.Done, FinishedAt: now.Add(-time.Hour), Herdr: open}, "open, due"},
		{task.Task{Status: task.Done, FinishedAt: now.Add(-time.Hour), Herdr: open, Keep: true}, "kept"},
		{task.Task{Status: task.Done, Herdr: task.Herdr{PaneID: "w1:p1", Closed: true}}, "closed"},
	} {
		if got := tabState(&c.t, now); got != c.want {
			t.Errorf("tabState(%s) = %q, want %q", c.t.Status, got, c.want)
		}
	}
}

func TestKeepLine(t *testing.T) {
	now := time.Date(2026, 10, 13, 16, 0, 0, 0, time.Local)
	tk := task.Task{ID: "t3", Status: task.Done, FinishedAt: now.Add(-15 * time.Minute), Herdr: task.Herdr{PaneID: "w1:p1"}}
	if got := keepLine(&tk, now); got != "t3 closes on its own at 16:15" {
		t.Errorf("off: %q", got)
	}
	tk.Keep = true
	if got := keepLine(&tk, now); got != "t3 kept open; close it with stop t3" {
		t.Errorf("on: %q", got)
	}
	tk.Keep, tk.Herdr.Closed = false, true
	if got := keepLine(&tk, now); got != "t3 closed now" {
		t.Errorf("closed: %q", got)
	}
}

func TestBranchNote(t *testing.T) {
	got := branchNote("hrk/t1-x", []string{"feat/CSD-1-a", "main"})
	want := "note: branch hrk/t1-x is the kit's default; recent branches here: feat/CSD-1-a, main. " +
		"Pass --branch to follow the repository's convention."
	if got != want {
		t.Errorf("note = %q", got)
	}
	if got := branchNote("t1-x", nil); got != "note: branch t1-x is the kit's default." {
		t.Errorf("note without branches = %q", got)
	}
}
