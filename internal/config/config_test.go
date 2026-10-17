package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Kinds["code"].Agent != "claude:claude-sonnet-5-5:medium" || !cfg.Kinds["code"].Worktree {
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
    mcp: true
telegram:
  notify: [needs-human]
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Kinds["fix"]; got.Agent != "claude:claude-sonnet-5-5:medium" || !got.Worktree {
		t.Errorf("fix = %+v, want default agent with worktree on", got)
	}
	if got := cfg.Kinds["review"]; got.Agent != "codex::high" || got.Worktree || !got.MCP {
		t.Errorf("review = %+v", got)
	}
	if got := cfg.Kinds["code"]; got.Agent != "claude:claude-sonnet-5-5:medium" || got.MCP {
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

func TestParseKeepFor(t *testing.T) {
	if cfg := Default(); cfg.KeepFor != 4*time.Hour {
		t.Errorf("default keepFor = %s", cfg.KeepFor)
	}
	for in, want := range map[string]time.Duration{"keepFor: 90m\n": 90 * time.Minute, "keepFor: 0\n": 0, "keepFor: \"0\"\n": 0} {
		cfg, err := Parse([]byte(in))
		if err != nil || cfg.KeepFor != want {
			t.Errorf("Parse(%q) = %s, %v", in, cfg.KeepFor, err)
		}
	}
}

func TestParseContextHandoff(t *testing.T) {
	if cfg := Default(); !cfg.ContextHandoff || cfg.ContextHandoffAt != 15 {
		t.Errorf("default contextHandoff = %v at %d", cfg.ContextHandoff, cfg.ContextHandoffAt)
	}
	cfg, err := Parse([]byte("contextHandoff: false\ncontextHandoffAt: 20\n"))
	if err != nil || cfg.ContextHandoff || cfg.ContextHandoffAt != 20 {
		t.Errorf("Parse = %v at %d, %v", cfg.ContextHandoff, cfg.ContextHandoffAt, err)
	}
}

func TestParseStallAfter(t *testing.T) {
	if cfg := Default(); cfg.StallAfter != 10*time.Minute {
		t.Errorf("default stallAfter = %s", cfg.StallAfter)
	}
	for in, want := range map[string]time.Duration{"stallAfter: 2m\n": 2 * time.Minute, "stallAfter: 0\n": 0} {
		cfg, err := Parse([]byte(in))
		if err != nil || cfg.StallAfter != want {
			t.Errorf("Parse(%q) = %s, %v", in, cfg.StallAfter, err)
		}
	}
}

func TestParseRunnerMemoryMax(t *testing.T) {
	if cfg := Default(); cfg.RunnerMemoryMax != "25%" {
		t.Errorf("default runnerMemoryMax = %q", cfg.RunnerMemoryMax)
	}
	for _, v := range []string{"25%", "100%", "16G", "8192M", "512M", "off"} {
		cfg, err := Parse([]byte("runnerMemoryMax: " + v + "\n"))
		if err != nil || cfg.RunnerMemoryMax != v {
			t.Errorf("Parse(%q) = %q, %v", v, cfg.RunnerMemoryMax, err)
		}
	}
	for _, v := range []string{"16GB", "-1", "110%", "0%", "0G", "16", "16g", "''", "infinity"} {
		if _, err := Parse([]byte("runnerMemoryMax: " + v + "\n")); err == nil || !strings.Contains(err.Error(), "runnerMemoryMax") {
			t.Errorf("Parse(%q) error = %v", v, err)
		}
	}
}

func TestParseHeavySlots(t *testing.T) {
	if cfg := Default(); cfg.HeavySlots != 2 {
		t.Errorf("default heavySlots = %d", cfg.HeavySlots)
	}
	for v, want := range map[string]int{"0": 0, "1": 1, "64": 64} {
		cfg, err := Parse([]byte("heavySlots: " + v + "\n"))
		if err != nil || cfg.HeavySlots != want {
			t.Errorf("Parse(%s) = %d, %v", v, cfg.HeavySlots, err)
		}
	}
	for _, v := range []string{"-1", "65", "two"} {
		if _, err := Parse([]byte("heavySlots: " + v + "\n")); err == nil || !strings.Contains(err.Error(), "heavySlots") && !strings.Contains(err.Error(), "cannot unmarshal") {
			t.Errorf("Parse(%q) error = %v", v, err)
		}
	}
}

func TestParseRunnerInput(t *testing.T) {
	if cfg := Default(); cfg.RunnerInput != InputLocked {
		t.Errorf("default runnerInput = %q, want locked", cfg.RunnerInput)
	}
	for _, v := range []string{"locked", "open"} {
		cfg, err := Parse([]byte("runnerInput: " + v + "\n"))
		if err != nil || cfg.RunnerInput != v {
			t.Errorf("Parse(%s) = %q, %v", v, cfg.RunnerInput, err)
		}
	}
	for _, v := range []string{`""`, "closed", "true"} {
		if _, err := Parse([]byte("runnerInput: " + v + "\n")); err == nil || !strings.Contains(err.Error(), "runnerInput") {
			t.Errorf("Parse(%s) error = %v", v, err)
		}
	}
}

func TestParseWorktreeSetup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg, err := Parse([]byte(`
worktreeSetup:
  ~/Work/app/:
    - cp "$HRK_REPO_ROOT/.env" .env
    - npm ci
  /srv/api:
    - make deps
`))
	if err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(home, "Work", "app")
	if got := cfg.SetupFor(app); !slices.Equal(got, []string{`cp "$HRK_REPO_ROOT/.env" .env`, "npm ci"}) {
		t.Errorf("app setup = %q", got)
	}
	if got := cfg.SetupFor("/srv/api"); !slices.Equal(got, []string{"make deps"}) {
		t.Errorf("api setup = %q", got)
	}
	if cfg.SetupFor("/srv/other") != nil || cfg.SetupFor("") != nil {
		t.Error("a repository without an entry got setup commands")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(app, link); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(app, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.WorktreeSetup = map[string][]string{link: {"true"}}
	if got := cfg.SetupFor(app); !slices.Equal(got, []string{"true"}) {
		t.Errorf("a symlinked key did not match its target: %q", got)
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
		{"kinds:\n  x:\n    agent: pi\n", "pi needs a model"},
		{"quotaFallback:\n  - agent: omp\n", "omp needs a model"},
		{"quotaFallback:\n  - claudeConfig: claude2\n", "quotaFallback[0].agent is required"},
		{"quotaFallback:\n  - agent: codex\n    claudeConfig: claude2\n", "needs a Claude agent"},
		{"quotaFallback:\n  - agent: codex\n    model: x\n", "unknown key model"},
		{"keepFor: soon\n", "keepFor"},
		{"keepFor: -1h\n", "keepFor must not be negative"},
		{"stallAfter: 90s\n", "stallAfter must be 0 (off) or at least 2m0s"},
		{"stallAfter: often\n", "stallAfter"},
		{"contextHandoffAt: 4\n", "contextHandoffAt must be from 5 to 50"},
		{"contextHandoffAt: 51\n", "contextHandoffAt must be from 5 to 50"},
		{"worktreeSetup:\n  Work/app:\n    - npm ci\n", `worktreeSetup: "Work/app" is not an absolute path`},
		{"worktreeSetup:\n  /srv/app: []\n", "worktreeSetup./srv/app: list at least one command"},
		{"worktreeSetup:\n  /srv/app:\n    - \" \"\n", "worktreeSetup./srv/app[0] is empty"},
		{"worktreeSetup:\n  /srv/app: npm ci\n", "cannot unmarshal"},
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
