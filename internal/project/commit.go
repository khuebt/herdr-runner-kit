package project

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/fsutil"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// Commit tuning.
var (
	// CommitTimeout bounds one git commit, hooks included.
	CommitTimeout = 30 * time.Second
	// indexLockRetries is how often a commit is retried while another git
	// process holds the index lock.
	indexLockRetries = 5
	indexLockPause   = 300 * time.Millisecond
)

// Commit renders the project's record into its repository and commits the
// rendered files, and nothing else, as one commit. paths picks some of
// RenderedPaths; none means all of them. It returns the new commit, or ""
// when the files were already up to date.
//
// It runs only at commit points: gate outcomes, a milestone done, new and
// graduation. Other staged changes stay staged and out of the commit. A
// failing hook leaves the rendered files written, and the error holds the
// hook's first line and the command to commit them by hand.
func Commit(ctx context.Context, s *state.Store, name, message string, paths ...string) (string, error) {
	if len(paths) == 0 {
		paths = RenderedPaths
	}
	for _, p := range paths {
		if !slices.Contains(RenderedPaths, p) {
			return "", fmt.Errorf("%s is not a file the kit renders", p)
		}
	}
	if strings.TrimSpace(message) == "" {
		return "", errors.New("a commit message is required")
	}
	p, err := s.ProjectByName(name)
	if err != nil {
		return "", err
	}
	unlock, err := s.LockProject(name, "checkout")
	if err != nil {
		return "", err
	}
	defer unlock()
	if err := checkCheckout(ctx, s, p); err != nil {
		return "", err
	}
	rec, err := Load(s, name)
	if err != nil {
		return "", err
	}
	for _, rel := range paths {
		data, err := Render(rec, rel)
		if err != nil {
			return "", err
		}
		full := filepath.Join(p.Path, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "", fmt.Errorf("create %s: %w", filepath.Dir(rel), err)
		}
		if err := fsutil.WriteFileAtomic(full, data, 0o644); err != nil {
			return "", fmt.Errorf("write %s: %w", rel, err)
		}
	}
	if out, err := gitLocked(ctx, p.Path, append([]string{"add", "-A", "--"}, paths...)...); err != nil {
		return "", fmt.Errorf("git add: %s", firstLine(out, err))
	}
	changed, err := staged(ctx, p.Path, paths)
	if err != nil {
		return "", err
	}
	sha := ""
	if changed {
		if sha, err = commitPaths(ctx, p.Path, message, paths); err != nil {
			return "", err
		}
	}
	// The snapshot now holds every change counted when the record was read.
	if _, err := update(s, name, func(r *Record) error {
		r.Pending = max(r.Pending-rec.Pending, 0)
		return nil
	}); err != nil {
		return sha, fmt.Errorf("committed %s, but could not update the record: %w", sha, err)
	}
	return sha, nil
}

// checkCheckout refuses to commit while a merge is in progress or a runner
// of the project works in the main checkout, since either may be editing
// the index the commit uses.
func checkCheckout(ctx context.Context, s *state.Store, p state.Project) error {
	mergeHead, err := git(ctx, p.Path, "rev-parse", "--path-format=absolute", "--git-path", "MERGE_HEAD")
	if err != nil {
		return err
	}
	if _, err := os.Stat(mergeHead); err == nil {
		return fmt.Errorf("%s has a merge in progress; finish or abort it, then retry", p.Path)
	}
	tasks, err := s.List()
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if t.Project == p.Name && !t.UseWorktree && runnerActive(t.Status) {
			return fmt.Errorf("%s (%s) is %s in the main checkout of %s; retry when it has finished", t.ID, t.Title, t.Status, p.Name)
		}
	}
	return nil
}

// runnerActive reports whether a task's runner may be working right now.
func runnerActive(s task.Status) bool {
	return s == task.Starting || s == task.Running || s == task.NeedsHuman
}

// commitPaths commits exactly paths. Hooks run: --no-verify is never
// passed.
func commitPaths(ctx context.Context, root, message string, paths []string) (string, error) {
	args := append([]string{"commit", "--only", "-m", message, "--"}, paths...)
	if out, err := gitLocked(ctx, root, args...); err != nil {
		return "", fmt.Errorf("commit failed: %s\nthe rendered files are written; commit them with:\n  %s",
			firstLine(out, err), shellCommand(append([]string{"git", "-C", root}, args...)))
	}
	return git(ctx, root, "rev-parse", "HEAD")
}

// gitLocked runs a git command that takes the index lock, retrying while
// another git process holds it. Each try may take CommitTimeout. It returns
// the command's combined output.
func gitLocked(ctx context.Context, root string, args ...string) (string, error) {
	for attempt := 0; ; attempt++ {
		cctx, cancel := context.WithTimeout(ctx, CommitTimeout)
		out, err := gitCombined(cctx, root, args...)
		timedOut := errors.Is(cctx.Err(), context.DeadlineExceeded)
		cancel()
		switch {
		case err == nil:
			return out, nil
		case timedOut:
			return fmt.Sprintf("git %s did not finish in %s", args[0], CommitTimeout), err
		case strings.Contains(out, "index.lock") && attempt < indexLockRetries:
		default:
			return out, err
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(indexLockPause):
		}
	}
}

// git runs git in root and returns its trimmed stdout.
func git(ctx context.Context, root string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", args[0], firstLine(stderr.String(), err))
	}
	return strings.TrimSpace(string(out)), nil
}

// staged reports whether the index holds changes to paths.
func staged(ctx context.Context, root string, paths []string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root, "diff", "--cached", "--quiet", "--"}, paths...)...)
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return false, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return true, nil
	}
	return false, fmt.Errorf("git diff: %w", err)
}

func gitCombined(ctx context.Context, root string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func firstLine(out string, err error) string {
	for line := range strings.Lines(out) {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return err.Error()
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./:=@+-]+$`)

// shellCommand quotes args for a POSIX shell.
func shellCommand(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		if shellSafe.MatchString(a) {
			quoted[i] = a
		} else {
			quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(quoted, " ")
}
