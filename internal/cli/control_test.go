package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/khuebt/herdr-runner-kit/internal/config"
	"github.com/khuebt/herdr-runner-kit/internal/herdr"
	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/state"
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
		if got := tabState(&c.t, now, 0); got != c.want {
			t.Errorf("tabState(%s) = %q, want %q", c.t.Status, got, c.want)
		}
	}
	kept := task.Task{Status: task.Done, FinishedAt: now.Add(-time.Hour), Herdr: open, Keep: true}
	if got := tabState(&kept, now, 4*time.Hour); got != "kept until 19:00" {
		t.Errorf("kept with a lifetime: %q", got)
	}
}

func TestKeepLine(t *testing.T) {
	now := time.Date(2026, 10, 13, 16, 0, 0, 0, time.Local)
	tk := task.Task{ID: "t3", Status: task.Done, FinishedAt: now.Add(-15 * time.Minute), Herdr: task.Herdr{PaneID: "w1:p1"}}
	if got := keepLine(&tk, now, 0); got != "t3 closes on its own at 16:15" {
		t.Errorf("off: %q", got)
	}
	tk.Keep = true
	if got := keepLine(&tk, now, 0); got != "t3 kept open; close it with stop t3" {
		t.Errorf("on: %q", got)
	}
	if got := keepLine(&tk, now, 4*time.Hour); got != "t3 kept open until 19:45; close it sooner with stop t3" {
		t.Errorf("on with a lifetime: %q", got)
	}
	running := task.Task{ID: "t4", Status: task.Running, Keep: true, Herdr: task.Herdr{PaneID: "w1:p2"}}
	if got := keepLine(&running, now, 90*time.Minute); got != "t4 kept open for 1h30m after it finishes; close it sooner with stop t4" {
		t.Errorf("running with a lifetime: %q", got)
	}
	tk.Keep, tk.Herdr.Closed = false, true
	if got := keepLine(&tk, now, 0); got != "t3 closed now" {
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

func TestStopManyIDs(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	t.Setenv("HRK_STATE_DIR", stateDir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HRK_TASK", "")
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	for _, f := range []string{"half-done.go", "plans/notes.md"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, f), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store, err := state.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := store.Create(&task.Task{Status: task.Running, Owner: "/o", Cwd: repo, RepoRoot: repo}); err != nil {
			t.Fatal(err)
		}
	}
	out, err := runCLI("stop", "t1", "t2", "bad")
	if err == nil || !strings.Contains(err.Error(), "could not stop bad") {
		t.Fatalf("stop with a bad id: %v", err)
	}
	if !strings.Contains(out, "t1 stopped\n") || !strings.Contains(out, "t2 stopped\n") {
		t.Errorf("output:\n%s", out)
	}
	if want := "note: 1 untracked files in " + repo + ", which the runner may have left: half-done.go\n"; !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
	for _, id := range []string{"t1", "t2"} {
		if got, err := store.Get(id); err != nil || got.Status != task.Stopped {
			t.Errorf("%s = %+v, %v", id, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, "half-done.go")); err != nil {
		t.Errorf("stop deleted the runner's file: %v", err)
	}
	if _, err := runCLI("stop", "t9"); err == nil || strings.Contains(err.Error(), "could not stop") {
		t.Errorf("one bad id: %v", err)
	}
}

// TestGCOwners closes only the finished runners of the selected owners:
// by default this main session's and the projects', never another main
// session's.
func TestGCOwners(t *testing.T) {
	for _, tc := range []struct {
		name   string
		owners runner.Owners
		want   []string
	}{
		{"default", runner.Owners{Owner: "/a", Projects: true}, []string{"t1", "t3"}},
		{"owner", runner.Owners{Owner: "/b"}, []string{"t2"}},
		{"all", runner.Owners{}, []string{"t1", "t2", "t3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := state.Open(filepath.Join(t.TempDir(), "state"))
			if err != nil {
				t.Fatal(err)
			}
			done := time.Now().Add(-time.Hour)
			for i, owner := range []string{"/a", "/b", "project:web"} {
				tk := &task.Task{
					Owner: owner, Status: task.Done, FinishedAt: done,
					Herdr: task.Herdr{TabID: "tab" + strconv.Itoa(i), PaneID: "pane" + strconv.Itoa(i)},
				}
				if err := store.Create(tk); err != nil {
					t.Fatal(err)
				}
			}
			r := runner.New(store, config.Default(), "/usr/local/bin/hrk")
			r.Herdr = &herdr.Client{Bin: "herdr", Exec: (&fakeHerdrExec{}).exec}
			r.ProcTable = nil
			r.Notify = nil
			closed, err := r.GC(context.Background(), tc.owners, 0, false, false)
			slices.Sort(closed)
			if err != nil || !slices.Equal(closed, tc.want) {
				t.Errorf("closed = %v, %v; want %v", closed, err, tc.want)
			}
		})
	}
}

func TestGCOwnerAndAllExcludeEachOther(t *testing.T) {
	cmd := newGCCommand()
	cmd.SetArgs([]string{"--owner", "/a", "--all"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "[all owner] were all set") {
		t.Errorf("err = %v", err)
	}
}
