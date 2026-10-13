package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/config"
	"github.com/khuebt/herdr-runner-kit/internal/herdr"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// fakeHerdr stands in for the herdr CLI. Handlers are keyed by the first two
// arguments, such as "agent start"; unhandled commands succeed with "{}".
type fakeHerdr struct {
	mu       sync.Mutex
	calls    [][]string
	handlers map[string]func(args []string) (string, error)
}

func newFakeHerdr() *fakeHerdr {
	f := &fakeHerdr{handlers: map[string]func([]string) (string, error){}}
	f.on("tab create", ok(`{"result":{"root_pane":{"pane_id":"w1:p9","tab_id":"w1:t9","workspace_id":"w1"}}}`))
	f.on("worktree create", ok(`{"result":{"root_pane":{"pane_id":"w2:p1","tab_id":"w2:t1","workspace_id":"w2"},"worktree":{"path":"/wt/t1","branch":"hrk/t1-x"}}}`))
	f.on("workspace list", ok(`{"result":{"workspaces":[]}}`))
	f.on("workspace get", ok(`{"result":{"workspace":{"workspace_id":"w2","worktree":{"checkout_path":"/wt/t1","is_linked_worktree":true}}}}`))
	f.on("agent start", ok(`{"result":{"agent":{"agent":"claude","agent_status":"idle"}}}`))
	f.on("agent wait", ok(`{"result":{"agent":{"agent_status":"working"}}}`))
	f.on("pane read", ok("❯ \n"))
	return f
}

func ok(out string) func([]string) (string, error) {
	return func([]string) (string, error) { return out, nil }
}

func herdrErr(code string) func([]string) (string, error) {
	return func([]string) (string, error) { return "", herdrError(code) }
}

type herdrError string

func (e herdrError) Error() string { return string(e) }

func (f *fakeHerdr) on(cmd string, h func([]string) (string, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[cmd] = h
}

func (f *fakeHerdr) exec(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, args)
	h := f.handlers[strings.Join(args[:min(2, len(args))], " ")]
	f.mu.Unlock()
	if h == nil {
		return []byte("{}"), nil, nil
	}
	out, err := h(args)
	var he herdrError
	if errors.As(err, &he) {
		stderr := fmt.Sprintf(`{"error":{"code":%q,"message":"fake"}}`, string(he))
		return nil, []byte(stderr), errors.New("exit status 1")
	}
	return []byte(out), nil, err
}

// call returns the first recorded call starting with prefix.
func (f *fakeHerdr) call(prefix ...string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if len(c) >= len(prefix) && slices.Equal(c[:len(prefix)], prefix) {
			return c
		}
	}
	return nil
}

func (f *fakeHerdr) count(prefix ...string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if len(c) >= len(prefix) && slices.Equal(c[:len(prefix)], prefix) {
			n++
		}
	}
	return n
}

type env map[string]string

func (e env) get(k string) string { return e[k] }

func newTestRunner(t *testing.T) (*Runner, *fakeHerdr, env) {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", t.TempDir())
	fake := newFakeHerdr()
	e := env{"HERDR_ENV": "1", "HERDR_WORKSPACE_ID": "w1", "CLAUDE_CONFIG_DIR": t.TempDir()}
	r := &Runner{
		Store:         store,
		Config:        config.Default(),
		Herdr:         &herdr.Client{Bin: "herdr", Exec: fake.exec},
		Bin:           "/usr/local/bin/herdr-runner-kit",
		Getenv:        e.get,
		StartLauncher: func(string) (int, error) { return 4242, nil },
		StartAfter:    func(string) error { return nil },
		Sleep:         func(time.Duration) {},
	}
	return r, fake, e
}

func plainDir(t *testing.T) string {
	t.Helper()
	return t.TempDir() // not a git repository
}

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	resolved, _ := filepath.EvalSymlinks(dir)
	return resolved
}

func events(t *testing.T, r *Runner, id string) []string {
	t.Helper()
	all, _, err := r.Store.ReadEventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, ev := range all {
		if ev.Task == id {
			types = append(types, ev.Type)
		}
	}
	return types
}

func TestSpawnRefusedInsideRunner(t *testing.T) {
	r, _, e := newTestRunner(t)
	e["HRK_TASK"] = "t1"
	_, err := r.Spawn(context.Background(), SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	if !errors.Is(err, ErrInsideRunner) || err.Error() != "runners cannot spawn runners" {
		t.Fatalf("err = %v", err)
	}
}

func TestSpawnNeedsHerdr(t *testing.T) {
	r, _, e := newTestRunner(t)
	delete(e, "HERDR_ENV")
	if _, err := r.Spawn(context.Background(), SpawnOptions{Prompt: "x", Cwd: plainDir(t)}); err == nil {
		t.Fatal("spawn outside herdr succeeded")
	}
}

func TestSpawnStartsLauncherQuickly(t *testing.T) {
	r, _, _ := newTestRunner(t)
	dir := plainDir(t)
	start := time.Now()
	tk, err := r.Spawn(context.Background(), SpawnOptions{Kind: "research", Prompt: "Compare two queues", Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Errorf("spawn took %v, budget is 300ms", d)
	}
	if tk.Status != task.Starting || tk.LauncherPID != 4242 || tk.LaunchDeadline.IsZero() {
		t.Errorf("task = %+v", tk)
	}
	if tk.Agent != "claude:claude-opus-5-5:high" || tk.UseWorktree || tk.Owner != dir || tk.MainWorkspace != "w1" {
		t.Errorf("defaults not applied: %+v", tk)
	}
	if tk.Title != "Compare two queues" {
		t.Errorf("title = %q", tk.Title)
	}
	if got := events(t, r, tk.ID); !slices.Equal(got, []string{"spawned"}) {
		t.Errorf("events = %v", got)
	}
}

func TestSpawnWorktreeInGitRepo(t *testing.T) {
	r, _, _ := newTestRunner(t)
	repo := gitRepo(t)
	tk, err := r.Spawn(context.Background(), SpawnOptions{Kind: "code", Prompt: "Fix the login bug", Cwd: repo})
	if err != nil {
		t.Fatal(err)
	}
	if !tk.UseWorktree || tk.RepoRoot != repo || tk.Worktree == nil || tk.Worktree.Branch != "hrk/t1-fix-the-login-bug" {
		t.Fatalf("task = %+v worktree=%+v", tk, tk.Worktree)
	}
	no := false
	tk, err = r.Spawn(context.Background(), SpawnOptions{Kind: "code", Prompt: "x", Cwd: repo, Worktree: &no})
	if err != nil || tk.UseWorktree || tk.Worktree != nil {
		t.Fatalf("--no-worktree: %+v %v", tk, err)
	}
	yes := true
	if _, err := r.Spawn(context.Background(), SpawnOptions{Prompt: "x", Cwd: plainDir(t), Worktree: &yes}); err == nil {
		t.Fatal("--worktree outside git succeeded")
	}
}

func TestSpawnNamesTheBranch(t *testing.T) {
	r, _, _ := newTestRunner(t)
	repo := gitRepo(t)
	tk := spawnStarting(t, r, SpawnOptions{Kind: "code", Prompt: "x", Cwd: repo, Branch: "fix/CSD-11-paid-leave"})
	if tk.Worktree == nil || tk.Worktree.Branch != "fix/CSD-11-paid-leave" {
		t.Errorf("worktree = %+v", tk.Worktree)
	}
	for _, o := range []SpawnOptions{
		{Kind: "code", Prompt: "x", Cwd: repo, Branch: "bad..name"},
		{Kind: "fix", Prompt: "x", Cwd: repo, Branch: "fix/no-worktree"},
	} {
		if _, err := r.Spawn(context.Background(), o); err == nil {
			t.Errorf("Spawn(%+v) succeeded", o)
		}
	}
}

func TestSpawnRefusesExistingBranch(t *testing.T) {
	r, _, _ := newTestRunner(t)
	repo := gitRepo(t)
	if out, err := exec.Command("git", "-C", repo, "branch", "hrk/t1-x").CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	_, err := r.Spawn(context.Background(), SpawnOptions{Kind: "code", Prompt: "x", Cwd: repo})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v", err)
	}
	got, _ := r.Store.Get("t1")
	if got.Status != task.Failed {
		t.Errorf("status = %s, want failed", got.Status)
	}
}

func TestSpawnQueuedAndHeld(t *testing.T) {
	r, _, _ := newTestRunner(t)
	dir := plainDir(t)
	launches := 0
	r.StartLauncher = func(string) (int, error) { launches++; return 1, nil }

	parent, _ := r.Spawn(context.Background(), SpawnOptions{Prompt: "parent", Cwd: dir})
	child, err := r.Spawn(context.Background(), SpawnOptions{Prompt: "child", Cwd: dir, After: []string{"#" + parent.ID}})
	if err != nil || child.Status != task.Queued || !slices.Equal(child.After, []string{parent.ID}) {
		t.Fatalf("child = %+v, %v", child, err)
	}
	held, err := r.Spawn(context.Background(), SpawnOptions{Prompt: "held", Cwd: dir, Gate: task.GateHuman})
	if err != nil || held.Status != task.Held {
		t.Fatalf("held = %+v, %v", held, err)
	}
	if launches != 1 {
		t.Errorf("launched %d times, want only the parent", launches)
	}
	if got := events(t, r, held.ID); !slices.Equal(got, []string{"held"}) {
		t.Errorf("held events = %v", got)
	}

	// A done parent lets the child start at once; a failed one is refused.
	if _, _, err := r.Store.SetStatus(parent.ID, task.Done); err != nil {
		t.Fatal(err)
	}
	c2, err := r.Spawn(context.Background(), SpawnOptions{Prompt: "after done", Cwd: dir, After: []string{parent.ID}})
	if err != nil || c2.Status != task.Starting {
		t.Fatalf("after done parent: %+v, %v", c2, err)
	}
	if _, _, err := r.Store.SetStatus(held.ID, task.Failed); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Spawn(context.Background(), SpawnOptions{Prompt: "x", Cwd: dir, After: []string{held.ID}}); err == nil {
		t.Fatal("spawn after a failed task succeeded")
	}
}

func TestSpawnValidation(t *testing.T) {
	r, _, _ := newTestRunner(t)
	dir := plainDir(t)
	for _, o := range []SpawnOptions{
		{Cwd: dir},
		{Prompt: "x", Cwd: dir, Kind: "nope"},
		{Prompt: "x", Cwd: dir, Agent: "gemini"},
		{Prompt: "x", Cwd: dir, Gate: "maybe"},
		{Prompt: "x", Cwd: dir, Base: "main"},
		{Prompt: "x", Cwd: filepath.Join(dir, "missing")},
		{Prompt: "x", Cwd: dir, After: []string{"t99"}},
	} {
		if _, err := r.Spawn(context.Background(), o); err == nil {
			t.Errorf("Spawn(%+v) succeeded", o)
		}
	}
}

func TestSpawnPicksClaudeConfig(t *testing.T) {
	r, fake, e := newTestRunner(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, d := range []string{".claude", ".claude2"} {
		if err := os.Mkdir(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	dir := plainDir(t)
	cases := map[string]string{
		"":                 e["CLAUDE_CONFIG_DIR"], // the main session's
		"claude2":          filepath.Join(home, ".claude2"),
		"~/.claude2":       filepath.Join(home, ".claude2"),
		home + "/.claude2": filepath.Join(home, ".claude2"),
		"claude":           "", // Claude's default, which has no CLAUDE_CONFIG_DIR
	}
	for in, want := range cases {
		tk := spawnStarting(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: dir, ClaudeConfig: in})
		if tk.ClaudeConfigDir != want {
			t.Errorf("--claude-config %q = %q, want %q", in, tk.ClaudeConfigDir, want)
		}
	}
	for _, o := range []SpawnOptions{
		{Kind: "research", Prompt: "x", Cwd: dir, ClaudeConfig: "claude3"},
		{Kind: "code", Prompt: "x", Cwd: dir, ClaudeConfig: "claude2"},
	} {
		if _, err := r.Spawn(context.Background(), o); err == nil {
			t.Errorf("Spawn(%+v) succeeded", o)
		}
	}

	// A runner on the default account gets no CLAUDE_CONFIG_DIR, and its
	// trust goes to ~/.claude.json, not to the main session's folder, which
	// the launcher inherits.
	t.Setenv("CLAUDE_CONFIG_DIR", e["CLAUDE_CONFIG_DIR"])
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"projects":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tk := spawnStarting(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: dir, ClaudeConfig: "claude"})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	if tab := strings.Join(fake.call("tab", "create"), " "); strings.Contains(tab, "CLAUDE_CONFIG_DIR") {
		t.Errorf("tab create sets CLAUDE_CONFIG_DIR: %s", tab)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".claude.json")); !strings.Contains(string(b), dir) {
		t.Errorf("~/.claude.json does not trust %s: %s", dir, b)
	}
}

func TestSpawnNormalisesDefaultClaudeConfig(t *testing.T) {
	r, _, e := newTestRunner(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, inherited := range []string{filepath.Join(home, ".claude"), filepath.Join(home, ".claude") + "/", "~/.claude"} {
		e["CLAUDE_CONFIG_DIR"] = inherited
		tk := spawnStarting(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: plainDir(t)})
		if tk.ClaudeConfigDir != "" {
			t.Errorf("inherited CLAUDE_CONFIG_DIR=%s stored as %q, want \"\"", inherited, tk.ClaudeConfigDir)
		}
	}
}

func spawnStarting(t *testing.T, r *Runner, o SpawnOptions) *task.Task {
	t.Helper()
	tk, err := r.Spawn(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestLaunchClaudeTab(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	dir := plainDir(t)
	tk := spawnStarting(t, r, SpawnOptions{Kind: "research", Skill: "ak:research", Prompt: "queues", Cwd: dir})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}

	tab := fake.call("tab", "create")
	for _, want := range []string{"--workspace w1", "--cwd " + dir, "--env HRK_TASK=t1", "--env TELEGRAM_BOT_TOKEN=", "--no-focus"} {
		if !strings.Contains(strings.Join(tab, " "), want) {
			t.Errorf("tab create %q lacks %q", tab, want)
		}
	}
	start := fake.call("agent", "start")
	joined := strings.Join(start, " ")
	if !strings.Contains(joined, "hrk-t1 --kind claude --pane w1:p9") ||
		!strings.Contains(joined, "--settings "+r.Store.TaskDir("t1")+"/claude-settings.json") ||
		!strings.Contains(joined, "--append-system-prompt-file "+r.Store.TaskDir("t1")+"/contract.md") {
		t.Errorf("agent start = %q", start)
	}
	if strings.Contains(joined, "queues") {
		t.Error("the prompt was passed as a launch argument")
	}
	prompt := fake.call("agent", "prompt")
	if len(prompt) != 4 || prompt[2] != "w1:p9" || prompt[3] != "/ak:research queues" {
		t.Errorf("agent prompt = %q", prompt)
	}
	contract, _ := os.ReadFile(filepath.Join(r.Store.TaskDir("t1"), "contract.md"))
	if !strings.Contains(string(contract), "/usr/local/bin/herdr-runner-kit report done") {
		t.Errorf("contract = %s", contract)
	}

	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Running || got.Herdr.PaneID != "w1:p9" || got.Herdr.TabID != "w1:t9" || got.Herdr.AgentName != "hrk-t1" {
		t.Errorf("task = %+v", got)
	}
	if ev := events(t, r, tk.ID); !slices.Equal(ev, []string{"spawned", "claude-trust-skipped", "started", "prompted"}) {
		t.Errorf("events = %v", ev)
	}
}

func TestLaunchCodexWorktree(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	repo := gitRepo(t)
	fake.on("agent start", ok(`{"result":{"agent":{"agent":"codex","agent_status":"idle"}}}`))
	screens := []string{"  Hooks need review\n  24 hooks are new or changed.", "› Ask Codex to do anything"}
	fake.on("pane read", func([]string) (string, error) {
		s := screens[0]
		if len(screens) > 1 {
			screens = screens[1:]
		}
		return s, nil
	})
	tk := spawnStarting(t, r, SpawnOptions{Kind: "code", Skill: "ak:cook", Prompt: "plan.md", Cwd: repo})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}

	wt := fake.call("worktree", "create")
	if !strings.Contains(strings.Join(wt, " "), "--cwd "+repo+" --branch hrk/t1-plan-md --base HEAD") {
		t.Errorf("worktree create = %q", wt)
	}
	run := fake.call("pane", "run")
	if len(run) != 4 || run[2] != "w2:p1" || !strings.HasPrefix(run[3], "export HRK_TASK=t1 HRK_STATE_DIR="+r.Store.Dir()+" TELEGRAM_BOT_TOKEN='' ") {
		t.Errorf("pane run = %q", run)
	}
	start := strings.Join(fake.call("agent", "start"), " ")
	if !strings.Contains(start, "--kind codex --pane w2:p1") || !strings.Contains(start, `notify=["/usr/local/bin/herdr-runner-kit","__turn-end","--harness","codex"]`) {
		t.Errorf("agent start = %q", start)
	}
	if keys := fake.call("agent", "send-keys"); !slices.Equal(keys, []string{"agent", "send-keys", "w2:p1", "esc"}) {
		t.Errorf("send-keys = %q", keys)
	}
	prompt := fake.call("agent", "prompt")
	if !strings.HasPrefix(prompt[3], "$ak-cook plan.md\n\nYou are runner t1") {
		t.Errorf("codex prompt = %q", prompt[3])
	}
	cfg, _ := os.ReadFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"))
	if !strings.Contains(string(cfg), `[projects."`+repo+`"]`) {
		t.Errorf("codex trust not added: %s", cfg)
	}
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Running || got.Worktree.Path != "/wt/t1" {
		t.Errorf("task = %+v", got)
	}
	ev := events(t, r, tk.ID)
	for _, want := range []string{"codex-trusted", "codex-hooks-review-skipped", "started", "prompted"} {
		if !slices.Contains(ev, want) {
			t.Errorf("events %v lack %s", ev, want)
		}
	}
}

func TestLaunchWaitsForHumanAtTrustDialog(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	fake.on("agent start", herdrErr(herdr.CodeAgentNotReady))
	screens := []string{" Quick safety check: Is this a project you created or one you trust?", "❯ "}
	fake.on("pane read", func([]string) (string, error) {
		s := screens[0]
		if len(screens) > 1 {
			screens = screens[1:]
		}
		return s, nil
	})
	var statusDuringWait task.Status
	fake.on("agent wait", func(args []string) (string, error) {
		if slices.Contains(args, "idle") {
			got, _ := r.Store.Get("t1")
			statusDuringWait = got.Status
			return `{"result":{"agent":{"agent_status":"idle"}}}`, nil
		}
		return `{"result":{"agent":{"agent_status":"working"}}}`, nil
	})
	tk := spawnStarting(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	if statusDuringWait != task.NeedsHuman {
		t.Errorf("status while waiting = %s, want needs-human", statusDuringWait)
	}
	if fake.count("agent", "send-keys") != 0 {
		t.Error("pressed keys on a trust dialog")
	}
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Running || got.Error != "" {
		t.Errorf("after answer: %+v", got)
	}
	ev := events(t, r, tk.ID)
	if !slices.Equal(ev, []string{"spawned", "claude-trust-skipped", "needs-human", "started", "prompted"}) {
		t.Errorf("events = %v", ev)
	}
}

// screensThen serves blank screens n times, then the given screens in
// order, repeating the last one.
func screensThen(n int, screens ...string) func([]string) (string, error) {
	var mu sync.Mutex
	reads := 0
	return func([]string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		reads++
		if reads <= n {
			return "", nil
		}
		return screens[min(reads-n, len(screens))-1], nil
	}
}

func TestLaunchFindsALateTrustDialog(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	fake.on("agent start", herdrErr(herdr.CodeAgentNotReady))
	fake.on("pane read", screensThen(3, "│ Is this a project you created │\n│ or one you trust? │", "❯ "))
	var statusDuringWait task.Status
	fake.on("agent wait", func(args []string) (string, error) {
		if slices.Contains(args, "idle") && statusDuringWait == "" {
			got, _ := r.Store.Get("t1")
			statusDuringWait = got.Status
		}
		return `{"result":{"agent":{"agent_status":"idle"}}}`, nil
	})
	tk := spawnStarting(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	if statusDuringWait != task.NeedsHuman {
		t.Errorf("status while waiting = %s, want needs-human", statusDuringWait)
	}
	all, _, _ := r.Store.ReadEventsSince(0)
	for _, ev := range all {
		if ev.Type == "needs-human" && ev.Summary != "claude-workspace-trust" {
			t.Errorf("needs-human for %q, want claude-workspace-trust", ev.Summary)
		}
	}
	if ev := events(t, r, tk.ID); !slices.Contains(ev, "needs-human") {
		t.Errorf("events = %v", ev)
	}
}

func TestLaunchUnknownDialogAfterFallback(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	fake.on("agent start", herdrErr(herdr.CodeAgentNotReady))
	fake.on("pane read", ok("  Some new question?\n  1. Yes  2. No"))
	idleWaits := 0
	fake.on("agent wait", func(args []string) (string, error) {
		if !slices.Contains(args, "idle") {
			return `{"result":{"agent":{"agent_status":"working"}}}`, nil
		}
		idleWaits++
		if idleWaits == 1 {
			return "", herdrError(herdr.CodeTimeout) // still not ready
		}
		return `{"result":{"agent":{"agent_status":"idle"}}}`, nil // the user answered
	})
	tk := spawnStarting(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	if n := fake.count("pane", "read"); n < 21 {
		t.Errorf("pane read %d times before the unknown dialog, want at least 21", n)
	}
	if idleWaits < 2 {
		t.Errorf("idle waits = %d, want the fallback wait and the human wait", idleWaits)
	}
	all, _, _ := r.Store.ReadEventsSince(0)
	var question string
	for _, ev := range all {
		if ev.Type == "needs-human" {
			question = ev.Question
		}
	}
	if !strings.Contains(question, "shows the unknown startup dialog") || strings.Contains(question, "dialog dialog") {
		t.Errorf("question = %q", question)
	}
}

func TestLaunchNotReadyThenIdleIsNoDialog(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	fake.on("agent start", herdrErr(herdr.CodeAgentNotReady))
	fake.on("agent wait", ok(`{"result":{"agent":{"agent_status":"idle"}}}`))
	tk := spawnStarting(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	if ev := events(t, r, tk.ID); slices.Contains(ev, "needs-human") {
		t.Errorf("an agent that turned idle was marked needs-human: %v", ev)
	}
}

func TestLaunchNeverPromptsIntoADialog(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	// Ready at start; the trust dialog opens on the check before the prompt.
	fake.on("pane read", screensThen(1, " Is this a project you created or one you trust?", "❯ "))
	promptedDuringDialog := false
	fake.on("agent prompt", func([]string) (string, error) {
		if got, _ := r.Store.Get("t1"); got.Status == task.NeedsHuman {
			promptedDuringDialog = true
		}
		return "{}", nil
	})
	var callsAtAnswer int
	fake.on("agent wait", func(args []string) (string, error) {
		if slices.Contains(args, "idle") {
			callsAtAnswer = fake.count("agent", "prompt")
			return `{"result":{"agent":{"agent_status":"idle"}}}`, nil
		}
		return `{"result":{"agent":{"agent_status":"working"}}}`, nil
	})
	tk := spawnStarting(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	if promptedDuringDialog || callsAtAnswer != 0 {
		t.Errorf("prompt sent before the dialog was answered (%d prompts)", callsAtAnswer)
	}
	if fake.count("agent", "prompt") != 1 || fake.count("agent", "send-keys") != 0 {
		t.Errorf("prompts = %d, send-keys = %d", fake.count("agent", "prompt"), fake.count("agent", "send-keys"))
	}
	if ev := events(t, r, tk.ID); !slices.Equal(ev, []string{"spawned", "claude-trust-skipped", "needs-human", "started", "prompted"}) {
		t.Errorf("events = %v", ev)
	}
}

func TestLaunchFailsWhenAgentExitsAtDialog(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	fake.on("agent start", herdrErr(herdr.CodeAgentNotReady))
	fake.on("pane read", ok("  Trust this folder? Codex can read"))
	fake.on("agent wait", herdrErr(herdr.CodeAgentNotRunning))
	tk := spawnStarting(t, r, SpawnOptions{Agent: "codex", Prompt: "x", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err == nil {
		t.Fatal("want error")
	}
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Failed || !strings.Contains(got.Error, "exited at the codex-trust dialog") {
		t.Errorf("task = %+v", got)
	}
}

func TestLaunchTimeoutWithErrorLine(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	fake.on("agent start", herdrErr(herdr.CodeTimeout))
	fake.on("pane read", ok("➜ codex --dangerously\nError: account/read failed during TUI bootstrap\n➜ "))
	tk := spawnStarting(t, r, SpawnOptions{Agent: "codex", Prompt: "x", Cwd: plainDir(t)})
	_ = r.Launch(context.Background(), tk.ID)
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Failed || !strings.Contains(got.Error, "Error: account/read failed") {
		t.Errorf("task = %+v", got)
	}
	if ev := events(t, r, tk.ID); ev[len(ev)-1] != "failed" {
		t.Errorf("events = %v", ev)
	}
}

func TestLaunchFailureIsNews(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	fake.on("tab create", func([]string) (string, error) { return "", errors.New("herdr: server not running") })
	tk := spawnStarting(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err == nil {
		t.Fatal("want error")
	}
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Failed || got.Error == "" {
		t.Errorf("task = %+v", got)
	}
	if ev := events(t, r, tk.ID); ev[len(ev)-1] != "failed" {
		t.Errorf("events = %v", ev)
	}
}

func TestEarlyDoneStaysDone(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	// The runner reports done while the launcher is still sending the prompt.
	fake.on("agent prompt", func([]string) (string, error) {
		_, _, err := r.Store.SetStatus("t1", task.Done)
		return "{}", err
	})
	tk := spawnStarting(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Done {
		t.Errorf("status = %s, want done", got.Status)
	}
}

func TestStopQuitsAgentAndClosesTab(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := spawnStarting(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	fake.on("agent get", ok(`{"result":{"agent":{"agent":"claude","agent_status":"working"}}}`))
	// The agent exits on Ctrl+C, and a running wait's backstop sees it go
	// at once; the task must still end as stopped, not failed.
	fake.on("agent wait", func(args []string) (string, error) {
		r.fail(tk.ID, "the agent exited without reporting")
		return herdrErr(herdr.CodeAgentNotRunning)(args)
	})
	got, err := r.Stop(context.Background(), tk.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.Stopped || !got.Herdr.Closed {
		t.Errorf("task = %+v", got)
	}
	if keys := fake.call("agent", "send-keys"); !slices.Equal(keys, []string{"agent", "send-keys", "w1:p9", "ctrl+c", "ctrl+c"}) {
		t.Errorf("send-keys = %q", keys)
	}
	if c := fake.call("tab", "close"); !slices.Equal(c, []string{"tab", "close", "w1:t9"}) {
		t.Errorf("tab close = %q", c)
	}
	if _, err := r.Send(context.Background(), tk.ID, "more"); err == nil {
		t.Error("send to a closed runner succeeded")
	}
}

func TestSendReopensADoneTask(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk := spawnStarting(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Report(tk.ID, ReportOptions{Status: task.Done, Summary: "first"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Send(context.Background(), tk.ID, "now run the full check"); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Running || !got.FinishedAt.IsZero() {
		t.Fatalf("after send: status %s, finished %v", got.Status, got.FinishedAt)
	}
	if got, err := r.Report(tk.ID, ReportOptions{Status: task.Done, Summary: "check passed"}); err != nil || got.Summary != "check passed" {
		t.Errorf("second report = %+v, %v", got, err)
	}
}

func TestStopKeepsWorktreeUnlessAsked(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	repo := gitRepo(t)
	fake.on("agent get", ok(`{"result":{"agent":{"agent_status":"idle"}}}`)) // shell only
	tk := spawnStarting(t, r, SpawnOptions{Kind: "code", Prompt: "x", Cwd: repo})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Stop(context.Background(), tk.ID, false); err != nil {
		t.Fatal(err)
	}
	if fake.count("tab", "close") != 0 || fake.count("worktree", "remove") != 0 {
		t.Error("worktree runner was closed or removed without --remove-worktree")
	}
	// A later stop --remove-worktree on the stopped task still removes it.
	got, err := r.Stop(context.Background(), tk.ID, true)
	if err != nil || !got.Worktree.Removed || fake.count("worktree", "remove") != 1 {
		t.Fatalf("late removal: %+v, %v", got.Worktree, err)
	}
	if _, err := r.Stop(context.Background(), tk.ID, true); err != nil || fake.count("worktree", "remove") != 1 {
		t.Errorf("removed twice: %v", err)
	}

	tk2 := spawnStarting(t, r, SpawnOptions{Kind: "code", Prompt: "y", Cwd: repo})
	if err := r.Launch(context.Background(), tk2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Stop(context.Background(), tk2.ID, true); err != nil {
		t.Fatal(err)
	}
	if rm := fake.call("worktree", "remove"); !slices.Equal(rm, []string{"worktree", "remove", "--workspace", "w2"}) {
		t.Errorf("worktree remove = %q (must not force)", rm)
	}
}

func TestGCClosesOnlyOldFinishedRunners(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	dir := plainDir(t)
	fake.on("agent get", herdrErr(herdr.CodeAgentNotFound))
	for range 3 {
		tk := spawnStarting(t, r, SpawnOptions{Prompt: "x", Cwd: dir})
		if err := r.Launch(context.Background(), tk.ID); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-time.Hour).UTC()
	if _, _, err := r.Store.Transition("t1", task.Done, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.Update("t1", func(t *task.Task) error { t.FinishedAt = old; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Store.SetStatus("t2", task.Done); err != nil { // finished just now
		t.Fatal(err)
	}
	closed, err := r.GC(context.Background(), "", GCAge, false)
	if err != nil || !slices.Equal(closed, []string{"t1"}) {
		t.Fatalf("closed = %v, %v", closed, err)
	}
	if closed, _ := r.GC(context.Background(), "", GCAge, false); len(closed) != 0 {
		t.Errorf("second gc closed %v", closed)
	}
}

func TestSlugAndTitle(t *testing.T) {
	if got := Slug("Fix the Login bug — now!", 32); got != "fix-the-login-bug-now" {
		t.Errorf("Slug = %q", got)
	}
	if got := Slug("Sửa lỗi đăng nhập", 32); got != "sua-loi-dang-nhap" {
		t.Errorf("Slug(non-ASCII) = %q", got)
	}
	if got := Slug(strings.Repeat("abc ", 20), 10); len(got) > 10 || strings.HasSuffix(got, "-") {
		t.Errorf("Slug cut = %q", got)
	}
	if got := BranchName("hrk/", "t3", "!!!"); got != "hrk/t3" {
		t.Errorf("BranchName = %q", got)
	}
	long := "Investigate why the background wait command misses events written during startup\nmore"
	if got := TitleFrom(long); len(got) > 48 || !strings.HasPrefix(got, "Investigate why") {
		t.Errorf("TitleFrom = %q", got)
	}
}

func TestGCLeavesARunnerSomeoneStillUses(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := spawnStarting(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Store.Transition(tk.ID, task.Done, nil, func(t *task.Task) { t.FinishedAt = time.Now().Add(-time.Hour) }); err != nil {
		t.Fatal(err)
	}
	// The user kept typing in the tab after the report.
	fake.on("agent get", ok(`{"result":{"agent":{"agent":"claude","agent_status":"working"}}}`))
	if closed, err := r.GC(context.Background(), "", GCAge, false); err != nil || len(closed) != 0 || fake.count("agent", "send-keys") != 0 {
		t.Fatalf("closed %v, %v, send-keys %d", closed, err, fake.count("agent", "send-keys"))
	}
	fake.on("agent get", herdrErr(herdr.CodeAgentNotFound))
	if closed, err := r.GC(context.Background(), "", GCAge, false); err != nil || !slices.Equal(closed, []string{tk.ID}) {
		t.Fatalf("after the work: closed %v, %v", closed, err)
	}
}

// finishedRunner launches a plain runner whose task finished an hour ago, so
// gc may close it. Its agent is gone from herdr unless a test says otherwise.
func finishedRunner(t *testing.T, r *Runner, fake *fakeHerdr) *task.Task {
	t.Helper()
	fake.on("agent get", ok(`{"result":{"agent":{"agent_status":"idle"}}}`)) // shell only
	tk := spawnStarting(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	finishAgo(t, r, tk.ID, time.Hour)
	got, err := r.Store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// finishAgo sets the task done, finished d ago, with its tab open.
func finishAgo(t *testing.T, r *Runner, id string, d time.Duration) {
	t.Helper()
	if _, err := r.Store.Update(id, func(t *task.Task) error {
		t.Status, t.FinishedAt = task.Done, time.Now().Add(-d).UTC()
		t.Herdr.Closed, t.Herdr.ClosingSince = false, time.Time{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func gcCheck() closeCheck {
	return closeCheck{idle: true, cutoff: time.Now().Add(-GCAge), keep: true}
}

func TestGCSkipsATaskReopenedBySend(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	listed := finishedRunner(t, r, fake)
	// Send reopens the task after gc listed it as finished.
	if _, err := r.Send(context.Background(), listed.ID, "one more thing"); err != nil {
		t.Fatal(err)
	}
	if err := r.closeRunner(context.Background(), listed, false, gcCheck()); !errors.Is(err, errReopened) {
		t.Fatalf("close of a reopened runner = %v", err)
	}
	if closed, err := r.GC(context.Background(), "", GCAge, false); err != nil || len(closed) != 0 {
		t.Fatalf("gc closed %v, %v", closed, err)
	}
	got, _ := r.Store.Get(listed.ID)
	if got.Status != task.Running || got.Herdr.Closed || !got.Herdr.ClosingSince.IsZero() || fake.count("agent", "send-keys") != 0 {
		t.Errorf("task = %s closed %v claim %v; send-keys %d", got.Status, got.Herdr.Closed, got.Herdr.ClosingSince, fake.count("agent", "send-keys"))
	}
}

func TestGCRechecksAgeUnderLock(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	listed := finishedRunner(t, r, fake)
	// Reopened and finished again after gc listed it.
	if _, err := r.Send(context.Background(), listed.ID, "again"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Report(listed.ID, ReportOptions{Status: task.Done, Summary: "again"}); err != nil {
		t.Fatal(err)
	}
	if err := r.closeRunner(context.Background(), listed, false, gcCheck()); !errors.Is(err, errReopened) {
		t.Fatalf("close of a runner that finished again = %v", err)
	}
	if got, _ := r.Store.Get(listed.ID); got.Herdr.Closed || got.Status != task.Done {
		t.Errorf("task = %s closed %v", got.Status, got.Herdr.Closed)
	}
}

func TestSendRefusesAClosingRunner(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := finishedRunner(t, r, fake)
	prompts := fake.count("agent", "prompt")
	var sendErr error
	fake.on("agent get", func([]string) (string, error) {
		if sendErr == nil {
			_, sendErr = r.Send(context.Background(), tk.ID, "late")
		}
		return `{"result":{"agent":{"agent_status":"idle"}}}`, nil
	})
	if closed, err := r.GC(context.Background(), "", GCAge, false); err != nil || !slices.Equal(closed, []string{tk.ID}) {
		t.Fatalf("gc closed %v, %v", closed, err)
	}
	if sendErr == nil || !strings.Contains(sendErr.Error(), "tab is closing") {
		t.Errorf("send during the close = %v", sendErr)
	}
	if fake.count("agent", "prompt") != prompts {
		t.Error("a closing runner got the prompt")
	}
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Done || !got.Herdr.Closed || !got.Herdr.ClosingSince.IsZero() {
		t.Errorf("task = %s closed %v claim %v", got.Status, got.Herdr.Closed, got.Herdr.ClosingSince)
	}
}

func TestSendDuringPromptSeesTheClaim(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := finishedRunner(t, r, fake)
	// gc claims the runner while the prompt is being typed.
	fake.on("agent prompt", func([]string) (string, error) {
		_, err := r.claimClose(tk.ID, gcCheck())
		return "{}", err
	})
	if _, err := r.Send(context.Background(), tk.ID, "late"); err == nil || !strings.Contains(err.Error(), "was closing while the prompt was sent") {
		t.Fatalf("send = %v", err)
	}
	if got, _ := r.Store.Get(tk.ID); got.Status != task.Done {
		t.Errorf("status = %s, want done", got.Status)
	}
}

func TestStaleClaimExpires(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := finishedRunner(t, r, fake)
	claim := func(ago time.Duration) {
		t.Helper()
		finishAgo(t, r, tk.ID, time.Hour)
		if _, err := r.Store.Update(tk.ID, func(t *task.Task) error {
			t.Herdr.ClosingSince = time.Now().Add(-ago).UTC()
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	claim(10 * time.Second)
	if _, err := r.Send(context.Background(), tk.ID, "x"); err == nil {
		t.Error("send got past a live claim")
	}
	if closed, err := r.GC(context.Background(), "", GCAge, false); err != nil || len(closed) != 0 {
		t.Errorf("gc got past a live claim: %v, %v", closed, err)
	}

	claim(2 * time.Minute)
	if _, err := r.Send(context.Background(), tk.ID, "x"); err != nil {
		t.Errorf("send refused after the claim expired: %v", err)
	}
	claim(2 * time.Minute)
	if closed, err := r.GC(context.Background(), "", GCAge, false); err != nil || !slices.Equal(closed, []string{tk.ID}) {
		t.Errorf("gc did not reclaim an expired claim: %v, %v", closed, err)
	}
}

func TestSendPromptFailureKeepsDone(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := finishedRunner(t, r, fake)
	fake.on("agent prompt", herdrErr("pane_busy"))
	if _, err := r.Send(context.Background(), tk.ID, "x"); err == nil {
		t.Fatal("send succeeded")
	}
	// The backstop sees the idle agent go; the task must stay done.
	fake.on("agent wait", herdrErr(herdr.CodeAgentNotRunning))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r.WatchAgent(ctx, tk.ID)
	if got, _ := r.Store.Get(tk.ID); got.Status != task.Done {
		t.Errorf("status = %s, want done", got.Status)
	}
}

func TestSendReopensAFailedTask(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := finishedRunner(t, r, fake)
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error {
		t.Status, t.Error = task.Failed, "tests failed"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Send(context.Background(), tk.ID, "fix the tests"); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Running || got.Error != "" || !got.FinishedAt.IsZero() {
		t.Errorf("after send: %s, error %q, finished %v", got.Status, got.Error, got.FinishedAt)
	}
}

func TestCloseFailureClearsClaim(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := finishedRunner(t, r, fake)
	// The agent ignores Ctrl+C.
	fake.on("agent get", ok(`{"result":{"agent":{"agent":"claude","agent_status":"idle"}}}`))
	if _, err := r.Stop(context.Background(), tk.ID, false); err == nil || !strings.Contains(err.Error(), "did not exit") {
		t.Fatalf("stop = %v", err)
	}
	got, _ := r.Store.Get(tk.ID)
	if got.Herdr.Closed || !got.Herdr.ClosingSince.IsZero() {
		t.Errorf("closed %v claim %v", got.Herdr.Closed, got.Herdr.ClosingSince)
	}
	if _, err := r.Send(context.Background(), tk.ID, "x"); err != nil {
		t.Errorf("send after a failed close: %v", err)
	}
}

// Send and gc race on one finished runner many times: one of them wins
// each time, and a runner is never left running with its tab closed.
func TestSendAndGCNeverBothAct(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := finishedRunner(t, r, fake)
	for i := range 30 {
		finishAgo(t, r, tk.ID, time.Hour)
		var wg sync.WaitGroup
		var sendErr, gcErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, sendErr = r.Send(context.Background(), tk.ID, "x") }()
		go func() { defer wg.Done(); _, gcErr = r.GC(context.Background(), "", GCAge, false) }()
		wg.Wait()
		if gcErr != nil {
			t.Fatalf("run %d: gc: %v", i, gcErr)
		}
		got, _ := r.Store.Get(tk.ID)
		switch {
		case !got.Herdr.ClosingSince.IsZero():
			t.Fatalf("run %d: claim left behind", i)
		case got.Herdr.Closed && got.Status != task.Done:
			t.Fatalf("run %d: %s with a closed tab", i, got.Status)
		case sendErr == nil && (got.Herdr.Closed || got.Status != task.Running):
			t.Fatalf("run %d: send succeeded, task %s closed %v", i, got.Status, got.Herdr.Closed)
		case sendErr != nil && got.Status != task.Done:
			t.Fatalf("run %d: send failed (%v), task %s", i, sendErr, got.Status)
		}
	}
}

func TestKeepStopsGC(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := finishedRunner(t, r, fake)
	if _, err := r.Keep(context.Background(), tk.ID, true); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Store.Get(tk.ID)
	if at, ok := CloseAt(got); ok {
		t.Errorf("kept runner closes at %v", at)
	}
	if _, ok := NextGC([]*task.Task{got}, ""); ok {
		t.Error("NextGC counts a kept runner")
	}
	if closed, err := r.GC(context.Background(), "", GCAge, false); err != nil || len(closed) != 0 {
		t.Fatalf("gc closed a kept runner: %v, %v", closed, err)
	}
	// A stale list that still shows it unkept does not close it either.
	if err := r.closeRunner(context.Background(), tk, false, gcCheck()); !errors.Is(err, errReopened) {
		t.Errorf("close of a kept runner = %v", err)
	}
	if got, err := r.Stop(context.Background(), tk.ID, false); err != nil || !got.Herdr.Closed {
		t.Fatalf("stop of a kept runner: %+v, %v", got, err)
	}
	if _, err := r.Keep(context.Background(), tk.ID, true); err == nil {
		t.Error("kept a closed runner")
	}
}

func TestKeepOffPastDueClosesNow(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := finishedRunner(t, r, fake)
	if _, err := r.Keep(context.Background(), tk.ID, true); err != nil {
		t.Fatal(err)
	}
	got, err := r.Keep(context.Background(), tk.ID, false)
	if err != nil || !got.Herdr.Closed || got.Keep {
		t.Fatalf("keep --off past due: %+v, %v", got, err)
	}

	// Not yet due: it stays open, with its close time back.
	fresh := finishedRunner(t, r, fake)
	finishAgo(t, r, fresh.ID, time.Minute)
	if _, err := r.Keep(context.Background(), fresh.ID, true); err != nil {
		t.Fatal(err)
	}
	got, err = r.Keep(context.Background(), fresh.ID, false)
	if at, ok := CloseAt(got); err != nil || got.Herdr.Closed || !ok || !at.Equal(got.FinishedAt.Add(GCAge)) {
		t.Errorf("keep --off before due: %+v, %v", got, err)
	}
}

func TestCloseAt(t *testing.T) {
	done := time.Date(2026, 10, 13, 15, 45, 0, 0, time.UTC)
	tk := &task.Task{Status: task.Done, FinishedAt: done, Herdr: task.Herdr{PaneID: "w1:p1"}}
	if at, ok := CloseAt(tk); !ok || !at.Equal(done.Add(GCAge)) {
		t.Errorf("CloseAt = %v, %v", at, ok)
	}
	for name, change := range map[string]func(*task.Task){
		"running": func(t *task.Task) { t.Status = task.Running },
		"closed":  func(t *task.Task) { t.Herdr.Closed = true },
		"kept":    func(t *task.Task) { t.Keep = true },
		"no pane": func(t *task.Task) { t.Herdr.PaneID = "" },
	} {
		c := *tk
		change(&c)
		if at, ok := CloseAt(&c); ok {
			t.Errorf("%s: closes at %v", name, at)
		}
	}
}

func TestEndClaimOnlyEndsItsOwnClaim(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := finishedRunner(t, r, fake)
	since, err := r.claimClose(tk.ID, gcCheck())
	if err != nil {
		t.Fatal(err)
	}
	// The claim expired mid-quit and another close took the runner.
	newer := since.Add(2 * time.Minute)
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error { t.Herdr.ClosingSince = newer; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := r.endClaim(tk.ID, since, false); err != nil {
		t.Fatalf("drop of a replaced claim = %v", err)
	}
	if got, _ := r.Store.Get(tk.ID); !got.Herdr.ClosingSince.Equal(newer) {
		t.Errorf("newer claim wiped: %v", got.Herdr.ClosingSince)
	}
	if err := r.endClaim(tk.ID, since, true); !errors.Is(err, errReopened) {
		t.Fatalf("close under a replaced claim = %v", err)
	}
	if got, _ := r.Store.Get(tk.ID); got.Herdr.Closed || !got.Herdr.ClosingSince.Equal(newer) {
		t.Errorf("closed %v claim %v", got.Herdr.Closed, got.Herdr.ClosingSince)
	}

	// The claim is still ours, but the task runs again.
	finishAgo(t, r, tk.ID, time.Hour)
	if since, err = r.claimClose(tk.ID, gcCheck()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error { t.Status = task.Running; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := r.endClaim(tk.ID, since, true); !errors.Is(err, errReopened) {
		t.Fatalf("close of a running task = %v", err)
	}
	if got, _ := r.Store.Get(tk.ID); got.Herdr.Closed || !got.Herdr.ClosingSince.IsZero() {
		t.Errorf("closed %v claim %v", got.Herdr.Closed, got.Herdr.ClosingSince)
	}
}

func TestCloseQuitIsBounded(t *testing.T) {
	if quitBudget >= closeClaimTTL {
		t.Fatalf("quitBudget %v must be shorter than closeClaimTTL %v", quitBudget, closeClaimTTL)
	}
	r, fake, _ := newTestRunner(t)
	tk := finishedRunner(t, r, fake)
	defer func(d time.Duration) { quitBudget = d }(quitBudget)
	quitBudget = 50 * time.Millisecond
	// herdr hangs while the agent quits.
	r.Herdr.Exec = func(ctx context.Context, bin string, args ...string) ([]byte, []byte, error) {
		if len(args) > 1 && args[0] == "agent" && args[1] == "get" {
			<-ctx.Done()
			return nil, nil, ctx.Err()
		}
		return fake.exec(ctx, bin, args...)
	}
	start := time.Now()
	if _, err := r.Stop(context.Background(), tk.ID, false); err == nil {
		t.Fatal("stop of a hung agent succeeded")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("stop took %v", d)
	}
	if got, _ := r.Store.Get(tk.ID); got.Herdr.Closed || !got.Herdr.ClosingSince.IsZero() {
		t.Errorf("closed %v claim %v", got.Herdr.Closed, got.Herdr.ClosingSince)
	}
}

func TestLaunchTimeoutBlockedIsADialog(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	fake.on("agent start", herdrErr(herdr.CodeTimeout))
	fake.on("pane read", ok("  Some new question?\n  1. Yes  2. No"))
	idleWaits := 0
	fake.on("agent wait", func(args []string) (string, error) {
		switch {
		case slices.Contains(args, "blocked"):
			return `{"result":{"agent":{"agent_status":"blocked"}}}`, nil
		case !slices.Contains(args, "idle"):
			return `{"result":{"agent":{"agent_status":"working"}}}`, nil
		}
		idleWaits++
		if idleWaits == 1 {
			return "", herdrError(herdr.CodeTimeout) // still not ready
		}
		return `{"result":{"agent":{"agent_status":"idle"}}}`, nil // the user answered
	})
	promptedAt := 0
	fake.on("agent prompt", func([]string) (string, error) {
		promptedAt = idleWaits
		return "{}", nil
	})
	tk := spawnStarting(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(events(t, r, tk.ID), "needs-human") || promptedAt < 2 {
		t.Errorf("events = %v, prompted after %d idle waits", events(t, r, tk.ID), promptedAt)
	}
}

func TestLaunchBlockedBeforePromptIsADialog(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	// Ready at start; herdr sees it blocked on the check before the prompt.
	fake.on("pane read", ok("  Some new question?\n  1. Yes  2. No"))
	fake.on("agent get", ok(`{"result":{"agent":{"agent":"claude","agent_status":"blocked"}}}`))
	idleWaits := 0
	fake.on("agent wait", func(args []string) (string, error) {
		if !slices.Contains(args, "idle") {
			return `{"result":{"agent":{"agent_status":"working"}}}`, nil
		}
		idleWaits++
		if idleWaits == 1 {
			return "", herdrError(herdr.CodeTimeout)
		}
		return `{"result":{"agent":{"agent_status":"idle"}}}`, nil
	})
	promptedAt := 0
	fake.on("agent prompt", func([]string) (string, error) {
		promptedAt = idleWaits
		return "{}", nil
	})
	tk := spawnStarting(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(events(t, r, tk.ID), "needs-human") || promptedAt < 2 {
		t.Errorf("events = %v, prompted after %d idle waits", events(t, r, tk.ID), promptedAt)
	}
}

func TestKeepRefusesAClosingRunner(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := finishedRunner(t, r, fake)
	if _, err := r.claimClose(tk.ID, gcCheck()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Keep(context.Background(), tk.ID, true); err == nil || !strings.Contains(err.Error(), "closing") {
		t.Fatalf("keep of a closing runner = %v", err)
	}
	if got, _ := r.Store.Get(tk.ID); got.Keep {
		t.Error("a closing runner was kept")
	}
}

func TestSendNeverReopensAStoppedTask(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := finishedRunner(t, r, fake)
	// Stop lands while the prompt is typed, before it claims the runner.
	fake.on("agent prompt", func([]string) (string, error) {
		_, _, err := r.Store.SetStatus(tk.ID, task.Stopped)
		return "{}", err
	})
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error { t.Status = task.Running; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Send(context.Background(), tk.ID, "x"); err == nil || !strings.Contains(err.Error(), "was stopped") {
		t.Fatalf("send during a stop = %v", err)
	}
	if got, _ := r.Store.Get(tk.ID); got.Status != task.Stopped {
		t.Errorf("status = %s, want stopped", got.Status)
	}
	// A stopped task whose tab failed to close gets no prompt.
	prompts := fake.count("agent", "prompt")
	if _, err := r.Send(context.Background(), tk.ID, "x"); err == nil || !strings.Contains(err.Error(), "is stopped") {
		t.Fatalf("send to a stopped task = %v", err)
	}
	if fake.count("agent", "prompt") != prompts {
		t.Error("a stopped task got the prompt")
	}
}

func TestKeepOffClosesOnlyThatRunner(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	other := finishedRunner(t, r, fake)
	tk := finishedRunner(t, r, fake)
	if _, err := r.Keep(context.Background(), tk.ID, true); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Keep(context.Background(), tk.ID, false); err != nil || !got.Herdr.Closed {
		t.Fatalf("keep --off past due: %+v, %v", got, err)
	}
	if got, _ := r.Store.Get(other.ID); got.Herdr.Closed {
		t.Error("keep --off closed another runner")
	}
}

func TestKeepOffLeavesAnAgentInUse(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := finishedRunner(t, r, fake)
	if _, err := r.Keep(context.Background(), tk.ID, true); err != nil {
		t.Fatal(err)
	}
	fake.on("agent get", ok(`{"result":{"agent":{"agent":"claude","agent_status":"working"}}}`))
	got, err := r.Keep(context.Background(), tk.ID, false)
	if err != nil || got.Herdr.Closed || got.Keep || fake.count("agent", "send-keys") != 0 {
		t.Fatalf("keep --off with the agent in use: %+v, %v", got, err)
	}
}
