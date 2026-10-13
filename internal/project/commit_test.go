package project

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// isolateGit keeps the user's git config, hooks and signing out of tests.
func isolateGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "t")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@t")
	}
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newRepo(t *testing.T) string {
	t.Helper()
	isolateGit(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(dir, "demo-app")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "init", "-q", "-b", "main")
	run(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	return dir
}

// registered returns a store with the repository at dir registered as
// demo-app.
func registered(t *testing.T, dir string) *state.Store {
	t.Helper()
	s := openStore(t)
	if _, _, err := Register(context.Background(), s, dir, ""); err != nil {
		t.Fatal(err)
	}
	return s
}

func addDecision(t *testing.T, s *state.Store, text string) {
	t.Helper()
	if _, err := Update(s, "demo-app", func(r *Record) error {
		r.Decisions = append(r.Decisions, Decision{At: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC), Text: text})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCommitOnlyRenderedFiles(t *testing.T) {
	dir := newRepo(t)
	s := registered(t, dir)
	addDecision(t, s, "use SQLite")
	// Unrelated work the user staged stays staged and out of the commit.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("draft\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", "notes.txt")

	sha, err := Commit(context.Background(), s, "demo-app", "docs: record decisions")
	if err != nil {
		t.Fatal(err)
	}
	if sha == "" || run(t, dir, "rev-parse", "HEAD") != sha {
		t.Fatalf("sha = %q", sha)
	}
	files := strings.Fields(run(t, dir, "show", "--name-only", "--format=", "HEAD"))
	if !slices.Equal(files, []string{BacklogPath, DecisionsPath, SnapshotPath, RoadmapPath}) {
		t.Errorf("committed %v", files)
	}
	if got := run(t, dir, "diff", "--cached", "--name-only"); got != "notes.txt" {
		t.Errorf("staged after commit: %q", got)
	}
	if got := run(t, dir, "log", "-1", "--format=%s"); got != "docs: record decisions" {
		t.Errorf("message %q", got)
	}
	if rec, _ := Load(s, "demo-app"); rec.Pending != 0 {
		t.Errorf("pending = %d after commit", rec.Pending)
	}
	data, err := os.ReadFile(filepath.Join(dir, DecisionsPath))
	if err != nil || !strings.Contains(string(data), "- 2026-10-14: use SQLite") {
		t.Errorf("decisions.md: %s %v", data, err)
	}

	// Nothing changed: no empty commit.
	if sha, err := Commit(context.Background(), s, "demo-app", "docs: again"); err != nil || sha != "" {
		t.Fatalf("second commit: %q %v", sha, err)
	}
}

func TestCommitSomePaths(t *testing.T) {
	dir := newRepo(t)
	s := registered(t, dir)
	if _, err := Commit(context.Background(), s, "demo-app", "docs: start project", SnapshotPath, DecisionsPath); err != nil {
		t.Fatal(err)
	}
	files := strings.Fields(run(t, dir, "show", "--name-only", "--format=", "HEAD"))
	if !slices.Equal(files, []string{DecisionsPath, SnapshotPath}) {
		t.Errorf("committed %v", files)
	}
	if _, err := Commit(context.Background(), s, "demo-app", "x", ".gitignore"); err == nil {
		t.Error("committed a path the kit does not render")
	}
}

func TestCommitRefusesDuringMerge(t *testing.T) {
	dir := newRepo(t)
	s := registered(t, dir)
	head := run(t, dir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, ".git", "MERGE_HEAD"), []byte(head+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Commit(context.Background(), s, "demo-app", "docs: x")
	if err == nil || !strings.Contains(err.Error(), "merge in progress") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, SnapshotPath)); !os.IsNotExist(statErr) {
		t.Error("files written during a merge")
	}
}

func TestCommitRefusesWhileARunnerWorksInTheCheckout(t *testing.T) {
	dir := newRepo(t)
	s := registered(t, dir)
	add := func(status task.Status, worktree bool) *task.Task {
		tk := &task.Task{
			Kind: "fix", Agent: "claude", Title: "write spec", Owner: task.ProjectOwner("demo-app"),
			Project: "demo-app", Gate: task.GateAuto, Status: status, UseWorktree: worktree,
		}
		if err := s.Create(tk); err != nil {
			t.Fatal(err)
		}
		return tk
	}
	add(task.Running, true) // in its own worktree: no conflict
	add(task.Done, false)
	add(task.Queued, false) // not started yet
	if _, err := Commit(context.Background(), s, "demo-app", "docs: x"); err != nil {
		t.Fatalf("refused without a runner in the checkout: %v", err)
	}
	busy := add(task.Running, false)
	addDecision(t, s, "more")
	_, err := Commit(context.Background(), s, "demo-app", "docs: y")
	if err == nil || !strings.Contains(err.Error(), busy.ID+" (write spec) is running") {
		t.Fatalf("err = %v", err)
	}
}

func TestCommitHookFailureKeepsFiles(t *testing.T) {
	dir := newRepo(t)
	s := registered(t, dir)
	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'lint: trailing space' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Commit(context.Background(), s, "demo-app", "docs: it's done")
	if err == nil {
		t.Fatal("commit passed a failing hook")
	}
	msg := err.Error()
	if !strings.Contains(msg, "lint: trailing space") ||
		!strings.Contains(msg, "git -C "+dir+" commit --only -m 'docs: it'\\''s done' -- "+SnapshotPath) {
		t.Errorf("err = %v", msg)
	}
	if _, statErr := os.Stat(filepath.Join(dir, SnapshotPath)); statErr != nil {
		t.Errorf("rendered file removed: %v", statErr)
	}
	if got := run(t, dir, "rev-list", "--count", "HEAD"); got != "1" {
		t.Errorf("commits = %s", got)
	}
}

func TestCommitRetriesWhileIndexLocked(t *testing.T) {
	dir := newRepo(t)
	s := registered(t, dir)
	pause := indexLockPause
	indexLockPause = 50 * time.Millisecond
	t.Cleanup(func() { indexLockPause = pause })
	// Another git process holds the index lock for a moment.
	lockFile := filepath.Join(dir, ".git", "index.lock")
	if err := os.WriteFile(lockFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(120 * time.Millisecond)
		_ = os.Remove(lockFile)
	}()
	sha, err := Commit(context.Background(), s, "demo-app", "docs: x")
	if err != nil || sha == "" {
		t.Fatalf("commit: %q %v", sha, err)
	}
}

func TestCommitGivesUpOnAStuckIndexLock(t *testing.T) {
	dir := newRepo(t)
	s := registered(t, dir)
	pause := indexLockPause
	indexLockPause = time.Millisecond
	t.Cleanup(func() { indexLockPause = pause })
	if err := os.WriteFile(filepath.Join(dir, ".git", "index.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Commit(context.Background(), s, "demo-app", "docs: x"); err == nil || !strings.Contains(err.Error(), "index.lock") {
		t.Fatalf("err = %v", err)
	}
}

func TestCommitRefusesDuringOtherOperations(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"rebase-merge", "a rebase in progress"},
		{"rebase-apply", "a rebase or git am in progress"},
		{"CHERRY_PICK_HEAD", "a cherry-pick in progress"},
		{"REVERT_HEAD", "a revert in progress"},
		{"BISECT_LOG", "a bisect in progress"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			dir := newRepo(t)
			s := registered(t, dir)
			marker := filepath.Join(dir, ".git", tc.path)
			var err error
			if strings.HasPrefix(tc.path, "rebase-") {
				err = os.Mkdir(marker, 0o755)
			} else {
				err = os.WriteFile(marker, []byte("x\n"), 0o644)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = Commit(context.Background(), s, "demo-app", "docs: x")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if _, statErr := os.Stat(filepath.Join(dir, SnapshotPath)); !os.IsNotExist(statErr) {
				t.Error("files written during the operation")
			}
		})
	}
}

func TestCommitTimeoutLeavesNoIndexLock(t *testing.T) {
	dir := newRepo(t)
	s := registered(t, dir)
	done := filepath.Join(t.TempDir(), "hook-finished")
	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
	// The hook outlives the timeout; its child must stop with git.
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nsleep 2\ntouch "+done+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	timeout := CommitTimeout
	CommitTimeout = 300 * time.Millisecond
	t.Cleanup(func() { CommitTimeout = timeout })
	start := time.Now()
	_, err := Commit(context.Background(), s, "demo-app", "docs: x")
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("err = %v", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("the timed-out commit took %s", took)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "index.lock")); !os.IsNotExist(err) {
		t.Errorf("index.lock left behind: %v", err)
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(done); !os.IsNotExist(err) {
		t.Error("the hook kept running after the timeout")
	}
}

func TestLeftLockNamesTheFix(t *testing.T) {
	dir := newRepo(t)
	if got := leftLock(context.Background(), dir); got != "" {
		t.Fatalf("no lock, got %q", got)
	}
	lock := filepath.Join(dir, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := leftLock(context.Background(), dir); got != lock {
		t.Fatalf("lock %q, want %q", got, lock)
	}
}
