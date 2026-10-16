package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/harness"
	"github.com/khuebt/herdr-runner-kit/internal/herdr"
	"github.com/khuebt/herdr-runner-kit/internal/project"
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
	var carried []proc
	if t.Herdr.PaneID != "" && (len(t.FallbackFrom) > 0 || t.Attempt > 1) {
		// The agent hit its usage limit, or neared its context limit: its
		// fallback, or a fresh session, takes over the same pane and
		// checkout.
		left, err := r.clearPane(ctx, t)
		if err != nil {
			return err
		}
		carried = left
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
		// The first round starts here; the git line counts its commits, and a
		// plan phase is checked against what it changed since.
		if head := roundHead(t); head != "" {
			if t, err = r.Store.Update(t.ID, func(t *task.Task) error {
				t.RoundHead = head
				if t.Plan != "" && t.PhaseStart == "" {
					t.PhaseStart = head
				}
				return nil
			}); err != nil {
				return err
			}
		}
	}

	spec, adapter, contract, err := r.startAgent(ctx, t, name, loc.PaneID)
	for errors.Is(err, errUsageLimit) {
		// errUsageLimit comes only while a fallback is left, and each switch
		// records the agent it leaves, so this ends once they run out.
		if t, err = r.claimSwitch(t, []task.Status{task.Starting}, false, false); err != nil {
			return err
		}
		// The latest snapshot holds every process still running, including
		// those an earlier quit left.
		if carried, err = r.clearPane(ctx, t); err != nil {
			return err
		}
		spec, adapter, contract, err = r.startAgent(ctx, t, name, loc.PaneID)
	}
	if err != nil {
		return err
	}
	scope := r.enterScope(ctx, t, loc.PaneID)

	// Address the agent by pane id from here on: herdr drops the name when
	// a start times out.
	prompt := t.Prompt
	if t.Inputs != "" {
		prompt = strings.TrimSpace(prompt + "\n\n" + untrustedInputs(t))
	}
	for _, note := range relaunchNotes(t) {
		prompt = strings.TrimSpace(prompt + "\n\n" + note)
	}
	if len(carried) > 0 {
		prompt = strings.TrimSpace(prompt + "\n\n" + carriedNote(carried))
	}
	text := adapter.FirstPrompt(t.Skill, prompt, contract)
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
		r.emit(t, "started", startedLine(t, spec, scope), "")
	}
	if _, err := r.Herdr.AgentWait(ctx, loc.PaneID, 5*time.Second, "working"); err != nil {
		r.emit(t, "prompt-unconfirmed", "prompt sent, but herdr saw no turn start within 5 s", "")
		r.resubmitFirstPrompt(ctx, t, adapter, loc.PaneID, text)
	}
	r.emit(t, "prompted", "", "")
	return nil
}

// resubmitFirstPrompt handles a first prompt herdr saw no turn start for. When
// the adapter reads the agent's input box as still holding the prompt, its
// Enter was lost: it sends one more Enter, never the text again, and waits
// once more. A runner whose prompt is still in the input after that is
// stalled news. An agent already working, an empty input and an input the
// adapter cannot read leave everything as it was; Codex can report a stale
// state after skipped hooks, so it never resubmits.
func (r *Runner) resubmitFirstPrompt(ctx context.Context, t *task.Task, adapter harness.Adapter, pane, prompt string) {
	if a, err := r.Herdr.AgentGet(ctx, pane); err != nil || a.Status == "working" {
		return
	}
	screen, err := r.Herdr.PaneRead(ctx, pane, progressScreenLines)
	if err != nil || adapter.InputState(screen, prompt) != harness.InputHolds {
		return
	}
	if err := r.Herdr.AgentSendKeys(ctx, pane, "enter"); err != nil {
		return
	}
	if _, err := r.Herdr.AgentWait(ctx, pane, 5*time.Second, "working"); err != nil {
		// A loaded machine can take longer to show the turn: only a prompt
		// still in the input is stalled.
		if screen, err := r.Herdr.PaneRead(ctx, pane, progressScreenLines); err == nil && adapter.InputState(screen, prompt) == harness.InputHolds {
			r.markStalled(t.ID, "stalled", "prompt not submitted: the first prompt is still in the input after a second Enter; the runner is left open")
			return
		}
	}
	r.emit(t, "prompt-resubmitted", "the first prompt was still in the input; sent Enter again", "")
}

// startAgent writes the runner files for t's agent, prepares its trust and
// starts it in pane until it is ready for the first prompt. It returns
// errUsageLimit when the agent shows a usage limit at startup and a
// fallback agent is left; without one the prompt goes in, and the agent's
// own turn-end reports the limit. It also returns the contract text.
func (r *Runner) startAgent(ctx context.Context, t *task.Task, name, pane string) (task.AgentSpec, harness.Adapter, string, error) {
	spec, err := task.ParseAgentSpec(t.Agent)
	if err != nil {
		return spec, nil, "", err
	}
	adapter, err := harness.For(spec.Harness)
	if err != nil {
		return spec, nil, "", err
	}
	contract, err := r.writeRunnerFiles(t, adapter)
	if err != nil {
		return spec, nil, "", err
	}
	if err := r.prepareTrust(t, adapter); err != nil {
		return spec, nil, "", err
	}
	args := adapter.LaunchArgs(harness.LaunchOptions{Spec: spec, Bin: r.Bin, Dir: r.Store.TaskDir(t.ID)})
	_, err = r.Herdr.AgentStart(ctx, name, adapter.HerdrKind(), pane, agentStartTimeout, args)
	if err := r.settleStartup(ctx, t, adapter, pane, err); err != nil {
		return spec, nil, "", err
	}
	if screen, err := r.Herdr.PaneRead(ctx, pane, 40); err == nil && adapter.ShowsUsageLimit(screen) && r.switchable(t) {
		return spec, nil, "", errUsageLimit
	}
	return spec, adapter, contract, nil
}

// writeRunnerFiles creates the output directory and writes the adapter's
// per-task runner files into the task folder. It returns the contract
// text.
func (r *Runner) writeRunnerFiles(t *task.Task, adapter harness.Adapter) (string, error) {
	dir := r.Store.TaskDir(t.ID)
	outDir := r.OutDir(t.ID)
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return "", err
	}
	contract, err := r.contract(t, outDir, adapter.Harness())
	if err != nil {
		return "", err
	}
	files, err := adapter.Files(harness.FilesInput{Bin: r.Bin, Contract: contract})
	if err != nil {
		return "", err
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.Name), f.Data, 0o600); err != nil {
			return "", err
		}
	}
	return contract, nil
}

// runnerEnv is the environment every runner gets. HRK_STATE_DIR points
// report and the turn-end hook at the spawner's state, and HRK_ATTEMPT,
// from the second session on, tells them which session they run in.
// TELEGRAM_BOT_TOKEN is empty so the Telegram plugin exits before it takes
// over the main session's poller.
func (r *Runner) runnerEnv(t *task.Task) []string {
	env := []string{
		"HRK_TASK=" + t.ID, "HRK_STATE_DIR=" + r.Store.Dir(),
		"TELEGRAM_BOT_TOKEN=", "CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=false",
	}
	if t.ClaudeConfigDir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+t.ClaudeConfigDir)
	}
	if t.Attempt > 1 {
		env = append(env, "HRK_ATTEMPT="+strconv.Itoa(t.Attempt))
	}
	return env
}

// openLocation creates the worktree workspace or the tab the agent runs in.
// A project task opens in its project's workspace; when that cannot be
// opened, it falls back to where a task without a project would go.
func (r *Runner) openLocation(ctx context.Context, t *task.Task) (herdr.Location, error) {
	label := t.ID + " · " + t.Title
	tabWS, repoWS := t.MainWorkspace, ""
	if t.Project != "" {
		ws, err := r.projectWorkspace(ctx, t)
		if err == nil {
			tabWS, repoWS = ws, ws
		} else {
			where := "the tab opens in the main workspace"
			if t.UseWorktree {
				where = "the worktree opens as its own workspace"
			}
			r.emit(t, "project-workspace-fallback", "open the workspace of project "+t.Project+": "+err.Error()+"; "+where, "")
		}
	}
	if t.CheckoutOf != "" {
		path, ws, err := r.sharedCheckout(ctx, t)
		if err != nil {
			return herdr.Location{}, err
		}
		if ws != "" {
			tabWS = ws
		}
		return r.Herdr.TabCreate(ctx, tabWS, path, label, r.runnerEnv(t))
	}
	if !t.UseWorktree {
		return r.Herdr.TabCreate(ctx, tabWS, t.Cwd, label, r.runnerEnv(t))
	}
	if t.Project == "" {
		// Reuse the repository's open workspace, so herdr does not open another.
		var err error
		if repoWS, err = r.Herdr.RepoWorkspace(ctx, t.RepoRoot); err != nil {
			return herdr.Location{}, err
		}
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

// sharedCheckout points t, which works in the worktree of task
// t.CheckoutOf, at that worktree, and returns it with the anchor's herdr
// workspace while that is still open on it.
func (r *Runner) sharedCheckout(ctx context.Context, t *task.Task) (path, workspace string, err error) {
	anchor, err := r.Store.Get(t.CheckoutOf)
	if err != nil {
		return "", "", err
	}
	if anchor.Worktree == nil || anchor.Worktree.Path == "" {
		return "", "", fmt.Errorf("the run's checkout of %s was never created", anchor.ID)
	}
	path = filepath.Clean(anchor.Worktree.Path)
	if anchor.Worktree.Removed || !isDir(path) {
		return "", "", fmt.Errorf("the run's checkout %s is gone", path)
	}
	if _, err := r.Store.Update(t.ID, func(t *task.Task) error {
		t.Cwd = path
		return nil
	}); err != nil {
		return "", "", err
	}
	// herdr reuses the ids of closed workspaces, so the anchor's id counts
	// only while it still shows this checkout.
	if id := anchor.Herdr.WorkspaceID; id != "" {
		if checkout, err := r.Herdr.WorkspaceCheckout(ctx, id); err == nil && filepath.Clean(checkout) == path {
			workspace = id
		}
	}
	return path, workspace, nil
}

// projectWorkspace returns the herdr workspace of t's project, opening it
// again when the user closed it.
func (r *Runner) projectWorkspace(ctx context.Context, t *task.Task) (string, error) {
	if t.RepoRoot == "" {
		return "", fmt.Errorf("%s is not in a git repository", t.Cwd)
	}
	return project.OpenWorkspace(ctx, r.Store, r.Herdr, t.Project, t.RepoRoot)
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
func (r *Runner) prepareTrust(t *task.Task, adapter harness.Adapter) error {
	runnerPath := t.Cwd
	if t.Worktree != nil && t.Worktree.Path != "" {
		runnerPath = t.Worktree.Path
	}
	res, err := adapter.PrepareTrust(harness.TrustInput{
		Cwd: t.Cwd, RepoRoot: t.RepoRoot, RunnerPath: runnerPath, ClaudeConfigDir: t.ClaudeConfigDir,
	})
	if err != nil {
		return err
	}
	if res.Event != "" {
		r.emit(t, res.Event, res.Detail, "")
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
		if err := r.waitForHuman(ctx, t, pane, d); err != nil {
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
// dialog in the runner's tab, then lets the launch continue. herdr's state
// alone does not tell: it reports pi idle while pi's trust dialog shows, so
// a known dialog must also be gone from the screen.
func (r *Runner) waitForHuman(ctx context.Context, t *task.Task, pane string, d harness.Dialog) error {
	dialog := d.Name
	deadline := time.Now().Add(dialogAnswerTimeout)
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
	if d.IdleWhileShown {
		if err := r.waitScreenClears(ctx, pane, d, deadline); err != nil {
			return err
		}
	}
	_, _, err = r.Store.Transition(t.ID, task.Starting, []task.Status{task.NeedsHuman}, func(t *task.Task) {
		t.Error = ""
		t.LaunchDeadline = time.Now().UTC().Add(LaunchBudget)
	})
	return err
}

// dialogPollEvery is how often waitForHuman reads the screen while a
// dialog stays on it.
const dialogPollEvery = time.Second

// waitScreenClears reads the screen until dialog d is gone from it, up to
// deadline. A read error counts as gone: the next dialog round reads again.
func (r *Runner) waitScreenClears(ctx context.Context, pane string, d harness.Dialog, deadline time.Time) error {
	for range int(dialogAnswerTimeout / dialogPollEvery) {
		screen, err := r.Herdr.PaneRead(ctx, pane, 40)
		if err != nil || !harness.Shows(screen, d) {
			return nil
		}
		if _, err := r.Herdr.AgentGet(ctx, pane); isGone(err) {
			return fmt.Errorf("the agent exited at the %s dialog", d.Name)
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return fmt.Errorf("no answer to the %s dialog", d.Name)
		}
		r.Sleep(dialogPollEvery)
	}
	return fmt.Errorf("no answer to the %s dialog", d.Name)
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

func startedLine(t *task.Task, spec task.AgentSpec, scope string) string {
	parts := []string{t.ID + " started", string(spec.Harness)}
	if t.Worktree != nil && t.Worktree.Path != "" {
		parts = append(parts, "worktree "+t.Worktree.Branch+" at "+t.Worktree.Path)
	}
	if scope != "" {
		parts = append(parts, scope)
	}
	parts = append(parts, fmt.Sprintf("tab %q", t.ID+" · "+t.Title))
	return strings.Join(parts, " · ")
}
