package project

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/redact"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// Classes of a change request, as its impact runner reports them.
const (
	// ClassTweak changes no spec: copy, colour, a field order. It goes to
	// the runner building it with send.
	ClassTweak = "tweak"
	// ClassWithin changes a feature of the running milestone.
	ClassWithin = "within-milestone"
	// ClassScope adds or drops a feature or a user type, or moves the
	// approved timeline.
	ClassScope = "scope"
)

// Classes lists the classes of a change request.
var Classes = []string{ClassTweak, ClassWithin, ClassScope}

// Change request statuses. Open waits for the user's decision; the others
// are the decision.
const (
	ChangeOpen     = "open"
	ChangeNow      = "now"
	ChangeNext     = "next"
	ChangeBacklog  = "backlog"
	ChangeRejected = "rejected"
)

// ChangeOutcomes are the decisions the user takes on a change request.
var ChangeOutcomes = []string{ChangeNow, ChangeNext, ChangeBacklog, "reject"}

// GraduatedChangesDir is where change requests go once the project has
// graduated and its discovery folder is gone.
const GraduatedChangesDir = "handbook/product/changes"

// ChangesFolder returns the folder of rec's change request files.
func ChangesFolder(rec Record) string {
	if rec.Graduated != nil {
		return GraduatedChangesDir
	}
	return ChangesDir
}

// changeID matches a change request id or its bare number.
var changeID = regexp.MustCompile(`^(?i:cr-?)?([1-9][0-9]{0,5})$`)

// ParseChangeID reads "3", "CR-3" or "cr3" as CR-3.
func ParseChangeID(s string) (string, error) {
	m := changeID.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", fmt.Errorf("invalid change request %q: want its number, such as 3 or CR-3", s)
	}
	return "CR-" + m[1], nil
}

// AddChange allocates the next change request of a project titled title,
// with values that look like secrets hidden, and names its file with slug.
// The impact runner writes the file; the record keeps the request until
// the next commit point.
func AddChange(s *state.Store, name, title, slug string, now time.Time) (Change, error) {
	title = redact.Secrets(oneLine(title))
	if title == "" {
		return Change{}, errors.New("the change request is empty")
	}
	if slug == "" {
		slug = "change"
	}
	var c Change
	_, err := Update(s, name, func(rec *Record) error {
		ids := make([]string, 0, len(rec.Changes))
		for _, c := range rec.Changes {
			ids = append(ids, c.ID)
		}
		id := nextID("CR-", ids)
		c = Change{
			ID: id, Title: clip(title, maxNote), Status: ChangeOpen, At: stamp(now),
			Path: ChangesFolder(*rec) + "/" + id + "-" + slug + ".md",
		}
		rec.Changes = append(rec.Changes, c)
		return nil
	})
	return c, err
}

// SetChangeTask records the impact runner of change request id.
func SetChangeTask(s *state.Store, name, id, taskID string) error {
	_, err := update(s, name, func(rec *Record) error {
		c, err := findChange(rec, id)
		if err != nil {
			return err
		}
		c.Task = taskID
		return nil
	})
	return err
}

// DropChange removes change request id while no runner was started for
// it, so a failed start leaves no request behind.
func DropChange(s *state.Store, name, id string) error {
	_, err := update(s, name, func(rec *Record) error {
		i := slices.IndexFunc(rec.Changes, func(c Change) bool { return c.ID == id })
		if i < 0 || rec.Changes[i].Task != "" {
			return fmt.Errorf("%s cannot be dropped", id)
		}
		rec.Changes = slices.Delete(rec.Changes, i, i+1)
		rec.Pending = max(rec.Pending-1, 0)
		return nil
	})
	return err
}

func findChange(rec *Record, id string) (*Change, error) {
	for i := range rec.Changes {
		if rec.Changes[i].ID == id {
			return &rec.Changes[i], nil
		}
	}
	return nil, fmt.Errorf("project %s has no change request %s", rec.Name, id)
}

// ChangeClass returns the class a runner reported with a decision
// "class -> <class>", or "" when it reported none.
func ChangeClass(decisions []string) string {
	for _, d := range decisions {
		q, choice, ok := strings.Cut(d, "->")
		if !ok || !strings.EqualFold(strings.TrimSpace(q), "class") {
			continue
		}
		choice = strings.ToLower(strings.Trim(strings.TrimSpace(choice), ".\"'`"))
		if slices.Contains(Classes, choice) {
			return choice
		}
	}
	return ""
}

// ChangeDecision is the user's decision on a change request.
type ChangeDecision struct {
	ID string
	// Outcome is now, next, backlog or reject.
	Outcome string
	// Blocking lets a scope change decided now change the running
	// milestone.
	Blocking bool
	By, Note string
	Now      time.Time
}

// ChangeDecided is a recorded decision.
type ChangeDecided struct {
	Change Change
	// Reapproval is set when the decision asks for a new pack approval
	// before the next milestone starts.
	Reapproval bool
	// Backlog is the backlog item a backlog decision added.
	Backlog string
}

// DecideChange records the user's decision on a change request. The class
// comes from the impact runner's report. The running milestone keeps its
// scope: a scope change decided now needs Blocking while a milestone is in
// progress. A scope change decided now or next, after the pack was
// approved, sets the re-approval flag that only milestone start checks.
// Nothing is committed: the record reaches the repository at the next
// commit point.
func DecideChange(s *state.Store, name string, o ChangeDecision) (ChangeDecided, error) {
	status := o.Outcome
	if status == "reject" {
		status = ChangeRejected
	}
	if !slices.Contains([]string{ChangeNow, ChangeNext, ChangeBacklog, ChangeRejected}, status) {
		return ChangeDecided{}, fmt.Errorf("unknown decision %q: want %s", o.Outcome, strings.Join(ChangeOutcomes, ", "))
	}
	if o.Blocking && status != ChangeNow {
		return ChangeDecided{}, errors.New("--blocking goes only with now")
	}
	o.By, o.Note = oneLine(o.By), clip(oneLine(o.Note), maxNote)
	if o.By == "" {
		return ChangeDecided{}, errors.New("name who decided with --by")
	}
	if status == ChangeRejected && o.Note == "" {
		return ChangeDecided{}, errors.New("say why with --note")
	}
	if redact.Found(o.Note) {
		return ChangeDecided{}, errors.New("the note looks like it holds a secret; say where the secret is kept instead")
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	rec, err := Load(s, name)
	if err != nil {
		return ChangeDecided{}, err
	}
	c, err := findChange(&rec, o.ID)
	if err != nil {
		return ChangeDecided{}, err
	}
	class := c.Class
	if class == "" {
		if class, err = reportedClass(s, *c); err != nil {
			return ChangeDecided{}, err
		}
	}
	var d ChangeDecided
	_, err = Update(s, name, func(rec *Record) error {
		c, err := findChange(rec, o.ID)
		if err != nil {
			return err
		}
		if c.Status != ChangeOpen {
			return fmt.Errorf("%s is already decided: %s", c.ID, c.Status)
		}
		running := runningMilestone(*rec)
		if status == ChangeNow && class == ClassScope && running != nil && !o.Blocking {
			return fmt.Errorf("%s changes the scope, and %s is in progress, which keeps its scope; decide next, or decide now with --blocking when %s cannot ship without it",
				c.ID, running.ID, running.ID)
		}
		c.Class, c.Status, c.Blocking, c.Milestone = class, status, o.Blocking, ""
		switch {
		case status == ChangeNow && running != nil:
			c.Milestone = running.ID
		case status == ChangeNext:
			if m := nextMilestone(*rec); m != nil {
				c.Milestone = m.ID
			}
		}
		text := fmt.Sprintf("%s (%s) decided %s", c.ID, class, status)
		if c.Blocking {
			text += ", blocking " + orDash(c.Milestone)
		} else if c.Milestone != "" {
			text += " for " + c.Milestone
		}
		text += " by " + o.By
		if status == ChangeBacklog {
			ids := make([]string, 0, len(rec.Backlog))
			for _, it := range rec.Backlog {
				ids = append(ids, it.ID)
			}
			d.Backlog = nextID("B", ids)
			rec.Backlog = append(rec.Backlog, BacklogItem{ID: d.Backlog, Text: c.ID + ": " + c.Title, At: stamp(o.Now), Status: BacklogOpen})
			text += "; kept as " + d.Backlog
		}
		if class == ClassScope && (status == ChangeNow || status == ChangeNext) && rec.Gates.Pack.Status == GateApproved {
			d.Reapproval = true
			if rec.Reapproval.Needed && rec.Reapproval.Reason != "" {
				rec.Reapproval.Reason += ", " + c.ID
			} else {
				rec.Reapproval = Reapproval{Needed: true, Reason: "scope changed by " + c.ID}
			}
			text += "; the pack needs a new approval before the next milestone"
		}
		if o.Note != "" {
			text += ": " + o.Note
		}
		rec.Decisions = append(rec.Decisions, Decision{At: stamp(o.Now), Text: text})
		d.Change = *c
		return nil
	})
	return d, err
}

// reportedClass reads the class the impact runner of c reported.
func reportedClass(s *state.Store, c Change) (string, error) {
	if c.Task == "" {
		return "", fmt.Errorf("%s has no impact runner; start one with: herdr-runner-kit project run change --stdin", c.ID)
	}
	t, err := s.Get(c.Task)
	if err != nil {
		return "", err
	}
	if class := ChangeClass(t.Decisions); class != "" {
		return class, nil
	}
	if t.Status == task.Done {
		return "", fmt.Errorf("%s: its impact runner %s reported no class; ask it with: herdr-runner-kit send %s \"report done again with --decision 'class -> <%s>'\"",
			c.ID, t.ID, t.ID, strings.Join(Classes, "|"))
	}
	return "", fmt.Errorf("%s has no class yet: its impact runner %s is %s; wait for its report", c.ID, t.ID, t.Status)
}

// runningMilestone is the milestone in progress, or nil.
func runningMilestone(rec Record) *Milestone {
	for i := range rec.Milestones {
		if rec.Milestones[i].Status == MilestoneInProgress {
			return &rec.Milestones[i]
		}
	}
	return nil
}

// nextMilestone is the first milestone after the running one that is not
// done, or the first not started when none runs.
func nextMilestone(rec Record) *Milestone {
	start := 0
	for i, m := range rec.Milestones {
		if m.Status == MilestoneInProgress {
			start = i + 1
		}
	}
	for i := start; i < len(rec.Milestones); i++ {
		if s := rec.Milestones[i].Status; s != MilestoneDone && s != MilestoneInProgress {
			return &rec.Milestones[i]
		}
	}
	return nil
}

// CheckReapproval refuses to start a milestone while a scope change waits
// for a new pack approval. Only milestone start calls it: graduation and
// the running milestone go on.
func CheckReapproval(rec Record) error {
	if !rec.Reapproval.Needed {
		return nil
	}
	return fmt.Errorf("the pack needs a new approval before the next milestone (%s); run the pack stage and approve it: herdr-runner-kit project run pack --project %s",
		orDash(oneLine(rec.Reapproval.Reason)), rec.Name)
}

// BacklogOpen is the status of a backlog item no plan has taken yet.
const BacklogOpen = "open"

// AddBacklog keeps an idea for later as the next backlog item. No runner
// starts and nothing is committed until the next commit point.
func AddBacklog(s *state.Store, name, text string, now time.Time) (BacklogItem, error) {
	text = oneLine(text)
	if text == "" {
		return BacklogItem{}, errors.New("the backlog entry is empty")
	}
	if redact.Found(text) {
		return BacklogItem{}, errors.New("the backlog entry looks like it holds a secret; say where the secret is kept instead")
	}
	var it BacklogItem
	_, err := Update(s, name, func(rec *Record) error {
		ids := make([]string, 0, len(rec.Backlog))
		for _, b := range rec.Backlog {
			ids = append(ids, b.ID)
		}
		it = BacklogItem{ID: nextID("B", ids), Text: text, At: stamp(now), Status: BacklogOpen}
		rec.Backlog = append(rec.Backlog, it)
		return nil
	})
	return it, err
}

// Dependency owners and statuses.
var (
	DependencyOwners   = []string{"user", "vendor", "runner"}
	DependencyStatuses = []string{DependencyNeeded, "requested", DependencyReady}
)

// Dependency statuses the kit acts on.
const (
	DependencyNeeded = "needed"
	DependencyReady  = "ready"
)

// DependencyOptions describe a new dependency. SecretLocation says where a
// secret is kept, such as "1Password: payments sandbox", never its value.
type DependencyOptions struct {
	Name, Owner, Milestone, Workaround, SecretLocation string
}

// AddDependency records something outside the project the work waits on.
// A value that looks like a secret is refused.
func AddDependency(s *state.Store, name string, o DependencyOptions) (Dependency, error) {
	o.Name, o.Workaround, o.SecretLocation = oneLine(o.Name), oneLine(o.Workaround), oneLine(o.SecretLocation)
	o.Milestone = strings.TrimSpace(o.Milestone)
	if o.Name == "" {
		return Dependency{}, errors.New("name the dependency")
	}
	if !slices.Contains(DependencyOwners, o.Owner) {
		return Dependency{}, fmt.Errorf("--owner %q: want one of %s", o.Owner, strings.Join(DependencyOwners, ", "))
	}
	if err := checkNoSecrets(map[string]string{"the name": o.Name, "--workaround": o.Workaround, "--secret-location": o.SecretLocation}); err != nil {
		return Dependency{}, err
	}
	var d Dependency
	_, err := Update(s, name, func(rec *Record) error {
		if i := slices.IndexFunc(rec.Dependencies, func(d Dependency) bool { return strings.EqualFold(d.Name, o.Name) }); i >= 0 {
			return fmt.Errorf("%s already tracks %q; change its status with: herdr-runner-kit project dep set %s --status STATUS",
				rec.Dependencies[i].ID, rec.Dependencies[i].Name, rec.Dependencies[i].ID)
		}
		if o.Milestone != "" && len(rec.Milestones) > 0 && !slices.ContainsFunc(rec.Milestones, func(m Milestone) bool { return m.ID == o.Milestone }) {
			return fmt.Errorf("--milestone %s is not a milestone of %s", o.Milestone, rec.Name)
		}
		d = Dependency{
			ID: nextID("D", depIDs(rec.Dependencies)), Name: o.Name, Owner: o.Owner, Status: DependencyNeeded,
			Milestone: o.Milestone, Workaround: o.Workaround, SecretLocation: o.SecretLocation,
		}
		rec.Dependencies = append(rec.Dependencies, d)
		return nil
	})
	return d, err
}

// checkNoSecrets refuses fields whose value looks like a secret. The error
// names the field, never the value.
func checkNoSecrets(fields map[string]string) error {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if redact.Found(fields[name]) {
			return fmt.Errorf("%s looks like it holds a secret; record only where the secret is kept, such as \"1Password: payments sandbox\"", name)
		}
	}
	return nil
}

// dependencyID matches a dependency id or its bare number.
var dependencyID = regexp.MustCompile(`^(?i:d)?([1-9][0-9]{0,5})$`)

// ParseDependencyID reads "2" or "D2" as D2.
func ParseDependencyID(s string) (string, error) {
	m := dependencyID.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", fmt.Errorf("invalid dependency %q: want its id, such as D2", s)
	}
	return "D" + m[1], nil
}

// SetDependencyStatus changes the status of dependency id.
func SetDependencyStatus(s *state.Store, name, id, status string) (Dependency, error) {
	if !slices.Contains(DependencyStatuses, status) {
		return Dependency{}, fmt.Errorf("--status %q: want one of %s", status, strings.Join(DependencyStatuses, ", "))
	}
	var d Dependency
	_, err := Update(s, name, func(rec *Record) error {
		i := slices.IndexFunc(rec.Dependencies, func(d Dependency) bool { return d.ID == id })
		if i < 0 {
			return fmt.Errorf("project %s has no dependency %s", rec.Name, id)
		}
		if rec.Dependencies[i].Status == status {
			return fmt.Errorf("%s is already %s", id, status)
		}
		rec.Dependencies[i].Status = status
		d = rec.Dependencies[i]
		return nil
	})
	return d, err
}

// HeldFor returns the held tasks of project whose hold reason names
// dependency id.
func HeldFor(tasks []*task.Task, project, id string) []*task.Task {
	word := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(id) + `\b`)
	var held []*task.Task
	for _, t := range tasks {
		if t.Project == project && t.Status == task.Held && word.MatchString(t.HoldReason) {
			held = append(held, t)
		}
	}
	return held
}

// ActiveTasks lists the unfinished tasks of project, one per line, as the
// impact runner of a change request sees them.
func ActiveTasks(tasks []*task.Task, project string) string {
	var lines []string
	for _, t := range tasks {
		if t.Project != project || t.Status.Terminal() {
			continue
		}
		parts := []string{t.ID + " " + string(t.Status)}
		if t.Stage != "" {
			parts = append(parts, "stage "+t.Stage)
		}
		if t.Worktree != nil && t.Worktree.Branch != "" {
			parts = append(parts, "branch "+t.Worktree.Branch)
		}
		parts = append(parts, oneLine(t.Title))
		lines = append(lines, "- "+strings.Join(parts, " · "))
	}
	if len(lines) == 0 {
		return "none"
	}
	return strings.Join(lines, "\n")
}
