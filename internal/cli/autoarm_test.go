package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
	if code != install.NewsExit || !strings.Contains(stderr.String(), tk.ID+" done · fix login · Login fixed") {
		t.Fatalf("news: code %d, %q", code, stderr.String())
	}

	// While another wait holds the cursor, the hook stays quiet even with
	// a task still running.
	t2 := &task.Task{Kind: "fix", Agent: "claude", Prompt: "y", Title: "other", Owner: "/repo", Gate: task.GateAuto, Status: task.Running}
	t2.Herdr.PaneID = "w1:p2"
	if err := r.Store.Create(t2); err != nil {
		t.Fatal(err)
	}
	unlock, err := r.Store.LockCursor(state.CursorName("/repo"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	stderr.Reset()
	if code := autoarm(context.Background(), r, "/repo", &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("cursor held: code %d, %q", code, stderr.String())
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
