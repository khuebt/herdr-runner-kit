package runner

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// removeWorktree removes the task's worktree. herdr removes it, closing its
// workspace, only while that workspace is still open on this worktree: herdr
// reuses the ids of closed workspaces, so an old id may name another
// checkout. Otherwise git removes it and an open workspace stays. Without
// force, both refuse a worktree with changes or untracked files.
func (r *Runner) removeWorktree(ctx context.Context, t *task.Task, force bool) error {
	if t.Worktree == nil || t.Worktree.Path == "" {
		return nil // never created
	}
	path := filepath.Clean(t.Worktree.Path)
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
