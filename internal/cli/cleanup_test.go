package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func TestCleanupIncludesProjectTasksByDefault(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	t.Setenv("HRK_STATE_DIR", stateDir)
	t.Setenv("HRK_TASK", "")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Chdir(t.TempDir())
	store, err := state.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	tk := &task.Task{
		Kind: "code", Agent: "codex", Prompt: "x", Owner: task.ProjectOwner("demo-app"), Gate: task.GateAuto,
		UseWorktree: true, Worktree: &task.Worktree{Path: t.TempDir(), Branch: "feat/a"}, Status: task.Done,
	}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}
	if out, err := runCLI("cleanup", "--no-fetch"); err != nil || !strings.HasPrefix(out, tk.ID+" ") {
		t.Errorf("cleanup = %q %v", out, err)
	}
	if out, err := runCLI("cleanup", "--no-fetch", "--owner", "/elsewhere"); err != nil || out != "nothing to clean up\n" {
		t.Errorf("cleanup --owner = %q %v", out, err)
	}
}
