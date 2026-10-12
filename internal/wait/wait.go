// Package wait blocks a main session until one of its tasks has news. It
// watches the event log with fsnotify, so a report wakes it at once, and it
// runs the herdr backstop for runners that stop without reporting.
package wait

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// Defaults for Options.
const (
	DefaultTimeout = 30 * time.Minute
	// BurstWindow is how long wait keeps collecting after the first news,
	// so a burst of reports wakes the main session once.
	BurstWindow = 200 * time.Millisecond
	// MaxLine is the longest line wait prints for one event.
	MaxLine = 300
	// scanEvery is how often wait checks launches and new runners.
	scanEvery = 2 * time.Second
	// statusSettle is how long a status change may go without its event.
	// Writers save the task first and append the event right after, so a
	// task that just went idle may still have news on the way.
	statusSettle = time.Second
)

// newsTypes are the events that wake the main session. Everything else is
// progress and only moves the cursor.
var newsTypes = []string{"done", "needs-human", "failed", "held", "stalled", "stopped", "turn-ended-unreported"}

// IsNews reports whether an event type wakes the main session.
func IsNews(typ string) bool { return slices.Contains(newsTypes, typ) }

// Options select what a wait watches.
type Options struct {
	Owner   string
	Tasks   []string // empty means every task of Owner
	Cursor  string   // cursor name; default derived from Owner
	Timeout time.Duration
	Window  time.Duration
}

// Result is what one wait returns.
type Result struct {
	Lines []string // one line per news event
	// Idle is set when nothing is running and no news is pending.
	Idle string
	// TimedOut is set when the safety timeout passed with no news.
	TimedOut bool
}

// Run blocks until news arrives for the owner's tasks, nothing is left
// running, the timeout passes or ctx ends.
func Run(ctx context.Context, r *runner.Runner, o Options) (Result, error) {
	if o.Cursor == "" {
		o.Cursor = state.CursorName(o.Owner)
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.Window <= 0 {
		o.Window = BurstWindow
	}
	unlock, err := r.Store.LockCursor(o.Cursor)
	if err != nil {
		return Result{}, err
	}
	defer unlock()

	// Watch before reading, so an event written in between is never missed.
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return Result{}, fmt.Errorf("watch events: %w", err)
	}
	defer func() { _ = watcher.Close() }()
	if err := watcher.Add(r.Store.Dir()); err != nil {
		return Result{}, fmt.Errorf("watch events: %w", err)
	}
	if err := r.Store.InitCursor(o.Cursor); err != nil {
		return Result{}, err
	}
	cursor, _, err := r.Store.Cursor(o.Cursor)
	if err != nil {
		return Result{}, err
	}
	saved := cursor

	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	b := backstop{r: r, watched: map[string]bool{}}
	timeout := time.NewTimer(o.Timeout)
	defer timeout.Stop()
	scan := time.NewTicker(scanEvery)
	defer scan.Stop()
	eventsPath := r.Store.EventsPath()

	for {
		var news []state.Event
		news, cursor, err = readNews(r.Store, cursor, o)
		if err != nil {
			return Result{}, err
		}
		if len(news) > 0 {
			if !sleep(ctx, o.Window) {
				return Result{}, ctx.Err()
			}
			var more []state.Event
			if more, cursor, err = readNews(r.Store, cursor, o); err != nil {
				return Result{}, err
			}
			news = append(news, more...)
			if err := r.Store.SaveCursor(o.Cursor, cursor); err != nil {
				return Result{}, err
			}
			return Result{Lines: wake(r, o.Owner, news)}, nil
		}
		if cursor != saved {
			if err := r.Store.SaveCursor(o.Cursor, cursor); err != nil {
				return Result{}, err
			}
			saved = cursor
		}
		tasks, err := ownTasks(r.Store, o)
		if err != nil {
			return Result{}, err
		}
		if idle := idleLine(tasks); idle != "" {
			if d := statusSettle - sinceLastChange(tasks); d > 0 {
				if !sleep(ctx, d) {
					return Result{}, ctx.Err()
				}
				continue
			}
			return Result{Idle: idle}, nil
		}
		b.scan(ctx, tasks)
		if res, done, err := next(ctx, watcher, scan.C, timeout.C, eventsPath); done || err != nil {
			return res, err
		}
	}
}

// next blocks until the event log changes or the periodic scan is due. Other
// files in the state directory, such as the cursor itself, are ignored.
func next(ctx context.Context, w *fsnotify.Watcher, scan, timeout <-chan time.Time, eventsPath string) (Result, bool, error) {
	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				return Result{}, true, errors.New("event watch closed")
			}
			if filepath.Clean(ev.Name) == eventsPath {
				return Result{}, false, nil
			}
		case err := <-w.Errors:
			return Result{}, true, fmt.Errorf("watch events: %w", err)
		case <-scan:
			return Result{}, false, nil
		case <-timeout:
			return Result{TimedOut: true}, true, nil
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return Result{TimedOut: true}, true, nil
			}
			return Result{}, true, ctx.Err()
		}
	}
}

func readNews(s *state.Store, cursor int64, o Options) ([]state.Event, int64, error) {
	events, next, err := s.ReadEventsSince(cursor)
	if err != nil {
		return nil, cursor, err
	}
	var news []state.Event
	for _, ev := range events {
		if IsNews(ev.Type) && mine(ev.Owner, ev.Task, o) {
			news = append(news, ev)
		}
	}
	return news, next, nil
}

func mine(owner, id string, o Options) bool {
	if o.Owner != "" && owner != o.Owner {
		return false
	}
	return len(o.Tasks) == 0 || slices.Contains(o.Tasks, id)
}

func ownTasks(s *state.Store, o Options) ([]*task.Task, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	var out []*task.Task
	for _, t := range all {
		if mine(t.Owner, t.ID, o) {
			out = append(out, t)
		}
	}
	return out, nil
}

// idleLine returns a message when none of the tasks can produce news on its
// own: nothing is queued, starting or running.
func idleLine(tasks []*task.Task) string {
	var waiting []string
	for _, t := range tasks {
		switch t.Status {
		case task.Queued, task.Starting, task.Running:
			return ""
		case task.NeedsHuman, task.Held:
			waiting = append(waiting, t.ID+" "+string(t.Status))
		}
	}
	if len(waiting) > 0 {
		return "idle: no running tasks; waiting on you: " + strings.Join(waiting, ", ")
	}
	return "idle: no active tasks"
}

func sinceLastChange(tasks []*task.Task) time.Duration {
	var last time.Time
	for _, t := range tasks {
		if t.UpdatedAt.After(last) {
			last = t.UpdatedAt
		}
	}
	return time.Since(last)
}

// wake formats the news and records the wake-up, the measurement point for
// report-to-main latency.
func wake(r *runner.Runner, owner string, news []state.Event) []string {
	lines := make([]string, 0, len(news))
	ids := make([]string, 0, len(news))
	for _, ev := range news {
		title := ""
		if t, err := r.Store.Get(ev.Task); err == nil {
			title = t.Title
		}
		lines = append(lines, Line(ev, title))
		if !slices.Contains(ids, ev.Task) {
			ids = append(ids, ev.Task)
		}
	}
	_ = r.Store.Append(state.Event{Owner: owner, Type: "woke", Summary: strings.Join(ids, ",")})
	return lines
}

// Line formats one news event for the main session, at most MaxLine
// characters.
func Line(ev state.Event, title string) string {
	parts := []string{ev.Task + " " + ev.Type}
	if title != "" {
		parts = append(parts, title)
	}
	if ev.Question != "" {
		parts = append(parts, "Q: "+ev.Question)
	} else if ev.Summary != "" {
		parts = append(parts, ev.Summary)
	}
	if len(ev.Outputs) > 0 {
		parts = append(parts, "out: "+strings.Join(ev.Outputs, ", "))
	}
	return truncate(strings.Join(strings.Fields(strings.Join(parts, " · ")), " "), MaxLine)
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}

// backstop watches each live runner's agent once per wait, and fails
// launches that never finished.
type backstop struct {
	r       *runner.Runner
	watched map[string]bool
}

func (b *backstop) scan(ctx context.Context, tasks []*task.Task) {
	for _, t := range tasks {
		switch t.Status {
		case task.Starting:
			b.r.CheckLaunch(t)
		case task.Running, task.NeedsHuman:
			if !b.watched[t.ID] && t.Herdr.PaneID != "" {
				b.watched[t.ID] = true
				go b.r.WatchAgent(ctx, t.ID)
			}
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
