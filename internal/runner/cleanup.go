package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// Cleanup verdicts of a finished worktree task.
const (
	// Merged means removal is safe: the branch and the worktree's commit
	// are in the default branch, and no tracked file has changes.
	Merged   = "merged"
	Unmerged = "unmerged"
	Dirty    = "dirty"
)

// CleanupCheckEvery is how often wait looks for merged worktrees to offer.
const CleanupCheckEvery = 10 * time.Minute

// CleanupItem is a finished worktree task whose worktree or branch is still
// there.
type CleanupItem struct {
	Task    *task.Task
	Verdict string
	Detail  string
	// Worktree and Branch report what is left to remove.
	Worktree, Branch bool
	// Untracked counts files that removing the worktree would delete.
	Untracked int
}

// Cleanable lists the owner's finished worktree tasks with a worktree or
// branch left, and whether removing them is safe. With fetch it first
// fetches origin once per repository, so merges made on the remote count.
func (r *Runner) Cleanable(ctx context.Context, owner string, fetch bool) ([]CleanupItem, error) {
	tasks, err := r.Store.List()
	if err != nil {
		return nil, err
	}
	bases := map[string]string{}
	var items []CleanupItem
	for _, t := range tasks {
		if (owner != "" && t.Owner != owner) || !t.Status.Terminal() || t.Worktree == nil {
			continue
		}
		it := CleanupItem{
			Task:     t,
			Worktree: !t.Worktree.Removed && isDir(t.Worktree.Path),
			Branch:   !t.Worktree.BranchDeleted && BranchExists(ctx, t.RepoRoot, t.Worktree.Branch),
		}
		if !it.Worktree && !it.Branch {
			continue
		}
		base, seen := bases[t.RepoRoot]
		if !seen {
			if fetch {
				fetchOrigin(ctx, t.RepoRoot)
			}
			base = defaultBranch(ctx, t.RepoRoot)
			bases[t.RepoRoot] = base
		}
		it.Verdict, it.Detail = verdict(ctx, t, base, &it)
		items = append(items, it)
	}
	return items, nil
}

func verdict(ctx context.Context, t *task.Task, base string, it *CleanupItem) (string, string) {
	if it.Worktree {
		status, err := exec.CommandContext(ctx, "git", "-C", t.Worktree.Path, "status", "--porcelain").Output()
		if err != nil {
			return Dirty, "git status failed in " + t.Worktree.Path
		}
		changed := 0
		for _, line := range strings.Split(strings.TrimRight(string(status), "\n"), "\n") {
			switch {
			case line == "":
			case strings.HasPrefix(line, "??"):
				it.Untracked++
			default:
				changed++
			}
		}
		if changed > 0 {
			return Dirty, plural(fmt.Sprint(changed), "changed file") + " in the worktree"
		}
	}
	if base == "" {
		return Unmerged, "no default branch found to compare with"
	}
	refs := map[string]string{}
	if it.Branch {
		refs[t.Worktree.Branch] = t.RepoRoot
	}
	if it.Worktree {
		refs["HEAD"] = t.Worktree.Path // the runner may have left another branch checked out
	}
	for ref, dir := range refs {
		if !isAncestor(ctx, dir, ref, base) {
			n := gitOut(ctx, dir, "rev-list", "--count", base+".."+ref)
			return Unmerged, fmt.Sprintf("%s on %s not in %s", plural(n, "commit"), ref, base)
		}
	}
	detail := "merged into " + base
	if it.Untracked > 0 {
		detail += "; " + plural(fmt.Sprint(it.Untracked), "untracked file") + " in the worktree will be deleted"
	}
	return Merged, detail
}

// Cleanup removes the worktree and branch of each merged item: it quits the
// runner if still open, removes the worktree, and deletes the local branch.
// Items that are not merged are skipped. It returns the ids it cleaned.
func (r *Runner) Cleanup(ctx context.Context, items []CleanupItem) ([]string, error) {
	var cleaned []string
	var errs []error
	for _, it := range items {
		if it.Verdict != Merged {
			continue
		}
		if err := r.cleanOne(ctx, it); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", it.Task.ID, err))
			continue
		}
		cleaned = append(cleaned, it.Task.ID)
	}
	return cleaned, errors.Join(errs...)
}

func (r *Runner) cleanOne(ctx context.Context, it CleanupItem) error {
	t := it.Task
	if err := r.closeRunner(ctx, t, false); err != nil {
		return err
	}
	if it.Worktree {
		force := it.Untracked > 0
		if err := r.Herdr.WorktreeRemove(ctx, t.Herdr.WorkspaceID, force); err != nil {
			// The workspace may be closed already; remove the worktree with git.
			args := []string{"-C", t.RepoRoot, "worktree", "remove", t.Worktree.Path}
			if force {
				args = append(args, "--force")
			}
			if out, gerr := exec.CommandContext(ctx, "git", args...).CombinedOutput(); gerr != nil {
				return fmt.Errorf("remove worktree: %w; git: %s", err, strings.TrimSpace(string(out)))
			}
		}
	}
	if it.Branch {
		// -D, because the branch is merged into the remote default branch,
		// which the local checkout may not have pulled yet.
		if out, err := exec.CommandContext(ctx, "git", "-C", t.RepoRoot, "branch", "-D", t.Worktree.Branch).CombinedOutput(); err != nil {
			return fmt.Errorf("delete branch: %s", strings.TrimSpace(string(out)))
		}
	}
	t, err := r.Store.Update(t.ID, func(t *task.Task) error {
		t.Worktree.Removed, t.Worktree.BranchDeleted = true, true
		return nil
	})
	if err != nil {
		return err
	}
	r.emit(t, "cleaned", "removed worktree and branch "+t.Worktree.Branch, "")
	return nil
}

// NoticeCleanable emits one cleanable event for each of the owner's merged
// worktrees that was not offered before, so the main session can ask the
// user. It runs at most once per CleanupCheckEvery for an owner.
func (r *Runner) NoticeCleanable(ctx context.Context, owner string) {
	mark := filepath.Join(r.Store.Dir(), state.CursorName(owner)+"-cleanup")
	if info, err := os.Stat(mark); err == nil && time.Since(info.ModTime()) < CleanupCheckEvery {
		return
	}
	if err := os.WriteFile(mark, nil, 0o600); err != nil {
		return
	}
	now := time.Now()
	_ = os.Chtimes(mark, now, now)
	items, err := r.Cleanable(ctx, owner, true)
	if err != nil {
		return
	}
	for _, it := range items {
		if it.Verdict != Merged || it.Task.Worktree.Offered {
			continue
		}
		t, err := r.Store.Update(it.Task.ID, func(t *task.Task) error {
			t.Worktree.Offered = true
			return nil
		})
		if err == nil {
			r.emit(t, "cleanable", "branch "+t.Worktree.Branch+" is "+it.Detail+"; cleanup removes the worktree and branch", "")
		}
	}
}

func fetchOrigin(ctx context.Context, root string) {
	if gitOut(ctx, root, "remote", "get-url", "origin") == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "git", "-C", root, "fetch", "--quiet", "--prune", "origin").Run()
}

// defaultBranch returns the ref merged work lands on: origin's HEAD, else
// the first of origin/main, origin/master, main and master that exists.
func defaultBranch(ctx context.Context, root string) string {
	if ref := gitOut(ctx, root, "rev-parse", "--abbrev-ref", "origin/HEAD"); ref != "" && ref != "origin/HEAD" {
		return ref
	}
	for _, ref := range []string{"origin/main", "origin/master", "main", "master"} {
		if gitOut(ctx, root, "rev-parse", "--verify", "--quiet", ref) != "" {
			return ref
		}
	}
	return ""
}

func isAncestor(ctx context.Context, dir, ref, base string) bool {
	return exec.CommandContext(ctx, "git", "-C", dir, "merge-base", "--is-ancestor", ref, base).Run() == nil
}

func plural(n, word string) string {
	if n == "1" {
		return n + " " + word
	}
	return n + " " + word + "s"
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
