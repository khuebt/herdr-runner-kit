package project

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func savedRecord(t *testing.T) *state.Store {
	t.Helper()
	s := openStore(t)
	if err := Save(s, NewRecord("demo-app")); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestChangeNumberingAndPathAcrossGraduation(t *testing.T) {
	s := savedRecord(t)
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	c1, err := AddChange(s, "demo-app", "Add CSV export\nfor admins", "add-csv-export", now)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := AddChange(s, "demo-app", "Drop the share page", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if c1.ID != "CR-1" || c1.Path != ChangesDir+"/CR-1-add-csv-export.md" || c1.Title != "Add CSV export for admins" || c1.Status != ChangeOpen ||
		c2.ID != "CR-2" || c2.Path != ChangesDir+"/CR-2-change.md" {
		t.Errorf("changes = %+v %+v", c1, c2)
	}
	if _, err := AddChange(s, "demo-app", "  ", "x", now); err == nil {
		t.Error("allocated an empty change request")
	}
	// A request whose runner never started is dropped, and its number is
	// free again; one with a runner stays.
	if err := DropChange(s, "demo-app", "CR-2"); err != nil {
		t.Fatal(err)
	}
	if err := SetChangeTask(s, "demo-app", "CR-1", "t4"); err != nil {
		t.Fatal(err)
	}
	if err := DropChange(s, "demo-app", "CR-1"); err == nil {
		t.Error("dropped a change request that has a runner")
	}
	if _, err := Update(s, "demo-app", func(r *Record) error {
		r.Graduated = &Graduated{At: now}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c3, err := AddChange(s, "demo-app", "Rename lists to shelves", "rename", now)
	if err != nil {
		t.Fatal(err)
	}
	if c3.ID != "CR-2" || c3.Path != GraduatedChangesDir+"/CR-2-rename.md" {
		t.Errorf("after graduation = %+v", c3)
	}
	rec := load(t, s)
	if len(rec.Changes) != 2 || rec.Changes[0].Task != "t4" || rec.Pending != 3 {
		t.Errorf("record = %+v", rec)
	}
	if got := NewStageData(rec, "/work", Tools{}, "", now).ChangesDir; got != GraduatedChangesDir {
		t.Errorf("stage data changes dir = %s", got)
	}
	for in, want := range map[string]string{"3": "CR-3", "CR-12": "CR-12", "cr3": "CR-3", " cr-7 ": "CR-7"} {
		if got, err := ParseChangeID(in); err != nil || got != want {
			t.Errorf("ParseChangeID(%q) = %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "0", "CR-", "D3", "3a"} {
		if _, err := ParseChangeID(bad); err == nil {
			t.Errorf("ParseChangeID(%q) accepted", bad)
		}
	}
}

func TestChangeClassFromDecisions(t *testing.T) {
	for want, decisions := range map[string][]string{
		ClassScope:  {"store -> sqlite", "Class -> scope."},
		ClassWithin: {"class ->  within-milestone"},
		"":          {"class -> huge", "category -> scope"},
	} {
		if got := ChangeClass(decisions); got != want {
			t.Errorf("ChangeClass(%q) = %q, want %q", decisions, got, want)
		}
	}
}

// approvedProject returns a project whose spec and pack are approved, with
// milestones M0 (in progress) and M1 imported from the roadmap proposal.
func approvedProject(t *testing.T) (*state.Store, string) {
	t.Helper()
	dir := newRepo(t)
	s := registered(t, dir)
	commitSpec(t, dir, 1, "standard")
	if _, err := decide(s, GateSpec, "approve"); err != nil {
		t.Fatal(err)
	}
	commitPack(t, dir, 1, proposal)
	if _, err := decide(s, GatePack, "approve"); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(s, "demo-app", func(r *Record) error {
		r.Milestones[0].Status = MilestoneInProgress
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s, dir
}

// impactChange allocates a change request whose impact runner is done and
// reported decisions.
func impactChange(t *testing.T, s *state.Store, title string, decisions ...string) Change {
	t.Helper()
	c, err := AddChange(s, "demo-app", title, "x", decidedAt)
	if err != nil {
		t.Fatal(err)
	}
	tk := &task.Task{
		Kind: "report", Agent: "claude", Title: title, Owner: task.ProjectOwner("demo-app"), Project: "demo-app",
		Stage: "change", Gate: task.GateAuto, Status: task.Done, Decisions: decisions,
	}
	if err := s.Create(tk); err != nil {
		t.Fatal(err)
	}
	if err := SetChangeTask(s, "demo-app", c.ID, tk.ID); err != nil {
		t.Fatal(err)
	}
	c.Task = tk.ID
	return c
}

func decideChange(s *state.Store, id, outcome string, mutate ...func(*ChangeDecision)) (ChangeDecided, error) {
	o := ChangeDecision{ID: id, Outcome: outcome, By: "Lan Pham", Now: decidedAt}
	for _, m := range mutate {
		m(&o)
	}
	return DecideChange(s, "demo-app", o)
}

func TestChangeDecideKeepsTheRunningMilestone(t *testing.T) {
	s, dir := approvedProject(t)
	ctx := context.Background()
	commits := commitCount(t, dir)

	scope := impactChange(t, s, "Add CSV export", "class -> scope")
	if _, err := decideChange(s, scope.ID, ChangeNow); err == nil || !strings.Contains(err.Error(), "M0 is in progress, which keeps its scope") {
		t.Fatalf("now without --blocking: %v", err)
	}
	if _, err := decideChange(s, scope.ID, ChangeNext, func(o *ChangeDecision) { o.Blocking = true }); err == nil {
		t.Error("--blocking accepted with next")
	}
	d, err := decideChange(s, scope.ID, ChangeNow, func(o *ChangeDecision) { o.Blocking = true })
	if err != nil {
		t.Fatal(err)
	}
	if c := d.Change; c.Class != ClassScope || c.Status != ChangeNow || !c.Blocking || c.Milestone != "M0" || !d.Reapproval {
		t.Errorf("decided = %+v", d)
	}
	if _, err := decideChange(s, scope.ID, ChangeNext); err == nil || !strings.Contains(err.Error(), "already decided") {
		t.Errorf("decided twice: %v", err)
	}

	// Re-approval holds only the next milestone's start: the pack gate,
	// which the running milestone and graduation rely on, stays passed.
	rec := load(t, s)
	if !rec.Reapproval.Needed || rec.Reapproval.Reason != "scope changed by "+scope.ID {
		t.Errorf("reapproval = %+v", rec.Reapproval)
	}
	if err := CheckReapproval(rec); err == nil || !strings.Contains(err.Error(), "new approval before the next milestone") {
		t.Errorf("milestone start with re-approval pending: %v", err)
	}
	if err := RequireGate(ctx, dir, rec, GatePack); err != nil {
		t.Errorf("the pack gate closed: %v", err)
	}

	next := impactChange(t, s, "Drop sharing", "class -> scope")
	if d, err := decideChange(s, next.ID, ChangeNext); err != nil || d.Change.Milestone != "M1" || d.Change.Blocking {
		t.Errorf("next = %+v %v", d, err)
	}
	within := impactChange(t, s, "Longer list names", "class -> within-milestone")
	if d, err := decideChange(s, within.ID, ChangeNow); err != nil || d.Change.Milestone != "M0" || d.Reapproval {
		t.Errorf("within now = %+v %v", d, err)
	}
	later := impactChange(t, s, "Dark mode", "class -> scope")
	if d, err := decideChange(s, later.ID, ChangeBacklog); err != nil || d.Backlog != "B1" || d.Reapproval {
		t.Errorf("backlog = %+v %v", d, err)
	}
	no := impactChange(t, s, "Offline mode", "class -> scope")
	if _, err := decideChange(s, no.ID, "reject"); err == nil || !strings.Contains(err.Error(), "--note") {
		t.Errorf("reject without a reason: %v", err)
	}
	if d, err := decideChange(s, no.ID, "reject", func(o *ChangeDecision) { o.Note = "out of budget" }); err != nil || d.Change.Status != ChangeRejected {
		t.Errorf("reject = %+v %v", d, err)
	}

	rec = load(t, s)
	if rec.Reapproval.Reason != "scope changed by CR-1, CR-2" || len(rec.Backlog) != 1 || rec.Backlog[0].Text != "CR-4: Dark mode" {
		t.Errorf("record = %+v", rec)
	}
	if got := rec.Decisions[len(rec.Decisions)-1].Text; got != "CR-5 (scope) decided rejected by Lan Pham: out of budget" {
		t.Errorf("last decision %q", got)
	}
	if got := commitCount(t, dir); got != commits {
		t.Errorf("deciding change requests made %d commits", got-commits)
	}
}

func TestChangeDecideNeedsAClass(t *testing.T) {
	s, _ := approvedProject(t)
	c, err := AddChange(s, "demo-app", "Add CSV export", "x", decidedAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decideChange(s, c.ID, ChangeNext); err == nil || !strings.Contains(err.Error(), "no impact runner") {
		t.Errorf("no runner: %v", err)
	}
	silent := impactChange(t, s, "Drop sharing")
	if _, err := decideChange(s, silent.ID, ChangeNext); err == nil || !strings.Contains(err.Error(), "reported no class") {
		t.Errorf("no class reported: %v", err)
	}
	if _, err := decideChange(s, "CR-9", ChangeNext); err == nil || !strings.Contains(err.Error(), "no change request CR-9") {
		t.Errorf("unknown request: %v", err)
	}
	if _, err := decideChange(s, silent.ID, "later"); err == nil {
		t.Error("unknown decision accepted")
	}
}

func TestChangeScopeBeforeThePackSetsNoReapproval(t *testing.T) {
	dir := newRepo(t)
	s := registered(t, dir)
	c := impactChange(t, s, "Add CSV export", "class -> scope")
	d, err := decideChange(s, c.ID, ChangeNow)
	if err != nil || d.Reapproval || d.Change.Milestone != "" {
		t.Fatalf("decided = %+v %v", d, err)
	}
	if err := CheckReapproval(load(t, s)); err != nil {
		t.Error(err)
	}
}

func TestRecordsWaitForTheNextCommitPoint(t *testing.T) {
	dir := newRepo(t)
	s := registered(t, dir)
	commitSpec(t, dir, 1, "standard")
	commits := commitCount(t, dir)
	if _, err := AddBacklog(s, "demo-app", "Dark mode\nlater", decidedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := AddDependency(s, "demo-app", DependencyOptions{
		Name: "Payments sandbox", Owner: "vendor", Workaround: "fake gateway", SecretLocation: "1Password: payments sandbox",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := AddChange(s, "demo-app", "Add CSV export", "add-csv-export", decidedAt); err != nil {
		t.Fatal(err)
	}
	if got := commitCount(t, dir); got != commits {
		t.Fatalf("records made %d commits", got-commits)
	}
	if rec := load(t, s); rec.Pending != 3 {
		t.Errorf("pending = %d", rec.Pending)
	}
	if _, err := decide(s, GateSpec, "approve"); err != nil {
		t.Fatal(err)
	}
	if got := commitCount(t, dir); got != commits+1 {
		t.Errorf("the gate outcome made %d commits", got-commits)
	}
	if got := run(t, dir, "show", "HEAD:"+BacklogPath); !strings.Contains(got, "- B1 [open] Dark mode later") {
		t.Errorf("backlog.md:\n%s", got)
	}
	snapshot := run(t, dir, "show", "HEAD:"+SnapshotPath)
	for _, want := range []string{
		"- D1 [needed] Payments sandbox, owner vendor; workaround: fake gateway\n",
		"- CR-1 [open] Add CSV export, see " + ChangesDir + "/CR-1-add-csv-export.md\n",
	} {
		if !strings.Contains(snapshot, want) {
			t.Errorf("project.md lacks %q:\n%s", want, snapshot)
		}
	}
	if rec := load(t, s); rec.Pending != 0 {
		t.Errorf("pending after the commit = %d", rec.Pending)
	}
}

func TestDependencyRefusesSecrets(t *testing.T) {
	s := savedRecord(t)
	const secret = "sk-live0123456789abcdefXYZ"
	for _, o := range []DependencyOptions{
		{Name: "Payments", Owner: "vendor", SecretLocation: secret},
		{Name: "Payments", Owner: "vendor", Workaround: "use API_KEY=hunter2"},
		{Name: "token " + secret, Owner: "vendor"},
	} {
		_, err := AddDependency(s, "demo-app", o)
		if err == nil || !strings.Contains(err.Error(), "looks like it holds a secret") {
			t.Errorf("%+v: %v", o, err)
			continue
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("the refusal repeats the secret: %v", err)
		}
	}
	if rec := load(t, s); len(rec.Dependencies) != 0 || rec.Pending != 0 {
		t.Errorf("a refused dependency was recorded: %+v", rec)
	}
	if _, err := AddBacklog(s, "demo-app", "rotate password=hunter2", decidedAt); err == nil {
		t.Error("a backlog entry with a secret was recorded")
	}
	// The roadmap proposal's dependencies go through the same check.
	block := strings.Replace(proposal, "secretLocation: team vault", "secretLocation: "+secret, 1)
	if _, err := ParseBlock([]byte(block), RoadmapProposal, NewRecord("demo-app")); err == nil ||
		!strings.Contains(err.Error(), "dependencies[0].secretLocation looks like it holds a secret") {
		t.Errorf("proposal with a secret: %v", err)
	}
}

func TestDependencyAddAndSet(t *testing.T) {
	s := savedRecord(t)
	d, err := AddDependency(s, "demo-app", DependencyOptions{Name: "Payments sandbox", Owner: "vendor", Milestone: "M1"})
	if err != nil || d.ID != "D1" || d.Status != DependencyNeeded {
		t.Fatalf("add = %+v %v", d, err)
	}
	for _, o := range []DependencyOptions{
		{Name: "payments SANDBOX", Owner: "vendor"},
		{Name: "Mail", Owner: "ops"},
		{Name: " ", Owner: "user"},
	} {
		if _, err := AddDependency(s, "demo-app", o); err == nil {
			t.Errorf("%+v accepted", o)
		}
	}
	if _, err := Update(s, "demo-app", func(r *Record) error {
		r.Milestones = []Milestone{{ID: "M1", Title: "Lists", Status: MilestonePlanned}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := AddDependency(s, "demo-app", DependencyOptions{Name: "Mail", Owner: "user", Milestone: "M7"}); err == nil {
		t.Error("an unknown milestone was accepted")
	}
	if _, err := SetDependencyStatus(s, "demo-app", "D1", "done"); err == nil {
		t.Error("an unknown status was accepted")
	}
	if _, err := SetDependencyStatus(s, "demo-app", "D9", DependencyReady); err == nil {
		t.Error("an unknown dependency was accepted")
	}
	if d, err := SetDependencyStatus(s, "demo-app", "D1", DependencyReady); err != nil || d.Status != DependencyReady {
		t.Errorf("set = %+v %v", d, err)
	}
	for in, want := range map[string]string{"2": "D2", "d2": "D2", "D10": "D10"} {
		if got, err := ParseDependencyID(in); err != nil || got != want {
			t.Errorf("ParseDependencyID(%q) = %q %v", in, got, err)
		}
	}

	tasks := []*task.Task{
		{ID: "t1", Project: "demo-app", Status: task.Held, HoldReason: "needs D1 keys"},
		{ID: "t2", Project: "demo-app", Status: task.Held, HoldReason: "needs D11"},
		{ID: "t3", Project: "other-app", Status: task.Held, HoldReason: "needs D1"},
		{ID: "t4", Project: "demo-app", Status: task.Running, HoldReason: "needs D1"},
		{ID: "t5", Project: "demo-app", Status: task.Held, HoldReason: "waits for d1"},
	}
	var ids []string
	for _, tk := range HeldFor(tasks, "demo-app", "D1") {
		ids = append(ids, tk.ID)
	}
	if strings.Join(ids, ",") != "t1,t5" {
		t.Errorf("held for D1 = %v", ids)
	}
}

func TestActiveTasksList(t *testing.T) {
	tasks := []*task.Task{
		{ID: "t1", Project: "demo-app", Stage: "ux", Title: "sketch", Status: task.Running},
		{ID: "t2", Project: "demo-app", Title: "list page", Status: task.Queued, Worktree: &task.Worktree{Branch: "feat/list"}},
		{ID: "t3", Project: "demo-app", Title: "old", Status: task.Done},
		{ID: "t4", Project: "other-app", Title: "x", Status: task.Running},
	}
	want := "- t1 running · stage ux · sketch\n- t2 queued · branch feat/list · list page"
	if got := ActiveTasks(tasks, "demo-app"); got != want {
		t.Errorf("active tasks:\n%s", got)
	}
	if got := ActiveTasks(nil, "demo-app"); got != "none" {
		t.Errorf("no tasks: %q", got)
	}
}
