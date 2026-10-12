package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"syscall"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/telegram"
)

// record appends an event and, for event types the config selects, hands it
// to the detached notifier. Sending never runs in the caller's process, so
// report, wait and the turn-end hook stay fast with the network down.
func (r *Runner) record(ev state.Event) error {
	if err := r.Store.Append(ev); err != nil {
		return err
	}
	if r.Notify != nil && slices.Contains(r.Config.Telegram.Notify, ev.Type) {
		r.Notify(ev)
	}
	return nil
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
// outcome as a notified or notify-failed event. A done task that other tasks
// wait for is skipped: the chain's last task reports instead. Missing
// credentials mean Telegram is off, which is not an error.
func (r *Runner) SendNotification(ctx context.Context, ev state.Event) error {
	t, err := r.Store.Get(ev.Task)
	if err != nil {
		return err
	}
	if ev.Type == "done" {
		tasks, err := r.Store.List()
		if err != nil {
			return err
		}
		for _, q := range tasks {
			if slices.Contains(q.After, t.ID) {
				return nil
			}
		}
	}
	creds, err := telegram.Resolve(r.Config.Telegram, r.Getenv)
	if errors.Is(err, telegram.ErrNoCredentials) {
		return nil
	}
	result := state.Event{Task: t.ID, Owner: t.Owner, Type: "notified", Summary: ev.Type}
	if err == nil {
		ctx, cancel := context.WithTimeout(ctx, telegram.SendTimeout)
		defer cancel()
		err = r.Telegram.Send(ctx, creds, telegram.Message(ev, t))
	}
	if err != nil {
		result.Type, result.Summary = "notify-failed", ev.Type+": "+err.Error()
	}
	if appendErr := r.Store.Append(result); appendErr != nil {
		return appendErr
	}
	return err
}
