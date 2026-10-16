package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// removeWorktree removes the task's worktree. herdr removes it, closing its
// workspace, only while that workspace is still open on this worktree: herdr
// reuses the ids of closed workspaces, so an old id may name another
// checkout. Otherwise git removes it, and the runner's workspace is closed
// if still open on it (closeOrphanWorkspace). Without
// force, both refuse a worktree with changes or untracked files. Once the
// worktree is gone, its repository folder in herdr's worktree root goes too
// when no other worktree is left in it.
func (r *Runner) removeWorktree(ctx context.Context, t *task.Task, force bool) error {
	if t.Worktree == nil || t.Worktree.Path == "" {
		return nil // never created
	}
	if err := r.refuseSharedCheckout(t); err != nil {
		return err
	}
	path := filepath.Clean(t.Worktree.Path)
	if err := r.removeCheckout(ctx, t, path, force); err != nil {
		return err
	}
	removeEmptyRepoFolder(path)
	return nil
}

// refuseSharedCheckout refuses to remove t's worktree while another task
// of its plan run still works in it.
func (r *Runner) refuseSharedCheckout(t *task.Task) error {
	tasks, err := r.Store.List()
	if err != nil {
		return err
	}
	for _, q := range tasks {
		if q.CheckoutOf == t.ID && !q.Status.Terminal() {
			return fmt.Errorf("%s works in this checkout", q.ID)
		}
	}
	return nil
}

func (r *Runner) removeCheckout(ctx context.Context, t *task.Task, path string, force bool) error {
	if id := t.Herdr.WorkspaceID; id != "" {
		if checkout, err := r.Herdr.WorkspaceCheckout(ctx, id); err == nil && checkout != "" && filepath.Clean(checkout) == path {
			// herdr cannot remove a worktree whose folder is already gone.
			if err := r.Herdr.WorktreeRemove(ctx, id, force); err == nil || isDir(path) {
				return err
			}
		}
	}
	if isDir(path) {
		args := []string{"-C", t.RepoRoot, "worktree", "remove"}
		if force {
			args = append(args, "--force")
		}
		if out, err := exec.CommandContext(ctx, "git", append(args, path)...).CombinedOutput(); err != nil {
			return fmt.Errorf("git worktree remove: %s", strings.TrimSpace(string(out)))
		}
	}
	r.closeOrphanWorkspace(ctx, t)
	return nil
}

// closeOrphanWorkspace closes the herdr workspace of t, whose worktree is
// gone, so herdr does not keep it open on a deleted folder. A workspace
// another unfinished task still works in, as the phases of a plan run do,
// stays. herdr reuses the ids of closed workspaces, so the id counts only
// while the workspace still shows the worktree or, after a herdr restart,
// which drops a deleted checkout, still has the runner's label. Failures
// are ignored: the workspace is only left open, as before.
func (r *Runner) closeOrphanWorkspace(ctx context.Context, t *task.Task) {
	id := t.Herdr.WorkspaceID
	if id == "" || t.Worktree == nil || t.Worktree.Path == "" {
		return
	}
	tasks, err := r.Store.List()
	if err != nil {
		return
	}
	for _, q := range tasks {
		if q.ID != t.ID && q.Herdr.WorkspaceID == id && !q.Status.Terminal() {
			return
		}
	}
	checkout, err := r.Herdr.WorkspaceCheckout(ctx, id)
	if err != nil {
		return // closed already, or herdr did not answer
	}
	if checkout == "" {
		// The label launch gave the runner's workspace.
		if label, found, err := r.Herdr.WorkspaceLabel(ctx, id); err != nil || !found || label != t.ID+" · "+t.Title {
			return
		}
	} else if filepath.Clean(checkout) != filepath.Clean(t.Worktree.Path) {
		return
	}
	_ = r.Herdr.WorkspaceClose(ctx, id)
}

// removeEmptyRepoFolder removes the folder holding a removed worktree when
// it follows herdr's layout, worktrees/<repo>/<branch>, and is empty. Any
// other folder is left alone, and os.Remove never removes one with content.
func removeEmptyRepoFolder(path string) {
	if isDir(path) {
		return
	}
	folder := filepath.Dir(path)
	if filepath.Base(filepath.Dir(folder)) != "worktrees" {
		return
	}
	_ = os.Remove(folder)
}
