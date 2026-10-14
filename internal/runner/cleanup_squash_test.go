package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// squashMerge lands branch's work on origin/main as one new commit, the way
// a squash-merged pull request does, and fetches it.
func squashMerge(t *testing.T, repo, branch string) {
	t.Helper()
	land := filepath.Join(t.TempDir(), "land")
	git(t, repo, "worktree", "add", "-q", "--detach", land, "origin/main")
	git(t, land, "merge", "-q", "--squash", branch)
	git(t, land, "commit", "-q", "-m", "feat: squashed")
	git(t, land, "push", "-q", "origin", "HEAD:main")
	git(t, repo, "worktree", "remove", land)
	git(t, repo, "fetch", "-q", "origin")
}

func TestMergedSeesSquashAndRebase(t *testing.T) {
	ctx := context.Background()
	r, _, _ := newTestRunner(t)
	tk, repo := worktreeTask(t, r, "feat/a")
	if ok, _ := merged(ctx, repo, "feat/a", "origin/main"); ok {
		t.Fatal("an unmerged branch is merged")
	}
	squashMerge(t, repo, "feat/a")
	if ok, squash := merged(ctx, repo, "feat/a", "origin/main"); !ok || !squash {
		t.Errorf("squash-merged = %v %v", ok, squash)
	}

	// One more commit on the branch is work base lacks.
	writeFile(t, filepath.Join(tk.Worktree.Path, "b.txt"))
	git(t, tk.Worktree.Path, "add", "b.txt")
	git(t, tk.Worktree.Path, "commit", "-q", "-m", "feat: b")
	if ok, _ := merged(ctx, repo, "feat/a", "origin/main"); ok {
		t.Error("a branch with a new commit is merged")
	}

	// Rebased onto base and pushed as new commits.
	git(t, tk.Worktree.Path, "rebase", "-q", "origin/main")
	git(t, tk.Worktree.Path, "push", "-q", "origin", "HEAD:main")
	git(t, tk.Worktree.Path, "commit", "-q", "--amend", "--no-edit", "--date=2000-01-01T00:00:00")
	git(t, repo, "fetch", "-q", "origin")
	if ok, squash := merged(ctx, repo, "feat/a", "origin/main"); !ok || !squash {
		t.Errorf("rebase-merged = %v %v", ok, squash)
	}
}

func TestCleanupRemovesASquashMergedWorktree(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk, repo := worktreeTask(t, r, "feat/a")
	herdrOwnsWorktree(fake, repo, tk.Worktree.Path)
	squashMerge(t, repo, "feat/a")
	it := cleanable(t, r, repo)
	if it.Verdict != Merged || it.Detail != "merged into origin/main (squash or rebase)" {
		t.Fatalf("verdict = %+v", it)
	}
	if cleaned, err := r.Cleanup(context.Background(), []CleanupItem{it}); err != nil || len(cleaned) != 1 {
		t.Fatalf("Cleanup = %v, %v", cleaned, err)
	}
	if isDir(tk.Worktree.Path) || BranchExists(context.Background(), repo, "feat/a") {
		t.Error("worktree or branch left")
	}
}

func TestCleanupRefusesAWorktreeThatMovedAfterTheCheck(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk, repo := worktreeTask(t, r, "feat/a")
	herdrOwnsWorktree(fake, repo, tk.Worktree.Path)
	squashMerge(t, repo, "feat/a")
	it := cleanable(t, r, repo)
	writeFile(t, filepath.Join(tk.Worktree.Path, "b.txt"))
	git(t, tk.Worktree.Path, "add", "b.txt")
	git(t, tk.Worktree.Path, "commit", "-q", "-m", "feat: b")
	if _, err := r.Cleanup(context.Background(), []CleanupItem{it}); err == nil || !strings.Contains(err.Error(), "changed since the check") {
		t.Fatalf("err = %v", err)
	}
	if !isDir(tk.Worktree.Path) {
		t.Error("removed a worktree with new work")
	}
}

func TestCleanableTakesProjectOwners(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk, repo := worktreeTask(t, r, "feat/a")
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error {
		t.Owner = task.ProjectOwner("demo-app")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if items, _ := r.Cleanable(context.Background(), Owners{Owner: repo}, false); len(items) != 0 {
		t.Errorf("one owner lists a project task: %+v", items)
	}
	if items, _ := r.Cleanable(context.Background(), Owners{Owner: repo, Projects: true}, false); len(items) != 1 {
		t.Errorf("project task not listed: %+v", items)
	}
}

func TestCleanableMarksAWorktreeRemovedOutsideTheKit(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk, repo := worktreeTask(t, r, "feat/a")
	git(t, repo, "worktree", "remove", "--force", tk.Worktree.Path)
	git(t, repo, "branch", "-D", "feat/a")
	for range 2 {
		if items, err := r.Cleanable(context.Background(), Owners{Owner: repo}, false); err != nil || len(items) != 0 {
			t.Fatalf("Cleanable = %+v, %v", items, err)
		}
	}
	got, _ := r.Store.Get(tk.ID)
	if !got.Worktree.Removed || !got.Worktree.BranchDeleted {
		t.Errorf("worktree = %+v", got.Worktree)
	}
	if evs := eventsOf(t, r, tk.ID, "cleaned"); len(evs) != 1 || evs[0].Summary != "removed outside the kit" {
		t.Errorf("cleaned events = %+v", evs)
	}
}

func TestCleanableKeepsTheBranchOfAVanishedWorktree(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk, repo := worktreeTask(t, r, "feat/a")
	if err := os.RemoveAll(tk.Worktree.Path); err != nil {
		t.Fatal(err)
	}
	it := cleanable(t, r, repo)
	if it.Worktree || !it.Branch || it.Verdict != Unmerged {
		t.Errorf("item = %+v", it)
	}
	got, _ := r.Store.Get(tk.ID)
	if !got.Worktree.Removed || got.Worktree.BranchDeleted {
		t.Errorf("worktree = %+v", got.Worktree)
	}
	if evs := eventsOf(t, r, tk.ID, "cleaned"); len(evs) != 0 {
		t.Errorf("cleaned events = %+v", evs)
	}
}
