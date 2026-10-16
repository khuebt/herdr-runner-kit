package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// setupTask stores a starting code task whose worktree exists, with setup
// commands cmds configured for its repository.
func setupTask(t *testing.T, r *Runner, cmds ...string) (*task.Task, string, string) {
	t.Helper()
	repo := gitRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, repo, "worktree", "add", "-q", "-b", "hrk/t1-x", wt)
	r.Config.WorktreeSetup = map[string][]string{repo: cmds}
	tk := &task.Task{
		Kind: "code", Agent: "claude", Prompt: "x", Cwd: repo, Owner: repo, RepoRoot: repo, Status: task.Starting,
		UseWorktree: true, Worktree: &task.Worktree{Path: wt, Branch: "hrk/t1-x", BranchCreated: new(true)},
	}
	if err := r.Store.Create(tk); err != nil {
		t.Fatal(err)
	}
	return tk, repo, wt
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestWorktreeSetupRunsInOrderInTheWorktree(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk, repo, wt := setupTask(t, r,
		`pwd > order.txt; echo "$HRK_REPO_ROOT $HRK_WORKTREE $HRK_TASK" >> order.txt`,
		`echo two >> order.txt; cat "$HRK_STATE_DIR/heavy/slot-1.json" > held.json`)
	tk, res, err := r.setUpWorktree(context.Background(), tk)
	if err != nil || res == nil {
		t.Fatalf("setup = %+v, %v", res, err)
	}
	if res.Failed != "" || res.Steps != 2 || !strings.HasPrefix(res.summary(), "setup: 2 steps, ") {
		t.Errorf("result = %+v, summary %q", res, res.summary())
	}
	if got, want := readFile(t, filepath.Join(wt, "order.txt")), wt+"\n"+repo+" "+wt+" t1\ntwo\n"; got != want {
		t.Errorf("order.txt = %q, want %q", got, want)
	}
	if held := readFile(t, filepath.Join(wt, "held.json")); !strings.Contains(held, `"task":"t1"`) || !strings.Contains(held, `"command":"worktree setup"`) {
		t.Errorf("the setup held no heavy slot: %s", held)
	}
	if log := readFile(t, res.Log); res.Log != filepath.Join(r.OutDir("t1"), setupLog) || !strings.Contains(log, "$ echo two >> order.txt") {
		t.Errorf("log %s = %q", res.Log, log)
	}
	if st, _ := r.Store.HeavyStatus(r.Config.HeavySlots); len(st.Holders) != 0 {
		t.Errorf("the slot is still held: %+v", st.Holders)
	}
	if left := time.Until(tk.LaunchDeadline); left > LaunchBudget || left < LaunchBudget-time.Minute {
		t.Errorf("launch deadline %s ahead, want it back to the launch budget", left)
	}
	if ev := events(t, r, tk.ID); slices.Contains(ev, "worktree-setup-failed") {
		t.Errorf("events = %v", ev)
	}
}

func TestWorktreeSetupStopsAtTheFirstFailure(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk, _, wt := setupTask(t, r, "echo installing; exit 3", "touch never")
	_, res, err := r.setUpWorktree(context.Background(), tk)
	if err != nil || res == nil {
		t.Fatalf("setup = %+v, %v", res, err)
	}
	if res.Failed != "echo installing; exit 3" || res.Reason != "exit 3" || res.Steps != 0 {
		t.Errorf("result = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(wt, "never")); err == nil {
		t.Error("the step after the failure ran")
	}
	if log := readFile(t, res.Log); !strings.Contains(log, "installing\n") || !strings.Contains(log, "failed: exit 3") {
		t.Errorf("log = %q", log)
	}
	failed := eventsOf(t, r, tk.ID, "worktree-setup-failed")
	want := fmt.Sprintf(`setup step "echo installing; exit 3" failed (exit 3); log: %s`, res.Log)
	if len(failed) != 1 || failed[0].Summary != want {
		t.Errorf("events = %+v, want %q", failed, want)
	}
}

func TestWorktreeSetupTimeoutKillsTheStep(t *testing.T) {
	r, _, _ := newTestRunner(t)
	defer func(d time.Duration) { worktreeSetupTimeout = d }(worktreeSetupTimeout)
	worktreeSetupTimeout = 300 * time.Millisecond
	tk, _, wt := setupTask(t, r, "sleep 30 & echo $! > child.pid; wait", "touch never")
	start := time.Now()
	_, res, err := r.setUpWorktree(context.Background(), tk)
	if err != nil || res == nil {
		t.Fatalf("setup = %+v, %v", res, err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("setup took %s after its timeout", d)
	}
	if res.Reason != "timed out after 300ms" || res.Steps != 0 {
		t.Errorf("result = %+v", res)
	}
	pid := strings.TrimSpace(readFile(t, filepath.Join(wt, "child.pid")))
	for range 50 {
		if exec.Command("kill", "-0", pid).Run() != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("the setup command's child %s outlived the timeout", pid)
}

func TestWorktreeSetupWithoutAnEntryDoesNothing(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk, _, _ := setupTask(t, r, "touch x")
	r.Config.WorktreeSetup = map[string][]string{"/elsewhere": {"touch x"}}
	if _, res, err := r.setUpWorktree(context.Background(), tk); res != nil || err != nil {
		t.Errorf("setup = %+v, %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(r.OutDir(tk.ID), setupLog)); err == nil {
		t.Error("a setup log was written")
	}
}

// setupLaunch launches a code task whose worktree herdr creates for real,
// with setup commands cmds. It returns the task, its worktree and whether
// the setup had run when the agent started.
func setupLaunch(t *testing.T, cmds ...string) (*Runner, *fakeHerdr, *task.Task, string, bool) {
	t.Helper()
	r, fake, _ := newTestRunner(t)
	repo := gitRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	fake.on("worktree create", func(args []string) (string, error) {
		branch := args[slices.Index(args, "--branch")+1]
		if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-q", "-b", branch, wt).CombinedOutput(); err != nil {
			return "", fmt.Errorf("%v: %s", err, out)
		}
		return fmt.Sprintf(`{"result":{"root_pane":{"pane_id":"w2:p1","tab_id":"w2:t1","workspace_id":"w2"},"worktree":{"path":%q}}}`, wt), nil
	})
	ranFirst := false
	fake.on("agent start", func([]string) (string, error) {
		_, err := os.Stat(filepath.Join(r.OutDir("t1"), setupLog))
		ranFirst = err == nil
		return `{"result":{"agent":{"agent":"claude","agent_status":"idle"}}}`, nil
	})
	r.Config.WorktreeSetup = map[string][]string{repo: cmds}
	t.Chdir(repo)
	tk := spawnStarting(t, r, SpawnOptions{Kind: "code", Prompt: "fix it", Cwd: repo})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	tk, _ = r.Store.Get(tk.ID)
	return r, fake, tk, wt, ranFirst
}

func TestLaunchRunsWorktreeSetupBeforeTheAgent(t *testing.T) {
	r, fake, tk, wt, ranFirst := setupLaunch(t, `cp "$HRK_REPO_ROOT/.git/HEAD" copied`, "true")
	if !ranFirst {
		t.Error("the agent started before the setup ran")
	}
	if _, err := os.Stat(filepath.Join(wt, "copied")); err != nil {
		t.Errorf("setup did not run in the worktree: %v", err)
	}
	started := eventsOf(t, r, tk.ID, "started")
	if len(started) != 1 || !strings.Contains(started[0].Summary, " · setup: 2 steps, ") {
		t.Errorf("started = %+v", started)
	}
	if prompt := lastCall(fake, "agent", "prompt")[3]; strings.Contains(prompt, "worktree setup") {
		t.Errorf("a successful setup added a note: %q", prompt)
	}
	if tk.Status != task.Running {
		t.Errorf("status = %s", tk.Status)
	}
}

func TestLaunchAfterAFailedWorktreeSetup(t *testing.T) {
	r, fake, tk, _, _ := setupLaunch(t, "exit 1", "true")
	if tk.Status != task.Running {
		t.Errorf("a failed setup kept the agent from starting: %+v", tk)
	}
	log := filepath.Join(r.OutDir(tk.ID), setupLog)
	note := `The worktree setup step "exit 1" failed (exit 1); log: ` + log + ". Fix the setup before running checks, or report needs-human."
	if prompt := lastCall(fake, "agent", "prompt")[3]; !strings.Contains(prompt, "fix it\n\n"+note) {
		t.Errorf("first prompt lacks the note:\n%s", prompt)
	}
	if ev := events(t, r, tk.ID); !slices.Contains(ev, "worktree-setup-failed") {
		t.Errorf("events = %v", ev)
	}
	if started := eventsOf(t, r, tk.ID, "started"); len(started) != 1 || strings.Contains(started[0].Summary, "setup:") {
		t.Errorf("started = %+v", started)
	}
}

func TestSharedCheckoutSkipsWorktreeSetup(t *testing.T) {
	r, _, _ := newTestRunner(t)
	repo, wt := worktreeRepo(t, "hrk/t1-x")
	anchor := &task.Task{
		Kind: "flow", Agent: "claude", Prompt: "x", Cwd: repo, RepoRoot: repo, Owner: repo, Status: task.Done,
		UseWorktree: true, Worktree: &task.Worktree{Branch: "hrk/t1-x", Path: wt, BranchCreated: new(true)},
	}
	if err := r.Store.Create(anchor); err != nil {
		t.Fatal(err)
	}
	r.Config.WorktreeSetup = map[string][]string{repo: {"touch marker"}}
	tk := spawnStarting(t, r, SpawnOptions{Kind: "flow", Prompt: "phase", Cwd: repo, CheckoutOf: anchor.ID, Plan: "/p", Phase: 2})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt, "marker")); err == nil {
		t.Error("a plan phase in the run's checkout ran the setup again")
	}
}

func TestRelaunchSkipsWorktreeSetup(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	repo, wt := worktreeRepo(t, "hrk/t1-x")
	r.Config.WorktreeSetup = map[string][]string{repo: {"touch marker"}}
	tk := &task.Task{
		Kind: "code", Agent: "claude", Prompt: "x", Cwd: repo, Owner: repo, RepoRoot: repo,
		UseWorktree: true, Worktree: &task.Worktree{Path: wt, Branch: "hrk/t1-x", BranchCreated: new(true)},
		Gate: task.GateAuto, Status: task.Starting, FallbackFrom: []string{"codex"},
		Herdr: task.Herdr{WorkspaceID: "w2", PaneID: "w2:p1"},
	}
	if err := r.Store.Create(tk); err != nil {
		t.Fatal(err)
	}
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	if fake.count("agent", "start") != 1 {
		t.Fatalf("agent start calls = %d", fake.count("agent", "start"))
	}
	if _, err := os.Stat(filepath.Join(wt, "marker")); err == nil {
		t.Error("a fallback relaunch ran the setup again")
	}
}
