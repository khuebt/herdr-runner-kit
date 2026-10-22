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
	"sync"
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
	// Kept means the worktree and branch stay although merged: the runner
	// is kept open, and keep --off comes first, or a task that has no
	// worktree yet starts from the branch.
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
	// base is the default branch the verdict compared with, and tips the
	// commits the kit found merged that count as merged references too.
	base string
	tips []string
	// stacked is set when the branch counts as merged only through another
	// branch's tip.
	stacked bool
	// branchAt and headAt are the commits of the branch and the worktree's
	// HEAD that a merged verdict checked. Only these are recorded, and
	// removal stops when either moved past them.
	branchAt, headAt string
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
	tips := map[string][]string{}
	for _, t := range tasks {
		if t.Worktree != nil {
			tips[tipKey(t)] = append(tips[tipKey(t)], t.Worktree.MergedTips()...)
		}
	}
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
		it.base, it.tips = base, tips[tipKey(t)]
		if t.Keep && !t.Herdr.Closed {
			it.Verdict, it.Detail = Kept, "the runner is kept open; run keep --off "+t.ID+" first"
		} else {
			it.Verdict, it.Detail = verdict(ctx, t, base, &it)
		}
		items = append(items, it)
	}
	// A branch stacked under one found merged now counts as merged too.
	// Ancestry is transitive, so one more pass is enough.
	live := map[string][]string{}
	for _, it := range items {
		if it.Verdict != Merged {
			continue
		}
		live[tipKey(it.Task)] = append(live[tipKey(it.Task)], it.checked()...)
	}
	for i := range items {
		it := &items[i]
		if more := live[tipKey(it.Task)]; it.Verdict == Unmerged && len(more) > 0 {
			it.tips = append(slices.Clone(it.tips), more...)
			it.Verdict, it.Detail = verdict(ctx, it.Task, it.base, it)
		}
	}
	for _, it := range items {
		if it.Verdict == Merged && !it.stacked {
			// The branch still counts as merged once later commits on the
			// default branch change the same lines and its squash merge no
			// longer applies cleanly.
			if err := r.recordMergedTips(ctx, it.Task, it.checked()); err != nil {
				fmt.Fprintf(os.Stderr, "hrk: %s: %v\n", it.Task.ID, err)
			}
		}
	}
	spared := startsFrom(tasks)
	for i := range items {
		it := &items[i]
		if id := spared[[2]string{it.Task.RepoRoot, it.Task.Worktree.Branch}]; it.Verdict == Merged && it.Branch && id != "" {
			it.Verdict, it.Detail = Kept, id+" starts from it"
		}
	}
	return items, nil
}

// branchBaseLock orders spawn's choice of a task's base against cleanup's
// deletion of that branch.
const branchBaseLock = "branch-base"

// startsFrom maps each repository and branch that an unfinished task with
// no worktree yet will start from to that task: its base, or else the
// branch of its single parent with one, as parentInputs picks it.
func startsFrom(tasks []*task.Task) map[[2]string]string {
	byID := make(map[string]*task.Task, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}
	out := map[[2]string]string{}
	for _, t := range tasks {
		if t.Status.Terminal() || t.Worktree == nil || t.Worktree.Path != "" {
			continue
		}
		from := t.Worktree.Base
		if from == "" {
			var branches []string
			for _, id := range t.After {
				if p := byID[id]; p != nil && p.Worktree != nil && p.Worktree.Branch != "" {
					branches = append(branches, p.Worktree.Branch)
				}
			}
			if len(branches) == 1 {
				from = branches[0]
			}
		}
		if from != "" {
			out[[2]string{t.RepoRoot, from}] = t.ID
		}
	}
	return out
}

// recordMergedTips records tips, commits the check found merged, on task t,
// keeping every tip recorded before that is not an ancestor of them. Refs
// under refs/hrk/merged keep each recorded tip from being pruned once no
// branch holds it. They are written first, and a failed one leaves the
// record as it was: a ref without the record only pins a commit, while a
// record without its ref can lose the commit to gc.
func (r *Runner) recordMergedTips(ctx context.Context, t *task.Task, tips []string) error {
	errSame := errors.New("unchanged")
	_, err := r.Store.Update(t.ID, func(t *task.Task) error {
		changed := false
		for _, tip := range tips {
			if t.Worktree != nil && t.Worktree.AddMergedTip(tip, func(a, b string) bool { return isAncestor(ctx, t.RepoRoot, a, b) }) {
				changed = true
			}
		}
		if !changed {
			return errSame
		}
		for _, ref := range t.Worktree.MergedTipRefs(t.ID) {
			if out, err := exec.CommandContext(ctx, "git", "-C", t.RepoRoot, "update-ref", ref[0], ref[1]).CombinedOutput(); err != nil {
				return fmt.Errorf("record merged commit %s: %s", ref[1], strings.TrimSpace(string(out)))
			}
		}
		return nil
	})
	if errors.Is(err, errSame) {
		return nil
	}
	return err
}

// tipKey scopes merged tips to one owner and repository.
func tipKey(t *task.Task) string {
	return t.Owner + "\x00" + t.RepoRoot
}

// checked returns the commits a merged verdict checked.
func (it CleanupItem) checked() []string {
	return slices.DeleteFunc([]string{it.branchAt, it.headAt}, func(c string) bool { return c == "" })
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
	// Each ref is read once, and its commit is checked and kept, so a commit
	// made during the check is never taken as merged.
	type gitRef struct {
		ref, dir string
		at       *string
	}
	var refs []gitRef
	if it.Branch {
		refs = append(refs, gitRef{t.Worktree.Branch, t.RepoRoot, &it.branchAt})
	}
	if it.Worktree {
		refs = append(refs, gitRef{"HEAD", t.Worktree.Path, &it.headAt}) // the runner may have left another branch checked out
	}
	it.branchAt, it.headAt = "", ""
	// A branch under its own recorded tip is merged by itself, unless another
	// branch's tip holds it too.
	own := t.Worktree.MergedTips()
	others := slices.DeleteFunc(slices.Clone(it.tips), func(tip string) bool { return slices.Contains(own, tip) })
	squashed, stacked := false, false
	at := make([]string, len(refs))
	for i, r := range refs {
		c := gitOut(ctx, r.dir, "rev-parse", "--verify", "--quiet", r.ref+"^{commit}")
		if c == "" {
			return Unmerged, "cannot read " + r.ref
		}
		ok, bySquash, under := mergedOrUnder(ctx, r.dir, c, base, others)
		if !ok && underAny(ctx, r.dir, c, own) {
			ok, bySquash = true, true
		}
		if !ok {
			n := gitOut(ctx, r.dir, "rev-list", "--count", base+".."+c)
			return Unmerged, fmt.Sprintf("%s on %s not in %s", plural(n, "commit"), r.ref, base)
		}
		at[i] = c
		squashed, stacked = squashed || bySquash, stacked || under
	}
	for i, r := range refs {
		*r.at = at[i]
	}
	it.stacked = stacked
	detail := "merged into " + base
	switch {
	case stacked:
		detail += " (squash or rebase of a branch stacked on it)"
	case squashed:
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
		force, err := recheckWorktree(ctx, t.Worktree.Path, it.base, it.tips, it.headAt)
		if err != nil {
			return err
		}
		if err := r.removeWorktree(ctx, t, force); err != nil {
			return fmt.Errorf("remove worktree: %w", err)
		}
	}
	// Spawn writes a task's base under this lock, and the deletion is
	// recorded before it is released: a task spawned since the check either
	// shows here, or reads the branch as deleted.
	unlock, err := r.Store.Lock(branchBaseLock)
	if err != nil {
		return err
	}
	defer unlock()
	branchGone, startsFromIt := false, ""
	if it.Branch {
		tasks, err := r.Store.List()
		if err != nil {
			return err
		}
		startsFromIt = startsFrom(tasks)[[2]string{t.RepoRoot, t.Worktree.Branch}]
		if startsFromIt == "" {
			if branchGone, err = deleteMergedBranch(ctx, t.RepoRoot, t.Worktree.Branch, it.base, it.tips, it.branchAt); err != nil {
				return err
			}
		}
	}
	if err := r.recordMergedTips(ctx, t, it.checked()); err != nil {
		fmt.Fprintf(os.Stderr, "hrk: %s: %v\n", t.ID, err)
	}
	t, err = r.Store.Update(t.ID, func(t *task.Task) error {
		t.Worktree.Removed = true
		t.Worktree.BranchDeleted = t.Worktree.BranchDeleted || branchGone
		return nil
	})
	if err != nil {
		return err
	}
	summary := "removed worktree and branch " + t.Worktree.Branch
	switch {
	case startsFromIt != "":
		summary = "removed the worktree; kept branch " + t.Worktree.Branch + ", which " + startsFromIt + " starts from"
	case !branchGone:
		summary = "removed the worktree; kept branch " + t.Worktree.Branch
	}
	r.emit(t, "cleaned", summary, "")
	return nil
}

// recheckWorktree reads the worktree again right before removal, since its
// tab may have been used after the check: HEAD must still be at or under
// verified, the commit the check found merged. It reports whether removal
// must force past untracked plan files, the only untracked files it may
// delete.
func recheckWorktree(ctx context.Context, path, base string, tips []string, verified string) (bool, error) {
	st, err := readStatus(ctx, path)
	if err != nil {
		return false, err
	}
	scratch, other := splitUntracked(st.Untracked)
	head := gitOut(ctx, path, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	ok := head != "" && verified != "" && isAncestor(ctx, path, head, verified)
	if ok {
		ok, _, _ = mergedOrUnder(ctx, path, head, base, tips)
	}
	if len(st.Changed) > 0 || len(other) > 0 || !ok {
		return false, errors.New("worktree changed since the check")
	}
	return len(scratch) > 0, nil
}

// deleteMergedBranch deletes branch if it is still at or under verified, the
// commit the check found merged, and still merged into base, and reports
// whether it is gone. The branch may be merged into the remote default
// branch only, which the local checkout may not have pulled yet, so git's
// own merged test is not used. A branch another worktree has checked out is
// kept: that worktree's task, which shares the branch, deletes it.
func deleteMergedBranch(ctx context.Context, root, branch, base string, tips []string, verified string) (bool, error) {
	if !BranchExists(ctx, root, branch) {
		return true, nil
	}
	ref := "refs/heads/" + branch
	at := gitOut(ctx, root, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	ok := at != "" && verified != "" && isAncestor(ctx, root, at, verified)
	if ok {
		ok, _, _ = mergedOrUnder(ctx, root, at, base, tips)
	}
	if !ok {
		return false, fmt.Errorf("branch %s moved since the check", branch)
	}
	if checkedOut(ctx, root, branch) {
		return false, nil
	}
	// Deleted only while it still points at the commit checked, so a commit
	// made meanwhile keeps the branch.
	if out, err := exec.CommandContext(ctx, "git", "-C", root, "update-ref", "-d", ref, at).CombinedOutput(); err != nil {
		return false, fmt.Errorf("delete branch: %s", strings.TrimSpace(string(out)))
	}
	_ = exec.CommandContext(ctx, "git", "-C", root, "config", "--remove-section", "branch."+branch).Run()
	return true, nil
}

// checkedOut reports whether any worktree of the repository at root has
// branch checked out, or is rebasing or bisecting it, as git branch -D
// checks: such a worktree shows a detached HEAD.
func checkedOut(ctx context.Context, root, branch string) bool {
	for line := range strings.SplitSeq(gitOut(ctx, root, "worktree", "list", "--porcelain"), "\n") {
		if line == "branch refs/heads/"+branch {
			return true
		}
		wt, ok := strings.CutPrefix(line, "worktree ")
		if !ok {
			continue
		}
		for _, f := range []string{"rebase-merge/head-name", "rebase-apply/head-name", "BISECT_START"} {
			path := gitOut(ctx, wt, "rev-parse", "--git-path", f)
			if !filepath.IsAbs(path) {
				path = filepath.Join(wt, path)
			}
			if b, err := os.ReadFile(path); err == nil {
				if name := strings.TrimSpace(string(b)); name == branch || name == "refs/heads/"+branch {
					return true
				}
			}
		}
	}
	return false
}

// startedInLog returns the ids of the tasks with a started event. It reads
// the whole event log, so only tasks an older version started, which have no
// StartedAt, need it.
func (r *Runner) startedInLog() (map[string]bool, error) {
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

// startedBefore reports whether the log read by started names task id. A log
// that cannot be read counts as started, so nothing is called unstarted
// without evidence.
func startedBefore(started func() (map[string]bool, error), id string) bool {
	m, err := started()
	return err != nil || m[id]
}

// removeUnstarted removes the worktree and branch the launch of task id
// created when its agent never started and nothing was written there; the
// worktree's removal closes its workspace. Any doubt keeps them for cleanup.
func (r *Runner) removeUnstarted(ctx context.Context, id string) {
	t, err := r.Store.Get(id)
	if err != nil || t.Worktree == nil || t.Worktree.Removed || !t.Worktree.OwnsBranch() || !isDir(t.Worktree.Path) {
		return
	}
	if !t.StartedAt.IsZero() {
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
	branchGone, _ := deleteMergedBranch(ctx, t.RepoRoot, branch, base, nil, base)
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
	// The log is read once, and only for a task with no StartedAt to say.
	started := sync.OnceValues(r.startedInLog)
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
		if t.StartedAt.IsZero() && t.Status != task.Done && !startedBefore(started, t.ID) {
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

// mergedOrUnder reports whether ref is merged into base, or else is an
// ancestor of one of tips, commits the kit found merged: a squash merge of a
// branch leaves the commits of the branches stacked under it off base.
func mergedOrUnder(ctx context.Context, dir, ref, base string, tips []string) (ok, bySquash, under bool) {
	if ok, bySquash := merged(ctx, dir, ref, base); ok {
		return true, bySquash, false
	}
	if underAny(ctx, dir, ref, tips) {
		return true, true, true
	}
	return false, false, false
}

// holdsWork reports whether branch has the work of tips, commits found
// merged: it has everything on the ref cleanup checks merges against, or
// each tip is merged into it. A local default branch not yet updated from
// the remote, where the merge happened, has neither.
func holdsWork(ctx context.Context, root, branch string, tips []string) bool {
	if ref := defaultBranch(ctx, root); ref != "" && isAncestor(ctx, root, ref, branch) {
		return true
	}
	return !slices.ContainsFunc(tips, func(tip string) bool {
		ok, _ := merged(ctx, root, tip, branch)
		return !ok
	})
}

// underAny reports whether ref is one of tips or an ancestor of one.
func underAny(ctx context.Context, dir, ref string, tips []string) bool {
	return slices.ContainsFunc(tips, func(tip string) bool { return isAncestor(ctx, dir, ref, tip) })
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
