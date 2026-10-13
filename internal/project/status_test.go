package project

import (
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func TestStatusReadsOnlyItsProjectTasks(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	rec := NewRecord("demo-app")
	rec.Type, rec.Pending = "cli", 2
	rec.ReferenceRepo = "https://user:secret@example.com/team/ref.git"
	rec.Milestones = []Milestone{{ID: "M0", Title: "scaffold", Status: "done"}, {ID: "M1", Title: "first slice", Status: "planned"}}
	reg := state.Project{Name: "demo-app", Path: "/work/demo-app"}
	tasks := []*task.Task{
		{ID: "t1", Project: "demo-app", Stage: "intake", Title: "read brief", Status: task.Done},
		{ID: "t2", Project: "demo-app", Stage: "spec", Title: "write spec", Status: task.Running},
		{ID: "t3", Project: "other-app", Stage: "ux", Title: "sketch", Status: task.NeedsHuman, Question: "A or B?"},
		{ID: "t4", Project: "demo-app", Title: "review", Status: task.NeedsHuman, Question: "Ship it?", UpdatedAt: now.Add(-12 * time.Minute)},
		{ID: "t5", Title: "no project", Status: task.Running},
		{ID: "t6", Project: "demo-app", Title: "gated", Status: task.Held, UpdatedAt: now.Add(-time.Hour)},
	}
	r := Status(&rec, reg, tasks, now)
	r.Remote = "https://token@example.com/team/demo-app.git"
	got := r.Text()
	for _, want := range []string{
		"demo-app · cli/standard · /work/demo-app\n",
		"remote: https://example.com/team/demo-app.git\n",
		"reference: https://example.com/team/ref.git\n",
		"stage: spec\n",
		"gates: spec open · pack open\n",
		"milestone: M1 first slice (planned)\n",
		"runners:\n  t2 running · stage spec · write spec\n",
		"waiting on you:\n  t4 needs-human 12m0s · review · Q: Ship it?\n  t6 held 1h0m0s · gated\n",
		"records not yet committed: 2\n",
		`next: answer t4: herdr-runner-kit answer t4 "<answer>" (1 more waiting)` + "\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status lacks %q:\n%s", want, got)
		}
	}
	for _, leak := range []string{"secret", "token", "t3", "t5", "other-app"} {
		if strings.Contains(got, leak) {
			t.Errorf("status shows %q:\n%s", leak, got)
		}
	}
	if n := strings.Count(got, "\n"); n > 20 {
		t.Errorf("status takes %d lines", n)
	}
}

func TestStatusNextStep(t *testing.T) {
	reg := state.Project{Name: "demo-app", Path: "/work/demo app"}
	decided := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	fresh := NewRecord("demo-app")
	approved := NewRecord("demo-app")
	approved.Gates.Spec = Gate{Status: GateApproved, Version: "v1", At: decided}
	specChanges := NewRecord("demo-app")
	specChanges.Gates.Spec = Gate{Status: GateChangesRequested, Version: "v1", At: decided}
	packChanges := approved
	packChanges.Gates.Pack = Gate{Status: GateChangesRequested, Version: "v1", At: decided}
	packed := approved
	packed.Gates.Pack = Gate{Status: GateApproved, Version: "v1", At: decided}
	waived := approved
	waived.Gates.Pack = Gate{Waived: true, At: decided}
	graduated := NewRecord("demo-app")
	graduated.Graduated = &Graduated{At: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)}
	running := []*task.Task{{ID: "t1", Project: "demo-app", Status: task.Running}}
	held := []*task.Task{{ID: "t2", Project: "demo-app", Status: task.Held}}
	dialog := []*task.Task{{ID: "t3", Project: "demo-app", Status: task.NeedsHuman, Error: "codex-trust"}}
	stage := func(name string, status task.Status, finished time.Time) *task.Task {
		return &task.Task{ID: "t9", Project: "demo-app", Stage: name, Status: status, FinishedAt: finished}
	}
	before, after := decided.Add(-time.Hour), decided.Add(time.Hour)
	reasoned := []*task.Task{{ID: "t2", Project: "demo-app", Status: task.Held, HoldReason: "needs D1"}}
	assessed := packed
	assessed.Changes = []Change{{ID: "CR-1", Status: ChangeOpen, Path: ChangesDir + "/CR-1-x.md", Task: "t8"}}
	impact := &task.Task{ID: "t8", Project: "demo-app", Stage: "change", Status: task.Done}
	intake := stage("intake", task.Done, before)
	specDone := stage("spec", task.Done, before)
	for _, c := range []struct {
		name    string
		rec     *Record
		tasks   []*task.Task
		missing bool
		want    string
	}{
		{"no record", nil, nil, false, "herdr-runner-kit project forget demo-app && herdr-runner-kit project register '/work/demo app' --name demo-app"},
		{"folder gone", &fresh, held, true, "the folder /work/demo app is gone"},
		{"held", &fresh, held, false, "herdr-runner-kit release t2"},
		{"dialog", &fresh, dialog, false, "herdr-runner-kit focus t3"},
		{"graduated", &graduated, nil, false, "nothing; the project has graduated"},
		{"runners", &fresh, running, false, "wait for the 1 runner(s) at work"},
		{"discovery", &fresh, nil, false, "start discovery from the brief in " + BriefDir + ": herdr-runner-kit project run intake --project demo-app"},
		{"after intake", &fresh, []*task.Task{intake}, false, "write the spec from the brief summary: herdr-runner-kit project run spec --project demo-app"},
		{"spec done", &fresh, []*task.Task{intake, specDone}, false, "spec done: ask the user to approve it, then record the outcome: herdr-runner-kit project approve spec --project demo-app --by NAME"},
		{"spec failed", &fresh, []*task.Task{stage("spec", task.Failed, before)}, false, "run it again: herdr-runner-kit project run spec --project demo-app"},
		{"spec changes not sent", &specChanges, []*task.Task{specDone}, false, "revise the spec with the feedback: herdr-runner-kit project run spec --project demo-app --stdin"},
		{"spec revised", &specChanges, []*task.Task{stage("spec", task.Done, after)}, false, "spec done: ask the user to approve it"},
		{"after the spec", &approved, []*task.Task{specDone}, false, "run the next discovery stage: herdr-runner-kit project run ux --project demo-app"},
		{"after ux", &approved, []*task.Task{stage("ux", task.Done, after)}, false, "project run design --project demo-app"},
		{"later stages done", &approved, []*task.Task{
			stage("ux", task.Done, after), stage("design", task.Done, after),
			stage("architecture", task.Done, after), stage("roadmap", task.Done, after),
		}, false, "assemble the stakeholder approval pack: herdr-runner-kit project run pack --project demo-app"},
		{
			"pack done", &approved,
			[]*task.Task{stage("pack", task.Done, after)},
			false,
			"pack done: share pack.html / pack.pdf from " + ApprovalDir + "/ with the stakeholders, then record the outcome: herdr-runner-kit project approve pack --project demo-app --by NAME",
		},
		{"pack changes not sent", &packChanges, []*task.Task{stage("pack", task.Done, before)}, false, "revise the pack with the feedback: herdr-runner-kit project run pack --project demo-app --stdin"},
		{"pack approved", &packed, nil, false, "spawn a runner for the next piece of work"},
		{"pack waived", &waived, nil, false, "spawn a runner for the next piece of work"},
		{"held for a dependency", &packed, reasoned, false, "release t2 once this is settled: needs D1; then: herdr-runner-kit release t2"},
		{
			"change assessed", &assessed,
			[]*task.Task{impact},
			false,
			"CR-1's impact is in " + ChangesDir + "/CR-1-x.md: ask the user, then record the decision: herdr-runner-kit project cr decide 1 <now|next|backlog|reject> --project demo-app",
		},
		{"change being assessed", &assessed, []*task.Task{{ID: "t8", Project: "demo-app", Status: task.Running}}, false, "wait for the 1 runner(s) at work"},
	} {
		r := Status(c.rec, reg, c.tasks, time.Now())
		r.PathMissing = c.missing
		if got := r.Next(); !strings.Contains(got, c.want) {
			t.Errorf("%s: next = %q, want %q", c.name, got, c.want)
		}
		if got := r.Next(); strings.Contains(got, "spawn --project") && c.rec != nil && c.rec.Gates.Pack.Status == "" && !c.rec.Gates.Pack.Waived {
			t.Errorf("%s: discovery suggests a plain spawn: %q", c.name, got)
		}
	}
}

func TestStatusChangesAndBlockers(t *testing.T) {
	rec := NewRecord("demo-app")
	rec.Reapproval = Reapproval{Needed: true, Reason: "scope changed by CR-2"}
	rec.Changes = []Change{
		{ID: "CR-1", Status: ChangeOpen, Task: "t7"},
		{ID: "CR-2", Class: ClassScope, Status: ChangeNext, Milestone: "M2"},
		{ID: "CR-3", Class: ClassScope, Status: ChangeNow, Blocking: true, Milestone: "M1"},
		{ID: "CR-4", Class: ClassScope, Status: ChangeRejected},
		{ID: "CR-5", Class: ClassTweak, Status: ChangeBacklog},
	}
	rec.Dependencies = []Dependency{
		{ID: "D1", Name: "Payments sandbox", Owner: "vendor", Status: "requested", Milestone: "M2", SecretLocation: "team vault"},
		{ID: "D2", Name: "Domain", Owner: "user", Status: DependencyReady},
	}
	tasks := []*task.Task{{ID: "t5", Project: "demo-app", Title: "checkout", Status: task.Held, HoldReason: "needs D1"}}
	got := Status(&rec, state.Project{Name: "demo-app", Path: "/work/demo-app"}, tasks, time.Now()).Text()
	for _, want := range []string{
		"re-approval needed before the next milestone: scope changed by CR-2\n",
		"changes: CR-1 open (t7) · CR-2 scope next M2 · CR-3 scope now blocking M1\n",
		"blockers: D1 Payments sandbox (requested, vendor, needed by M2)\n",
		"  t5 held · checkout · for: needs D1\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "team vault") {
		t.Errorf("status names where a secret is kept:\n%s", got)
	}
	empty := NewRecord("demo-app")
	got = Status(&empty, state.Project{Name: "demo-app", Path: "/work/demo-app"}, nil, time.Now()).Text()
	if !strings.Contains(got, "changes: none\nblockers: none\n") {
		t.Errorf("status:\n%s", got)
	}
}

func TestStatusWithoutRecord(t *testing.T) {
	r := Status(nil, state.Project{Name: "demo-app", Path: "/work/demo-app"}, nil, time.Now())
	got := r.Text()
	if !strings.Contains(got, "record: missing\n") || !strings.Contains(got, "project register /work/demo-app --name demo-app") ||
		strings.Contains(got, "stage:") {
		t.Errorf("status:\n%s", got)
	}
	if r.Stage != "intake" || r.Milestone() != "-" {
		t.Errorf("stage %q, milestone %q", r.Stage, r.Milestone())
	}
}

func TestStatusOfAnUnfinishedSetup(t *testing.T) {
	rec := NewRecord("demo-app")
	r := Status(&rec, state.Project{Name: "demo-app", Path: "/work/demo-app", Reserved: true}, nil, time.Now())
	got := r.Text()
	if !strings.Contains(got, "setup: not finished\n") || !strings.Contains(got, "next: wait for herdr-runner-kit new to finish; if it was interrupted, drop the project with: herdr-runner-kit project forget demo-app") {
		t.Errorf("status:\n%s", got)
	}
}
