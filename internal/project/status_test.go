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
		"gates: spec pending · pack pending\n",
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
	fresh := NewRecord("demo-app")
	approved := NewRecord("demo-app")
	approved.Gates.Spec.Status = "approved"
	graduated := NewRecord("demo-app")
	graduated.Graduated = &Graduated{At: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)}
	running := []*task.Task{{ID: "t1", Project: "demo-app", Status: task.Running}}
	held := []*task.Task{{ID: "t2", Project: "demo-app", Status: task.Held}}
	dialog := []*task.Task{{ID: "t3", Project: "demo-app", Status: task.NeedsHuman, Error: "codex-trust"}}
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
		{"discovery", &fresh, nil, false, "start discovery from the brief in " + BriefDir},
		{"after the spec", &approved, nil, false, "spawn a runner for the next piece of work"},
	} {
		r := Status(c.rec, reg, c.tasks, time.Now())
		r.PathMissing = c.missing
		if got := r.Next(); !strings.Contains(got, c.want) {
			t.Errorf("%s: next = %q, want %q", c.name, got, c.want)
		}
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
