package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

var finishedAt = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

func TestTagName(t *testing.T) {
	for _, c := range []struct {
		format, id string
		index      int
		want       string
	}{
		{"", "M0", 0, "v0.0.0"},
		{DefaultTagFormat, "M3", 1, "v0.3.0"},
		{"release-{N}", "M12", 0, "release-12"},
		{DefaultTagFormat, "beta", 4, "v0.4.0"},
	} {
		got, err := TagName(c.format, c.id, c.index)
		if err != nil || got != c.want {
			t.Errorf("TagName(%q, %q, %d) = %q, %v; want %q", c.format, c.id, c.index, got, err, c.want)
		}
	}
	for _, bad := range []string{"v..{N}", "-v{N}", "v{N} final"} {
		if _, err := TagName(bad, "M1", 1); err == nil || !strings.Contains(err.Error(), "not a valid tag name") {
			t.Errorf("format %q: %v", bad, err)
		}
	}
}

// closingProject returns a project whose M0 is in progress with a done
// scaffold runner and a done close runner, whose release branch release/M0
// holds the release notes and, when roadmap is set, a roadmap update.
func closingProject(t *testing.T, roadmap string) (*state.Store, state.Project, []*task.Task) {
	t.Helper()
	s, p := packApprovedProject(t)
	if _, _, err := start(s, p, "M0"); err != nil {
		t.Fatal(err)
	}
	created := true
	scaffold := &task.Task{
		ID: "t1", Project: "demo-app", Stage: "M0/scaffold", Status: task.Done,
		Worktree: &task.Worktree{Branch: "feat/M0-scaffold", BranchCreated: &created},
	}
	closer := &task.Task{
		ID: "t2", Project: "demo-app", Stage: "M0/close", Status: task.Done, After: []string{"t1"},
		Worktree: &task.Worktree{Branch: "release/M0", Base: "feat/M0-scaffold", BranchCreated: &created},
	}
	run(t, p.Path, "checkout", "-q", "-b", "feat/M0-scaffold")
	writeFile(t, p.Path, "app.txt", "app\n")
	run(t, p.Path, "add", "app.txt")
	run(t, p.Path, "commit", "-q", "-m", "chore(scaffold): M0")
	run(t, p.Path, "checkout", "-q", "-b", "release/M0")
	writeFile(t, p.Path, ReleaseNotesPath("M0"), "# M0 release notes\n")
	run(t, p.Path, "add", ReleaseNotesPath("M0"))
	if roadmap != "" {
		writeFile(t, p.Path, RoadmapUpdatePath("M0"), string(markdown(roadmap)))
		run(t, p.Path, "add", RoadmapUpdatePath("M0"))
	}
	run(t, p.Path, "commit", "-q", "-m", "docs(release): M0 release notes")
	run(t, p.Path, "checkout", "-q", "main")
	return s, p, []*task.Task{scaffold, closer}
}

func finish(s *state.Store, p state.Project, tasks []*task.Task, merged string) (Finished, error) {
	return FinishMilestone(context.Background(), s, p, "M0", tasks, FinishOptions{MergedCommit: merged, Now: finishedAt})
}

func tagExists(t *testing.T, dir, tag string) bool {
	t.Helper()
	return run(t, dir, "tag", "-l", tag) == tag
}

func TestDoneRefusesUntilMergedAndClean(t *testing.T) {
	s, p, tasks := closingProject(t, "")
	if _, err := finish(s, p, tasks, ""); err == nil || !strings.Contains(err.Error(), "release/M0 is not merged into main: merge release/M0 through your pull request, then pull") {
		t.Fatalf("unmerged: %v", err)
	}
	if _, err := finish(s, p, tasks[:1], ""); err == nil || !strings.Contains(err.Error(), "no close runner") {
		t.Fatalf("no close runner: %v", err)
	}
	running := *tasks[1]
	running.Status = task.Running
	if _, err := finish(s, p, []*task.Task{tasks[0], &running}, ""); err == nil || !strings.Contains(err.Error(), "is running, not done") {
		t.Fatalf("close running: %v", err)
	}
	run(t, p.Path, "merge", "-q", "--no-ff", "--no-edit", "release/M0")

	run(t, p.Path, "checkout", "-q", "feat/M0-scaffold")
	if _, err := finish(s, p, tasks, ""); err == nil || !strings.Contains(err.Error(), "is not on main") {
		t.Fatalf("on another branch: %v", err)
	}
	run(t, p.Path, "checkout", "-q", "main")

	writeFile(t, p.Path, RoadmapPath, "edited by hand\n")
	if _, err := finish(s, p, tasks, ""); err == nil || !strings.Contains(err.Error(), "files the kit renders have changes not committed: "+RoadmapPath) {
		t.Fatalf("dirty rendered file: %v", err)
	}
	run(t, p.Path, "checkout", "--", RoadmapPath)

	mergeHead := run(t, p.Path, "rev-parse", "--path-format=absolute", "--git-path", "MERGE_HEAD")
	writeFile(t, filepath.Dir(mergeHead), filepath.Base(mergeHead), run(t, p.Path, "rev-parse", "HEAD")+"\n")
	if _, err := finish(s, p, tasks, ""); err == nil || !strings.Contains(err.Error(), "a merge in progress") {
		t.Fatalf("MERGE_HEAD: %v", err)
	}
	if err := os.Remove(mergeHead); err != nil {
		t.Fatal(err)
	}
	if tagExists(t, p.Path, "v0.0.0") || load(t, s).Milestones[0].Status != MilestoneInProgress {
		t.Fatal("a refused done tagged or recorded the milestone")
	}
	if _, err := finish(s, p, tasks, ""); err != nil {
		t.Fatal(err)
	}
}

func TestDoneCommitsRecordsThenTags(t *testing.T) {
	update := roadmapYAML[:strings.Index(roadmapYAML, "dependencies:")] + "  - {id: M2, title: Reports, planned: 2026-12-01}\n"
	s, p, tasks := closingProject(t, update)
	run(t, p.Path, "merge", "-q", "--no-ff", "--no-edit", "release/M0")
	commits := commitCount(t, p.Path)

	// A refused records commit leaves no tag, the milestone in progress
	// and the rendered files as they were.
	hooks := t.TempDir()
	writeFile(t, hooks, "pre-commit", "#!/bin/sh\necho blocked by hook >&2\nexit 1\n")
	if err := os.Chmod(filepath.Join(hooks, "pre-commit"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, p.Path, "config", "core.hooksPath", hooks)
	pending := load(t, s).Pending
	if _, err := finish(s, p, tasks, ""); err == nil || !strings.Contains(err.Error(), "M0 stays in progress and is not tagged") ||
		!strings.Contains(err.Error(), "blocked by hook") {
		t.Fatalf("hook refused the commit: %v", err)
	}
	rec := load(t, s)
	if tagExists(t, p.Path, "v0.0.0") || rec.Milestones[0].Status != MilestoneInProgress || len(rec.Milestones) != 2 || rec.Pending != pending {
		t.Fatalf("after a failed commit: tag %v, record %+v", tagExists(t, p.Path, "v0.0.0"), rec.Milestones)
	}
	if st := run(t, p.Path, "status", "--porcelain"); st != "" || commitCount(t, p.Path) != commits {
		t.Fatalf("a failed commit left changes:\n%s", st)
	}
	run(t, p.Path, "config", "--unset", "core.hooksPath")

	d, err := finish(s, p, tasks, "")
	if err != nil {
		t.Fatal(err)
	}
	head := run(t, p.Path, "rev-parse", "HEAD")
	if d.Tag != "v0.0.0" || d.Commit != head || d.Branch != "release/M0" || d.DefaultBranch != "main" || d.Roadmap != 3 || d.Fetch != "" {
		t.Errorf("done = %+v", d)
	}
	if got := run(t, p.Path, "rev-parse", "v0.0.0^{commit}"); got != head {
		t.Errorf("tag on %s, want the records commit %s", got, head)
	}
	if typ := run(t, p.Path, "cat-file", "-t", "v0.0.0"); typ != "tag" {
		t.Errorf("tag is %s, want an annotated tag", typ)
	}
	if msg := run(t, p.Path, "log", "-1", "--format=%s"); msg != "docs(project): M0 done" {
		t.Errorf("commit = %q", msg)
	}
	rec = load(t, s)
	m := rec.Milestones[0]
	if m.Status != MilestoneDone || m.Tag != "v0.0.0" || m.Branch != "release/M0" || !m.Closed.Equal(finishedAt) || rec.Pending != 0 {
		t.Errorf("record = %+v pending %d", m, rec.Pending)
	}
	if len(rec.Milestones) != 3 || rec.Milestones[2].ID != "M2" {
		t.Errorf("roadmap update not imported: %+v", rec.Milestones)
	}
	roadmap := run(t, p.Path, "show", "HEAD:"+RoadmapPath)
	for _, want := range []string{"## M2 Reports", "- Tag: v0.0.0", "- Branch: release/M0"} {
		if !strings.Contains(roadmap, want) {
			t.Errorf("roadmap lacks %q:\n%s", want, roadmap)
		}
	}
	if !strings.Contains(rec.Decisions[len(rec.Decisions)-1].Text, "M0 done: release/M0 is merged into main; tagged v0.0.0 locally") {
		t.Errorf("decisions = %+v", rec.Decisions)
	}
	if _, err := finish(s, p, tasks, ""); err == nil || !strings.Contains(err.Error(), "done already, tagged v0.0.0") {
		t.Fatalf("done twice: %v", err)
	}
}

func TestDoneAfterASquashMerge(t *testing.T) {
	s, p, tasks := closingProject(t, "")
	if err := s.Create(tasks[1]); err != nil {
		t.Fatal(err)
	}
	run(t, p.Path, "merge", "-q", "--squash", "release/M0")
	run(t, p.Path, "commit", "-q", "-m", "M0 (#1)")
	if _, err := finish(s, p, tasks, ""); err == nil || !strings.Contains(err.Error(), "--merged-commit") {
		t.Fatalf("squash without the commit: %v", err)
	}
	if _, err := finish(s, p, tasks, "HEAD"); err == nil || !strings.Contains(err.Error(), "is not a commit id") {
		t.Fatalf("a ref as merged commit: %v", err)
	}
	if _, err := finish(s, p, tasks, run(t, p.Path, "rev-parse", "HEAD~1")); err == nil || !strings.Contains(err.Error(), "holds no "+ReleaseNotesPath("M0")) {
		t.Fatalf("a commit without the notes: %v", err)
	}
	if _, err := finish(s, p, tasks, run(t, p.Path, "rev-parse", "release/M0")); err == nil || !strings.Contains(err.Error(), "is not on main") {
		t.Fatalf("a commit off main: %v", err)
	}
	if _, err := finish(s, p, tasks, run(t, p.Path, "rev-parse", "HEAD")); err != nil {
		t.Fatal(err)
	}
	if !tagExists(t, p.Path, "v0.0.0") {
		t.Error("no tag")
	}
	// cleanup counts the branches stacked under the release as merged.
	tip := run(t, p.Path, "rev-parse", "release/M0")
	if got, err := s.Get(tasks[1].ID); err != nil || got.Worktree.MergedTip != tip {
		t.Errorf("merged tip = %+v, %v", got.Worktree, err)
	}
	if ref := run(t, p.Path, "rev-parse", task.MergedTipRef(tasks[1].ID)); ref != tip {
		t.Errorf("merged tip ref = %s, want %s", ref, tip)
	}
}

func TestDoneRecordsNoTipWithWorkTheSquashLacks(t *testing.T) {
	s, p, tasks := closingProject(t, "")
	if err := s.Create(tasks[1]); err != nil {
		t.Fatal(err)
	}
	run(t, p.Path, "merge", "-q", "--squash", "release/M0")
	run(t, p.Path, "commit", "-q", "-m", "M0 (#1)")
	squash := run(t, p.Path, "rev-parse", "HEAD")
	// A fix committed on the release branch after its pull request merged.
	run(t, p.Path, "checkout", "-q", "release/M0")
	writeFile(t, p.Path, "late.txt", "late\n")
	run(t, p.Path, "add", "late.txt")
	run(t, p.Path, "commit", "-q", "-m", "fix: late")
	run(t, p.Path, "checkout", "-q", "main")
	if _, err := finish(s, p, tasks, squash); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(tasks[1].ID); err != nil || got.Worktree.MergedTip != "" {
		t.Errorf("merged tip = %+v, %v", got.Worktree, err)
	}
}

func TestDoneAfterTheReleaseBranchIsDeleted(t *testing.T) {
	s, p, tasks := closingProject(t, "")
	run(t, p.Path, "merge", "-q", "--squash", "release/M0")
	run(t, p.Path, "commit", "-q", "-m", "M0 (#1)")
	merged := run(t, p.Path, "rev-parse", "HEAD")
	writeFile(t, p.Path, "later.txt", "later\n")
	run(t, p.Path, "add", "later.txt")
	run(t, p.Path, "commit", "-q", "-m", "later work")
	run(t, p.Path, "branch", "-D", "release/M0")
	d, err := finish(s, p, tasks, "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Merged != merged || !tagExists(t, p.Path, "v0.0.0") {
		t.Errorf("done = %+v, want merged %s", d, merged)
	}
}

// releaseMilestone commits the release notes and roadmap update of
// milestone id on release/<id> from main and merges it into main.
func releaseMilestone(t *testing.T, dir, id, update string) {
	t.Helper()
	run(t, dir, "checkout", "-q", "-b", ReleaseBranch(id), "main")
	writeFile(t, dir, ReleaseNotesPath(id), "# "+id+" release notes\n")
	writeFile(t, dir, RoadmapUpdatePath(id), string(markdown(update)))
	run(t, dir, "add", ReleaseNotesPath(id), RoadmapUpdatePath(id))
	run(t, dir, "commit", "-q", "-m", "docs(release): "+id+" release notes")
	run(t, dir, "checkout", "-q", "main")
	run(t, dir, "merge", "-q", "--no-ff", "--no-edit", ReleaseBranch(id))
}

// movedUpdate is a roadmap update in which M0 delivered F1 only and F2 to
// F4 moved to M1.
const movedUpdate = `milestones:
  - id: M0
    title: Scaffold
    planned: 2026-11-01
    features:
      - {id: F1, title: Sign in}
  - id: M1
    title: Lists
    planned: 2026-11-15
    features:
      - {id: F2, title: Add a list}
      - {id: F3, title: Share a list}
      - {id: F4, title: Export a list}
`

var m0Features = []Feature{{ID: "F1", Title: "Sign in"}, {ID: "F2", Title: "Add a list"}, {ID: "F3", Title: "Share a list"}, {ID: "F4", Title: "Export a list"}}

// finishM1 starts M1 by hand, releases it with update and runs done.
func finishM1(t *testing.T, s *state.Store, p state.Project, tasks []*task.Task, update string) {
	t.Helper()
	if _, err := Update(s, "demo-app", func(r *Record) error {
		r.Milestones[1].Status = MilestoneInProgress
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	created := true
	tasks = append(tasks, &task.Task{
		ID: "t3", Project: "demo-app", Stage: "M1/close", Status: task.Done,
		Worktree: &task.Worktree{Branch: "release/M1", BranchCreated: &created},
	})
	run(t, p.Path, "add", "-A")
	run(t, p.Path, "commit", "-q", "--allow-empty", "-m", "docs(project): M1 started")
	releaseMilestone(t, p.Path, "M1", update)
	if _, err := FinishMilestone(context.Background(), s, p, "M1", tasks, FinishOptions{Now: finishedAt}); err != nil {
		t.Fatal(err)
	}
	rec := load(t, s)
	if rec.Milestones[0].ID != "M0" || rec.Milestones[1].Status != MilestoneDone || !sameFeatures(rec.Milestones[0].Features, m0Features[:1]) {
		t.Errorf("after done M1: %+v", rec.Milestones)
	}
}

func TestDoneTakesTheClosingMilestonesFeaturesFromItsUpdate(t *testing.T) {
	s, p, tasks := closingProject(t, "")
	if _, err := Update(s, "demo-app", func(r *Record) error {
		r.Milestones[0].Features = m0Features
		r.Milestones[1].Features = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	run(t, p.Path, "branch", "-D", "release/M0")
	// M0 delivered F1, moved F2 and F3 to M1 and dropped F4; its update
	// gives no planned date, so the recorded one stays.
	update := strings.Replace(movedUpdate, "    planned: 2026-11-01\n", "", 1)
	update = strings.Replace(update, "      - {id: F4, title: Export a list}\n", "", 1)
	releaseMilestone(t, p.Path, "M0", update)
	if _, err := finish(s, p, tasks, ""); err != nil {
		t.Fatal(err)
	}
	rec := load(t, s)
	if !sameFeatures(rec.Milestones[0].Features, m0Features[:1]) || !sameFeatures(rec.Milestones[1].Features, m0Features[1:3]) ||
		dateOrEmpty(rec.Milestones[0].Planned) != "2026-11-01" {
		t.Fatalf("after done M0: %+v", rec.Milestones)
	}
	// M1's update leaves out the done M0, which stays as recorded.
	finishM1(t, s, p, tasks, "milestones:\n"+movedUpdate[strings.Index(movedUpdate, "  - id: M1"):])
}

func TestDoneHealsADoneMilestoneThatKeptMovedFeatures(t *testing.T) {
	s, p, tasks := closingProject(t, "")
	run(t, p.Path, "merge", "-q", "--no-ff", "--no-edit", "release/M0")
	if _, err := finish(s, p, tasks, ""); err != nil {
		t.Fatal(err)
	}
	// The state an older kit left: M0 done with every feature, M1 holding
	// the moved ones too, and a decision written before notes were bounded.
	old := load(t, s)
	old.Milestones[0].Features = m0Features
	old.Milestones[1].Features = m0Features[1:]
	old.Decisions = append(old.Decisions, Decision{At: finishedAt, Text: strings.Repeat("x", maxNote+10)})
	if err := write(s, old); err != nil {
		t.Fatal(err)
	}
	rec := load(t, s)
	if !sameFeatures(rec.Milestones[0].Features, m0Features[:1]) || rec.clipped != 1 || rec.Pending != old.Pending+1 {
		t.Fatalf("healed = %+v pending %d", rec.Milestones[0], rec.Pending)
	}
	finishM1(t, s, p, tasks, movedUpdate)
}

// withOrigin adds a bare origin holding main and the release branch, and
// returns a second clone of it.
func withOrigin(t *testing.T, dir string) (origin, other string) {
	t.Helper()
	origin = filepath.Join(t.TempDir(), "origin.git")
	run(t, dir, "init", "-q", "--bare", origin)
	run(t, dir, "remote", "add", "origin", origin)
	run(t, dir, "push", "-q", "origin", "main", "release/M0")
	other = filepath.Join(t.TempDir(), "other")
	run(t, dir, "clone", "-q", "-b", "main", origin, other)
	return origin, other
}

func TestDoneWithOrigin(t *testing.T) {
	s, p, tasks := closingProject(t, "")
	origin, other := withOrigin(t, p.Path)

	// Merged on origin only: pull first.
	run(t, other, "merge", "-q", "--no-ff", "--no-edit", "origin/release/M0")
	run(t, other, "push", "-q", "origin", "main")
	if _, err := finish(s, p, tasks, ""); err == nil || !strings.Contains(err.Error(), "merged into origin/main but not into your local main: pull main") {
		t.Fatalf("merged on origin only: %v", err)
	}
	run(t, p.Path, "merge", "-q", "--ff-only", "origin/main")

	// Behind origin: pull first.
	writeFile(t, other, "more.txt", "more\n")
	run(t, other, "add", "more.txt")
	run(t, other, "commit", "-q", "-m", "more")
	run(t, other, "push", "-q", "origin", "main")
	if _, err := finish(s, p, tasks, ""); err == nil || !strings.Contains(err.Error(), "your main is behind origin/main") {
		t.Fatalf("behind origin: %v", err)
	}
	run(t, p.Path, "merge", "-q", "--ff-only", "origin/main")

	if _, err := finish(s, p, tasks, ""); err != nil {
		t.Fatal(err)
	}
	if !tagExists(t, p.Path, "v0.0.0") {
		t.Error("no local tag")
	}
	if tags := run(t, p.Path, "ls-remote", "--tags", origin); tags != "" {
		t.Errorf("origin got tags: %s", tags)
	}

	// A tag pushed to origin is not taken back by reopen.
	run(t, p.Path, "push", "-q", "origin", "v0.0.0")
	if _, err := ReopenMilestone(context.Background(), s, p, "M0", "export drops a row", finishedAt); err == nil ||
		!strings.Contains(err.Error(), "tag v0.0.0 is on origin") {
		t.Fatalf("reopen a pushed tag: %v", err)
	}
	if !tagExists(t, p.Path, "v0.0.0") || load(t, s).Milestones[0].Status != MilestoneDone {
		t.Error("a refused reopen changed the tag or the record")
	}
}

func TestReopenThenCloseAgain(t *testing.T) {
	s, p, tasks := closingProject(t, "")
	ctx := context.Background()
	exists := func(b string) string { return BranchTaken(ctx, p.Path, b) }
	if _, err := ReopenMilestone(ctx, s, p, "M0", "export drops a row", finishedAt); err == nil || !strings.Contains(err.Error(), "only a done milestone is reopened") {
		t.Fatalf("reopen in progress: %v", err)
	}
	if _, _, err := Reclose(load(t, s), "demo-app", "M0", tasks, exists); err == nil || !strings.Contains(err.Error(), "is done; merge release/M0 through your pull request") {
		t.Fatalf("close before a reopen: %v", err)
	}
	run(t, p.Path, "merge", "-q", "--no-ff", "--no-edit", "release/M0")
	if _, err := finish(s, p, tasks, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ReopenMilestone(ctx, s, p, "M0", "  ", finishedAt); err == nil || !strings.Contains(err.Error(), "--reason") {
		t.Fatalf("reopen without a reason: %v", err)
	}
	re, err := ReopenMilestone(ctx, s, p, "M0", "export drops a row", finishedAt)
	if err != nil {
		t.Fatal(err)
	}
	rec := load(t, s)
	m := rec.Milestones[0]
	if !re.Deleted || re.Tag != "v0.0.0" || tagExists(t, p.Path, "v0.0.0") {
		t.Errorf("reopen = %+v, tag left: %v", re, tagExists(t, p.Path, "v0.0.0"))
	}
	if m.Status != MilestoneInProgress || m.Tag != "" || !m.Closed.IsZero() || m.Branch != "release/M0" || rec.Pending != 1 {
		t.Errorf("reopened = %+v pending %d", m, rec.Pending)
	}
	if got := rec.Decisions[len(rec.Decisions)-1].Text; got != "M0 reopened: export drops a row; local tag v0.0.0 deleted" {
		t.Errorf("decision = %q", got)
	}
	if !CloseStale(m, tasks[1]) {
		t.Error("the merged close runner is not stale")
	}
	if _, err := finish(s, p, tasks, ""); err == nil || !strings.Contains(err.Error(), "milestone close M0") {
		t.Fatalf("done with a stale close: %v", err)
	}

	// A fix on the scaffold's branch: the new close runner starts there.
	created := true
	fix := &task.Task{
		ID: "t3", Project: "demo-app", Stage: "M0/scaffold", Status: task.Done,
		Worktree: &task.Worktree{Branch: "feat/M0-scaffold-r1", BranchCreated: &created},
	}
	tasks = append(tasks, fix)
	step, parent, err := Reclose(rec, "demo-app", "M0", tasks, exists)
	if err != nil {
		t.Fatal(err)
	}
	if parent != fix || step.Stage != "M0/close" || step.Branch != "release/M0-r1" || step.Base != "feat/M0-scaffold-r1" || NodeTemplate(step.Node) != "close" {
		t.Errorf("close again = %+v after %v", step, parent.ID)
	}

	// The new close runner is not stale; once it fails, close runs again.
	again := &task.Task{
		ID: "t4", Project: "demo-app", Stage: "M0/close", Status: task.Running, After: []string{"t3"},
		Worktree: &task.Worktree{Branch: "release/M0-r1", BranchCreated: &created},
	}
	tasks = append(tasks, again)
	if _, _, err := Reclose(rec, "demo-app", "M0", tasks, exists); err == nil || !strings.Contains(err.Error(), "t4 of M0/close is running") {
		t.Fatalf("close while one runs: %v", err)
	}
	again.Status = task.Done
	if CloseStale(m, again) {
		t.Error("the new close runner is stale")
	}
	if _, _, err := Reclose(rec, "demo-app", "M0", tasks, exists); err == nil || !strings.Contains(err.Error(), "is done; merge release/M0-r1") {
		t.Fatalf("close after a new release: %v", err)
	}
	again.Status = task.Failed
	run(t, p.Path, "branch", "release/M0-r1")
	if step, _, err := Reclose(rec, "demo-app", "M0", tasks, exists); err != nil || step.Branch != "release/M0-r2" {
		t.Fatalf("close after a failed close: %+v %v", step, err)
	}
}

func TestReopenRefusesWhileLaterWorkMoved(t *testing.T) {
	s, p := packApprovedProject(t)
	setMilestone(t, s, "M0", MilestoneDone)
	setMilestone(t, s, "M1", MilestoneInProgress)
	if _, err := ReopenMilestone(context.Background(), s, p, "M0", "bug", finishedAt); err == nil || !strings.Contains(err.Error(), "M1 is in progress") {
		t.Fatalf("reopen while M1 runs: %v", err)
	}
	setMilestone(t, s, "M1", MilestoneDone)
	if _, err := ReopenMilestone(context.Background(), s, p, "M0", "bug", finishedAt); err == nil || !strings.Contains(err.Error(), "M1 after M0 is done") {
		t.Fatalf("reopen under a done M1: %v", err)
	}
}

func TestParallelNote(t *testing.T) {
	rec := NewRecord("demo-app")
	var tasks []*task.Task
	add := func(project string, statuses ...task.Status) {
		for _, st := range statuses {
			tasks = append(tasks, &task.Task{ID: "t" + string(rune('a'+len(tasks))), Project: project, Status: st})
		}
	}
	add("demo-app", task.Starting, task.Running, task.NeedsHuman, task.Queued, task.Queued, task.Held, task.Done, task.Failed)
	add("other", task.Running, task.Running)
	if n := ActiveRunners(tasks, "demo-app"); n != 3 {
		t.Errorf("active = %d, want 3", n)
	}
	if note := ParallelNote(rec, tasks); note != "" {
		t.Errorf("three active warned: %q", note)
	}
	add("demo-app", task.Running)
	if note := ParallelNote(rec, tasks); note != "note: 4 runners of demo-app are active (warning above 3); nothing was blocked" {
		t.Errorf("note = %q", note)
	}
	rec.ParallelWarning = 0
	if note := ParallelNote(rec, tasks); note != "" {
		t.Errorf("warning off, note = %q", note)
	}
}

// After a reopen, the release merged before it still sits on main: done
// takes only the new release's notes, and imports a roadmap update only
// when it changed.
func TestDoneAfterReopenNeedsTheNewRelease(t *testing.T) {
	update := roadmapYAML[:strings.Index(roadmapYAML, "dependencies:")] + "  - {id: M2, title: Reports, planned: 2026-12-01}\n"
	s, p, tasks := closingProject(t, update)
	ctx := context.Background()
	run(t, p.Path, "merge", "-q", "--no-ff", "--no-edit", "release/M0")
	if _, err := finish(s, p, tasks, ""); err != nil {
		t.Fatal(err)
	}
	first := load(t, s).Milestones[0]
	if len(first.Released) != 1 || first.RoadmapUpdate == "" {
		t.Fatalf("done recorded %+v", first)
	}
	if _, err := ReopenMilestone(ctx, s, p, "M0", "export drops a row", finishedAt); err != nil {
		t.Fatal(err)
	}
	// A later decision moved M2; the old roadmap update must not undo it.
	if _, err := Update(s, "demo-app", func(r *Record) error {
		r.Milestones[2].Title = "Reports, later"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	created := true
	tasks = append(tasks, &task.Task{
		ID: "t4", Project: "demo-app", Stage: "M0/close", Status: task.Done, After: []string{"t1"},
		Worktree: &task.Worktree{Branch: "release/M0-r1", Base: "feat/M0-scaffold", BranchCreated: &created},
	})
	oldMerge := run(t, p.Path, "rev-parse", "HEAD")
	if _, err := finish(s, p, tasks, oldMerge); err == nil || !strings.Contains(err.Error(), "merged before it was reopened") {
		t.Fatalf("the old merge, new branch gone: %v", err)
	}
	if _, err := finish(s, p, tasks, ""); err == nil || !strings.Contains(err.Error(), "merged before it was reopened") {
		t.Fatalf("new branch gone, the old notes found on main: %v", err)
	}

	run(t, p.Path, "checkout", "-q", "-b", "release/M0-r1", "main")
	writeFile(t, p.Path, ReleaseNotesPath("M0"), "# M0 release notes\n\nFixed: export drops a row.\n")
	run(t, p.Path, "commit", "-q", "-am", "docs(release): M0 release notes")
	run(t, p.Path, "checkout", "-q", "main")
	if _, err := finish(s, p, tasks, oldMerge); err == nil || !strings.Contains(err.Error(), "holds other release notes than release/M0-r1") {
		t.Fatalf("the old merge as the new release: %v", err)
	}
	if _, err := finish(s, p, tasks, ""); err == nil || !strings.Contains(err.Error(), "release/M0-r1 is not merged into main") {
		t.Fatalf("new release not merged: %v", err)
	}
	if tagExists(t, p.Path, "v0.0.0") || load(t, s).Milestones[0].Status != MilestoneInProgress {
		t.Fatal("a refused done tagged or recorded the milestone")
	}

	run(t, p.Path, "merge", "-q", "--no-ff", "--no-edit", "release/M0-r1")
	d, err := finish(s, p, tasks, "")
	if err != nil {
		t.Fatal(err)
	}
	rec := load(t, s)
	m := rec.Milestones[0]
	if d.Roadmap != 0 || rec.Milestones[2].Title != "Reports, later" || strings.Contains(rec.Decisions[len(rec.Decisions)-1].Text, "roadmap updated") {
		t.Errorf("the old roadmap update was imported again: %d, %+v", d.Roadmap, rec.Milestones[2])
	}
	if m.Status != MilestoneDone || m.Branch != "release/M0-r1" || len(m.Released) != 2 || m.Released[0] == m.Released[1] || m.RoadmapUpdate != first.RoadmapUpdate {
		t.Errorf("second done = %+v", m)
	}
	if !tagExists(t, p.Path, "v0.0.0") {
		t.Error("no tag")
	}
}

func TestReopenKeepsTheTagWhenTheRecordFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into read-only folders")
	}
	s, p, tasks := closingProject(t, "")
	run(t, p.Path, "merge", "-q", "--no-ff", "--no-edit", "release/M0")
	if _, err := finish(s, p, tasks, ""); err != nil {
		t.Fatal(err)
	}
	dir := s.ProjectDir("demo-app")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := ReopenMilestone(context.Background(), s, p, "M0", "export drops a row", finishedAt); err == nil {
		t.Fatal("reopened without a writable record")
	}
	if !tagExists(t, p.Path, "v0.0.0") {
		t.Error("a refused reopen deleted the tag")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	re, err := ReopenMilestone(context.Background(), s, p, "M0", "export drops a row", finishedAt)
	if err != nil || !re.Deleted || tagExists(t, p.Path, "v0.0.0") {
		t.Fatalf("reopen = %+v, %v", re, err)
	}
}

func TestReleaseBranchAvoidsNameClashes(t *testing.T) {
	dir := newRepo(t)
	ctx := context.Background()
	run(t, dir, "branch", "release/M1/hotfix")
	run(t, dir, "branch", "integrate")
	taken := func(b string) string { return BranchTaken(ctx, dir, b) }
	for b, want := range map[string]string{
		"release/M1": "release/M1/hotfix", "integrate/M1-I1": "integrate", "release/M10": "", "integrate": "integrate", "main": "main",
	} {
		if got := taken(b); got != want {
			t.Errorf("BranchTaken(%s) = %q, want %q", b, got, want)
		}
	}
	step, err := CloseStep("demo-app", "M1", "I1", "integrate/M1-I1", "", nil, taken)
	if err != nil {
		t.Fatal(err)
	}
	if step.Branch != "release/M1-r1" || !strings.Contains(step.Note, "release/M1-r1 is used instead of release/M1") || !strings.Contains(step.Note, "release/M1/hotfix") {
		t.Errorf("close step = %+v", step)
	}
	in := chainInput([]BlockNode{{ID: "F1", Type: NodeFeature, Title: "List"}}, nil)
	in.BranchTaken = taken
	if _, err := PlanChain(in); err == nil || !strings.Contains(err.Error(), "branch integrate exists, so git cannot create integrate/M1-") {
		t.Fatalf("integrate under a branch named integrate: %v", err)
	}
}

func TestDoneKeepsATipCleanupRecordedBefore(t *testing.T) {
	s, p, tasks := closingProject(t, "")
	// cleanup recorded a commit the release branch does not hold, such as
	// the HEAD of another branch in the close runner's worktree.
	run(t, p.Path, "checkout", "-q", "-b", "side")
	writeFile(t, p.Path, "side.txt", "side\n")
	run(t, p.Path, "add", "side.txt")
	run(t, p.Path, "commit", "-q", "-m", "chore: side")
	side := run(t, p.Path, "rev-parse", "HEAD")
	run(t, p.Path, "checkout", "-q", "main")
	tasks[1].Worktree.MergedTip = side
	if err := s.Create(tasks[1]); err != nil {
		t.Fatal(err)
	}
	run(t, p.Path, "merge", "-q", "--squash", "release/M0")
	run(t, p.Path, "commit", "-q", "-m", "M0 (#1)")
	if _, err := finish(s, p, tasks, run(t, p.Path, "rev-parse", "HEAD")); err != nil {
		t.Fatal(err)
	}
	tip := run(t, p.Path, "rev-parse", "release/M0")
	got, err := s.Get(tasks[1].ID)
	if err != nil || got.Worktree.MergedTip != tip || !slices.Equal(got.Worktree.OlderMergedTips, []string{side}) {
		t.Fatalf("merged tips = %+v, %v", got.Worktree, err)
	}
	if ref := run(t, p.Path, "rev-parse", task.MergedTipRef(tasks[1].ID)+"-"+side); ref != side {
		t.Errorf("older tip ref = %s, want %s", ref, side)
	}
}

// A done whose records commit fails puts its milestone back in progress,
// and keeps what another command, which takes no milestone lock, wrote to
// the record while the commit ran.
func TestFailedDoneKeepsWhatAnotherCommandChanged(t *testing.T) {
	s, p, tasks := closingProject(t, "")
	run(t, p.Path, "merge", "-q", "--no-ff", "--no-edit", "release/M0")
	other := Decision{At: finishedAt, Text: "pack v2 approved"}
	CommitHook = func() error {
		CommitHook = nil
		if _, err := Update(s, "demo-app", func(r *Record) error {
			r.Milestones[1].Title = "Shared lists"
			r.Decisions = append(r.Decisions, other)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return errors.New("the hook failed")
	}
	t.Cleanup(func() { CommitHook = nil })
	_, err := finish(s, p, tasks, "")
	if err == nil || !strings.Contains(err.Error(), "M0 stays in progress") || !strings.Contains(err.Error(), "milestone(s) M1 changed meanwhile and are kept") {
		t.Fatalf("done = %v", err)
	}
	rec := load(t, s)
	if m := rec.Milestones[0]; m.Status != MilestoneInProgress || m.Tag != "" || len(m.Released) != 0 {
		t.Errorf("M0 = %+v", m)
	}
	if rec.Milestones[1].Title != "Shared lists" || rec.Decisions[len(rec.Decisions)-1] != other {
		t.Errorf("the other command's change is lost: %+v, last decision %+v", rec.Milestones[1], rec.Decisions[len(rec.Decisions)-1])
	}
	if _, err := finish(s, p, tasks, ""); err != nil {
		t.Fatalf("done again: %v", err)
	}
}

// A graduation whose commit fails takes back its own record changes and
// keeps a change request added while the commit ran.
func TestFailedGraduationKeepsAChangeAddedMeanwhile(t *testing.T) {
	s, p := graduatingProject(t)
	cr1 := load(t, s).Changes[0]
	var added Change
	CommitHook = func() error {
		CommitHook = nil
		var err error
		if added, err = AddChange(s, "demo-app", "Add dark mode", "add-dark-mode", decidedAt); err != nil {
			t.Fatal(err)
		}
		return errors.New("the hook failed")
	}
	t.Cleanup(func() { CommitHook = nil })
	if _, err := graduate(s, p, GraduateOptions{}); err == nil || !strings.Contains(err.Error(), "nothing graduated") {
		t.Fatalf("graduate = %v", err)
	}
	rec := load(t, s)
	if rec.Graduated != nil || len(rec.Changes) != 2 || rec.Changes[1].ID != added.ID || rec.Changes[0] != cr1 {
		t.Errorf("record = graduated %+v, changes %+v", rec.Graduated, rec.Changes)
	}
}
