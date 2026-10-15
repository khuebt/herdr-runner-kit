package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/redact"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// proc is one row of the process table. Start is the start time as ps
// prints it, which tells a process from a later one that reused its pid.
type proc struct {
	PID, PPID int
	Start     string
	Command   string
}

// survivorWait bounds how long a close waits for the runner's processes to
// exit after its agent quit, and survivorPoll is how often it looks.
const (
	survivorWait = 3 * time.Second
	survivorPoll = 500 * time.Millisecond
)

// maxNamedSurvivors is how many leftover processes a news line names; the
// rest are counted.
const maxNamedSurvivors = 3

// psTable lists every process with its parent, start time and command. The
// C locale keeps the start time in one format on Linux and macOS.
func psTable(ctx context.Context) ([]proc, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=,lstart=,command=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	return parsePS(string(out))
}

// parsePS reads `ps -o pid=,ppid=,lstart=,command=` output. lstart is five
// words, such as "Thu Oct 15 01:21:00 2026".
func parsePS(out string) ([]proc, error) {
	var procs []proc
	for line := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		words, rest := cutWords(line, 7)
		if len(words) < 7 {
			return nil, fmt.Errorf("ps: unexpected line %q", line)
		}
		pid, err1 := strconv.Atoi(words[0])
		ppid, err2 := strconv.Atoi(words[1])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("ps: unexpected line %q", line)
		}
		procs = append(procs, proc{PID: pid, PPID: ppid, Start: strings.Join(words[2:7], " "), Command: strings.TrimSpace(rest)})
	}
	return procs, nil
}

// cutWords splits off the first n space-separated words of s and returns
// them with the rest of s as is.
func cutWords(s string, n int) ([]string, string) {
	var words []string
	for len(words) < n {
		s = strings.TrimLeft(s, " \t")
		if s == "" {
			break
		}
		i := strings.IndexAny(s, " \t")
		if i < 0 {
			words, s = append(words, s), ""
			break
		}
		words, s = append(words, s[:i]), s[i:]
	}
	return words, s
}

// descendants returns every process below root in the table, without root.
func descendants(table []proc, root int) []proc {
	children := map[int][]proc{}
	for _, p := range table {
		if p.PID != p.PPID {
			children[p.PPID] = append(children[p.PPID], p)
		}
	}
	var out []proc
	queue := []int{root}
	seen := map[int]bool{root: true}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, c := range children[pid] {
			if !seen[c.PID] {
				seen[c.PID] = true
				out = append(out, c)
				queue = append(queue, c.PID)
			}
		}
	}
	return out
}

// survivors returns the processes of before that still run in table: same
// pid and same start time, so a reused pid is not mistaken for one.
func survivors(before, table []proc) []proc {
	now := make(map[int]string, len(table))
	for _, p := range table {
		now[p.PID] = p.Start
	}
	var out []proc
	for _, p := range before {
		if start, ok := now[p.PID]; ok && start == p.Start {
			out = append(out, p)
		}
	}
	return out
}

// snapshotProcs records the processes the runner's pane started, before its
// agent quits: once the agent exits, its children lose their parent and can
// no longer be traced to the runner through the tree, only through the
// runner environment they inherited. ok is false when there is nothing to
// check later, and a failure is recorded as process-check-skipped.
func (r *Runner) snapshotProcs(ctx context.Context, t *task.Task) ([]proc, bool) {
	if r.ProcTable == nil {
		return nil, false
	}
	info, err := r.Herdr.PaneProcessInfo(ctx, t.Herdr.PaneID)
	if err == nil && info.ShellPID <= 0 {
		err = fmt.Errorf("herdr named no shell for %s", t.Herdr.PaneID)
	}
	if err != nil {
		r.emit(t, "process-check-skipped", "could not list the runner's processes: "+firstLine(err.Error(), 160), "")
		return nil, false
	}
	table, err := r.ProcTable(ctx)
	if err != nil {
		r.emit(t, "process-check-skipped", "could not list the runner's processes: "+firstLine(err.Error(), 160), "")
		return nil, false
	}
	return append(descendants(table, info.ShellPID), r.markedProcs(table, t, info.ShellPID)...), true
}

// procEnviron reads a process's environment where the system shows it, in
// /proc on Linux.
func procEnviron(pid int) ([]byte, error) {
	return os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
}

// markedProcs returns the processes outside the pane shell's tree that carry
// t's runner environment: a command the agent started with & in a shell that
// exited at once, as Claude's Bash tool does, is re-parented to init or a
// subreaper long before the close. The shell, its tree and this process are
// left out.
func (r *Runner) markedProcs(table []proc, t *task.Task, shell int) []proc {
	if r.ProcEnv == nil {
		return nil
	}
	inTree := map[int]bool{shell: true, os.Getpid(): true}
	for _, p := range descendants(table, shell) {
		inTree[p.PID] = true
	}
	marks := [][]byte{[]byte("HRK_TASK=" + t.ID), []byte("HRK_STATE_DIR=" + r.Store.Dir())}
	var out []proc
	for _, p := range table {
		if inTree[p.PID] {
			continue
		}
		env, err := r.ProcEnv(p.PID)
		if err != nil {
			continue
		}
		if hasEnv(env, marks) {
			out = append(out, p)
		}
	}
	return out
}

// hasEnv reports whether a NUL-separated environment holds every one of
// entries exactly.
func hasEnv(env []byte, entries [][]byte) bool {
	for _, want := range entries {
		found := false
		for e := range bytes.SplitSeq(env, []byte{0}) {
			if bytes.Equal(e, want) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// reportSurvivors waits briefly for the processes of before to exit, then
// names the ones still running in one processes-left event. It never
// signals them: they may be a dev server the user still wants.
func (r *Runner) reportSurvivors(ctx context.Context, t *task.Task, before []proc) {
	if len(before) == 0 {
		return
	}
	var left []proc
	for waited := time.Duration(0); ; waited += survivorPoll {
		table, err := r.ProcTable(ctx)
		if err != nil {
			r.emit(t, "process-check-skipped", "could not list the runner's processes: "+firstLine(err.Error(), 160), "")
			return
		}
		if left = survivors(before, table); len(left) == 0 || waited >= survivorWait {
			break
		}
		r.Sleep(survivorPoll)
	}
	if len(left) == 0 {
		return
	}
	r.emit(t, "processes-left", survivorSummary(left), "")
}

// survivorSummary names up to maxNamedSurvivors leftover processes, with
// their commands redacted, and counts the rest.
func survivorSummary(left []proc) string {
	noun := "processes"
	if len(left) == 1 {
		noun = "process"
	}
	named := make([]string, 0, maxNamedSurvivors)
	for _, p := range left[:min(len(left), maxNamedSurvivors)] {
		named = append(named, fmt.Sprintf("%d `%s`", p.PID, firstLine(redact.Secrets(p.Command), 60)))
	}
	list := strings.Join(named, ", ")
	if extra := len(left) - len(named); extra > 0 {
		list += fmt.Sprintf(" and %d more", extra)
	}
	return fmt.Sprintf("%d %s the runner started still run after its tab closed: %s (stop them yourself if not needed)", len(left), noun, list)
}
