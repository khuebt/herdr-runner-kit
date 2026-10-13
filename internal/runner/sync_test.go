package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// syncTask makes a finished worktree task with an open, idle runner, and a
// commit on origin's main that its branch does not have. path is written
// with content on main.
func syncTask(t *testing.T, path, content string) (*Runner, *fakeHerdr, env, *task.Task) {
	t.Helper()
	for _, k := range []string{"GIT_AUTHOR", "GIT_COMMITTER"} {
		t.Setenv(k+"_NAME", "t")
		t.Setenv(k+"_EMAIL", "t@t")
	}
	r, fake, e := newTestRunner(t)
	tk, repo := worktreeTask(t, r, "fix/sync-a")
	if err := os.WriteFile(filepath.Join(repo, path), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", path)
	git(t, repo, "commit", "-q", "-m", "feat: on main")
	git(t, repo, "push", "-q", "origin", "main")
	tk, err := r.Store.Update(tk.ID, func(t *task.Task) error { t.Herdr.Closed = false; return nil })
	if err != nil {
		t.Fatal(err)
	}
	fake.on("agent get", ok(`{"result":{"agent":{"agent":"codex","agent_status":"idle"}}}`))
	return r, fake, e, tk
}

func head(t *testing.T, dir string) string {
	t.Helper()
	return gitOut(context.Background(), dir, "rev-parse", "HEAD")
}

func TestSyncMergesDefaultBranchAndTellsTheRunner(t *testing.T) {
	r, fake, _, tk := syncTask(t, "b.txt", "b\n")
	res, err := r.Sync(context.Background(), tk.ID, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ref != "origin/main" || res.Branch != "fix/sync-a" || !res.Sent || res.UpToDate {
		t.Fatalf("result = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(tk.Worktree.Path, "b.txt")); err != nil {
		t.Errorf("main's file is not in the worktree: %v", err)
	}
	prompt := fake.call("agent", "prompt")
	if len(prompt) == 0 || prompt[len(prompt)-1] != SyncNote("origin/main", res.SHA) {
		t.Errorf("prompt = %q", prompt)
	}
	got, _ := r.Store.Get(tk.ID)
	if got.Status != task.Running {
		t.Errorf("status after sync = %s, want running so the next report counts", got.Status)
	}
	if evs := events(t, r, tk.ID); !slices.Equal(evs, []string{"synced", "sent"}) {
		t.Errorf("events = %v", evs)
	}

	res, err = r.Sync(context.Background(), tk.ID, "main", false)
	if err != nil || !res.UpToDate {
		t.Errorf("second sync = %+v, %v", res, err)
	}
	if fake.count("agent", "prompt") != 1 {
		t.Error("an up-to-date sync told the runner")
	}
}

func TestSyncNoSendLeavesTheRunnerAlone(t *testing.T) {
	r, fake, _, tk := syncTask(t, "b.txt", "b\n")
	res, err := r.Sync(context.Background(), tk.ID, "main", false)
	if err != nil || res.Sent || res.UpToDate {
		t.Fatalf("Sync = %+v, %v", res, err)
	}
	if fake.count("agent", "prompt") != 0 {
		t.Error("--no-send told the runner")
	}
	if got, _ := r.Store.Get(tk.ID); got.Status != task.Done {
		t.Errorf("status = %s", got.Status)
	}
}

func TestSyncClosedTabMergesWithoutSending(t *testing.T) {
	r, fake, _, tk := syncTask(t, "b.txt", "b\n")
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error { t.Herdr.Closed = true; return nil }); err != nil {
		t.Fatal(err)
	}
	res, err := r.Sync(context.Background(), tk.ID, "main", true)
	if err != nil || res.Sent || res.UpToDate {
		t.Fatalf("Sync = %+v, %v", res, err)
	}
	if fake.count("agent", "prompt") != 0 {
		t.Error("a closed runner was prompted")
	}
}

func TestSyncRefuses(t *testing.T) {
	cases := map[string]struct {
		setup func(t *testing.T, r *Runner, fake *fakeHerdr, e env, tk *task.Task)
		want  string
	}{
		"inside a runner": {
			setup: func(_ *testing.T, _ *Runner, _ *fakeHerdr, e env, _ *task.Task) { e["HRK_TASK"] = "t9" },
			want:  "runners cannot sync runners",
		},
		"dirty": {
			setup: func(t *testing.T, _ *Runner, _ *fakeHerdr, _ env, tk *task.Task) {
				if err := os.WriteFile(filepath.Join(tk.Worktree.Path, "a.txt"), []byte("edit\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "uncommitted changes (a.txt)",
		},
		"untracked work": {
			setup: func(t *testing.T, _ *Runner, _ *fakeHerdr, _ env, tk *task.Task) {
				writeFile(t, filepath.Join(tk.Worktree.Path, "notes.txt"))
			},
			want: "uncommitted changes (notes.txt)",
		},
		"busy": {
			setup: func(_ *testing.T, _ *Runner, fake *fakeHerdr, _ env, _ *task.Task) {
				fake.on("agent get", ok(`{"result":{"agent":{"agent":"codex","agent_status":"working"}}}`))
			},
			want: "busy",
		},
		"another branch": {
			setup: func(t *testing.T, _ *Runner, _ *fakeHerdr, _ env, tk *task.Task) {
				git(t, tk.Worktree.Path, "checkout", "-q", "-b", "other")
			},
			want: "is on other, not its branch fix/sync-a",
		},
		"removed worktree": {
			setup: func(t *testing.T, r *Runner, _ *fakeHerdr, _ env, tk *task.Task) {
				if _, err := r.Store.Update(tk.ID, func(t *task.Task) error { t.Worktree.Removed = true; return nil }); err != nil {
					t.Fatal(err)
				}
			},
			want: "has no worktree",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r, fake, e, tk := syncTask(t, "b.txt", "b\n")
			c.setup(t, r, fake, e, tk)
			before := head(t, tk.Worktree.Path)
			_, err := r.Sync(context.Background(), tk.ID, "main", true)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if head(t, tk.Worktree.Path) != before || fake.count("agent", "prompt") != 0 {
				t.Error("a refused sync merged or told the runner")
			}
		})
	}
}

func TestSyncConflictAbortsWithHeadUnchanged(t *testing.T) {
	// The branch added a.txt too, with other content.
	r, fake, _, tk := syncTask(t, "a.txt", "main's a\n")
	before := head(t, tk.Worktree.Path)
	res, err := r.Sync(context.Background(), tk.ID, "", true)
	if !errors.Is(err, ErrSyncConflict) {
		t.Fatalf("err = %v", err)
	}
	if !slices.Equal(res.Conflicts, []string{"a.txt"}) {
		t.Errorf("conflicts = %q", res.Conflicts)
	}
	if head(t, tk.Worktree.Path) != before {
		t.Error("HEAD moved")
	}
	if gitOut(context.Background(), tk.Worktree.Path, "rev-parse", "--verify", "--quiet", "MERGE_HEAD") != "" {
		t.Error("the merge was left in progress")
	}
	if st, _ := readStatus(context.Background(), tk.Worktree.Path); len(st.Changed) != 0 {
		t.Errorf("worktree left with changes: %v", st.Changed)
	}
	if fake.count("agent", "prompt") != 0 || slices.Contains(events(t, r, tk.ID), "synced") {
		t.Error("a conflicting sync told the runner or recorded synced")
	}
	if note := SyncConflictNote(res.Ref, res.SHA, res.Conflicts); !strings.Contains(note, "conflicts in a.txt") {
		t.Errorf("note = %q", note)
	}
}
