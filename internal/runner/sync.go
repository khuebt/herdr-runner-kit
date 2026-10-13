package runner

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// ErrSyncInsideRunner is returned when sync runs inside a runner: only the
// main session merges into a runner's branch.
var ErrSyncInsideRunner = errors.New("runners cannot sync runners")

// ErrSyncConflict means the merge conflicted and was aborted, so the
// worktree's HEAD is unchanged.
var ErrSyncConflict = errors.New("the merge conflicts")

// SyncResult describes what sync did to a runner's branch.
type SyncResult struct {
	// Ref is the ref merged, and SHA the short commit it pointed to.
	Ref, SHA string
	// Branch is the runner's branch the ref was merged into.
	Branch string
	// UpToDate is set when the branch already contained the ref, so
	// nothing was merged or sent.
	UpToDate bool
	// Conflicts lists the files of a merge that was aborted.
	Conflicts []string
	// Sent is set when the runner was told about the merge, and Working
	// when herdr saw it start a turn.
	Sent, Working bool
}

// SyncNote is the prompt that tells a runner its branch was synced.
func SyncNote(ref, sha string) string {
	return fmt.Sprintf("`%s` (%s) was merged into your branch. Re-run the checks and report.", ref, sha)
}

// SyncConflictNote is the prompt that hands a conflicting merge to the
// runner.
func SyncConflictNote(ref, sha string, files []string) string {
	return fmt.Sprintf("Merge `%s` (%s) into your branch: it conflicts in %s. "+
		"Resolve the conflicts, commit the merge, re-run the checks and report.",
		ref, sha, strings.Join(files, ", "))
}

// Sync merges ref, or the default branch when ref is empty, into the branch
// checked out in id's worktree, and with send tells the runner. It refuses a
// worktree with changes, an agent in use, and a worktree on another branch.
// A conflicting merge is aborted, leaving HEAD where it was. It never pushes,
// rebases or touches any other branch.
func (r *Runner) Sync(ctx context.Context, id, ref string, send bool) (*SyncResult, error) {
	if r.Getenv("HRK_TASK") != "" {
		return nil, ErrSyncInsideRunner
	}
	t, err := r.Store.Get(id)
	if err != nil {
		return nil, err
	}
	if t.Worktree == nil || t.Worktree.Removed || !isDir(t.Worktree.Path) {
		return nil, fmt.Errorf("%s has no worktree to sync", id)
	}
	wt := t.Worktree.Path
	if ref == "" {
		fetchOrigin(ctx, t.RepoRoot)
		if ref = defaultBranch(ctx, t.RepoRoot); ref == "" {
			return nil, fmt.Errorf("no default branch found in %s; name the ref with --from", t.RepoRoot)
		}
	}
	if strings.HasPrefix(ref, "-") {
		return nil, fmt.Errorf("%q is not a ref", ref)
	}
	sha := gitOut(ctx, wt, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if sha == "" {
		return nil, fmt.Errorf("%s is not a commit in %s", ref, wt)
	}
	res := &SyncResult{Ref: ref, SHA: shortSHA(sha), Branch: t.Worktree.Branch}
	if err := r.syncable(ctx, t); err != nil {
		return nil, err
	}
	if isAncestor(ctx, wt, sha, "HEAD") {
		res.UpToDate = true
		return res, nil
	}

	msg := fmt.Sprintf("Merge %s (%s) into %s", ref, res.SHA, res.Branch)
	out, mergeErr := exec.CommandContext(ctx, "git", "-C", wt, "merge", "--no-edit", "-m", msg, sha).CombinedOutput()
	if mergeErr != nil {
		return res, r.abortMerge(ctx, wt, res, strings.TrimSpace(string(out)))
	}
	r.emit(t, "synced", fmt.Sprintf("merged %s (%s) into %s", ref, res.SHA, res.Branch), "")
	if !send || t.Herdr.PaneID == "" || t.Herdr.Closed || t.Status == task.Stopped {
		return res, nil
	}
	if res.Working, err = r.Send(ctx, id, SyncNote(ref, res.SHA)); err != nil {
		return res, fmt.Errorf("merged, but telling %s failed: %w", id, err)
	}
	res.Sent = true
	return res, nil
}

// syncable refuses a merge someone may be disturbed by: an agent in use, a
// task still starting, a worktree with changes, or a worktree that has left
// the task's branch.
func (r *Runner) syncable(ctx context.Context, t *task.Task) error {
	if t.Status == task.Starting {
		return fmt.Errorf("%s is starting; sync it once its runner is up", t.ID)
	}
	if t.Herdr.PaneID != "" && !t.Herdr.Closed && r.inUse(ctx, t.Herdr.PaneID) {
		return fmt.Errorf("%s's agent is busy; sync it once it is idle", t.ID)
	}
	st, err := readStatus(ctx, t.Worktree.Path)
	if err != nil {
		return err
	}
	_, untracked := splitUntracked(st.Untracked)
	if len(st.Changed) > 0 || len(untracked) > 0 {
		return fmt.Errorf("%s's worktree has uncommitted changes (%s); it was not synced",
			t.ID, listPaths(append(st.Changed, untracked...)))
	}
	if st.Head != t.Worktree.Branch {
		return fmt.Errorf("%s's worktree is on %s, not its branch %s; it was not synced", t.ID, st.Head, t.Worktree.Branch)
	}
	return nil
}

// abortMerge undoes a failed merge. A conflict leaves a merge in progress,
// which is aborted; any other failure, such as an untracked file in the
// way, stopped before changing anything.
func (r *Runner) abortMerge(ctx context.Context, wt string, res *SyncResult, output string) error {
	if gitOut(ctx, wt, "rev-parse", "--verify", "--quiet", "MERGE_HEAD") == "" {
		return fmt.Errorf("git merge %s: %s", res.Ref, output)
	}
	if files := strings.Trim(gitOut(ctx, wt, "diff", "--name-only", "-z", "--diff-filter=U"), "\x00"); files != "" {
		res.Conflicts = strings.Split(files, "\x00")
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", wt, "merge", "--abort").CombinedOutput(); err != nil {
		return fmt.Errorf("%w, and git merge --abort failed in %s: %s", ErrSyncConflict, wt, strings.TrimSpace(string(out)))
	}
	return fmt.Errorf("%w; it was aborted and %s is unchanged", ErrSyncConflict, res.Branch)
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
