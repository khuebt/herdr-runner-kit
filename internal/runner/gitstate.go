package runner

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// gitStateTimeout caps the git snapshot a report takes.
const gitStateTimeout = 2 * time.Second

// gitWaitDelay is how long a git command may hold its output open after it
// exits or is cancelled, so a child it left behind cannot block the caller.
const gitWaitDelay = 500 * time.Millisecond

// gitState snapshots the checkout t works in: its own worktree, or else the
// repository it was spawned in. It returns nil outside git or when git
// fails or takes longer than gitStateTimeout; parts it cannot read stay
// empty. It only runs local git, never a fetch.
func gitState(t *task.Task) *task.GitState {
	g := &task.GitState{}
	dir := t.Cwd
	switch {
	case t.Worktree != nil && t.Worktree.Path != "" && !t.Worktree.Removed:
		dir, g.Worktree = t.Worktree.Path, true
	case t.RepoRoot == "":
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitStateTimeout)
	defer cancel()
	s, err := readStatus(ctx, dir)
	if err != nil {
		return nil
	}
	g.Branch, g.Upstream, g.Ahead, g.Behind = s.Head, s.Upstream, s.Ahead, s.Behind
	g.Uncommitted, g.Untracked = len(s.Changed), len(s.Untracked)
	if g.Base = defaultBranch(ctx, dir); g.Base != "" {
		n, err := strconv.Atoi(gitOut(ctx, dir, "rev-list", "--count", g.Base+"..HEAD"))
		if err != nil {
			g.Base = "" // unknown, so Short never claims there are no new commits
		}
		g.AheadOfBase = n
	}
	g.CheckedAt = time.Now().UTC()
	return g
}

// gitStatus is one read of a checkout's branch and working tree.
type gitStatus struct {
	// Head is the checked-out branch, or "(detached)".
	Head string
	// Upstream is empty when the branch tracks nothing; Ahead and Behind
	// count commits against it.
	Upstream      string
	Ahead, Behind int
	// Changed lists tracked paths with staged, unstaged or conflicted changes.
	Changed []string
	// Untracked lists every untracked file, not just their directories.
	Untracked []string
	// Ignored counts ignored paths; an ignored directory counts once.
	Ignored int
}

// readStatus reads dir's git status without taking the index lock, so it
// never blocks or disturbs a runner working in the checkout.
func readStatus(ctx context.Context, dir string) (gitStatus, error) {
	cmd := exec.CommandContext(ctx, "git", "--no-optional-locks", "-C", dir, "status",
		"--porcelain=v2", "-z", "--branch", "--untracked-files=all", "--ignored=matching")
	cmd.WaitDelay = gitWaitDelay
	out, err := cmd.Output()
	if err != nil {
		return gitStatus{}, fmt.Errorf("git status in %s: %w", dir, err)
	}
	return parseStatus(string(out))
}

// parseStatus parses `git status --porcelain=v2 -z --branch` output. With
// -z paths are never quoted, and a rename's original path is the next field.
func parseStatus(out string) (gitStatus, error) {
	var s gitStatus
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		switch {
		case f == "":
		case strings.HasPrefix(f, "# "):
			if err := s.header(strings.TrimPrefix(f, "# ")); err != nil {
				return gitStatus{}, err
			}
		case strings.HasPrefix(f, "1 "):
			s.Changed = append(s.Changed, entryPath(f, 8))
		case strings.HasPrefix(f, "2 "):
			s.Changed = append(s.Changed, entryPath(f, 9))
			i++ // the original path of the rename or copy
		case strings.HasPrefix(f, "u "):
			s.Changed = append(s.Changed, entryPath(f, 10))
		case strings.HasPrefix(f, "? "):
			s.Untracked = append(s.Untracked, f[2:])
		case strings.HasPrefix(f, "! "):
			s.Ignored++
		default:
			return gitStatus{}, fmt.Errorf("unexpected git status entry %q", f)
		}
	}
	return s, nil
}

func (s *gitStatus) header(h string) error {
	key, val, _ := strings.Cut(h, " ")
	switch key {
	case "branch.head":
		s.Head = val
	case "branch.upstream":
		s.Upstream = val
	case "branch.ab":
		if n, err := fmt.Sscanf(val, "+%d -%d", &s.Ahead, &s.Behind); n != 2 || err != nil {
			return fmt.Errorf("unexpected git status header %q", h)
		}
	}
	return nil
}

// entryPath returns the path of a changed entry: everything after its first
// n space-separated fields, since the path itself may contain spaces.
func entryPath(entry string, n int) string {
	parts := strings.SplitN(entry, " ", n+1)
	return parts[len(parts)-1]
}
