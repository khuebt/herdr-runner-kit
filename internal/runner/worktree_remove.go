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
// checkout. Otherwise git removes it and an open workspace stays. Without
// force, both refuse a worktree with changes or untracked files. Once the
// worktree is gone, its repository folder in herdr's worktree root goes too
// when no other worktree is left in it.
func (r *Runner) removeWorktree(ctx context.Context, t *task.Task, force bool) error {
	if t.Worktree == nil || t.Worktree.Path == "" {
		return nil // never created
	}
	path := filepath.Clean(t.Worktree.Path)
	if err := r.removeCheckout(ctx, t, path, force); err != nil {
		return err
	}
	removeEmptyRepoFolder(path)
	return nil
}

func (r *Runner) removeCheckout(ctx context.Context, t *task.Task, path string, force bool) error {
	if id := t.Herdr.WorkspaceID; id != "" {
		if checkout, err := r.Herdr.WorkspaceCheckout(ctx, id); err == nil && checkout != "" && filepath.Clean(checkout) == path {
			return r.Herdr.WorktreeRemove(ctx, id, force)
		}
	}
	if !isDir(path) {
		return nil // already gone
	}
	args := []string{"-C", t.RepoRoot, "worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	if out, err := exec.CommandContext(ctx, "git", append(args, path)...).CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree remove: %s", strings.TrimSpace(string(out)))
	}
	return nil
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
