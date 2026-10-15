package planrun

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func phase(n int, deps []int, scope ...string) Phase {
	return Phase{Number: n, Deps: deps, Scope: scope}
}

func numbers(waves [][]Phase) [][]int {
	var out [][]int
	for _, w := range waves {
		var nums []int
		for _, ph := range w {
			nums = append(nums, ph.Number)
		}
		out = append(out, nums)
	}
	return out
}

func noneDone(int) bool { return false }

func doneSet(nums ...int) func(int) bool {
	return func(n int) bool {
		for _, d := range nums {
			if d == n {
				return true
			}
		}
		return false
	}
}

func TestWavesFirstPhaseRunsAlone(t *testing.T) {
	plan := &Plan{Phases: []Phase{
		phase(1, nil, "a/"),
		phase(2, nil, "b/"),
		phase(3, []int{1}, "c/"),
	}}
	waves, err := Waves(plan, 2, noneDone)
	if err != nil {
		t.Fatal(err)
	}
	if got := numbers(waves); !reflect.DeepEqual(got, [][]int{{1}, {2, 3}}) {
		t.Fatalf("waves = %v", got)
	}
}

func TestWavesMaxParallel(t *testing.T) {
	plan := &Plan{Phases: []Phase{
		phase(1, nil, "a/"),
		phase(2, []int{1}, "b/"),
		phase(3, []int{1}, "c/"),
		phase(4, []int{1}, "d/"),
	}}
	for maxParallel, want := range map[int][][]int{
		1: {{1}, {2}, {3}, {4}},
		2: {{1}, {2, 3}, {4}},
		0: {{1}, {2}, {3}, {4}},
	} {
		waves, err := Waves(plan, maxParallel, noneDone)
		if err != nil {
			t.Fatal(err)
		}
		if got := numbers(waves); !reflect.DeepEqual(got, want) {
			t.Errorf("maxParallel %d: waves = %v, want %v", maxParallel, got, want)
		}
	}
}

func TestWavesNeverPairAnEmptyOrOverlappingScope(t *testing.T) {
	plan := &Plan{Phases: []Phase{
		phase(1, nil, "a/"),
		phase(2, []int{1}),
		phase(3, []int{1}, "internal/api/"),
		phase(4, []int{1}, "internal/api/server.go"),
		phase(5, []int{1}, "docs/x.md"),
	}}
	waves, err := Waves(plan, 3, noneDone)
	if err != nil {
		t.Fatal(err)
	}
	if got := numbers(waves); !reflect.DeepEqual(got, [][]int{{1}, {2}, {3, 5}, {4}}) {
		t.Fatalf("waves = %v", got)
	}
}

func TestWavesDropDonePhases(t *testing.T) {
	plan := &Plan{Phases: []Phase{
		phase(1, nil, "a/"),
		phase(2, []int{1}, "b/"),
		phase(3, []int{2}, "c/"),
		phase(4, []int{1}, "d/"),
	}}
	waves, err := Waves(plan, 2, doneSet(1, 2))
	if err != nil {
		t.Fatal(err)
	}
	if got := numbers(waves); !reflect.DeepEqual(got, [][]int{{3, 4}}) {
		t.Fatalf("after a resume the dependents of done phases are ready together, got %v", got)
	}
	waves, err = Waves(plan, 2, doneSet(1, 2, 3, 4))
	if err != nil || len(waves) != 0 {
		t.Fatalf("all done: %v, %v", waves, err)
	}
}

func TestWavesFromReadPlan(t *testing.T) {
	plan, err := Read("testdata/basic")
	if err != nil {
		t.Fatal(err)
	}
	waves, err := Waves(plan, 2, doneSet(1))
	if err != nil {
		t.Fatal(err)
	}
	// Phase 3 names a bare file that may sit in phase 2's folder.
	if got := numbers(waves); !reflect.DeepEqual(got, [][]int{{2}, {3}, {4}}) {
		t.Fatalf("waves = %v", got)
	}
}

func TestWavesReportACycle(t *testing.T) {
	plan := &Plan{Phases: []Phase{phase(1, []int{2}), phase(2, []int{1})}}
	if _, err := Waves(plan, 2, noneDone); err == nil || !strings.Contains(err.Error(), "1, 2") {
		t.Fatalf("err = %v", err)
	}
}

func TestOverlaps(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"a/b.go", "a/b.go", true},
		{"a/b.go", "a/c.go", false},
		{"a/", "a/b.go", true},
		{"a/", "ab/c.go", false},
		{"a/", "a/b/", true},
		{"a/b", "a/b/c.go", true},
		{"a/*_test.go", "a/x_test.go", true},
		{"a/*_test.go", "b/x_test.go", false},
		{"runner_test.go", "internal/runner/runner_test.go", true},
		{"runner_test.go", "internal/runner/spawn.go", false},
		{"runner_test.go", "internal/", true},
	}
	for _, s := range []string{"`GitState.Head`", "`Handler`", "`a b.go`"} {
		if got := Scope([]byte("## Files\n- " + s + "\n")); len(got) != 0 {
			t.Errorf("%s is not a path, got %q", s, got)
		}
	}
	for _, c := range cases {
		if got := Overlaps([]string{c.a}, []string{c.b}); got != c.want {
			t.Errorf("Overlaps(%q, %q) = %v", c.a, c.b, got)
		}
		if got := Overlaps([]string{c.b}, []string{c.a}); got != c.want {
			t.Errorf("Overlaps(%q, %q) = %v", c.b, c.a, got)
		}
	}
}

func TestInScope(t *testing.T) {
	scope := []string{"internal/api/", "docs/api.md", "internal/cli/*_test.go", "runner_test.go", "pkg/**"}
	for file, want := range map[string]bool{
		"internal/api/server.go":         true,
		"docs/api.md":                    true,
		"docs/other.md":                  false,
		"internal/cli/plan_test.go":      true,
		"internal/cli/plan.go":           false,
		"internal/runner/runner_test.go": true,
		"pkg/deep/x.go":                  true,
	} {
		if got := InScope(scope, file); got != want {
			t.Errorf("InScope(%q) = %v", file, got)
		}
	}
}

func TestSetStatusChangesOnlyThatLine(t *testing.T) {
	src, err := os.ReadFile("testdata/basic/phase-02-api.md")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "phase.md")
	if err := os.WriteFile(file, src, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetStatus(file, "completed"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(file)
	want := strings.Replace(string(src), "status: pending\n", "status: completed\n", 1)
	if string(got) != want {
		t.Fatalf("file =\n%s", got)
	}

	crlf := filepath.Join(t.TempDir(), "crlf.md")
	if err := os.WriteFile(crlf, []byte("---\r\nstatus: pending\r\n---\r\nstatus: body\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetStatus(crlf, "in-progress"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(crlf); string(got) != "---\r\nstatus: in-progress\r\n---\r\nstatus: body\r\n" {
		t.Fatalf("crlf file = %q", got)
	}
}

func TestSetStatusWithoutStatusLine(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"no-line.md":        "---\ntitle: x\n---\nstatus: body\n",
		"no-frontmatter.md": "status: pending\n",
	} {
		file := filepath.Join(dir, name)
		if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := SetStatus(file, "completed"); err == nil {
			t.Errorf("%s: want an error", name)
		}
		if got, _ := os.ReadFile(file); string(got) != body {
			t.Errorf("%s changed: %q", name, got)
		}
	}
}
