package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Kinds["code"].Agent != "codex" || !cfg.Kinds["code"].Worktree {
		t.Errorf("code kind = %+v", cfg.Kinds["code"])
	}
	if cfg.WorktreeBranchPrefix != "hrk/" || cfg.DefaultKind != "code" {
		t.Errorf("defaults = %+v", cfg)
	}
}

func TestDefaultsAreValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestParseMergesOverDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`
schemaVersion: 1
kinds:
  fix:
    worktree: true
  review:
    agent: codex::high
telegram:
  notify: [needs-human]
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Kinds["fix"]; got.Agent != "claude:claude-sonnet-5-5:medium" || !got.Worktree {
		t.Errorf("fix = %+v, want default agent with worktree on", got)
	}
	if got := cfg.Kinds["review"]; got.Agent != "codex::high" || got.Worktree {
		t.Errorf("review = %+v", got)
	}
	if got := cfg.Kinds["code"]; got.Agent != "codex" {
		t.Errorf("code = %+v, want untouched default", got)
	}
	if len(cfg.Telegram.Notify) != 1 || cfg.Telegram.Notify[0] != "needs-human" {
		t.Errorf("notify = %v", cfg.Telegram.Notify)
	}
}

func TestParseLanguage(t *testing.T) {
	if cfg, err := Parse(nil); err != nil || cfg.Language != "" {
		t.Fatalf("default language = %q, %v", cfg.Language, err)
	}
	cfg, err := Parse([]byte("language: \" Tiếng Việt \"\n"))
	if err != nil || cfg.Language != "Tiếng Việt" {
		t.Fatalf("language = %q, %v", cfg.Language, err)
	}
	if err := CheckLanguage(strings.Repeat("ệ", 40)); err != nil {
		t.Errorf("40 characters refused: %v", err)
	}
}

func TestParseQuotaFallback(t *testing.T) {
	if cfg, err := Parse(nil); err != nil || cfg.QuotaFallback != nil {
		t.Fatalf("default quotaFallback = %v, %v", cfg.QuotaFallback, err)
	}
	cfg, err := Parse([]byte(`
quotaFallback:
  - agent: codex::high
  - agent: claude:claude-sonnet-5-5
    claudeConfig: claude2
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Fallback{{Agent: "codex::high"}, {Agent: "claude:claude-sonnet-5-5", ClaudeConfig: "claude2"}}
	if !slices.Equal(cfg.QuotaFallback, want) {
		t.Errorf("quotaFallback = %+v", cfg.QuotaFallback)
	}
}

func TestParseEmptyFile(t *testing.T) {
	if _, err := Parse(nil); err != nil {
		t.Fatal(err)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct{ yaml, want string }{
		{"kinds:\n  code:\n    agnet: codex\n", `unknown key agnet`},
		{"telegrm: {}\n", `unknown key telegrm`},
		{"kinds:\n  code:\n    agent: gemini\n", "kinds.code.agent"},
		{"kinds:\n  new:\n    worktree: true\n", "kinds.new.agent is required"},
		{"defaultKind: nope\n", `defaultKind "nope"`},
		{"telegram:\n  notify: [everything]\n", `unknown event "everything"`},
		{"worktreeBranchPrefix: \"a b/\"\n", "worktreeBranchPrefix"},
		{"schemaVersion: 2\n", "upgrade herdr-runner-kit"},
		{"language: \"" + strings.Repeat("x", 41) + "\"\n", "language"},
		{"language: \"English\\nand more\"\n", "language"},
		{"quotaFallback:\n  - agent: gemini\n", "quotaFallback[0].agent"},
		{"quotaFallback:\n  - claudeConfig: claude2\n", "quotaFallback[0].agent is required"},
		{"quotaFallback:\n  - agent: codex\n    claudeConfig: claude2\n", "needs a Claude agent"},
		{"quotaFallback:\n  - agent: codex\n    model: x\n", "unknown key model"},
	}
	for _, tt := range tests {
		_, err := Parse([]byte(tt.yaml))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Parse(%q) error = %v, want it to contain %q", tt.yaml, err, tt.want)
		}
		if err != nil && strings.Contains(err.Error(), "fileKind") {
			t.Errorf("error leaks an internal type name: %v", err)
		}
	}
}

func TestLoadNamesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("bogus: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "unknown key bogus") {
		t.Fatalf("Load error = %v", err)
	}
}

func TestPathHonoursXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/x/cfg")
	got, err := Path()
	if err != nil || got != "/x/cfg/herdr-runner-kit/config.yaml" {
		t.Errorf("Path() = %q, %v", got, err)
	}
}
