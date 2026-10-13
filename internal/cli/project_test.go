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
