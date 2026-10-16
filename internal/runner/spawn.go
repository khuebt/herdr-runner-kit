// Package runner creates, launches and controls runners: one agent in one
// herdr tab working on one task.
package runner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/khuebt/herdr-runner-kit/internal/config"
	"github.com/khuebt/herdr-runner-kit/internal/harness"
	"github.com/khuebt/herdr-runner-kit/internal/herdr"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
	"github.com/khuebt/herdr-runner-kit/internal/telegram"
)

// LaunchBudget is how long a launch may take before the backstop treats a
// task stuck in "starting" as failed.
const LaunchBudget = 2 * time.Minute

// maxHoldReason bounds a hold reason, which shows in news lines.
const maxHoldReason = 200

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
	// ProcTable lists the machine's processes, so a close can name the
	// ones a runner left running. Nil skips that check.
	ProcTable func(context.Context) ([]proc, error)
	// ProcEnv returns a process's environment as NUL-separated KEY=VALUE
	// entries, so a close finds the processes a runner started that left
	// its process tree. Nil, or an error, leaves only the tree.
	ProcEnv func(pid int) ([]byte, error)
	// EnterScope moves agent, and the processes children lists, into a new
	// memory-capped systemd scope named unit. Nil starts agents where herdr
	// puts them.
	EnterScope func(ctx context.Context, unit string, agent int, children func() []int) (Scope, error)
}

// New returns a Runner for the real environment.
func New(store *state.Store, cfg config.Config, bin string) *Runner {
	r := &Runner{
		Store: store, Config: cfg, Herdr: herdr.New(), Telegram: telegram.New(),
		Bin: bin, Getenv: os.Getenv, Sleep: time.Sleep, ProcTable: psTable, ProcEnv: procEnviron,
		EnterScope: SystemScope(cfg.RunnerMemoryMax, runtime.GOOS),
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
	// ClaudeConfig is the Claude config folder of a Claude runner, as a path
	// or a bare name such as claude2. Empty means the main session's.
	ClaudeConfig string
	// Branch names the worktree branch, following the repository's own
	// convention. Empty means prefix + "<id>-<slug>".
	Branch string
	// BranchTitle is the text the slug of the default branch comes from.
	// Empty means the title.
	BranchTitle string
	// Language overrides the configured report language.
	Language string
	// Project names the registered project the task works for. Empty means
	// the project whose folder holds Cwd, if any. A project task runs in
	// the project's folder by default and reports to its project owner.
	Project string
	// Stage is the project stage the task runs; it needs a project.
	Stage string
	// Keep holds the runner's tab open after the task finishes, so gc
	// leaves it.
	Keep bool
	// Reason says why a task with the human gate waits, such as the
	// dependency it needs. Only a release that names the task opens it.
	Reason string
	// Plan is the absolute folder of the AgentKit plan the task runs a
	// phase of, and Phase that phase's number.
	Plan  string
	Phase int
	// CheckoutOf is the plan task whose worktree the task works in. It
	// implies no worktree of the task's own.
	CheckoutOf string
	// Warn, when set, receives a one-line warning that spawn carried on
	// without the project registry because it could not be read.
	Warn func(string)
}

// Spawn records a task and, unless it waits on other tasks or on a human,
// starts a detached launcher. It returns quickly: the agent starts in the
// background and reports its progress as events.
func (r *Runner) Spawn(ctx context.Context, o SpawnOptions) (*task.Task, error) {
	if r.Getenv("HRK_TASK") != "" {
		return nil, ErrInsideRunner
	}
	t, caller, err := r.newTask(ctx, o)
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
	// The owner's first wait must see this task's events. A project task's
	// news reaches the caller's wait too, so its cursor starts here as well.
	for _, owner := range []string{t.Owner, caller} {
		if err := r.Store.InitCursor(state.CursorName(owner)); err != nil {
			return nil, err
		}
	}
	if err := r.Store.Create(t); err != nil {
		return nil, err
	}
	if t.UseWorktree {
		branch := o.Branch
		if branch == "" {
			branch = BranchName(r.Config.WorktreeBranchPrefix, t.ID, cmp.Or(o.BranchTitle, t.Title))
		}
		if BranchExists(ctx, t.RepoRoot, branch) {
			r.fail(t.ID, fmt.Sprintf("branch %s already exists", branch))
			return nil, fmt.Errorf("branch %s already exists in %s", branch, t.RepoRoot)
		}
		if t, err = r.Store.Update(t.ID, func(t *task.Task) error {
			t.Worktree = &task.Worktree{Branch: branch, Base: base, BranchCreated: new(false)}
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
			r.emit(t, "held", task.HeldSummary(t), "")
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

// newTask builds the task from the options, and returns it with the owner
// of the main session that called spawn.
func (r *Runner) newTask(ctx context.Context, o SpawnOptions) (*task.Task, string, error) {
	if strings.TrimSpace(o.Prompt) == "" && strings.TrimSpace(o.Skill) == "" {
		return nil, "", errors.New("a prompt or --skill is required")
	}
	kindName := o.Kind
	if kindName == "" {
		kindName = r.Config.DefaultKind
	}
	kind, ok := r.Config.Kinds[kindName]
	if !ok {
		return nil, "", fmt.Errorf("unknown kind %q", kindName)
	}
	agent := kind.Agent
	if o.Agent != "" {
		agent = o.Agent
	}
	spec, err := task.ParseAgentSpec(agent)
	if err != nil {
		return nil, "", err
	}
	// The main session may run with CLAUDE_CONFIG_DIR=~/.claude; store it as
	// Claude's default, as --claude-config does.
	configDir := harness.DefaultClaudeConfigDir(r.Getenv("CLAUDE_CONFIG_DIR"))
	if o.ClaudeConfig != "" {
		if spec.Harness != task.Claude {
			return nil, "", fmt.Errorf("--claude-config needs a Claude runner, not %s", spec.Harness)
		}
		if configDir, err = ClaudeConfigDir(o.ClaudeConfig); err != nil {
			return nil, "", err
		}
	}
	gate := o.Gate
	if gate == "" {
		gate = task.GateAuto
	}
	if !gate.Valid() {
		return nil, "", fmt.Errorf("invalid gate %q: want auto or human", gate)
	}
	reason := strings.Join(strings.Fields(o.Reason), " ")
	if reason != "" && gate != task.GateHuman {
		return nil, "", errors.New("--reason goes only with --gate human")
	}
	if n := utf8.RuneCountInString(reason); n > maxHoldReason {
		return nil, "", fmt.Errorf("--reason has %d characters; keep it under %d", n, maxHoldReason+1)
	}
	workspace := r.Getenv("HERDR_WORKSPACE_ID")
	if r.Getenv("HERDR_ENV") != "1" || workspace == "" {
		return nil, "", errors.New("spawn must run inside a herdr pane, so runners can open next to it")
	}

	wd, err := os.Getwd()
	if err != nil {
		return nil, "", err
	}
	var project state.Project
	if o.Project != "" {
		if project, err = r.Store.ProjectByName(o.Project); err != nil {
			return nil, "", err
		}
		if project.Reserved {
			return nil, "", fmt.Errorf("project %s is still being set up; if hrk new was interrupted, drop it with: hrk project forget %s, then run new again", project.Name, project.Name)
		}
	}
	cwd := o.Cwd
	switch {
	case cwd != "":
	case project.Path != "":
		cwd = project.Path
	default:
		cwd = wd
	}
	if cwd, err = filepath.Abs(cwd); err != nil {
		return nil, "", err
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return nil, "", fmt.Errorf("cwd %s is not a directory", cwd)
	}
	root := RepoRoot(ctx, cwd)
	// A linked worktree belongs to the project of its main repository.
	home := cwd
	if root != "" {
		home = root
	}
	found, ok, err := r.Store.ProjectFor(home)
	switch {
	case err != nil && project.Name != "":
		return nil, "", err
	case err != nil:
		// Only project tasks need the registry; any other task starts as if
		// no project were registered.
		if o.Warn != nil {
			o.Warn(fmt.Sprintf("warning: %v; spawning as if no project were registered", err))
		}
		ok = false
	}
	switch {
	case project.Name == "" && ok:
		project = found
	case project.Name != "" && (!ok || found.Name != project.Name):
		return nil, "", fmt.Errorf("cwd %s is outside project %s (%s)", cwd, project.Name, project.Path)
	}
	stage := strings.TrimSpace(o.Stage)
	if stage != "" && project.Name == "" {
		return nil, "", errors.New("--stage needs a project task")
	}
	useWorktree := kind.Worktree && root != ""
	if o.Worktree != nil {
		useWorktree = *o.Worktree
	}
	if o.CheckoutOf != "" {
		if o.Base != "" || o.Branch != "" || (o.Worktree != nil && *o.Worktree) {
			return nil, "", errors.New("a task working in another task's checkout takes no worktree, --base or --branch")
		}
		if _, err := r.Store.Get(o.CheckoutOf); err != nil {
			return nil, "", fmt.Errorf("checkout of %s: %w", o.CheckoutOf, err)
		}
		useWorktree = false
	}
	if useWorktree && root == "" {
		return nil, "", fmt.Errorf("--worktree needs a git repository, and %s is not in one", cwd)
	}
	if o.Base != "" && !useWorktree {
		return nil, "", errors.New("--base needs a worktree task")
	}
	if o.Branch != "" {
		if !useWorktree {
			return nil, "", errors.New("--branch needs a worktree task")
		}
		if exec.CommandContext(ctx, "git", "check-ref-format", "--branch", o.Branch).Run() != nil {
			return nil, "", fmt.Errorf("invalid branch name %q", o.Branch)
		}
	}
	language := r.Config.Language
	if l := strings.TrimSpace(o.Language); l != "" {
		language = l
	}
	if err := config.CheckLanguage(language); err != nil {
		return nil, "", fmt.Errorf("--language: %w", err)
	}
	caller := CallerOwner(ctx, wd)
	owner := o.Owner
	switch {
	case owner != "":
	case project.Name != "":
		owner = task.ProjectOwner(project.Name)
	default:
		owner = caller
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
			return nil, "", err
		}
		after = append(after, id)
	}
	if o.Owner != "" {
		caller = o.Owner
	}
	return &task.Task{
		Kind: kindName, Agent: spec.String(), Prompt: o.Prompt, Skill: o.Skill, Title: title,
		Cwd: cwd, Owner: owner, RepoRoot: root, UseWorktree: useWorktree, After: after, Gate: gate, HoldReason: reason,
		MainWorkspace: workspace, ClaudeConfigDir: configDir, Language: language,
		Project: project.Name, Stage: stage, Keep: o.Keep,
		Plan: o.Plan, Phase: o.Phase, CheckoutOf: o.CheckoutOf,
		InputLocked: r.Config.RunnerInput == config.InputLocked,
		// Queued until spawn decides: a task with no pending parents moves on
		// to held or starting at once.
		Status: task.Queued,
	}, caller, nil
}

// ClaudeConfigDir resolves a --claude-config value: a path, or a bare name
// such as claude2 for ~/.claude2. The folder must exist. ~/.claude resolves to
// "": Claude keeps that default account in ~/.claude.json, and setting
// CLAUDE_CONFIG_DIR=~/.claude would point it at ~/.claude/.claude.json.
func ClaudeConfigDir(s string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch {
	case strings.HasPrefix(s, "~/"):
		s = filepath.Join(home, s[2:])
	case !strings.ContainsRune(s, filepath.Separator):
		s = filepath.Join(home, "."+strings.TrimPrefix(s, "."))
	}
	dir, err := filepath.Abs(s)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("claude config %s is not a directory", dir)
	}
	return harness.DefaultClaudeConfigDir(dir), nil
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
	ev := state.Event{Task: t.ID, Owner: t.Owner, Type: typ, Status: t.Status, Summary: summary, Question: question, Attempt: eventAttempt(t)}
	if err := r.record(ev); err != nil {
		fmt.Fprintf(os.Stderr, "hrk: %s: write event: %v\n", t.ID, err)
	}
}

// fail marks a task failed with reason and emits the failed event. It
// returns the failed task, and false when the task was already failed or
// could not be read.
func (r *Runner) fail(id, reason string) (*task.Task, bool) {
	t, changed, err := r.Store.Transition(id, task.Failed, nil, func(t *task.Task) { t.Error = reason })
	if err != nil || !changed {
		return nil, false
	}
	r.emit(t, "failed", reason, "")
	return t, true
}

// CallerOwner names the main session working in dir: its git top-level, or
// dir itself outside a repository.
func CallerOwner(ctx context.Context, dir string) string {
	if top := Toplevel(ctx, dir); top != "" {
		return top
	}
	return dir
}
