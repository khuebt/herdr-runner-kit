package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/herdr"
	"github.com/khuebt/herdr-runner-kit/internal/redact"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// GCAge is how long a finished runner stays open before gc closes it.
const GCAge = 30 * time.Minute

// Send types text into a live runner as a new prompt, and reports whether
// herdr saw the agent start a turn within a few seconds. A message to a
// done or failed runner is new work, so the task is reopened, but only after
// the prompt went in: a failed prompt leaves the task finished, and the
// backstop never mistakes its idle agent for a failure. A stopped task is
// never reopened: its runner is closing.
func (r *Runner) Send(ctx context.Context, id, text string) (bool, error) {
	t, err := r.liveTask(id)
	if err != nil {
		return false, err
	}
	if t.Status == task.Stopped {
		return false, fmt.Errorf("%s is stopped; spawn a new runner", id)
	}
	if err := r.errIfLost(ctx, t); err != nil {
		return false, err
	}
	head := roundHead(t) // a new round begins with this prompt
	if err := r.Herdr.AgentPrompt(ctx, t.Herdr.PaneID, text); err != nil {
		return false, err
	}
	errClosing, errStopped := errors.New("closing"), errors.New("stopped")
	if t, err = r.Store.Update(id, func(t *task.Task) error {
		switch {
		case t.Herdr.Closed || claimLive(t, time.Now()):
			return errClosing
		case t.Status == task.Stopped:
			return errStopped
		}
		t.Unreported = 0
		if head != "" {
			t.RoundHead = head
		}
		// Reopen the task, so its next report counts, unreported turns are
		// nudged, and wait watches it.
		if t.Status.Terminal() {
			t.Status, t.FinishedAt, t.Error = task.Running, time.Time{}, ""
		}
		return nil
	}); errors.Is(err, errClosing) {
		return false, fmt.Errorf("%s was closing while the prompt was sent; it may not run", id)
	} else if errors.Is(err, errStopped) {
		return false, fmt.Errorf("%s was stopped while the prompt was sent; it may not run", id)
	} else if err != nil {
		return false, err
	}
	// The message may name where a secret lives, never the secret itself;
	// hide anything that looks like one before it reaches the log.
	r.emit(t, "sent", firstLine(redact.Secrets(text), 120), "")
	_, err = r.Herdr.AgentWait(ctx, t.Herdr.PaneID, 3*time.Second, "working")
	return err == nil, nil
}

// Answer sends the user's answer to a runner that reported needs-human and
// sets it running again. It reports whether herdr saw the turn start.
func (r *Runner) Answer(ctx context.Context, id, text string) (bool, error) {
	t, err := r.liveTask(id)
	if err != nil {
		return false, err
	}
	if t.Status != task.NeedsHuman {
		return false, fmt.Errorf("%s is %s, not waiting for an answer; use send", id, t.Status)
	}
	if t.Question == "" {
		return false, fmt.Errorf("%s waits on the %s dialog in its tab; answer it there (hrk focus %s)", id, t.Error, id)
	}
	if err := r.errIfLost(ctx, t); err != nil {
		return false, err
	}
	head := roundHead(t)
	if err := r.Herdr.AgentPrompt(ctx, t.Herdr.PaneID, text); err != nil {
		return false, err
	}
	nt, changed, err := r.Store.Transition(id, task.Running, []task.Status{task.NeedsHuman}, func(t *task.Task) {
		t.Question, t.Unreported = "", 0
		if head != "" {
			t.RoundHead = head
		}
	})
	if err != nil {
		return false, err
	}
	if changed {
		// The answer may name where a secret lives, never the secret itself;
		// hide anything that looks like one before it reaches the log.
		r.emit(nt, "answered", firstLine(redact.Secrets(text), 120), "")
	}
	_, err = r.Herdr.AgentWait(ctx, t.Herdr.PaneID, 3*time.Second, "working")
	return err == nil, nil
}

// Release opens a held task's human gate and launches it. A note is added
// to its prompt.
func (r *Runner) Release(id, note string) (*task.Task, error) {
	t, err := r.Store.Get(id)
	if err != nil {
		return nil, err
	}
	if t.Status != task.Held {
		return nil, fmt.Errorf("%s is %s, not held", id, t.Status)
	}
	if note = strings.TrimSpace(note); note != "" {
		if _, err := r.Store.Update(id, func(t *task.Task) error {
			t.Prompt = strings.TrimSpace(t.Prompt + "\n\nNote from the user: " + note)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return r.StartLaunch(id)
}

// Focus switches the herdr UI to the runner.
func (r *Runner) Focus(ctx context.Context, id string) error {
	t, err := r.liveTask(id)
	if err != nil {
		return err
	}
	return r.Herdr.AgentFocus(ctx, t.Herdr.PaneID)
}

// Peek returns the last lines of a runner's visible pane, with anything that
// looks like a secret hidden. It only reads.
func (r *Runner) Peek(ctx context.Context, id string, lines int) (string, error) {
	t, err := r.liveTask(id)
	if err != nil {
		return "", err
	}
	screen, err := r.Herdr.PaneRead(ctx, t.Herdr.PaneID, lines)
	if err != nil {
		return "", err
	}
	return redact.Secrets(screen), nil
}

func (r *Runner) liveTask(id string) (*task.Task, error) {
	t, err := r.Store.Get(id)
	if err != nil {
		return nil, err
	}
	if t.Herdr.PaneID == "" || t.Herdr.Closed {
		return nil, fmt.Errorf("%s has no open runner (status %s)", id, t.Status)
	}
	if claimLive(t, time.Now()) {
		return nil, fmt.Errorf("%s's tab is closing; spawn a new runner", id)
	}
	return t, nil
}

// Stop ends a task's agent and marks the task stopped. A worktree task keeps
// its workspace and branch unless removeWorktree is set; removal refuses a
// dirty worktree, and the branch is never deleted.
func (r *Runner) Stop(ctx context.Context, id string, removeWorktree bool) (*task.Task, error) {
	// Mark it stopped before the agent quits, or a running wait's backstop
	// sees the agent go first and reports a failure.
	t, changed, err := r.Store.SetStatus(id, task.Stopped)
	if err != nil {
		return nil, err
	}
	if changed {
		r.emit(t, "stopped", "stopped by the user", "")
		_ = r.failDependents(t)
	}
	if err := r.closeRunner(ctx, t, removeWorktree, closeCheck{}); err != nil {
		return nil, err
	}
	return r.Store.Get(id)
}

// LeftFiles names the untracked files in the checkout of a task that worked
// without a worktree of its own, other than plan scratch files: a stopped
// runner may have left partial work there. It returns the checkout and up
// to a few paths with the rest counted, or "" when there is nothing to say.
// It never deletes anything.
func LeftFiles(ctx context.Context, t *task.Task) (dir string, n int, list string) {
	if t.Worktree != nil || t.RepoRoot == "" {
		return "", 0, ""
	}
	ctx, cancel := context.WithTimeout(ctx, gitStateTimeout)
	defer cancel()
	s, err := readStatus(ctx, t.Cwd)
	if err != nil {
		return "", 0, ""
	}
	_, other := splitUntracked(s.Untracked)
	if len(other) == 0 {
		return "", 0, ""
	}
	return t.Cwd, len(other), listPaths(other)
}

// Keep holds a runner's tab open after its task finishes, so gc leaves it
// for keepFor, until keep is turned off or the task is stopped. Turning keep
// off on a runner already past its close time closes it at once, unless its
// agent is in use; gc closes it later then.
func (r *Runner) Keep(ctx context.Context, id string, on bool) (*task.Task, error) {
	t, err := r.Store.Update(id, func(t *task.Task) error {
		if on && t.Herdr.Closed {
			return fmt.Errorf("%s has no open runner (status %s)", id, t.Status)
		}
		if on && claimLive(t, time.Now()) {
			return fmt.Errorf("%s's tab is closing; spawn a new runner", id)
		}
		t.Keep = on
		return nil
	})
	if err != nil {
		return nil, err
	}
	at, ok := CloseAt(t, r.Config.KeepFor)
	if on || !ok || time.Now().Before(at) {
		return t, nil
	}
	err = r.closeRunner(ctx, t, false, closeCheck{idle: true, cutoff: time.Now().Add(-GCAge), keep: true, released: true})
	if err != nil && !errors.Is(err, errReopened) {
		return nil, fmt.Errorf("close %s: %w", id, err)
	}
	return r.Store.Get(id)
}

// GC closes runners whose tasks finished more than olderThan ago, so their
// agents stop using memory. A kept runner closes once it finished keepFor
// ago, or with kept or removeWorktree like any other. An empty owner means
// every owner's tasks. It returns the ids it closed.
func (r *Runner) GC(ctx context.Context, owner string, olderThan time.Duration, removeWorktree, kept bool) ([]string, error) {
	tasks, err := r.Store.List()
	if err != nil {
		return nil, err
	}
	var closed []string
	var errs []error
	now := time.Now()
	c := closeCheck{idle: true, cutoff: now.Add(-olderThan), keep: !removeWorktree}
	if kept {
		c.keepCutoff = c.cutoff
	}
	if r.Config.KeepFor > 0 {
		if kc := now.Add(-r.Config.KeepFor); kc.After(c.keepCutoff) {
			c.keepCutoff = kc
		}
	}
	for _, t := range tasks {
		if (owner != "" && t.Owner != owner) || !collectable(t, removeWorktree) || finishedAt(t).After(c.cutoffFor(t)) {
			continue
		}
		err := r.closeRunner(ctx, t, removeWorktree, c)
		if errors.Is(err, errReopened) {
			continue // reopened, kept or busy since the list; try later
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.ID, err))
			continue
		}
		closed = append(closed, t.ID)
	}
	return closed, errors.Join(errs...)
}

// GCRetry is how long callers leave a runner that GC skipped or could not
// close before they try again.
const GCRetry = time.Minute

// inUse reports whether the agent in pane is busy: working, or blocked on a
// prompt someone may be answering.
func (r *Runner) inUse(ctx context.Context, pane string) bool {
	a, err := r.Herdr.AgentGet(ctx, pane)
	return err == nil && (a.Status == "working" || a.Status == "blocked")
}

// NextGC returns when GC can first close one of these tasks' finished
// runners, and false when none is open. A non-empty owner limits it to that
// owner's tasks. Kept runners close keepFor after they finish.
func NextGC(tasks []*task.Task, owner string, keepFor time.Duration) (time.Time, bool) {
	var next time.Time
	for _, t := range tasks {
		if owner != "" && t.Owner != owner {
			continue
		}
		if due, ok := CloseAt(t, keepFor); ok && (next.IsZero() || due.Before(next)) {
			next = due
		}
	}
	return next, !next.IsZero()
}

// CloseAt returns when GC closes t's runner, and false when it never will on
// its own: the task is not finished, its tab is closed, or it is kept with
// no keepFor. A kept runner closes keepFor after it finished.
func CloseAt(t *task.Task, keepFor time.Duration) (time.Time, bool) {
	switch {
	case !collectable(t, false):
		return time.Time{}, false
	case t.Keep && keepFor <= 0:
		return time.Time{}, false
	case t.Keep:
		return finishedAt(t).Add(keepFor), true
	}
	return finishedAt(t).Add(GCAge), true
}

// collectable reports whether t finished and still has something for GC
// to close: its runner, or with removeWorktree its runner or its worktree.
func collectable(t *task.Task, removeWorktree bool) bool {
	if !t.Status.Terminal() || t.Herdr.PaneID == "" {
		return false
	}
	if removeWorktree {
		return !t.Herdr.Closed || (t.Worktree != nil && !t.Worktree.Removed)
	}
	return !t.Herdr.Closed
}

func finishedAt(t *task.Task) time.Time {
	if t.FinishedAt.IsZero() {
		return t.UpdatedAt
	}
	return t.FinishedAt
}

// closeClaimTTL is how long a close claim holds. Quitting an agent takes
// seconds and is cut off at quitBudget, so a claim older than this was
// abandoned: its gc was cut off, or its process died.
const closeClaimTTL = time.Minute

// quitBudget bounds quitting an agent and closing its tab. It is shorter
// than closeClaimTTL, so a claim cannot expire while its close is under way.
// A variable so tests can shorten it.
var quitBudget = 45 * time.Second

var (
	// errAlreadyClosed means the runner was closed before; only its
	// worktree may be left to remove.
	errAlreadyClosed = errors.New("the runner is already closed")
	// errReopened means the runner may not be closed now: it is in use
	// again, kept, not due, or another close is under way.
	errReopened = errors.New("the runner is in use")
)

// closeCheck says what a close re-checks under the task lock. Stop uses the
// zero value: any finished runner closes.
// keptClosed is the summary of the closed event of a kept runner's tab,
// which learning reads to measure the time the tab stayed open idle.
const keptClosed = "kept tab closed"

// KeptClosed reports whether a closed event closed a kept runner's tab.
func KeptClosed(ev state.Event) bool { return ev.Type == "closed" && ev.Summary == keptClosed }

type closeCheck struct {
	idle   bool      // also check that the agent is not in use
	cutoff time.Time // close only a task that finished by then
	keep   bool      // leave a kept runner open, unless keepCutoff allows
	// keepCutoff closes a kept runner that finished by then; zero leaves
	// every kept runner open. It applies only with keep.
	keepCutoff time.Time
	// released marks a close right after keep --off, so the closed event
	// still counts the tab as kept.
	released bool
}

// cutoffFor returns when t must have finished by for c to close it, and the
// zero time when c leaves it open because it is kept.
func (c closeCheck) cutoffFor(t *task.Task) time.Time {
	if c.keep && t.Keep {
		return c.keepCutoff
	}
	return c.cutoff
}

// claimLive reports whether a close claim on t is still held. The window
// spans both sides of now, so a clock that steps back cannot stretch it.
func claimLive(t *task.Task, now time.Time) bool {
	if t.Herdr.ClosingSince.IsZero() {
		return false
	}
	d := now.Sub(t.Herdr.ClosingSince)
	return d > -closeClaimTTL && d < closeClaimTTL
}

// claimClose marks id's runner as closing, after checking under the task
// lock that it may still close: the list the caller worked from may be stale.
// It returns the claim it wrote, which identifies it to endClaim.
func (r *Runner) claimClose(id string, c closeCheck) (time.Time, error) {
	var since time.Time
	_, err := r.Store.Update(id, func(t *task.Task) error {
		now := time.Now()
		switch {
		case t.Herdr.Closed:
			return errAlreadyClosed
		case !t.Status.Terminal():
			return fmt.Errorf("%w: it is %s", errReopened, t.Status)
		case c.keep && t.Keep && c.keepCutoff.IsZero():
			return fmt.Errorf("%w: it is kept open", errReopened)
		case c.keep && t.Keep && finishedAt(t).After(c.keepCutoff):
			return fmt.Errorf("%w: it is kept open for now", errReopened)
		case (!c.keep || !t.Keep) && !c.cutoff.IsZero() && finishedAt(t).After(c.cutoff):
			return fmt.Errorf("%w: it finished again recently", errReopened)
		case claimLive(t, now):
			return fmt.Errorf("%w: its tab is already closing", errReopened)
		}
		since = now.UTC()
		t.Herdr.ClosingSince = since
		return nil
	})
	return since, err
}

// endClaim drops the close claim since on id, and with closed marks the
// runner closed in the same write. A claim that expired and was replaced
// belongs to another close and is left alone. Closed is written only while
// the claim is still this one and the task is still finished; otherwise it
// fails with errReopened.
func (r *Runner) endClaim(id string, since time.Time, closed bool) error {
	errNotOurs := errors.New("not our claim")
	var reopened error
	_, err := r.Store.Update(id, func(t *task.Task) error {
		reopened = nil
		if !t.Herdr.ClosingSince.Equal(since) {
			return errNotOurs
		}
		t.Herdr.ClosingSince = time.Time{}
		if closed && !t.Status.Terminal() {
			reopened = fmt.Errorf("%w: it is %s", errReopened, t.Status)
			return nil
		}
		t.Herdr.Closed = t.Herdr.Closed || closed
		return nil
	})
	switch {
	case errors.Is(err, errNotOurs) && closed:
		return fmt.Errorf("%w: its close claim expired", errReopened)
	case errors.Is(err, errNotOurs):
		return nil
	case err != nil:
		return err
	}
	return reopened
}

// closeRunner claims the runner, quits the agent gracefully, then closes its
// tab. A worktree runner keeps its workspace, whose pane drops back to a
// shell, unless removeWorktree is set. A runner closed before still has its
// worktree removed. It fails with errReopened when the runner may not close.
func (r *Runner) closeRunner(ctx context.Context, t *task.Task, removeWorktree bool, c closeCheck) error {
	if t.Herdr.PaneID == "" {
		return nil
	}
	switch since, err := r.claimClose(t.ID, c); {
	case errors.Is(err, errAlreadyClosed):
	case err != nil:
		return err
	default:
		quitCtx, cancel := context.WithTimeout(ctx, quitBudget)
		err := r.endAgent(quitCtx, t, c.idle)
		cancel()
		if err != nil {
			if clearErr := r.endClaim(t.ID, since, false); clearErr != nil {
				return errors.Join(err, clearErr)
			}
			return err
		}
		// Closed is written only once the agent is gone.
		if err := r.endClaim(t.ID, since, true); err != nil {
			return err
		}
		if cur, err := r.Store.Get(t.ID); err == nil {
			summary := "tab closed"
			if cur.Keep || c.released {
				summary = keptClosed
			}
			r.emit(cur, "closed", summary, "")
		}
	}
	if t.Worktree == nil || !removeWorktree {
		return nil
	}
	current, err := r.Store.Get(t.ID)
	if err != nil {
		return err
	}
	if current.Worktree == nil || current.Worktree.Removed {
		return nil
	}
	if err := r.removeWorktree(ctx, current, false); err != nil {
		return fmt.Errorf("remove worktree (the branch is kept): %w", err)
	}
	_, err = r.Store.Update(t.ID, func(t *task.Task) error {
		t.Worktree.Removed = true
		return nil
	})
	return err
}

// endAgent quits the agent and closes its tab. With idle it first leaves an
// agent someone is still using, which keeps working after its report. The
// processes the runner started and left running are reported, never killed.
func (r *Runner) endAgent(ctx context.Context, t *task.Task, idle bool) error {
	if idle && r.inUse(ctx, t.Herdr.PaneID) {
		return fmt.Errorf("%w: its agent is busy", errReopened)
	}
	procs, check := r.snapshotProcs(ctx, t)
	if err := r.quitAgent(ctx, t.Herdr.PaneID); err != nil {
		return err
	}
	if t.Worktree == nil {
		if err := r.Herdr.TabClose(ctx, t.Herdr.TabID); err != nil && !isGone(err) {
			return err
		}
	}
	if check {
		r.reportSurvivors(ctx, t, procs)
	}
	return nil
}

// quitAgent asks the agent to exit with Ctrl+C pairs, the way a user would.
// Killing a Codex process directly can wedge the shared Codex daemon, so
// the agent is never signalled.
func (r *Runner) quitAgent(ctx context.Context, pane string) error {
	for range 3 {
		a, err := r.Herdr.AgentGet(ctx, pane)
		if isGone(err) || (err == nil && a.Kind == "") {
			return nil
		}
		if err != nil {
			return err
		}
		if err := r.Herdr.AgentSendKeys(ctx, pane, "ctrl+c", "ctrl+c"); err != nil && !isGone(err) {
			return err
		}
		_, err = r.Herdr.AgentWait(ctx, pane, 3*time.Second, "unknown")
		if isGone(err) {
			return nil
		}
	}
	return fmt.Errorf("the agent in %s did not exit after Ctrl+C", pane)
}

// isGone reports whether herdr says the agent or pane no longer exists.
func isGone(err error) bool {
	if err == nil {
		return false
	}
	return herdr.HasCode(err, herdr.CodeAgentNotRunning) || herdr.HasCode(err, herdr.CodeAgentNotFound) ||
		herdr.HasCode(err, "pane_not_found") || herdr.HasCode(err, "tab_not_found")
}

func firstLine(s string, n int) string {
	line := s
	for i, c := range s {
		if c == '\n' {
			line = s[:i]
			break
		}
	}
	if len(line) > n {
		return line[:n] + "…"
	}
	return line
}
