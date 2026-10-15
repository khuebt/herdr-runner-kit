package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/redact"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// MaxHandoffs is how many times one task may continue in a fresh session
// after its agent neared its context limit.
const MaxHandoffs = 1

// AttemptFromEnv reads HRK_ATTEMPT, which the launcher sets from the second
// session on. Empty, or a value that is not a positive number, means the
// first session.
func AttemptFromEnv(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// callerAttempt is the agent session the calling hook or command runs in.
func (r *Runner) callerAttempt() int { return AttemptFromEnv(r.Getenv("HRK_ATTEMPT")) }

// eventAttempt is the attempt an event of t carries: none for the first
// session, so events of tasks that never handed off stay as they were.
func eventAttempt(t *task.Task) int {
	if n := task.AttemptOf(t); n > 1 {
		return n
	}
	return 0
}

// staleAttempt refuses a report from another agent session than t's
// current one. attempt 0 skips the check.
func staleAttempt(t *task.Task, attempt int) error {
	if attempt == 0 || attempt == task.AttemptOf(t) {
		return nil
	}
	return fmt.Errorf("%s is on attempt %d; a report from attempt %d is ignored", t.ID, task.AttemptOf(t), attempt)
}

// CheckAttempt refuses an answer addressed to an attempt of task id other
// than its current one, such as "t3a1" once attempt 2 runs. attempt 0, a
// bare id, always passes.
func (r *Runner) CheckAttempt(id string, attempt int) error {
	if attempt == 0 {
		return nil
	}
	t, err := r.Store.Get(id)
	if err != nil {
		return err
	}
	switch cur := task.AttemptOf(t); {
	case attempt < cur:
		return fmt.Errorf("that question was for attempt %d of %s, which ended", attempt, id)
	case attempt > cur:
		return fmt.Errorf("%s is on attempt %d; it has no attempt %d", id, cur, attempt)
	}
	return nil
}

// Handoff ends the runner's current agent session near its context limit
// and continues the task in a fresh session of the same agent, in the same
// pane and checkout. The runner wrote o.Outputs[0], a handoff note the next
// session reads first. A task hands off at most MaxHandoffs times. Like a
// quota switch, it never quits the agent itself, because it runs inside
// that agent's tool: a detached launcher does.
func (r *Runner) Handoff(id string, o ReportOptions) (*task.Task, error) {
	o.Summary = redact.Secrets(strings.TrimSpace(o.Summary))
	if o.Summary == "" {
		return nil, fmt.Errorf("report handoff needs --summary")
	}
	if len(o.Questions) > 0 || len(o.Decisions) > 0 {
		return nil, fmt.Errorf("--question and --decision are not for handoff")
	}
	if len(o.Outputs) != 1 {
		return nil, fmt.Errorf("report handoff needs exactly one --output: the handoff file")
	}
	file, err := filepath.Abs(o.Outputs[0])
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(file); err != nil {
		return nil, fmt.Errorf("--output %s: %w", o.Outputs[0], err)
	} else if info.IsDir() {
		return nil, fmt.Errorf("--output %s is a folder; pass the handoff file", o.Outputs[0])
	}
	nt, err := r.Store.Update(id, func(t *task.Task) error {
		if err := staleAttempt(t, o.Attempt); err != nil {
			return err
		}
		switch {
		case len(t.Handoffs) >= MaxHandoffs:
			return fmt.Errorf("%s already used its one context replacement; keep working in this session (it compacts on its own), or report failed or needs-human", t.ID)
		case t.Status != task.Running:
			return fmt.Errorf("%s is %s; report handoff only works while it runs", t.ID, t.Status)
		case t.Herdr.PaneID == "":
			return fmt.Errorf("%s has no runner pane to continue in", t.ID)
		}
		t.Attempt = task.AttemptOf(t) + 1
		t.Handoffs = append(t.Handoffs, file)
		t.Status, t.LauncherPID, t.Relaunched = task.Starting, 0, task.RelaunchHandoff
		// A fresh session starts a new Codex thread and a fresh turn count.
		t.CodexThread, t.Unreported, t.Question, t.Stalled = "", 0, "", false
		t.LaunchDeadline = time.Now().UTC().Add(LaunchBudget)
		return nil
	})
	if err != nil {
		return nil, err
	}
	n := task.AttemptOf(nt)
	r.emit(nt, "handoff", fmt.Sprintf("attempt %d ended near its context limit; attempt %d continues from %s. %s", n-1, n, file, o.Summary), "")
	return nt, r.relaunch(nt)
}

// relaunchNotes follow the first prompt of a session the launcher started
// in the pane of an earlier one: the reason for the restart, and from the
// second attempt on, where the last handoff is.
func relaunchNotes(t *task.Task) []string {
	reason := t.Relaunched
	if reason == "" && len(t.FallbackFrom) > 0 {
		reason = task.RelaunchUsageLimit
	}
	switch {
	case reason == task.RelaunchHandoff:
		return []string{handoffNote(t)}
	case reason == task.RelaunchUsageLimit && task.AttemptOf(t) > 1:
		return []string{resumeNote, fmt.Sprintf("This is attempt %d of task %s; the handoff of the attempt before it is %s: read it too.", task.AttemptOf(t), t.ID, lastHandoff(t))}
	case reason == task.RelaunchUsageLimit:
		return []string{resumeNote}
	}
	return nil
}

// handoffNote opens the first prompt of an attempt the last handoff started.
func handoffNote(t *task.Task) string {
	return fmt.Sprintf("This is attempt %d of task %s. The previous session neared its context limit and wrote a handoff: %s. "+
		"Read it first, check `git status` and `git log`, continue only the unfinished work, rerun only the checks it affects, then report.",
		task.AttemptOf(t), t.ID, lastHandoff(t))
}

func lastHandoff(t *task.Task) string {
	if len(t.Handoffs) == 0 {
		return ""
	}
	return t.Handoffs[len(t.Handoffs)-1]
}
