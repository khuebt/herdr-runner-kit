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
		{"project", "run", "spec"},
		{"project", "run", "ux", "--project", "demo-app", "--stdin"},
		{"project", "share", "stitch", "on"},
		{"project", "share", "lavish-hosted", "off", "--project", "demo-app"},
		{"project", "run", "pack", "--docx"},
		{"project", "approve", "spec"},
		{"project", "approve", "spec", "--waive-pack", "--by", "Lan"},
		{"project", "approve", "pack", "--project", "demo-app"},
		{"project", "changes", "spec", "--stdin"},
		{"project", "changes", "pack", "--stdin", "--project", "demo-app"},
		{"project", "reject", "spec", "--note", "no"},
		{"project", "reject", "pack", "--note", "no", "--by", "Lan"},
		{"project", "decide", "--stdin"},
		{"project", "run", "change", "--stdin"},
		{"project", "cr", "decide", "1", "next"},
		{"project", "cr", "decide", "CR-2", "now", "--blocking", "--project", "demo-app"},
		{"project", "cr", "decide", "3", "reject", "--note", "no", "--by", "Lan"},
		{"project", "backlog", "add", "--stdin"},
		{"project", "dep", "add", "Payments sandbox", "--owner", "vendor"},
		{"project", "dep", "add", "Domain", "--owner", "user", "--secret-location", "registrar account", "--project", "demo-app"},
		{"project", "dep", "set", "D1", "--status", "ready"},
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

func TestAutoarmWakesForAProjectStartedAfterIt(t *testing.T) {
	r := testRunner(t)
	mainTask := &task.Task{Kind: "fix", Agent: "claude", Prompt: "x", Title: "fix login", Owner: "/main", Gate: task.GateAuto, Status: task.Running}
	mainTask.Herdr.PaneID = "w1:p1"
	if err := r.Store.Create(mainTask); err != nil {
		t.Fatal(err)
	}
	type armed struct {
		code   int
		stderr string
	}
	ch := make(chan armed, 1)
	go func() {
		var stderr strings.Builder
		code := autoarm(context.Background(), r, "/main", &stderr)
		ch <- armed{code, stderr.String()}
	}()
	time.Sleep(100 * time.Millisecond)
	// new registers a project while the hook waits; its runner reports.
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
	tk.Herdr.PaneID = "w1:p2"
	if err := r.Store.Create(tk); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Report(tk.ID, runner.ReportOptions{Status: task.Done, Summary: "Spec written"}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-ch:
		if got.code != install.NewsExit || !strings.Contains(got.stderr, tk.ID+" done · write spec") {
			t.Fatalf("code %d, %q", got.code, got.stderr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the project's news never reached the hook")
	}
}

func TestBrokenRegistryKeepsOtherCommandsWorking(t *testing.T) {
	r := testRunner(t)
	if err := os.WriteFile(filepath.Join(r.Store.Dir(), "projects.json"), []byte(`{"schemaVersion": 99, "projects": []}`), 0o600); err != nil {
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
	if code := autoarm(context.Background(), r, "/main", &stderr); code != install.NewsExit ||
		!strings.Contains(stderr.String(), tk.ID+" done") {
		t.Fatalf("code %d, %q", code, stderr.String())
	}
	if line, err := r.RouteTelegram(context.Background(), "status demo-app"); err != nil || !strings.HasPrefix(line, "ambiguous: no task is waiting") {
		t.Fatalf("telegram routing: %q %v", line, err)
	}
}

func TestProjectStatus(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	t.Setenv("HRK_STATE_DIR", stateDir)
	t.Setenv("HRK_TASK", "")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	// herdr is down: status never asks it.
	t.Setenv("PATH", t.TempDir()+string(os.PathListSeparator)+os.Getenv("PATH"))
	elsewhere, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(elsewhere)

	if out, err := runCLI("project", "status"); err != nil || !strings.HasPrefix(out, "no project is registered") {
		t.Fatalf("no projects: %q %v", out, err)
	}
	repos := map[string]string{}
	for _, name := range []string{"demo-app", "demo-cli"} {
		dir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "https://user:secret@example.com/team/" + name + ".git"}} {
			if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v %s", args, err, out)
			}
		}
		if _, err := runCLI("project", "register", dir, "--name", name); err != nil {
			t.Fatal(err)
		}
		repos[name] = dir
	}
	store, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range []*task.Task{
		{
			Kind: "research", Agent: "claude", Prompt: "x", Title: "read brief", Owner: task.ProjectOwner("demo-app"),
			Project: "demo-app", Stage: "intake", Gate: task.GateAuto, Status: task.Running,
		},
		{
			Kind: "research", Agent: "claude", Prompt: "x", Title: "other", Owner: task.ProjectOwner("demo-cli"),
			Project: "demo-cli", Stage: "spec", Gate: task.GateAuto, Status: task.NeedsHuman, Question: "Which?",
		},
	} {
		if err := store.Create(tk); err != nil {
			t.Fatal(err)
		}
	}

	// Two projects and a folder outside both: one line per project.
	out, err := runCLI("project", "status")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "NAME") ||
		strings.Join(strings.Fields(lines[1]), " ") != "demo-app "+repos["demo-app"]+" intake none yet 1" ||
		strings.Join(strings.Fields(lines[2]), " ") != "demo-cli "+repos["demo-cli"]+" spec none yet 1" {
		t.Fatalf("list:\n%s", out)
	}

	// Inside a project, its status; the remote shows no credentials.
	t.Chdir(repos["demo-app"])
	out, err = runCLI("project", "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"demo-app · unknown/standard · " + repos["demo-app"] + "\n",
		"remote: https://example.com/team/demo-app.git\n",
		"stage: intake\n",
		"runners:\n  t1 running · stage intake · read brief\n",
		"waiting on you: none\n",
		"next: wait for the 1 runner(s) at work",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "secret") || strings.Contains(out, "demo-cli") || strings.Contains(out, "t2") {
		t.Errorf("status shows another project or a credential:\n%s", out)
	}

	// A lost record prints the fix.
	if err := os.Remove(filepath.Join(store.ProjectDir("demo-cli"), "record.json")); err != nil {
		t.Fatal(err)
	}
	out, err = runCLI("project", "status", "demo-cli")
	if err != nil || !strings.Contains(out, "record: missing\n") ||
		!strings.Contains(out, "herdr-runner-kit project register "+repos["demo-cli"]+" --name demo-cli") {
		t.Errorf("missing record: %q %v", out, err)
	}
	if _, err := runCLI("project", "status", "nope"); err == nil {
		t.Error("status of an unknown project")
	}
	if _, err := runCLI("events", "--project", "demo-app"); err == nil || !strings.Contains(err.Error(), "--learning") {
		t.Errorf("events --project without --learning: %v", err)
	}
}
