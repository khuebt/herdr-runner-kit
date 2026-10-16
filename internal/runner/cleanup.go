package runner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
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

// Owners selects tasks by owner: one main session's folder, and with
// Projects the tasks of every project too. An empty Owner selects every
// task.
type Owners struct {
	Owner    string
	Projects bool
}

// Has reports whether o selects the tasks of owner.
func (o Owners) Has(owner string) bool {
	return o.Owner == "" || owner == o.Owner || (o.Projects && task.IsProjectOwner(owner))
}

// Cleanable lists the owners' finished worktree tasks with a worktree or
// branch left, and whether removing them is safe. With fetch it first
// fetches origin once per repository, so merges made on the remote count.
// It reads each worktree live rather than trust the task's report-time git
// snapshot, since the worktree may have changed after the report. A
// worktree removed outside the kit is marked removed on the way.
func (r *Runner) Cleanable(ctx context.Context, owners Owners, fetch bool) ([]CleanupItem, error) {
	tasks, err := r.Store.List()
	if err != nil {
		return nil, err
	}
	bases := map[string]string{}
	var items []CleanupItem
	for _, t := range tasks {
		if !owners.Has(t.Owner) || !t.Status.Terminal() || t.Worktree == nil {
			continue
		}
		if !t.Worktree.Removed && t.Worktree.Path != "" && notExist(t.Worktree.Path) {
			if t = r.markRemovedOutside(ctx, t); t == nil {
				continue
			}
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

// markRemovedOutside records that t's worktree folder was removed outside
// the kit, and closes the runner's workspace left open on the deleted
// folder. When its branch is gone too, or was never the task's, nothing is
// left to clean: the task is marked cleaned, with a cleaned event, and nil
// is returned. Otherwise the branch stays for the verdict.
func (r *Runner) markRemovedOutside(ctx context.Context, t *task.Task) *task.Task {
	owns := t.Worktree.OwnsBranch()
	// A repository that cannot be read keeps the branch for the verdict.
	branchGone := !owns || t.Worktree.BranchDeleted || (isDir(t.RepoRoot) && !BranchExists(ctx, t.RepoRoot, t.Worktree.Branch))
	errSame := errors.New("unchanged")
	nt, err := r.Store.Update(t.ID, func(t *task.Task) error {
		if t.Worktree == nil || t.Worktree.Removed {
			return errSame
		}
		t.Worktree.Removed = true
		if branchGone && owns {
			t.Worktree.BranchDeleted = true
		}
		return nil
	})
	switch {
	case errors.Is(err, errSame):
		return nil // another process marked it
	case err != nil:
		return t
	}
	r.closeOrphanWorkspace(ctx, nt)
	if branchGone {
		r.emit(nt, "cleaned", "removed outside the kit", "")
		return nil
	}
	return nt
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
	// The branch is checked before the worktree's HEAD, so the detail names
	// the branch whenever it is the one not merged.
	type gitRef struct{ ref, dir string }
	var refs []gitRef
	if it.Branch {
		refs = append(refs, gitRef{t.Worktree.Branch, t.RepoRoot})
	}
	if it.Worktree {
		refs = append(refs, gitRef{"HEAD", t.Worktree.Path}) // the runner may have left another branch checked out
	}
	squashed := false
	for _, r := range refs {
		ok, bySquash := merged(ctx, r.dir, r.ref, base)
		if !ok {
			n := gitOut(ctx, r.dir, "rev-list", "--count", base+".."+r.ref)
			return Unmerged, fmt.Sprintf("%s on %s not in %s", plural(n, "commit"), r.ref, base)
		}
		squashed = squashed || bySquash
	}
	detail := "merged into " + base
	if squashed {
		detail += " (squash or rebase)"
	}
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
	if ok, _ := merged(ctx, path, "HEAD", base); len(st.Changed) > 0 || len(other) > 0 || !ok {
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
	if ok, _ := merged(ctx, root, branch, base); !ok {
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

// startedTasks returns the ids of the tasks whose agent ever got its first
// prompt: those with a started event.
func (r *Runner) startedTasks() (map[string]bool, error) {
	all, _, err := r.Store.ReadEventsSince(0)
	if err != nil {
		return nil, err
	}
	started := map[string]bool{}
	for _, ev := range all {
		if ev.Type == "started" {
			started[ev.Task] = true
		}
	}
	return started, nil
}

// removeUnstarted removes the worktree and branch the launch of task id
// created when its agent never started and nothing was written there; the
// worktree's removal closes its workspace. Any doubt keeps them for cleanup.
func (r *Runner) removeUnstarted(ctx context.Context, id string) {
	t, err := r.Store.Get(id)
	if err != nil || t.Worktree == nil || t.Worktree.Removed || !t.Worktree.OwnsBranch() || !isDir(t.Worktree.Path) {
		return
	}
	if started, err := r.startedTasks(); err != nil || started[id] {
		return
	}
	wt, branch := t.Worktree.Path, t.Worktree.Branch
	// Nothing written: no change or untracked file, and no commit beyond
	// the one the branch started from.
	base := gitOut(ctx, t.RepoRoot, "rev-parse", "--verify", "--quiet", cmp.Or(t.Worktree.Base, "HEAD")+"^{commit}")
	st, err := readStatus(ctx, wt)
	if err != nil || len(st.Changed) > 0 || len(st.Untracked) > 0 || base == "" ||
		!BranchExists(ctx, t.RepoRoot, branch) || !isAncestor(ctx, t.RepoRoot, branch, base) || !isAncestor(ctx, wt, "HEAD", base) {
		return
	}
	if err := r.removeWorktree(ctx, t, false); err != nil {
		return
	}
	branchGone, _ := deleteMergedBranch(ctx, t.RepoRoot, branch, base)
	t, err = r.Store.Update(id, func(t *task.Task) error {
		t.Worktree.Removed = true
		t.Worktree.BranchDeleted = t.Worktree.BranchDeleted || branchGone
		t.Herdr.Closed = true // the pane went with the worktree's workspace
		return nil
	})
	if err != nil {
		return
	}
	summary := "never started; removed its empty worktree and branch"
	if !branchGone {
		summary = "never started; removed its empty worktree; kept branch " + branch
	}
	r.emit(t, "cleaned", summary, "")
}

// NoticeCleanable emits one cleanable event for each of the owner's merged
// worktrees that was not offered before, so the main session can ask the
// user. It runs at most once per CleanupCheckEvery for an owner, and never
// alongside another check for that owner.
func (r *Runner) NoticeCleanable(ctx context.Context, owner string) {
	unlock, err := r.Store.LockCursor(state.CursorName(owner)+"-cleanup", "cleanup")
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
	items, err := r.Cleanable(ctx, Owners{Owner: owner}, true)
	if err != nil {
		return
	}
	errOffered := errors.New("offered")
	started, startedErr := r.startedTasks()
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
		summary := "branch " + t.Worktree.Branch + " is " + it.Detail
		if startedErr == nil && !started[t.ID] && t.Status != task.Done {
			// A branch with no commits counts as merged; say why it has none.
			summary = "never started"
			if reason, _, _ := strings.Cut(t.Error, "\n"); reason != "" {
				summary += " (" + reason + ")"
			}
		}
		ev := state.Event{
			Task: t.ID, Owner: t.Owner, Type: "cleanable", Status: t.Status,
			Summary: summary + "; cleanup removes " + removes,
		}
		if err := r.record(ev); err != nil {
			// Not marked offered, so the next check offers it again.
			fmt.Fprintf(os.Stderr, "hrk: %s: write event: %v\n", t.ID, err)
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

// merged reports whether ref's work is in base: ref is an ancestor of base,
// or merging ref into base would change nothing, as after a squash or
// rebase merge. bySquash is set when only the second test held. A git
// without merge-tree --write-tree (before 2.38), or a repository with a
// custom merge driver, which may drop one side's changes, has ancestry only.
func merged(ctx context.Context, dir, ref, base string) (ok, bySquash bool) {
	if isAncestor(ctx, dir, ref, base) {
		return true, false
	}
	if gitOut(ctx, dir, "config", "--get-regexp", `^merge\..*\.driver$`) != "" {
		return false, false
	}
	// On success the first line is the merged tree; a conflict exits 1.
	out := gitOut(ctx, dir, "merge-tree", "--write-tree", base, ref)
	tree, _, _ := strings.Cut(out, "\n")
	baseTree := gitOut(ctx, dir, "rev-parse", base+"^{tree}")
	if tree != "" && tree == baseTree {
		return true, true
	}
	return false, false
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

// notExist reports whether path is known to be gone, not merely unreadable.
func notExist(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, fs.ErrNotExist)
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
