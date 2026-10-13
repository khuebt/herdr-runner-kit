package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	// Kept means the runner is kept open; keep --off comes first.
	Kept = "kept"
)

// CleanupCheckEvery is how often wait looks for merged worktrees to offer.
const CleanupCheckEvery = 10 * time.Minute

// CleanupItem is a finished worktree task whose worktree or branch is still
// there.
type CleanupItem struct {
	Task    *task.Task
	Verdict string
	Detail  string
	// Worktree and Branch report what is left to remove. Branch is set only
	// for a branch the task created.
	Worktree, Branch bool
	// Scratch counts untracked files under plans/, and Ignored the ignored
	// paths, that removing a merged worktree deletes.
	Scratch, Ignored int
	// base is the default branch the verdict compared with.
	base string
}

// scratchDir holds the plan files runners write. Untracked files there are
// the only untracked files cleanup may delete.
const scratchDir = "plans/"

// maxListedPaths is how many untracked paths a dirty verdict names.
const maxListedPaths = 5

// Cleanable lists the owner's finished worktree tasks with a worktree or
// branch left, and whether removing them is safe. With fetch it first
// fetches origin once per repository, so merges made on the remote count.
// It reads each worktree live rather than trust the task's report-time git
// snapshot, since the worktree may have changed after the report.
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
			Branch: !t.Worktree.BranchDeleted && t.Worktree.OwnsBranch() &&
				BranchExists(ctx, t.RepoRoot, t.Worktree.Branch),
		}
		if !it.Worktree && !it.Branch {
			continue
		}
		base, seen := bases[t.RepoRoot]
		if !seen {
			if fetch {
				fetchOrigin(ctx, t.RepoRoot, true)
			}
			base = defaultBranch(ctx, t.RepoRoot)
			bases[t.RepoRoot] = base
		}
		it.base = base
		if t.Keep && !t.Herdr.Closed {
			it.Verdict, it.Detail = Kept, "the runner is kept open; run keep --off "+t.ID+" first"
		} else {
			it.Verdict, it.Detail = verdict(ctx, t, base, &it)
		}
		items = append(items, it)
	}
	return items, nil
}

func verdict(ctx context.Context, t *task.Task, base string, it *CleanupItem) (string, string) {
	if it.Worktree {
		st, err := readStatus(ctx, t.Worktree.Path)
		if err != nil {
			return Dirty, "git status failed in " + t.Worktree.Path
		}
		if len(st.Changed) > 0 {
			return Dirty, plural(fmt.Sprint(len(st.Changed)), "changed file") + " in the worktree"
		}
		scratch, other := splitUntracked(st.Untracked)
		if len(other) > 0 {
			return Dirty, "untracked: " + listPaths(other)
		}
		it.Scratch, it.Ignored = len(scratch), st.Ignored
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
	var deleted []string
	if it.Scratch > 0 {
		deleted = append(deleted, plural(fmt.Sprint(it.Scratch), "plan file"))
	}
	if it.Ignored > 0 {
		deleted = append(deleted, plural(fmt.Sprint(it.Ignored), "ignored file"))
	}
	if len(deleted) > 0 {
		detail += "; " + strings.Join(deleted, " and ") + " will be deleted"
	}
	return Merged, detail
}

// splitUntracked separates untracked plan files from every other untracked
// file, which is work cleanup must keep.
func splitUntracked(paths []string) (scratch, other []string) {
	for _, p := range paths {
		if strings.HasPrefix(p, scratchDir) {
			scratch = append(scratch, p)
		} else {
			other = append(other, p)
		}
	}
	return scratch, other
}

// listPaths names the first few paths and counts the rest.
func listPaths(paths []string) string {
	if len(paths) <= maxListedPaths {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(paths[:maxListedPaths], ", "), len(paths)-maxListedPaths)
}

// Cleanup removes the worktree and branch of each merged item: it quits the
// runner if still open, removes the worktree, and deletes the local branch
// the task created. Items that are not merged are skipped. It returns the ids
// it cleaned.
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
	// A runner closed before is fine: only its worktree and branch are left.
	// One kept or in use since the check stays open, and is not cleaned.
	if err := r.closeRunner(ctx, t, false, closeCheck{idle: true, keep: true}); errors.Is(err, errReopened) {
		return fmt.Errorf("%w; not cleaned", err)
	} else if err != nil {
		return err
	}
	if it.Worktree {
		force, err := recheckWorktree(ctx, t.Worktree.Path, it.base)
		if err != nil {
			return err
		}
		if err := r.removeWorktree(ctx, t, force); err != nil {
			return fmt.Errorf("remove worktree: %w", err)
		}
	}
	branchGone := false
	if it.Branch {
		var err error
		if branchGone, err = deleteMergedBranch(ctx, t.RepoRoot, t.Worktree.Branch, it.base); err != nil {
			return err
		}
	}
	t, err := r.Store.Update(t.ID, func(t *task.Task) error {
		t.Worktree.Removed = true
		t.Worktree.BranchDeleted = t.Worktree.BranchDeleted || branchGone
		return nil
	})
	if err != nil {
		return err
	}
	summary := "removed worktree and branch " + t.Worktree.Branch
	if !branchGone {
		summary = "removed the worktree; kept branch " + t.Worktree.Branch
	}
	r.emit(t, "cleaned", summary, "")
	return nil
}

// recheckWorktree reads the worktree again right before removal, since its
// tab may have been used after the check. It reports whether removal must
// force past untracked plan files, the only untracked files it may delete.
func recheckWorktree(ctx context.Context, path, base string) (bool, error) {
	st, err := readStatus(ctx, path)
	if err != nil {
		return false, err
	}
	scratch, other := splitUntracked(st.Untracked)
	if len(st.Changed) > 0 || len(other) > 0 || !isAncestor(ctx, path, "HEAD", base) {
		return false, errors.New("worktree changed since the check")
	}
	return len(scratch) > 0, nil
}

// deleteMergedBranch deletes branch if it is still merged into base, and
// reports whether it is gone. It uses -D, because the branch may be merged
// into the remote default branch only, which the local checkout may not have
// pulled yet. A branch another worktree has checked out is kept: that
// worktree's task, which shares the branch, deletes it.
func deleteMergedBranch(ctx context.Context, root, branch, base string) (bool, error) {
	if !BranchExists(ctx, root, branch) {
		return true, nil
	}
	if !isAncestor(ctx, root, branch, base) {
		return false, fmt.Errorf("branch %s moved since the check", branch)
	}
	if checkedOut(ctx, root, branch) {
		return false, nil
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", root, "branch", "-D", branch).CombinedOutput(); err != nil {
		return false, fmt.Errorf("delete branch: %s", strings.TrimSpace(string(out)))
	}
	return true, nil
}

// checkedOut reports whether any worktree of the repository at root has
// branch checked out.
func checkedOut(ctx context.Context, root, branch string) bool {
	list := gitOut(ctx, root, "worktree", "list", "--porcelain")
	return slices.Contains(strings.Split(list, "\n"), "branch refs/heads/"+branch)
}

// NoticeCleanable emits one cleanable event for each of the owner's merged
// worktrees that was not offered before, so the main session can ask the
// user. It runs at most once per CleanupCheckEvery for an owner, and never
// alongside another check for that owner.
func (r *Runner) NoticeCleanable(ctx context.Context, owner string) {
	unlock, err := r.Store.LockCursor(state.CursorName(owner) + "-cleanup")
	if err != nil {
		return // another wait is checking, or the lock is unusable
	}
	defer unlock()
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
	errOffered := errors.New("offered")
	for _, it := range items {
		if it.Verdict != Merged {
			continue
		}
		// The list is a snapshot; another process may have offered it since.
		t, err := r.Store.Get(it.Task.ID)
		if err != nil || t.Worktree == nil || t.Worktree.Offered {
			continue
		}
		removes := "the worktree and branch"
		switch {
		case !it.Branch:
			removes = "the worktree"
		case !it.Worktree:
			removes = "the branch"
		}
		ev := state.Event{
			Task: t.ID, Owner: t.Owner, Type: "cleanable", Status: t.Status,
			Summary: "branch " + t.Worktree.Branch + " is " + it.Detail + "; cleanup removes " + removes,
		}
		if err := r.record(ev); err != nil {
			// Not marked offered, so the next check offers it again.
			fmt.Fprintf(os.Stderr, "herdr-runner-kit: %s: write event: %v\n", t.ID, err)
			continue
		}
		_, _ = r.Store.Update(t.ID, func(t *task.Task) error {
			if t.Worktree == nil || t.Worktree.Offered {
				return errOffered
			}
			t.Worktree.Offered = true
			return nil
		})
	}
}

// fetchOrigin fetches origin, if the repository has one, ignoring failure.
// prune also drops remote-tracking branches deleted on origin.
func fetchOrigin(ctx context.Context, root string, prune bool) {
	if gitOut(ctx, root, "remote", "get-url", "origin") == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{"-C", root, "fetch", "--quiet"}
	if prune {
		args = append(args, "--prune")
	}
	_ = exec.CommandContext(ctx, "git", append(args, "origin")...).Run()
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
