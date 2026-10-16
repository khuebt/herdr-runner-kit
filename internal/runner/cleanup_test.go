package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	args = append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// worktreeRepo makes a repository with an origin and a worktree on a new
// branch with no commits of its own.
func worktreeRepo(t *testing.T, branch string) (repo, wt string) {
	t.Helper()
	origin := filepath.Join(t.TempDir(), "origin.git")
	git(t, ".", "init", "-q", "--bare", "-b", "main", origin)
	repo = gitRepo(t)
	git(t, repo, "remote", "add", "origin", origin)
	git(t, repo, "push", "-q", "origin", "main")
	wt = filepath.Join(t.TempDir(), "wt")
	git(t, repo, "worktree", "add", "-q", "-b", branch, wt)
	return repo, wt
}

// worktreeTask makes a repository with an origin, a worktree on branch with
// one commit, and a finished task that owns them.
func worktreeTask(t *testing.T, r *Runner, branch string) (*task.Task, string) {
	t.Helper()
	repo, wt := worktreeRepo(t, branch)
	writeFile(t, filepath.Join(wt, "a.txt"))
	git(t, wt, "add", "a.txt")
	git(t, wt, "commit", "-q", "-m", "feat: a")
	return finishedTask(t, r, repo, &task.Worktree{Path: wt, Branch: branch}), repo
}

func finishedTask(t *testing.T, r *Runner, repo string, w *task.Worktree) *task.Task {
	t.Helper()
	tk := &task.Task{
		Kind: "code", Agent: "codex", Prompt: "x", Cwd: repo, Owner: repo, RepoRoot: repo,
		UseWorktree: true, Worktree: w, Gate: task.GateAuto,
		Status: task.Done, Herdr: task.Herdr{WorkspaceID: "w2", PaneID: "w2:p1", Closed: true},
	}
	if err := r.Store.Create(tk); err != nil {
		t.Fatal(err)
	}
	return tk
}

func cleanable(t *testing.T, r *Runner, owner string) CleanupItem {
	t.Helper()
	items, err := r.Cleanable(context.Background(), Owners{Owner: owner}, true)
	if err != nil || len(items) != 1 {
		t.Fatalf("Cleanable = %+v, %v", items, err)
	}
	return items[0]
}

// herdrOwnsWorktree makes the fake herdr report wt as workspace w2's
// checkout and remove it with git the way herdr does, honouring --force.
func herdrOwnsWorktree(fake *fakeHerdr, repo, wt string) {
	fake.on("workspace get", ok(fmt.Sprintf(`{"result":{"workspace":{"worktree":{"checkout_path":%q}}}}`, wt)))
	fake.on("worktree remove", func(args []string) (string, error) {
		gitArgs := []string{"-C", repo, "worktree", "remove", wt}
		if slices.Contains(args, "--force") {
			gitArgs = append(gitArgs, "--force")
		}
		if out, err := exec.Command("git", gitArgs...).CombinedOutput(); err != nil {
			return "", fmt.Errorf("%v: %s", err, out)
		}
		return "{}", nil
	})
}

func TestCleanupRemovesOnlyMergedWorktrees(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk, repo := worktreeTask(t, r, "fix/CSD-1-a")
	wt := tk.Worktree.Path
	herdrOwnsWorktree(fake, repo, wt)

	if it := cleanable(t, r, repo); it.Verdict != Unmerged || !it.Worktree || !it.Branch {
		t.Errorf("before merge: %+v", it)
	}
	if cleaned, err := r.Cleanup(context.Background(), []CleanupItem{cleanable(t, r, repo)}); err != nil || len(cleaned) != 0 {
		t.Errorf("cleaned an unmerged branch: %v, %v", cleaned, err)
	}

	// Merged on the remote only; the local main is behind.
	git(t, repo, "push", "-q", "origin", "fix/CSD-1-a:main")
	if err := os.WriteFile(filepath.Join(wt, "a.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if it := cleanable(t, r, repo); it.Verdict != Dirty {
		t.Errorf("tracked change: %+v", it)
	}
	git(t, wt, "checkout", "-q", "--", "a.txt")

	// Untracked work outside plans/ is kept.
	writeFile(t, filepath.Join(wt, "notes.txt"))
	if it := cleanable(t, r, repo); it.Verdict != Dirty || it.Detail != "untracked: notes.txt" {
		t.Errorf("untracked work: %+v", it)
	}
	if err := os.Remove(filepath.Join(wt, "notes.txt")); err != nil {
		t.Fatal(err)
	}

	// An untracked plan file is scratch, and the only reason to force.
	writeFile(t, filepath.Join(wt, "plans", "x.md"))
	it := cleanable(t, r, repo)
	if it.Verdict != Merged || it.Scratch != 1 || !strings.Contains(it.Detail, "1 plan file will be deleted") {
		t.Fatalf("merged with a plan file: %+v", it)
	}
	if cleaned, err := r.Cleanup(context.Background(), []CleanupItem{it}); err != nil || len(cleaned) != 1 {
		t.Fatalf("Cleanup = %v, %v", cleaned, err)
	}
	if rm := fake.call("worktree", "remove"); !slices.Equal(rm, []string{"worktree", "remove", "--workspace", "w2", "--force"}) {
		t.Errorf("worktree remove = %q", rm)
	}
	if isDir(wt) || BranchExists(context.Background(), repo, "fix/CSD-1-a") {
		t.Error("worktree or branch left")
	}
	got, _ := r.Store.Get(tk.ID)
	if !got.Worktree.Removed || !got.Worktree.BranchDeleted {
		t.Errorf("task = %+v", got.Worktree)
	}
	if items, _ := r.Cleanable(context.Background(), Owners{Owner: repo}, false); len(items) != 0 {
		t.Errorf("left after cleanup: %+v", items)
	}
}

func TestCleanupUntrackedOnlyBranchIsDirty(t *testing.T) {
	r, _, _ := newTestRunner(t)
	repo, wt := worktreeRepo(t, "hrk/t1-a")
	tk := finishedTask(t, r, repo, &task.Worktree{Path: wt, Branch: "hrk/t1-a"})
	writeFile(t, filepath.Join(wt, "src", "new.go"))

	if it := cleanable(t, r, repo); it.Verdict != Dirty || it.Detail != "untracked: src/new.go" {
		t.Errorf("verdict = %+v", it)
	}
	r.NoticeCleanable(context.Background(), repo)
	if got := events(t, r, tk.ID); len(got) != 0 {
		t.Errorf("offered a branch with untracked work: %v", got)
	}
}

func TestCleanupListsAtMostFivePaths(t *testing.T) {
	r, _, _ := newTestRunner(t)
	_, repo := worktreeTask(t, r, "hrk/t1-a")
	git(t, repo, "push", "-q", "origin", "hrk/t1-a:main")
	wt := cleanable(t, r, repo).Task.Worktree.Path
	for _, f := range []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7"} {
		writeFile(t, filepath.Join(wt, f))
	}
	if it := cleanable(t, r, repo); it.Verdict != Dirty || it.Detail != "untracked: a1, a2, a3, a4, a5 and 2 more" {
		t.Errorf("verdict = %+v", it)
	}
}

func TestCleanupCountsIgnored(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk, repo := worktreeTask(t, r, "hrk/t1-a")
	wt := tk.Worktree.Path
	herdrOwnsWorktree(fake, repo, wt)
	if err := os.WriteFile(filepath.Join(wt, ".gitignore"), []byte("*.log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, wt, "add", ".gitignore")
	git(t, wt, "commit", "-q", "-m", "chore: ignore logs")
	git(t, repo, "push", "-q", "origin", "hrk/t1-a:main")
	writeFile(t, filepath.Join(wt, "x.log"))
	writeFile(t, filepath.Join(wt, "y.log"))

	it := cleanable(t, r, repo)
	if it.Verdict != Merged || it.Ignored != 2 || !strings.HasSuffix(it.Detail, "; 2 ignored files will be deleted") {
		t.Fatalf("verdict = %+v", it)
	}
	if _, err := r.Cleanup(context.Background(), []CleanupItem{it}); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(fake.call("worktree", "remove"), "--force") || isDir(wt) {
		t.Errorf("ignored files alone forced removal, or the worktree is left: %q", fake.call("worktree", "remove"))
	}
}

func TestCleanupAbortsWhenWorktreeChanged(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk, repo := worktreeTask(t, r, "hrk/t1-a")
	git(t, repo, "push", "-q", "origin", "hrk/t1-a:main")
	it := cleanable(t, r, repo)
	if it.Verdict != Merged {
		t.Fatalf("verdict = %+v", it)
	}
	// The user starts new work in the tab after the check.
	writeFile(t, filepath.Join(tk.Worktree.Path, "notes.txt"))
	_, err := r.Cleanup(context.Background(), []CleanupItem{it})
	if err == nil || !strings.Contains(err.Error(), "worktree changed since the check") {
		t.Fatalf("err = %v", err)
	}
	if !isDir(tk.Worktree.Path) || !BranchExists(context.Background(), repo, "hrk/t1-a") || fake.count("worktree", "remove") != 0 {
		t.Error("removed a worktree that changed")
	}
}

func TestRemoveWorktreeChecksWorkspacePath(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk, _ := worktreeTask(t, r, "hrk/t1-a")

	// herdr reused w2 for another checkout: git removes the worktree.
	fake.on("workspace get", ok(`{"result":{"workspace":{"worktree":{"checkout_path":"/home/u/other"}}}}`))
	if err := r.removeWorktree(context.Background(), tk, false); err != nil {
		t.Fatal(err)
	}
	if fake.count("worktree", "remove") != 0 || isDir(tk.Worktree.Path) {
		t.Fatalf("herdr remove calls %d, worktree left %v", fake.count("worktree", "remove"), isDir(tk.Worktree.Path))
	}

	tk2, repo2 := worktreeTask(t, r, "hrk/t2-a")
	herdrOwnsWorktree(fake, repo2, tk2.Worktree.Path+"/")
	if err := r.removeWorktree(context.Background(), tk2, false); err != nil {
		t.Fatal(err)
	}
	if rm := fake.call("worktree", "remove"); !slices.Equal(rm, []string{"worktree", "remove", "--workspace", "w2"}) {
		t.Errorf("worktree remove = %q", rm)
	}
}

func TestRemoveWorktreeRemovesEmptyRepoFolder(t *testing.T) {
	r, _, _ := newTestRunner(t)
	root := filepath.Join(t.TempDir(), "worktrees")
	repo := gitRepo(t)
	addWorktree := func(branch string) *task.Task {
		wt := filepath.Join(root, "greet", strings.ReplaceAll(branch, "/", "-"))
		git(t, repo, "worktree", "add", "-q", "-b", branch, wt)
		return finishedTask(t, r, repo, &task.Worktree{Path: wt, Branch: branch})
	}
	a, b := addWorktree("hrk/t1-a"), addWorktree("hrk/t2-b")

	if err := r.removeWorktree(context.Background(), a, false); err != nil {
		t.Fatal(err)
	}
	if !isDir(filepath.Join(root, "greet")) {
		t.Fatal("removed the repository folder while another worktree is in it")
	}
	if err := r.removeWorktree(context.Background(), b, false); err != nil {
		t.Fatal(err)
	}
	if isDir(filepath.Join(root, "greet")) || !isDir(root) {
		t.Errorf("repository folder left %v, worktree root gone %v", isDir(filepath.Join(root, "greet")), !isDir(root))
	}
}

func TestRemoveWorktreeKeepsFolderOutsideHerdrLayout(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk, _ := worktreeTask(t, r, "hrk/t1-a")
	if err := r.removeWorktree(context.Background(), tk, false); err != nil {
		t.Fatal(err)
	}
	if !isDir(filepath.Dir(tk.Worktree.Path)) {
		t.Error("removed a folder that is not in herdr's worktree root")
	}
}

func TestQueuedTaskRechecksBranchAtLaunch(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	repo := gitRepo(t)
	tk := spawnStarting(t, r, SpawnOptions{Kind: "code", Prompt: "x", Cwd: repo, Gate: task.GateHuman})
	git(t, repo, "branch", tk.Worktree.Branch) // made by hand while the task was held
	if _, err := r.Release(tk.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.Launch(context.Background(), tk.ID); err == nil {
		t.Fatal("launch on an existing branch succeeded")
	}
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Failed || !strings.Contains(got.Error, "branch "+tk.Worktree.Branch+" already exists") {
		t.Errorf("task = %+v", got)
	}
	if n := fake.count("worktree", "create"); n != 0 {
		t.Errorf("worktree create called %d times", n)
	}
	if got.Worktree.OwnsBranch() {
		t.Error("the task claims a branch it did not create")
	}
}

// failFirstStart launches a code task whose worktree herdr creates for real
// and whose environment cannot be set, after write has run in the new
// worktree. It returns the failed task, its repository and its worktree.
func failFirstStart(t *testing.T, r *Runner, fake *fakeHerdr, write func(t *testing.T, wt string)) (*task.Task, string, string) {
	t.Helper()
	repo := gitRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	herdrOwnsWorktree(fake, repo, wt)
	fake.on("worktree create", func(args []string) (string, error) {
		branch := args[slices.Index(args, "--branch")+1]
		if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-q", "-b", branch, wt).CombinedOutput(); err != nil {
			return "", fmt.Errorf("%v: %s", err, out)
		}
		write(t, wt)
		return fmt.Sprintf(`{"result":{"root_pane":{"pane_id":"w2:p1","tab_id":"w2:t1","workspace_id":"w2"},"worktree":{"path":%q}}}`, wt), nil
	})
	fake.on("pane run", herdrErr("pane_not_found"))
	t.Chdir(repo) // the main session works in the repository
	tk := spawnStarting(t, r, SpawnOptions{Kind: "code", Prompt: "x", Cwd: repo})
	if err := r.Launch(context.Background(), tk.ID); err == nil {
		t.Fatal("want error")
	}
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Failed || got.Worktree.Path != wt || !got.Worktree.OwnsBranch() ||
		got.Herdr.WorkspaceID != "w2" || got.Herdr.PaneID != "w2:p1" {
		t.Fatalf("task = %+v, worktree = %+v", got, got.Worktree)
	}
	return got, repo, wt
}

func TestFailedFirstStartRemovesEmptyWorktree(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk, repo, wt := failFirstStart(t, r, fake, func(*testing.T, string) {})
	if isDir(wt) || BranchExists(context.Background(), repo, tk.Worktree.Branch) {
		t.Error("worktree or branch left")
	}
	if !tk.Worktree.Removed || !tk.Worktree.BranchDeleted || !tk.Herdr.Closed {
		t.Errorf("worktree = %+v", tk.Worktree)
	}
	if n := fake.count("worktree", "remove"); n != 1 {
		t.Errorf("herdr worktree remove called %d times; it closes the workspace", n)
	}
	cleaned := eventsOf(t, r, tk.ID, "cleaned")
	if len(cleaned) != 1 || cleaned[0].Summary != "never started; removed its empty worktree and branch" {
		t.Errorf("cleaned = %+v", cleaned)
	}
	if items, _ := r.Cleanable(context.Background(), Owners{Owner: repo}, false); len(items) != 0 {
		t.Errorf("left after the failed start: %+v", items)
	}
}

func TestFailedFirstStartKeepsWrittenWorktree(t *testing.T) {
	for name, write := range map[string]func(t *testing.T, wt string){
		"commit": func(t *testing.T, wt string) {
			writeFile(t, filepath.Join(wt, "a.txt"))
			git(t, wt, "add", "a.txt")
			git(t, wt, "commit", "-q", "-m", "feat: a")
		},
		"untracked": func(t *testing.T, wt string) { writeFile(t, filepath.Join(wt, "notes.txt")) },
		"plan file": func(t *testing.T, wt string) { writeFile(t, filepath.Join(wt, "plans", "notes.md")) },
	} {
		t.Run(name, func(t *testing.T) {
			r, fake, _ := newTestRunner(t)
			tk, repo, wt := failFirstStart(t, r, fake, write)
			if !isDir(wt) || !BranchExists(context.Background(), repo, tk.Worktree.Branch) || tk.Worktree.Removed {
				t.Errorf("worktree = %+v", tk.Worktree)
			}
			if fake.count("worktree", "remove") != 0 || len(eventsOf(t, r, tk.ID, "cleaned")) != 0 {
				t.Error("removed or reported cleaned")
			}
			if it := cleanable(t, r, repo); it.Task.ID != tk.ID || !it.Worktree || !it.Branch {
				t.Errorf("cleanable = %+v", it)
			}
		})
	}
}

func TestFailedRelaunchKeepsWorktree(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	repo, wt := worktreeRepo(t, "hrk/t1-x")
	tk := &task.Task{
		Kind: "code", Agent: "gemini", Prompt: "x", Cwd: repo, Owner: repo, RepoRoot: repo,
		UseWorktree: true, Worktree: &task.Worktree{Path: wt, Branch: "hrk/t1-x", BranchCreated: new(true)},
		Gate: task.GateAuto, Status: task.Starting, FallbackFrom: []string{"codex"},
		Herdr: task.Herdr{WorkspaceID: "w2", PaneID: "w2:p1"},
	}
	if err := r.Store.Create(tk); err != nil {
		t.Fatal(err)
	}
	herdrOwnsWorktree(fake, repo, wt)
	if err := r.Launch(context.Background(), tk.ID); err == nil {
		t.Fatal("want error")
	}
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Failed || got.Worktree.Removed || !isDir(wt) || !BranchExists(context.Background(), repo, "hrk/t1-x") {
		t.Errorf("task = %+v, worktree = %+v", got, got.Worktree)
	}
}

func TestFailedTabStartRemovesNothing(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	fake.on("agent start", herdrErr("pane_not_found"))
	tk := spawnStarting(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err == nil {
		t.Fatal("want error")
	}
	if fake.count("worktree", "remove") != 0 || fake.count("workspace", "close") != 0 || len(eventsOf(t, r, tk.ID, "cleaned")) != 0 {
		t.Error("a tab task's failed start removed something")
	}
}

func TestCleanupKeepsBranchItDidNotCreate(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk, repo := worktreeTask(t, r, "hrk/t1-a")
	git(t, repo, "push", "-q", "origin", "hrk/t1-a:main")
	// A task from an older version owns the branch of the worktree it recorded.
	if it := cleanable(t, r, repo); !it.Branch {
		t.Errorf("legacy task: %+v", it)
	}

	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error {
		t.Worktree.BranchCreated = new(false)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	it := cleanable(t, r, repo)
	if it.Verdict != Merged || !it.Worktree || it.Branch {
		t.Fatalf("verdict = %+v", it)
	}
	if _, err := r.Cleanup(context.Background(), []CleanupItem{it}); err != nil {
		t.Fatal(err)
	}
	if isDir(tk.Worktree.Path) || !BranchExists(context.Background(), repo, "hrk/t1-a") {
		t.Error("worktree left, or the branch it did not create deleted")
	}
	if got, _ := r.Store.Get(tk.ID); got.Worktree.BranchDeleted {
		t.Errorf("worktree = %+v", got.Worktree)
	}
	if items, _ := r.Cleanable(context.Background(), Owners{Owner: repo}, false); len(items) != 0 {
		t.Errorf("left after cleanup: %+v", items)
	}
}

func TestCleanupSkipsBranchMovedSinceCheck(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk, repo := worktreeTask(t, r, "hrk/t1-a")
	git(t, repo, "push", "-q", "origin", "hrk/t1-a:main")
	git(t, repo, "worktree", "remove", tk.Worktree.Path)
	it := cleanable(t, r, repo)
	if it.Verdict != Merged || it.Worktree || !it.Branch {
		t.Fatalf("verdict = %+v", it)
	}
	// Someone commits to the branch after the check.
	git(t, repo, "checkout", "-q", "hrk/t1-a")
	writeFile(t, filepath.Join(repo, "b.txt"))
	git(t, repo, "add", "b.txt")
	git(t, repo, "commit", "-q", "-m", "more")
	git(t, repo, "checkout", "-q", "main")

	_, err := r.Cleanup(context.Background(), []CleanupItem{it})
	if err == nil || !strings.Contains(err.Error(), "moved since the check") {
		t.Fatalf("err = %v", err)
	}
	if !BranchExists(context.Background(), repo, "hrk/t1-a") {
		t.Error("deleted a branch with new commits")
	}
}

func TestCleanupSharedBranch(t *testing.T) {
	r, _, _ := newTestRunner(t)
	t1, repo := worktreeTask(t, r, "hrk/t1-a")
	git(t, repo, "push", "-q", "origin", "hrk/t1-a:main")
	t2 := finishedTask(t, r, repo, &task.Worktree{Path: filepath.Join(t.TempDir(), "gone"), Branch: "hrk/t1-a"})

	items, err := r.Cleanable(context.Background(), Owners{Owner: repo}, false)
	if err != nil || len(items) != 2 {
		t.Fatalf("Cleanable = %+v, %v", items, err)
	}
	// The task without a worktree goes first, while the other still has
	// the branch checked out.
	slices.SortFunc(items, func(a, b CleanupItem) int { return strings.Compare(b.Task.ID, a.Task.ID) })
	cleaned, err := r.Cleanup(context.Background(), items)
	if err != nil || !slices.Equal(cleaned, []string{t2.ID, t1.ID}) {
		t.Fatalf("Cleanup = %v, %v", cleaned, err)
	}
	if isDir(t1.Worktree.Path) || BranchExists(context.Background(), repo, "hrk/t1-a") {
		t.Error("worktree or branch left")
	}
	if items, _ := r.Cleanable(context.Background(), Owners{Owner: repo}, false); len(items) != 0 {
		t.Errorf("left after cleanup: %+v", items)
	}
}

func TestNoticeCleanableOffersOnce(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk, repo := worktreeTask(t, r, "hrk/t1-a")
	git(t, repo, "push", "-q", "origin", "hrk/t1-a:main")

	// Two waits with their own cursors check at the same time.
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { r.NoticeCleanable(context.Background(), repo) })
	}
	wg.Wait()
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

func TestCleanupSkipsAReopenedRunner(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk, repo := worktreeTask(t, r, "fix/CSD-9-a")
	herdrOwnsWorktree(fake, repo, tk.Worktree.Path)
	git(t, repo, "push", "-q", "origin", "fix/CSD-9-a:main")
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error { t.Herdr.Closed = false; return nil }); err != nil {
		t.Fatal(err)
	}
	it := cleanable(t, r, repo)
	if it.Verdict != Merged {
		t.Fatalf("item = %+v", it)
	}
	// The user sends the runner more work after the check.
	if _, err := r.Send(context.Background(), tk.ID, "one more fix"); err != nil {
		t.Fatal(err)
	}
	cleaned, err := r.Cleanup(context.Background(), []CleanupItem{it})
	if len(cleaned) != 0 || err == nil || !strings.Contains(err.Error(), "not cleaned") {
		t.Fatalf("Cleanup = %v, %v", cleaned, err)
	}
	if !isDir(tk.Worktree.Path) || fake.count("agent", "send-keys") != 0 {
		t.Error("the reopened runner was closed or its worktree removed")
	}
}

func TestCleanupLeavesAKeptRunner(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk, repo := worktreeTask(t, r, "fix/CSD-10-a")
	herdrOwnsWorktree(fake, repo, tk.Worktree.Path)
	git(t, repo, "push", "-q", "origin", "fix/CSD-10-a:main")
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error { t.Herdr.Closed = false; return nil }); err != nil {
		t.Fatal(err)
	}
	it := cleanable(t, r, repo)
	if it.Verdict != Merged {
		t.Fatalf("item = %+v", it)
	}
	// The user keeps the runner after the check.
	if _, err := r.Keep(context.Background(), tk.ID, true); err != nil {
		t.Fatal(err)
	}
	if kept := cleanable(t, r, repo); kept.Verdict != Kept || !strings.Contains(kept.Detail, "keep --off") {
		t.Errorf("kept item = %+v", kept)
	}
	cleaned, err := r.Cleanup(context.Background(), []CleanupItem{it})
	if len(cleaned) != 0 || err == nil || !strings.Contains(err.Error(), "kept open") {
		t.Fatalf("Cleanup = %v, %v", cleaned, err)
	}
	if !isDir(tk.Worktree.Path) || fake.count("agent", "send-keys") != 0 {
		t.Error("the kept runner was closed or its worktree removed")
	}
}

func TestCleanupLeavesAnAgentInUse(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk, repo := worktreeTask(t, r, "fix/CSD-11-a")
	herdrOwnsWorktree(fake, repo, tk.Worktree.Path)
	git(t, repo, "push", "-q", "origin", "fix/CSD-11-a:main")
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error { t.Herdr.Closed = false; return nil }); err != nil {
		t.Fatal(err)
	}
	fake.on("agent get", ok(`{"result":{"agent":{"agent":"codex","agent_status":"working"}}}`))
	cleaned, err := r.Cleanup(context.Background(), []CleanupItem{cleanable(t, r, repo)})
	if len(cleaned) != 0 || err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("Cleanup = %v, %v", cleaned, err)
	}
	if !isDir(tk.Worktree.Path) || fake.count("agent", "send-keys") != 0 {
		t.Error("the busy runner was closed or its worktree removed")
	}
}

// Listing is the report-only sweep: an old unmerged worktree is named, and
// nothing is removed until cleanup --yes, which removes merged ones only.
func TestCleanupListsOldUnmergedWithoutRemoving(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk, repo := worktreeTask(t, r, "fix/old-a")
	herdrOwnsWorktree(fake, repo, tk.Worktree.Path)
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error {
		t.FinishedAt = time.Now().Add(-72 * time.Hour).UTC()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	it := cleanable(t, r, repo)
	if it.Verdict != Unmerged || it.Detail != "1 commit on fix/old-a not in origin/main" {
		t.Fatalf("old unmerged worktree: %+v", it)
	}
	if !isDir(tk.Worktree.Path) || !BranchExists(context.Background(), repo, "fix/old-a") {
		t.Error("listing removed the worktree or branch")
	}
	if fake.count("worktree", "remove") != 0 || fake.count("agent", "send-keys") != 0 {
		t.Error("listing touched the runner or its worktree")
	}
	if got, _ := r.Store.Get(tk.ID); got.Worktree.Removed || got.Worktree.BranchDeleted {
		t.Errorf("task = %+v", got.Worktree)
	}
}
