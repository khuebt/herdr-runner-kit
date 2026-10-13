package project

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

var graduatedAt = time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)

// graduatingProject returns a project whose gates passed, with an ignored
// original brief, an open change request whose file the graduate runner
// copied, and the runner's requirements and pack copy committed.
func graduatingProject(t *testing.T) (*state.Store, state.Project) {
	t.Helper()
	s, p := packApprovedProject(t)
	writeFile(t, p.Path, ".gitignore", BriefDir+"/\n")
	writeFile(t, p.Path, BriefDir+"/brief.md", "the original brief\n")
	writeFile(t, p.Path, DiscoveryDir+"/brief.md", "# Brief summary\n")
	run(t, p.Path, "add", ".gitignore", DiscoveryDir+"/brief.md")
	run(t, p.Path, "commit", "-q", "-m", "docs(discovery): brief")
	c, err := AddChange(s, "demo-app", "Add CSV export", "add-csv-export", decidedAt)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, p.Path, c.Path, "---\nid: CR-1\nclass: scope\n---\n")
	run(t, p.Path, "add", c.Path)
	run(t, p.Path, "commit", "-q", "-m", "docs(change): CR-1 impact")

	writeFile(t, p.Path, RequirementsPath, "# Requirements\n\nFrom spec v1. [Approved pack](approval/pack-v1/pack.html)\n")
	writeFile(t, p.Path, GraduatedChangesDir+"/CR-1-add-csv-export.md", "---\nid: CR-1\nclass: scope\n---\n")
	for _, f := range []string{PackHTML, PackPDF} {
		data, err := os.ReadFile(filepath.Join(p.Path, PackDir(1), f))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, p.Path, GraduatedApprovalDir+"/pack-v1/"+f, string(data))
	}
	run(t, p.Path, "add", "handbook")
	run(t, p.Path, "commit", "-q", "-m", "docs(product): requirements before graduation")
	return s, p
}

func graduate(s *state.Store, p state.Project, o GraduateOptions) (Graduation, error) {
	o.Now = graduatedAt
	return Graduate(context.Background(), s, p, o)
}

// git add refuses a path that is gone from disk, so staging a removed
// folder through it fails; graduation stages the removal with git rm and
// commits it with commit --only instead.
func TestGraduateNeverAddsDeletedPaths(t *testing.T) {
	s, p := graduatingProject(t)
	scratch := newRepo(t)
	writeFile(t, scratch, "d/a.md", "a\n")
	writeFile(t, scratch, "m.md", "m\n")
	run(t, scratch, "add", "d", "m.md")
	run(t, scratch, "commit", "-q", "-m", "x")
	run(t, scratch, "rm", "-r", "-q", "--", "d")
	writeFile(t, scratch, "m.md", "m2\n")
	out, err := exec.Command("git", "-C", scratch, "add", "--", "m.md", "d").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "pathspec 'd' did not match") {
		t.Fatalf("git add of a removed folder: %v %s", err, out)
	}

	// Another staged change stays staged and out of the commit.
	writeFile(t, p.Path, "notes.txt", "staged\n")
	run(t, p.Path, "add", "notes.txt")
	before := run(t, p.Path, "rev-parse", "HEAD")
	g, err := graduate(s, p, GraduateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if g.Tag != ArchiveTag || g.Head != before || g.Files != 6 {
		t.Errorf("graduation = %+v", g)
	}
	if got := run(t, p.Path, "rev-parse", "HEAD^"); got != before {
		t.Errorf("HEAD^ = %s, want %s", got, before)
	}
	if got := run(t, p.Path, "rev-parse", ArchiveTag+"^{commit}"); got != before {
		t.Errorf("tag on %s, want %s", got, before)
	}
	if got := run(t, p.Path, "cat-file", "-t", ArchiveTag); got != "tag" {
		t.Errorf("tag type = %s, want an annotated tag", got)
	}
	if got := run(t, p.Path, "ls-tree", "-r", "--name-only", ArchiveTag, "--", DiscoveryDir); !strings.Contains(got, SpecPath) {
		t.Errorf("archive lacks the spec: %s", got)
	}
	if got := run(t, p.Path, "ls-tree", "-r", "--name-only", "HEAD", "--", DiscoveryDir); got != "" {
		t.Errorf("HEAD still holds %s", got)
	}
	changed := run(t, p.Path, "show", "--name-only", "--format=", "HEAD")
	if strings.Contains(changed, "notes.txt") || !strings.Contains(changed, SnapshotPath) || !strings.Contains(changed, SpecPath) {
		t.Errorf("commit holds %s", changed)
	}
	if got := run(t, p.Path, "status", "--porcelain"); got != "A  notes.txt" {
		t.Errorf("status = %q", got)
	}
	if data, err := os.ReadFile(filepath.Join(p.Path, BriefDir, "brief.md")); err != nil || string(data) != "the original brief\n" {
		t.Errorf("original brief = %q, %v", data, err)
	}
	if len(g.Originals) != 1 || g.Originals[0] != BriefDir+"/brief.md" {
		t.Errorf("originals = %v", g.Originals)
	}

	rec := load(t, s)
	if rec.Graduated == nil || rec.Graduated.Tag != ArchiveTag || !rec.Graduated.At.Equal(graduatedAt) || rec.Pending != 0 {
		t.Fatalf("record = %+v", rec)
	}
	if rec.Changes[0].Path != GraduatedChangesDir+"/CR-1-add-csv-export.md" {
		t.Errorf("CR-1 path = %s", rec.Changes[0].Path)
	}
	if last := rec.Decisions[len(rec.Decisions)-1].Text; !strings.Contains(last, "stay in git history, archived at tag "+ArchiveTag) {
		t.Errorf("decision = %q", last)
	}
	snap, err := ParseSnapshot([]byte(run(t, p.Path, "show", "HEAD:"+SnapshotPath)))
	if err != nil || snap.Graduated == nil {
		t.Errorf("committed snapshot = %+v, %v", snap.Graduated, err)
	}

	// The gates keep passing from the record and the kept pack, change
	// requests go to the evergreen folder, and discovery stages refuse.
	for _, gate := range []string{GateSpec, GatePack} {
		if err := RequireGate(context.Background(), p.Path, rec, gate); err != nil {
			t.Errorf("%s gate after graduation: %v", gate, err)
		}
	}
	if c, err := AddChange(s, "demo-app", "Rename lists", "rename", graduatedAt); err != nil || c.ID != "CR-2" || c.Path != GraduatedChangesDir+"/CR-2-rename.md" {
		t.Errorf("change after graduation = %+v, %v", c, err)
	}
	ux, err := LoadStage("ux")
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckRequires(context.Background(), p.Path, rec, ux); err == nil || !strings.Contains(err.Error(), "archived at tag "+ArchiveTag) {
		t.Errorf("ux after graduation: %v", err)
	}
	if _, err := graduate(s, p, GraduateOptions{}); err == nil || !strings.Contains(err.Error(), "graduated on") {
		t.Errorf("graduated twice: %v", err)
	}
	if _, err := decide(s, GateSpec, "approve"); err == nil || !strings.Contains(err.Error(), "spec was archived") {
		t.Errorf("spec decided after graduation: %v", err)
	}
}

func TestGraduateCommitFailureRestoresEverything(t *testing.T) {
	s, p := graduatingProject(t)
	before := load(t, s)
	head := run(t, p.Path, "rev-parse", "HEAD")
	hook := filepath.Join(p.Path, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'blocked by hook' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := graduate(s, p, GraduateOptions{})
	if err == nil || !strings.Contains(err.Error(), "nothing graduated: blocked by hook") {
		t.Fatalf("err = %v", err)
	}
	if got := run(t, p.Path, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD moved to %s", got)
	}
	if tagExists(t, p.Path, ArchiveTag) {
		t.Error("a failed graduation left a tag")
	}
	if got := run(t, p.Path, "status", "--porcelain"); got != "" {
		t.Errorf("status after rollback = %q", got)
	}
	if _, err := os.Stat(filepath.Join(p.Path, SpecPath)); err != nil {
		t.Errorf("spec not restored: %v", err)
	}
	after := load(t, s)
	if after.Graduated != nil || len(after.Decisions) != len(before.Decisions) || after.Pending != before.Pending ||
		after.Changes[0].Path != before.Changes[0].Path {
		t.Errorf("record after rollback = %+v", after)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if _, err := graduate(s, p, GraduateOptions{}); err != nil {
		t.Fatalf("after the hook is fixed: %v", err)
	}
}

// Every condition is checked again under the lock, so a state that
// changed after a passing dry run is refused.
func TestGraduatePreconditions(t *testing.T) {
	s, p := graduatingProject(t)
	ctx := context.Background()
	plan, err := PlanGraduation(ctx, s, p, GraduateOptions{})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if plan.Tag != ArchiveTag || plan.Moved["CR-1"] != GraduatedChangesDir+"/CR-1-add-csv-export.md" {
		t.Errorf("plan = %+v", plan)
	}
	refused := func(name, want string, o GraduateOptions) {
		t.Helper()
		head := run(t, p.Path, "rev-parse", "HEAD")
		if _, err := graduate(s, p, o); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", name, err, want)
		}
		if run(t, p.Path, "rev-parse", "HEAD") != head || tagExists(t, p.Path, ArchiveTag) || load(t, s).Graduated != nil {
			t.Fatalf("%s: a refused graduation changed something", name)
		}
	}

	writeFile(t, p.Path, DiscoveryDir+"/ux/notes.md", "draft\n")
	refused("untracked file", DiscoveryDir+"/ has changes or files not committed (?? "+DiscoveryDir+"/ux/notes.md)", GraduateOptions{})
	if err := os.Remove(filepath.Join(p.Path, DiscoveryDir, "ux", "notes.md")); err != nil {
		t.Fatal(err)
	}

	tk := &task.Task{
		Kind: "report", Agent: "claude", Title: "ux", Owner: task.ProjectOwner("demo-app"),
		Project: "demo-app", Stage: "ux", Gate: task.GateAuto, Status: task.Running, UseWorktree: true,
	}
	if err := s.Create(tk); err != nil {
		t.Fatal(err)
	}
	refused("discovery runner", tk.ID+" (ux stage) is running", GraduateOptions{})
	if _, err := s.Update(tk.ID, func(t *task.Task) error { t.Status = task.Done; return nil }); err != nil {
		t.Fatal(err)
	}

	writeFile(t, p.Path, RequirementsPath, "# Requirements, edited\n")
	refused("requirements changed", RequirementsPath+" has changes not committed", GraduateOptions{})
	run(t, p.Path, "checkout", "--", RequirementsPath)

	writeFile(t, p.Path, "docs/guide.md", "intro\nsee [the spec](../"+SpecPath+"#scope) and [site](https://example.com/discovery/)\n")
	writeFile(t, p.Path, "handbook/index.html", "<a href='/"+DiscoveryDir+"/ux/'>ux</a>\n")
	run(t, p.Path, "add", "docs", "handbook/index.html")
	run(t, p.Path, "commit", "-q", "-m", "docs: links")
	refused("link", "docs/guide.md:2 links to ../"+SpecPath+"#scope", GraduateOptions{})
	refused("html link", "handbook/index.html:1 links to /"+DiscoveryDir+"/ux/", GraduateOptions{})
	run(t, p.Path, "rm", "-q", "docs/guide.md", "handbook/index.html")
	run(t, p.Path, "commit", "-q", "-m", "docs: drop links")

	export := t.TempDir()
	writeFile(t, export, "keep.txt", "x")
	refused("export not empty", "--export: "+export+" is not empty", GraduateOptions{Export: export})
	refused("export inside the repository", "is inside the repository", GraduateOptions{Export: filepath.Join(p.Path, "export")})
	refused("original without export", "--include-original goes only with --export", GraduateOptions{IncludeOriginal: true})

	writeFile(t, p.Path, GraduatedApprovalDir+"/pack-v1/"+PackHTML, "<html>edited</html>\n")
	run(t, p.Path, "commit", "-q", "-am", "docs: edit the kept pack")
	refused("pack copy changed", GraduatedApprovalDir+"/pack-v1/ is not the approved pack v1", GraduateOptions{})
	run(t, p.Path, "revert", "--no-edit", "HEAD")

	mergeHead := run(t, p.Path, "rev-parse", "--path-format=absolute", "--git-path", "MERGE_HEAD")
	writeFile(t, filepath.Dir(mergeHead), filepath.Base(mergeHead), run(t, p.Path, "rev-parse", "HEAD")+"\n")
	refused("merge in progress", "a merge in progress", GraduateOptions{})
	if err := os.Remove(mergeHead); err != nil {
		t.Fatal(err)
	}

	if _, err := graduate(s, p, GraduateOptions{}); err != nil {
		t.Fatalf("after every condition holds: %v", err)
	}
}

func TestGraduateNeedsRequirementsAndGates(t *testing.T) {
	dir := newRepo(t)
	s := registered(t, dir)
	p, err := s.ProjectByName("demo-app")
	if err != nil {
		t.Fatal(err)
	}
	commitSpec(t, dir, 1, "standard")
	_, err = PlanGraduation(context.Background(), s, p, GraduateOptions{})
	for _, want := range []string{"the spec gate is open", "the pack gate is open", "HEAD holds no " + RequirementsPath} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
}

func TestGraduateExportKeepsLinks(t *testing.T) {
	s, p := graduatingProject(t)
	if err := os.Symlink("../spec.md", filepath.Join(p.Path, DiscoveryDir, "ux-spec.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(p.Path, DiscoveryDir, "outside")); err != nil {
		t.Fatal(err)
	}
	run(t, p.Path, "add", DiscoveryDir)
	run(t, p.Path, "commit", "-q", "-m", "docs: links")

	if err := os.Symlink("/etc/hostname", filepath.Join(p.Path, BriefDir, "linked.txt")); err != nil {
		t.Fatal(err)
	}
	export := filepath.Join(t.TempDir(), "archive")
	if _, err := graduate(s, p, GraduateOptions{Export: export, IncludeOriginal: true}); err == nil ||
		!strings.Contains(err.Error(), "regular files only, and "+BriefDir+"/linked.txt is not one") {
		t.Fatalf("symlink in the original brief: %v", err)
	}
	if _, err := os.Stat(export); !os.IsNotExist(err) {
		t.Fatalf("a refused export wrote %s: %v", export, err)
	}
	if err := os.Remove(filepath.Join(p.Path, BriefDir, "linked.txt")); err != nil {
		t.Fatal(err)
	}

	g, err := graduate(s, p, GraduateOptions{Export: export, IncludeOriginal: true})
	if err != nil {
		t.Fatal(err)
	}
	if g.Export != export || load(t, s).Graduated.Export != export {
		t.Errorf("export = %q", g.Export)
	}
	for link, want := range map[string]string{"ux-spec.md": "../spec.md", "outside": "/etc/passwd"} {
		fi, err := os.Lstat(filepath.Join(export, link))
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s is not a link: %v", link, err)
		}
		if got, _ := os.Readlink(filepath.Join(export, link)); got != want {
			t.Errorf("%s -> %s, want %s", link, got, want)
		}
	}
	for rel, want := range map[string]string{"spec.md": specText(1, "standard"), "brief/original/brief.md": "the original brief\n"} {
		if data, err := os.ReadFile(filepath.Join(export, rel)); err != nil || string(data) != want {
			t.Errorf("%s = %q, %v", rel, data, err)
		}
	}
}

func TestLinksInto(t *testing.T) {
	for _, c := range []struct {
		file, target string
		want         bool
	}{
		{"handbook/product/roadmap.md", "discovery/spec.md", true},
		{"handbook/product/roadmap.md", "./discovery", true},
		{"docs/a.md", "../handbook/product/discovery/ux/flows.md#login", true},
		{"README.md", "/handbook/product/discovery/prototype/index.html?x=1", true},
		{"README.md", "handbook/product/discovery%2Fspec.md", true},
		{"handbook/product/roadmap.md", "discovery-notes.md", false},
		{"handbook/product/roadmap.md", "requirements.md", false},
		{"README.md", "https://example.com/handbook/product/discovery/spec.md", false},
		{"README.md", "#discovery", false},
		{"README.md", "//cdn.example.com/handbook/product/discovery/x.js", false},
	} {
		if got := linksInto(c.file, c.target, DiscoveryDir); got != c.want {
			t.Errorf("linksInto(%q, %q) = %v, want %v", c.file, c.target, got, c.want)
		}
	}
	got := linkTargets(`[a](x.md) [b]( <y z.md> ) <img src="i.png"> <a HREF='h.html'>`)
	if strings.Join(got, ",") != "x.md,y,i.png,h.html" {
		t.Errorf("targets = %v", got)
	}
	if got := linkTargets("[ref]: discovery/spec.md \"Spec\""); len(got) != 1 || got[0] != "discovery/spec.md" {
		t.Errorf("reference targets = %v", got)
	}
}

func TestGraduateStageData(t *testing.T) {
	s, p := graduatingProject(t)
	rec := load(t, s)
	d := NewStageData(rec, p.Path, Tools{}, "", graduatedAt)
	if err := GraduateData(context.Background(), &d, p.Path, rec); err != nil {
		t.Fatal(err)
	}
	if d.ArchiveTag != ArchiveTag || d.SpecVersion != "v1" || d.PackVersion != 1 || !strings.Contains(d.Changes, "- CR-1 [open] Add CSV export, file "+ChangesDir) {
		t.Errorf("data = %+v", d)
	}
	stage, err := LoadStage(GraduateStage)
	if err != nil {
		t.Fatal(err)
	}
	got, err := stage.Render(task.Claude, d)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Write " + RequirementsPath, "spec v1", "Copy " + PackDir(1) + "/ to " + GraduatedApprovalDir + "/pack-v1/",
		"from " + ChangesDir + "/ to " + GraduatedChangesDir + "/", "(archived at tag " + ArchiveTag + ")", "```untrusted-data",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("graduate prompt lacks %q:\n%s", want, got)
		}
	}
	run(t, p.Path, "tag", ArchiveTag)
	if err := GraduateData(context.Background(), &d, p.Path, rec); err != nil || d.ArchiveTag != ArchiveTag+"-2" {
		t.Errorf("taken tag: %q, %v", d.ArchiveTag, err)
	}

	// After graduation runners read the requirements, and the milestone
	// plan names the archive instead of the prototype folder.
	rec.Graduated = &Graduated{At: graduatedAt, Tag: ArchiveTag}
	d = NewStageData(rec, p.Path, Tools{}, "", graduatedAt)
	if d.Spec != RequirementsPath || d.Prototype != "" || d.ApprovalDir != GraduatedApprovalDir || d.ChangesDir != GraduatedChangesDir {
		t.Errorf("graduated data = %+v", d)
	}
	plan, err := LoadStage("milestone-plan")
	if err != nil {
		t.Fatal(err)
	}
	rec.Type = "ui"
	md := milestoneStageData(rec)
	got, err = plan.Render(task.Claude, md)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "from the approved "+RequirementsPath) || !strings.Contains(got, "archived at tag "+ArchiveTag) || strings.Contains(got, DiscoveryDir) {
		t.Errorf("graduated milestone plan:\n%s", got)
	}
}

// A scope change decided after graduation needs a new pack: the pack
// stage builds it from the evergreen docs, never the archived discovery
// folder, and approving it lets the next milestone start.
func TestPackAfterGraduation(t *testing.T) {
	s, p := graduatingProject(t)
	ctx := context.Background()
	if _, err := graduate(s, p, GraduateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(s, "demo-app", func(r *Record) error {
		r.Changes[0].Class = ClassScope
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if d, err := decideChange(s, "CR-1", ChangeNext); err != nil || !d.Reapproval {
		t.Fatalf("scope change decided next: %+v, %v", d, err)
	}
	if _, _, err := start(s, p, "M0"); err == nil || !strings.Contains(err.Error(), "the pack needs a new approval") {
		t.Fatalf("start before the new pack: %v", err)
	}

	rec := load(t, s)
	pack, err := LoadStage(GatePack)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckRequires(ctx, p.Path, rec, pack); err != nil {
		t.Fatalf("pack after graduation: %v", err)
	}
	n, err := CommittedPackVersion(ctx, p.Path, rec)
	if err != nil || n != 1 {
		t.Fatalf("committed pack = %d, %v", n, err)
	}
	d := NewStageData(rec, p.Path, Tools{}, "", graduatedAt)
	d.PackVersion = n + 1
	got, err := pack.Render(task.Claude, d)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"from the committed " + RequirementsPath, "handbook/product/roadmap.md", GraduatedChangesDir + "/",
		GraduatedApprovalDir + "/pack-v2/pack.html",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("graduated pack prompt lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(strings.ToLower(got), "discovery") {
		t.Errorf("graduated pack prompt names the discovery folder:\n%s", got)
	}
	if outputs, err := pack.RenderOutputs(d); err != nil || len(outputs) != 1 || outputs[0] != GraduatedApprovalDir+"/" {
		t.Errorf("outputs = %v, %v", outputs, err)
	}

	// The runner commits pack v2 next to the kept v1.
	writeFile(t, p.Path, GraduatedApprovalDir+"/pack-v2/"+PackHTML, "<html>pack v2</html>\n")
	writeFile(t, p.Path, GraduatedApprovalDir+"/pack-v2/"+PackPDF, "%PDF v2\n")
	run(t, p.Path, "add", GraduatedApprovalDir)
	run(t, p.Path, "commit", "-q", "-m", "docs(approval): pack v2")
	approved, err := decide(s, GatePack, "approve")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Version != "v2" || approved.Folder != GraduatedApprovalDir+"/pack-v2" || approved.Roadmap {
		t.Errorf("approved = %+v", approved)
	}
	if rec := load(t, s); rec.Reapproval.Needed || len(rec.Milestones) != 2 {
		t.Errorf("after approval: reapproval %+v, milestones %+v", rec.Reapproval, rec.Milestones)
	}
	if _, _, err := start(s, p, "M0"); err != nil {
		t.Fatalf("start after the new pack: %v", err)
	}
}

// A graduation that fails after the export takes the copy back, so the
// same folder is accepted when it runs again.
func TestGraduateFailureTakesBackTheExport(t *testing.T) {
	s, p := graduatingProject(t)
	hook := filepath.Join(p.Path, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'blocked by hook' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(t.TempDir(), "export")
	if _, err := graduate(s, p, GraduateOptions{Export: fresh}); err == nil ||
		!strings.Contains(err.Error(), "the export copy in "+fresh+" is removed") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Lstat(fresh); !os.IsNotExist(err) {
		t.Errorf("the export folder it created is still there: %v", err)
	}
	empty := t.TempDir()
	if _, err := graduate(s, p, GraduateOptions{Export: empty}); err == nil {
		t.Fatal("graduated through the hook")
	}
	if entries, err := os.ReadDir(empty); err != nil || len(entries) != 0 {
		t.Errorf("the empty export folder holds %v, %v", entries, err)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if _, err := graduate(s, p, GraduateOptions{Export: empty}); err != nil {
		t.Fatalf("after the hook is fixed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(empty, "spec.md")); err != nil {
		t.Errorf("export lacks the spec: %v", err)
	}
}
