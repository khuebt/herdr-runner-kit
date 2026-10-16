package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/config"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// cgroupRoot is where the unified cgroup hierarchy is mounted.
var cgroupRoot = "/sys/fs/cgroup"

// scopeUnit names the transient systemd scope an agent session runs in. The
// time suffix keeps it unique: a fallback switch keeps the attempt number,
// and processes the previous agent left behind keep its scope alive.
func scopeUnit(id string, attempt int, now time.Time) string {
	return fmt.Sprintf("hrk-%s-%d-%d.scope", id, attempt, now.Unix())
}

// memScope moves running processes into new memory-capped systemd scopes
// through the user manager's D-Bus API, with busctl. Nothing is typed into
// a pane and nothing is restarted.
type memScope struct {
	// memoryMax is runnerMemoryMax: a size such as 16G or a share of RAM.
	memoryMax string
	// busctl runs busctl with args and returns its combined output.
	busctl func(ctx context.Context, args ...string) ([]byte, error)
	// cgroup returns the cgroup path of a process, as /proc shows it.
	cgroup func(pid int) (string, error)
	// memTotal returns the machine's memory in bytes.
	memTotal func() (uint64, error)
	sleep    func(time.Duration)
}

// newMemScope returns the memory scope of the real system.
func newMemScope(memoryMax string) memScope {
	return memScope{memoryMax: memoryMax, busctl: runBusctl, cgroup: procCgroup, memTotal: memTotal, sleep: time.Sleep}
}

// SystemScope returns the Runner.EnterScope of this system: nil on systems
// without systemd and with runnerMemoryMax off.
func SystemScope(memoryMax, goos string) func(context.Context, string, []int) (string, error) {
	if memoryMax == config.MemoryOff || goos != "linux" {
		return nil
	}
	return newMemScope(memoryMax).enter
}

// ErrNoSystemd is ScopeProblem's answer on a system without systemd.
var ErrNoSystemd = errors.New("not available")

// ScopeProblem says why runners cannot run in a memory-capped systemd scope
// on goos, or returns nil when they can. It moves a short-lived process into
// a scope exactly as a launch moves an agent.
func ScopeProblem(ctx context.Context, memoryMax, goos string) error {
	if memoryMax == config.MemoryOff {
		return errors.New("runnerMemoryMax is off")
	}
	if goos != "linux" {
		if goos == "darwin" {
			goos = "macOS"
		}
		return fmt.Errorf("%w on %s", ErrNoSystemd, goos)
	}
	if _, err := exec.LookPath("busctl"); err != nil {
		return errors.New("busctl is not on PATH")
	}
	return newMemScope(memoryMax).probe(ctx)
}

// probe moves a sleeping child into its own scope, then ends it; the scope
// goes with it.
func (s memScope) probe(ctx context.Context) error {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	_, err := s.enter(ctx, fmt.Sprintf("hrk-probe-%d.scope", cmd.Process.Pid), []int{cmd.Process.Pid})
	return err
}

// enter moves pids into a new scope named unit, capped at memoryMax, and
// returns the scope's cgroup path once the first of pids is in it.
func (s memScope) enter(ctx context.Context, unit string, pids []int) (string, error) {
	maxBytes, err := s.bytes()
	if err != nil {
		return "", err
	}
	out, err := s.busctl(ctx, "get-property", "org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager", "Version")
	if err != nil {
		return "", busError("read the systemd version", out, err)
	}
	version := systemdVersion(string(out))
	args := []string{
		"call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager",
		"StartTransientUnit", "ssa(sv)a(sa(sv))", unit, "fail",
	}
	args = append(append(args, scopeProperties(pids, maxBytes, version)...), "0")
	if out, err := s.busctl(ctx, args...); err != nil {
		return "", busError("start "+unit, out, err)
	}
	// systemd moves the processes when the scope's start job runs, right
	// after the call returns.
	var path string
	for range 20 {
		if path, err = s.cgroup(pids[0]); err == nil && filepath.Base(path) == unit {
			return path, nil
		}
		s.sleep(50 * time.Millisecond)
	}
	if err != nil {
		return "", err
	}
	return "", fmt.Errorf("process %d is still in %s, not %s", pids[0], path, unit)
}

// scopeProperties returns the properties of a runner scope as busctl
// arguments: pids moved in, capped at maxBytes. MemorySwapMax=0 makes the cap
// hard: a command over it is killed instead of swapping. CollectMode drops
// the unit once it is empty, even when an OOM kill marked it failed. From
// systemd 253 on, a scope stops, killing every process in it, when the kernel
// kills one for memory, unless OOMPolicy is continue; older versions reject
// OOMPolicy for scopes and never stop one on an OOM kill. Every other
// property is accepted from systemd 236 on.
func scopeProperties(pids []int, maxBytes uint64, systemd int) []string {
	props := []string{"PIDs", "au", strconv.Itoa(len(pids))}
	for _, pid := range pids {
		props = append(props, strconv.Itoa(pid))
	}
	props = append(props,
		"MemoryMax", "t", strconv.FormatUint(maxBytes, 10),
		"MemorySwapMax", "t", "0",
		"CollectMode", "s", "inactive-or-failed")
	n := 4
	if systemd >= 253 {
		props = append(props, "OOMPolicy", "s", "continue")
		n++
	}
	return append([]string{strconv.Itoa(n)}, props...)
}

// bytes returns memoryMax in bytes; a share of RAM is taken of the
// machine's memory, as systemd does.
func (s memScope) bytes() (uint64, error) {
	v := s.memoryMax
	if pct, ok := strings.CutSuffix(v, "%"); ok {
		n, err := strconv.ParseUint(pct, 10, 64)
		if err != nil {
			return 0, err
		}
		total, err := s.memTotal()
		if err != nil {
			return 0, err
		}
		return total / 100 * n, nil
	}
	if v == "" {
		return 0, errors.New("no runnerMemoryMax")
	}
	shift := map[byte]uint{'K': 10, 'M': 20, 'G': 30, 'T': 40}[v[len(v)-1]]
	n, err := strconv.ParseUint(v[:len(v)-1], 10, 64)
	if err != nil || shift == 0 {
		return 0, fmt.Errorf("runnerMemoryMax %q is not a size", v)
	}
	return n << shift, nil
}

// systemdVersion reads the major version from busctl's answer for the
// manager's Version, such as `s "255.4-1ubuntu8.18"`, or returns 0.
func systemdVersion(out string) int {
	v := strings.TrimPrefix(strings.TrimSpace(out), `s "`)
	end := strings.IndexFunc(v, func(r rune) bool { return r < '0' || r > '9' })
	if end < 0 {
		end = len(v)
	}
	n, _ := strconv.Atoi(v[:end])
	return n
}

// busError names what failed with busctl's own first line.
func busError(what string, out []byte, err error) error {
	msg := firstLine(strings.TrimSpace(string(out)), 160)
	if msg == "" {
		msg = err.Error()
	}
	return fmt.Errorf("%s: %s", what, msg)
}

func runBusctl(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "busctl", append([]string{"--user"}, args...)...).CombinedOutput()
}

// procCgroup returns pid's cgroup in the unified hierarchy.
func procCgroup(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			return path, nil
		}
	}
	return "", fmt.Errorf("process %d is in no unified cgroup", pid)
}

func memTotal() (uint64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			kb, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64)
			return kb << 10, err
		}
	}
	return 0, errors.New("no MemTotal in /proc/meminfo")
}

// enterScope moves t's agent, which has just started in pane, and the
// processes it already started into a new memory-capped scope, so a runaway
// command meets its runner's cap instead of the memory every session
// shares. Each agent session gets its own; systemd drops a scope once it is
// empty. It records the scope on t and returns what the started event says
// about it. When no scope can be made, the agent keeps running where it is.
func (r *Runner) enterScope(ctx context.Context, t *task.Task, pane string) string {
	if r.EnterScope == nil {
		return ""
	}
	path, err := r.scopeAgent(ctx, t, pane)
	if err != nil {
		return "no memory scope: " + firstLine(err.Error(), 160)
	}
	if _, err := r.Store.Update(t.ID, func(t *task.Task) error {
		t.Scopes = append(t.Scopes, path)
		return nil
	}); err != nil {
		return "no memory scope: " + firstLine(err.Error(), 160)
	}
	return "memory scope " + filepath.Base(path)
}

func (r *Runner) scopeAgent(ctx context.Context, t *task.Task, pane string) (string, error) {
	info, err := r.Herdr.PaneProcessInfo(ctx, pane)
	if err != nil {
		return "", err
	}
	agent := info.ForegroundPGID
	if agent <= 0 || agent == info.ShellPID {
		return "", errors.New("herdr names no agent process in the pane")
	}
	// ponytail: a child that exits between the list and the call fails the
	// call, and one forked in between stays out; retry if that shows up.
	pids := []int{agent}
	if r.ProcTable != nil {
		if table, err := r.ProcTable(ctx); err == nil {
			for _, p := range descendants(table, agent) {
				pids = append(pids, p.PID)
			}
		}
	}
	return r.EnterScope(ctx, scopeUnit(t.ID, task.AttemptOf(t), time.Now()), pids)
}

// scopedProcs returns the processes of table that run in the cgroups of
// scopes. A scope systemd already dropped holds none.
func scopedProcs(table []proc, scopes []string) []proc {
	in := map[int]bool{}
	for _, path := range scopes {
		data, err := os.ReadFile(filepath.Join(cgroupRoot, path, "cgroup.procs"))
		if err != nil {
			continue
		}
		for f := range bytes.FieldsSeq(data) {
			if pid, err := strconv.Atoi(string(f)); err == nil {
				in[pid] = true
			}
		}
	}
	var out []proc
	for _, p := range table {
		if in[p.PID] {
			out = append(out, p)
		}
	}
	return out
}
