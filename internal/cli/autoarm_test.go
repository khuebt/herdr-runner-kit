package cli

import (
	"context"
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
