package cli

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/config"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func TestOptional(t *testing.T) {
	f := fail("omp is not installed", "install omp")
	if r := optional(f, false); r.level != "warn" || r.fix != f.fix || r.detail != f.detail+" (not used by your config)" {
		t.Errorf("unused fail = %+v", r)
	}
	if r := optional(f, true); r != f {
		t.Errorf("used fail = %+v", r)
	}
	if r := optional(ok("pi 1.2.0"), false); r != ok("pi 1.2.0") {
		t.Errorf("unused ok = %+v", r)
	}
}

func TestConfigHarnesses(t *testing.T) {
	cfg := config.Default()
	cfg.Kinds["code"] = config.Kind{Agent: "codex"}
	cfg.QuotaFallback = []config.Fallback{{Agent: "omp:openai-codex/gpt-6-luna"}}
	got := configHarnesses(cfg)
	want := map[task.Harness]bool{task.Codex: true, task.Claude: true, task.Omp: true}
	if !maps.Equal(got, want) {
		t.Errorf("configHarnesses = %v, want %v", got, want)
	}
	if got := configHarnesses(config.Config{}); len(got) != 0 {
		t.Errorf("zero config = %v", got)
	}
}

func TestDoctorClaudeTrust(t *testing.T) {
	home, _, c2 := setupEnv(t)
	repo := filepath.Join(home, "repo")
	sub := filepath.Join(repo, "pkg")

	// Missing file: Claude has never run with this folder.
	r := checkClaudeConfig(c2, sub, repo)
	if r.level != "fail" || !strings.Contains(r.detail, "Claude has never run with "+c2) ||
		r.fix != "run `CLAUDE_CONFIG_DIR="+c2+" claude` once in "+repo+", log in and accept the trust dialog" {
		t.Errorf("missing = %+v", r)
	}
	// The default folder needs no env prefix.
	if r := checkClaudeConfig("", sub, repo); r.level != "fail" || !strings.HasPrefix(r.fix, "run `claude` once in "+repo) {
		t.Errorf("missing default = %+v", r)
	}
	if _, err := os.Stat(filepath.Join(c2, ".claude.json")); err == nil {
		t.Error("doctor created .claude.json")
	}

	// Unreadable file.
	if err := os.MkdirAll(c2, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(c2, ".claude.json")
	if err := os.WriteFile(cfg, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := checkClaudeConfig(c2, sub, repo); r.level != "fail" || !strings.Contains(r.detail, "unreadable") {
		t.Errorf("unreadable = %+v", r)
	}

	// Present but this repository is untrusted: information only.
	if err := os.WriteFile(cfg, []byte(`{"projects":{"/elsewhere":{"hasTrustDialogAccepted":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r = checkClaudeConfig(c2, sub, repo)
	if r.level != "ok" || !strings.Contains(r.detail, "not trusted") || !strings.Contains(r.detail, "the kit marks each runner's folder trusted at launch") {
		t.Errorf("untrusted = %+v", r)
	}

	// Trusted at the repository root, found from a subfolder by exact key.
	if err := os.WriteFile(cfg, []byte(`{"projects":{"`+repo+`":{"hasTrustDialogAccepted":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r = checkClaudeConfig(c2, sub, repo)
	if r.level != "ok" || !strings.Contains(r.detail, repo+" is trusted") || !strings.Contains(r.detail, `projects["`+repo+`"].hasTrustDialogAccepted`) {
		t.Errorf("trusted = %+v", r)
	}
}

func TestTrustCandidates(t *testing.T) {
	for _, tt := range []struct {
		cwd, repo string
		want      []string
	}{
		{"/r/a/b", "/r", []string{"/r/a/b", "/r/a", "/r"}},
		{"/r", "/r", []string{"/r"}},
		{"/x", "", []string{"/x"}},
		{"/rx/a", "/r", []string{"/rx/a", "/r"}},
	} {
		if got := trustCandidates(tt.cwd, tt.repo); !slices.Equal(got, tt.want) {
			t.Errorf("trustCandidates(%q, %q) = %v, want %v", tt.cwd, tt.repo, got, tt.want)
		}
	}
}

func TestDoctorChecksTaskClaudeConfigs(t *testing.T) {
	home, c1, c2 := setupEnv(t)
	c3 := filepath.Join(home, ".claude3")
	for _, d := range []string{c2, c3} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	tasks := map[string]string{
		"t1": `{"agent":"claude","claudeConfigDir":"` + c3 + `"}`,
		"t2": `{"agent":"claude:opus","claudeConfigDir":"` + c3 + `"}`,
		"t3": `{"agent":"codex","claudeConfigDir":"` + c2 + `"}`,
		"t4": `{"agent":"claude","claudeConfigDir":"` + filepath.Join(home, ".gone") + `"}`,
		"t5": `{"agent":"claude"}`,
	}
	for id, body := range tasks {
		dir := filepath.Join(home, "state", "tasks", id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "task.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := runnerClaudeConfigs([]string{c1})
	// ~/.claude and a default task both map to "", Claude's default.
	if len(got) != 2 || !slices.Contains(got, "") || !slices.Contains(got, c3) {
		t.Errorf("configs = %q, want \"\" and %s", got, c3)
	}
}

func TestNotOnPathFix(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if got := notOnPathFix("/opt/kit/herdr-runner-kit"); got != "ln -sf herdr-runner-kit /opt/kit/hrk, and keep /opt/kit on PATH" {
		t.Errorf("old name: %q", got)
	}
	if got := notOnPathFix("/opt/kit/hrk"); !strings.HasPrefix(got, "put /opt/kit on PATH") {
		t.Errorf("new name: %q", got)
	}
}

func TestStaleAlias(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "hrk")
	alias := filepath.Join(dir, "herdr-runner-kit")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if old, _ := staleAlias(bin); old != "" {
		t.Fatalf("no alias, yet %q", old)
	}
	if err := os.Symlink("hrk", alias); err != nil {
		t.Fatal(err)
	}
	if old, _ := staleAlias(bin); old != "" {
		t.Fatalf("alias to this binary reported as stale: %q", old)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(alias, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if old, resolved := staleAlias(bin); old != alias || resolved != alias {
		t.Fatalf("stale alias: %q %q", old, resolved)
	}
}

func TestMemoryCapCheck(t *testing.T) {
	ctx := t.Context()
	if r := memoryCapCheck("25%", nil); r != ok("%s", "25% per runner (systemd scope)") {
		t.Errorf("usable = %+v", r)
	}
	if r := memoryCapCheck("25%", scopeProblem(ctx, "25%", "darwin")); r != ok("not available on macOS; runners share herdr's memory") {
		t.Errorf("macOS = %+v", r)
	}
	if r := memoryCapCheck("off", scopeProblem(ctx, "off", "linux")); r != ok("off; runners share herdr's memory") {
		t.Errorf("off = %+v", r)
	}
	t.Setenv("PATH", t.TempDir())
	r := memoryCapCheck("16G", scopeProblem(ctx, "16G", "linux"))
	if r.level != "warn" || r.detail != "systemd-run is not on PATH; runners share herdr's memory" || !strings.Contains(r.fix, "runnerMemoryMax: off") {
		t.Errorf("no systemd-run = %+v", r)
	}
}

func TestMemoryScopeOff(t *testing.T) {
	if got := memoryScope("off")(t.Context()); got != "" {
		t.Errorf("off scope = %q", got)
	}
}
