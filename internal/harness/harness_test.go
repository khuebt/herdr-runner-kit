package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func spec(t *testing.T, s string) task.AgentSpec {
	t.Helper()
	sp, err := task.ParseAgentSpec(s)
	if err != nil {
		t.Fatal(err)
	}
	return sp
}

func TestClaudeLaunchArgs(t *testing.T) {
	tests := []struct {
		agent    string
		settings string
		contract string
		want     []string
	}{
		{"claude", "", "", []string{"--dangerously-skip-permissions"}},
		{"claude:claude-opus-5-5", "", "", []string{"--dangerously-skip-permissions", "--model", "claude-opus-5-5"}},
		{"claude::max", "", "", []string{"--dangerously-skip-permissions", "--effort", "max"}},
		{
			"claude:claude-sonnet-5-5:medium", "/s/t3/settings.json", "Report when done.",
			[]string{
				"--dangerously-skip-permissions", "--model", "claude-sonnet-5-5", "--effort", "medium",
				"--settings", "/s/t3/settings.json", "--append-system-prompt", "Report when done.",
			},
		},
	}
	for _, tt := range tests {
		got := claude{}.LaunchArgs(LaunchOptions{Spec: spec(t, tt.agent), SettingsPath: tt.settings, Contract: tt.contract})
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: argv = %q, want %q", tt.agent, got, tt.want)
		}
	}
}

func TestCodexLaunchArgs(t *testing.T) {
	tests := []struct {
		agent string
		bin   string
		want  []string
	}{
		{"codex", "", []string{"--dangerously-bypass-approvals-and-sandbox"}},
		{"codex:gpt-6-astra", "", []string{"--dangerously-bypass-approvals-and-sandbox", "-m", "gpt-6-astra"}},
		{
			"codex:gpt-6-astra:xhigh", "/usr/local/bin/herdr-runner-kit",
			[]string{
				"--dangerously-bypass-approvals-and-sandbox", "-m", "gpt-6-astra",
				"-c", `model_reasoning_effort="xhigh"`,
				"-c", `notify=["/usr/local/bin/herdr-runner-kit","__turn-end","--harness","codex"]`,
			},
		},
	}
	for _, tt := range tests {
		got := codex{}.LaunchArgs(LaunchOptions{Spec: spec(t, tt.agent), Bin: tt.bin, Contract: "ignored"})
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: argv = %q, want %q", tt.agent, got, tt.want)
		}
	}
}

func TestLaunchArgsNeverContainThePrompt(t *testing.T) {
	for _, h := range []task.Harness{task.Claude, task.Codex} {
		a, err := For(h)
		if err != nil {
			t.Fatal(err)
		}
		args := a.LaunchArgs(LaunchOptions{Spec: task.AgentSpec{Harness: h}})
		for _, arg := range args {
			if !strings.HasPrefix(arg, "-") {
				t.Errorf("%s: positional argument %q would be taken as a prompt", h, arg)
			}
		}
	}
	if _, err := For("gemini"); err == nil {
		t.Error("For(gemini) succeeded")
	}
}

func TestNotifyValueIsValidTOML(t *testing.T) {
	args := codex{}.LaunchArgs(LaunchOptions{Spec: task.AgentSpec{Harness: task.Codex}, Bin: `/opt/my "kit"/bin`})
	value := strings.TrimPrefix(args[len(args)-1], "notify=")
	var doc struct{ Notify []string }
	if err := toml.Unmarshal([]byte("notify = "+value), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Notify[0] != `/opt/my "kit"/bin` || doc.Notify[1] != "__turn-end" {
		t.Errorf("notify = %q", doc.Notify)
	}
}

func TestRenderSkill(t *testing.T) {
	tests := []struct {
		h        task.Harness
		in, want string
	}{
		{task.Claude, "ak:plan", "/ak:plan"},
		{task.Claude, "/ak:cook", "/ak:cook"},
		{task.Claude, "$ak-cook", "/ak-cook"},
		{task.Codex, "ak:plan", "$ak-plan"},
		{task.Codex, "/ak:cook", "$ak-cook"},
		{task.Codex, "superpowers:brainstorming", "$superpowers-brainstorming"},
		{task.Codex, "", ""},
		{task.Claude, "  ", ""},
	}
	for _, tt := range tests {
		a, _ := For(tt.h)
		if got := a.RenderSkill(tt.in); got != tt.want {
			t.Errorf("%s RenderSkill(%q) = %q, want %q", tt.h, tt.in, got, tt.want)
		}
	}
}

func TestFirstPrompt(t *testing.T) {
	if got := (claude{}).FirstPrompt("ak:cook", "plans/x/plan.md", "CONTRACT"); got != "/ak:cook plans/x/plan.md" {
		t.Errorf("claude = %q", got)
	}
	if got := (codex{}).FirstPrompt("ak:cook", "plans/x/plan.md", "CONTRACT"); got != "$ak-cook plans/x/plan.md\n\nCONTRACT" {
		t.Errorf("codex = %q", got)
	}
	if got := (codex{}).FirstPrompt("", "fix the login bug", ""); got != "fix the login bug" {
		t.Errorf("raw prompt = %q", got)
	}
}

func TestDetectDialog(t *testing.T) {
	tests := []struct {
		h      task.Harness
		screen string
		name   string
		action DialogAction
	}{
		{task.Claude, " Quick safety check: Is this a project you created or one you trust? (Like your own code", "claude-workspace-trust", NeedsHuman},
		{task.Claude, "  Allow external CLAUDE.md file imports?\n", "claude-external-imports", NeedsHuman},
		{task.Codex, "  Trust this folder? Codex can read, edit, and run files here", "codex-trust", NeedsHuman},
		{task.Codex, "  Hooks need review\n  24 hooks are new or changed.", "codex-hooks-review", SendKeys},
	}
	for _, tt := range tests {
		a, _ := For(tt.h)
		d, ok := DetectDialog(a, tt.screen)
		if !ok || d.Name != tt.name || d.Action != tt.action {
			t.Errorf("%s: got %+v, %v; want %s", tt.h, d, ok, tt.name)
		}
	}
	a, _ := For(task.Codex)
	if _, ok := DetectDialog(a, "› Ask Codex to do anything"); ok {
		t.Error("ready prompt detected as a dialog")
	}
}

func TestClaudeSettings(t *testing.T) {
	data, err := ClaudeSettings("/home/u/bin/herdr-runner-kit")
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Hooks struct {
			Stop []struct {
				Hooks []struct{ Type, Command string }
			}
		}
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	h := s.Hooks.Stop[0].Hooks[0]
	if h.Type != "command" || h.Command != "/home/u/bin/herdr-runner-kit __turn-end --harness claude" {
		t.Errorf("hook = %+v", h)
	}
	data, _ = ClaudeSettings("/opt/it's here/hrk")
	if !strings.Contains(string(data), `'/opt/it'\\''s here/hrk' __turn-end`) {
		t.Errorf("unsafe quoting: %s", data)
	}
	if _, err := ClaudeSettings("hrk"); err == nil {
		t.Error("relative binary accepted")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCopyClaudeTrust(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	writeFile(t, path, `{
  "numStartups": 9007199254740993,
  "projects": {
    "/repo": {"hasTrustDialogAccepted": true, "hasClaudeMdExternalIncludesApproved": true, "allowedTools": []},
    "/plain": {"hasTrustDialogAccepted": false}
  }
}`)
	set, err := CopyClaudeTrust(path, "/repo/", "/wt/t3")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(set, []string{"hasTrustDialogAccepted", "hasClaudeMdExternalIncludesApproved"}) {
		t.Fatalf("set = %v", set)
	}
	ok, err := ClaudeTrustPresent(path, "/wt/t3", set)
	if err != nil || !ok {
		t.Fatalf("present = %v, %v", ok, err)
	}
	ok, _ = ClaudeTrustPresent(path, "/wt/t3", []string{"hasClaudeMdExternalIncludesWarningShown"})
	if ok {
		t.Error("granted a flag the origin did not have")
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "9007199254740993") {
		t.Error("large integer was rewritten")
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o", info.Mode().Perm())
	}

	// Second run is a no-op; an untrusted origin grants nothing.
	if set, err := CopyClaudeTrust(path, "/repo", "/wt/t3"); err != nil || set != nil {
		t.Errorf("second run = %v, %v", set, err)
	}
	if set, err := CopyClaudeTrust(path, "/plain", "/wt/t4"); err != nil || set != nil {
		t.Errorf("untrusted origin = %v, %v", set, err)
	}
	// Workspace trust is inherited from a trusted parent; import approval is not.
	writeFile(t, path, `{"projects": {"/home/u/Work": {"hasTrustDialogAccepted": true, "hasClaudeMdExternalIncludesApproved": true}}}`)
	set, err = CopyClaudeTrust(path, "/home/u/Work/app", "/wt/t5")
	if err != nil || !reflect.DeepEqual(set, []string{"hasTrustDialogAccepted"}) {
		t.Errorf("inherited trust = %v, %v", set, err)
	}
	if set, err := CopyClaudeTrust(filepath.Join(t.TempDir(), "missing.json"), "/repo", "/wt"); err != nil || set != nil {
		t.Errorf("missing file = %v, %v", set, err)
	}
}

func TestClaudeConfigPath(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/h/.claude2")
	if got, _ := ClaudeConfigPath(); got != "/h/.claude2/.claude.json" {
		t.Errorf("got %q", got)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("HOME", "/h")
	if got, _ := ClaudeConfigPath(); got != "/h/.claude.json" {
		t.Errorf("got %q", got)
	}
}

const userCodexConfig = `model = "gpt-6-astra"
model_reasoning_effort = "xhigh"

[features]
  hooks = true

[projects]
  [projects."/home/u"]
    trust_level = "trusted"
  [projects."/home/u/Work/app"]
    trust_level = "trusted"
  [projects."/home/u/untrusted"]
    trust_level = "untrusted"`

func TestEnsureCodexTrust(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, userCodexConfig)

	added, err := EnsureCodexTrust(path, "/home/u/Work/app")
	if err != nil || added {
		t.Fatalf("already trusted: added=%v err=%v", added, err)
	}
	// A trusted ancestor does not count for Codex.
	added, err = EnsureCodexTrust(path, "/home/u/Work/new-repo")
	if err != nil || !added {
		t.Fatalf("new repo: added=%v err=%v", added, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), userCodexConfig) {
		t.Fatal("existing content was changed")
	}
	var cfg codexConfig
	if err := toml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("result does not parse: %v\n%s", err, data)
	}
	if lvl := cfg.Projects["/home/u/Work/new-repo"].TrustLevel; lvl == nil || *lvl != "trusted" {
		t.Fatalf("trust_level = %v", lvl)
	}
	if added, _ := EnsureCodexTrust(path, "/home/u/Work/new-repo/"); added {
		t.Error("appended twice")
	}

	if _, err := EnsureCodexTrust(path, "/home/u/untrusted"); err == nil || !strings.Contains(err.Error(), `"untrusted"`) {
		t.Errorf("explicit untrusted: err = %v", err)
	}
	if _, err := EnsureCodexTrust(path, "relative/repo"); err == nil {
		t.Error("relative root accepted")
	}
}

func TestEnsureCodexTrustMissingOrBrokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex", "config.toml")
	if added, err := EnsureCodexTrust(path, "/r"); err != nil || !added {
		t.Fatalf("missing file: added=%v err=%v", added, err)
	}
	broken := filepath.Join(dir, "broken.toml")
	writeFile(t, broken, "model = \n")
	if _, err := EnsureCodexTrust(broken, "/r"); err == nil {
		t.Fatal("edited a file that does not parse")
	}
	if data, _ := os.ReadFile(broken); string(data) != "model = \n" {
		t.Errorf("broken file changed: %q", data)
	}
}

func TestCodexConfigPath(t *testing.T) {
	t.Setenv("CODEX_HOME", "/c")
	if got, _ := CodexConfigPath(); got != "/c/config.toml" {
		t.Errorf("got %q", got)
	}
}
