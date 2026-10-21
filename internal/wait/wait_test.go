package wait

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
		if got.err != nil || len(got.res.Lines) != 1 || !strings.HasPrefix(got.res.Lines[0], tk.ID+" done · fix login · tab closes ") ||
			!strings.HasSuffix(got.res.Lines[0], " · done "+tk.ID) {
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
	r, repo := mergedBranchTask(t, task.Done, "")
	got, err := Run(context.Background(), r, Options{Owner: repo})
	if err != nil || len(got.Lines) != 1 || !strings.HasPrefix(got.Lines[0], "t1 cleanable") {
		t.Fatalf("got %+v, %v", got, err)
	}
	if want := "t1 cleanable · fix login · branch hrk/t1-a is merged into main; cleanup removes the branch"; got.Lines[0] != want {
		t.Errorf("line = %q, want %q", got.Lines[0], want)
	}
	if got, _ := Run(context.Background(), r, Options{Owner: repo}); got.Idle == "" {
		t.Errorf("second wait = %+v, want idle", got)
	}
}

func TestCleanableNeverStartedReadsFailed(t *testing.T) {
	r, repo := mergedBranchTask(t, task.Failed, "start agent: herdr agent start: agent_pane_busy: pane busy\nmore")
	got, err := Run(context.Background(), r, Options{Owner: repo})
	want := "t1 cleanable · failed · fix login · never started (start agent: herdr agent start: agent_pane_busy: pane busy); cleanup removes the branch"
	if err != nil || len(got.Lines) != 1 || got.Lines[0] != want {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// mergedBranchTask makes a task with status and error whose worktree is
// gone and whose branch holds nothing beyond main, and returns its
// repository.
func mergedBranchTask(t *testing.T, status task.Status, reason string) (*runner.Runner, string) {
	t.Helper()
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
	tk := addTask(t, r, status, repo)
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error {
		t.Error = reason
		t.RepoRoot = repo
		t.Worktree = &task.Worktree{Path: repo + "-wt", Branch: "hrk/t1-a", Removed: true, BranchCreated: new(true)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return r, repo
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

func TestLostRunnerFailsAtWaitStart(t *testing.T) {
	r, _ := newTestRunner(t)
	// After a restart herdr has no pane for the task: every call on it
	// fails, so the task's own watcher would wait forever.
	r.Herdr = &herdr.Client{Bin: "herdr", Exec: func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		switch strings.Join(args[:2], " ") {
		case "agent get", "agent wait":
			return nil, []byte(`{"error":{"code":"agent_not_found","message":"gone"}}`), errors.New("exit status 1")
		case "pane read":
			return nil, []byte(`{"error":{"code":"pane_not_found","message":"gone"}}`), errors.New("exit status 1")
		}
		return []byte("{}"), nil, nil
	}}
	tk := addTask(t, r, task.NeedsHuman, owner)
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error {
		t.Herdr.PaneID, t.Herdr.AgentName, t.Question = "w1C:p1", "hrk-"+t.ID, "1. which?"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got := get(t, start(r, Options{Owner: owner, Window: time.Millisecond}))
	if len(got.res.Lines) != 1 || !strings.HasPrefix(got.res.Lines[0], tk.ID+" failed · fix login · runner lost: herdr pane gone") {
		t.Fatalf("lines = %q %v", got.res.Lines, got.err)
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
	tk := &task.Task{Title: "title", Status: task.Done, FinishedAt: time.Now(), Herdr: task.Herdr{PaneID: "w1:p1"}}
	line := Line(state.Event{Task: "t1", Type: "done", Summary: long, Outputs: []string{"/a.md"}}, tk, 0)
	if n := len([]rune(line)); n > MaxLine {
		t.Errorf("line has %d characters", n)
	}
	if !strings.Contains(line, "tab closes ") {
		t.Errorf("the cut dropped the close time: %q", line)
	}
	q := Line(state.Event{Task: "t2", Type: "needs-human", Question: "Drop it?\nyes or no"}, &task.Task{Title: "t"}, 0)
	if q != "t2 needs-human · t · Q: Drop it? yes or no" {
		t.Errorf("question line = %q", q)
	}
	if got := Line(state.Event{Task: "t3", Type: "done", Summary: "ok"}, nil, 0); got != "t3 done · ok" {
		t.Errorf("line without a task = %q", got)
	}
}

func TestDoneLineHasTheGitLine(t *testing.T) {
	g := task.GitState{Branch: "main", Base: "origin/main", Untracked: 1536, UntrackedPlans: 1530, Head: "abc1234", SinceStart: 2}
	line := Line(state.Event{Task: "t1", Type: "done", Summary: "ok", Git: g.Short()}, &task.Task{Title: "fix"}, 0)
	if want := "t1 done · fix · git: repo state, on main, 1536 untracked (1530 under plans/), 2 commits since start, head abc1234 · ok"; line != want {
		t.Errorf("line = %q\nwant   %q", line, want)
	}
}

func TestDoneLineHasCloseTime(t *testing.T) {
	finished := time.Date(2026, 10, 13, 15, 45, 0, 0, time.Local)
	tk := &task.Task{Title: "fix login", Status: task.Done, FinishedAt: finished, Herdr: task.Herdr{PaneID: "w1:p1"}}
	ev := state.Event{Task: "t1", Type: "done", Summary: "fixed"}
	if got := Line(ev, tk, 0); got != "t1 done · fix login · tab closes 16:15 · fixed" {
		t.Errorf("open tab: %q", got)
	}
	tk.Keep = true
	if got := Line(ev, tk, 0); got != "t1 done · fix login · tab kept · fixed" {
		t.Errorf("kept tab: %q", got)
	}
	if got := Line(ev, tk, 4*time.Hour); got != "t1 done · fix login · tab kept until 19:45 · fixed" {
		t.Errorf("kept tab with a lifetime: %q", got)
	}
	tk.Keep, tk.Herdr.Closed = false, true
	if got := Line(ev, tk, 0); got != "t1 done · fix login · fixed" {
		t.Errorf("closed tab: %q", got)
	}
	tk.Herdr.Closed = false
	if got := Line(state.Event{Task: "t1", Type: "failed", Summary: "broke"}, tk, 0); strings.Contains(got, "tab") {
		t.Errorf("failed line has a tab note: %q", got)
	}
}

func TestLineUsesEventSnapshot(t *testing.T) {
	tk := &task.Task{
		Title: "fix login", Status: task.Done, Herdr: task.Herdr{PaneID: "w1:p1", Closed: true},
		Git: &task.GitState{Worktree: true, Branch: "b", Uncommitted: 4},
	}
	ev := state.Event{Task: "t1", Type: "done", Summary: "fixed", Git: "on b, 1 commit not pushed"}
	if got := Line(ev, tk, 0); got != "t1 done · fix login · git: on b, 1 commit not pushed · fixed" {
		t.Errorf("done line = %q", got)
	}
	ev.Type, ev.Summary = "failed", "broke"
	if got := Line(ev, tk, 0); got != "t1 failed · fix login · git: on b, 1 commit not pushed · broke" {
		t.Errorf("failed line = %q", got)
	}
	q := Line(state.Event{Task: "t1", Type: "needs-human", Question: "Which?", Git: "on b, clean"}, tk, 0)
	if strings.Contains(q, "git:") {
		t.Errorf("needs-human line has git: %q", q)
	}
	long := Line(state.Event{Task: "t1", Type: "done", Summary: strings.Repeat("word ", 200), Git: "on b, clean"}, tk, 0)
	if !strings.Contains(long, "git: on b, clean") {
		t.Errorf("the cut dropped the git state: %q", long)
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

func TestWaitSeesProjectNews(t *testing.T) {
	r, _ := newTestRunner(t)
	projectOwner := task.ProjectOwner("demo-app")
	proj := addTask(t, r, task.Running, projectOwner)
	other := addTask(t, r, task.Running, "/elsewhere")
	if err := r.Store.InitCursor(state.CursorName(owner)); err != nil {
		t.Fatal(err)
	}
	ch := start(r, Options{Owner: owner, Projects: []string{"demo-app"}, Window: time.Millisecond})
	waitBlocked()
	report(t, r, other.ID, "not watched")
	report(t, r, proj.ID, "spec written")
	got := get(t, ch)
	if got.err != nil || len(got.res.Lines) != 1 || !strings.HasPrefix(got.res.Lines[0], proj.ID+" done") {
		t.Fatalf("lines = %q, err = %v", got.res.Lines, got.err)
	}

	// --owner narrows wait to that one owner.
	addTask(t, r, task.Running, owner)
	next := addTask(t, r, task.Running, projectOwner)
	ch = start(r, Options{Owner: owner, Window: time.Millisecond, Timeout: 300 * time.Millisecond})
	waitBlocked()
	report(t, r, next.ID, "not for this wait")
	if got := get(t, ch); !got.res.TimedOut {
		t.Fatalf("narrowed wait woke: %+v", got.res)
	}
}

func TestWaitSeesTheProjectHoldingItsFolder(t *testing.T) {
	r, _ := newTestRunner(t)
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"demo-app", "other-app"} {
		if _, err := r.Store.ReserveProject(name, filepath.Join(repo, name)); err != nil {
			t.Fatal(err)
		}
		if err := r.Store.CommitProject(name); err != nil {
			t.Fatal(err)
		}
	}
	here := filepath.Join(repo, "demo-app")
	other := addTask(t, r, task.Running, task.ProjectOwner("other-app"))
	proj := addTask(t, r, task.Running, task.ProjectOwner("demo-app"))
	if err := r.Store.InitCursor(state.CursorName(here)); err != nil {
		t.Fatal(err)
	}
	ch := start(r, Options{Owner: here, Window: time.Millisecond})
	waitBlocked()
	report(t, r, other.ID, "not watched")
	report(t, r, proj.ID, "spec written")
	got := get(t, ch)
	if got.err != nil || len(got.res.Lines) != 1 || !strings.HasPrefix(got.res.Lines[0], proj.ID+" done") {
		t.Fatalf("lines = %q, err = %v", got.res.Lines, got.err)
	}
}

func TestWaitSeesAProjectStartedAfterIt(t *testing.T) {
	r, _ := newTestRunner(t)
	addTask(t, r, task.Running, owner) // keeps the wait from going idle
	if err := r.Store.InitCursor(state.CursorName(owner)); err != nil {
		t.Fatal(err)
	}
	ch := start(r, Options{Owner: owner, Window: time.Millisecond})
	waitBlocked()
	// new registers the project, attaches it to the main session and a
	// runner starts for it while the wait runs; a progress event moves the
	// cursor before the news.
	if _, err := r.Store.ReserveProject("demo-app", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.CommitProject("demo-app"); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.SetMainProjects(owner, []string{"demo-app"}); err != nil {
		t.Fatal(err)
	}
	proj := addTask(t, r, task.Running, task.ProjectOwner("demo-app"))
	if err := r.Store.Append(state.Event{Task: proj.ID, Owner: proj.Owner, Type: "progress", Summary: "reading"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	report(t, r, proj.ID, "spec written")
	got := get(t, ch)
	if got.err != nil || len(got.res.Lines) != 1 || !strings.HasPrefix(got.res.Lines[0], proj.ID+" done") {
		t.Fatalf("lines = %q, err = %v", got.res.Lines, got.err)
	}
}

func TestDoneLineListsDecisionsBelow(t *testing.T) {
	tk := &task.Task{Title: "export", Status: task.Done, Herdr: task.Herdr{PaneID: "w1:p1", Closed: true}}
	ev := state.Event{
		Task: "t1", Type: "done", Summary: "Export added.", Git: "on b, clean",
		Decisions: []string{"CSV or JSON? -> CSV", "Page size? -> 50"},
	}
	want := "t1 done · export · git: on b, clean · Export added.\n  decided: CSV or JSON? -> CSV\n  decided: Page size? -> 50"
	if got := Line(ev, tk, 0); got != want {
		t.Errorf("done line = %q", got)
	}
	ev.Summary = strings.Repeat("word ", 200)
	got := strings.Split(Line(ev, tk, 0), "\n")
	if len(got) != 3 || !strings.HasPrefix(got[0], "t1 done · export · git: on b, clean · word") || !strings.HasSuffix(got[0], "word…") ||
		got[1] != "  decided: CSV or JSON? -> CSV" || got[2] != "  decided: Page size? -> 50" {
		t.Errorf("the cut dropped more than the summary: %q", got)
	}
	ev.Decisions = []string{"What? -> " + strings.Repeat("lorem ", 80)}
	if got := strings.Split(Line(ev, tk, 0), "\n"); len(got) != 2 || len([]rune(got[1])) > MaxLine || !strings.HasSuffix(got[1], "lorem…") {
		t.Errorf("a long decision was not cut at a word: %q", got)
	}
}

func TestQuestionRoundLines(t *testing.T) {
	ev := state.Event{Task: "t4", Type: "needs-human", Question: "1. Which database? a) Postgres (recommended)\n2. " + strings.Repeat("x", 400)}
	got := strings.Split(Line(ev, &task.Task{Title: "store"}, 0), "\n")
	if len(got) != 3 || got[0] != "t4 needs-human · store · 2 questions" || got[1] != "  1. Which database? a) Postgres (recommended)" ||
		!strings.HasPrefix(got[2], "  2. xxx") || len([]rune(got[2])) > MaxLine {
		t.Errorf("round lines = %q", got)
	}
}

func TestNeedsHumanLineNamesALaterAttempt(t *testing.T) {
	tk := &task.Task{Title: "store"}
	if got := Line(state.Event{Task: "t4", Type: "needs-human", Question: "Which?", Attempt: 2}, tk, 0); got != "t4 needs-human · store · attempt 2 · Q: Which?" {
		t.Errorf("attempt 2 = %q", got)
	}
	round := state.Event{Task: "t4", Type: "needs-human", Question: "1. Which?\n2. Where?", Attempt: 2}
	if got := strings.Split(Line(round, tk, 0), "\n")[0]; got != "t4 needs-human · store · attempt 2 · 2 questions" {
		t.Errorf("round header = %q", got)
	}
	if got := Line(state.Event{Task: "t4", Type: "needs-human", Question: "Which?"}, tk, 0); got != "t4 needs-human · store · Q: Which?" {
		t.Errorf("first attempt = %q", got)
	}
	if got := Line(state.Event{Task: "t4", Type: "done", Summary: "ok", Attempt: 2}, tk, 0); got != "t4 done · store · ok" {
		t.Errorf("done of attempt 2 = %q", got)
	}
}

// chain makes a running task and a queued one that waits for it.
func chain(t *testing.T, r *runner.Runner) (first, next *task.Task) {
	t.Helper()
	first = addTask(t, r, task.Running, owner)
	next = &task.Task{Kind: "fix", Agent: "claude", Prompt: "y", Title: "next step", Owner: owner, Gate: task.GateAuto, Status: task.Queued, After: []string{first.ID}}
	if err := r.Store.Create(next); err != nil {
		t.Fatal(err)
	}
	return first, next
}

func setRunning(t *testing.T, r *runner.Runner, id string) {
	t.Helper()
	if _, err := r.Store.Update(id, func(tk *task.Task) error {
		tk.Status, tk.Herdr.PaneID = task.Running, "w1:p9"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// quiet appends a quiet news event of id, as record marks it.
func quiet(t *testing.T, r *runner.Runner, id, typ string) {
	t.Helper()
	if err := r.Store.Append(state.Event{Task: id, Owner: owner, Type: typ, Summary: typ + " " + id, Quiet: true}); err != nil {
		t.Fatal(err)
	}
}

func noWake(t *testing.T, ch chan result) {
	t.Helper()
	select {
	case got := <-ch:
		t.Fatalf("woke for quiet news: %+v %v", got.res, got.err)
	case <-time.After(300 * time.Millisecond):
	}
}

func prefixes(t *testing.T, lines []string, want ...string) {
	t.Helper()
	if len(lines) != len(want) {
		t.Fatalf("lines = %q, want %q", lines, want)
	}
	for i, w := range want {
		if !strings.HasPrefix(lines[i], w) {
			t.Fatalf("lines = %q, want %q", lines, want)
		}
	}
}

func TestChainStepDoneComesWithTheChainEnd(t *testing.T) {
	r, _ := newTestRunner(t)
	first, next := chain(t, r)
	ch := start(r, Options{Owner: owner, Window: time.Millisecond})
	waitBlocked()
	report(t, r, first.ID, "step one")
	noWake(t, ch)
	setRunning(t, r, next.ID)
	report(t, r, next.ID, "step two")
	prefixes(t, get(t, ch).res.Lines, first.ID+" done", next.ID+" done")
}

func TestQuietTypesComeWithTheNextNews(t *testing.T) {
	r, _ := newTestRunner(t)
	a := addTask(t, r, task.Running, owner)
	b := addTask(t, r, task.Running, owner)
	ch := start(r, Options{Owner: owner, Window: time.Millisecond})
	waitBlocked()
	for _, typ := range []string{"cleanable", "processes-left", "stopped"} {
		quiet(t, r, a.ID, typ)
	}
	noWake(t, ch)
	if _, err := r.Report(b.ID, runner.ReportOptions{Status: task.Failed, Summary: "broke"}); err != nil {
		t.Fatal(err)
	}
	prefixes(t, get(t, ch).res.Lines, a.ID+" cleanable", a.ID+" processes-left", a.ID+" stopped", b.ID+" failed")
}

func TestQuietNewsSurvivesANewWaitOnce(t *testing.T) {
	r, _ := newTestRunner(t)
	first, next := chain(t, r)
	if err := r.Store.InitCursor(state.CursorName(owner)); err != nil {
		t.Fatal(err)
	}
	report(t, r, first.ID, "step one")
	short := 300 * time.Millisecond
	if got := get(t, start(r, Options{Owner: owner, Holder: "hook", Timeout: short})); !got.res.TimedOut {
		t.Fatalf("hook wait = %+v %v", got.res, got.err)
	}
	// The next step runs now; the stored flag still keeps the done quiet.
	setRunning(t, r, next.ID)
	if got := get(t, start(r, Options{Owner: owner, Timeout: short})); !got.res.TimedOut {
		t.Fatalf("second wait = %+v %v", got.res, got.err)
	}
	got := get(t, start(r, Options{Owner: owner, Timeout: short, FlushOnTimeout: true}))
	prefixes(t, got.res.Lines, first.ID+" done")
	if got := get(t, start(r, Options{Owner: owner, Timeout: short, FlushOnTimeout: true})); !got.res.TimedOut {
		t.Fatalf("delivered twice: %+v %v", got.res, got.err)
	}
}

func TestQuietNewsFlushesWhenMainWasIdleTooLong(t *testing.T) {
	r, _ := newTestRunner(t)
	a := addTask(t, r, task.Running, owner)
	o := Options{Owner: owner, Window: time.Millisecond}
	if err := MarkActive(r.Store, o); err != nil {
		t.Fatal(err)
	}
	ch := start(r, o)
	waitBlocked()
	quiet(t, r, a.ID, "cleanable")
	noWake(t, ch) // the main session was just active
	old := time.Now().Add(-flushAfter)
	if err := os.Chtimes(activePath(r.Store, state.CursorName(owner)), old, old); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.Append(state.Event{Task: a.ID, Owner: owner, Type: "started"}); err != nil {
		t.Fatal(err)
	}
	prefixes(t, get(t, ch).res.Lines, a.ID+" cleanable")
}

func TestIdleBringsQuietNewsThenIdle(t *testing.T) {
	r, _ := newTestRunner(t)
	a := addTask(t, r, task.Done, owner)
	if err := r.Store.InitCursor(state.CursorName(owner)); err != nil {
		t.Fatal(err)
	}
	quiet(t, r, a.ID, "processes-left")
	got := get(t, start(r, Options{Owner: owner}))
	prefixes(t, got.res.Lines, a.ID+" processes-left")
	if got := get(t, start(r, Options{Owner: owner})); got.res.Idle == "" {
		t.Fatalf("second wait = %+v %v", got.res, got.err)
	}
}

func TestEveryNewsWakesWhenAsked(t *testing.T) {
	for name, set := range map[string]func(*runner.Runner, *Options, string){
		"all-news":    func(_ *runner.Runner, o *Options, _ string) { o.AllNews = true },
		"task":        func(_ *runner.Runner, o *Options, id string) { o.Tasks = []string{id} },
		"wakeOn: all": func(r *runner.Runner, _ *Options, _ string) { r.Config.WakeOn = config.WakeAll },
	} {
		t.Run(name, func(t *testing.T) {
			r, _ := newTestRunner(t)
			a := addTask(t, r, task.Running, owner)
			o := Options{Owner: owner, Window: time.Millisecond}
			set(r, &o, a.ID)
			ch := start(r, o)
			waitBlocked()
			quiet(t, r, a.ID, "cleanable")
			prefixes(t, get(t, ch).res.Lines, a.ID+" cleanable")
		})
	}
}

func TestTaskCursorName(t *testing.T) {
	got := TaskCursor([]string{"t2", "t1", "t2"})
	if got != TaskCursor([]string{"t1", "t2"}) {
		t.Errorf("order or duplicates change the name: %s", got)
	}
	if !strings.HasPrefix(got, "tasks-") || got == TaskCursor([]string{"t1"}) || got == state.CursorName(owner) {
		t.Errorf("name = %s", got)
	}
}

func TestTaskWaitLeavesTheMainCursor(t *testing.T) {
	r, _ := newTestRunner(t)
	a, b := addTask(t, r, task.Running, owner), addTask(t, r, task.Running, owner)
	main := state.CursorName(owner)
	if err := r.Store.InitCursor(main); err != nil {
		t.Fatal(err)
	}
	before, _, _ := r.Store.Cursor(main)
	ch := start(r, Options{Owner: owner, Tasks: []string{a.ID}, Window: time.Millisecond})
	waitBlocked()
	report(t, r, b.ID, "other")
	report(t, r, a.ID, "mine")
	got := get(t, ch)
	prefixes(t, got.res.Lines, a.ID+" done")
	if after, _, _ := r.Store.Cursor(main); after != before {
		t.Fatalf("main cursor moved from %d to %d", before, after)
	}
	plain, err := Run(context.Background(), r, Options{Owner: owner, Window: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	prefixes(t, plain.Lines, b.ID+" done", a.ID+" done")
}

func TestTaskCursorStartsAtTheMainCursor(t *testing.T) {
	r, _ := newTestRunner(t)
	a := addTask(t, r, task.Running, owner)
	if err := r.Store.InitCursor(state.CursorName(owner)); err != nil {
		t.Fatal(err)
	}
	report(t, r, a.ID, "early")
	o := Options{Owner: owner, Tasks: []string{a.ID}, Window: time.Millisecond}
	first, err := Run(context.Background(), r, o)
	if err != nil {
		t.Fatal(err)
	}
	prefixes(t, first.Lines, a.ID+" done")
	again, err := Run(context.Background(), r, o)
	if err != nil || len(again.Lines) != 0 {
		t.Fatalf("second wait: %+v %v", again, err)
	}
}

func TestTaskWaitRunsBesideTheHook(t *testing.T) {
	r, _ := newTestRunner(t)
	a := addTask(t, r, task.Running, owner)
	unlock, err := r.Store.LockCursor(state.CursorName(owner), "hook")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ch := start(r, Options{Owner: owner, Tasks: []string{a.ID}, Window: time.Millisecond})
	waitBlocked()
	report(t, r, a.ID, "beside")
	got := get(t, ch)
	if got.err != nil {
		t.Fatal(got.err)
	}
	prefixes(t, got.res.Lines, a.ID+" done")
}

func TestMarkActiveMarksTheMainCursor(t *testing.T) {
	r, _ := newTestRunner(t)
	if err := MarkActive(r.Store, Options{Owner: owner, Tasks: []string{"t1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.Store.Dir(), "main-active-"+state.CursorName(owner))); err != nil {
		t.Error(err)
	}
	if m, _ := filepath.Glob(filepath.Join(r.Store.Dir(), "main-active-tasks-*")); len(m) != 0 {
		t.Errorf("task cursor marked: %v", m)
	}
}
