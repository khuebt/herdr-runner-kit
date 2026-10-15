package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/fsutil"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// progressEvery is how often WatchProgress samples a working agent. A
// variable so tests can shorten it.
var progressEvery = time.Minute

// Progress check thresholds.
const (
	// minSamples is how many samples with no progress the ladder needs
	// before it acts, so one slow sample never escalates.
	minSamples = 3
	// retryEscalation is how many stallAfter periods of visible retrying
	// pass before a single stalled line says so.
	retryEscalation = 3
	// interruptSettle is how long an interrupted agent may take to stop.
	interruptSettle = 10 * time.Second
	// progressScreenLines is how much of the pane a sample reads.
	progressScreenLines = 40
)

// Ladder steps, each taken once per stall.
const (
	stepNone = iota
	stepNudged
	stepInterrupted
	stepStalled
)

// progressState is what the progress check remembers between samples, in
// tasks/<id>/progress.json. It lives outside task.json so that sampling never
// moves the task's UpdatedAt. The file also holds the context state, which
// outlives each stall.
type progressState struct {
	contextState
	Fingerprint string    `json:"fingerprint"`
	Revision    int       `json:"revision"`
	Seq         int       `json:"seq"`
	UpdatedAt   time.Time `json:"updatedAt"` // the task's, when last sampled
	Since       time.Time `json:"since"`     // last progress, or the last step
	Began       time.Time `json:"began,omitzero"`
	Samples     int       `json:"samples"`
	Step        int       `json:"step"`
	StepAt      time.Time `json:"stepAt,omitzero"` // when the last step was taken
	RetrySince  time.Time `json:"retrySince,omitzero"`
	RetryNoted  bool      `json:"retryNoted,omitempty"`
}

// WatchProgress samples a running task's agent once per progressEvery
// while ctx lasts, and climbs the stall ladder when a working agent shows no
// progress for stallAfter: first a queued nudge, then an interrupt and a
// resume prompt, then stalled news with the runner left open. It returns
// when the task finishes or its runner closes.
func (r *Runner) WatchProgress(ctx context.Context, id string) {
	if r.Config.StallAfter <= 0 {
		return
	}
	for sleepCtx(ctx, progressEvery) {
		t, err := r.Store.Get(id)
		if err != nil || t.Status.Terminal() || t.Herdr.PaneID == "" || t.Herdr.Closed {
			return
		}
		r.checkProgress(ctx, t, time.Now())
	}
}

// checkProgress takes one sample of t's agent at now and acts on it. Two
// watchers of one task take turns under the task's progress lock, so each
// step happens once.
func (r *Runner) checkProgress(ctx context.Context, t *task.Task, now time.Time) {
	adapter, ok := adapterOf(t)
	if !ok {
		return
	}
	unlock, err := r.Store.LockFile(t.ID, "progress.lock")
	if err != nil {
		return
	}
	defer unlock()
	if t, err = r.Store.Get(t.ID); err != nil {
		return
	}
	st := r.loadProgress(t.ID)
	if !st.UpdatedAt.Equal(t.UpdatedAt) {
		// A report, send, answer or turn end: the runner moved on.
		st = progressState{contextState: st.contextState, UpdatedAt: t.UpdatedAt, Since: now}
	}
	a, err := r.Herdr.AgentGet(ctx, t.Herdr.PaneID)
	if t.Status != task.Running || t.Stalled || err != nil || a.Status != "working" {
		// Not working: nothing can hang, and the next working stretch is
		// measured from here.
		st.Since, st.Samples, st.RetrySince, st.RetryNoted = now, 0, time.Time{}, false
		r.saveProgress(t.ID, st)
		return
	}
	screen, err := r.Herdr.PaneRead(ctx, t.Herdr.PaneID, progressScreenLines)
	if err != nil {
		return
	}
	sum := sha256.Sum256([]byte(adapter.ProgressText(screen)))
	fp := hex.EncodeToString(sum[:])
	if fp != st.Fingerprint || a.Revision != st.Revision || a.StateChangeSeq != st.Seq {
		st.Since, st.Samples = now, 0
	}
	st.Fingerprint, st.Revision, st.Seq = fp, a.Revision, a.StateChangeSeq
	st.Samples++
	stallAfter := r.Config.StallAfter
	if adapter.ShowsRetry(screen) {
		// Retrying is progress of a kind: it never escalates, but a long
		// run of it is worth one line.
		st.Since, st.Samples = now, 0
		if st.RetrySince.IsZero() {
			st.RetrySince = now
		}
		if span := now.Sub(st.RetrySince); !st.RetryNoted && span >= retryEscalation*stallAfter {
			st.RetryNoted = true
			r.emit(t, "stalled", fmt.Sprintf("retrying for %d min; the runner is left open", int(span.Minutes())), "")
		}
		r.saveProgress(t.ID, st)
		return
	}
	st.RetrySince, st.RetryNoted = time.Time{}, false
	if st.Samples >= minSamples && now.Sub(st.Since) >= stallAfter {
		// A reply to a step, such as the agent saying what it waits for,
		// does not undo it, but work for a whole stallAfter after it does:
		// this is a new stall, which starts with a nudge again.
		if st.Step == stepNone || st.Since.Sub(st.StepAt) >= stallAfter {
			st.Step, st.Began = stepNone, st.Since
		}
		if st.Step < stepStalled {
			st.Step++
			r.climb(ctx, t, st.Step, now.Sub(st.Began))
			st.Since, st.Samples, st.StepAt = now, 0, now
		}
	}
	r.saveProgress(t.ID, st)
}

// climb takes one step of the stall ladder for t, which has shown no
// progress for idle.
func (r *Runner) climb(ctx context.Context, t *task.Task, step int, idle time.Duration) {
	minutes := int(idle.Minutes())
	pane := t.Herdr.PaneID
	switch step {
	case stepNudged:
		// Queued, never waited on: the agent reads it after its current step.
		text := fmt.Sprintf("No visible progress for %d minutes. If a command or network call is hanging, stop it and continue; otherwise say what you are waiting for.", minutes)
		if err := r.Herdr.AgentPrompt(ctx, pane, text); err != nil {
			return
		}
		r.emit(t, "nudged", fmt.Sprintf("no visible progress for %d min; nudged", minutes), "")
	case stepInterrupted:
		before := t.Unreported
		_ = r.Herdr.AgentSendKeys(ctx, pane, "esc")
		if status, err := r.Herdr.AgentWait(ctx, pane, interruptSettle, "idle", "done"); err != nil || status == "working" {
			_ = r.Herdr.AgentSendKeys(ctx, pane, "ctrl+c") // once: two would quit the agent
		}
		summary := fmt.Sprintf("no visible progress for %d min after a nudge; interrupted", minutes)
		if cur, err := r.Store.Get(t.ID); err == nil && cur.Unreported == before {
			text := fmt.Sprintf("You were interrupted after %d minutes without visible progress. Continue the task; run a command that may hang with a timeout, or skip it and say so in your report.", minutes)
			if r.Herdr.AgentPrompt(ctx, pane, text) == nil {
				summary += " and resumed"
			}
		}
		r.emit(t, "interrupted", summary, "")
	case stepStalled:
		r.emit(t, "stalled", fmt.Sprintf("no progress for %d min after a nudge and an interrupt; the runner is left open", minutes), "")
	}
}

func (r *Runner) progressPath(id string) string {
	return filepath.Join(r.Store.TaskDir(id), "progress.json")
}

// loadProgress reads the task's progress state; a missing or unreadable file
// starts afresh.
func (r *Runner) loadProgress(id string) progressState {
	var st progressState
	data, err := os.ReadFile(r.progressPath(id))
	if err == nil && json.Unmarshal(data, &st) == nil {
		return st
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "hrk: %s: read progress: %v\n", id, err)
	}
	return progressState{}
}

func (r *Runner) saveProgress(id string, st progressState) {
	data, err := json.Marshal(st)
	if err == nil {
		err = fsutil.WriteFileAtomic(r.progressPath(id), data, 0o600)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "hrk: %s: save progress: %v\n", id, err)
	}
}
