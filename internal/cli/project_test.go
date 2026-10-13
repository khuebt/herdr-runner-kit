package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/install"
	"github.com/khuebt/herdr-runner-kit/internal/project"
	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func runCLI(args ...string) (string, error) {
	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

// TestProjectGuard lists every command that changes a project: each must
// refuse to run inside a runner before it touches any state.
func TestProjectGuard(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	t.Setenv("HRK_STATE_DIR", stateDir)
	t.Setenv("HRK_TASK", "t1")
	for _, args := range [][]string{
		{"project", "register", t.TempDir()},
		{"project", "forget", "demo-app"},
		{"project", "forget", "demo-app", "--yes"},
		{"new", filepath.Join(t.TempDir(), "app"), "--stdin"},
	} {
		if _, err := runCLI(args...); !errors.Is(err, project.ErrInsideRunner) {
			t.Errorf("%v inside a runner: err = %v", args, err)
		}
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Errorf("a refused command touched state: %v", err)
	}
}

func TestProjectRegisterAndForget(t *testing.T) {
	t.Setenv("HRK_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv("HRK_TASK", "")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	out, err := runCLI("project", "register", dir, "--name", "demo-app")
	if err != nil || out != "registered demo-app at "+dir+" (unknown, standard track); its runners report to project:demo-app\n" {
		t.Fatalf("register: %q %v", out, err)
	}
	if _, err := runCLI("project", "register", dir, "--name", "again"); err == nil {
		t.Fatal("registered the same folder twice")
	}
	if out, err := runCLI("project", "forget", "demo-app"); err != nil || !strings.HasPrefix(out, "forgot demo-app") {
		t.Fatalf("forget: %q %v", out, err)
	}
	if _, err := runCLI("project", "forget", "demo-app"); err == nil {
		t.Fatal("forgot an unknown project")
	}
}

func TestNewProjectFromStdin(t *testing.T) {
	t.Setenv("HRK_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv("HRK_TASK", "")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	// herdr is down: the project is still created, with a warning.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "herdr"), []byte("#!/bin/sh\necho 'herdr is not running' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "demo-app")

	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader("A small tool that tracks reading lists.\n"))
	root.SetArgs([]string{"new", path, "--stdin", "--remote", "git@example.com:team/demo-app.git"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("new: %v\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{
		"created demo-app at " + path + " (commit ",
		", branch main)\n",
		"remote: origin git@example.com:team/demo-app.git\n",
		"workspace: not created: ",
		"brief: original kept untracked at " + filepath.Join(path, project.BriefDir, project.IdeaFile) + "\n",
		"next: herdr-runner-kit project status demo-app\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if _, err := runCLI("new", filepath.Join(dir, "other")); err == nil || !strings.Contains(err.Error(), "--brief FILE or --stdin") {
		t.Errorf("new without a brief: %v", err)
	}
}

func TestAutoarmWakesForProjectNews(t *testing.T) {
	r := testRunner(t)
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.ReserveProject("demo-app", repo); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.CommitProject("demo-app"); err != nil {
		t.Fatal(err)
	}
	tk := &task.Task{
		Kind: "fix", Agent: "claude", Prompt: "x", Title: "write spec", Owner: task.ProjectOwner("demo-app"),
		Project: "demo-app", Gate: task.GateAuto, Status: task.Running,
	}
	tk.Herdr.PaneID = "w1:p1"
	if err := r.Store.Create(tk); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = r.Report(tk.ID, runner.ReportOptions{Status: task.Done, Summary: "Spec written"})
	}()
	var stderr strings.Builder
	// The main session works in another folder that never spawned a task.
	if code := autoarm(context.Background(), r, "/main", &stderr); code != install.NewsExit ||
		!strings.Contains(stderr.String(), tk.ID+" done · write spec") {
		t.Fatalf("code %d, %q", code, stderr.String())
	}
}
