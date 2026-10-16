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

// A Scope is the memory scope an agent session runs in.
type Scope struct {
	// Path is the scope's cgroup.
	Path string
	// Notes say what the started event adds about it, such as processes
	// that stayed outside.
	Notes []string
}

// SystemScope returns the Runner.EnterScope of this system: nil on systems
// without systemd and with runnerMemoryMax off.
func SystemScope(memoryMax, goos string) func(context.Context, string, int, func() []int) (Scope, error) {
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
	_, err := s.enter(ctx, fmt.Sprintf("hrk-probe-%d.scope", cmd.Process.Pid), cmd.Process.Pid, func() []int { return nil })
	return err
}

// attachRounds bounds how often enter moves the agent's children that are
// still outside its scope: a child can fork while the ones before it move.
const attachRounds = 3

// enter moves agent into a new scope named unit, capped at memoryMax, and
// returns the scope once agent is in it. The scope starts with agent alone,
// so a child that exits meanwhile cannot fail it; then the processes
// children lists, agent's descendants, join it, until none is left outside.
// One that exited on the way is not an error.
func (s memScope) enter(ctx context.Context, unit string, agent int, children func() []int) (Scope, error) {
	maxBytes, err := s.bytes()
	if err != nil {
		return Scope{}, err
	}
	var sc Scope
	out, err := s.busctl(ctx, startArgs(unit, scopeProperties(agent, maxBytes, true))...)
	if err != nil && strings.Contains(string(out), "OOMPolicy") {
		out, err = s.busctl(ctx, startArgs(unit, scopeProperties(agent, maxBytes, false))...)
		sc.Notes = append(sc.Notes, "systemd rejected OOMPolicy=continue, so an OOM kill may end the runner")
	}
	if err != nil {
		return Scope{}, busError("start "+unit, out, err)
	}
	// systemd moves the agent when the scope's start job runs, right after
	// the call returns.
	for range 20 {
		if sc.Path, err = s.cgroup(agent); err == nil && filepath.Base(sc.Path) == unit {
			break
		}
		s.sleep(50 * time.Millisecond)
	}
	if err != nil {
		return Scope{}, err
	}
	if filepath.Base(sc.Path) != unit {
		return Scope{}, fmt.Errorf("process %d is still in %s, not %s", agent, sc.Path, unit)
	}
	for round := 0; ; round++ {
		var outside []int
		for _, pid := range children() {
			if path, err := s.cgroup(pid); err == nil && path != sc.Path {
				outside = append(outside, pid)
			}
		}
		if len(outside) == 0 {
			break
		}
		if round == attachRounds {
			sc.Notes = append(sc.Notes, fmt.Sprintf("%d of its processes stayed outside", len(outside)))
			break
		}
		// One call per process: a call with one that exited fails whole.
		for _, pid := range outside {
			_, _ = s.busctl(ctx, "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager",
				"AttachProcessesToUnit", "ssau", unit, "", "1", strconv.Itoa(pid))
		}
	}
	return sc, nil
}

// startArgs returns the busctl arguments that start the scope unit with
// props.
func startArgs(unit string, props []string) []string {
	args := []string{
		"call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager",
		"StartTransientUnit", "ssa(sv)a(sa(sv))", unit, "fail",
	}
	return append(append(args, props...), "0")
}

// scopeProperties returns the properties of a runner scope as busctl
// arguments: agent moved in, capped at maxBytes. MemorySwapMax=0 makes the
// cap hard: a command over it is killed instead of swapping. CollectMode
// drops the unit once it is empty, even when an OOM kill marked it failed.
// Delegate lets the agent's children join it later, through
// AttachProcessesToUnit. Where systemd handles OOM kills in scopes, as
// Debian 12's 252 and 255 do, a scope stops, killing the agent too, when
// the kernel kills one of its processes, unless oomPolicy adds
// OOMPolicy=continue. Ubuntu 22.04's 249 rejects the property and keeps the
// scope running; some 252 releases reject it and still end the scope.
// Every other property is accepted from systemd 238 on.
func scopeProperties(agent int, maxBytes uint64, oomPolicy bool) []string {
	props := []string{
		"PIDs", "au", "1", strconv.Itoa(agent),
		"MemoryMax", "t", strconv.FormatUint(maxBytes, 10),
		"MemorySwapMax", "t", "0",
		"CollectMode", "s", "inactive-or-failed",
		"Delegate", "b", "true",
	}
	n := 5
	if oomPolicy {
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
// processes it started into a new memory-capped scope, so a runaway command
// meets its runner's cap instead of the memory every session shares. Each
// agent session gets its own; systemd drops a scope once it is empty. It
// records the scope on t and returns what the started
// event says about it. When no scope can be made, the agent keeps running
// where it is.
func (r *Runner) enterScope(ctx context.Context, t *task.Task, pane string) string {
	if r.EnterScope == nil {
		return ""
	}
	sc, err := r.scopeAgent(ctx, t, pane)
	if err == nil {
		_, err = r.Store.Update(t.ID, func(t *task.Task) error {
			t.Scopes = append(t.Scopes, sc.Path)
			return nil
		})
	}
	if err != nil {
		return "no memory scope: " + firstLine(err.Error(), 160)
	}
	line := "memory scope " + filepath.Base(sc.Path)
	if len(sc.Notes) > 0 {
		line += " (" + strings.Join(sc.Notes, "; ") + ")"
	}
	return line
}

func (r *Runner) scopeAgent(ctx context.Context, t *task.Task, pane string) (Scope, error) {
	info, err := r.Herdr.PaneProcessInfo(ctx, pane)
	if err != nil {
		return Scope{}, err
	}
	agent := info.ForegroundPGID
	if agent <= 0 || agent == info.ShellPID {
		return Scope{}, errors.New("herdr names no agent process in the pane")
	}
	children := func() []int {
		if r.ProcTable == nil {
			return nil
		}
		table, err := r.ProcTable(ctx)
		if err != nil {
			return nil
		}
		var pids []int
		for _, p := range descendants(table, agent) {
			pids = append(pids, p.PID)
		}
		return pids
	}
	return r.EnterScope(ctx, scopeUnit(t.ID, task.AttemptOf(t), time.Now()), agent, children)
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
