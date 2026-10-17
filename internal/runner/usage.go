package runner

import (
	"slices"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/harness"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// recordTranscript adds the transcript a hook of harness h named to t,
// once.
func (r *Runner) recordTranscript(t *task.Task, h task.Harness, path string) {
	tr := task.Transcript{Harness: h, Path: path}
	if path == "" || slices.Contains(t.Transcripts, tr) {
		return
	}
	_, _ = r.Store.Update(t.ID, func(t *task.Task) error {
		if !slices.Contains(t.Transcripts, tr) {
			t.Transcripts = append(t.Transcripts, tr)
		}
		return nil
	})
}

// usageWait bounds how long AwaitUsage waits for the agent to write its
// turn; closing the runner reads the usage again in any case.
const usageWait = 30 * time.Second

// AwaitUsage stores a task's usage once its current session's transcript
// holds its turn to the end, or usageWait has passed. It runs in the
// detached child a turn end starts: an agent may write a turn's last call
// only after its turn-end hooks return, so the hook itself never reads that
// call.
func (r *Runner) AwaitUsage(id string) {
	t, err := r.Store.Get(id)
	if err != nil {
		return
	}
	const step = 200 * time.Millisecond
	for waited := time.Duration(0); waited < usageWait && !turnsWritten(t.Transcripts); waited += step {
		r.Sleep(step)
	}
	r.refreshUsage(id)
}

// turnsWritten reports whether the last transcript, the current session's,
// holds its latest turn to the end. An earlier session the launcher quit
// after a handoff may end on a tool call for good.
func turnsWritten(transcripts []task.Transcript) bool {
	if len(transcripts) == 0 {
		return true
	}
	tr := transcripts[len(transcripts)-1]
	a, err := harness.For(tr.Harness)
	return err != nil || a.TurnWritten(tr.Path)
}

// refreshUsage stores the token usage read from the task's transcripts.
// Usage is best effort: an unreadable transcript leaves what was stored,
// and no error reaches the caller.
func (r *Runner) refreshUsage(id string) {
	t, err := r.Store.Get(id)
	if err != nil {
		return
	}
	u := ReadUsage(t.Transcripts, time.Time{}, time.Time{})
	if u == nil {
		return
	}
	_, _ = r.Store.Update(id, func(t *task.Task) error {
		t.Usage = u
		return nil
	})
}

// ReadUsage sums the usage in transcripts, each read by the harness that
// wrote it, counting calls between from and to (zero leaves a side open).
// It is nil when none could be read.
func ReadUsage(transcripts []task.Transcript, from, to time.Time) *task.Usage {
	byHarness := map[task.Harness][]string{}
	var order []task.Harness
	for _, tr := range transcripts {
		if _, ok := byHarness[tr.Harness]; !ok {
			order = append(order, tr.Harness)
		}
		byHarness[tr.Harness] = append(byHarness[tr.Harness], tr.Path)
	}
	var sum *task.Usage
	for _, h := range order {
		a, err := harness.For(h)
		if err != nil {
			continue
		}
		u, err := a.Usage(byHarness[h], from, to)
		if err != nil || u == nil {
			continue
		}
		if sum == nil {
			sum = &task.Usage{}
		}
		sum.Add(u)
		sum.At = u.At
	}
	return sum
}
