package project

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// statusListCap bounds the runners and waiting tasks a status lists, so the
// whole report stays about one screen.
const statusListCap = 5

// Report is a read-only view of where a project stands, built from the
// kit's record and its tasks. It never reads herdr, so it works with herdr
// down.
type Report struct {
	Name, Path string
	// Record is nil when the kit holds no record for the project.
	Record *Record
	// Remote is the URL of the repository's origin, when it has one.
	Remote string
	// PathMissing is set when the project's folder no longer exists.
	PathMissing bool
	// Stage is the stage of the project's latest staged task, else intake.
	Stage string
	// Runners are the project's tasks at work or about to start; Waiting
	// are those waiting on the user.
	Runners, Waiting []*task.Task
	Now              time.Time
}

// Status builds the report of a registered project from its record (nil
// when there is none) and the tasks of every project.
func Status(rec *Record, reg state.Project, tasks []*task.Task, now time.Time) Report {
	r := Report{Name: reg.Name, Path: reg.Path, Record: rec, Stage: "intake", Now: now}
	for _, t := range tasks {
		if t.Project != reg.Name {
			continue
		}
		if t.Stage != "" {
			r.Stage = t.Stage // tasks come in creation order: the last one wins
		}
		switch t.Status {
		case task.NeedsHuman, task.Held:
			r.Waiting = append(r.Waiting, t)
		case task.Queued, task.Starting, task.Running:
			r.Runners = append(r.Runners, t)
		}
	}
	return r
}

// Text renders the report: a header, one line per fact and one
// recommended next step.
func (r Report) Text() string {
	var b strings.Builder
	rec := r.Record
	head := []string{r.Name}
	if rec != nil {
		head = append(head, rec.Type+"/"+rec.Track)
	}
	head = append(head, r.Path)
	if rec != nil && rec.Graduated != nil {
		head = append(head, "graduated "+day(rec.Graduated.At))
	}
	b.WriteString(strings.Join(head, " · ") + "\n")
	if r.PathMissing {
		b.WriteString("folder: missing\n")
	}
	if r.Remote != "" {
		fmt.Fprintf(&b, "remote: %s\n", RedactURL(r.Remote))
	}
	if rec == nil {
		b.WriteString("record: missing\n")
		fmt.Fprintf(&b, "next: %s\n", r.Next())
		return b.String()
	}
	if rec.ReferenceRepo != "" {
		fmt.Fprintf(&b, "reference: %s\n", RedactURL(rec.ReferenceRepo))
	}
	fmt.Fprintf(&b, "stage: %s\n", r.Stage)
	fmt.Fprintf(&b, "gates: spec %s · pack %s\n", gateLine(rec.Gates.Spec), gateLine(rec.Gates.Pack))
	if rec.Reapproval.Needed {
		fmt.Fprintf(&b, "re-approval needed: %s\n", orDash(oneLine(rec.Reapproval.Reason)))
	}
	b.WriteString("milestone: " + r.Milestone() + "\n")
	r.writeTasks(&b, "runners", r.Runners)
	r.writeTasks(&b, "waiting on you", r.Waiting)
	fmt.Fprintf(&b, "records not yet committed: %d\n", rec.Pending)
	fmt.Fprintf(&b, "next: %s\n", r.Next())
	return b.String()
}

// Milestone describes the current milestone in a few words.
func (r Report) Milestone() string {
	if r.Record == nil {
		return "-"
	}
	m := currentMilestone(*r.Record)
	if m == nil {
		return "none yet"
	}
	return fmt.Sprintf("%s %s (%s)", m.ID, oneLine(m.Title), orDash(m.Status))
}

func (r Report) writeTasks(b *strings.Builder, label string, list []*task.Task) {
	if len(list) == 0 {
		fmt.Fprintf(b, "%s: none\n", label)
		return
	}
	fmt.Fprintf(b, "%s:\n", label)
	for i, t := range list {
		if i == statusListCap {
			fmt.Fprintf(b, "  and %d more\n", len(list)-i)
			break
		}
		fmt.Fprintf(b, "  %s\n", r.taskLine(t))
	}
}

// taskLine is "t3 needs-human 12m · write spec · Q: A or B?".
func (r Report) taskLine(t *task.Task) string {
	parts := []string{t.ID + " " + string(t.Status)}
	if t.Status == task.NeedsHuman || t.Status == task.Held {
		if !t.UpdatedAt.IsZero() && r.Now.After(t.UpdatedAt) {
			parts[0] += " " + r.Now.Sub(t.UpdatedAt).Round(time.Minute).String()
		}
	}
	if t.Stage != "" {
		parts = append(parts, "stage "+t.Stage)
	}
	if t.Title != "" {
		parts = append(parts, oneLine(t.Title))
	}
	switch {
	case t.Status == task.NeedsHuman && t.Question != "":
		parts = append(parts, "Q: "+oneLine(t.Question))
	case t.Status == task.NeedsHuman && t.Error != "":
		parts = append(parts, "waits on the "+t.Error+" dialog in its tab")
	}
	return strings.Join(parts, " · ")
}

// Next is the one step the report recommends. The rules are checked in
// order and the first that applies wins.
func (r Report) Next() string {
	rec := r.Record
	switch {
	case rec == nil:
		return fmt.Sprintf("the kit has no record of %s; import it again with: herdr-runner-kit project forget %s && herdr-runner-kit project register %s --name %s",
			r.Name, r.Name, shellCommand([]string{r.Path}), r.Name)
	case r.PathMissing:
		return fmt.Sprintf("the folder %s is gone; move it back, or drop the project with: herdr-runner-kit project forget %s", r.Path, r.Name)
	case len(r.Waiting) > 0:
		t := r.Waiting[0]
		more := ""
		if n := len(r.Waiting) - 1; n > 0 {
			more = fmt.Sprintf(" (%d more waiting)", n)
		}
		switch {
		case t.Status == task.Held:
			return fmt.Sprintf("release %s when it may start: herdr-runner-kit release %s%s", t.ID, t.ID, more)
		case t.Question != "":
			return fmt.Sprintf("answer %s: herdr-runner-kit answer %s \"<answer>\"%s", t.ID, t.ID, more)
		default:
			return fmt.Sprintf("answer the dialog of %s in its tab: herdr-runner-kit focus %s%s", t.ID, t.ID, more)
		}
	case rec.Graduated != nil:
		return "nothing; the project has graduated"
	case len(r.Runners) > 0:
		return fmt.Sprintf("wait for the %d runner(s) at work: herdr-runner-kit wait", len(r.Runners))
	case rec.Gates.Spec.Status != "approved":
		return fmt.Sprintf("start discovery from the brief in %s: herdr-runner-kit spawn --project %s --stdin", BriefDir, r.Name)
	}
	return fmt.Sprintf("spawn a runner for the next piece of work: herdr-runner-kit spawn --project %s --stdin", r.Name)
}

// Origin returns the URL of the repository's origin remote, or "" when it
// has none or cannot be read. Print it through RedactURL.
func Origin(ctx context.Context, dir string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
