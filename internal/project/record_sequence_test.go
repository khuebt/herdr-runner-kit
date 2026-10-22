package project_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/project"
	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// Randomized record sequences drive one project's record through gate
// approvals, milestone starts, dones with and without a roadmap update,
// reopens, change decisions, registration from the committed snapshot and
// dones whose commit fails while another command runs, and check the
// record rules of docs/state-model.md after each step. The count follows
// the standard -quickchecks flag; -seqseed replays a run. The engine is the
// one of the task lifecycle sequences in internal/runner.

var seqSeed = flag.Int64("seqseed", 0, "seed of the randomized sequence tests; 0 picks one")

type seqStep struct {
	Op string
	R  [4]int
}

type seqOp struct {
	name   string
	weight int
}

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

// recordScenario is a sequence of record steps.
type recordScenario []seqStep

var recordOps = []seqOp{
	{"approve-pack", 3},
	{"start", 4},
	{"done", 4},
	{"reopen", 1},
	{"cr-decide", 2},
	{"register", 1},
	{"done-race", 3},
}

func (recordScenario) Generate(rand *rand.Rand, size int) reflect.Value {
	return reflect.ValueOf(recordScenario(genSteps(rand, size, 14, recordOps)))
}

// recWorld is one registered project with its repository, and the close
// runners of its dones, which the record rules look for.
type recWorld struct {
	t      *testing.T
	ctx    context.Context
	s      *state.Store
	p      state.Project
	now    time.Time
	closer []*task.Task
	packs  int // pack versions committed so far
	notes  int // releases made so far
	extra  int // milestones and features the roadmaps added so far
	prev   *runner.Snapshot
}

func (w *recWorld) git(args ...string) string {
	w.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", w.p.Path}, args...)...).CombinedOutput()
	if err != nil {
		w.t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (w *recWorld) commit(files map[string]string, msg string) {
	w.t.Helper()
	for rel, content := range files {
		full := filepath.Join(w.p.Path, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			w.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			w.t.Fatal(err)
		}
		w.git("add", rel)
	}
	w.git("commit", "-q", "-m", msg)
}

// newRecWorld returns a project whose spec and pack are approved, with the
// roadmap M0, M1 and M2 imported.
func newRecWorld(t *testing.T) *recWorld {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for k, v := range map[string]string{
		"GIT_AUTHOR_NAME": "Lan Pham", "GIT_COMMITTER_NAME": "Lan Pham",
		"GIT_AUTHOR_EMAIL": "lan@example.com", "GIT_COMMITTER_EMAIL": "lan@example.com",
	} {
		t.Setenv(k, v)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(dir, "demo-app")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	w := &recWorld{t: t, ctx: context.Background(), s: s, p: state.Project{Name: "demo-app", Path: dir}, now: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)}
	w.git("init", "-q", "-b", "main")
	w.git("commit", "-q", "--allow-empty", "-m", "init")
	if w.p, _, err = project.Register(w.ctx, s, dir, "", ""); err != nil {
		t.Fatal(err)
	}
	w.commit(map[string]string{project.SpecPath: "---\nversion: 1\ntype: cli\ntrack: standard\n---\n\n# Spec\n"}, "docs(discovery): spec")
	if _, err := project.Decide(w.ctx, s, "demo-app", project.DecideOptions{Gate: project.GateSpec, Outcome: "approve", By: "Lan Pham", Now: w.now}); err != nil {
		t.Fatal(err)
	}
	ms := []project.Milestone{
		{ID: "M0", Title: "Scaffold"},
		{ID: "M1", Title: "Lists", Features: []project.Feature{{ID: "F1", Title: "Add"}, {ID: "F2", Title: "Share"}}},
		{ID: "M2", Title: "Reports", Features: []project.Feature{{ID: "F3", Title: "Export"}}},
	}
	if err := w.approvePack(ms); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *recWorld) record() project.Record {
	w.t.Helper()
	rec, err := project.Load(w.s, "demo-app")
	if err != nil {
		w.t.Fatal(err)
	}
	return rec
}

// roadmapYAML renders milestones as a roadmap block's milestones section.
func roadmapYAML(ms []project.Milestone) string {
	var b strings.Builder
	b.WriteString("```yaml\nmilestones:\n")
	for _, m := range ms {
		fmt.Fprintf(&b, "  - id: %s\n    title: %s\n", m.ID, m.Title)
		if !m.Planned.IsZero() {
			fmt.Fprintf(&b, "    planned: %s\n", m.Planned.Format(time.DateOnly))
		}
		if len(m.Features) > 0 {
			b.WriteString("    features:\n")
			for _, f := range m.Features {
				fmt.Fprintf(&b, "      - {id: %s, title: %s}\n", f.ID, f.Title)
			}
		}
	}
	b.WriteString("```\n")
	return b.String()
}

// approvePack commits a new pack whose roadmap proposal lists ms and
// approves it.
func (w *recWorld) approvePack(ms []project.Milestone) error {
	w.packs++
	w.commit(map[string]string{
		project.PackDir(w.packs) + "/" + project.PackHTML: fmt.Sprintf("<html>pack %d</html>\n", w.packs),
		project.PackDir(w.packs) + "/" + project.PackPDF:  "%PDF\n",
		project.RoadmapProposalPath:                       "# Roadmap\n\n" + roadmapYAML(ms),
	}, "docs(approval): pack")
	_, err := project.Decide(w.ctx, w.s, "demo-app", project.DecideOptions{Gate: project.GatePack, Outcome: "approve", By: "Lan Pham", Now: w.now})
	return err
}

// open returns the record's milestones that are not done, the ones a
// roadmap lists.
func open(rec project.Record) []project.Milestone {
	var ms []project.Milestone
	for _, m := range rec.Milestones {
		if m.Status != project.MilestoneDone {
			m.Features = slices.Clone(m.Features)
			ms = append(ms, m)
		}
	}
	return ms
}

// amendRoadmap changes the planned milestones of ms the way a new pack
// does: a title, a feature moved to a later one, or a milestone added.
func (w *recWorld) amendRoadmap(ms []project.Milestone, s seqStep) ([]project.Milestone, string) {
	var planned []int
	for i, m := range ms {
		if m.Status == project.MilestonePlanned {
			planned = append(planned, i)
		}
	}
	w.extra++
	switch s.R[1] % 3 {
	case 0:
		if len(planned) > 0 {
			i := planned[s.R[2]%len(planned)]
			ms[i].Title = fmt.Sprintf("Renamed%d", w.extra)
			return ms, fmt.Sprintf("%s titled %s", ms[i].ID, ms[i].Title)
		}
	case 1:
		if len(planned) > 1 {
			i := planned[s.R[2]%(len(planned)-1)]
			if len(ms[i].Features) > 0 {
				f := ms[i].Features[0]
				ms[i].Features = ms[i].Features[1:]
				j := planned[len(planned)-1]
				ms[j].Features = append(ms[j].Features, f)
				return ms, fmt.Sprintf("%s moved from %s to %s", f.ID, ms[i].ID, ms[j].ID)
			}
		}
	}
	id := fmt.Sprintf("M%d", 2+w.extra)
	ms = append(ms, project.Milestone{ID: id, Title: "Extra", Features: []project.Feature{{ID: fmt.Sprintf("X%d", w.extra), Title: "More"}}})
	return ms, "added " + id
}

// running returns the milestone in progress, or "".
func running(rec project.Record) string {
	for _, m := range rec.Milestones {
		if m.Status == project.MilestoneInProgress {
			return m.ID
		}
	}
	return ""
}

// release merges a release of milestone id into main, with a roadmap
// update that moves the first feature of id to the next milestone when
// withUpdate, and adds the done close runner of that release.
func (w *recWorld) release(id string, withUpdate bool) {
	w.notes++
	branch := fmt.Sprintf("release/%s-n%d", id, w.notes)
	files := map[string]string{project.ReleaseNotesPath(id): fmt.Sprintf("# %s release notes %d\n", id, w.notes)}
	if withUpdate {
		rec := w.record()
		ms := open(rec)
		i := slices.IndexFunc(ms, func(m project.Milestone) bool { return m.ID == id })
		if i+1 < len(ms) && len(ms[i].Features) > 0 {
			ms[i+1].Features = append(ms[i+1].Features, ms[i].Features[0])
			ms[i].Features = ms[i].Features[1:]
		}
		files[project.RoadmapUpdatePath(id)] = "# Roadmap update\n\n" + roadmapYAML(ms[i:])
	}
	w.git("checkout", "-q", "-b", branch, "main")
	w.commit(files, "docs(release): "+id)
	w.git("checkout", "-q", "main")
	w.git("merge", "-q", "--no-ff", "--no-edit", branch)
	created := true
	w.closer = append(w.closer, &task.Task{
		ID: fmt.Sprintf("t%d", len(w.closer)+1), Project: "demo-app", Stage: project.CloseStage(id), Status: task.Done,
		FinishedAt: w.now, Worktree: &task.Worktree{Branch: branch, BranchCreated: &created},
	})
}

func (w *recWorld) finish(id string) error {
	_, err := project.FinishMilestone(w.ctx, w.s, w.p, id, w.closer, project.FinishOptions{Now: w.now})
	return err
}

// decideChange adds a change request with an impact runner's class and
// decides it.
func (w *recWorld) decideChange(s seqStep) string {
	c, err := project.AddChange(w.s, "demo-app", "Change", "change", w.now)
	if err != nil {
		return "cr: " + err.Error()
	}
	class := project.Classes[s.R[1]%len(project.Classes)]
	tk := &task.Task{
		Kind: "report", Agent: "claude", Title: "impact", Owner: task.ProjectOwner("demo-app"), Project: "demo-app",
		Stage: "change", Gate: task.GateAuto, Status: task.Done, FinishedAt: w.now, Decisions: []string{"class -> " + class},
	}
	if err := w.s.Create(tk); err != nil {
		w.t.Fatal(err)
	}
	if err := project.SetChangeTask(w.s, "demo-app", c.ID, tk.ID); err != nil {
		w.t.Fatal(err)
	}
	outcome := []string{project.ChangeNow, project.ChangeNext, project.ChangeBacklog, "reject"}[s.R[2]%4]
	o := project.ChangeDecision{ID: c.ID, Outcome: outcome, Blocking: s.R[3]%2 == 0, By: "Lan Pham", Note: "why", Now: w.now}
	_, err = project.DecideChange(w.s, "demo-app", o)
	return fmt.Sprintf("cr %s (%s) %s: %v", c.ID, class, outcome, err)
}

func (w *recWorld) step(s seqStep) string {
	switch s.Op {
	case "approve-pack":
		ms, desc := w.amendRoadmap(open(w.record()), s)
		return fmt.Sprintf("approve pack v%d, %s: %v", w.packs+1, desc, w.approvePack(ms))
	case "start":
		rec := w.record()
		i := slices.IndexFunc(rec.Milestones, func(m project.Milestone) bool { return m.Status == project.MilestonePlanned })
		if i < 0 {
			return "start: nothing planned"
		}
		id := rec.Milestones[i].ID
		_, _, err := project.StartMilestone(w.ctx, w.s, w.p, id, w.closer, w.now)
		return fmt.Sprintf("start %s: %v", id, err)
	case "done":
		id := running(w.record())
		if id == "" {
			return "done: nothing in progress"
		}
		withUpdate := s.R[1]%2 == 0
		w.release(id, withUpdate)
		return fmt.Sprintf("done %s (roadmap update %v): %v", id, withUpdate, w.finish(id))
	case "reopen":
		rec := w.record()
		if running(rec) != "" {
			return "reopen: a milestone is in progress"
		}
		i := slices.IndexFunc(rec.Milestones, func(m project.Milestone) bool { return m.Status != project.MilestoneDone })
		if i == 0 {
			return "reopen: nothing done"
		}
		if i < 0 {
			i = len(rec.Milestones)
		}
		id := rec.Milestones[i-1].ID
		_, err := project.ReopenMilestone(w.ctx, w.s, w.p, id, "a bug", w.now)
		return fmt.Sprintf("reopen %s: %v", id, err)
	case "cr-decide":
		return w.decideChange(s)
	case "register":
		return w.register()
	case "done-race":
		return w.doneRace(s)
	}
	w.t.Fatalf("unknown step %q", s.Op)
	return ""
}

// register registers the repository in a new store from its committed
// snapshot, and checks the record it takes.
func (w *recWorld) register() string {
	s, err := state.Open(filepath.Join(w.t.TempDir(), "state"))
	if err != nil {
		w.t.Fatal(err)
	}
	_, rec, err := project.Register(w.ctx, s, w.p.Path, "", "")
	if err != nil {
		return "register: " + err.Error()
	}
	if vs := runner.CheckInvariants(runner.Snapshot{Tasks: w.closer, Projects: map[string]project.Record{"demo-app": rec}, Now: w.now}); len(vs) > 0 {
		return "register took a record that breaks:\n" + formatViolations(vs)
	}
	return "register: <nil>"
}

// doneRace runs done on the milestone in progress, and inside its commit
// window another command that takes no milestone lock: a pack approval,
// which waits for the checkout lock to commit, or a change decision. Then
// the commit fails. What the other command changed must survive the undo.
func (w *recWorld) doneRace(s seqStep) string {
	id := running(w.record())
	if id == "" {
		return "done-race: nothing in progress"
	}
	w.release(id, false)
	var inner string
	var mid project.Record
	wait := func() {}
	project.CommitHook = func() error {
		project.CommitHook = nil
		if s.R[1]%2 == 0 {
			inner = w.decideChange(s)
			mid = w.record()
			return errors.New("the hook failed the commit")
		}
		ms, desc := w.amendRoadmap(open(w.record()), s)
		before := w.record().Gates.Pack
		done := make(chan error, 1)
		w.packs++
		n := w.packs
		w.commit(map[string]string{
			project.PackDir(n) + "/" + project.PackHTML: fmt.Sprintf("<html>pack %d</html>\n", n),
			project.PackDir(n) + "/" + project.PackPDF:  "%PDF\n",
			project.RoadmapProposalPath:                 "# Roadmap\n\n" + roadmapYAML(ms),
		}, "docs(approval): pack")
		go func() {
			_, err := project.Decide(w.ctx, w.s, "demo-app", project.DecideOptions{Gate: project.GatePack, Outcome: "approve", By: "Lan Pham", Now: w.now})
			done <- err
		}()
		// The approval records its outcome, then waits for the checkout
		// lock done holds to commit it.
		var err error
		for range 500 {
			select {
			case err = <-done:
			default:
				time.Sleep(10 * time.Millisecond)
			}
			if err != nil || w.record().Gates.Pack != before {
				break
			}
		}
		inner = fmt.Sprintf("approve pack v%d, %s", n, desc)
		mid = w.record()
		wait = func() {
			if err == nil {
				err = <-done
			}
			inner += fmt.Sprintf(": %v", err)
		}
		return errors.New("the hook failed the commit")
	}
	defer func() { project.CommitHook = nil }()
	err := w.finish(id)
	wait()
	desc := fmt.Sprintf("done %s, failing its commit while %s; done: %v", id, inner, err)
	if inner == "" {
		return desc
	}
	// The record the other command left, with the milestone back in
	// progress as before the failed done.
	got := w.record()
	want := slices.Clone(mid.Milestones)
	if i := slices.IndexFunc(want, func(m project.Milestone) bool { return m.ID == id }); i >= 0 {
		m := &want[i]
		m.Status, m.Closed, m.Tag = project.MilestoneInProgress, time.Time{}, ""
		m.Released = m.Released[:len(m.Released)-1]
		if len(m.Released) == 0 {
			m.Released = nil
		}
		if j := slices.IndexFunc(got.Milestones, func(g project.Milestone) bool { return g.ID == id }); j >= 0 {
			m.Branch = got.Milestones[j].Branch
		}
	}
	if !reflect.DeepEqual(got.Milestones, want) || !reflect.DeepEqual(got.Gates, mid.Gates) || !reflect.DeepEqual(got.Changes, mid.Changes) || !reflect.DeepEqual(got.Backlog, mid.Backlog) {
		return desc + fmt.Sprintf("\nLOST: the undo dropped what the other command changed:\n  milestones %+v\n  want       %+v\n  gates %+v / %+v",
			got.Milestones, want, got.Gates, mid.Gates)
	}
	return desc
}

func formatViolations(vs []runner.Violation) string {
	lines := make([]string, len(vs))
	for i, v := range vs {
		lines[i] = "  " + v.String()
	}
	return strings.Join(lines, "\n")
}

func (w *recWorld) check() []runner.Violation {
	cur := runner.Snapshot{Tasks: w.closer, Projects: map[string]project.Record{"demo-app": w.record()}, Now: w.now}
	s := cur
	s.Prev, w.prev = w.prev, &cur
	return runner.CheckInvariants(s)
}

func TestRecordSequences(t *testing.T) {
	checkSequences[recordScenario](t, 30, func(steps []seqStep) ([]string, string) {
		w := newRecWorld(t)
		var trace []string
		for i, s := range steps {
			line := w.step(s)
			trace = append(trace, line)
			if strings.Contains(line, "\nLOST: ") || strings.HasPrefix(line, "register took") {
				return trace, fmt.Sprintf("after step %d", i+1)
			}
			if vs := w.check(); len(vs) > 0 {
				return trace, fmt.Sprintf("after step %d:\n%s", i+1, formatViolations(vs))
			}
		}
		return trace, ""
	})
}
