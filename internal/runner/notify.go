package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
	"github.com/khuebt/herdr-runner-kit/internal/telegram"
)

// record appends an event and, for event types the config selects, hands it
// to the detached notifier. A task that ends failed or stopped goes to the
// notifier too when done is selected, so the dones held back for its chain
// are sent. Sending never runs in the caller's process, so report, wait and
// the turn-end hook stay fast with the network down.
func (r *Runner) record(ev state.Event) error {
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	if err := r.Store.Append(ev); err != nil {
		return err
	}
	if r.Notify != nil && (slices.Contains(r.Config.Telegram.Notify, ev.Type) || r.endsChain(ev)) {
		r.Notify(ev)
	}
	return nil
}

// endsChain reports whether ev ends a task without the chain report its
// parents' held-back dones wait for, and those dones are wanted.
func (r *Runner) endsChain(ev state.Event) bool {
	return (ev.Type == "failed" || ev.Type == "stopped") && slices.Contains(r.Config.Telegram.Notify, "done")
}

// startDetachedNotify runs `<bin> __notify <event-json>` in its own session.
func (r *Runner) startDetachedNotify(ev state.Event) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	cmd := exec.Command(r.Bin, "__notify", string(data))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "herdr-runner-kit: %s: start notify: %v\n", ev.Task, err)
		return
	}
	_ = cmd.Process.Release()
}

// SendNotification sends one news event to Telegram and records the
// outcome as a notified or notify-failed event. A first-round done with no
// decisions, whose dependent has not started yet, is held back as
// notify-deferred: the chain's last task reports instead. When a task ends
// failed or stopped, the held-back dones of its chain go out in one summary.
// Missing credentials mean Telegram is off, which is not an error.
func (r *Runner) SendNotification(ctx context.Context, ev state.Event) error {
	t, err := r.Store.Get(ev.Task)
	if err != nil {
		return err
	}
	selected := slices.Contains(r.Config.Telegram.Notify, ev.Type)
	if ev.Type == "done" && selected {
		deferred, err := r.deferDone(ev, t)
		if err != nil || deferred {
			return err
		}
	}
	creds, err := telegram.Resolve(r.Config.Telegram, r.Getenv)
	if errors.Is(err, telegram.ErrNoCredentials) {
		return nil
	}
	var errs []error
	if selected {
		errs = append(errs, r.send(ctx, creds, err, t, ev.Type, telegram.Message(ev, t)))
	}
	if r.endsChain(ev) {
		errs = append(errs, r.flushDeferred(ctx, creds, err, t, ev.Type))
	}
	return errors.Join(errs...)
}

// send delivers text unless resolving the credentials failed with credErr,
// and records the outcome on t as notified or notify-failed, labelled what.
func (r *Runner) send(ctx context.Context, creds telegram.Creds, credErr error, t *task.Task, what, text string) error {
	err := credErr
	result := state.Event{Task: t.ID, Owner: t.Owner, Type: "notified", Summary: what}
	if err == nil {
		ctx, cancel := context.WithTimeout(ctx, telegram.SendTimeout)
		defer cancel()
		err = r.Telegram.Send(ctx, creds, text)
	}
	if err != nil {
		result.Type, result.Summary = "notify-failed", what+": "+err.Error()
	}
	if appendErr := r.Store.Append(result); appendErr != nil {
		return appendErr
	}
	return err
}

// deferDone holds back ev, a done of t, when the chain's last task will
// report instead: it is t's first round, it settled no decisions, and a
// task waiting for t has not started yet. A held-back done is recorded as
// notify-deferred.
func (r *Runner) deferDone(ev state.Event, t *task.Task) (bool, error) {
	if len(ev.Decisions) > 0 {
		return false, nil
	}
	tasks, err := r.Store.List()
	if err != nil {
		return false, err
	}
	waiting := slices.ContainsFunc(tasks, func(q *task.Task) bool {
		return slices.Contains(q.After, t.ID) && (q.Status == task.Queued || q.Status == task.Held)
	})
	if !waiting {
		return false, nil
	}
	all, _, err := r.Store.ReadEventsSince(0)
	if err != nil {
		return false, err
	}
	for _, e := range all {
		if e.Task == t.ID && e.TS.Before(ev.TS) && slices.Contains(laterRoundEvents, e.Type) {
			return false, nil
		}
	}
	return true, r.Store.Append(state.Event{Task: t.ID, Owner: t.Owner, Type: "notify-deferred", Summary: "done"})
}

// laterRoundEvents mark that a done is not the task's first round: an
// earlier done, or new work typed into the runner.
var laterRoundEvents = []string{"done", "sent", "answered"}

// flushDeferred sends the held-back dones of end's chain, the tasks it
// waited for directly or through others, in one message, because end
// finished as how and will never report the chain. Each held-back done is
// sent at most once.
func (r *Runner) flushDeferred(ctx context.Context, creds telegram.Creds, credErr error, end *task.Task, how string) error {
	unlock, err := r.Store.Lock("telegram-chain")
	if err != nil {
		return err
	}
	defer unlock()
	all, _, err := r.Store.ReadEventsSince(0)
	if err != nil {
		return err
	}
	pending := map[string]bool{}
	for _, e := range all {
		switch {
		case e.Type == "notify-deferred":
			pending[e.Task] = true
		case e.Type == "notified" && strings.HasPrefix(e.Summary, "done"):
			delete(pending, e.Task)
		}
	}
	var chain []*task.Task
	seen := map[string]bool{end.ID: true}
	queue := slices.Clone(end.After)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		p, err := r.Store.Get(id)
		if err != nil {
			continue
		}
		queue = append(queue, p.After...)
		if pending[id] {
			chain = append(chain, p)
		}
	}
	if len(chain) == 0 {
		return nil
	}
	slices.SortFunc(chain, func(a, b *task.Task) int { return idNumber(a.ID) - idNumber(b.ID) })
	var errs []error
	if err := r.send(ctx, creds, credErr, end, "chain", telegram.ChainMessage(end, how, chain)); err != nil {
		errs = append(errs, err)
	} else {
		for _, p := range chain {
			errs = append(errs, r.Store.Append(state.Event{Task: p.ID, Owner: p.Owner, Type: "notified", Summary: "done (chain of " + end.ID + ")"}))
		}
	}
	return errors.Join(errs...)
}

// idNumber is the sequence number of a task id, or 0 when it has none.
func idNumber(id string) int {
	_, n, _ := task.ParseID(id)
	return n
}
