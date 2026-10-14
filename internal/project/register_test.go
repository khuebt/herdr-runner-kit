package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func TestRegisterFreshRecord(t *testing.T) {
	dir := newRepo(t)
	s := openStore(t)
	p, rec, err := Register(context.Background(), s, filepath.Join(dir, "."), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "demo-app" || p.Path != dir || p.Reserved || rec.Type != "unknown" || rec.Track != "standard" {
		t.Fatalf("project %+v record %+v", p, rec)
	}
	if got, found, _ := s.ProjectFor(dir); !found || got.Name != "demo-app" {
		t.Fatalf("ProjectFor = %+v %v", got, found)
	}
	if out := run(t, dir, "status", "--porcelain", "--ignored"); out != "" {
		t.Errorf("register wrote to the repository: %q", out)
	}
	if _, _, err := Register(context.Background(), s, dir, "other", ""); err == nil {
		t.Error("registered the same folder twice")
	}
}

func TestRegisterImportsCommittedSnapshot(t *testing.T) {
	dir := newRepo(t)
	rec := fullRecord()
	rec.Name = "reading-lists"
	data, err := Render(rec, SnapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(dir, SnapshotPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		t.Fatal(err)
	}
	// Uncommitted, the snapshot is not trusted.
	s := openStore(t)
	if _, got, err := Register(context.Background(), s, dir, "", ""); err != nil || got.Type != "unknown" || got.Name != "demo-app" {
		t.Fatalf("uncommitted snapshot used: %+v %v", got, err)
	}
	run(t, dir, "add", SnapshotPath)
	run(t, dir, "commit", "-q", "-m", "snapshot")

	s = openStore(t)
	p, got, err := Register(context.Background(), s, dir, "", "")
	if err != nil || p.Name != "reading-lists" || got.Type != "ui" || len(got.Milestones) != 1 {
		t.Fatalf("import: %+v %+v %v", p, got, err)
	}
	if stored, err := Load(s, "reading-lists"); err != nil || stored.Gates.Spec.Status != "approved" || stored.Pending != 0 {
		t.Fatalf("stored record: %+v %v", stored, err)
	}

	s = openStore(t)
	if p, got, err := Register(context.Background(), s, dir, "renamed", ""); err != nil || p.Name != "renamed" || got.Name != "renamed" {
		t.Fatalf("--name: %+v %+v %v", p, got, err)
	}

	if err := os.WriteFile(full, []byte("---\nmanifestVersion: 1\nname: x\nbogus: 1\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "commit", "-q", "-am", "broken snapshot")
	if _, _, err := Register(context.Background(), openStore(t), dir, "", ""); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("broken snapshot: %v", err)
	}
}

func TestRegisterRefusals(t *testing.T) {
	dir := newRepo(t)
	s := openStore(t)
	if _, _, err := Register(context.Background(), s, t.TempDir(), "plain", ""); err == nil {
		t.Error("registered a folder outside git")
	}
	wt := filepath.Join(filepath.Dir(dir), "wt")
	run(t, dir, "worktree", "add", "-q", "-b", "side", wt)
	if _, _, err := Register(context.Background(), s, wt, "side", ""); err == nil || !strings.Contains(err.Error(), "linked worktree") {
		t.Errorf("linked worktree: %v", err)
	}
	if _, _, err := Register(context.Background(), s, dir, "Bad Name", ""); err == nil {
		t.Error("invalid name accepted")
	}
	if list, _ := s.Projects(); len(list) != 0 {
		t.Errorf("refusals left entries: %+v", list)
	}
}

func TestRegisterAndCommitWithoutCommits(t *testing.T) {
	isolateGit(t)
	dir := filepath.Join(t.TempDir(), "fresh")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "init", "-q", "-b", "main")
	s := openStore(t)
	if _, _, err := Register(context.Background(), s, dir, "", ""); err != nil {
		t.Fatal(err)
	}
	if sha, err := Commit(context.Background(), s, "fresh", "docs: start project", SnapshotPath, DecisionsPath); err != nil || sha == "" {
		t.Fatalf("first commit: %q %v", sha, err)
	}
}

func TestForget(t *testing.T) {
	dir := newRepo(t)
	s := registered(t, dir)
	tk := &task.Task{
		Kind: "fix", Agent: "claude", Owner: task.ProjectOwner("demo-app"), Project: "demo-app",
		Gate: task.GateAuto, Status: task.Held,
	}
	if err := s.Create(tk); err != nil {
		t.Fatal(err)
	}
	if err := Forget(s, "demo-app", false); err == nil || !strings.Contains(err.Error(), tk.ID) {
		t.Fatalf("forget with a held task: %v", err)
	}
	if _, _, err := s.Transition(tk.ID, task.Stopped, nil, nil); err != nil {
		t.Fatal(err)
	}
	addDecision(t, s, "not committed")
	if err := Forget(s, "demo-app", false); err == nil || !strings.Contains(err.Error(), "1 change(s) not yet committed") {
		t.Fatalf("forget with pending changes: %v", err)
	}
	if err := Forget(s, "demo-app", true); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(s, "demo-app"); !errors.Is(err, ErrNoRecord) {
		t.Errorf("record kept: %v", err)
	}
	if err := Forget(s, "demo-app", true); !errors.Is(err, state.ErrUnknownProject) {
		t.Errorf("unknown project: %v", err)
	}
}

func TestRegisterReferenceWithoutPath(t *testing.T) {
	dir := newRepo(t)
	rec := fullRecord()
	rec.Name, rec.ReferenceRepo = "reading-lists", "reference-app"
	data, err := Render(rec, SnapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(dir, SnapshotPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", SnapshotPath)
	run(t, dir, "commit", "-q", "-m", "snapshot")

	_, got, err := Register(context.Background(), openStore(t), dir, "", "")
	if err != nil || got.ReferenceRepo != "reference-app" || !ReferenceWithoutPath(got) {
		t.Fatalf("name only: %+v %v", got, err)
	}
	local := t.TempDir()
	s := openStore(t)
	_, got, err = Register(context.Background(), s, dir, "", local)
	if err != nil || got.ReferenceRepo != local || ReferenceWithoutPath(got) {
		t.Fatalf("--reference: %+v %v", got, err)
	}
	if stored, err := Load(s, "reading-lists"); err != nil || stored.ReferenceRepo != local {
		t.Fatalf("stored record: %+v %v", stored, err)
	}
	if _, _, err := Register(context.Background(), openStore(t), dir, "", "https://u:fake-token-123@example.com/r.git"); err == nil {
		t.Error("registered a reference URL with credentials")
	}
}
