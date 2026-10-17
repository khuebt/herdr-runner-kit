package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/config"
	"github.com/khuebt/herdr-runner-kit/internal/herdr"
	"github.com/khuebt/herdr-runner-kit/internal/install"
	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
	"github.com/khuebt/herdr-runner-kit/internal/wait"
)

func testRunner(t *testing.T) *runner.Runner {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	exec := func(ctx context.Context, _ string, args ...string) ([]byte, []byte, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "wait" {
			<-ctx.Done() // the agent stays busy
			return nil, nil, ctx.Err()
		}
		return []byte("{}"), nil, nil
	}
	return &runner.Runner{
		Store: store, Config: config.Default(), Bin: "/bin/hrk",
		Herdr:      &herdr.Client{Bin: "herdr", Exec: exec},
		Getenv:     func(string) string { return "" },
		StartAfter: func(string) error { return nil },
		Sleep:      func(time.Duration) {},
	}
}

func TestAutoarm(t *testing.T) {
	r := testRunner(t)
	var stderr strings.Builder
	if code := autoarm(context.Background(), r, "/repo", &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("nothing running: code %d, %q", code, stderr.String())
	}
	if _, found, _ := r.Store.Cursor(state.CursorName("/repo")); found {
		t.Fatal("a folder without tasks must not get a cursor")
	}

	tk := &task.Task{Kind: "fix", Agent: "claude", Prompt: "x", Title: "fix login", Owner: "/repo", Gate: task.GateAuto, Status: task.Running}
	tk.Herdr.PaneID = "w1:p1"
	if err := r.Store.Create(tk); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = r.Report(tk.ID, runner.ReportOptions{Status: task.Done, Summary: "Login fixed"})
	}()
	code := autoarm(context.Background(), r, "/repo", &stderr)
	if code != install.NewsExit || !strings.Contains(stderr.String(), tk.ID+" done · fix login · tab closes ") ||
		!strings.Contains(stderr.String(), " · Login fixed\n") {
		t.Fatalf("news: code %d, %q", code, stderr.String())
	}

	// While another wait holds the cursor, the hook stays quiet even with
	// a task still running.
	t2 := &task.Task{Kind: "fix", Agent: "claude", Prompt: "y", Title: "other", Owner: "/repo", Gate: task.GateAuto, Status: task.Running}
	t2.Herdr.PaneID = "w1:p2"
	if err := r.Store.Create(t2); err != nil {
		t.Fatal(err)
	}
	unlock, err := r.Store.LockCursor(state.CursorName("/repo"), "wait")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	// The refused hook still marks the main session active, which
	// postpones the flush of quiet news.
	marker := filepath.Join(r.Store.Dir(), "main-active-"+state.CursorName("/repo"))
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := autoarm(context.Background(), r, "/repo", &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("cursor held: code %d, %q", code, stderr.String())
	}
	if info, err := os.Stat(marker); err != nil || time.Since(info.ModTime()) > time.Minute {
		t.Errorf("activity marker not touched: %v", err)
	}
}

func TestAutoarmClosesFinishedRunnersOnTime(t *testing.T) {
	r := testRunner(t)
	tk := &task.Task{Kind: "fix", Agent: "claude", Prompt: "x", Title: "fix", Owner: "/repo", Gate: task.GateAuto, Status: task.Done}
	tk.Herdr.PaneID, tk.Herdr.TabID = "w1:p1", "w1:t1"
	tk.FinishedAt = time.Now().Add(2 * time.Second).Add(-runner.GCAge)
	if err := r.Store.Create(tk); err != nil {
		t.Fatal(err)
	}
	// Nothing runs, so wait is idle; the first hook stays until the runner
	// is due, closes it, and only then exits.
	first := make(chan int)
	go func() { first <- autoarm(context.Background(), r, "/repo", &strings.Builder{}) }()
	time.Sleep(1300 * time.Millisecond)
	// A later turn's hook leaves at once instead of piling up.
	if code := autoarm(context.Background(), r, "/repo", &strings.Builder{}); code != 0 {
		t.Fatalf("second hook: code %d", code)
	}
	if got, _ := r.Store.Get(tk.ID); got.Herdr.Closed {
		t.Fatal("closed before it was due")
	}
	if code := <-first; code != 0 {
		t.Fatalf("first hook: code %d", code)
	}
	if got, _ := r.Store.Get(tk.ID); !got.Herdr.Closed {
		t.Fatal("not closed")
	}
}

func TestWaitClosesDueRunnersWhileOthersRun(t *testing.T) {
	r := testRunner(t)
	busy := &task.Task{Kind: "fix", Agent: "claude", Prompt: "x", Title: "busy", Owner: "/repo", Gate: task.GateAuto, Status: task.Running}
	busy.Herdr.PaneID = "w1:p1"
	old := &task.Task{Kind: "fix", Agent: "claude", Prompt: "y", Title: "old", Owner: "/repo", Gate: task.GateAuto, Status: task.Done}
	old.Herdr.PaneID, old.Herdr.TabID = "w1:p2", "w1:t2"
	old.FinishedAt = time.Now().Add(-runner.GCAge - time.Minute)
	other := &task.Task{Kind: "fix", Agent: "claude", Prompt: "z", Title: "other", Owner: "/elsewhere", Gate: task.GateAuto, Status: task.Done}
	other.Herdr.PaneID = "w2:p1"
	other.FinishedAt = old.FinishedAt
	for _, tk := range []*task.Task{busy, old, other} {
		if err := r.Store.Create(tk); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := wait.Run(context.Background(), r, wait.Options{Owner: "/repo", Timeout: 500 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Store.Get(old.ID); !got.Herdr.Closed {
		t.Fatal("the due runner is still open")
	}
	if got, _ := r.Store.Get(other.ID); got.Herdr.Closed {
		t.Fatal("closed another owner's runner")
	}
}

func TestWhileAliveEndsWithTheProcess(t *testing.T) {
	aliveEvery = 10 * time.Millisecond
	t.Cleanup(func() { aliveEvery = 5 * time.Second })
	proc := exec.Command("sleep", "60")
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	ctx := whileAlive(context.Background(), proc.Process.Pid)
	select {
	case <-ctx.Done():
		t.Fatal("cancelled while the process runs")
	case <-time.After(50 * time.Millisecond):
	}
	_ = proc.Process.Kill()
	_ = proc.Wait()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("not cancelled after the process died")
	}
}

func TestWithEnvReplaces(t *testing.T) {
	got := withEnv([]string{"A=1", "TOKEN=", "B=2", "TOKEN=old"}, "TOKEN", "new")
	if strings.Join(got, " ") != "A=1 B=2 TOKEN=new" {
		t.Fatalf("got %q", got)
	}
}

func TestWhileAliveEndsWithItsParent(t *testing.T) {
	if os.Getenv("HRK_TEST_ORPHAN") == "1" {
		// Child: watch a live PID (init) and report when the watch ends.
		aliveEvery = 10 * time.Millisecond
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		<-whileAlive(ctx, 1).Done()
		if ctx.Err() != nil {
			return
		}
		_ = os.WriteFile(os.Getenv("HRK_TEST_OUT"), []byte("ended"), 0o600)
		return
	}
	out := filepath.Join(t.TempDir(), "out")
	// sh starts the test binary in the background and exits, orphaning it.
	sh := exec.Command("sh", "-c", `"$0" -test.run '^TestWhileAliveEndsWithItsParent$' & sleep 0.3`, os.Args[0])
	sh.Env = append(os.Environ(), "HRK_TEST_ORPHAN=1", "HRK_TEST_OUT="+out)
	if err := sh.Run(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, _ := os.ReadFile(out); string(data) == "ended" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the orphaned watch did not end")
}

func TestStartedByMain(t *testing.T) {
	// The hook's shell 300 runs under Claude 200, which a wrapper 123 that
	// hrk main ran as started.
	chain := func() []int { return []int{300, 200, 123} }
	cases := []struct {
		name, hrkMain string
		ancestors     func() []int
		want          bool
	}{
		{"started by hrk main --exec", "200", chain, true},
		{"under a wrapper that does not exec", "123", chain, true},
		{"a claude started from the main session's shell", "123", func() []int { return []int{400, 450, 77} }, false},
		{"the printed command names no process", "1", chain, true},
		{"no HRK_MAIN", "", chain, false},
	}
	for _, c := range cases {
		if got := startedByMain(c.hrkMain, c.ancestors); got != c.want {
			t.Errorf("%s: startedByMain = %v", c.name, got)
		}
	}
}

func TestAncestors(t *testing.T) {
	ps := "  1     0\n 77     1\n123    77\n200   123\n300   200\n310   300\n"
	if got := ancestors(ps, 310, 6); !slices.Equal(got, []int{300, 200, 123, 77}) {
		t.Errorf("ancestors = %v", got)
	}
	if got := ancestors(ps, 310, 2); !slices.Equal(got, []int{300, 200}) {
		t.Errorf("two levels = %v", got)
	}
}

func TestAutoarmLeavesTheStateOfAnotherSessionUnopened(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	t.Setenv("HRK_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HRK_MAIN", "")
	t.Setenv("HRK_TASK", "")
	cmd := newAutoarmCommand()
	cmd.SetIn(strings.NewReader(`{"session_id":"other-id","cwd":"/tmp"}`))
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state opened for another session: %v", err)
	}
}

func TestAlreadyWaitingNamesTheHolder(t *testing.T) {
	for holder, want := range map[string]string{
		"hook": "already waiting: the main session's auto re-arm hook holds this cursor (pid 7); it wakes you on news",
		"wait": "already waiting: another hrk wait holds this cursor (pid 7)",
		"":     "already waiting: another wait holds this cursor",
	} {
		if got := alreadyWaiting(&state.HeldError{Holder: holder, PID: 7}); got != want {
			t.Errorf("%q: got %q", holder, got)
		}
	}
}

func TestAutoarmRecordsTheMainTranscript(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	t.Setenv("HRK_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HRK_MAIN", "1")
	t.Setenv("HRK_TASK", "")
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A line from before transcripts were kept holds the id only.
	if err := os.WriteFile(filepath.Join(dir, "main-sessions"), []byte("old-id\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newAutoarmCommand()
	cmd.SetIn(strings.NewReader(`{"session_id":"main-id","transcript_path":"/home/user/.claude/projects/demo/main-id.jsonl","cwd":"` + t.TempDir() + `"}`))
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	want := []state.MainSession{{ID: "old-id"}, {ID: "main-id", Transcript: "/home/user/.claude/projects/demo/main-id.jsonl"}}
	if got := store.MainSessions(); !slices.Equal(got, want) {
		t.Errorf("main sessions = %+v, want %+v", got, want)
	}
	if !state.MainSessionListed(dir, "old-id") || !state.MainSessionListed(dir, "main-id") || state.MainSessionListed(dir, want[1].Transcript) {
		t.Error("MainSessionListed must match ids only, old and new lines alike")
	}
	// A later arm without a transcript keeps the recorded one.
	if err := store.RecordMainSession("main-id"); err != nil {
		t.Fatal(err)
	}
	if got := store.MainSessions(); !slices.Equal(got, want) {
		t.Errorf("after a re-record = %+v, want %+v", got, want)
	}
}
