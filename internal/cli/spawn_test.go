package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func spawnOptions(kind string) runner.SpawnOptions {
	return runner.SpawnOptions{Kind: kind, Prompt: "write the parser", Warn: func(string) {}}
}

func TestSpawnReviewChainsAReview(t *testing.T) {
	r, _, _, _ := planRunner(t, map[string]string{"phase-01-a.md": phaseFile(1, "[]", "a/")})
	r.StartLauncher = func(string) (int, error) { return 4242, nil }
	r.Config.Review.Agent = "claude:claude-opus-5-5:high"
	var out, errOut bytes.Buffer
	yes := true
	o := spawnOptions("code")
	if err := spawnTask(t.Context(), r, o, &yes, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], "review of t1") || !strings.Contains(lines[1], "after t1") {
		t.Fatalf("output:\n%s", out.String())
	}
	if rv := getTask(t, r, "t2"); rv.Review == nil || rv.Review.Of != "t1" || rv.Status != task.Queued {
		t.Errorf("review = %+v %+v", rv, rv.Review)
	}
}

func TestSpawnReviewFollowsTheKind(t *testing.T) {
	r, _, _, _ := planRunner(t, map[string]string{"phase-01-a.md": phaseFile(1, "[]", "a/")})
	r.StartLauncher = func(string) (int, error) { return 4242, nil }
	r.Config.Review.Agent = "claude:claude-opus-5-5:high"
	k := r.Config.Kinds["code"]
	k.Review = true
	r.Config.Kinds["code"] = k
	var out bytes.Buffer
	if err := spawnTask(t.Context(), r, spawnOptions("code"), nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	if tasks, _ := r.Store.List(); len(tasks) != 2 {
		t.Fatalf("a kind with review spawned %d tasks, want 2", len(tasks))
	}
	no := false
	if err := spawnTask(t.Context(), r, spawnOptions("code"), &no, &out, &out); err != nil {
		t.Fatal(err)
	}
	if tasks, _ := r.Store.List(); len(tasks) != 3 {
		t.Fatalf("--review=false spawned %d tasks in total, want 3", len(tasks))
	}
}

func TestSpawnReviewRefusesFix(t *testing.T) {
	r, _, _, _ := planRunner(t, map[string]string{"phase-01-a.md": phaseFile(1, "[]", "a/")})
	r.StartLauncher = func(string) (int, error) { return 4242, nil }
	yes := true
	var out bytes.Buffer
	err := spawnTask(t.Context(), r, spawnOptions("fix"), &yes, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "fix") {
		t.Fatalf("err = %v", err)
	}
	if tasks, _ := r.Store.List(); len(tasks) != 0 {
		t.Errorf("a refused kind created %d tasks", len(tasks))
	}
}
