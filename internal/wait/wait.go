// Package wait blocks a main session until one of its tasks has news. It
// watches the event log with fsnotify, so a report wakes it at once, and it
// runs the herdr backstop for runners that stop without reporting.
package wait

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/khuebt/herdr-runner-kit/internal/config"
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
	// flushAfter is how long quiet news may wait after the main session's
	// last activity before wait delivers it on its own.
	// ponytail: 10 minutes short of the 1-hour prompt cache the main session
	// was seen to keep; a config key if a shorter cache shows up.
	flushAfter = 50 * time.Minute
)

// newsTypes are the events that wake the main session, or come with its next
// wake when marked quiet. Everything else is progress and only moves the
// cursor.
var newsTypes = []string{"done", "needs-human", "failed", "held", "stalled", "stopped", "turn-ended-unreported", "usage-limit", "api-error", "cleanable", "processes-left", "plan-done"}

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
	Cursor   string   // cursor name; default derived from Tasks, else from Owner
	// Holder names this wait in the cursor lock, so a refused wait can say
	// who holds it; default "wait".
	Holder  string
	Timeout time.Duration
	Window  time.Duration
	// AllNews wakes for every news line. Otherwise quiet news waits for the
	// next wake, the flush deadline or idle, unless Tasks names tasks or the
	// config's wakeOn is all.
	AllNews bool
	// FlushOnTimeout delivers waiting quiet news when the timeout passes,
	// instead of leaving it for the next wait.
	FlushOnTimeout bool
}

func (o Options) cursor() string {
	switch {
	case o.Cursor != "":
		return o.Cursor
	case len(o.Tasks) > 0:
		return TaskCursor(o.Tasks)
	}
	return state.CursorName(o.Owner)
}

// TaskCursor names the cursor of a wait for the tasks ids: the same ids in
// any order share it, and it is never the main session's cursor.
func TaskCursor(ids []string) string {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	sum := sha256.Sum256([]byte(strings.Join(slices.Compact(ids), ",")))
	return "tasks-" + hex.EncodeToString(sum[:6])
}

// activePath is the marker whose modification time is the last activity of
// the main session that waits on cursor.
func activePath(s *state.Store, cursor string) string {
	return filepath.Join(s.Dir(), "main-active-"+cursor)
}

// MarkActive records that the main session is active now: its turn just ended
// or it started a wait. It marks the cursor of o.Cursor, else the owner's
// cursor, also for a wait on tasks, which keeps a cursor of its own. Quiet
// news waits at most flushAfter past the latest mark.
func MarkActive(s *state.Store, o Options) error {
	name := o.Cursor
	if name == "" {
		name = state.CursorName(o.Owner)
	}
	p, now := activePath(s, name), time.Now()
	if err := os.Chtimes(p, now, now); !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.WriteFile(p, nil, 0o600)
}

// lastActive returns the latest MarkActive of cursor, or since when there
// was none.
func lastActive(s *state.Store, cursor string, since time.Time) time.Time {
	if info, err := os.Stat(activePath(s, cursor)); err == nil {
		return info.ModTime()
	}
	return since
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
// running, the timeout passes or ctx ends. Quiet news is held and comes
// first with the next news that wakes, at the flush deadline or instead of
// the idle line. The saved cursor never passes held news, so a later Run,
// in this process or another, finds it again.
func Run(ctx context.Context, r *runner.Runner, o Options) (Result, error) {
	derived := o.Cursor == "" && len(o.Tasks) > 0
	o.Cursor = o.cursor()
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.Window <= 0 {
		o.Window = BurstWindow
	}
	if o.Holder == "" {
		o.Holder = "wait"
	}
	unlock, err := r.Store.LockCursor(o.Cursor, o.Holder)
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
	if derived {
		// The first wait on these tasks sees what the main session has not
		// been woken for.
		if _, ok, err := r.Store.Cursor(o.Cursor); err != nil {
			return Result{}, err
		} else if !ok {
			if offset, ok, err := r.Store.Cursor(state.CursorName(o.Owner)); err != nil {
				return Result{}, err
			} else if ok {
				if err := r.Store.SaveCursor(o.Cursor, offset); err != nil {
					return Result{}, err
				}
			}
		}
	}
	if err := r.Store.InitCursor(o.Cursor); err != nil {
		return Result{}, err
	}
	cursor, _, err := r.Store.Cursor(o.Cursor)
	if err != nil {
		return Result{}, err
	}
	saved := cursor
	hold := !o.AllNews && len(o.Tasks) == 0 && r.Config.WakeOn != config.WakeAll
	var held []state.Event // quiet news not delivered yet, in log order
	var heldFrom int64     // where the read that found the first held news started
	began := time.Now()
	deliver := func(news []state.Event) (Result, error) {
		if err := r.Store.SaveCursor(o.Cursor, cursor); err != nil {
			return Result{}, err
		}
		return Result{Lines: wake(r, o.Owner, append(held, news...))}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	b := backstop{r: r, watched: map[string]bool{}, sampled: map[string]bool{}, held: map[string]bool{}}
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
		from := cursor
		var news []state.Event
		news, cursor, err = readNews(r.Store, cursor, o)
		if err != nil {
			return Result{}, err
		}
		if hold && len(news) > 0 && !slices.ContainsFunc(news, func(ev state.Event) bool { return !ev.Quiet }) {
			if len(held) == 0 {
				heldFrom = from
			}
			held, news = append(held, news...), nil
		}
		if len(news) > 0 {
			if !sleep(ctx, o.Window) {
				return Result{}, ctx.Err()
			}
			var more []state.Event
			if more, cursor, err = readNews(r.Store, cursor, o); err != nil {
				return Result{}, err
			}
			return deliver(append(news, more...))
		}
		if len(held) > 0 && time.Since(lastActive(r.Store, o.Cursor, began)) >= flushAfter {
			return deliver(nil)
		}
		save := cursor
		if len(held) > 0 {
			save = heldFrom
		}
		if save != saved {
			if err := r.Store.SaveCursor(o.Cursor, save); err != nil {
				return Result{}, err
			}
			saved = save
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
			if len(held) > 0 {
				return deliver(nil)
			}
			return Result{Idle: idle}, nil
		}
		b.scan(ctx, tasks)
		// tasks holds only the watched owners' tasks.
		if due, ok := runner.NextGC(tasks, "", r.Config.KeepFor); ok && time.Now().After(due) && time.Now().After(gcAfter) {
			// In the background, so a slow agent exit never delays news; at
			// most once a minute, so a runner that is in use or will not close is not
			// sent Ctrl+C every scan.
			gcAfter = time.Now().Add(runner.GCRetry)
			go func(owners []string) {
				for _, owner := range owners {
					_, _ = r.GC(ctx, runner.Owners{Owner: owner}, runner.GCAge, false, false)
				}
			}(o.owners(tasks))
		}
		res, done, err := next(ctx, watcher, scan.C, timeout.C, eventsPath)
		if res.TimedOut && o.FlushOnTimeout && len(held) > 0 {
			return deliver(nil)
		}
		if done || err != nil {
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
		lines = append(lines, Line(ev, t, r.Config.KeepFor))
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
// question, each at most MaxLine characters. t may be nil. keepFor is how
// long a kept tab stays open.
func Line(ev state.Event, t *task.Task, keepFor time.Duration) string {
	parts := []string{ev.Task + " " + ev.Type}
	if ev.Type == "cleanable" && ev.Status != "" && ev.Status != task.Done {
		parts = append(parts, string(ev.Status)) // a cleanable task that failed never reads as done
	}
	if t != nil && t.Title != "" {
		parts = append(parts, t.Title)
	}
	if ev.Type == "needs-human" && ev.Attempt > 1 {
		parts = append(parts, fmt.Sprintf("attempt %d", ev.Attempt))
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
		if tab := tabNote(t, keepFor); tab != "" {
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

// tabNote says what happens to a finished runner's open tab: kept, kept
// until a given time, or closed by gc at a given time.
func tabNote(t *task.Task, keepFor time.Duration) string {
	if t.Herdr.PaneID == "" || t.Herdr.Closed {
		return ""
	}
	at, ok := runner.CloseAt(t, keepFor)
	switch {
	case t.Keep && ok:
		return "tab kept until " + at.Local().Format("15:04")
	case t.Keep:
		return "tab kept"
	case ok:
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

// lostEvery is how often wait checks that herdr still has each live
// runner's pane and agent.
const lostEvery = time.Minute

// backstop watches each live runner's agent and its progress once per
// wait, fails launches that never finished, and fails runners herdr lost.
type backstop struct {
	r       *runner.Runner
	watched map[string]bool
	sampled map[string]bool
	held    map[string]bool // tasks whose held done is watched
	lostAt  time.Time       // last check for lost runners
	// checking is set while a check for lost runners runs.
	checking atomic.Bool
}

func (b *backstop) scan(ctx context.Context, tasks []*task.Task) {
	if time.Since(b.lostAt) >= lostEvery && b.checking.CompareAndSwap(false, true) {
		// In the background, so a slow herdr never delays news; one check
		// at a time.
		b.lostAt = time.Now()
		go func() {
			defer b.checking.Store(false)
			for _, t := range tasks {
				b.r.CheckLost(ctx, t)
			}
		}()
	}
	for _, t := range tasks {
		switch t.Status {
		case task.Starting:
			b.r.CheckLaunch(t)
		case task.Running, task.NeedsHuman:
			if !b.watched[t.ID] && t.Herdr.PaneID != "" {
				b.watched[t.ID] = true
				go b.r.WatchAgent(ctx, t.ID)
			}
			if !b.held[t.ID] && t.Herdr.PaneID != "" {
				b.held[t.ID] = true
				go b.r.WatchPendingDone(ctx, t.ID)
			}
			if !b.sampled[t.ID] && t.Herdr.PaneID != "" && b.r.Config.StallAfter > 0 {
				b.sampled[t.ID] = true
				go b.r.WatchProgress(ctx, t.ID)
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
