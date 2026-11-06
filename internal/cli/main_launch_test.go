package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/harness"
	"github.com/khuebt/herdr-runner-kit/internal/herdr"
	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func mainLaunchEnv(t *testing.T) *state.Store {
	t.Helper()
	root := filepath.Join(t.TempDir(), "main space's")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	for key, value := range map[string]string{
		"HRK_STATE_DIR": filepath.Join(root, "state"), "XDG_CONFIG_HOME": filepath.Join(root, "config"),
		"HOME": root, "HRK_TASK": "", "HRK_MAIN": "", runner.MainOwnerEnv: "", "HERDR_PANE_ID": "", "HRK_TELEGRAM": "off",
		"CLAUDE_CONFIG_DIR": filepath.Join(root, "absent-claude"), "CLAUDE_CODE_SESSION_ID": "inherited-claude",
	} {
		t.Setenv(key, value)
	}
	s, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func printMain(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out strings.Builder
	cmd := newMainCommand()
	cmd.SetArgs(args)
	cmd.SetOut(&out)
	cmd.SetErr(&strings.Builder{})
	err := cmd.Execute()
	return out.String(), err
}

func TestMainExtensionPrint(t *testing.T) {
	for _, agent := range []string{"pi", "omp", "pi:ollama/qwen3:8b:high", "omp:provider/model:auto"} {
		t.Run(agent, func(t *testing.T) {
			s := mainLaunchEnv(t)
			t.Setenv("TELEGRAM_BOT_TOKEN", "test-token-must-not-be-printed")
			out, err := printMain(t, "--agent", agent)
			if err != nil {
				t.Fatal(err)
			}
			spec, _ := harness.ParseMainSpec(agent)
			for _, want := range []string{"HRK_MAIN=1 HRK_MAIN_OWNER=", "HRK_MAIN_HARNESS=" + string(spec.Harness), "HRK_MAIN_NONCE=", "HRK_MAIN_INTERACTIVE=1", "hrk-main.ts", "--append-system-prompt", "SKILL.md", "main-transcripts", "unset CLAUDE_CODE_SESSION_ID"} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q in %s", want, out)
				}
			}
			for _, bad := range []string{"test-token-must-not-be-printed", "--yolo", "--dangerously", "omp-runner.yml", "--channels", "--settings"} {
				if strings.Contains(out, bad) {
					t.Errorf("unexpected %q", bad)
				}
			}
			if _, err := os.Stat(os.Getenv("CLAUDE_CONFIG_DIR")); !os.IsNotExist(err) {
				t.Fatalf("Claude setup touched: %v", err)
			}
			files, err := filepath.Glob(filepath.Join(s.Dir(), "main-transcripts", "*.jsonl"))
			if err != nil || len(files) != 1 {
				t.Fatalf("files %v: %v", files, err)
			}
			info, err := os.Stat(files[0])
			if err != nil || info.Size() != 0 || info.Mode().Perm() != 0o600 {
				t.Fatalf("bootstrap file: %v %v", info, err)
			}
			sessions, err := s.TypedMainSessions()
			if err != nil || len(sessions) != 0 {
				t.Fatalf("CLI registered before runtime header: %v %v", sessions, err)
			}
			assets, _ := filepath.Glob(filepath.Join(s.Dir(), "main-runtime", "*", "hrk-main.ts"))
			if len(assets) != 1 {
				t.Fatalf("assets: %v", assets)
			}
			for _, name := range []string{"hrk-main.ts", "SKILL.md", "references/commands.md"} {
				if _, err := os.Stat(filepath.Join(filepath.Dir(assets[0]), name)); err != nil {
					t.Fatal(err)
				}
			}
			// Parse through a real shell. Replace only the TTY check/exec script with an
			// argv dumper, leaving all emitted argument and environment quoting intact.
			capture := filepath.Join(t.TempDir(), "capture")
			if err := os.WriteFile(capture, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			shell := strings.Replace(out, harness.ShellQuote(mainInteractiveShell), harness.ShellQuote(`exec "$@"`), 1)
			prefix := " " + harness.ShellQuote(files[0]) + " "
			shell = strings.Replace(shell, prefix+string(spec.Harness)+" ", prefix+harness.ShellQuote(capture)+" ", 1)
			got, err := exec.Command("sh", "-c", shell).Output()
			if err != nil {
				t.Fatal(err)
			}
			argv := strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
			want := harness.MainArgs(spec, assets[0], filepath.Join(filepath.Dir(assets[0]), "SKILL.md"), files[0])[1:]
			if !slices.Equal(argv, want) {
				t.Fatalf("argv = %q, want %q", argv, want)
			}
		})
	}
}

func TestMainReserveTranscript(t *testing.T) {
	dir := t.TempDir()
	path, cleanup, err := reserveMainTranscript(dir, "collision")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reserveMainTranscript(dir, "collision"); err == nil {
		t.Fatal("adopted existing empty file")
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("empty failed launch retained: %v", err)
	}
	path, cleanup, err = reserveMainTranscript(dir, "written")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("runtime-owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanup()
	if data, err := os.ReadFile(path); err != nil || string(data) != "runtime-owned" {
		t.Fatalf("deleted runtime output: %q %v", data, err)
	}
}

func registeredMain(t *testing.T, s *state.Store, h task.Harness, id, owner, cwd string, at time.Time) state.MainSession {
	t.Helper()
	file := filepath.Join(t.TempDir(), "session file's.jsonl")
	header, _ := json.Marshal(map[string]any{"type": "session", "version": 3, "id": id, "cwd": cwd})
	if err := os.WriteFile(file, append(header, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	m := state.MainSession{Harness: h, ID: id, Owner: owner, Cwd: cwd, Transcript: file, UpdatedAt: at}
	if err := s.RecordTypedMainSession(m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMainResumeSelection(t *testing.T) {
	s := mainLaunchEnv(t)
	cwd, _ := os.Getwd()
	now := time.Now()
	older := registeredMain(t, s, task.Pi, "shared", cwd, cwd, now.Add(-time.Minute))
	newer := registeredMain(t, s, task.Pi, "new", cwd, cwd, now)
	_ = registeredMain(t, s, task.Omp, "shared", cwd, cwd, now.Add(time.Minute))
	_ = registeredMain(t, s, task.Pi, "wrong-owner", t.TempDir(), cwd, now.Add(2*time.Minute))
	_ = registeredMain(t, s, task.Pi, "wrong-cwd", cwd, t.TempDir(), now.Add(3*time.Minute))
	for _, tc := range []struct {
		name string
		args []string
		pane herdr.PaneSession
		want string
	}{
		{name: "newest", want: newer.Transcript},
		{name: "explicit id", args: []string{older.ID}, want: older.Transcript},
		{name: "explicit path", args: []string{older.Transcript}, want: older.Transcript},
		{name: "explicit precedes pane", args: []string{newer.ID}, pane: herdr.PaneSession{Agent: "pi", Kind: "id", Value: older.ID}, want: newer.Transcript},
		{name: "pane id", pane: herdr.PaneSession{Agent: "pi", Kind: "id", Value: older.ID}, want: older.Transcript},
		{name: "pane path", pane: herdr.PaneSession{Agent: "pi", Kind: "path", Value: older.Transcript}, want: older.Transcript},
		{name: "wrong harness pane", pane: herdr.PaneSession{Agent: "omp", Kind: "id", Value: older.ID}, want: newer.Transcript},
		{name: "stale pane", pane: herdr.PaneSession{Agent: "pi", Kind: "id", Value: "gone"}, want: newer.Transcript},
		{name: "id cannot be path", pane: herdr.PaneSession{Agent: "pi", Kind: "id", Value: older.Transcript}, want: newer.Transcript},
		{name: "path cannot be id", pane: herdr.PaneSession{Agent: "pi", Kind: "path", Value: older.ID}, want: newer.Transcript},
		{name: "wrong owner", args: []string{"wrong-owner"}},
		{name: "wrong cwd", args: []string{"wrong-cwd"}},
		{name: "missing id", args: []string{"missing"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveMainResume(s, task.Pi, cwd, cwd, tc.args, tc.pane)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("accepted %q", got)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("%q %v, want %q", got, err, tc.want)
			}
		})
	}
	// A plausible external header never makes an unregistered file resumable.
	external := filepath.Join(t.TempDir(), "external.jsonl")
	data, _ := os.ReadFile(older.Transcript)
	if err := os.WriteFile(external, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveMainResume(s, task.Pi, cwd, cwd, []string{external}, herdr.PaneSession{}); err == nil {
		t.Fatal("adopted external file")
	}
	if err := os.Remove(newer.Transcript); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveMainResume(s, task.Pi, cwd, cwd, nil, herdr.PaneSession{}); err == nil {
		t.Fatal("resumed missing newest file")
	}
	if _, err := os.Stat(newer.Transcript); !os.IsNotExist(err) {
		t.Fatal("recreated missing file")
	}
	if err := os.WriteFile(older.Transcript, []byte(`{"type":"session","version":3,"id":"wrong","cwd":"/wrong"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveMainResume(s, task.Pi, cwd, cwd, []string{older.ID}, herdr.PaneSession{}); err == nil {
		t.Fatal("accepted mismatching header")
	}
}

func TestMainResumeRebuildsAssetsAndKeepsOwner(t *testing.T) {
	s := mainLaunchEnv(t)
	cwd, _ := os.Getwd()
	owner := t.TempDir()
	t.Setenv(runner.MainOwnerEnv, owner)
	if err := s.RecordMainTranscript("inherited-claude", "", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	m := registeredMain(t, s, task.Omp, "main", owner, cwd, time.Now())
	if _, err := s.ReserveProject("demo", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitProject("demo"); err != nil {
		t.Fatal(err)
	}
	out, err := printMain(t, "--agent", "omp:provider/model:high", "--resume", m.ID, "--project", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "HRK_MAIN_OWNER="+harness.ShellQuote(owner)) || !strings.Contains(out, "--resume "+harness.ShellQuote(m.Transcript)) || !strings.Contains(out, "--thinking high") {
		t.Fatalf("wrong resume: %s", out)
	}
	files, _ := filepath.Glob(filepath.Join(s.Dir(), "main-transcripts", "*"))
	if len(files) != 0 {
		t.Fatal("allocated file on resume")
	}
	assets, _ := filepath.Glob(filepath.Join(s.Dir(), "main-runtime", "*", "hrk-main.ts"))
	if len(assets) != 1 {
		t.Fatal("missing assets")
	}
	if err := os.Remove(assets[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := printMain(t, "--agent", "omp", "--resume", m.Transcript); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.MainProjects(owner), []string{"demo"}) || len(s.MainProjects(cwd)) != 0 {
		t.Fatal("resume lost attachments or attached them to the shell cwd")
	}
	if _, err := os.Stat(assets[0]); err != nil {
		t.Fatal("resume did not restore asset", err)
	}
	if got := runner.MainOwner(s, func(key string) string {
		if key == "CLAUDE_CODE_SESSION_ID" {
			return ""
		}
		return os.Getenv(key)
	}); got != owner {
		t.Fatalf("owner = %s", got)
	}
}

func TestMainRejectsInvalidSpecAndMissingBinary(t *testing.T) {
	mainLaunchEnv(t)
	for _, agent := range []string{"codex", "pi::high", "omp::high", "pi:model:auto", "unknown"} {
		if _, err := printMain(t, "--agent", agent); err == nil {
			t.Fatalf("accepted %s", agent)
		}
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := printMain(t, "--agent", "pi", "--exec"); err == nil || !strings.Contains(err.Error(), "pi is not on PATH") {
		t.Fatalf("missing binary: %v", err)
	}
}

func TestMainRejectsNonTTY(t *testing.T) {
	mainLaunchEnv(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "pi"), []byte("#!/bin/sh\nexit 99\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "test -t 0 && test -t 1")
	cmd.Stdin, cmd.Stdout = os.Stdin, os.Stdout
	if cmd.Run() == nil {
		t.Skip("test process has a terminal")
	}
	if _, err := printMain(t, "--agent", "pi", "--exec"); err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("non-TTY: %v", err)
	}
}

func TestMainLaunchKeepsLegacyRegisteredOwner(t *testing.T) {
	s := mainLaunchEnv(t)
	owner := t.TempDir()
	if err := s.RecordMainTranscript("inherited-claude", "", owner); err != nil {
		t.Fatal(err)
	}
	out, err := printMain(t, "--agent", "pi")
	if err != nil || !strings.Contains(out, "HRK_MAIN_OWNER="+harness.ShellQuote(owner)) {
		t.Fatalf("legacy owner lost: %s %v", out, err)
	}
}

func TestMainResumeRelativeFileAndAmbiguity(t *testing.T) {
	s := mainLaunchEnv(t)
	cwd, _ := os.Getwd()
	m := registeredMain(t, s, task.Pi, "id", cwd, cwd, time.Now())
	relative := "session.jsonl"
	file := filepath.Join(cwd, relative)
	data, err := os.ReadFile(m.Transcript)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	m.Transcript = file
	if err := s.RecordTypedMainSession(m); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveMainResume(s, task.Pi, cwd, cwd, []string{relative}, herdr.PaneSession{}); err != nil || got != file {
		t.Fatalf("relative file: %q %v", got, err)
	}
	_ = registeredMain(t, s, task.Pi, relative, cwd, cwd, time.Now())
	if _, err := resolveMainResume(s, task.Pi, cwd, cwd, []string{relative}, herdr.PaneSession{}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous ref: %v", err)
	}
	if _, err := resolveMainResume(s, task.Pi, cwd, cwd, []string{""}, herdr.PaneSession{}); err == nil {
		t.Fatal("empty explicit ref selected newest")
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), "main-sessions.json"), []byte(`{"version":999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveMainResume(s, task.Pi, cwd, cwd, []string{file}, herdr.PaneSession{}); err == nil {
		t.Fatal("ignored corrupt registry")
	}
}

func TestMainPrintedFailureCleansOnlyNewEmptyTranscript(t *testing.T) {
	s := mainLaunchEnv(t)
	out, err := printMain(t, "--agent", "pi")
	if err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("sh", "-c", out).Run(); err == nil {
		t.Fatal("printed non-TTY launch succeeded")
	}
	files, _ := filepath.Glob(filepath.Join(s.Dir(), "main-transcripts", "*.jsonl"))
	if len(files) != 0 {
		t.Fatal("failed printed launch kept empty reservation")
	}
	cwd, _ := os.Getwd()
	m := registeredMain(t, s, task.Pi, "resume", cwd, cwd, time.Now())
	out, err = printMain(t, "--agent", "pi", "--resume", m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("sh", "-c", out).Run(); err == nil {
		t.Fatal("printed non-TTY resume succeeded")
	}
	if _, err := os.Stat(m.Transcript); err != nil {
		t.Fatal("failed resume removed existing transcript", err)
	}
}
