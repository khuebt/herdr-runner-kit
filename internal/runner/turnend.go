package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/harness"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// TurnEnd handles a runner's turn-end hook. A turn-end is a hint, never a
// verdict: the first turn that ends without a report is nudged, the second
// becomes news, and later ones are ignored until the main session prompts
// the runner again. block holds the nudge text, which the hook returns when
// the adapter nudges in the hook; otherwise it is typed into the pane.
//
// A usage limit switches the runner to its next fallback agent. A limit the
// agent confirms fails the task when no fallback is left. A limit only seen
// on screen switches when a fallback is left and otherwise becomes
// usage-limit news next to the nudge.
func (r *Runner) TurnEnd(ctx context.Context, id string, h task.Harness, payload []byte) (block string, err error) {
	adapter, err := harness.For(h)
	if err != nil {
		return "", err
	}
	t, err := r.Store.Get(id)
	if err != nil {
		return "", err
	}
	// The transcript belongs to the task whatever the attempt: a session
	// that handed off holds the task's first calls.
	r.recordTranscript(t, h, adapter.HookTranscript(payload))
	// A session that handed off may end a turn before the launcher quits
	// it; only the current session's turns count.
	if r.callerAttempt() != task.AttemptOf(t) {
		return "", nil
	}
	sig, err := adapter.ReadTurnEnd(harness.TurnInput{Payload: payload, Screen: r.screen(ctx, t)})
	if err != nil {
		return "", err
	}
	// The turn after a report holds the session's last calls, so a task
	// that finished, or finishes now with a held done, has its usage read
	// once the turn end is handled, or, while the agent has still to write
	// the turn's last call, by a detached child once it has.
	defer func() {
		if cur, err := r.Store.Get(id); err == nil && cur.Status.Terminal() && len(cur.Transcripts) > 0 {
			if turnsWritten(cur.Transcripts) {
				r.refreshUsage(id)
			} else if err := r.StartUsage(id); err != nil {
				fmt.Fprintf(os.Stderr, "hrk: %s: start usage reader: %v\n", id, err)
			}
		}
	}()
	if sig.LimitConfirmed {
		if t.Herdr.PaneID == "" {
			return "", nil
		}
		return "", r.switchAgent(t, true)
	}
	if sig.Ignore {
		return "", nil
	}
	if ignore, err := r.otherThread(t, sig.Thread); ignore || err != nil {
		return "", err
	}
	if t.PendingDone != nil || !t.QueuedSend.IsZero() {
		if settled, err := r.settleTurn(t.ID); settled || err != nil {
			return "", err
		}
	}
	if t.Status != task.Running || t.Herdr.PaneID == "" {
		return "", nil
	}
	limited := sig.LimitOnScreen
	if limited {
		// An agent out of usage cannot act on a nudge.
		if err := r.switchAgent(t, false); !errors.Is(err, errNoFallback) {
			return "", err
		}
	} else if sig.ContextKnown {
		// A runner near its context limit is asked to hand off instead of
		// being nudged, and the turn does not count as unreported.
		if asked, err := r.contextLow(ctx, t, sig.ContextLeft); asked || err != nil {
			return "", err
		}
	}
	if sig.Background {
		return "", nil
	}
	t, err = r.Store.Update(id, func(t *task.Task) error {
		if t.Status != task.Running {
			return errNotRunning
		}
		t.Unreported++
		return nil
	})
	if errors.Is(err, errNotRunning) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	switch t.Unreported {
	case 1:
		nudge := r.nudgeText()
		if r.handoffPending(t) {
			nudge = r.handoffRequest(t)
		}
		if !adapter.NudgeInHook() {
			if err := r.typePrompt(ctx, t, t.Herdr.PaneID, nudge); err != nil {
				return "", fmt.Errorf("nudge: %w", err)
			}
		}
		r.emit(t, "nudged", "turn ended without a report; asked the runner to report", "")
		if limited {
			r.emit(t, "usage-limit", limitNews, "")
		}
		return nudge, nil
	case 2:
		summary := "ended its turn again without reporting; it may wait for background work or need input"
		if limited {
			summary = "ended its turn again without reporting; " + limitNews
		}
		r.emit(t, "turn-ended-unreported", summary, "")
	}
	return "", nil
}

// settleTurn handles the turn end of a task that holds a done or has a
// message sent while its agent worked, and reports whether the turn end
// needs nothing more. The agent reads such a message within the running
// turn, or in the next one when the turn ended as it was sent. A done
// reported after the message answers it; one reported before is superseded,
// and the turn end goes on to the nudge, since the runner has not reported
// on the message. Otherwise the held done becomes final.
func (r *Runner) settleTurn(id string) (bool, error) {
	var finish, superseded bool
	t, err := r.Store.Update(id, func(t *task.Task) error {
		p := t.PendingDone
		if sent := t.QueuedSend; !sent.IsZero() {
			t.QueuedSend = time.Time{}
			if p != nil && !p.Superseded && !p.Since.After(sent) {
				p.Superseded, p.Since, superseded = true, time.Now().UTC(), true
			}
		}
		finish = p != nil && !p.Superseded
		return nil
	})
	if err != nil {
		return false, err
	}
	if superseded {
		r.emit(t, "done-superseded", "a message sent while the runner worked came after its done; it reports again", "")
	}
	if !finish {
		return false, nil
	}
	if _, err := r.FinishDone(id, false); errors.Is(err, errNothingHeld) {
		return false, nil // a report or another turn end took it first
	} else if err != nil {
		return false, err
	}
	return true, nil
}

// limitNews tells the main session about a usage limit seen on a runner's
// screen that no fallback agent can take over.
const limitNews = "the runner appears to be at its usage limit, and no quotaFallback agent is left to switch to"

var errNotRunning = errors.New("not running")

// otherThread reports whether thread is not the runner's own. The first
// thread an agent reports is the runner's, and is recorded.
func (r *Runner) otherThread(t *task.Task, thread string) (bool, error) {
	if thread == "" || t.CodexThread == thread {
		return false, nil
	}
	if t.CodexThread != "" {
		return true, nil
	}
	_, err := r.Store.Update(t.ID, func(t *task.Task) error {
		if t.CodexThread == "" {
			t.CodexThread = thread
		}
		return nil
	})
	return false, err
}

// screen reads the last lines of t's pane, or fails when t has none.
func (r *Runner) screen(ctx context.Context, t *task.Task) harness.Screen {
	return func(lines int) (string, error) {
		if t.Herdr.PaneID == "" {
			return "", errors.New("no pane")
		}
		return r.Herdr.PaneRead(ctx, t.Herdr.PaneID, lines)
	}
}

func (r *Runner) nudgeText() string {
	return fmt.Sprintf("You ended your turn without reporting to the main session. "+
		"If background shells or agents are still running, end your turn now without reporting: "+
		"you are woken when they finish. Never start sleep or heartbeat commands to stay awake. "+
		"Otherwise report: if the task is finished, run `%[1]s report done --summary \"...\"`; "+
		"if you need a decision, run `%[1]s report needs-human --question \"...\"`; "+
		"if it failed, run `%[1]s report failed --summary \"...\"`.", r.Bin)
}
