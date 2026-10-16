package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func TestScopeUnit(t *testing.T) {
	if unit := scopeUnit("t7", 2, time.Unix(1760000000, 0)); unit != "hrk-t7-2-1760000000.scope" {
		t.Errorf("unit = %q", unit)
	}
}

func TestScopeProperties(t *testing.T) {
	base := "PIDs au 2 11 12 MemoryMax t 536870912 MemorySwapMax t 0 CollectMode s inactive-or-failed"
	if got := strings.Join(scopeProperties([]int{11, 12}, 512<<20, false), " "); got != "4 "+base {
		t.Errorf("without OOMPolicy: %s", got)
	}
	if got := strings.Join(scopeProperties([]int{11, 12}, 512<<20, true), " "); got != "5 "+base+" OOMPolicy s continue" {
		t.Errorf("with OOMPolicy: %s", got)
	}
}

func TestScopeBytes(t *testing.T) {
	total := func() (uint64, error) { return 1000 << 20, nil }
	for in, want := range map[string]uint64{"512M": 512 << 20, "16G": 16 << 30, "8K": 8 << 10, "1T": 1 << 40, "25%": 250 << 20, "100%": 1000 << 20} {
		if got, err := (memScope{memoryMax: in, memTotal: total}).bytes(); err != nil || got != want {
			t.Errorf("%s = %d, %v; want %d", in, got, err, want)
		}
	}
}

// fakeBus is a memScope whose busctl records its calls and whose processes
// land in the scope at once. With old, it rejects OOMPolicy as systemd 249
// does.
func fakeBus(old bool) (memScope, *[][]string) {
	var calls [][]string
	s := memScope{
		memoryMax: "25%",
		memTotal:  func() (uint64, error) { return 4 << 30, nil },
		sleep:     func(time.Duration) {},
	}
	unit := ""
	s.busctl = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		if old && slices.Contains(args, "OOMPolicy") {
			return []byte("Call failed: Cannot set property OOMPolicy, or unknown property."), errors.New("exit status 1")
		}
		unit = args[6]
		return []byte(`o "/org/freedesktop/systemd1/job/1"`), nil
	}
	s.cgroup = func(int) (string, error) { return "/user.slice/app.slice/" + unit, nil }
	return s, &calls
}

// properties returns a StartTransientUnit call's properties after PIDs.
func properties(t *testing.T, call []string) []string {
	t.Helper()
	i := slices.Index(call, "PIDs")
	if i < 0 || call[i+1] != "au" {
		t.Fatalf("no PIDs in %q", call)
	}
	n := 0
	for _, c := range call[i+2] {
		n = n*10 + int(c-'0')
	}
	return call[i+3+n:]
}

func TestProbeAndLaunchShareProperties(t *testing.T) {
	for _, old := range []bool{false, true} {
		s, calls := fakeBus(old)
		if err := s.probe(t.Context()); err != nil {
			t.Fatal(err)
		}
		probeCalls := len(*calls)
		path, err := s.enter(t.Context(), "hrk-t1-1-1.scope", []int{101, 102})
		if err != nil || path != "/user.slice/app.slice/hrk-t1-1-1.scope" {
			t.Fatalf("enter = %q, %v", path, err)
		}
		probe, launch := (*calls)[:probeCalls], (*calls)[probeCalls:]
		if want := map[bool]int{false: 1, true: 2}[old]; len(probe) != want || len(launch) != want {
			t.Fatalf("old %v: calls = %q", old, *calls)
		}
		for i := range probe {
			if !slices.Equal(properties(t, probe[i]), properties(t, launch[i])) || probe[i][8] != launch[i][8] || !strings.HasPrefix(probe[i][6], "hrk-probe-") {
				t.Errorf("old %v: probe %q, launch %q", old, probe[i], launch[i])
			}
		}
		if last := launch[len(launch)-1]; slices.Contains(last, "OOMPolicy") == old {
			t.Errorf("old %v: last call %q", old, last)
		}
	}
}

func TestEnterChecksTheProcessMoved(t *testing.T) {
	s, _ := fakeBus(false)
	s.cgroup = func(int) (string, error) { return "/user.slice/app.slice/herdr.service", nil }
	if _, err := s.enter(t.Context(), "hrk-t1-1-1.scope", []int{101}); err == nil || !strings.Contains(err.Error(), "still in /user.slice/app.slice/herdr.service") {
		t.Errorf("err = %v", err)
	}
}

// scopedLaunch launches a runner whose agent is pid 101 under shell 100,
// with enter as its EnterScope, and returns the task and its started event.
func scopedLaunch(t *testing.T, enter func(context.Context, string, []int) (string, error)) (*Runner, *fakeHerdr, *task.Task, string) {
	t.Helper()
	r, fake, _ := newTestRunner(t)
	r.EnterScope = enter
	r.ProcTable = procTables(runnerTree)
	fake.on("pane process-info", ok(`{"result":{"process_info":{"shell_pid":100,"foreground_process_group_id":101}}}`))
	tk := spawnStarting(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: plainDir(t)})
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Store.Get(tk.ID)
	started := eventsOf(t, r, tk.ID, "started")
	if len(started) != 1 {
		t.Fatalf("started = %+v", started)
	}
	return r, fake, got, started[0].Summary
}

func TestLaunchMovesTheAgentIntoAScope(t *testing.T) {
	var unit string
	var pids []int
	_, fake, tk, started := scopedLaunch(t, func(_ context.Context, u string, p []int) (string, error) {
		unit, pids = u, p
		return "/user.slice/app.slice/" + u, nil
	})
	if !strings.HasPrefix(unit, "hrk-t1-1-") || !slices.Equal(pids, []int{101, 102}) {
		t.Errorf("entered %q with %v", unit, pids)
	}
	if !strings.Contains(started, "memory scope "+unit) || !slices.Equal(tk.Scopes, []string{"/user.slice/app.slice/" + unit}) {
		t.Errorf("started %q, scopes %q", started, tk.Scopes)
	}
	if n := fake.count("pane", "run"); n != 0 {
		t.Errorf("%d pane runs typed", n)
	}
}

func TestLaunchWithoutAScope(t *testing.T) {
	_, _, tk, started := scopedLaunch(t, func(context.Context, string, []int) (string, error) {
		return "", errors.New("start hrk-t1-1-1.scope: Unknown assignment")
	})
	if tk.Status != task.Running || len(tk.Scopes) != 0 || !strings.Contains(started, "no memory scope: start hrk-t1-1-1.scope: Unknown assignment") {
		t.Errorf("status %s, scopes %q, started %q", tk.Status, tk.Scopes, started)
	}

	_, fake, tk, started := scopedLaunch(t, nil)
	if tk.Status != task.Running || strings.Contains(started, "memory scope") || fake.count("pane", "process-info") != 0 {
		t.Errorf("without a cap: status %s, started %q", tk.Status, started)
	}
}

// scopeDir makes a fake cgroup holding pids and returns its path.
func scopeDir(t *testing.T, pids string) string {
	t.Helper()
	root := t.TempDir()
	old := cgroupRoot
	cgroupRoot = root
	t.Cleanup(func() { cgroupRoot = old })
	path := "/app.slice/hrk-t1-1-1.scope"
	if err := os.MkdirAll(filepath.Join(root, path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, path, "cgroup.procs"), []byte(pids), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestScopedProcs(t *testing.T) {
	path := scopeDir(t, "101\n102\n999\n")
	got := scopedProcs(runnerTree, []string{path, "/app.slice/gone.scope"})
	if len(got) != 2 || got[0].PID != 101 || got[1].PID != 102 {
		t.Errorf("scoped = %+v", got)
	}
}
