// Package runner creates, launches and controls runners: one agent in one
// herdr tab working on one task.
package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/config"
	"github.com/khuebt/herdr-runner-kit/internal/herdr"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
	"github.com/khuebt/herdr-runner-kit/internal/telegram"
)

// LaunchBudget is how long a launch may take before the backstop treats a
// task stuck in "starting" as failed.
const LaunchBudget = 2 * time.Minute

// ErrInsideRunner is returned when spawn runs inside a runner.
var ErrInsideRunner = errors.New("runners cannot spawn runners")

// Runner holds what every runner command needs.
type Runner struct {
	Store  *state.Store
	Config config.Config
	Herdr  *herdr.Client
	// Bin is the absolute path of this binary, used by hooks and the
	// report contract.
	Bin string
	// Getenv reads the caller's environment.
	Getenv func(string) string
	// StartLauncher starts the detached launcher for a task and returns its
	// pid.
	StartLauncher func(id string) (int, error)
	// StartAfter starts the detached follow-up of a report.
	StartAfter func(id string) error
	// Notify hands a news event to the detached Telegram notifier.
	Notify func(ev state.Event)
	// Telegram sends notifications in the notifier process.
	Telegram *telegram.Client
	// Sleep pauses between polls; tests replace it.
	Sleep func(time.Duration)
}

// New returns a Runner for the real environment.
func New(store *state.Store, cfg config.Config, bin string) *Runner {
	r := &Runner{
		Store: store, Config: cfg, Herdr: herdr.New(), Telegram: telegram.New(),
		Bin: bin, Getenv: os.Getenv, Sleep: time.Sleep,
	}
	r.Notify = r.startDetachedNotify
	r.StartLauncher = func(id string) (int, error) { return r.startDetached(id, "__launch", "launch.log") }
	r.StartAfter = func(id string) error {
		_, err := r.startDetached(id, "__after", "after.log")
		return err
	}
	return r
}

// SpawnOptions are the spawn command's inputs. Empty fields take the kind's
// defaults.
type SpawnOptions struct {
	Kind     string
	Agent    string
	Worktree *bool
	Base     string
	Skill    string
	After    []string
	Gate     task.Gate
	Cwd      string
	Title    string
	Owner    string
	Prompt   string
}

// Spawn records a task and, unless it waits on other tasks or on a human,
// starts a detached launcher. It returns quickly: the agent starts in the
// background and reports its progress as events.
func (r *Runner) Spawn(ctx context.Context, o SpawnOptions) (*task.Task, error) {
	if r.Getenv("HRK_TASK") != "" {
		return nil, ErrInsideRunner
	}
	t, err := r.newTask(ctx, o)
	if err != nil {
		return nil, err
	}
	pending, err := r.pendingParents(t.After)
	if err != nil {
		return nil, err
	}
	base := o.Base
	if len(t.After) > 0 && len(pending) == 0 {
		var parentBase string
		if t.Inputs, parentBase, err = r.parentInputs(t.After); err != nil {
			return nil, err
		}
		if base == "" {
			base = parentBase
		}
	}
	// The owner's first wait must see this task's events.
	if err := r.Store.InitCursor(state.CursorName(t.Owner)); err != nil {
		return nil, err
	}
	if err := r.Store.Create(t); err != nil {
		return nil, err
	}
	if t.UseWorktree {
		branch := BranchName(r.Config.WorktreeBranchPrefix, t.ID, t.Title)
		if BranchExists(ctx, t.RepoRoot, branch) {
			r.fail(t.ID, fmt.Sprintf("branch %s already exists", branch))
			return nil, fmt.Errorf("branch %s already exists in %s", branch, t.RepoRoot)
		}
		if t, err = r.Store.Update(t.ID, func(t *task.Task) error {
			t.Worktree = &task.Worktree{Branch: branch, Base: base}
			return nil
		}); err != nil {
			return nil, err
		}
	}

	switch {
	case len(pending) > 0:
		r.emit(t, "queued", "waits for "+strings.Join(pending, ", "), "")
		return t, nil
	case t.Gate == task.GateHuman:
		t, _, err = r.Store.SetStatus(t.ID, task.Held)
		if err == nil {
			r.emit(t, "held", "held for release: "+t.Title, "")
		}
		return t, err
	}
	return r.StartLaunch(t.ID)
}

// StartLaunch moves a task to "starting" and hands it to a detached launcher.
func (r *Runner) StartLaunch(id string) (*task.Task, error) {
	t, changed, err := r.beginLaunch(id, nil)
	if err != nil {
		return nil, err
	}
	if !changed {
		return t, fmt.Errorf("%s is %s, not ready to launch", id, t.Status)
	}
	pid, err := r.StartLauncher(id)
	if err != nil {
		r.fail(id, "start launcher: "+err.Error())
		return nil, fmt.Errorf("start launcher: %w", err)
	}
	return r.Store.Update(id, func(t *task.Task) error {
		t.LauncherPID = pid
		return nil
	})
}

// beginLaunch moves a queued or held task to starting, applying mutate in
// the same write, and emits spawned. changed is false when another process
// already moved it.
func (r *Runner) beginLaunch(id string, mutate func(*task.Task)) (*task.Task, bool, error) {
	t, changed, err := r.Store.Transition(id, task.Starting, []task.Status{task.Queued, task.Held}, func(t *task.Task) {
		t.LaunchDeadline = time.Now().UTC().Add(LaunchBudget)
		if mutate != nil {
			mutate(t)
		}
	})
	if err == nil && changed {
		r.emit(t, "spawned", t.Title, "")
	}
	return t, changed, err
}

func (r *Runner) newTask(ctx context.Context, o SpawnOptions) (*task.Task, error) {
	if strings.TrimSpace(o.Prompt) == "" && strings.TrimSpace(o.Skill) == "" {
		return nil, errors.New("a prompt or --skill is required")
	}
	kindName := o.Kind
	if kindName == "" {
		kindName = r.Config.DefaultKind
	}
	kind, ok := r.Config.Kinds[kindName]
	if !ok {
		return nil, fmt.Errorf("unknown kind %q", kindName)
	}
	agent := kind.Agent
	if o.Agent != "" {
		agent = o.Agent
	}
	spec, err := task.ParseAgentSpec(agent)
	if err != nil {
		return nil, err
	}
	gate := o.Gate
	if gate == "" {
		gate = task.GateAuto
	}
	if !gate.Valid() {
		return nil, fmt.Errorf("invalid gate %q: want auto or human", gate)
	}
	workspace := r.Getenv("HERDR_WORKSPACE_ID")
	if r.Getenv("HERDR_ENV") != "1" || workspace == "" {
		return nil, errors.New("spawn must run inside a herdr pane, so runners can open next to it")
	}

	cwd := o.Cwd
	if cwd == "" {
		if cwd, err = os.Getwd(); err != nil {
			return nil, err
		}
	}
	if cwd, err = filepath.Abs(cwd); err != nil {
		return nil, err
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("cwd %s is not a directory", cwd)
	}
	root := RepoRoot(ctx, cwd)
	useWorktree := kind.Worktree && root != ""
	if o.Worktree != nil {
		useWorktree = *o.Worktree
	}
	if useWorktree && root == "" {
		return nil, fmt.Errorf("--worktree needs a git repository, and %s is not in one", cwd)
	}
	if o.Base != "" && !useWorktree {
		return nil, errors.New("--base needs a worktree task")
	}
	owner := o.Owner
	if owner == "" {
		if owner = Toplevel(ctx, cwd); owner == "" {
			owner = cwd
		}
	}
	title := strings.TrimSpace(o.Title)
	if title == "" {
		title = TitleFrom(o.Prompt)
	}
	if title == "" {
		title = strings.TrimLeft(o.Skill, "/$")
	}
	after := make([]string, 0, len(o.After))
	for _, a := range o.After {
		id, _, err := task.ParseID(a)
		if err != nil {
			return nil, err
		}
		after = append(after, id)
	}
	return &task.Task{
		Kind: kindName, Agent: spec.String(), Prompt: o.Prompt, Skill: o.Skill, Title: title,
		Cwd: cwd, Owner: owner, RepoRoot: root, UseWorktree: useWorktree, After: after, Gate: gate,
		MainWorkspace: workspace, ClaudeConfigDir: r.Getenv("CLAUDE_CONFIG_DIR"),
		// Queued until spawn decides: a task with no pending parents moves on
		// to held or starting at once.
		Status: task.Queued,
	}, nil
}

// pendingParents returns the --after tasks that are not done yet. A parent
// that failed or was stopped can never unblock its child, so it is an error.
func (r *Runner) pendingParents(after []string) ([]string, error) {
	var pending []string
	for _, id := range after {
		p, err := r.Store.Get(id)
		if err != nil {
			return nil, err
		}
		switch p.Status {
		case task.Done:
		case task.Failed, task.Stopped:
			return nil, fmt.Errorf("--after %s: that task is %s", id, p.Status)
		default:
			pending = append(pending, id)
		}
	}
	return pending, nil
}

// startDetached runs `<bin> <verb> <id>` in its own session with every
// stream redirected to the task's log, so neither the command nor its caller
// waits for it.
func (r *Runner) startDetached(id, verb, logName string) (int, error) {
	logPath := filepath.Join(r.Store.TaskDir(id), logName)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer func() { _ = logFile.Close() }()
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return 0, err
	}
	defer func() { _ = devNull.Close() }()
	cmd := exec.Command(r.Bin, verb, id)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

// emit appends an event for t. Event loss is reported but never fatal,
// because the task file already holds the state.
func (r *Runner) emit(t *task.Task, typ, summary, question string) {
	ev := state.Event{Task: t.ID, Owner: t.Owner, Type: typ, Status: t.Status, Summary: summary, Question: question}
	if err := r.record(ev); err != nil {
		fmt.Fprintf(os.Stderr, "herdr-runner-kit: %s: write event: %v\n", t.ID, err)
	}
}

// fail marks a task failed with reason and emits the failed event.
func (r *Runner) fail(id, reason string) {
	t, changed, err := r.Store.Transition(id, task.Failed, nil, func(t *task.Task) { t.Error = reason })
	if err != nil || !changed {
		return
	}
	r.emit(t, "failed", reason, "")
}
