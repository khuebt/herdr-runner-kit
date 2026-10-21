package project

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/redact"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

var decidedAt = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func specText(version int, track string) string {
	return "---\nversion: " + strconv.Itoa(version) + "\ntype: cli\ntrack: " + track + "\n---\n\n# Spec\n"
}

// commitSpec commits a spec of the given version and track, as the spec
// runner does.
func commitSpec(t *testing.T, dir string, version int, track string) {
	t.Helper()
	writeFile(t, dir, SpecPath, specText(version, track))
	run(t, dir, "add", SpecPath)
	run(t, dir, "commit", "-q", "-m", "docs(discovery): spec")
}

// commitPack commits pack version n with its HTML, and the roadmap
// proposal when roadmap is not empty.
func commitPack(t *testing.T, dir string, n int, roadmap string) {
	t.Helper()
	writeFile(t, dir, PackDir(n)+"/"+PackHTML, "<html>pack</html>\n")
	writeFile(t, dir, PackDir(n)+"/"+PackPDF, "%PDF\n")
	run(t, dir, "add", PackDir(n))
	if roadmap != "" {
		writeFile(t, dir, RoadmapProposalPath, roadmap)
		run(t, dir, "add", RoadmapProposalPath)
	}
	run(t, dir, "commit", "-q", "-m", "docs(approval): pack")
}

func decide(s *state.Store, gate, outcome string, mutate ...func(*DecideOptions)) (Decided, error) {
	o := DecideOptions{Gate: gate, Outcome: outcome, By: "Lan Pham", Now: decidedAt}
	for _, m := range mutate {
		m(&o)
	}
	return Decide(context.Background(), s, "demo-app", o)
}

func commitCount(t *testing.T, dir string) int {
	t.Helper()
	return len(strings.Split(run(t, dir, "log", "--oneline"), "\n"))
}

func load(t *testing.T, s *state.Store) Record {
	t.Helper()
	rec, err := Load(s, "demo-app")
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestGateApproveSpecNeedsCommittedSpec(t *testing.T) {
	dir := newRepo(t)
	s := registered(t, dir)
	if _, err := decide(s, GateSpec, "approve"); err == nil || !strings.Contains(err.Error(), "HEAD holds no "+SpecPath) {
		t.Fatalf("approve without a spec: %v", err)
	}
	writeFile(t, dir, SpecPath, specText(1, "standard"))
	if _, err := decide(s, GateSpec, "approve"); err == nil || !strings.Contains(err.Error(), "HEAD holds no") {
		t.Fatalf("approve an untracked spec: %v", err)
	}
	commitSpec(t, dir, 1, "standard")
	writeFile(t, dir, SpecPath, specText(2, "standard"))
	if _, err := decide(s, GateSpec, "approve"); err == nil || !strings.Contains(err.Error(), "changes not committed") {
		t.Fatalf("approve a dirty spec: %v", err)
	}
	run(t, dir, "checkout", "--", SpecPath)

	// Records made since the last commit go into the outcome's commit.
	if err := AddDecision(s, "demo-app", "use SQLite", decidedAt); err != nil {
		t.Fatal(err)
	}
	before := commitCount(t, dir)
	d, err := decide(s, GateSpec, "approve", func(o *DecideOptions) { o.Note = "looks good" })
	if err != nil {
		t.Fatal(err)
	}
	if d.Version != "v1" || d.Status != GateApproved || d.Commit == "" {
		t.Errorf("decided = %+v", d)
	}
	if n := commitCount(t, dir); n != before+1 {
		t.Errorf("%d commits for one outcome", n-before)
	}
	if msg := run(t, dir, "log", "-1", "--format=%s"); msg != "docs(project): approve spec v1" {
		t.Errorf("commit message %q", msg)
	}
	files := strings.Split(run(t, dir, "show", "--name-only", "--format=", "HEAD"), "\n")
	if !sameSet(files, RenderedPaths) {
		t.Errorf("the outcome commit holds:\n%s", files)
	}
	decisions := run(t, dir, "show", "HEAD:"+DecisionsPath)
	for _, want := range []string{"use SQLite", "spec v1 approved by Lan Pham: looks good"} {
		if !strings.Contains(decisions, want) {
			t.Errorf("decisions.md lacks %q:\n%s", want, decisions)
		}
	}
	rec := load(t, s)
	if g := rec.Gates.Spec; g.Status != GateApproved || g.Version != "v1" || g.By != "Lan Pham" || !g.At.Equal(decidedAt) {
		t.Errorf("spec gate = %+v", g)
	}
	if rec.Type != "cli" || rec.Track != "standard" || rec.Pending != 0 {
		t.Errorf("record = type %s, track %s, pending %d", rec.Type, rec.Track, rec.Pending)
	}
	if !strings.Contains(run(t, dir, "show", "HEAD:"+SnapshotPath), "- Spec gate: approved v1 by Lan Pham on 2026-10-14") {
		t.Error("project.md does not show the approval")
	}

	// A version gets one outcome; a new outcome needs a revision.
	for _, outcome := range []string{"approve", "changes"} {
		if _, err := decide(s, GateSpec, outcome, func(o *DecideOptions) { o.Note = "x" }); err == nil || !strings.Contains(err.Error(), "already has an outcome") {
			t.Errorf("%s on a decided version: %v", outcome, err)
		}
	}
	if n := commitCount(t, dir); n != before+1 {
		t.Errorf("a refused outcome committed")
	}
}

func TestGateOutcomeNeedsApproverAndReason(t *testing.T) {
	dir := newRepo(t)
	s := registered(t, dir)
	commitSpec(t, dir, 1, "standard")
	if _, err := decide(s, GateSpec, "approve", func(o *DecideOptions) { o.By = " " }); err == nil || !strings.Contains(err.Error(), "--by") {
		t.Errorf("approve without an approver: %v", err)
	}
	if _, err := decide(s, GateSpec, "reject"); err == nil || !strings.Contains(err.Error(), "--note") {
		t.Errorf("reject without a reason: %v", err)
	}
	if _, err := decide(s, "design", "approve"); err == nil {
		t.Error("decided an unknown gate")
	}
	if _, err := decide(s, GateSpec, "maybe"); err == nil {
		t.Error("recorded an unknown outcome")
	}
	if Approver(context.Background(), dir) != "t" {
		t.Errorf("approver = %q, want git's author name", Approver(context.Background(), dir))
	}
}

func TestGateVersionComesFromHEAD(t *testing.T) {
	dir := newRepo(t)
	s := registered(t, dir)
	ctx := context.Background()
	commitSpec(t, dir, 1, "standard")
	writeFile(t, dir, SpecPath, specText(5, "lite"))
	g, err := ReadGate(ctx, dir, load(t, s), GateSpec)
	if err != nil || g.Committed != 1 || g.Track != "standard" || g.Status() != GateOpen {
		t.Fatalf("gate = %+v %v", g, err)
	}
	run(t, dir, "checkout", "--", SpecPath)

	// A record that claims more than HEAD holds opens nothing.
	rec := load(t, s)
	rec.Gates.Spec = Gate{Status: GateApproved, Version: "v2", By: "x", At: decidedAt}
	if err := RequireGate(ctx, dir, rec, GateSpec); err == nil || !strings.Contains(err.Error(), "HEAD of "+dir+" holds v1") {
		t.Errorf("approved v2 with v1 committed: %v", err)
	}
	// An approval stays in force while a later version is committed.
	rec.Gates.Spec.Version = "v1"
	commitSpec(t, dir, 2, "standard")
	if err := RequireGate(ctx, dir, rec, GateSpec); err != nil {
		t.Errorf("approved v1 with v2 committed: %v", err)
	}
}

func TestGateChangesThenRevision(t *testing.T) {
	dir := newRepo(t)
	s := registered(t, dir)
	ctx := context.Background()
	ux, err := LoadStage("ux")
	if err != nil {
		t.Fatal(err)
	}
	commitSpec(t, dir, 1, "standard")
	if err := CheckRequires(ctx, dir, load(t, s), ux); err == nil || !strings.Contains(err.Error(), "the spec gate is open") {
		t.Errorf("ux before the spec outcome: %v", err)
	}
	long := strings.Repeat("too vague about exports. ", 40)
	d, err := decide(s, GateSpec, "changes", func(o *DecideOptions) { o.Note = long })
	if err != nil || d.Status != GateChangesRequested {
		t.Fatalf("changes: %+v %v", d, err)
	}
	if msg := run(t, dir, "log", "-1", "--format=%s"); msg != "docs(project): request changes to spec v1" {
		t.Errorf("commit message %q", msg)
	}
	rec := load(t, s)
	if n := len([]rune(rec.Gates.Spec.Note)); n > maxNote {
		t.Errorf("the record keeps a %d-rune note", n)
	}
	if err := CheckRequires(ctx, dir, rec, ux); err == nil || !strings.Contains(err.Error(), "changes-requested") {
		t.Errorf("ux with changes requested: %v", err)
	}
	commitSpec(t, dir, 2, "standard")
	if g, _ := ReadGate(ctx, dir, rec, GateSpec); g.Status() != GateOpen {
		t.Errorf("revised spec status = %s", g.Status())
	}
	if _, err := decide(s, GateSpec, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := CheckRequires(ctx, dir, load(t, s), ux); err != nil {
		t.Errorf("ux after approval: %v", err)
	}
	intake, err := LoadStage("intake")
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckRequires(ctx, dir, NewRecord("demo-app"), intake); err != nil {
		t.Errorf("intake needs no gate: %v", err)
	}
}

func TestGatePackWaiverOnlyOnLiteTrack(t *testing.T) {
	dir := newRepo(t)
	s := registered(t, dir)
	ctx := context.Background()
	waive := func(o *DecideOptions) { o.WaivePack = true }
	commitSpec(t, dir, 1, "standard")
	if _, err := decide(s, GateSpec, "approve", waive); err == nil || !strings.Contains(err.Error(), "only a lite-track project") {
		t.Fatalf("waiver on the standard track: %v", err)
	}
	if load(t, s).Gates.Spec.Status != "" {
		t.Error("a refused waiver recorded the approval")
	}
	for _, c := range []struct{ gate, outcome string }{{GatePack, "approve"}, {GateSpec, "changes"}} {
		if _, err := decide(s, c.gate, c.outcome, waive, func(o *DecideOptions) { o.Note = "x" }); err == nil || !strings.Contains(err.Error(), "only with approve spec") {
			t.Errorf("waiver with %s %s: %v", c.outcome, c.gate, err)
		}
	}
	commitSpec(t, dir, 2, "lite")
	d, err := decide(s, GateSpec, "approve", waive)
	if err != nil || !d.Waived {
		t.Fatalf("waiver on the lite track: %+v %v", d, err)
	}
	rec := load(t, s)
	if !rec.Gates.Pack.Waived || rec.Gates.Pack.By != "Lan Pham" || rec.Gates.Spec.Waived || rec.Track != "lite" {
		t.Errorf("gates = %+v", rec.Gates)
	}
	if !strings.Contains(run(t, dir, "show", "HEAD:"+DecisionsPath), "the pack gate is waived for the lite track by the user's choice") {
		t.Error("decisions.md does not record the waiver")
	}
	if err := RequireGate(ctx, dir, rec, GatePack); err != nil {
		t.Errorf("waived pack gate: %v", err)
	}
	// The waiver holds only while the committed spec stays on the lite track.
	commitSpec(t, dir, 3, "standard")
	if err := RequireGate(ctx, dir, rec, GatePack); err == nil || !strings.Contains(err.Error(), "standard track") {
		t.Errorf("waiver after a switch to standard: %v", err)
	}
	rec.Gates.Spec.Waived = true
	if err := rec.Validate(); err == nil {
		t.Error("a waived spec gate validated")
	}
}

const proposal = "# Roadmap proposal\n\n```yaml\n" + `milestones:
  - {id: M0, title: Scaffold, planned: 2026-11-01}
  - id: M1
    title: Lists
    features:
      - {id: F1, title: Add a list}
      - {id: F2, title: Share a list, dependsOn: [F1]}
dependencies:
  - {name: Payments sandbox, owner: vendor, milestone: M1, secretLocation: team vault}
` + "```\n"

func TestPackGateBlocksMilestonesUntilApproved(t *testing.T) {
	dir := newRepo(t)
	s := registered(t, dir)
	ctx := context.Background()
	commitSpec(t, dir, 1, "standard")
	commitPack(t, dir, 1, "")
	if _, err := decide(s, GatePack, "approve"); err == nil || !strings.Contains(err.Error(), "after the spec") {
		t.Fatalf("pack before the spec: %v", err)
	}
	if _, err := decide(s, GateSpec, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := RequireGate(ctx, dir, load(t, s), GatePack); err == nil || !strings.Contains(err.Error(), "pack gate is open") {
		t.Errorf("milestones before the pack: %v", err)
	}
	if _, err := decide(s, GatePack, "approve"); err == nil || !strings.Contains(err.Error(), "HEAD holds no "+RoadmapProposalPath) {
		t.Fatalf("pack without a roadmap: %v", err)
	}
	d, err := decide(s, GatePack, "changes", func(o *DecideOptions) { o.Note = "add a timeline" })
	if err != nil || d.Version != "v1" {
		t.Fatalf("changes to the pack: %+v %v", d, err)
	}
	writeFile(t, dir, PackDir(2)+"/"+PackHTML, "draft\n")
	if _, err := decide(s, GatePack, "approve"); err == nil || !strings.Contains(err.Error(), "already has an outcome") {
		t.Fatalf("approve with only an uncommitted v2: %v", err)
	}
	if _, err := Update(s, "demo-app", func(r *Record) error {
		r.Reapproval = Reapproval{Needed: true, Reason: "CR-1"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	commitPack(t, dir, 2, proposal)
	writeFile(t, dir, PackDir(2)+"/notes.txt", "x")
	if _, err := decide(s, GatePack, "approve"); err == nil || !strings.Contains(err.Error(), "changes not committed") {
		t.Fatalf("approve a dirty pack: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, PackDir(2), "notes.txt")); err != nil {
		t.Fatal(err)
	}
	d, err = decide(s, GatePack, "approve")
	if err != nil {
		t.Fatal(err)
	}
	if d.Version != "v2" || d.Milestones != 2 || d.Dependencies != 1 || d.NoPDF {
		t.Errorf("decided = %+v", d)
	}
	rec := load(t, s)
	if rec.Reapproval.Needed || len(rec.Milestones) != 2 || rec.Milestones[0].Status != MilestonePlanned ||
		!rec.Milestones[0].Planned.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) ||
		len(rec.Dependencies) != 1 || rec.Dependencies[0].ID != "D1" || rec.Dependencies[0].Status != "needed" {
		t.Errorf("record = %+v", rec)
	}
	roadmap := run(t, dir, "show", "HEAD:"+RoadmapPath)
	if !strings.Contains(roadmap, "## M1 Lists") || !strings.Contains(roadmap, "- F2 Share a list (after F1)") {
		t.Errorf("roadmap.md:\n%s", roadmap)
	}
	if msg := run(t, dir, "log", "-1", "--format=%s"); msg != "docs(project): approve pack v2" {
		t.Errorf("commit message %q", msg)
	}
	if err := RequireGate(ctx, dir, rec, GatePack); err != nil {
		t.Errorf("milestones after the pack: %v", err)
	}
}

func TestLaterPackMayOmitDoneMilestones(t *testing.T) {
	s, dir := approvedProject(t)
	if _, err := Update(s, "demo-app", func(r *Record) error {
		r.Milestones[0].Status = MilestoneDone
		r.Milestones[1].Status = MilestoneDone
		r.Reapproval = Reapproval{Needed: true, Reason: "CR-1"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	done := load(t, s).Milestones
	changed := strings.Replace(proposal, "      - {id: F1, title: Add a list}\n", "      - {id: F1, title: Add a todo list}\n", 1)
	commitPack(t, dir, 2, changed)
	if _, err := decide(s, GatePack, "approve"); err == nil || !strings.Contains(err.Error(), "M1 is done and cannot be changed") {
		t.Fatalf("a changed done milestone: %v", err)
	}
	commitPack(t, dir, 3, "# Roadmap proposal\n\n```yaml\nmilestones:\n  - id: M2\n    title: Share\n    features:\n      - {id: F3, title: Share by link, dependsOn: [F2]}\n```\n")
	if _, err := decide(s, GatePack, "approve"); err != nil {
		t.Fatal(err)
	}
	rec := load(t, s)
	if len(rec.Milestones) != 3 || !reflect.DeepEqual(rec.Milestones[:2], done) || rec.Milestones[2].ID != "M2" {
		t.Errorf("milestones = %+v", rec.Milestones)
	}
}

func TestGateRefusesWhileARunnerWorksInTheCheckout(t *testing.T) {
	dir := newRepo(t)
	s := registered(t, dir)
	commitSpec(t, dir, 1, "standard")
	tk := &task.Task{ID: "t1", Title: "ux", Project: "demo-app", Status: task.Running}
	if err := s.Create(tk); err != nil {
		t.Fatal(err)
	}
	if _, err := decide(s, GateSpec, "approve"); err == nil || !strings.Contains(err.Error(), "main checkout") {
		t.Fatalf("approve while a runner works: %v", err)
	}
	if rec := load(t, s); rec.Gates.Spec.Status != "" || rec.Pending != 0 {
		t.Errorf("a refused outcome changed the record: %+v", rec.Gates.Spec)
	}
}

func TestAddDecision(t *testing.T) {
	dir := newRepo(t)
	s := registered(t, dir)
	before := commitCount(t, dir)
	if err := AddDecision(s, "demo-app", "  ", decidedAt); err == nil {
		t.Error("kept an empty decision")
	}
	if err := AddDecision(s, "demo-app", "weekly demo on Fridays", decidedAt); err != nil {
		t.Fatal(err)
	}
	rec := load(t, s)
	if len(rec.Decisions) != 1 || rec.Decisions[0].Text != "weekly demo on Fridays" || rec.Pending != 1 {
		t.Errorf("record = %+v", rec)
	}
	if commitCount(t, dir) != before {
		t.Error("a decision was committed on its own")
	}
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func TestGateWaiverRefusedOnceThePackHasAnOutcome(t *testing.T) {
	dir := newRepo(t)
	s := registered(t, dir)
	commitSpec(t, dir, 1, "lite")
	if _, err := decide(s, GateSpec, "approve"); err != nil {
		t.Fatal(err)
	}
	commitPack(t, dir, 1, proposal)
	if _, err := decide(s, GatePack, "approve"); err != nil {
		t.Fatal(err)
	}
	commitSpec(t, dir, 2, "lite")
	before := commitCount(t, dir)
	_, err := decide(s, GateSpec, "approve", func(o *DecideOptions) { o.WaivePack = true })
	if err == nil || !strings.Contains(err.Error(), "the pack gate already has an outcome (approved v1)") {
		t.Fatalf("waiver over an approved pack: %v", err)
	}
	rec := load(t, s)
	if g := rec.Gates.Pack; g.Status != GateApproved || g.Version != "v1" || g.Waived {
		t.Errorf("pack gate = %+v", g)
	}
	if rec.Gates.Spec.Version != "v1" || commitCount(t, dir) != before {
		t.Error("a refused waiver recorded the spec approval")
	}
}

func TestGateApprovalIsBoundToContent(t *testing.T) {
	dir := newRepo(t)
	s := registered(t, dir)
	ctx := context.Background()
	commitSpec(t, dir, 1, "standard")
	if _, err := decide(s, GateSpec, "approve"); err != nil {
		t.Fatal(err)
	}
	rec := load(t, s)
	if blob := run(t, dir, "rev-parse", "HEAD:"+SpecPath); rec.Gates.Spec.Content != blob {
		t.Fatalf("spec content = %q, want blob %s", rec.Gates.Spec.Content, blob)
	}
	if d := GateDrift(ctx, dir, rec); len(d) != 0 {
		t.Errorf("drift of an unchanged spec = %q", d)
	}
	// The same version with other content reopens the gate.
	writeFile(t, dir, SpecPath, specText(1, "standard")+"\nAlso export to PDF.\n")
	run(t, dir, "commit", "-q", "-am", "docs(discovery): spec")
	g, err := ReadGate(ctx, dir, rec, GateSpec)
	if err != nil || !g.Changed || g.Status() != GateChanged || g.Passed() {
		t.Fatalf("changed spec: %+v %v", g, err)
	}
	drift := GateDrift(ctx, dir, rec)
	want := "git -C " + dir + " diff " + rec.Gates.Spec.Content + " HEAD:" + SpecPath
	if len(drift) != 1 || !strings.HasPrefix(drift[0], "spec v1 changed since its approval") || !strings.HasSuffix(drift[0], want) {
		t.Errorf("drift of a changed spec = %q", drift)
	}
	if out := (Report{Record: &rec, Drift: drift}).Text(); !strings.Contains(out, "warning: spec v1 changed since its approval") {
		t.Errorf("status hides the change:\n%s", out)
	}
	if err := RequireGate(ctx, dir, rec, GateSpec); err == nil || !strings.Contains(err.Error(), "spec v1 changed since its approval") {
		t.Errorf("changed spec passed: %v", err)
	}
	d, err := decide(s, GateSpec, "approve")
	if err != nil || d.Replaced != GateApproved || d.Version != "v1" {
		t.Fatalf("approve the changed spec: %+v %v", d, err)
	}
	if !strings.Contains(run(t, dir, "show", "HEAD:"+DecisionsPath), "its content changed since it was approved, which this outcome replaces") {
		t.Error("decisions.md does not say the outcome replaced one")
	}
	if err := RequireGate(ctx, dir, load(t, s), GateSpec); err != nil {
		t.Errorf("re-approved spec: %v", err)
	}
	if _, err := decide(s, GateSpec, "approve"); err == nil || !strings.Contains(err.Error(), "already has an outcome") {
		t.Errorf("a second outcome for the same content: %v", err)
	}

	// Each pack version keeps its folder; a change to it reopens the gate.
	commitPack(t, dir, 1, proposal)
	if _, err := decide(s, GatePack, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := RequireGate(ctx, dir, load(t, s), GatePack); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, PackDir(1)+"/"+PackHTML, "<html>other</html>\n")
	run(t, dir, "commit", "-q", "-am", "docs(approval): pack")
	if err := RequireGate(ctx, dir, load(t, s), GatePack); err == nil || !strings.Contains(err.Error(), "pack v1 changed since its approval") {
		t.Errorf("changed pack passed: %v", err)
	}
	// An outcome recorded without content keeps its old meaning.
	rec = load(t, s)
	rec.Gates.Pack.Content = ""
	if err := RequireGate(ctx, dir, rec, GatePack); err != nil {
		t.Errorf("outcome without content: %v", err)
	}
}

func TestGateNoteAndDecisionsHideSecrets(t *testing.T) {
	const token = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	dir := newRepo(t)
	s := registered(t, dir)
	commitSpec(t, dir, 1, "standard")
	if err := AddDecision(s, "demo-app", "deploy with "+token, decidedAt); err != nil {
		t.Fatal(err)
	}
	c, err := AddChange(s, "demo-app", "rotate "+token, "rotate", decidedAt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.Title, token) || !strings.Contains(c.Title, redact.Mark) {
		t.Errorf("change title = %q", c.Title)
	}
	if _, err := decide(s, GateSpec, "approve", func(o *DecideOptions) { o.Note = "ok, password=hunter2" }); err != nil {
		t.Fatal(err)
	}
	if note := load(t, s).Gates.Spec.Note; note != "ok, password="+redact.Mark {
		t.Errorf("note = %q", note)
	}
	for _, rel := range RenderedPaths {
		if got := run(t, dir, "show", "HEAD:"+rel); strings.Contains(got, token) || strings.Contains(got, "hunter2") {
			t.Errorf("%s holds a secret:\n%s", rel, got)
		}
	}
}

func TestRoadmapImportKeepsManualDependenciesAndTheRunningMilestone(t *testing.T) {
	rec := NewRecord("demo-app")
	rec.Milestones = []Milestone{{ID: "M0", Title: "Scaffold", Status: MilestoneInProgress, Features: []Feature{{ID: "F0", Title: "Repo"}}}}
	rec.Dependencies = []Dependency{{ID: "D1", Name: "Payments Sandbox", Owner: "user", Status: DependencyReady, SecretLocation: "1Password: payments"}}
	b, err := ParseBlock(markdown("milestones:\n  - {id: M0, title: Scaffold v2}\n  - {id: M1, title: Lists}\n"+
		"dependencies:\n  - {name: payments sandbox, milestone: M1, workaround: fake gateway}\n"), RoadmapProposal, rec)
	if err != nil {
		t.Fatal(err)
	}
	applyRoadmap(&rec, b, "")
	if m := rec.Milestones[0]; m.Title != "Scaffold" || len(m.Features) != 1 || m.Status != MilestoneInProgress {
		t.Errorf("running milestone = %+v", m)
	}
	if len(rec.Dependencies) != 1 {
		t.Fatalf("dependencies = %+v", rec.Dependencies)
	}
	want := Dependency{ID: "D1", Name: "Payments Sandbox", Owner: "user", Status: DependencyReady, Milestone: "M1", Workaround: "fake gateway", SecretLocation: "1Password: payments"}
	if rec.Dependencies[0] != want {
		t.Errorf("dependency = %+v", rec.Dependencies[0])
	}
}
