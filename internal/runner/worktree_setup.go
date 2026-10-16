package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/redact"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// worktreeSetupTimeout caps a worktree's whole setup step, the wait for a
// heavy command slot included. A variable so tests can shorten it.
var worktreeSetupTimeout = 15 * time.Minute

// setupLog is the name of the setup step's log in the task's out folder.
const setupLog = "worktree-setup.log"

// setupResult is the outcome of a worktree setup step.
type setupResult struct {
	Steps  int // commands that succeeded
	Took   time.Duration
	Failed string // the command that failed; empty when every one succeeded
	Reason string // why it failed, such as "exit 1"
	Log    string
}

// summary is the part of the started line a successful setup adds.
func (s setupResult) summary() string {
	steps := "steps"
	if s.Steps == 1 {
		steps = "step"
	}
	return fmt.Sprintf("setup: %d %s, %s", s.Steps, steps, s.Took.Round(time.Second))
}

// note is the first-prompt note of a failed setup.
func (s setupResult) note() string {
	return fmt.Sprintf("The worktree setup step %q failed (%s); log: %s. Fix the setup before running checks, or report needs-human.",
		redact.Secrets(s.Failed), s.Reason, s.Log)
}

// setUpWorktree runs the config's setup commands for t's repository in t's
// new worktree, once, before its agent starts. It returns nil when there
// is nothing to run. A failure is recorded as an event and returned for
// the first-prompt note; it never fails the launch, since the runner can
// often fix it.
func (r *Runner) setUpWorktree(ctx context.Context, t *task.Task) (*task.Task, *setupResult, error) {
	if !t.UseWorktree || t.Worktree == nil || t.Worktree.Path == "" {
		return t, nil, nil
	}
	cmds := r.Config.SetupFor(t.RepoRoot)
	if len(cmds) == 0 {
		return t, nil, nil
	}
	// A long setup must not read as a dead launcher.
	t, err := r.Store.Update(t.ID, func(t *task.Task) error {
		t.LaunchDeadline = time.Now().UTC().Add(worktreeSetupTimeout + LaunchBudget)
		return nil
	})
	if err != nil {
		return t, nil, err
	}
	res := r.runWorktreeSetup(ctx, t, cmds)
	if t, err = r.Store.Update(t.ID, func(t *task.Task) error {
		t.LaunchDeadline = time.Now().UTC().Add(LaunchBudget)
		return nil
	}); err != nil {
		return t, nil, err
	}
	if res.Failed != "" {
		r.emit(t, "worktree-setup-failed", fmt.Sprintf("setup step %q failed (%s); log: %s", redact.Secrets(res.Failed), res.Reason, res.Log), "")
	}
	return t, &res, nil
}

// runWorktreeSetup runs cmds in turn with sh -c in t's worktree, holding one
// heavy command slot, and logs their output to the task's out folder. The
// first command that fails stops the step.
func (r *Runner) runWorktreeSetup(ctx context.Context, t *task.Task, cmds []string) setupResult {
	start := time.Now()
	dir := r.OutDir(t.ID)
	res := setupResult{Log: filepath.Join(dir, setupLog)}
	done := func(failed, reason string) setupResult {
		res.Failed, res.Reason, res.Took = failed, reason, time.Since(start)
		return res
	}
	ctx, cancel := context.WithTimeout(ctx, worktreeSetupTimeout)
	defer cancel()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return done(cmds[0], "create the log folder: "+err.Error())
	}
	log, err := os.OpenFile(res.Log, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return done(cmds[0], "open the log: "+err.Error())
	}
	defer func() { _ = log.Close() }()

	release, _, err := r.Store.AcquireHeavy(ctx, r.Config.HeavySlots,
		state.HeavyRecord{Task: t.ID, Cwd: t.Worktree.Path, Command: "worktree setup"}, nil)
	if err != nil {
		if ctx.Err() != nil {
			return done(cmds[0], fmt.Sprintf("no heavy command slot came free within %s", worktreeSetupTimeout))
		}
		return done(cmds[0], err.Error())
	}
	defer release()

	env := append(os.Environ(), r.runnerEnv(t)...)
	env = append(env, "HRK_REPO_ROOT="+t.RepoRoot, "HRK_WORKTREE="+t.Worktree.Path)
	for _, c := range cmds {
		fmt.Fprintf(log, "$ %s\n", redact.Secrets(c))
		if reason := runSetupCommand(ctx, c, t.Worktree.Path, env, log); reason != "" {
			fmt.Fprintf(log, "failed: %s\n", reason)
			return done(c, reason)
		}
		res.Steps++
	}
	return done("", "")
}

// runSetupCommand runs c with sh -c in dir, its output going to out, and
// returns why it failed, or "" on success. When ctx ends, its whole process
// group gets SIGTERM, and SIGKILL a few seconds later.
func runSetupCommand(ctx context.Context, c, dir string, env []string, out *os.File) string {
	cmd := exec.CommandContext(ctx, "sh", "-c", c)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if ctx.Err() != nil && cmd.Process != nil {
		_ = killGroup(cmd.Process.Pid, syscall.SIGKILL) // the group may be gone already
	}
	var exit *exec.ExitError
	switch {
	case err == nil:
		return ""
	case ctx.Err() != nil:
		return fmt.Sprintf("timed out after %s", worktreeSetupTimeout)
	case errors.As(err, &exit):
		return fmt.Sprintf("exit %d", exit.ExitCode())
	}
	return err.Error()
}

// killGroup sends sig to the process group led by pid. A group that is
// already gone is not an error.
func killGroup(pid int, sig syscall.Signal) error {
	if err := syscall.Kill(-pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
