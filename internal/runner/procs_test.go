package runner

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func TestParsePS(t *testing.T) {
	for name, out := range map[string]string{
		"linux": "      1       0 Wed Oct 14 09:29:16 2026 /sbin/init splash\n" +
			"   4123    4100 Thu Oct 15 01:21:00 2026 npm run dev  --port 3000\n",
		"macos": "    1     0 Wed Oct 14 09:29:16 2026     /sbin/init splash\n" +
			" 4123  4100 Thu Oct 15 01:21:00 2026     npm run dev  --port 3000\n\n",
	} {
		procs, err := parsePS(out)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := proc{PID: 4123, PPID: 4100, Start: "Thu Oct 15 01:21:00 2026", Command: "npm run dev  --port 3000"}
		if len(procs) != 2 || procs[1] != want || procs[0].Command != "/sbin/init splash" {
			t.Errorf("%s: %+v", name, procs)
		}
	}
	if _, err := parsePS("garbage line\n"); err == nil {
		t.Error("parsed a line that is not ps output")
	}
}

func TestDescendantsAndSurvivors(t *testing.T) {
	table := []proc{
		{PID: 1, PPID: 0, Start: "a"},
		{PID: 100, PPID: 1, Start: "a", Command: "zsh"},
		{PID: 101, PPID: 100, Start: "a", Command: "claude"},
		{PID: 102, PPID: 101, Start: "a", Command: "npm run dev"},
		{PID: 103, PPID: 102, Start: "a", Command: "vite"},
		{PID: 200, PPID: 1, Start: "a", Command: "other"},
	}
	got := descendants(table, 100)
	var pids []int
	for _, p := range got {
		pids = append(pids, p.PID)
	}
	slices.Sort(pids)
	if !slices.Equal(pids, []int{101, 102, 103}) {
		t.Fatalf("descendants = %v", pids)
	}
	// After the close: the agent is gone, the dev server was reparented,
	// and 103's pid now belongs to a newer process.
	after := []proc{{PID: 1, Start: "a"}, {PID: 102, PPID: 1, Start: "a"}, {PID: 103, PPID: 1, Start: "b"}}
	left := survivors(got, after)
	if len(left) != 1 || left[0].PID != 102 {
		t.Errorf("survivors = %+v", left)
	}
}

func TestSurvivorSummary(t *testing.T) {
	left := []proc{
		{PID: 1, Command: "node server.js --token=abcdefgh12345678"},
		{PID: 2, Command: "vite"},
		{PID: 3, Command: "esbuild"},
		{PID: 4, Command: "tail -f"},
	}
	got := survivorSummary(left, "after its tab closed")
	if !strings.HasPrefix(got, "4 processes the runner started still run after its tab closed: 1 `node server.js --token=<redacted>`, 2 `vite`, 3 `esbuild` and 1 more") ||
		strings.Contains(got, "abcdefgh") {
		t.Errorf("summary = %q", got)
	}
	if got := survivorSummary(left[1:2], "after its tab closed"); !strings.HasPrefix(got, "1 process the runner started") {
		t.Errorf("one survivor = %q", got)
	}
}

// procTables returns a ProcTable that hands out the given tables in turn,
// repeating the last one.
func procTables(tables ...[]proc) func(context.Context) ([]proc, error) {
	var mu sync.Mutex
	return func(context.Context) ([]proc, error) {
		mu.Lock()
		defer mu.Unlock()
		tb := tables[0]
		if len(tables) > 1 {
			tables = tables[1:]
		}
		return tb, nil
	}
}

func eventsOf(t *testing.T, r *Runner, id, typ string) []state.Event {
	t.Helper()
	all, _, err := r.Store.ReadEventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	var out []state.Event
	for _, ev := range all {
		if ev.Task == id && ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

var runnerTree = []proc{
	{PID: 100, PPID: 1, Start: "a", Command: "zsh"},
	{PID: 101, PPID: 100, Start: "a", Command: "claude"},
	{PID: 102, PPID: 101, Start: "a", Command: "python3 -m http.server 8765"},
}

func TestCloseReportsLeftoverProcesses(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	fake.on("pane process-info", ok(`{"result":{"process_info":{"shell_pid":100}}}`))
	tk := finishedRunner(t, r, fake)
	reparented := []proc{{PID: 100, PPID: 1, Start: "a"}, {PID: 102, PPID: 1, Start: "a", Command: "python3 -m http.server 8765"}}
	r.ProcTable = procTables(runnerTree, reparented)
	if _, err := r.Stop(context.Background(), tk.ID, false); err != nil {
		t.Fatal(err)
	}
	left := eventsOf(t, r, tk.ID, "processes-left")
	if len(left) != 1 || !strings.Contains(left[0].Summary, "1 process the runner started still run after its tab closed: 102 `python3 -m http.server 8765`") {
		t.Fatalf("processes-left = %+v", left)
	}
	if fake.count("tab", "close") != 1 {
		t.Error("the tab was not closed")
	}
	// A second stop closes nothing, so it reports nothing again.
	if _, err := r.Stop(context.Background(), tk.ID, false); err != nil || len(eventsOf(t, r, tk.ID, "processes-left")) != 1 {
		t.Errorf("second stop: %v, %d reports", err, len(eventsOf(t, r, tk.ID, "processes-left")))
	}
}

func TestCloseQuietWhenEverythingExited(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	fake.on("pane process-info", ok(`{"result":{"process_info":{"shell_pid":100}}}`))
	tk := finishedRunner(t, r, fake)
	// The worktree shell itself stays, and a reused pid is someone else's.
	r.ProcTable = procTables(runnerTree, []proc{{PID: 100, PPID: 1, Start: "a"}, {PID: 102, PPID: 1, Start: "b"}})
	if _, err := r.Stop(context.Background(), tk.ID, false); err != nil {
		t.Fatal(err)
	}
	if ev := events(t, r, tk.ID); slices.Contains(ev, "processes-left") || slices.Contains(ev, "process-check-skipped") {
		t.Errorf("events = %v", ev)
	}
}

func TestWorktreeShellIsNotALeftover(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	fake.on("agent get", ok(`{"result":{"agent":{"agent_status":"idle"}}}`))
	fake.on("pane process-info", ok(`{"result":{"process_info":{"shell_pid":100}}}`))
	tk := spawnStarting(t, r, SpawnOptions{Kind: "code", Prompt: "x", Cwd: gitRepo(t)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	r.ProcTable = procTables(runnerTree, runnerTree[:1])
	if _, err := r.Stop(context.Background(), tk.ID, false); err != nil {
		t.Fatal(err)
	}
	if ev := events(t, r, tk.ID); slices.Contains(ev, "processes-left") {
		t.Errorf("events = %v", ev)
	}
}

func TestProcessCheckSkipped(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	fake.on("pane process-info", herdrErr("pane_unsupported"))
	tk := finishedRunner(t, r, fake)
	r.ProcTable = procTables(runnerTree)
	got, err := r.Stop(context.Background(), tk.ID, false)
	if err != nil || !got.Herdr.Closed {
		t.Fatalf("stop: %+v, %v", got, err)
	}
	if n := len(eventsOf(t, r, tk.ID, "process-check-skipped")); n != 1 {
		t.Errorf("%d process-check-skipped events", n)
	}

	r2, fake2, _ := newTestRunner(t)
	fake2.on("pane process-info", ok(`{"result":{"process_info":{"shell_pid":100}}}`))
	tk2 := finishedRunner(t, r2, fake2)
	r2.ProcTable = func(context.Context) ([]proc, error) { return nil, errors.New("ps: not found") }
	if got, err := r2.Stop(context.Background(), tk2.ID, false); err != nil || !got.Herdr.Closed {
		t.Fatalf("stop without ps: %+v, %v", got, err)
	}
	if n := len(eventsOf(t, r2, tk2.ID, "process-check-skipped")); n != 1 {
		t.Errorf("without ps: %d process-check-skipped events", n)
	}
}

func TestCloseFindsAProcessThatLeftTheTree(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	fake.on("pane process-info", ok(`{"result":{"process_info":{"shell_pid":100}}}`))
	tk := finishedRunner(t, r, fake)
	mine := "HRK_TASK=" + tk.ID + "\x00HRK_STATE_DIR=" + r.Store.Dir() + "\x00PATH=/bin\x00"
	envs := map[int]string{
		100: mine, 101: mine, // the shell and the agent
		300: mine,                                                       // started with & by a shell that exited, now under a subreaper
		301: "HRK_TASK=" + tk.ID + "\x00HRK_STATE_DIR=/other/state\x00", // another kit state's task
		302: "PATH=/bin\x00",
		303: mine, // the report's detached follow-up
	}
	r.ProcEnv = func(pid int) ([]byte, error) {
		if e, ok := envs[pid]; ok {
			return []byte(e), nil
		}
		return nil, errors.New("no such process")
	}
	before := append(slices.Clone(runnerTree[:2]),
		proc{PID: 300, PPID: 2414, Start: "a", Command: "python3 -m http.server 8765"},
		proc{PID: 301, PPID: 2414, Start: "a", Command: "other kit"},
		proc{PID: 302, PPID: 1, Start: "a", Command: "unrelated"},
		proc{PID: 303, PPID: 1, Start: "a", Command: r.Bin + " __after " + tk.ID})
	after := []proc{{PID: 300, PPID: 2414, Start: "a", Command: "python3 -m http.server 8765"}, before[3], before[4], before[5]}
	r.ProcTable = procTables(before, after)
	if _, err := r.Stop(context.Background(), tk.ID, false); err != nil {
		t.Fatal(err)
	}
	left := eventsOf(t, r, tk.ID, "processes-left")
	if len(left) != 1 || !strings.Contains(left[0].Summary, "1 process the runner started still run after its tab closed: 300 `python3 -m http.server 8765`") {
		t.Fatalf("processes-left = %+v", left)
	}
}

func TestCloseReportsScopeProcessesOfAGonePane(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := finishedRunner(t, r, fake)
	path := scopeDir(t, "101\n102\n")
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error { t.Scopes = []string{path}; return nil }); err != nil {
		t.Fatal(err)
	}
	fake.on("pane process-info", herdrErr("pane_not_found"))
	fake.on("agent get", herdrErr("pane_not_found"))
	r.ProcTable = procTables(runnerTree)
	if _, err := r.Stop(context.Background(), tk.ID, false); err != nil {
		t.Fatal(err)
	}
	left := eventsOf(t, r, tk.ID, "processes-left")
	if len(left) != 1 || !strings.HasPrefix(left[0].Summary, "2 processes the runner started still run after its tab closed: 101 `claude`, 102 `python3") {
		t.Fatalf("processes-left = %+v", left)
	}
	if ev := events(t, r, tk.ID); slices.Contains(ev, "process-check-skipped") {
		t.Errorf("events = %v", ev)
	}
}
