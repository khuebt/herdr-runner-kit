package wait

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/config"
	"github.com/khuebt/herdr-runner-kit/internal/herdr"
	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

const owner = "/repo"

// agentWait is the fake `herdr agent wait`: it blocks until released or the
// context ends, then returns its result.
type agentWait struct {
	gone chan struct{}
}

func newTestRunner(t *testing.T) (*runner.Runner, *agentWait) {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	aw := &agentWait{gone: make(chan struct{})}
	exec := func(ctx context.Context, _ string, args ...string) ([]byte, []byte, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "wait" {
			select {
			case <-aw.gone:
				return nil, []byte(`{"error":{"code":"agent_not_running","message":"gone"}}`), errors.New("exit status 1")
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		return []byte("{}"), nil, nil
	}
	r := &runner.Runner{
		Store: store, Config: config.Default(), Bin: "/bin/hrk",
		Herdr:      &herdr.Client{Bin: "herdr", Exec: exec},
		Getenv:     func(string) string { return "" },
		StartAfter: func(string) error { return nil },
		Sleep:      func(time.Duration) {},
	}
	return r, aw
}

// addTask writes a task directly in the given status.
func addTask(t *testing.T, r *runner.Runner, status task.Status, o string) *task.Task {
	t.Helper()
	tk := &task.Task{Kind: "fix", Agent: "claude", Prompt: "x", Title: "fix login", Owner: o, Gate: task.GateAuto, Status: status}
	if status == task.Running {
		tk.Herdr.PaneID = "w1:p1"
	}
	if err := r.Store.Create(tk); err != nil {
		t.Fatal(err)
	}
	return tk
}

func report(t *testing.T, r *runner.Runner, id, summary string) {
	t.Helper()
	if _, err := r.Report(id, runner.ReportOptions{Status: task.Done, Summary: summary}); err != nil {
		t.Fatal(err)
	}
}

type result struct {
	res Result
	err error
	at  time.Time
}

func start(r *runner.Runner, o Options) chan result {
	ch := make(chan result, 1)
	go func() {
		res, err := Run(context.Background(), r, o)
		ch <- result{res, err, time.Now()}
	}()
	return ch
}

func get(t *testing.T, ch chan result) result {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not return")
		return result{}
	}
}

// waitBlocked gives Run time to take its lock and register the watch.
func waitBlocked() { time.Sleep(50 * time.Millisecond) }

func TestReportWakesWaitFast(t *testing.T) {
	r, _ := newTestRunner(t)
	var spans []time.Duration
	for i := range 20 {
		tk := addTask(t, r, task.Running, owner)
		if i == 0 {
			if err := r.Store.InitCursor(state.CursorName(owner)); err != nil {
				t.Fatal(err)
			}
		}
		ch := start(r, Options{Owner: owner, Window: time.Millisecond})
		waitBlocked()
		at := time.Now()
		report(t, r, tk.ID, "done "+tk.ID)
		got := get(t, ch)
		if got.err != nil || len(got.res.Lines) != 1 || !strings.HasPrefix(got.res.Lines[0], tk.ID+" done · fix login · done "+tk.ID) {
			t.Fatalf("run %d: %+v %v", i, got.res, got.err)
		}
		spans = append(spans, got.at.Sub(at))
	}
	slices.Sort(spans)
	if p95 := spans[18]; p95 > time.Second {
		t.Errorf("p95 report→wake = %v, budget 1s (all: %v)", p95, spans)
	}
}

func TestBurstWakesOnce(t *testing.T) {
	r, _ := newTestRunner(t)
	var ids []string
	for range 5 {
		ids = append(ids, addTask(t, r, task.Running, owner).ID)
	}
	ch := start(r, Options{Owner: owner})
	waitBlocked()
	for _, id := range ids {
		report(t, r, id, "ok")
		time.Sleep(20 * time.Millisecond)
	}
	got := get(t, ch)
	if got.err != nil || len(got.res.Lines) != 5 {
		t.Fatalf("lines = %q, %v", got.res.Lines, got.err)
	}
	all, _, _ := r.Store.ReadEventsSince(0)
	if last := all[len(all)-1]; last.Type != "woke" || last.Summary != strings.Join(ids, ",") {
		t.Errorf("woke = %+v", last)
	}
}

func TestEventBeforeWaitIsNotLostOrRepeated(t *testing.T) {
	r, _ := newTestRunner(t)
	a := addTask(t, r, task.Running, owner)
	b := addTask(t, r, task.Running, owner)
	if err := r.Store.InitCursor(state.CursorName(owner)); err != nil {
		t.Fatal(err)
	}
	report(t, r, a.ID, "early") // lands before wait runs
	got := get(t, start(r, Options{Owner: owner, Window: time.Millisecond}))
	if len(got.res.Lines) != 1 || !strings.HasPrefix(got.res.Lines[0], a.ID+" done") {
		t.Fatalf("first wait = %+v %v", got.res, got.err)
	}
	ch := start(r, Options{Owner: owner, Window: time.Millisecond})
	waitBlocked()
	report(t, r, b.ID, "later")
	got = get(t, ch)
	if len(got.res.Lines) != 1 || !strings.HasPrefix(got.res.Lines[0], b.ID+" done") {
		t.Fatalf("second wait repeated or missed: %q", got.res.Lines)
	}
}

func TestOtherOwnersAndProgressDoNotWake(t *testing.T) {
	r, _ := newTestRunner(t)
	mineTask := addTask(t, r, task.Running, owner)
	other := addTask(t, r, task.Running, "/other")
	ch := start(r, Options{Owner: owner, Window: time.Millisecond})
	waitBlocked()
	report(t, r, other.ID, "not yours")
	if err := r.Store.Append(state.Event{Task: mineTask.ID, Owner: owner, Type: "started"}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-ch:
		t.Fatalf("woke for another owner or progress: %+v", got.res)
	case <-time.After(300 * time.Millisecond):
	}
	report(t, r, mineTask.ID, "yours")
	if got := get(t, ch); len(got.res.Lines) != 1 || !strings.Contains(got.res.Lines[0], "yours") {
		t.Fatalf("lines = %q", got.res.Lines)
	}
}

func TestSecondWaitOnCursorRefused(t *testing.T) {
	r, _ := newTestRunner(t)
	tk := addTask(t, r, task.Running, owner)
	ch := start(r, Options{Owner: owner, Window: time.Millisecond})
	waitBlocked()
	if _, err := Run(context.Background(), r, Options{Owner: owner}); !errors.Is(err, state.ErrAlreadyWaiting) {
		t.Fatalf("second wait: %v", err)
	}
	report(t, r, tk.ID, "ok")
	get(t, ch)
}

func TestIdleAndTimeout(t *testing.T) {
	r, _ := newTestRunner(t)
	got, err := Run(context.Background(), r, Options{Owner: owner})
	if err != nil || got.Idle != "idle: no active tasks" {
		t.Fatalf("empty: %+v %v", got, err)
	}
	// A held task can still bring news: another main session may release
	// it from Telegram.
	addTask(t, r, task.Held, owner)
	got, err = Run(context.Background(), r, Options{Owner: owner, Timeout: 100 * time.Millisecond})
	if err != nil || !got.TimedOut {
		t.Errorf("held: %+v %v", got, err)
	}
}

func TestIdleWaitOffersMergedWorktree(t *testing.T) {
	r, _ := newTestRunner(t)
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"branch", "hrk/t1-a"}, // nothing on it beyond main: merged
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	tk := addTask(t, r, task.Done, repo)
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error {
		t.RepoRoot = repo
		t.Worktree = &task.Worktree{Path: repo + "-wt", Branch: "hrk/t1-a", Removed: true, BranchCreated: new(true)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := Run(context.Background(), r, Options{Owner: repo})
	if err != nil || len(got.Lines) != 1 || !strings.HasPrefix(got.Lines[0], "t1 cleanable") {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got, _ := Run(context.Background(), r, Options{Owner: repo}); got.Idle == "" {
		t.Errorf("second wait = %+v, want idle", got)
	}
}

func TestAgentGoneFailsTask(t *testing.T) {
	r, aw := newTestRunner(t)
	tk := addTask(t, r, task.Running, owner)
	ch := start(r, Options{Owner: owner, Window: time.Millisecond})
	waitBlocked()
	at := time.Now()
	close(aw.gone)
	got := get(t, ch)
	if len(got.res.Lines) != 1 || !strings.HasPrefix(got.res.Lines[0], tk.ID+" failed · fix login · the agent exited without reporting") {
		t.Fatalf("lines = %q %v", got.res.Lines, got.err)
	}
	if d := got.at.Sub(at); d > 5*time.Second {
		t.Errorf("agent gone → failed took %v", d)
	}
}

func TestDeadLauncherFailsTask(t *testing.T) {
	r, _ := newTestRunner(t)
	tk := addTask(t, r, task.Starting, owner)
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error {
		t.LaunchDeadline = time.Now().Add(-time.Second)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got := get(t, start(r, Options{Owner: owner, Window: time.Millisecond}))
	if len(got.res.Lines) != 1 || !strings.Contains(got.res.Lines[0], "the launch did not finish in time") {
		t.Fatalf("lines = %q %v", got.res.Lines, got.err)
	}
}

func TestLineIsShort(t *testing.T) {
	long := strings.Repeat("word ", 200)
	line := Line(state.Event{Task: "t1", Type: "done", Summary: long, Outputs: []string{"/a.md"}}, "title")
	if n := len([]rune(line)); n > MaxLine {
		t.Errorf("line has %d characters", n)
	}
	q := Line(state.Event{Task: "t2", Type: "needs-human", Question: "Drop it?\nyes or no"}, "t")
	if q != "t2 needs-human · t · Q: Drop it? yes or no" {
		t.Errorf("question line = %q", q)
	}
}

// A status change lands before its event. A wait that looks in between must
// still return the news, not idle.
func TestIdleWaitsForTheEventOfAFreshChange(t *testing.T) {
	r, _ := newTestRunner(t)
	tk := addTask(t, r, task.Running, owner)
	if err := r.Store.InitCursor(state.CursorName(owner)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Store.SetStatus(tk.ID, task.Done); err != nil {
		t.Fatal(err)
	}
	ch := start(r, Options{Owner: owner, Window: time.Millisecond})
	waitBlocked()
	if err := r.Store.Append(state.Event{Task: tk.ID, Owner: owner, Type: "done", Status: task.Done, Summary: "late"}); err != nil {
		t.Fatal(err)
	}
	got := get(t, ch)
	if got.err != nil || len(got.res.Lines) != 1 {
		t.Fatalf("got %+v %v", got.res, got.err)
	}
}

// An answer from another main session, or typed in the runner's tab, lets a
// task that waits on a human finish; its owner's wait must still be there.
func TestWaitStaysForATaskThatWaitsOnAHuman(t *testing.T) {
	r, _ := newTestRunner(t)
	tk := addTask(t, r, task.NeedsHuman, owner)
	ch := start(r, Options{Owner: owner, Window: time.Millisecond})
	select {
	case got := <-ch:
		t.Fatalf("returned while the task waits on a human: %+v", got)
	case <-time.After(1500 * time.Millisecond): // past statusSettle
	}
	report(t, r, tk.ID, "answered elsewhere")
	if got := get(t, ch); got.err != nil || len(got.res.Lines) != 1 || !strings.Contains(got.res.Lines[0], tk.ID+" done") {
		t.Fatalf("got %+v", got)
	}
}
