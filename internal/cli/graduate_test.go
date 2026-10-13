package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/project"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func TestProjectGraduate(t *testing.T) {
	r, p := approvedRunner(t)
	gitIn(t, p.Path, "commit", "-q", "--allow-empty", "-m", "start")

	// The prep runner reads the approved versions and writes the
	// evergreen docs; graduation refuses until they are committed.
	prep, err := runStage(t.Context(), r, stageRun{Project: "demo-app", Stage: project.GraduateStage, Cwd: p.Path})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Write " + project.RequirementsPath, "spec v1", "/ak:scout", "pack-v1/ to " + project.GraduatedApprovalDir} {
		if !strings.Contains(prep.Prompt, want) {
			t.Errorf("graduate prompt lacks %q:\n%s", want, prep.Prompt)
		}
	}
	var out bytes.Buffer
	if err := graduateProject(t.Context(), &out, r.Store, p, project.GraduateOptions{}, false); err == nil ||
		!strings.Contains(err.Error(), prep.ID+" (graduate stage) is starting") || !strings.Contains(err.Error(), "HEAD holds no "+project.RequirementsPath) {
		t.Fatalf("before the prep runner finished: %v", err)
	}
	setTask(t, r, prep.ID, task.Done, false)
	pack, err := os.ReadFile(filepath.Join(p.Path, project.PackDir(1), "pack.html"))
	if err != nil {
		t.Fatal(err)
	}
	commitFiles(t, p.Path, map[string]string{
		project.RequirementsPath:                            "# Requirements\n\nFrom spec v1.\n",
		project.GraduatedApprovalDir + "/pack-v1/pack.html": string(pack),
	})

	out.Reset()
	if err := graduateProject(t.Context(), &out, r.Store, p, project.GraduateOptions{}, false); err != nil {
		t.Fatal(err)
	}
	dry := out.String()
	for _, want := range []string{
		"graduate demo-app: 3 committed file(s) under " + project.DiscoveryDir + "/ leave the tree",
		"archive: tag discovery-archive on", "graduation does not purge history",
		"nothing changed yet; graduate with: herdr-runner-kit project graduate --project demo-app --yes",
	} {
		if !strings.Contains(dry, want) {
			t.Errorf("dry run lacks %q:\n%s", want, dry)
		}
	}
	if _, err := os.Stat(filepath.Join(p.Path, project.SpecPath)); err != nil {
		t.Fatalf("the dry run changed the tree: %v", err)
	}

	out.Reset()
	if err := graduateProject(t.Context(), &out, r.Store, p, project.GraduateOptions{}, true); err != nil {
		t.Fatal(err)
	}
	commit := gitIn(t, p.Path, "rev-parse", "--short=12", "HEAD")
	done := out.String()
	for _, want := range []string{
		"graduated demo-app: " + project.DiscoveryDir + "/ removed in " + commit,
		"archive: tag discovery-archive on", "local only", "graduation does not purge history",
		"the way back: git -C " + p.Path + " show discovery-archive:" + project.SpecPath,
		"undo: git -C " + p.Path + " revert " + commit,
	} {
		if !strings.Contains(done, want) {
			t.Errorf("graduate lacks %q:\n%s", want, done)
		}
	}
	if got := gitIn(t, p.Path, "show", "discovery-archive:"+project.SpecPath); !strings.Contains(got, "# Spec") {
		t.Errorf("archived spec = %q", got)
	}

	// Discovery stages refuse; change requests go on, numbered as before,
	// into the evergreen folder, and read the requirements.
	if _, err := runStage(t.Context(), r, stageRun{Project: "demo-app", Stage: "ux", Cwd: p.Path}); err == nil ||
		!strings.Contains(err.Error(), "left the tree") {
		t.Errorf("ux after graduation: %v", err)
	}
	impact, err := runStage(t.Context(), r, stageRun{Project: "demo-app", Stage: "change", Notes: "Add CSV export", Cwd: p.Path})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(impact.Prompt, "Write "+project.GraduatedChangesDir+"/CR-1-add-csv-export.md") ||
		!strings.Contains(impact.Prompt, "Read "+project.RequirementsPath) || strings.Contains(impact.Prompt, project.DiscoveryDir+"/") {
		t.Errorf("change after graduation:\n%s", impact.Prompt)
	}
	setTask(t, r, impact.ID, task.Done, false)
	if err := startMilestone(t.Context(), &out, r, p, "M0", milestoneRun{}); err != nil {
		t.Errorf("milestone start after graduation: %v", err)
	}
}
