package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

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
