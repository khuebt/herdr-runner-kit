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
var newsTypes = []string{"done", "needs-human", "failed", "held", "stalled", "stopped", "turn-ended-unreported", "usage-limit", "cleanable"}

// IsNews reports whether an event type wakes the main session.
func IsNews(typ string) bool { return slices.Contains(newsTypes, typ) }

// Options select what a wait watches.
type Options struct {
	Owner string
	// Projects also watches the tasks of every project, whose runners
	// report to the main session from any folder. It goes by the owner of
	// each task, not by the registry, so a project registered after the
	// wait started is watched too. It is ignored when Owner is empty.
	Projects bool
	Tasks    []string // empty means every task of the owners
	Cursor   string   // cursor name; default derived from Owner
	Timeout  time.Duration
	Window   time.Duration
}

// Result is what one wait returns.
type Result struct {
	Lines []string // one entry per news event; a question round spans several lines
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
	var gcAfter time.Time
	tasks, err := ownTasks(r.Store, o)
	if err != nil {
		return Result{}, err
	}
	// Look for merged worktrees to offer in the background, since it may
	// fetch. An idle wait lets it finish first, so its news is not lost.
	checking := make(chan struct{})
	go func(owners []string) {
		defer close(checking)
		for _, owner := range owners {
			r.NoticeCleanable(ctx, owner)
		}
	}(o.owners(tasks))

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
		if tasks, err = ownTasks(r.Store, o); err != nil {
			return Result{}, err
		}
		if idle := idleLine(tasks); idle != "" {
			if checking != nil {
				select {
				case <-checking:
				case <-ctx.Done():
					return Result{}, ctx.Err()
				}
				checking = nil
				continue
			}
			// min guards against a clock that stepped back past UpdatedAt.
			if d := min(statusSettle-sinceLastChange(tasks), statusSettle); d > 0 {
				if !sleep(ctx, d) {
					return Result{}, ctx.Err()
				}
				continue
			}
			return Result{Idle: idle}, nil
		}
		b.scan(ctx, tasks)
		// tasks holds only the watched owners' tasks.
		if due, ok := runner.NextGC(tasks, ""); ok && time.Now().After(due) && time.Now().After(gcAfter) {
			// In the background, so a slow agent exit never delays news; at
			// most once a minute, so a runner that is in use or will not close is not
			// sent Ctrl+C every scan.
			gcAfter = time.Now().Add(runner.GCRetry)
			go func(owners []string) {
				for _, owner := range owners {
					_, _ = r.GC(ctx, owner, runner.GCAge, false)
				}
			}(o.owners(tasks))
		}
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
	if o.Owner != "" && owner != o.Owner && (!o.Projects || !task.IsProjectOwner(owner)) {
		return false
	}
	return len(o.Tasks) == 0 || slices.Contains(o.Tasks, id)
}

// owners lists Owner and the owners of the watched tasks, each once. An
// empty Owner means every owner.
func (o Options) owners(tasks []*task.Task) []string {
	if o.Owner == "" {
		return []string{""}
	}
	out := []string{o.Owner}
	for _, t := range tasks {
		if !slices.Contains(out, t.Owner) {
			out = append(out, t.Owner)
		}
	}
	return out
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

// idleLine returns a message when none of the tasks can produce news: all
// are finished. A task waiting on a human still can, because the answer or
// release may come from another main session or from its herdr tab.
func idleLine(tasks []*task.Task) string {
	for _, t := range tasks {
		if !t.Status.Terminal() {
			return ""
		}
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
		t, _ := r.Store.Get(ev.Task) // nil when unreadable: the line goes without task details
		lines = append(lines, Line(ev, t))
		if !slices.Contains(ids, ev.Task) {
			ids = append(ids, ev.Task)
		}
	}
	_ = r.Store.Append(state.Event{Owner: owner, Type: "woke", Summary: strings.Join(ids, ",")})
	return lines
}

// Line formats one news event of task t for the main session, at most
// MaxLine characters. A done line lists the decisions the runner took
// itself right after the title; a done or failed line then carries the git
// snapshot of that report, and a done line says when the runner's tab
// closes, all ahead of the summary so the cut drops the summary first. A
// round of questions is a header line followed by one indented line per
// question, each at most MaxLine characters. t may be nil.
func Line(ev state.Event, t *task.Task) string {
	parts := []string{ev.Task + " " + ev.Type}
	if t != nil && t.Title != "" {
		parts = append(parts, t.Title)
	}
	if round := task.QuestionRound(ev.Question); round != nil {
		lines := []string{oneLine(append(parts, fmt.Sprintf("%d questions", len(round))))}
		for i, q := range round {
			lines = append(lines, "  "+truncate(fmt.Sprintf("%d. %s", i+1, q), MaxLine-2))
		}
		return strings.Join(lines, "\n")
	}
	if ev.Type == "done" && len(ev.Decisions) > 0 {
		parts = append(parts, "decided: "+strings.Join(ev.Decisions, "; "))
	}
	if (ev.Type == "done" || ev.Type == "failed") && ev.Git != "" {
		parts = append(parts, "git: "+ev.Git)
	}
	if ev.Type == "done" && t != nil {
		if tab := tabNote(t); tab != "" {
			parts = append(parts, tab)
		}
	}
	if ev.Question != "" {
		parts = append(parts, "Q: "+ev.Question)
	} else if ev.Summary != "" {
		parts = append(parts, ev.Summary)
	}
	if len(ev.Outputs) > 0 {
		parts = append(parts, "out: "+strings.Join(ev.Outputs, ", "))
	}
	return oneLine(parts)
}

// oneLine joins parts with " · " on a single line of at most MaxLine
// characters.
func oneLine(parts []string) string {
	return truncate(strings.Join(strings.Fields(strings.Join(parts, " · ")), " "), MaxLine)
}

// tabNote says what happens to a finished runner's open tab: kept, or
// closed by gc at a given time.
func tabNote(t *task.Task) string {
	if t.Herdr.PaneID == "" || t.Herdr.Closed {
		return ""
	}
	if t.Keep {
		return "tab kept"
	}
	if at, ok := runner.CloseAt(t); ok {
		return "tab closes " + at.Local().Format("15:04")
	}
	return ""
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
