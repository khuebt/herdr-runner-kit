package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/config"
	"github.com/khuebt/herdr-runner-kit/internal/herdr"
	"github.com/khuebt/herdr-runner-kit/internal/project"
	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// Randomized chain sequences drive one project's milestone chain through
// chain runs, node reports, stops, lost agents, follow-ups and plan
// amendments committed at HEAD, and check every rule of
// docs/state-model.md after each step. The count follows the standard
// -quickchecks flag; -seqseed replays a run. The engine is the one of the
// task lifecycle sequences in internal/runner.

var seqSeed = flag.Int64("seqseed", 0, "seed of the randomized sequence tests; 0 picks one")

// seqStep is one step of a scenario: an operation and the random numbers it
// picks with, each read modulo the number of things it picks from.
type seqStep struct {
	Op string
	R  [4]int
}

type seqOp struct {
	name   string
	weight int
}

// genSteps draws between 5 and min(size, maxSteps) steps from ops.
func genSteps(rand *rand.Rand, size, maxSteps int, ops []seqOp) []seqStep {
	total := 0
	for _, o := range ops {
		total += o.weight
	}
	steps := make([]seqStep, 5+rand.Intn(max(1, min(size, maxSteps)-4)))
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

// minimise drops one step at a time while fails still holds.
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
func checkSequences[S ~[]seqStep](t *testing.T, maxCount int, prop func([]seqStep) (trace []string, failure string)) {
	t.Helper()
	seed := *seqSeed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	cfg := &quick.Config{Rand: rand.New(rand.NewSource(seed))}
	if f := flag.Lookup("quickchecks"); f == nil || f.Value.String() == "100" {
		cfg.MaxCount = maxCount // the flag's default: this test's own budget
	}
	err := quick.Check(func(s S) bool {
		_, failure := prop(s)
		return failure == ""
	}, cfg)
	if err == nil {
		return
	}
	ce, ok := err.(*quick.CheckError)
	if !ok {
		t.Fatal(err)
	}
	steps := minimise([]seqStep(ce.In[0].(S)), func(s []seqStep) bool {
		_, failure := prop(s)
		return failure != ""
	})
	trace, failure := prop(steps)
	t.Fatalf("seed %d (rerun with -seqseed %d), minimal steps:\n  %s\n%s", seed, seed, strings.Join(trace, "\n  "), failure)
}

func formatViolations(vs []runner.Violation) string {
	lines := make([]string, len(vs))
	for i, v := range vs {
		lines[i] = "  " + v.String()
	}
	return strings.Join(lines, "\n")
}

// chainScenario is a sequence of chain steps.
type chainScenario []seqStep

var chainOps = []seqOp{
	{"chain", 4},
	{"report", 4},
	{"agent-gone", 1},
	{"stop", 1},
	{"run-launcher", 4},
	{"run-follow-up", 4},
	{"lose-follow-up", 1},
	{"settle-sweep", 1},
	{"amend-plan", 2},
}

func (chainScenario) Generate(rand *rand.Rand, size int) reflect.Value {
	return reflect.ValueOf(chainScenario(genSteps(rand, size, 12, chainOps)))
}

// planNode is a node of the plan the scenario commits.
type planNode struct {
	ID, Type, Title string
	DependsOn       []string
}

// chainWorld is one project whose milestone M1 is chained, with the
// queues the runner's hooks push onto and the plan at HEAD.
type chainWorld struct {
	t   *testing.T
	ctx context.Context
	r   *runner.Runner
	p   state.Project
	out bytes.Buffer

	mu        sync.Mutex
	launchers []string
	afters    []string
	lost      map[string]bool
	gone      map[string]bool
	panes     int

	plan    []planNode
	added   int // leaf nodes added so far
	renamed int // titles renamed so far
	prev    *runner.Snapshot
}

// chainBase is the world newChainWorld builds once per test, kept as
// copies of its state folder and project that every later world of the
// test starts from instead of building it again.
type chainBase struct {
	state, project, stateCopy, projectCopy string
	p                                      state.Project
}

var chainBases sync.Map // *testing.T → *chainBase

// newChainWorld returns a project with M0 done and released, and M1 in
// progress with its plan runner done and the first plan committed.
func newChainWorld(t *testing.T) *chainWorld {
	if b, ok := chainBases.Load(t); ok {
		return restoreChainWorld(t, b.(*chainBase))
	}
	r, sp := approvedRunner(t)
	w := &chainWorld{t: t, ctx: t.Context(), r: r, p: sp, lost: map[string]bool{}, gone: map[string]bool{}}
	w.wire()
	// M0 done and released, as done leaves it.
	if err := startMilestone(w.ctx, &w.out, r, sp, "M0", milestoneRun{Warn: func(string) {}}); err != nil {
		t.Fatal(err)
	}
	for _, tk := range projectTasks(t, r, "M0/") {
		setTask(t, r, tk.ID, task.Done, false)
	}
	if _, err := project.Update(r.Store, "demo-app", func(rec *project.Record) error {
		m := &rec.Milestones[0]
		m.Status, m.Tag, m.Closed, m.Released, m.Branch = project.MilestoneDone, "v0.0.0", time.Now().UTC(), []string{"notes"}, "release/M0"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := startMilestone(w.ctx, &w.out, r, sp, "M1", milestoneRun{Warn: func(string) {}}); err != nil {
		t.Fatal(err)
	}
	setTask(t, r, projectTasks(t, r, "M1/plan")[0].ID, task.Done, false)
	w.launchers = nil
	w.plan = firstPlan()
	w.commitPlan()
	b := &chainBase{state: r.Store.Dir(), project: sp.Path, stateCopy: t.TempDir(), projectCopy: t.TempDir(), p: sp}
	for _, d := range [][2]string{{b.stateCopy, b.state}, {b.projectCopy, b.project}} {
		if err := os.CopyFS(d[0], os.DirFS(d[1])); err != nil {
			t.Fatal(err)
		}
	}
	chainBases.Store(t, b)
	t.Cleanup(func() { chainBases.Delete(t) })
	return w
}

// restoreChainWorld puts b's state folder and project back as they were
// built and returns a world on them with a new runner.
func restoreChainWorld(t *testing.T, b *chainBase) *chainWorld {
	for _, d := range [][2]string{{b.state, b.stateCopy}, {b.project, b.projectCopy}} {
		if err := os.RemoveAll(d[0]); err != nil {
			t.Fatal(err)
		}
		if err := os.CopyFS(d[0], os.DirFS(d[1])); err != nil {
			t.Fatal(err)
		}
	}
	store, err := state.Open(b.state)
	if err != nil {
		t.Fatal(err)
	}
	r := runner.New(store, config.Default(), "/usr/local/bin/hrk")
	r.Notify = nil
	w := &chainWorld{t: t, ctx: t.Context(), r: r, p: b.p, lost: map[string]bool{}, gone: map[string]bool{}, plan: firstPlan()}
	w.wire()
	return w
}

// firstPlan is the plan newChainWorld commits.
func firstPlan() []planNode {
	return []planNode{
		{ID: "F1", Type: "feature", Title: "Contract"},
		{ID: "F2", Type: "feature", Title: "List", DependsOn: []string{"F1"}},
		{ID: "F3", Type: "feature", Title: "Settings"},
	}
}

// wire points w.r's herdr, launcher and follow-up hooks at w.
func (w *chainWorld) wire() {
	t, r := w.t, w.r
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	env := map[string]string{"HERDR_ENV": "1", "HERDR_WORKSPACE_ID": "w1", "CLAUDE_CONFIG_DIR": t.TempDir()}
	r.Getenv = func(k string) string { return env[k] }
	r.Herdr = &herdr.Client{Bin: "herdr", Exec: w.exec}
	r.Sleep = func(time.Duration) {}
	r.LookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	r.EnterScope, r.ProcTable, r.ProcEnv = nil, nil, nil
	r.StartUsage = func(string) error { return nil }
	r.StartAPIRetry = func(string) error { return nil }
	r.StartLauncher = func(id string) (int, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.launchers = append(w.launchers, id)
		return 4242, nil
	}
	r.StartAfter = func(id string) error {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.afters = append(w.afters, id)
		return nil
	}
}

// exec stands in for herdr. A worktree create makes its branch in the
// project, as herdr does, so branches exist exactly when a runner made
// them; a pane marked gone has no agent.
func (w *chainWorld) exec(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
	fail := func(code string) ([]byte, []byte, error) {
		return nil, fmt.Appendf(nil, `{"error":{"code":%q,"message":"fake"}}`, code), errors.New("exit status 1")
	}
	cmd := strings.Join(args[:min(2, len(args))], " ")
	pane := ""
	if len(args) > 2 {
		pane = args[2]
	}
	w.mu.Lock()
	gone := w.gone[pane]
	w.mu.Unlock()
	switch cmd {
	case "worktree create", "tab create":
		w.mu.Lock()
		w.panes++
		n := w.panes
		w.mu.Unlock()
		if cmd == "tab create" {
			return fmt.Appendf(nil, `{"result":{"root_pane":{"pane_id":"w1:p%d","tab_id":"w1:t%d","workspace_id":"w1"}}}`, n, n), nil, nil
		}
		branch, base := flagValue(args, "--branch"), flagValue(args, "--base")
		if out, err := exec.Command("git", "-C", w.p.Path, "branch", branch, base).CombinedOutput(); err != nil {
			return nil, fmt.Appendf(nil, `{"error":{"code":"worktree_create_failed","message":%q}}`, string(out)), errors.New("exit status 1")
		}
		return fmt.Appendf(nil, `{"result":{"root_pane":{"pane_id":"w%d:p1","tab_id":"w%d:t1","workspace_id":"w%d"},"worktree":{"path":"/wt/%d","branch":%q}}}`,
			n+1, n+1, n+1, n, branch), nil, nil
	case "workspace list":
		return []byte(`{"result":{"workspaces":[]}}`), nil, nil
	case "agent start":
		return []byte(`{"result":{"agent":{"agent":"claude","agent_status":"idle"}}}`), nil, nil
	case "agent get", "agent wait", "agent prompt", "agent send-keys":
		if gone {
			return fail("agent_not_found")
		}
		if cmd == "agent send-keys" && slices.Contains(args, "ctrl+c") {
			w.mu.Lock()
			w.gone[pane] = true // the agent quits
			w.mu.Unlock()
			return []byte("{}"), nil, nil
		}
		if cmd == "agent get" {
			return []byte(`{"result":{"agent":{"agent":"claude","agent_status":"idle"}}}`), nil, nil
		}
		return []byte(`{"result":{"agent":{"agent_status":"working"}}}`), nil, nil
	case "pane read", "pane run":
		if gone {
			return fail("pane_not_found")
		}
		return []byte("❯ \n"), nil, nil
	}
	return []byte("{}"), nil, nil
}

func flagValue(args []string, name string) string {
	if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// commitPlan commits w.plan as M1's plan at HEAD.
func (w *chainWorld) commitPlan() {
	var b strings.Builder
	b.WriteString("# M1 plan\n\n```yaml\nnodes:\n")
	for _, n := range w.plan {
		fmt.Fprintf(&b, "  - {id: %s, type: %s, title: %s", n.ID, n.Type, n.Title)
		if len(n.DependsOn) > 0 {
			fmt.Fprintf(&b, ", dependsOn: [%s]", strings.Join(n.DependsOn, ", "))
		}
		b.WriteString("}\n")
	}
	b.WriteString("```\n")
	commitFiles(w.t, w.p.Path, map[string]string{project.MilestonePlanPath("M1"): b.String()})
}

// amend changes the plan the way a plan runner amends it between two chain
// runs, keeping it valid: a feature has at most one parent, and a node
// depends only on nodes listed before it.
func (w *chainWorld) amend(s seqStep) string {
	pick := func(n int, keep func(i int) bool) int {
		var c []int
		for i := range w.plan {
			if keep(i) {
				c = append(c, i)
			}
		}
		if len(c) == 0 {
			return -1
		}
		return c[n%len(c)]
	}
	var desc string
	switch s.R[0] % 9 {
	case 0, 1:
		i := pick(s.R[1], func(i int) bool { return len(w.plan[i].DependsOn) > 0 })
		if i < 0 {
			return "amend: no dependsOn to drop"
		}
		n := &w.plan[i]
		d := n.DependsOn[s.R[2]%len(n.DependsOn)]
		n.DependsOn = slices.DeleteFunc(slices.Clone(n.DependsOn), func(x string) bool { return x == d })
		desc = fmt.Sprintf("amend: %s no longer depends on %s", n.ID, d)
	case 2, 3:
		i := pick(s.R[1], func(i int) bool {
			n := w.plan[i]
			return i > 0 && (n.Type == "integrate" || len(n.DependsOn) == 0) && len(n.DependsOn) < i
		})
		if i < 0 {
			return "amend: no dependsOn to add"
		}
		n := &w.plan[i]
		var c []string
		for _, e := range w.plan[:i] {
			if !slices.Contains(n.DependsOn, e.ID) {
				c = append(c, e.ID)
			}
		}
		d := c[s.R[2]%len(c)]
		n.DependsOn = append(slices.Clone(n.DependsOn), d)
		desc = fmt.Sprintf("amend: %s now depends on %s", n.ID, d)
	case 4, 5:
		w.added++
		n := planNode{ID: fmt.Sprintf("N%d", w.added), Type: "feature", Title: fmt.Sprintf("Extra%d", w.added)}
		if s.R[1]%2 == 0 {
			n.DependsOn = []string{w.plan[s.R[2]%len(w.plan)].ID}
		}
		w.plan = append(w.plan, n)
		desc = fmt.Sprintf("amend: added leaf %s after %v", n.ID, n.DependsOn)
	case 6, 7:
		i := s.R[1] % len(w.plan)
		w.renamed++
		w.plan[i].Title = fmt.Sprintf("Renamed%d", w.renamed)
		desc = fmt.Sprintf("amend: %s is now titled %s", w.plan[i].ID, w.plan[i].Title)
	case 8:
		if slices.ContainsFunc(w.plan, func(n planNode) bool { return n.ID == "integrate" }) {
			return "amend: a node is already called integrate"
		}
		i := s.R[1] % len(w.plan)
		old := w.plan[i].ID
		w.plan[i].ID = "integrate"
		for j := range w.plan {
			for k, d := range w.plan[j].DependsOn {
				if d == old {
					w.plan[j].DependsOn = slices.Clone(w.plan[j].DependsOn)
					w.plan[j].DependsOn[k] = "integrate"
				}
			}
		}
		desc = fmt.Sprintf("amend: node %s is now called integrate", old)
	}
	w.commitPlan()
	return desc
}

// layout returns the steps of the plan at HEAD as chain lays them out:
// the plan's nodes, the final integrate node it adds when the plan does
// not end in one, and the close step after the final node.
func (w *chainWorld) layout() []planNode {
	nodes := slices.Clone(w.plan)
	hasChild := map[string]bool{}
	for _, n := range nodes {
		for _, d := range n.DependsOn {
			hasChild[d] = true
		}
	}
	var leaves []string
	for _, n := range nodes {
		if !hasChild[n.ID] {
			leaves = append(leaves, n.ID)
		}
	}
	final := leaves[0]
	if len(leaves) != 1 || nodes[slices.IndexFunc(nodes, func(n planNode) bool { return n.ID == leaves[0] })].Type != "integrate" {
		final = "integrate"
		if slices.ContainsFunc(nodes, func(n planNode) bool { return n.ID == final }) {
			final = "integrate-2"
		}
		nodes = append(nodes, planNode{ID: final, Type: "integrate", Title: "Integrate every feature", DependsOn: leaves})
	}
	return append(nodes, planNode{ID: "close", Type: "close", Title: "Close M1", DependsOn: []string{final}})
}

// strict checks what a chain that returned without error leaves, with the
// plan at HEAD at hand: the newest runner of every step has the step's
// title, and a waiting one waits for the newest runners of the step's
// parents.
func (w *chainWorld) strict() string {
	newest := map[string]*task.Task{}
	for _, t := range w.tasks() {
		if t.Project == "demo-app" {
			newest[t.Stage] = t
		}
	}
	var bad []string
	for _, n := range w.layout() {
		t := newest["M1/"+n.ID]
		if t == nil {
			bad = append(bad, "step "+n.ID+" has no runner")
			continue
		}
		if want := "demo-app M1/" + n.ID + ": " + runner.TitleFrom(n.Title); t.Title != want {
			bad = append(bad, fmt.Sprintf("%s of step %s is titled %q, want %q", t.ID, n.ID, t.Title, want))
		}
		if t.Status != task.Queued && t.Status != task.Held {
			continue
		}
		var after []string
		for _, d := range n.DependsOn {
			if p := newest["M1/"+d]; p != nil {
				after = append(after, p.ID)
			}
		}
		if !slices.Equal(t.After, after) {
			bad = append(bad, fmt.Sprintf("%s of step %s waits for %v, want %v", t.ID, n.ID, t.After, after))
		}
	}
	if len(bad) == 0 {
		return ""
	}
	return "  " + strings.Join(bad, "\n  ")
}

func (w *chainWorld) tasks() []*task.Task {
	all, err := w.r.Store.List()
	if err != nil {
		w.t.Fatal(err)
	}
	return all
}

// pick returns the n-th, modulo, of the milestone's node runners keep
// accepts.
func (w *chainWorld) pick(n int, keep func(*task.Task) bool) *task.Task {
	var c []*task.Task
	for _, t := range w.tasks() {
		if strings.HasPrefix(t.Stage, "M1/") && t.Stage != "M1/plan" && keep(t) {
			c = append(c, t)
		}
	}
	if len(c) == 0 {
		return nil
	}
	return c[n%len(c)]
}

func (w *chainWorld) pop(q *[]string, n int) string {
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

func (w *chainWorld) step(s seqStep) string {
	ctx, r := w.ctx, w.r
	switch s.Op {
	case "chain":
		w.out.Reset()
		err := chainMilestone(ctx, &w.out, r, w.p, "M1", milestoneRun{Warn: func(string) {}})
		return fmt.Sprintf("chain: %v\n    %s", err, strings.ReplaceAll(strings.TrimSpace(w.out.String()), "\n", "\n    "))
	case "report":
		t := w.pick(s.R[0], func(t *task.Task) bool {
			return t.Status == task.Starting || t.Status == task.Running || t.Status == task.NeedsHuman
		})
		if t == nil {
			return "report: no live node runner"
		}
		st := []task.Status{task.Done, task.Failed}[s.R[1]%2]
		_, err := r.Report(t.ID, runner.ReportOptions{Status: st, Summary: "did it"})
		return fmt.Sprintf("report %s (%s) %s: %v", t.ID, t.Stage, st, err)
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
		r.CheckLost(ctx, t)
		return "agent of " + t.ID + " (" + t.Stage + ") gone"
	case "stop":
		t := w.pick(s.R[0], func(*task.Task) bool { return true })
		if t == nil {
			return "stop: no node runner"
		}
		_, err := r.Stop(ctx, t.ID, false)
		return fmt.Sprintf("stop %s (%s, %s): %v", t.ID, t.Stage, t.Status, err)
	case "run-launcher":
		id := w.pop(&w.launchers, s.R[0])
		if id == "" {
			return "run-launcher: none queued"
		}
		return fmt.Sprintf("launch %s: %v", id, r.Launch(ctx, id))
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
	case "settle-sweep":
		err := r.SettleWaiting(ctx, 0)
		w.mu.Lock()
		clear(w.lost)
		w.mu.Unlock()
		return fmt.Sprintf("settle sweep: %v", err)
	case "amend-plan":
		return w.amend(s)
	}
	w.t.Fatalf("unknown step %q", s.Op)
	return ""
}

// check returns what the store breaks, with the queued and lost follow-ups
// in flight, against the snapshot the last check saw.
func (w *chainWorld) check() []runner.Violation {
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
	return runner.CheckInvariants(s)
}

// drain runs every queued launcher and follow-up, and the sweep, until
// nothing is left, checking the rules after each.
func (w *chainWorld) drain(trace []string) ([]string, string) {
	for range 200 {
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

func TestChainSequences(t *testing.T) {
	checkSequences[chainScenario](t, 30, func(steps []seqStep) ([]string, string) {
		w := newChainWorld(t)
		var trace []string
		for i, s := range steps {
			trace = append(trace, w.step(s))
			if vs := w.check(); len(vs) > 0 {
				return trace, fmt.Sprintf("after step %d:\n%s", i+1, formatViolations(vs))
			}
			if s.Op == "chain" && strings.HasPrefix(trace[len(trace)-1], "chain: <nil>") {
				if bad := w.strict(); bad != "" {
					return trace, fmt.Sprintf("after step %d, the chain left:\n%s", i+1, bad)
				}
			}
		}
		return w.drain(trace)
	})
}
