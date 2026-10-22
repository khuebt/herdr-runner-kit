package runner

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/herdr"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// Randomized event sequences drive one store through steps drawn from an
// alphabet of the events the kit sees, and check every rule of
// docs/state-model.md after each step. The count follows the standard
// -quickchecks flag; -seqseed replays a run.
//
// Nothing runs on its own: the hooks that start a detached launcher or
// follow-up push onto queues the scenario owns, and steps decide when a
// queued item runs, or whether it is lost, as a crash or reboot loses it.

var seqSeed = flag.Int64("seqseed", 0, "seed of the randomized sequence tests; 0 picks one")

// seqStep is one step of a scenario: an operation and the random numbers it
// picks with, each read modulo the number of things it picks from, so every
// step is valid against any store.
type seqStep struct {
	Op string
	R  [4]int
}

// seqOp is an operation of an alphabet and its weight.
type seqOp struct {
	name   string
	weight int
}

// genSteps draws between 5 and min(size, 30) steps from ops.
func genSteps(rand *rand.Rand, size int, ops []seqOp) []seqStep {
	total := 0
	for _, o := range ops {
		total += o.weight
	}
	steps := make([]seqStep, 5+rand.Intn(max(1, min(size, 30)-4)))
	for i := range steps {
		n := rand.Intn(total)
		for _, o := range ops {
			if n -= o.weight; n < 0 {
				steps[i].Op = o.name
				break
			}
		}
		for j := range steps[i].R {
			steps[i].R[j] = rand.Intn(1 << 16)
		}
	}
	return steps
}

// minimise drops one step at a time while fails still holds, until no drop
// helps. testing/quick does not shrink.
func minimise(steps []seqStep, fails func([]seqStep) bool) []seqStep {
	for changed := true; changed; {
		changed = false
		for i := 0; i < len(steps); i++ {
			shorter := slices.Delete(slices.Clone(steps), i, i+1)
			if fails(shorter) {
				steps, changed = shorter, true
				i--
			}
		}
	}
	return steps
}

// checkSequences runs prop on quick's scenarios, and on a failure minimises
// the steps and fails with the seed, the minimal steps and what broke.
// prop returns the resolved steps it ran and the first failure, or "".
func checkSequences[S ~[]seqStep](t *testing.T, prop func([]seqStep) (trace []string, failure string)) {
	t.Helper()
	seed := *seqSeed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	err := quick.Check(func(s S) bool {
		_, failure := prop(s)
		return failure == ""
	}, &quick.Config{Rand: rand.New(rand.NewSource(seed))})
	if err == nil {
		return
	}
	ce, ok := err.(*quick.CheckError)
	if !ok {
		t.Fatal(err)
	}
	steps := []seqStep(ce.In[0].(S))
	steps = minimise(steps, func(s []seqStep) bool {
		_, failure := prop(s)
		return failure != ""
	})
	trace, failure := prop(steps)
	t.Fatalf("seed %d (rerun with -seqseed %d), minimal steps:\n  %s\n%s", seed, seed, strings.Join(trace, "\n  "), failure)
}

// taskScenario is a sequence of task lifecycle steps.
type taskScenario []seqStep

var taskOps = []seqOp{
	{"spawn", 4},
	{"run-launcher", 4},
	{"fail-launcher", 1},
	{"report", 4},
	{"agent-gone", 1},
	{"launch-deadline", 1},
	{"run-follow-up", 4},
	{"lose-follow-up", 1},
	{"stop", 1},
	{"stop-during-launch", 1},
	{"release", 1},
	{"answer", 1},
	{"send", 1},
	{"settle-sweep", 1},
}

func (taskScenario) Generate(rand *rand.Rand, size int) reflect.Value {
	return reflect.ValueOf(taskScenario(genSteps(rand, size, taskOps)))
}

// seqWorld is one store under test, with the queues the hooks push onto.
type seqWorld struct {
	t    *testing.T
	ctx  context.Context
	r    *Runner
	fake *fakeHerdr
	// repo is the shared repository worktree tasks spawn in, dir a plain
	// folder, plan the plan folder plan tasks run.
	repo, dir, plan string

	mu        sync.Mutex
	launchers []string        // tasks whose detached launcher has not run
	afters    []string        // tasks whose detached follow-up has not run
	lost      map[string]bool // tasks whose follow-up was lost, until a sweep
	gone      map[string]bool // panes herdr no longer has
	panes     int
	failOpen  bool // the next tab or worktree create fails
	// stopAt, when set, stops stopID from inside that herdr call of the
	// next launch, counted from 1.
	stopAt, calls int
	stopID        string
	// prev is the snapshot the last check saw, so the rules that compare
	// two snapshots see every step.
	prev *Snapshot
}

// takenBranch exists in the shared repository, so a spawn that names it
// fails.
const takenBranch = "taken"

func newSeqWorld(t *testing.T, repo string) *seqWorld {
	r, fake, _ := newTestRunner(t)
	store, err := state.Open(filepath.Join(memDir(t), "state"))
	if err != nil {
		t.Fatal(err)
	}
	r.Store = store
	w := &seqWorld{t: t, ctx: context.Background(), r: r, fake: fake, repo: repo, dir: t.TempDir(), lost: map[string]bool{}, gone: map[string]bool{}}
	w.plan = filepath.Join(t.TempDir(), "plan")
	writeFiles(t, w.plan, map[string]string{
		"plan.md":       "---\ntitle: P\nstatus: in-progress\n---\n",
		"phase-01-a.md": "---\nphase: 1\nstatus: pending\n---\n# Phase 1\n",
	})
	r.StartLauncher = func(id string) (int, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.launchers = append(w.launchers, id)
		return os.Getpid(), nil // alive, as a launcher that has not run yet
	}
	r.StartAfter = func(id string) error {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.afters = append(w.afters, id)
		return nil
	}
	r.Herdr = &herdr.Client{Bin: "herdr", Exec: w.exec}
	open := func(ws string) func([]string) (string, error) {
		return func([]string) (string, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			if w.failOpen {
				w.failOpen = false
				return "", herdrError("workspace_not_found")
			}
			w.panes++
			n := w.panes
			if ws == "w1" {
				return fmt.Sprintf(`{"result":{"root_pane":{"pane_id":"w1:p%d","tab_id":"w1:t%d","workspace_id":"w1"}}}`, n, n), nil
			}
			return fmt.Sprintf(`{"result":{"root_pane":{"pane_id":"w%d:p1","tab_id":"w%d:t1","workspace_id":"w%d"},"worktree":{"path":"/wt/%d"}}}`, n+1, n+1, n+1, n), nil
		}
	}
	fake.on("tab create", open("w1"))
	fake.on("worktree create", open(""))
	for _, cmd := range []string{"agent get", "agent wait", "agent prompt", "agent send-keys", "pane read", "pane run"} {
		h := fake.handlers[cmd]
		if h == nil {
			h = ok("{}")
		}
		fake.on(cmd, func(args []string) (string, error) {
			if len(args) > 2 && w.paneGone(args[2]) {
				if cmd == "pane read" || cmd == "pane run" {
					return "", herdrError("pane_not_found")
				}
				return "", herdrError("agent_not_found")
			}
			return h(args)
		})
	}
	return w
}

// memDir returns a new folder in memory where the system has one: the
// store syncs every write, which on disk makes a scenario take seconds.
func memDir(t *testing.T) string {
	if info, err := os.Stat("/dev/shm"); err != nil || !info.IsDir() {
		return t.TempDir()
	}
	dir, err := os.MkdirTemp("/dev/shm", "hrk-seq-")
	if err != nil {
		return t.TempDir()
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func (w *seqWorld) paneGone(pane string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.gone[pane]
}

// exec counts herdr calls while a stop is armed, and stops the task from
// inside the chosen one.
func (w *seqWorld) exec(ctx context.Context, bin string, args ...string) ([]byte, []byte, error) {
	w.mu.Lock()
	stop := ""
	if w.stopAt > 0 {
		if w.calls++; w.calls == w.stopAt {
			stop, w.stopAt = w.stopID, 0
		}
	}
	w.mu.Unlock()
	if stop != "" {
		_, _ = w.r.Stop(ctx, stop, false)
	}
	return w.fake.exec(ctx, bin, args...)
}

func (w *seqWorld) tasks() []*task.Task {
	all, err := w.r.Store.List()
	if err != nil {
		w.t.Fatal(err)
	}
	return all
}

// pick returns the n-th task, modulo, of those keep accepts.
func (w *seqWorld) pick(n int, keep func(*task.Task) bool) *task.Task {
	var c []*task.Task
	for _, t := range w.tasks() {
		if keep(t) {
			c = append(c, t)
		}
	}
	if len(c) == 0 {
		return nil
	}
	return c[n%len(c)]
}

// pop takes the n-th id, modulo, off q.
func (w *seqWorld) pop(q *[]string, n int) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(*q) == 0 {
		return ""
	}
	i := n % len(*q)
	id := (*q)[i]
	*q = slices.Delete(*q, i, i+1)
	return id
}

func live(t *task.Task) bool { return !t.Status.Terminal() }

// step runs one step and describes what it did.
func (w *seqWorld) step(s seqStep) string {
	ctx, r := w.ctx, w.r
	switch s.Op {
	case "spawn":
		return w.spawn(s)
	case "run-launcher", "fail-launcher":
		id := w.pop(&w.launchers, s.R[0])
		if id == "" {
			return s.Op + ": none queued"
		}
		if s.Op == "fail-launcher" {
			w.mu.Lock()
			w.failOpen = true
			w.mu.Unlock()
		}
		err := r.Launch(ctx, id)
		w.mu.Lock()
		w.failOpen = false
		w.mu.Unlock()
		return fmt.Sprintf("%s %s: %v", s.Op, id, err)
	case "stop-during-launch":
		id := w.pop(&w.launchers, s.R[0])
		if id == "" {
			return s.Op + ": none queued"
		}
		k := 1 + s.R[1]%8
		w.mu.Lock()
		w.stopAt, w.calls, w.stopID = k, 0, id
		w.mu.Unlock()
		err := r.Launch(ctx, id)
		w.mu.Lock()
		w.stopAt = 0
		w.mu.Unlock()
		return fmt.Sprintf("launch %s, stopped at herdr call %d: %v", id, k, err)
	case "report":
		t := w.pick(s.R[0], func(t *task.Task) bool {
			return t.Status == task.Starting || t.Status == task.Running || t.Status == task.NeedsHuman
		})
		if t == nil {
			return "report: no live task"
		}
		o := ReportOptions{Status: []task.Status{task.Done, task.Failed, task.NeedsHuman}[s.R[1]%3], Summary: "did it"}
		if o.Status == task.NeedsHuman {
			o.Summary, o.Questions = "", []string{"Which one?"}
		}
		if t.Review != nil && t.Review.File != "" {
			verdict := []string{task.ReviewPass, "fix"}[s.R[2]%2]
			if err := os.WriteFile(t.Review.File, []byte("Verdict: "+verdict+"\n"), 0o644); err != nil {
				w.t.Fatal(err)
			}
		}
		_, err := r.Report(t.ID, o)
		return fmt.Sprintf("report %s %s: %v", t.ID, o.Status, err)
	case "agent-gone":
		t := w.pick(s.R[0], func(t *task.Task) bool {
			return (t.Status == task.Running || t.Status == task.NeedsHuman) && t.Herdr.PaneID != "" && !t.Herdr.Closed
		})
		if t == nil {
			return "agent-gone: no running agent"
		}
		w.mu.Lock()
		w.gone[t.Herdr.PaneID] = true
		w.mu.Unlock()
		if s.R[1]%2 == 0 {
			r.WatchAgent(ctx, t.ID)
			return "agent of " + t.ID + " gone, seen by the watch"
		}
		r.CheckLost(ctx, t)
		return "agent of " + t.ID + " gone, seen by the lost check"
	case "launch-deadline":
		t := w.pick(s.R[0], func(t *task.Task) bool { return t.Status == task.Starting })
		if t == nil {
			return "launch-deadline: nothing starting"
		}
		t, err := r.Store.Update(t.ID, func(t *task.Task) error {
			t.LaunchDeadline = time.Now().Add(-time.Second)
			return nil
		})
		if err != nil {
			w.t.Fatal(err)
		}
		r.CheckLaunch(t)
		return "launch deadline of " + t.ID + " passed"
	case "run-follow-up":
		id := w.pop(&w.afters, s.R[0])
		if id == "" {
			return "run-follow-up: none queued"
		}
		return fmt.Sprintf("follow-up of %s: %v", id, r.After(ctx, id))
	case "lose-follow-up":
		id := w.pop(&w.afters, s.R[0])
		if id == "" {
			return "lose-follow-up: none queued"
		}
		w.mu.Lock()
		w.lost[id] = true
		w.mu.Unlock()
		return "follow-up of " + id + " lost"
	case "stop":
		t := w.pick(s.R[0], func(*task.Task) bool { return true })
		if t == nil {
			return "stop: no task"
		}
		_, err := r.Stop(ctx, t.ID, false)
		return fmt.Sprintf("stop %s (%s): %v", t.ID, t.Status, err)
	case "release":
		t := w.pick(s.R[0], func(t *task.Task) bool { return t.Status == task.Held })
		if t == nil {
			return "release: nothing held"
		}
		_, err := r.Release(t.ID, "")
		return fmt.Sprintf("release %s: %v", t.ID, err)
	case "answer":
		t := w.pick(s.R[0], func(t *task.Task) bool { return t.Status == task.NeedsHuman })
		if t == nil {
			return "answer: nobody asks"
		}
		_, err := r.Answer(ctx, t.ID, "the first")
		return fmt.Sprintf("answer %s: %v", t.ID, err)
	case "send":
		// Reopening an old plan task beside a newer one of its phase is
		// the user's call; the harness leaves plan tasks alone.
		t := w.pick(s.R[0], func(t *task.Task) bool {
			return (t.Status == task.Done || t.Status == task.Failed) && t.Plan == ""
		})
		if t == nil {
			return "send: nothing finished"
		}
		_, err := r.Send(ctx, t.ID, "one more thing")
		return fmt.Sprintf("send %s (%s): %v", t.ID, t.Status, err)
	case "settle-sweep":
		return w.sweep()
	}
	w.t.Fatalf("unknown step %q", s.Op)
	return ""
}

// sweep runs the wait backstop's settle sweep; it recovers every lost
// follow-up.
func (w *seqWorld) sweep() string {
	err := w.r.SettleWaiting(w.ctx, 0)
	w.mu.Lock()
	clear(w.lost)
	w.mu.Unlock()
	return fmt.Sprintf("settle sweep: %v", err)
}

// spawn spawns a task with up to two random parents, a random gate, and
// with or without a worktree; some are plan phases, final reviews or
// fixups of the scenario's plan.
func (w *seqWorld) spawn(s seqStep) string {
	all := w.tasks()
	o := SpawnOptions{Prompt: "work", Cwd: w.dir, Worktree: new(false)}
	if n := s.R[0] % 3; len(all) > 0 {
		for i := range n {
			id := all[(s.R[1]>>(4*i))%len(all)].ID
			if !slices.Contains(o.After, id) {
				o.After = append(o.After, id)
			}
		}
	}
	if s.R[2]%3 == 0 {
		o.Gate = task.GateHuman
	}
	if s.R[2]%5 < 2 {
		o.Cwd, o.Worktree = w.repo, new(true)
		if s.R[3]%4 == 0 {
			o.Branch = takenBranch
		}
	}
	var latestReview *task.Task
	liveStep := map[string]bool{}
	for _, t := range all {
		if t.Plan == "" {
			continue
		}
		if t.Review != nil {
			latestReview = t
		}
		if live(t) {
			switch {
			case t.Review != nil && t.Review.Final:
				liveStep["review"] = true
			case !planHelper(t):
				liveStep["phase"] = true
			}
		}
	}
	latestPhase := LatestPhaseTasks(PlanTasks(all, w.plan))[1]
	switch s.R[3] % 7 {
	case 1, 2: // a phase of the plan
		if liveStep["phase"] {
			break
		}
		o.Plan, o.Phase = w.plan, 1
	case 3: // the final review, after the newest phase task
		if liveStep["review"] || latestPhase == nil {
			break
		}
		o = SpawnOptions{
			Prompt: "review", Cwd: w.dir, Worktree: new(false), Plan: w.plan, After: []string{latestPhase.ID},
			Review: &task.Review{Final: true, File: filepath.Join(w.plan, "reports", fmt.Sprintf("review-%d.md", len(all)))},
		}
		if err := os.MkdirAll(filepath.Dir(o.Review.File), 0o755); err != nil {
			w.t.Fatal(err)
		}
	case 4: // the fixup after the newest review
		if latestReview == nil {
			break
		}
		o = SpawnOptions{Prompt: "fix", Cwd: w.dir, Worktree: new(false), Plan: w.plan, After: []string{latestReview.ID}, FixupOf: latestReview.ID}
	}
	t, err := w.r.Spawn(w.ctx, o)
	desc := fmt.Sprintf("spawn after=%v gate=%s worktree=%v branch=%q plan=%v review=%v fixup=%q", o.After, o.Gate, *o.Worktree, o.Branch, o.Plan != "", o.Review != nil, o.FixupOf)
	if err != nil {
		return desc + ": " + err.Error()
	}
	return desc + ": " + t.ID + " " + string(t.Status)
}

// check returns what the store breaks, with the queued and lost follow-ups
// in flight, against the snapshot the last check saw.
func (w *seqWorld) check() []Violation {
	s, err := w.r.Snapshot()
	if err != nil {
		w.t.Fatal(err)
	}
	cur := s
	s.Prev, w.prev = w.prev, &cur
	w.mu.Lock()
	s.InFlight = map[string]bool{}
	for _, id := range w.afters {
		s.InFlight[id] = true
	}
	for id := range w.lost {
		s.InFlight[id] = true
	}
	w.mu.Unlock()
	return CheckInvariants(s)
}

// drain runs every queued launcher and follow-up, and the sweep that
// recovers the lost ones, until nothing is left, and checks the rules after
// each. It returns the trace and the first failure, or "".
func (w *seqWorld) drain(trace []string) ([]string, string) {
	for range 100 {
		var op string
		switch {
		case len(w.afters) > 0:
			op = "run-follow-up"
		case len(w.launchers) > 0:
			op = "run-launcher"
		case len(w.lost) > 0:
			op = "settle-sweep"
		default:
			return trace, ""
		}
		trace = append(trace, w.step(seqStep{Op: op}))
		if vs := w.check(); len(vs) > 0 {
			return trace, "while draining the queues:\n" + formatViolations(vs)
		}
	}
	w.t.Fatal("the queues never drained")
	return nil, ""
}

func formatViolations(vs []Violation) string {
	lines := make([]string, len(vs))
	for i, v := range vs {
		lines[i] = "  " + v.String()
	}
	return strings.Join(lines, "\n")
}

func TestTaskLifecycleSequences(t *testing.T) {
	repo := gitRepo(t)
	if out, err := exec.Command("git", "-C", repo, "branch", takenBranch).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	checkSequences[taskScenario](t, func(steps []seqStep) ([]string, string) {
		w := newSeqWorld(t, repo)
		var trace []string
		for i, s := range steps {
			trace = append(trace, w.step(s))
			if vs := w.check(); len(vs) > 0 {
				return trace, fmt.Sprintf("after step %d:\n%s", i+1, formatViolations(vs))
			}
		}
		return w.drain(trace)
	})
}

// The harness compares each snapshot with the one before it, so a writer
// that takes a stopped task back to running is caught.
func TestSequenceCheckSeesAStoppedTaskRunAgain(t *testing.T) {
	w := newSeqWorld(t, gitRepo(t))
	w.step(seqStep{Op: "spawn"})
	w.step(seqStep{Op: "stop"})
	if vs := w.check(); len(vs) > 0 {
		t.Fatalf("a stopped task breaks:\n%s", formatViolations(vs))
	}
	if _, err := w.r.Store.Update("t1", func(t *task.Task) error {
		t.Status, t.FinishedAt = task.Running, time.Time{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := rulesOf(w.check()); !slices.Contains(got, "terminal-stays-terminal") {
		t.Errorf("rules = %q, want terminal-stays-terminal", got)
	}
}
