package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	base := "PIDs au 1 11 MemoryMax t 536870912 MemorySwapMax t 0 CollectMode s inactive-or-failed Delegate b true"
	if got := strings.Join(scopeProperties(11, 512<<20, false), " "); got != "5 "+base {
		t.Errorf("without OOMPolicy: %s", got)
	}
	if got := strings.Join(scopeProperties(11, 512<<20, true), " "); got != "6 "+base+" OOMPolicy s continue" {
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

// bus is a fake user manager: it records busctl calls and keeps the cgroup
// of each process that moved; the others run in herdr's service.
type bus struct {
	calls   [][]string
	cgroups map[int]string
	// old rejects OOMPolicy, as systemd 249 does; gone processes have
	// exited, and stuck ones refuse to move.
	old         bool
	gone, stuck map[int]bool
}

func (b *bus) cgroup(pid int) (string, error) {
	if b.gone[pid] {
		return "", os.ErrNotExist
	}
	if cg, ok := b.cgroups[pid]; ok {
		return cg, nil
	}
	return "/app.slice/herdr.service", nil
}

// fakeBus returns a memScope on a fake manager.
func fakeBus(old bool) (memScope, *bus) {
	b := &bus{cgroups: map[int]string{}, old: old, gone: map[int]bool{}, stuck: map[int]bool{}}
	s := memScope{
		memoryMax: "25%",
		memTotal:  func() (uint64, error) { return 4 << 30, nil },
		sleep:     func(time.Duration) {},
		cgroup:    b.cgroup,
	}
	s.busctl = func(_ context.Context, args ...string) ([]byte, error) {
		b.calls = append(b.calls, args)
		unit := args[6]
		switch args[4] {
		case "StartTransientUnit":
			if b.old && slices.Contains(args, "OOMPolicy") {
				return []byte("Call failed: Cannot set property OOMPolicy, or unknown property."), errors.New("exit status 1")
			}
			pid, _ := strconv.Atoi(args[12])
			if b.gone[pid] {
				return []byte("Call failed: Process with ID " + args[12] + " does not exist."), errors.New("exit status 1")
			}
			b.cgroups[pid] = "/app.slice/" + unit
			return []byte(`o "/org/freedesktop/systemd1/job/1"`), nil
		case "AttachProcessesToUnit":
			pid, _ := strconv.Atoi(args[9])
			if b.gone[pid] || b.stuck[pid] {
				return []byte("Call failed: Process with ID " + args[9] + " does not exist."), errors.New("exit status 1")
			}
			b.cgroups[pid] = "/app.slice/" + unit
			return nil, nil
		}
		return nil, errors.New("unexpected call")
	}
	return s, b
}

func (b *bus) named(method string) [][]string {
	var out [][]string
	for _, c := range b.calls {
		if c[4] == method {
			out = append(out, c)
		}
	}
	return out
}

func TestProbeAndLaunchShareProperties(t *testing.T) {
	for _, old := range []bool{false, true} {
		s, b := fakeBus(old)
		if err := s.probe(t.Context()); err != nil {
			t.Fatal(err)
		}
		probe := b.named("StartTransientUnit")
		b.calls = nil
		sc, err := s.enter(t.Context(), "hrk-t1-1-1.scope", 101, func() []int { return nil })
		if err != nil || sc.Path != "/app.slice/hrk-t1-1-1.scope" {
			t.Fatalf("enter = %+v, %v", sc, err)
		}
		launch := b.named("StartTransientUnit")
		if want := map[bool]int{false: 1, true: 2}[old]; len(probe) != want || len(launch) != want {
			t.Fatalf("old %v: probe %q, launch %q", old, probe, launch)
		}
		for i := range probe {
			if !slices.Equal(probe[i][8:12], launch[i][8:12]) || !slices.Equal(probe[i][13:], launch[i][13:]) || !strings.HasPrefix(probe[i][6], "hrk-probe-") {
				t.Errorf("old %v: probe %q, launch %q", old, probe[i], launch[i])
			}
		}
		if note := strings.Join(sc.Notes, "; "); old != strings.Contains(note, "systemd rejected OOMPolicy=continue, so an OOM kill may end the runner") {
			t.Errorf("old %v: notes %q", old, sc.Notes)
		}
	}
}

func TestEnterChecksTheProcessMoved(t *testing.T) {
	s, _ := fakeBus(false)
	s.cgroup = func(int) (string, error) { return "/app.slice/herdr.service", nil }
	if _, err := s.enter(t.Context(), "hrk-t1-1-1.scope", 101, func() []int { return nil }); err == nil || !strings.Contains(err.Error(), "still in /app.slice/herdr.service") {
		t.Errorf("err = %v", err)
	}
}

func TestEnterStartsWithTheAgentAlone(t *testing.T) {
	s, b := fakeBus(false)
	// 103 exited after it was listed; 102 exits while it would move.
	b.gone[103] = true
	b.stuck[102] = true
	sc, err := s.enter(t.Context(), "hrk-t1-1-1.scope", 101, func() []int {
		if len(b.named("AttachProcessesToUnit")) > 0 {
			b.gone[102] = true
		}
		return []int{102, 103}
	})
	if err != nil || len(sc.Notes) != 0 || b.cgroups[101] != sc.Path {
		t.Fatalf("enter = %+v, %v", sc, err)
	}
	if start := b.named("StartTransientUnit"); len(start) != 1 || start[0][11] != "1" || start[0][12] != "101" {
		t.Errorf("start = %q", start)
	}
}

func TestEnterAttachesLateChildren(t *testing.T) {
	s, b := fakeBus(false)
	rounds := 0
	sc, err := s.enter(t.Context(), "hrk-t1-1-1.scope", 101, func() []int {
		rounds++
		if rounds >= 2 {
			return []int{102, 103} // an MCP server forked while 102 moved
		}
		return []int{102}
	})
	if err != nil || len(sc.Notes) != 0 || b.cgroups[102] != sc.Path || b.cgroups[103] != sc.Path || rounds != 3 {
		t.Fatalf("enter = %+v, %v; cgroups %v after %d rounds", sc, err, b.cgroups, rounds)
	}

	s, b = fakeBus(false)
	b.stuck[104] = true
	sc, err = s.enter(t.Context(), "hrk-t1-1-1.scope", 101, func() []int { return []int{104} })
	if err != nil || !slices.Equal(sc.Notes, []string{"1 of its processes stayed outside"}) || len(b.named("AttachProcessesToUnit")) != attachRounds {
		t.Errorf("stuck child: %+v, %v, %d attaches", sc, err, len(b.named("AttachProcessesToUnit")))
	}
}

// scopedLaunch launches a runner whose agent is pid 101 under shell 100,
// with enter as its EnterScope, and returns the task and its started event.
func scopedLaunch(t *testing.T, enter func(context.Context, string, int, func() []int) (Scope, error)) (*Runner, *fakeHerdr, *task.Task, string) {
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
	var agent int
	var children []int
	_, fake, tk, started := scopedLaunch(t, func(_ context.Context, u string, a int, c func() []int) (Scope, error) {
		unit, agent, children = u, a, c()
		return Scope{Path: "/user.slice/app.slice/" + u, Notes: []string{"1 of its processes stayed outside"}}, nil
	})
	if !strings.HasPrefix(unit, "hrk-t1-1-") || agent != 101 || !slices.Equal(children, []int{102}) {
		t.Errorf("entered %q with %d and %v", unit, agent, children)
	}
	if !strings.Contains(started, "memory scope "+unit+" (1 of its processes stayed outside)") ||
		!slices.Equal(tk.Scopes, []string{"/user.slice/app.slice/" + unit}) || tk.ShellPID != 100 {
		t.Errorf("started %q, scopes %q, shell %d", started, tk.Scopes, tk.ShellPID)
	}
	if n := fake.count("pane", "run"); n != 0 {
		t.Errorf("%d pane runs typed", n)
	}
}

func TestLaunchWithoutAScope(t *testing.T) {
	_, _, tk, started := scopedLaunch(t, func(context.Context, string, int, func() []int) (Scope, error) {
		return Scope{}, errors.New("start hrk-t1-1-1.scope: Unknown assignment")
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
