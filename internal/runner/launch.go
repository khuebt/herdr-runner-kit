package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/harness"
	"github.com/khuebt/herdr-runner-kit/internal/herdr"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

const (
	agentStartTimeout = 20 * time.Second
	// dialogAnswerTimeout is how long a launcher waits for the user to
	// answer a startup dialog in the runner's tab.
	dialogAnswerTimeout = 30 * time.Minute
	maxDialogRounds     = 4
	// unknownDialogIdleWait is how long a launcher waits for an agent that
	// herdr saw as not ready to turn idle before it calls the screen an
	// unknown startup dialog.
	unknownDialogIdleWait = 5 * time.Second
)

// errHandled means the launch ended in a state that is already recorded.
var errHandled = errors.New("handled")

// Launch opens the runner's herdr location, starts the agent and sends the
// first prompt. Every outcome is recorded as task state and an event, so a
// failure is news, never silence. It runs in the detached launcher, or
// in-process when a chained task starts.
func (r *Runner) Launch(ctx context.Context, id string) error {
	t, err := r.Store.Update(id, func(t *task.Task) error {
		if t.Status != task.Starting {
			return fmt.Errorf("%s is %s, not starting", t.ID, t.Status)
		}
		t.LauncherPID = os.Getpid()
		t.LaunchDeadline = time.Now().UTC().Add(LaunchBudget)
		return nil
	})
	if err != nil {
		return err
	}
	if err := r.launch(ctx, t); err != nil && !errors.Is(err, errHandled) {
		r.fail(id, err.Error())
		return err
	}
	return nil
}

func (r *Runner) launch(ctx context.Context, t *task.Task) error {
	// Check the agent before opening anything.
	if _, err := task.ParseAgentSpec(t.Agent); err != nil {
		return err
	}
	loc := herdr.Location{WorkspaceID: t.Herdr.WorkspaceID, TabID: t.Herdr.TabID, PaneID: t.Herdr.PaneID}
	name := "hrk-" + t.ID
	if len(t.FallbackFrom) > 0 && t.Herdr.PaneID != "" {
		// The agent hit its usage limit: its fallback takes over the same
		// pane and checkout.
		if err := r.clearPane(ctx, t); err != nil {
			return err
		}
	} else {
		var err error
		if loc, err = r.openLocation(ctx, t); err != nil {
			return fmt.Errorf("open runner location: %w", err)
		}
		if t, err = r.Store.Update(t.ID, func(t *task.Task) error {
			t.Herdr = task.Herdr{WorkspaceID: loc.WorkspaceID, TabID: loc.TabID, PaneID: loc.PaneID, AgentName: name}
			if t.Worktree != nil && loc.Path != "" {
				t.Worktree.Path = loc.Path
			}
			return nil
		}); err != nil {
			return err
		}
	}

	spec, adapter, files, err := r.startAgent(ctx, t, name, loc.PaneID)
	for errors.Is(err, errUsageLimit) {
		// errUsageLimit comes only while a fallback is left, and each switch
		// records the agent it leaves, so this ends once they run out.
		if t, err = r.claimSwitch(t, []task.Status{task.Starting}, false, false); err != nil {
			return err
		}
		if err := r.clearPane(ctx, t); err != nil {
			return err
		}
		spec, adapter, files, err = r.startAgent(ctx, t, name, loc.PaneID)
	}
	if err != nil {
		return err
	}

	// Address the agent by pane id from here on: herdr drops the name when
	// a start times out.
	prompt := t.Prompt
	if t.Inputs != "" {
		prompt = strings.TrimSpace(prompt + "\n\n" + t.Inputs)
	}
	if len(t.FallbackFrom) > 0 {
		prompt = strings.TrimSpace(prompt + "\n\n" + resumeNote)
	}
	text := adapter.FirstPrompt(t.Skill, prompt, files.contractText)
	// A dialog can open after the screen looked ready. Look once more, so
	// the prompt and its Enter never land in one. An agent herdr sees as
	// blocked shows a dialog even when no known one matches.
	a, err := r.Herdr.AgentGet(ctx, loc.PaneID)
	if err := r.settleDialogs(ctx, t, adapter, loc.PaneID, err == nil && a.Status == "blocked"); err != nil {
		return err
	}
	if err := r.Herdr.AgentPrompt(ctx, loc.PaneID, text); err != nil {
		return fmt.Errorf("send first prompt: %w", err)
	}
	t, changed, err := r.Store.SetStatus(t.ID, task.Running, task.Starting, task.NeedsHuman)
	if err != nil {
		return err
	}
	if changed {
		r.emit(t, "started", startedLine(t, spec), "")
	}
	if _, err := r.Herdr.AgentWait(ctx, loc.PaneID, 5*time.Second, "working"); err != nil {
		// Codex can report a stale state after skipped hooks; never resubmit.
		r.emit(t, "prompt-unconfirmed", "prompt sent, but herdr saw no turn start within 5 s", "")
	}
	r.emit(t, "prompted", "", "")
	return nil
}

// startAgent writes the runner files for t's agent, prepares its trust and
// starts it in pane until it is ready for the first prompt. It returns
// errUsageLimit when the agent shows a usage limit at startup and a
// fallback agent is left; without one the prompt goes in, and the agent's
// own turn-end reports the limit.
func (r *Runner) startAgent(ctx context.Context, t *task.Task, name, pane string) (task.AgentSpec, harness.Adapter, runnerFiles, error) {
	spec, err := task.ParseAgentSpec(t.Agent)
	if err != nil {
		return spec, nil, runnerFiles{}, err
	}
	adapter, err := harness.For(spec.Harness)
	if err != nil {
		return spec, nil, runnerFiles{}, err
	}
	files, err := r.writeRunnerFiles(t, adapter)
	if err != nil {
		return spec, nil, runnerFiles{}, err
	}
	if err := r.prepareTrust(t, spec.Harness); err != nil {
		return spec, nil, runnerFiles{}, err
	}
	args := adapter.LaunchArgs(harness.LaunchOptions{
		Spec: spec, Bin: r.Bin, SettingsPath: files.settings, ContractPath: files.contract,
	})
	_, err = r.Herdr.AgentStart(ctx, name, adapter.HerdrKind(), pane, agentStartTimeout, args)
	if err := r.settleStartup(ctx, t, adapter, pane, err); err != nil {
		return spec, nil, runnerFiles{}, err
	}
	if screen, err := r.Herdr.PaneRead(ctx, pane, 40); err == nil && adapter.ShowsUsageLimit(screen) && r.switchable(t) {
		return spec, nil, runnerFiles{}, errUsageLimit
	}
	return spec, adapter, files, nil
}

type runnerFiles struct {
	settings     string // Claude settings with the Stop hook
	contract     string // contract file for Claude
	contractText string // contract text for Codex's first prompt
}

// writeRunnerFiles writes the per-task hook settings, the contract and the
// output directory.
func (r *Runner) writeRunnerFiles(t *task.Task, adapter harness.Adapter) (runnerFiles, error) {
	dir := r.Store.TaskDir(t.ID)
	outDir := filepath.Join(dir, "out")
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return runnerFiles{}, err
	}
	contract := Contract(t, r.Bin, outDir)
	if adapter.Harness() == task.Codex {
		return runnerFiles{contractText: contract}, nil
	}
	files := runnerFiles{
		settings: filepath.Join(dir, "claude-settings.json"),
		contract: filepath.Join(dir, "contract.md"),
	}
	settings, err := harness.ClaudeSettings(r.Bin)
	if err != nil {
		return runnerFiles{}, err
	}
	if err := os.WriteFile(files.settings, settings, 0o600); err != nil {
		return runnerFiles{}, err
	}
	if err := os.WriteFile(files.contract, []byte(contract+"\n"), 0o600); err != nil {
		return runnerFiles{}, err
	}
	return files, nil
}

// runnerEnv is the environment every runner gets. HRK_STATE_DIR points
// report and the turn-end hook at the spawner's state. TELEGRAM_BOT_TOKEN is
// empty so the Telegram plugin exits before it takes over the main session's
// poller.
func (r *Runner) runnerEnv(t *task.Task) []string {
	env := []string{
		"HRK_TASK=" + t.ID, "HRK_STATE_DIR=" + r.Store.Dir(),
		"TELEGRAM_BOT_TOKEN=", "CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=false",
	}
	if t.ClaudeConfigDir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+t.ClaudeConfigDir)
	}
	return env
}

// openLocation creates the worktree workspace or the tab the agent runs in.
func (r *Runner) openLocation(ctx context.Context, t *task.Task) (herdr.Location, error) {
	label := t.ID + " · " + t.Title
	if !t.UseWorktree {
		return r.Herdr.TabCreate(ctx, t.MainWorkspace, t.Cwd, label, r.runnerEnv(t))
	}
	// Reuse the repository's open workspace, so herdr does not open another.
	repoWS, err := r.Herdr.RepoWorkspace(ctx, t.RepoRoot)
	if err != nil {
		return herdr.Location{}, err
	}
	base := t.Worktree.Base
	if base == "" {
		base = "HEAD"
	}
	// A queued or held task waited; the branch may have appeared since spawn
	// checked it. Never adopt a branch this task did not create.
	if BranchExists(ctx, t.RepoRoot, t.Worktree.Branch) {
		return herdr.Location{}, fmt.Errorf("branch %s already exists", t.Worktree.Branch)
	}
	loc, err := r.Herdr.WorktreeCreate(ctx, herdr.WorktreeOptions{
		Workspace: repoWS, Cwd: t.RepoRoot, Branch: t.Worktree.Branch, Base: base, Label: label,
	})
	if err != nil {
		return loc, err
	}
	// Record the worktree at once, so cleanup finds it even when a later
	// launch step fails.
	if _, err := r.Store.Update(t.ID, func(t *task.Task) error {
		t.Herdr.WorkspaceID, t.Herdr.TabID, t.Herdr.PaneID = loc.WorkspaceID, loc.TabID, loc.PaneID
		if loc.Path != "" {
			t.Worktree.Path = loc.Path
		}
		t.Worktree.BranchCreated = new(true)
		return nil
	}); err != nil {
		return loc, err
	}
	// worktree create has no --env, so export in the root pane's shell.
	return loc, r.exportEnv(ctx, loc.PaneID, t, false)
}

// exportEnv sets the runner's environment in the shell of pane. With reset,
// a Claude config folder the task no longer uses is unset too.
func (r *Runner) exportEnv(ctx context.Context, pane string, t *task.Task, reset bool) error {
	env := r.runnerEnv(t)
	quoted := make([]string, len(env))
	for i, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		quoted[i] = k + "=" + harness.ShellQuote(v)
	}
	cmd := "export " + strings.Join(quoted, " ")
	if reset && t.ClaudeConfigDir == "" {
		cmd += "; unset CLAUDE_CONFIG_DIR"
	}
	if err := r.Herdr.PaneRun(ctx, pane, cmd+"; clear"); err != nil {
		return fmt.Errorf("set runner environment: %w", err)
	}
	r.Sleep(300 * time.Millisecond) // let the shell return to its prompt
	return nil
}

// prepareTrust makes sure the agent trusts the folder spawn ran in, so the
// runner starts without a trust dialog.
func (r *Runner) prepareTrust(t *task.Task, h task.Harness) error {
	switch h {
	case task.Claude:
		runnerPath := t.Cwd
		if t.Worktree != nil && t.Worktree.Path != "" {
			runnerPath = t.Worktree.Path
		}
		path, err := harness.ClaudeConfigPath(t.ClaudeConfigDir)
		if err != nil {
			return err
		}
		set, skipped, err := harness.PrepareClaudeTrust(path, t.Cwd, runnerPath)
		if err != nil {
			return fmt.Errorf("prepare Claude trust: %w", err)
		}
		switch skipped {
		case harness.TrustSkipNoConfig:
			r.emit(t, "claude-trust-skipped", "Claude has never run with this config dir: "+path+" is missing", "")
		case harness.TrustSkipAlreadyTrusted:
			r.emit(t, "claude-trust-skipped", runnerPath+" is already trusted in "+path, "")
		default:
			r.emit(t, "claude-trusted", "set "+strings.Join(set, ", ")+" for "+runnerPath, "")
		}
	case task.Codex:
		root := t.RepoRoot
		if root == "" {
			root = t.Cwd
		}
		path, err := harness.CodexConfigPath()
		if err != nil {
			return err
		}
		added, err := harness.EnsureCodexTrust(path, root)
		if err != nil {
			return err
		}
		if added {
			r.emit(t, "codex-trusted", "added "+root+" to "+path+" as trusted", "")
		}
	}
	return nil
}

// settleStartup turns the agent start result into a ready agent, or into a
// recorded needs-human or failed state.
func (r *Runner) settleStartup(ctx context.Context, t *task.Task, a harness.Adapter, pane string, startErr error) error {
	notReady := herdr.HasCode(startErr, herdr.CodeAgentNotReady)
	switch {
	case startErr == nil, notReady:
	case herdr.HasCode(startErr, herdr.CodeTimeout):
		screen, _ := r.Herdr.PaneRead(ctx, pane, 40)
		if a.ShowsUsageLimit(screen) && r.switchable(t) {
			return errUsageLimit
		}
		if line := errorLine(screen); line != "" {
			return fmt.Errorf("agent exited during start: %s", line)
		}
		state, err := r.Herdr.AgentWait(ctx, pane, agentStartTimeout, "idle", "done", "blocked")
		if err != nil {
			return fmt.Errorf("agent did not become ready: %w", err)
		}
		notReady = state == "blocked"
	default:
		return fmt.Errorf("start agent: %w", startErr)
	}

	return r.settleDialogs(ctx, t, a, pane, notReady)
}

// settleDialogs answers or waits out the startup dialogs on screen until
// none shows. notReady says herdr saw the agent as not ready, which means a
// dialog is open even when no known one matches.
func (r *Runner) settleDialogs(ctx context.Context, t *task.Task, a harness.Adapter, pane string, notReady bool) error {
	for range maxDialogRounds {
		d, found, err := r.findDialog(ctx, a, pane, notReady)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		notReady = false
		if d.Action == harness.SendKeys {
			if err := r.Herdr.AgentSendKeys(ctx, pane, d.Keys...); err != nil {
				return err
			}
			r.emit(t, d.Name+"-skipped", "answered the "+d.Name+" dialog with "+strings.Join(d.Keys, " "), "")
			r.waitDialogGone(ctx, pane, d)
			continue
		}
		if err := r.waitForHuman(ctx, t, pane, d.Name); err != nil {
			return err
		}
	}
	return errors.New("startup dialogs kept appearing")
}

// findDialog returns the startup dialog on screen. When the agent is not
// ready and no known dialog shows, the dialog may still be drawing: it
// re-reads the screen for about two seconds, then waits a few seconds for
// the agent to turn idle, and reads once more. Only an agent that is still
// not ready with no known dialog on screen counts as an unknown dialog.
func (r *Runner) findDialog(ctx context.Context, a harness.Adapter, pane string, notReady bool) (harness.Dialog, bool, error) {
	screen, err := r.Herdr.PaneRead(ctx, pane, 40)
	if err != nil {
		return harness.Dialog{}, false, err
	}
	if d, ok := harness.DetectDialog(a, screen); ok || !notReady {
		return d, ok, nil
	}
	for range 20 {
		r.Sleep(100 * time.Millisecond)
		if screen, err = r.Herdr.PaneRead(ctx, pane, 40); err != nil {
			return harness.Dialog{}, false, err
		}
		if d, ok := harness.DetectDialog(a, screen); ok {
			return d, true, nil
		}
	}
	_, waitErr := r.Herdr.AgentWait(ctx, pane, unknownDialogIdleWait, "idle", "done")
	if screen, err = r.Herdr.PaneRead(ctx, pane, 40); err != nil {
		return harness.Dialog{}, false, err
	}
	if d, ok := harness.DetectDialog(a, screen); ok {
		return d, true, nil
	}
	if waitErr == nil {
		return harness.Dialog{}, false, nil
	}
	// herdr saw a dialog this kit does not know. Leave it to the user.
	return harness.Dialog{Name: "unknown startup", Action: harness.NeedsHuman}, true, nil
}

// waitDialogGone polls the screen until dialog d has closed, for at most two
// seconds.
func (r *Runner) waitDialogGone(ctx context.Context, pane string, d harness.Dialog) {
	for range 20 {
		r.Sleep(100 * time.Millisecond)
		screen, err := r.Herdr.PaneRead(ctx, pane, 40)
		if err != nil || !harness.Shows(screen, d) {
			return
		}
	}
}

// waitForHuman marks the task needs-human until the user answers a startup
// dialog in the runner's tab, then lets the launch continue.
func (r *Runner) waitForHuman(ctx context.Context, t *task.Task, pane, dialog string) error {
	q := fmt.Sprintf("Runner %s (%s) shows the %s dialog. Answer it in its herdr tab; the task continues on its own.", t.ID, t.Title, dialog)
	nt, changed, err := r.Store.Transition(t.ID, task.NeedsHuman, []task.Status{task.Starting}, func(t *task.Task) {
		t.Error = dialog
		t.LaunchDeadline = time.Now().UTC().Add(dialogAnswerTimeout + LaunchBudget)
	})
	if err != nil {
		return err
	}
	if changed {
		r.emit(nt, "needs-human", dialog, q)
	}
	_, err = r.Herdr.AgentWait(ctx, pane, dialogAnswerTimeout, "idle", "done")
	switch {
	case herdr.HasCode(err, herdr.CodeAgentNotRunning):
		return fmt.Errorf("the agent exited at the %s dialog", dialog)
	case err != nil:
		return fmt.Errorf("no answer to the %s dialog: %w", dialog, err)
	}
	_, _, err = r.Store.Transition(t.ID, task.Starting, []task.Status{task.NeedsHuman}, func(t *task.Task) {
		t.Error = ""
		t.LaunchDeadline = time.Now().UTC().Add(LaunchBudget)
	})
	return err
}

// errorLine returns the first "Error:" line on screen.
func errorLine(screen string) string {
	for line := range strings.SplitSeq(screen, "\n") {
		if l := strings.TrimSpace(line); strings.HasPrefix(l, "Error:") {
			return l
		}
	}
	return ""
}

func startedLine(t *task.Task, spec task.AgentSpec) string {
	parts := []string{t.ID + " started", string(spec.Harness)}
	if t.Worktree != nil && t.Worktree.Path != "" {
		parts = append(parts, "worktree "+t.Worktree.Branch+" at "+t.Worktree.Path)
	}
	parts = append(parts, fmt.Sprintf("tab %q", t.ID+" · "+t.Title))
	return strings.Join(parts, " · ")
}
