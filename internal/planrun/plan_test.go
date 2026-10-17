package planrun

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReadPlanAndPhases(t *testing.T) {
	plan, err := Read("testdata/basic")
	if err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs("testdata/basic")
	if plan.Title != "Toy plan" || plan.Status != "pending" || plan.Dir != abs || plan.File != filepath.Join(abs, "plan.md") {
		t.Fatalf("plan = %+v", plan)
	}
	viaFile, err := Read("testdata/basic/plan.md")
	if err != nil || viaFile.Dir != abs {
		t.Fatalf("read via plan.md: %+v, %v", viaFile, err)
	}
	var nums []int
	for _, ph := range plan.Phases {
		nums = append(nums, ph.Number)
	}
	if !reflect.DeepEqual(nums, []int{1, 2, 3, 4}) {
		t.Fatalf("phases = %v", nums)
	}
	p3, _ := plan.Phase(3)
	if p3.Title != "CLI" || p3.Status != "pending" || p3.File != filepath.Join(abs, "phase-03-cli.md") {
		t.Fatalf("phase 3 = %+v", p3)
	}
	p4, _ := plan.Phase(4)
	if p4.Title != "Phase 4: Docs" {
		t.Fatalf("a missing title falls back to the heading, got %q", p4.Title)
	}
}

func TestReadDependencies(t *testing.T) {
	plan, err := Read("testdata/basic")
	if err != nil {
		t.Fatal(err)
	}
	want := map[int][]int{1: nil, 2: {1}, 3: {1}, 4: {3}}
	for _, ph := range plan.Phases {
		if len(ph.Deps) == 0 && len(want[ph.Number]) == 0 {
			continue
		}
		if !reflect.DeepEqual(ph.Deps, want[ph.Number]) {
			t.Errorf("phase %d deps = %v, want %v", ph.Number, ph.Deps, want[ph.Number])
		}
	}

	dir := t.TempDir()
	write(t, dir, "plan.md", "---\ntitle: x\n---\n")
	write(t, dir, "phase-01-a.md", "---\nphase: 1\n---\n")
	write(t, dir, "phase-02-b.md", "---\nphase: 2\ndependencies: []\n---\n")
	plan, err = Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p2, _ := plan.Phase(2); len(p2.Deps) != 0 {
		t.Fatalf("an empty list makes a root phase, got %v", p2.Deps)
	}
}

func TestReadRefusesBadDependencies(t *testing.T) {
	for dir, want := range map[string]string{
		"testdata/unknown": "phase 2 depends on phase 7",
		"testdata/cycle":   "cycle: 1 -> 2 -> 1",
		"testdata/badDep":  `dependency phase-01 is not a phase number`,
	} {
		_, err := Read(dir)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", dir, err, want)
		}
	}
	if _, err := Read(t.TempDir()); err == nil {
		t.Error("a folder without plan.md must fail")
	}
	dir := t.TempDir()
	write(t, dir, "plan.md", "# x\n")
	if _, err := Read(dir); err == nil || !strings.Contains(err.Error(), "no phase-*.md") {
		t.Errorf("a plan without phases: err = %v", err)
	}
}

func TestScopeFromFileSections(t *testing.T) {
	plan, err := Read("testdata/basic")
	if err != nil {
		t.Fatal(err)
	}
	want := map[int][]string{
		1: {"cmd/tool/main.go"},
		2: {"internal/api/server.go", "internal/api/", "docs/api.md", "internal/api/notes.go"},
		3: {"internal/cli/*_test.go", "runner_test.go"},
		4: nil,
	}
	for _, ph := range plan.Phases {
		if !reflect.DeepEqual(ph.Scope, want[ph.Number]) {
			t.Errorf("phase %d scope = %q, want %q", ph.Number, ph.Scope, want[ph.Number])
		}
	}
}

func TestScopeHeadingVariants(t *testing.T) {
	for _, heading := range []string{"## Files", "## Files to Create", "### Files to Modify", "## Related Code Files", "## Files to Create / Modify"} {
		got := Scope([]byte(heading + "\n\n- `a/b.go:3`\n"))
		if !reflect.DeepEqual(got, []string{"a/b.go"}) {
			t.Errorf("%q: scope = %q", heading, got)
		}
	}
}

func TestScopePlaceholdersAndModulePaths(t *testing.T) {
	got := Scope([]byte("## Files\n- `internal/x/testdata/<small synthetic plans>/`\n- `gopkg.in/yaml.v3`\n- `github.com/a/b`\n- `.github/workflows/ci.yml`\n"))
	if want := []string{"internal/x/testdata/", ".github/workflows/ci.yml"}; !reflect.DeepEqual(got, want) {
		t.Errorf("scope = %q, want %q", got, want)
	}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func reviewPlan(t *testing.T, planFM string) (*Plan, error) {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "plan.md", "---\ntitle: x\n"+planFM+"---\n")
	for _, n := range []string{"1", "2", "3"} {
		write(t, dir, "phase-0"+n+"-p.md", "---\nphase: "+n+"\nreview: skip\nreview_after: 9\n---\n")
	}
	return Read(dir)
}

func TestReadReviewFields(t *testing.T) {
	plan, err := reviewPlan(t, "review: required\nreview_after: 2\nreview_agent: codex::high\n")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Review != ReviewRequired || plan.ReviewAfter != 2 || plan.ReviewAgent != "codex::high" {
		t.Fatalf("review fields = %q, %d, %q", plan.Review, plan.ReviewAfter, plan.ReviewAgent)
	}
	plan, err = reviewPlan(t, "review: required\nreview_after: \"2\"\n")
	if err != nil || plan.ReviewAfter != 2 {
		t.Fatalf("a string review_after reads as 2: %+v, %v", plan, err)
	}
	for name, dir := range map[string]string{"none": "", "basic": "testdata/basic"} {
		var plan *Plan
		if dir == "" {
			plan, err = reviewPlan(t, "")
		} else {
			plan, err = Read(dir)
		}
		if err != nil || plan.Review != "" || plan.ReviewAfter != 0 || plan.ReviewAgent != "" {
			t.Fatalf("%s: review fields = %+v, %v", name, plan, err)
		}
	}
}

func TestReadRefusesBadReviewFields(t *testing.T) {
	for fm, want := range map[string]string{
		"review: maybe\n":                         "review must be required or skip",
		"review: required\nreview_after: 9\n":     "which the plan does not have",
		"review: skip\nreview_after: 1\n":         "review_after needs review: required",
		"review_after: 1\n":                       "review_after needs review: required",
		"review: required\nreview_after: first\n": "is not a phase number",
		"review: required\nreview_agent: nope\n":  "review_agent:",
	} {
		_, err := reviewPlan(t, fm)
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "plan.md") {
			t.Errorf("%q: error = %v, want %q", fm, err, want)
		}
	}
}
