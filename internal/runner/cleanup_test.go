package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	args = append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// worktreeTask makes a repository with an origin, a worktree on branch with
// one commit, and a finished task that owns them.
func worktreeTask(t *testing.T, r *Runner, branch string) (*task.Task, string) {
	t.Helper()
	origin := filepath.Join(t.TempDir(), "origin.git")
	git(t, ".", "init", "-q", "--bare", "-b", "main", origin)
	repo := gitRepo(t)
	git(t, repo, "remote", "add", "origin", origin)
	git(t, repo, "push", "-q", "origin", "main")
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, repo, "worktree", "add", "-q", "-b", branch, wt)
	if err := os.WriteFile(filepath.Join(wt, "a.txt"), []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, wt, "add", "a.txt")
	git(t, wt, "commit", "-q", "-m", "feat: a")
	tk := &task.Task{
		Kind: "code", Agent: "codex", Prompt: "x", Cwd: repo, Owner: repo, RepoRoot: repo,
		UseWorktree: true, Worktree: &task.Worktree{Path: wt, Branch: branch}, Gate: task.GateAuto,
		Status: task.Done, Herdr: task.Herdr{WorkspaceID: "w2", PaneID: "w2:p1", Closed: true},
	}
	if err := r.Store.Create(tk); err != nil {
		t.Fatal(err)
	}
	return tk, repo
}

func cleanable(t *testing.T, r *Runner, owner string) CleanupItem {
	t.Helper()
	items, err := r.Cleanable(context.Background(), owner, true)
	if err != nil || len(items) != 1 {
		t.Fatalf("Cleanable = %+v, %v", items, err)
	}
	return items[0]
}

func TestCleanupRemovesOnlyMergedWorktrees(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	fake.on("worktree remove", herdrErr("workspace_not_found")) // closed in herdr; git removes it
	tk, repo := worktreeTask(t, r, "fix/CSD-1-a")

	if it := cleanable(t, r, repo); it.Verdict != Unmerged || !it.Worktree || !it.Branch {
		t.Errorf("before merge: %+v", it)
	}
	if cleaned, err := r.Cleanup(context.Background(), []CleanupItem{cleanable(t, r, repo)}); err != nil || len(cleaned) != 0 {
		t.Errorf("cleaned an unmerged branch: %v, %v", cleaned, err)
	}

	// Merged on the remote only; the local main is behind.
	git(t, repo, "push", "-q", "origin", "fix/CSD-1-a:main")
	wt := tk.Worktree.Path
	if err := os.WriteFile(filepath.Join(wt, "a.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if it := cleanable(t, r, repo); it.Verdict != Dirty {
		t.Errorf("tracked change: %+v", it)
	}
	git(t, wt, "checkout", "-q", "--", "a.txt")
	if err := os.WriteFile(filepath.Join(wt, "plan.md"), []byte("p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	it := cleanable(t, r, repo)
	if it.Verdict != Merged || it.Untracked != 1 {
		t.Fatalf("merged with an untracked file: %+v", it)
	}

	if cleaned, err := r.Cleanup(context.Background(), []CleanupItem{it}); err != nil || len(cleaned) != 1 {
		t.Fatalf("Cleanup = %v, %v", cleaned, err)
	}
	if isDir(wt) || BranchExists(context.Background(), repo, "fix/CSD-1-a") {
		t.Error("worktree or branch left")
	}
	got, _ := r.Store.Get(tk.ID)
	if !got.Worktree.Removed || !got.Worktree.BranchDeleted {
		t.Errorf("task = %+v", got.Worktree)
	}
	if items, _ := r.Cleanable(context.Background(), repo, false); len(items) != 0 {
		t.Errorf("left after cleanup: %+v", items)
	}
}

func TestNoticeCleanableOffersOnce(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk, repo := worktreeTask(t, r, "hrk/t1-a")
	git(t, repo, "push", "-q", "origin", "hrk/t1-a:main")

	r.NoticeCleanable(context.Background(), repo)
	if got := events(t, r, tk.ID); len(got) != 1 || got[0] != "cleanable" {
		t.Fatalf("events = %v", got)
	}
	// Offered once, even after the check interval passes.
	mark, _ := filepath.Glob(filepath.Join(r.Store.Dir(), "*-cleanup"))
	for _, m := range mark {
		_ = os.Remove(m)
	}
	r.NoticeCleanable(context.Background(), repo)
	if got := events(t, r, tk.ID); len(got) != 1 {
		t.Errorf("offered again: %v", got)
	}
}
