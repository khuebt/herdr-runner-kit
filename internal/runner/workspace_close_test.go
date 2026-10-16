package runner

import (
	"context"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/herdr"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// finishedWorktreeRunner launches a worktree task, whose fake worktree
// /wt/t1 never exists on disk, and finishes it.
func finishedWorktreeRunner(t *testing.T, r *Runner, fake *fakeHerdr) *task.Task {
	t.Helper()
	fake.on("agent get", ok(`{"result":{"agent":{"agent_status":"idle"}}}`)) // shell only
	tk := spawnStarting(t, r, SpawnOptions{Kind: "code", Prompt: "x", Cwd: gitRepo(t)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	finishAgo(t, r, tk.ID, time.Hour)
	got, err := r.Store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestCleanableClosesTheWorkspaceOfAGoneWorktree(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := finishedWorktreeRunner(t, r, fake)
	if _, err := r.Cleanable(context.Background(), Owners{}, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Store.Get(tk.ID); !got.Worktree.Removed {
		t.Fatalf("worktree not marked removed: %+v", got.Worktree)
	}
	if n, c := fake.count("workspace", "close"), fake.call("workspace", "close"); n != 1 || !slices.Equal(c, []string{"workspace", "close", "w2"}) {
		t.Errorf("workspace close ran %d times: %q", n, c)
	}
	// A later look finds it marked and closes nothing more.
	if _, err := r.Cleanable(context.Background(), Owners{}, false); err != nil || fake.count("workspace", "close") != 1 {
		t.Errorf("second look: %d closes, %v", fake.count("workspace", "close"), err)
	}
}

func TestCleanableKeepsAWorkspaceAnotherTaskUses(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	finishedWorktreeRunner(t, r, fake)
	// A later plan phase runs in the anchor's workspace.
	phase := spawnStarting(t, r, SpawnOptions{Prompt: "y", Cwd: plainDir(t)})
	if _, err := r.Store.Update(phase.ID, func(t *task.Task) error {
		t.Status, t.Herdr.WorkspaceID = task.Running, "w2"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Cleanable(context.Background(), Owners{}, false); err != nil {
		t.Fatal(err)
	}
	if n := fake.count("workspace", "close"); n != 0 {
		t.Errorf("workspace close ran %d times while a phase works in it", n)
	}
}

func TestCleanableLeavesAReusedWorkspaceID(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := finishedWorktreeRunner(t, r, fake)
	// herdr gave the closed workspace's id to another checkout, then to a
	// workspace outside git with another label.
	fake.on("workspace get", ok(`{"result":{"workspace":{"workspace_id":"w2","label":"other","worktree":{"checkout_path":"/wt/other"}}}}`))
	if _, err := r.Cleanable(context.Background(), Owners{}, false); err != nil {
		t.Fatal(err)
	}
	fake.on("workspace get", ok(`{"result":{"workspace":{"workspace_id":"w2","label":"other"}}}`))
	r.closeOrphanWorkspace(context.Background(), tk)
	if n := fake.count("workspace", "close"); n != 0 {
		t.Errorf("closed a workspace that is no longer the runner's (%d)", n)
	}
	// After a herdr restart the workspace has no checkout, but keeps the
	// runner's label.
	fake.on("workspace get", ok(`{"result":{"workspace":{"workspace_id":"w2","label":"`+tk.ID+` · `+tk.Title+`"}}}`))
	r.closeOrphanWorkspace(context.Background(), tk)
	if n := fake.count("workspace", "close"); n != 1 {
		t.Errorf("workspace close ran %d times after a restart", n)
	}
}

func TestCleanableIgnoresAClosedWorkspace(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := finishedWorktreeRunner(t, r, fake)
	fake.on("workspace get", herdrErr(herdr.CodeNoWorkspace))
	if _, err := r.Cleanable(context.Background(), Owners{}, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Store.Get(tk.ID); !got.Worktree.Removed || fake.count("workspace", "close") != 0 {
		t.Errorf("removed %v, %d closes", got.Worktree.Removed, fake.count("workspace", "close"))
	}
	// The workspace closes between the check and the close.
	fake.on("workspace get", ok(`{"result":{"workspace":{"worktree":{"checkout_path":"/wt/t1"}}}}`))
	fake.on("workspace close", herdrErr(herdr.CodeNoWorkspace))
	r.closeOrphanWorkspace(context.Background(), tk)
	if fake.count("workspace", "close") != 1 {
		t.Error("workspace close was not tried")
	}
}

func TestRemoveCheckoutClosesTheWorkspaceGitLeaves(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := finishedWorktreeRunner(t, r, fake)

	// Removed by herdr: its worktree remove closes the workspace itself.
	if err := r.removeCheckout(context.Background(), tk, "/wt/t1", false); err != nil {
		t.Fatal(err)
	}
	if fake.count("worktree", "remove") != 1 || fake.count("workspace", "close") != 0 {
		t.Fatalf("herdr removal: %d removes, %d closes", fake.count("worktree", "remove"), fake.count("workspace", "close"))
	}

	// herdr refuses a worktree whose folder is already gone.
	fake.on("worktree remove", herdrErr("worktree_remove_failed"))
	if err := r.removeCheckout(context.Background(), tk, "/wt/t1", false); err != nil {
		t.Fatal(err)
	}
	if fake.count("workspace", "close") != 1 {
		t.Fatalf("gone folder: %d closes", fake.count("workspace", "close"))
	}

	// Removed by git: herdr no longer shows the checkout, after a restart.
	path := filepath.Join(t.TempDir(), "wt")
	if out, err := exec.Command("git", "-C", tk.RepoRoot, "worktree", "add", "-q", "-b", "wt-git", path).CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v %s", err, out)
	}
	tk.Worktree.Path = path
	fake.on("workspace get", ok(`{"result":{"workspace":{"workspace_id":"w2","label":"`+tk.ID+` · `+tk.Title+`"}}}`))
	if err := r.removeCheckout(context.Background(), tk, path, false); err != nil {
		t.Fatal(err)
	}
	if isDir(path) || fake.count("worktree", "remove") != 2 || fake.count("workspace", "close") != 2 {
		t.Errorf("git removal: dir left %v, %d removes, %d closes", isDir(path), fake.count("worktree", "remove"), fake.count("workspace", "close"))
	}
}

func TestStopSaysWhatItKept(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	fake.on("agent get", ok(`{"result":{"agent":{"agent_status":"idle"}}}`)) // shell only
	wt := spawnStarting(t, r, SpawnOptions{Kind: "code", Prompt: "x", Cwd: gitRepo(t)})
	plain := spawnStarting(t, r, SpawnOptions{Prompt: "y", Cwd: plainDir(t)})
	for _, tk := range []*task.Task{wt, plain} {
		if err := r.Launch(context.Background(), tk.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Stop(context.Background(), tk.ID, false); err != nil {
			t.Fatal(err)
		}
	}
	all, _, err := r.Store.ReadEventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	closed := map[string]string{}
	for _, ev := range all {
		if ev.Type == "closed" {
			closed[ev.Task] = ev.Summary
		}
	}
	if closed[wt.ID] != "agent quit; workspace kept" || closed[plain.ID] != "tab closed" {
		t.Errorf("closed events = %q", closed)
	}
}
