package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	git(t, tk.Worktree.Path, "push", "-q", "-u", "origin", "feat/a")
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
	if cfg := gitOut(context.Background(), repo, "config", "--get-regexp", `^branch\.feat/a\.`); cfg != "" {
		t.Errorf("branch config left: %s", cfg)
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

func TestMergedIgnoresTheTreeTestWithACustomMergeDriver(t *testing.T) {
	ctx := context.Background()
	r, _, _ := newTestRunner(t)
	tk, repo := worktreeTask(t, r, "feat/a")
	// A driver that keeps base's side makes merge-tree report base's tree
	// although the branch's change is not in base.
	git(t, repo, "config", "merge.ours.driver", "true")
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("lock.txt merge=ours\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "lock.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".gitattributes", "lock.txt")
	git(t, repo, "commit", "-q", "-m", "chore: lock")
	git(t, repo, "push", "-q", "origin", "main")
	git(t, tk.Worktree.Path, "merge", "-q", "main")
	if err := os.WriteFile(filepath.Join(tk.Worktree.Path, "lock.txt"), []byte("feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, tk.Worktree.Path, "commit", "-q", "-am", "feat: lock")
	if err := os.WriteFile(filepath.Join(repo, "lock.txt"), []byte("base 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "commit", "-q", "-am", "chore: lock 2")
	if ok, _ := merged(ctx, repo, "feat/a", "main"); ok {
		t.Error("a branch with work base lacks is merged under a custom merge driver")
	}
}

// stackedTask adds a finished task on a new branch, in a worktree started
// from from, with one commit that rewrites a.txt, so a squash merge of a
// branch stacked on it leaves its own commits in conflict with base.
func stackedTask(t *testing.T, r *Runner, repo, branch, from string) *task.Task {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, repo, "worktree", "add", "-q", "-b", branch, wt, from)
	if err := os.WriteFile(filepath.Join(wt, "a.txt"), []byte(branch+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, wt, "add", "a.txt")
	git(t, wt, "commit", "-q", "-m", "feat: "+branch)
	created := true
	return finishedTask(t, r, repo, &task.Worktree{Path: wt, Branch: branch, BranchCreated: &created})
}

// stackedRelease makes a chain of stacked branches, feat/a, feat/b and
// integrate/M1 under release/M1, and squash-merges the release.
func stackedRelease(t *testing.T, r *Runner) (repo string, stacked []*task.Task, release *task.Task) {
	t.Helper()
	first, repo := worktreeTask(t, r, "feat/a")
	stacked = []*task.Task{first}
	for _, b := range []string{"feat/b", "integrate/M1"} {
		stacked = append(stacked, stackedTask(t, r, repo, b, stacked[len(stacked)-1].Worktree.Branch))
	}
	release = stackedTask(t, r, repo, "release/M1", "integrate/M1")
	squashMerge(t, repo, "release/M1")
	return repo, stacked, release
}

func TestCleanupCountsBranchesStackedUnderACleanedSquashedRelease(t *testing.T) {
	ctx := context.Background()
	r, _, _ := newTestRunner(t)
	repo, stacked, release := stackedRelease(t, r)
	for _, tk := range stacked {
		if ok, _ := merged(ctx, repo, tk.Worktree.Branch, "origin/main"); ok {
			t.Fatalf("%s is merged by itself; the case needs a conflict", tk.Worktree.Branch)
		}
	}
	// The release is cleaned first, alone, as on M1.
	git(t, repo, "worktree", "remove", release.Worktree.Path)
	items, err := r.Cleanable(ctx, Owners{Owner: repo}, false)
	if err != nil {
		t.Fatal(err)
	}
	var rel []CleanupItem
	for _, it := range items {
		if it.Task.ID == release.ID {
			rel = append(rel, it)
		}
	}
	if len(rel) != 1 || rel[0].Verdict != Merged {
		t.Fatalf("release = %+v", rel)
	}
	if cleaned, err := r.Cleanup(ctx, rel); err != nil || len(cleaned) != 1 {
		t.Fatalf("Cleanup = %v, %v", cleaned, err)
	}
	if BranchExists(ctx, repo, "release/M1") {
		t.Fatal("release branch left")
	}
	// The recorded tip outlives git's pruning of unreachable commits.
	git(t, repo, "reflog", "expire", "--expire=now", "--all")
	git(t, repo, "gc", "-q", "--prune=now")

	items, err = r.Cleanable(ctx, Owners{Owner: repo}, false)
	if err != nil || len(items) != len(stacked) {
		t.Fatalf("Cleanable = %+v, %v", items, err)
	}
	for _, it := range items {
		if it.Verdict != Merged || it.Detail != "merged into origin/main (squash or rebase of a branch stacked on it)" {
			t.Errorf("%s: %s %s", it.Task.Worktree.Branch, it.Verdict, it.Detail)
		}
	}
	if cleaned, err := r.Cleanup(ctx, items); err != nil || len(cleaned) != len(stacked) {
		t.Fatalf("Cleanup = %v, %v", cleaned, err)
	}
	for _, tk := range stacked {
		if BranchExists(ctx, repo, tk.Worktree.Branch) {
			t.Errorf("branch %s left", tk.Worktree.Branch)
		}
	}
}

func TestCleanupCountsBranchesStackedUnderALiveSquashedRelease(t *testing.T) {
	r, _, _ := newTestRunner(t)
	repo, stacked, _ := stackedRelease(t, r)
	items, err := r.Cleanable(context.Background(), Owners{Owner: repo}, false)
	if err != nil || len(items) != len(stacked)+1 {
		t.Fatalf("Cleanable = %+v, %v", items, err)
	}
	for _, it := range items {
		if it.Verdict != Merged {
			t.Errorf("%s: %s %s", it.Task.Worktree.Branch, it.Verdict, it.Detail)
		}
	}
}

func TestCleanupKeepsAStackedBranchWithWorkNoReleaseHolds(t *testing.T) {
	ctx := context.Background()
	r, _, _ := newTestRunner(t)
	repo, stacked, release := stackedRelease(t, r)
	if _, err := r.Store.Update(release.ID, func(t *task.Task) error {
		t.Worktree.MergedTip = gitOut(ctx, repo, "rev-parse", "release/M1")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A commit on feat/b after the release was cut.
	wt := stacked[1].Worktree.Path
	writeFile(t, filepath.Join(wt, "late.txt"))
	git(t, wt, "add", "late.txt")
	git(t, wt, "commit", "-q", "-m", "feat: late")
	// A tip of another owner is no reference for this one.
	other := stackedTask(t, r, repo, "feat/other", "feat/b")
	if _, err := r.Store.Update(other.ID, func(t *task.Task) error {
		t.Owner = "/elsewhere"
		t.Worktree.MergedTip = gitOut(ctx, repo, "rev-parse", "feat/other")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	items, err := r.Cleanable(ctx, Owners{Owner: repo}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		want := Merged
		if it.Task.ID == stacked[1].ID {
			want = Unmerged
		}
		if it.Verdict != want {
			t.Errorf("%s: %s %s", it.Task.Worktree.Branch, it.Verdict, it.Detail)
		}
	}
}

// editMain commits a change to path on origin/main, as a later pull request
// does, and fetches it.
func editMain(t *testing.T, repo, path, body string) {
	t.Helper()
	land := filepath.Join(t.TempDir(), "land")
	git(t, repo, "worktree", "add", "-q", "--detach", land, "origin/main")
	if err := os.WriteFile(filepath.Join(land, path), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, land, "commit", "-q", "-am", "fix: later edit")
	git(t, land, "push", "-q", "origin", "HEAD:main")
	git(t, repo, "worktree", "remove", land)
	git(t, repo, "fetch", "-q", "origin")
}

func TestCleanupRemembersASquashMergedBranchAfterMainEditsItsLines(t *testing.T) {
	ctx := context.Background()
	r, _, _ := newTestRunner(t)
	tk, repo := worktreeTask(t, r, "feat/a")
	squashMerge(t, repo, "feat/a")
	if it := cleanable(t, r, repo); it.Verdict != Merged {
		t.Fatalf("verdict = %s %s", it.Verdict, it.Detail)
	}
	tip := gitOut(ctx, repo, "rev-parse", "feat/a")
	if got, err := r.Store.Get(tk.ID); err != nil || got.Worktree.MergedTip != tip {
		t.Fatalf("MergedTip not recorded: %+v, %v", got.Worktree, err)
	}
	if ref := gitOut(ctx, repo, "rev-parse", task.MergedTipRef(tk.ID)); ref != tip {
		t.Errorf("merged ref = %q, want %q", ref, tip)
	}

	editMain(t, repo, "a.txt", "changed on main\n")
	if ok, _ := merged(ctx, repo, "feat/a", "origin/main"); ok {
		t.Fatal("the squash still applies; the test proves nothing")
	}
	if it := cleanable(t, r, repo); it.Verdict != Merged || it.Detail != "merged into origin/main (squash or rebase)" {
		t.Errorf("after main edited its lines: %s %s", it.Verdict, it.Detail)
	}

	// A commit after the recorded tip is checked again, and is work main lacks.
	writeFile(t, filepath.Join(tk.Worktree.Path, "b.txt"))
	git(t, tk.Worktree.Path, "add", "b.txt")
	git(t, tk.Worktree.Path, "commit", "-q", "-m", "feat: b")
	if it := cleanable(t, r, repo); it.Verdict != Unmerged {
		t.Errorf("with a later commit: %s %s", it.Verdict, it.Detail)
	}
}

func TestCleanupKeepsARecordedReleaseTipWhenTheReleaseMoves(t *testing.T) {
	ctx := context.Background()
	r, _, _ := newTestRunner(t)
	repo, stacked, release := stackedRelease(t, r)
	tip := gitOut(ctx, repo, "rev-parse", "release/M1")
	// milestone done records the squash-merged release's tip.
	if _, err := r.Store.Update(release.ID, func(t *task.Task) error {
		t.Worktree.MergedTip = tip
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "update-ref", task.MergedTipRef(release.ID), tip)
	// The release branch is then reset onto main, which holds it by ancestry.
	git(t, release.Worktree.Path, "reset", "-q", "--hard", "origin/main")
	if _, err := r.Cleanable(ctx, Owners{Owner: repo}, false); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "reflog", "expire", "--expire=now", "--all")
	git(t, repo, "gc", "-q", "--prune=now")
	items, err := r.Cleanable(ctx, Owners{Owner: repo}, false)
	if err != nil || len(items) != len(stacked)+1 {
		t.Fatalf("Cleanable = %+v, %v", items, err)
	}
	for _, it := range items {
		if it.Verdict != Merged {
			t.Errorf("%s: %s %s", it.Task.Worktree.Branch, it.Verdict, it.Detail)
		}
	}
	if got, err := r.Store.Get(release.ID); err != nil || !slices.Contains(got.Worktree.MergedTips(), tip) {
		t.Errorf("the release tip was dropped: %+v, %v", got.Worktree, err)
	}
}

func TestCleanupRemembersAWorktreeOnAnotherBranch(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk, repo := worktreeTask(t, r, "feat/a")
	// The runner left the work on another branch, which was squash-merged.
	git(t, tk.Worktree.Path, "checkout", "-q", "-b", "feat/a2")
	writeFile(t, filepath.Join(tk.Worktree.Path, "c.txt"))
	git(t, tk.Worktree.Path, "add", "c.txt")
	git(t, tk.Worktree.Path, "commit", "-q", "-m", "feat: c")
	squashMerge(t, repo, "feat/a2")
	if it := cleanable(t, r, repo); it.Verdict != Merged {
		t.Fatalf("verdict = %s %s", it.Verdict, it.Detail)
	}
	editMain(t, repo, "c.txt", "changed on main\n")
	if it := cleanable(t, r, repo); it.Verdict != Merged {
		t.Errorf("after main edited its lines: %s %s", it.Verdict, it.Detail)
	}
}

func TestCleanupLabelsAKeptStackedBranchAsStacked(t *testing.T) {
	ctx := context.Background()
	r, _, _ := newTestRunner(t)
	repo, stacked, release := stackedRelease(t, r)
	if _, err := r.Store.Update(release.ID, func(t *task.Task) error {
		t.Worktree.MergedTip = gitOut(ctx, repo, "rev-parse", "release/M1")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// feat/a is checked out in a second worktree too, so cleanup keeps it.
	git(t, repo, "worktree", "add", "-q", "-f", filepath.Join(t.TempDir(), "second"), "feat/a")
	items, err := r.Cleanable(ctx, Owners{Owner: repo}, false)
	if err != nil {
		t.Fatal(err)
	}
	items = slices.DeleteFunc(items, func(it CleanupItem) bool { return it.Task.ID != stacked[0].ID })
	if cleaned, err := r.Cleanup(ctx, items); err != nil || len(cleaned) != 1 {
		t.Fatalf("Cleanup = %v, %v", cleaned, err)
	}
	items, err = r.Cleanable(ctx, Owners{Owner: repo}, false)
	if err != nil {
		t.Fatal(err)
	}
	items = slices.DeleteFunc(items, func(it CleanupItem) bool { return it.Task.ID != stacked[0].ID })
	if len(items) != 1 || items[0].Detail != "merged into origin/main (squash or rebase of a branch stacked on it)" {
		t.Errorf("kept branch: %+v", items)
	}
}

func TestCleanupKeepsABranchThatMovedPastTheCheckedCommit(t *testing.T) {
	ctx := context.Background()
	for _, withWorktree := range []bool{true, false} {
		r, fake, _ := newTestRunner(t)
		tk, repo := worktreeTask(t, r, "feat/a")
		herdrOwnsWorktree(fake, repo, tk.Worktree.Path)
		squashMerge(t, repo, "feat/a")
		if !withWorktree {
			git(t, repo, "worktree", "remove", tk.Worktree.Path)
		}
		it := cleanable(t, r, repo)
		checked := gitOut(ctx, repo, "rev-parse", "feat/a")
		if it.Verdict != Merged {
			t.Fatalf("verdict = %s %s", it.Verdict, it.Detail)
		}
		// A commit after the check that adds nothing main lacks still is no
		// commit the check saw.
		if withWorktree {
			git(t, tk.Worktree.Path, "commit", "-q", "--allow-empty", "-m", "chore: later")
		} else {
			// gitOut sets no identity, which CI lacks.
			later := gitOut(ctx, repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit-tree", "feat/a^{tree}", "-p", "feat/a", "-m", "chore: later")
			git(t, repo, "update-ref", "refs/heads/feat/a", later)
		}
		if _, err := r.Cleanup(ctx, []CleanupItem{it}); err == nil {
			t.Errorf("worktree %v: Cleanup removed a branch that moved after the check", withWorktree)
		}
		if !BranchExists(ctx, repo, "feat/a") {
			t.Errorf("worktree %v: branch deleted", withWorktree)
		}
		if withWorktree && !isDir(tk.Worktree.Path) {
			t.Error("worktree removed although HEAD moved past the checked commit")
		}
		if got, err := r.Store.Get(tk.ID); err != nil || got.Worktree.MergedTip != checked {
			t.Errorf("worktree %v: recorded %q, want the checked %s", withWorktree, got.Worktree.MergedTip, checked)
		}
	}
}

func TestCleanupKeepsABranchAnotherWorktreeIsRebasing(t *testing.T) {
	ctx := context.Background()
	r, _, _ := newTestRunner(t)
	tk, repo := worktreeTask(t, r, "feat/a")
	squashMerge(t, repo, "feat/a")
	git(t, repo, "worktree", "remove", tk.Worktree.Path)
	// A rebase stopped at an edit leaves the other worktree's HEAD detached.
	other := filepath.Join(t.TempDir(), "other")
	git(t, repo, "worktree", "add", "-q", "--detach", other, "feat/a")
	cmd := exec.Command("git", "-C", other, "rebase", "-q", "-i", "feat/a~1", "feat/a")
	cmd.Env = append(os.Environ(), "GIT_SEQUENCE_EDITOR=sed -i 1s/^pick/edit/")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rebase: %v\n%s", err, out)
	}
	it := cleanable(t, r, repo)
	if it.Verdict != Merged || it.Worktree || !it.Branch {
		t.Fatalf("item = %+v", it)
	}
	if _, err := r.Cleanup(ctx, []CleanupItem{it}); err != nil {
		t.Fatal(err)
	}
	if !BranchExists(ctx, repo, "feat/a") {
		t.Error("deleted a branch another worktree is rebasing")
	}
}

// Cleanup counts a branch merged once its squash lands on origin's main. A
// child of it starts from the local main only once that holds the work.
func TestChildOfACleanedParentWaitsForTheLocalDefaultBranch(t *testing.T) {
	ctx := context.Background()
	r, fake, _ := newTestRunner(t)
	parent, repo := worktreeTask(t, r, "feat/a")
	herdrOwnsWorktree(fake, repo, parent.Worktree.Path)
	squashMerge(t, repo, "feat/a")
	if _, err := r.Cleanup(ctx, []CleanupItem{cleanable(t, r, repo)}); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Store.Get(parent.ID); !got.Worktree.BranchDeleted {
		t.Fatal("cleanup kept the merged branch")
	}
	if _, base, err := r.parentInputs(ctx, []string{parent.ID}); !errors.Is(err, errParentBranchGone) {
		t.Fatalf("a child starts from %q, which lacks the parent's work: %v", base, err)
	}
	git(t, repo, "merge", "-q", "--ff-only", "origin/main")
	if _, base, err := r.parentInputs(ctx, []string{parent.ID}); err != nil || base != "main" {
		t.Fatalf("after main was updated: base %q, %v", base, err)
	}
}
