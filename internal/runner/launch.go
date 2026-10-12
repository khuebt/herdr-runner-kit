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
	spec, err := task.ParseAgentSpec(t.Agent)
	if err != nil {
		return err
	}
	adapter, err := harness.For(spec.Harness)
	if err != nil {
		return err
	}
	files, err := r.writeRunnerFiles(t, adapter)
	if err != nil {
		return err
	}

	loc, err := r.openLocation(ctx, t)
	if err != nil {
		return fmt.Errorf("open runner location: %w", err)
	}
	name := "hrk-" + t.ID
	if t, err = r.Store.Update(t.ID, func(t *task.Task) error {
		t.Herdr = task.Herdr{WorkspaceID: loc.WorkspaceID, TabID: loc.TabID, PaneID: loc.PaneID, AgentName: name}
		if t.Worktree != nil && loc.Path != "" {
			t.Worktree.Path = loc.Path
		}
		return nil
	}); err != nil {
		return err
	}

	if err := r.prepareTrust(t, spec.Harness); err != nil {
		return err
	}

	args := adapter.LaunchArgs(harness.LaunchOptions{
		Spec: spec, Bin: r.Bin, SettingsPath: files.settings, ContractPath: files.contract,
	})
	_, err = r.Herdr.AgentStart(ctx, name, adapter.HerdrKind(), loc.PaneID, agentStartTimeout, args)
	if err := r.settleStartup(ctx, t, adapter, loc.PaneID, err); err != nil {
		return err
	}

	// Address the agent by pane id from here on: herdr drops the name when
	// a start times out.
	text := adapter.FirstPrompt(t.Skill, t.Prompt, files.contractText)
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

// runnerEnv is the environment every runner gets. TELEGRAM_BOT_TOKEN is empty
// so the Telegram plugin exits before it takes over the main session's
// poller.
func runnerEnv(t *task.Task) []string {
	env := []string{"HRK_TASK=" + t.ID, "TELEGRAM_BOT_TOKEN=", "CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=false"}
	if t.ClaudeConfigDir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+t.ClaudeConfigDir)
	}
	return env
}

// openLocation creates the worktree workspace or the tab the agent runs in.
func (r *Runner) openLocation(ctx context.Context, t *task.Task) (herdr.Location, error) {
	label := t.ID + " · " + t.Title
	env := runnerEnv(t)
	if !t.UseWorktree {
		return r.Herdr.TabCreate(ctx, t.MainWorkspace, t.Cwd, label, env)
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
	loc, err := r.Herdr.WorktreeCreate(ctx, herdr.WorktreeOptions{
		Workspace: repoWS, Cwd: t.RepoRoot, Branch: t.Worktree.Branch, Base: base, Label: label,
	})
	if err != nil {
		return loc, err
	}
	// worktree create has no --env, so export in the root pane's shell.
	quoted := make([]string, len(env))
	for i, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		quoted[i] = k + "=" + harness.ShellQuote(v)
	}
	if err := r.Herdr.PaneRun(ctx, loc.PaneID, "export "+strings.Join(quoted, " ")+"; clear"); err != nil {
		return loc, fmt.Errorf("set runner environment: %w", err)
	}
	r.Sleep(300 * time.Millisecond) // let the shell return to its prompt
	return loc, nil
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
		path := filepath.Join(t.ClaudeConfigDir, ".claude.json")
		if t.ClaudeConfigDir == "" {
			var err error
			if path, err = harness.ClaudeConfigPath(); err != nil {
				return err
			}
		}
		set, err := harness.PrepareClaudeTrust(path, t.Cwd, runnerPath)
		if err != nil {
			return fmt.Errorf("prepare Claude trust: %w", err)
		}
		if len(set) > 0 {
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
	switch {
	case startErr == nil, herdr.HasCode(startErr, herdr.CodeAgentNotReady):
	case herdr.HasCode(startErr, herdr.CodeTimeout):
		screen, _ := r.Herdr.PaneRead(ctx, pane, 40)
		if line := errorLine(screen); line != "" {
			return fmt.Errorf("agent exited during start: %s", line)
		}
		if _, err := r.Herdr.AgentWait(ctx, pane, agentStartTimeout, "idle", "done", "blocked"); err != nil {
			return fmt.Errorf("agent did not become ready: %w", err)
		}
	default:
		return fmt.Errorf("start agent: %w", startErr)
	}

	for range maxDialogRounds {
		screen, err := r.Herdr.PaneRead(ctx, pane, 40)
		if err != nil {
			return err
		}
		d, ok := harness.DetectDialog(a, screen)
		if !ok {
			if startErr == nil || !herdr.HasCode(startErr, herdr.CodeAgentNotReady) {
				return nil
			}
			// herdr saw a dialog this kit does not know. Leave it to the user.
			d = harness.Dialog{Name: "unknown startup dialog", Action: harness.NeedsHuman}
		}
		if d.Action == harness.SendKeys {
			if err := r.Herdr.AgentSendKeys(ctx, pane, d.Keys...); err != nil {
				return err
			}
			r.emit(t, d.Name+"-skipped", "answered the "+d.Name+" dialog with "+strings.Join(d.Keys, " "), "")
			r.waitDialogGone(ctx, pane, d)
			startErr = nil
			continue
		}
		if err := r.waitForHuman(ctx, t, pane, d.Name); err != nil {
			return err
		}
		startErr = nil
	}
	return errors.New("startup dialogs kept appearing")
}

// waitDialogGone polls the screen until dialog d has closed, for at most two
// seconds.
func (r *Runner) waitDialogGone(ctx context.Context, pane string, d harness.Dialog) {
	for range 20 {
		r.Sleep(100 * time.Millisecond)
		screen, err := r.Herdr.PaneRead(ctx, pane, 40)
		if err != nil || !strings.Contains(screen, d.Match) {
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
