package harness

import (
	"encoding/json"
	"errors"
	"io/fs"
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
		contract string // contract file path
		want     []string
	}{
		{"claude", "", "", []string{"--dangerously-skip-permissions"}},
		{"claude:claude-opus-5-5", "", "", []string{"--dangerously-skip-permissions", "--model", "claude-opus-5-5"}},
		{"claude::max", "", "", []string{"--dangerously-skip-permissions", "--effort", "max"}},
		{
			"claude:claude-sonnet-5-5:medium", "/s/t3/settings.json", "/s/t3/contract.md",
			[]string{
				"--dangerously-skip-permissions", "--model", "claude-sonnet-5-5", "--effort", "medium",
				"--settings", "/s/t3/settings.json", "--append-system-prompt-file", "/s/t3/contract.md",
			},
		},
	}
	for _, tt := range tests {
		got := claude{}.LaunchArgs(LaunchOptions{Spec: spec(t, tt.agent), SettingsPath: tt.settings, ContractPath: tt.contract})
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
		got := codex{}.LaunchArgs(LaunchOptions{Spec: spec(t, tt.agent), Bin: tt.bin, ContractPath: "/ignored"})
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
		{task.Claude, "is this a project you created or one you trust?", "claude-workspace-trust", NeedsHuman},
		{task.Claude, "│ Quick safety check: Is this a project you │\n│ created or one you trust? (Like your own  │", "claude-workspace-trust", NeedsHuman},
		{task.Claude, "Is this a  project you created\tor one  you trust", "claude-workspace-trust", NeedsHuman},
		{task.Codex, "╭──╮\n│ TRUST THIS\n│ FOLDER? │", "codex-trust", NeedsHuman},
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
	c, _ := For(task.Claude)
	if _, ok := DetectDialog(c, "❯ is this a project"); ok {
		t.Error("a fragment of the trust phrase detected as a dialog")
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"":                          "",
		"  ──│ ":                    "",
		"│ Is this a\n│ project? │": "is this a project",
		"Trust this folder?":        "trust this folder",
		"CLAUDE.md  file\timports":  "claude md file imports",
		"24 hooks are new":          "24 hooks are new",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShows(t *testing.T) {
	a, _ := For(task.Claude)
	d := a.Dialogs()[0]
	if !Shows("│ IS THIS A PROJECT YOU CREATED │\n│ OR ONE YOU TRUST │", d) {
		t.Error("wrapped dialog not shown")
	}
	if Shows("❯ ", d) || Shows("anything", Dialog{}) {
		t.Error("dialog shown on a ready screen")
	}
}

func TestClaudeSettings(t *testing.T) {
	data, err := ClaudeSettings("/home/u/bin/herdr-runner-kit")
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Hooks struct {
			Stop, StopFailure []struct {
				Hooks []struct{ Type, Command string }
			}
		}
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	for _, hooks := range [][]struct {
		Hooks []struct{ Type, Command string }
	}{s.Hooks.Stop, s.Hooks.StopFailure} {
		if len(hooks) != 1 || len(hooks[0].Hooks) != 1 {
			t.Fatalf("hooks = %s", data)
		}
		h := hooks[0].Hooks[0]
		if h.Type != "command" || h.Command != "/home/u/bin/herdr-runner-kit __turn-end --harness claude" {
			t.Errorf("hook = %+v", h)
		}
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

func TestPrepareClaudeTrust(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	writeFile(t, path, `{
  "numStartups": 9007199254740993,
  "projects": {
    "/repo": {"hasTrustDialogAccepted": true, "hasClaudeMdExternalIncludesApproved": true, "allowedTools": []},
    "/plain": {"hasTrustDialogAccepted": true}
  }
}`)
	set, skipped, err := PrepareClaudeTrust(path, "/repo/", "/wt/t3")
	if err != nil || skipped != "" {
		t.Fatal(err, skipped)
	}
	if !reflect.DeepEqual(set, []string{"hasTrustDialogAccepted", "hasClaudeMdExternalIncludesApproved"}) {
		t.Fatalf("set = %v", set)
	}
	if ok, err := ClaudeTrustPresent(path, "/wt/t3", set); err != nil || !ok {
		t.Fatalf("present = %v, %v", ok, err)
	}
	if ok, _ := ClaudeTrustPresent(path, "/wt/t3", []string{"hasClaudeMdExternalIncludesWarningShown"}); ok {
		t.Error("approved an import flag the origin did not have")
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "9007199254740993") || !strings.Contains(string(data), `"allowedTools": []`) {
		t.Error("existing content was rewritten")
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o", info.Mode().Perm())
	}

	// A second run is a no-op that says why.
	if set, skipped, err := PrepareClaudeTrust(path, "/repo", "/wt/t3"); err != nil || set != nil || skipped != TrustSkipAlreadyTrusted {
		t.Errorf("second run = %v, %q, %v", set, skipped, err)
	}
	// A folder the user never opened gets workspace trust from spawn, but no
	// import approval.
	set, skipped, err = PrepareClaudeTrust(path, "/new", "/new")
	if err != nil || skipped != "" || !reflect.DeepEqual(set, []string{"hasTrustDialogAccepted"}) {
		t.Errorf("new folder = %v, %q, %v", set, skipped, err)
	}
	// A missing config file is never created.
	missing := filepath.Join(t.TempDir(), "missing.json")
	if set, skipped, err := PrepareClaudeTrust(missing, "/repo", "/wt"); err != nil || set != nil || skipped != TrustSkipNoConfig {
		t.Errorf("missing file = %v, %q, %v", set, skipped, err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing config was created: %v", err)
	}
}

func TestClaudeTrusted(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	writeFile(t, path, `{"projects":{"/repo":{"hasTrustDialogAccepted":true},"/other":{"hasTrustDialogAccepted":false}}}`)
	if got, err := ClaudeTrusted(path, []string{"/repo/sub", "/repo/"}); err != nil || got != "/repo" {
		t.Errorf("got %q, %v", got, err)
	}
	if got, err := ClaudeTrusted(path, []string{"/other", "/re"}); err != nil || got != "" {
		t.Errorf("untrusted = %q, %v", got, err)
	}
	if _, err := ClaudeTrusted(filepath.Join(t.TempDir(), "x.json"), []string{"/repo"}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file err = %v", err)
	}
}

func TestDefaultClaudeConfigDir(t *testing.T) {
	t.Setenv("HOME", "/h")
	for in, want := range map[string]string{
		"": "", "/h/.claude": "", "/h/.claude/": "", "~/.claude": "", "/h/.claude2": "/h/.claude2", "/other/.claude": "/other/.claude",
	} {
		if got := DefaultClaudeConfigDir(in); got != want {
			t.Errorf("DefaultClaudeConfigDir(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClaudeConfigPath(t *testing.T) {
	if got, _ := ClaudeConfigPath("/h/.claude2"); got != "/h/.claude2/.claude.json" {
		t.Errorf("got %q", got)
	}
	t.Setenv("HOME", "/h")
	if got, _ := ClaudeConfigPath(""); got != "/h/.claude.json" {
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

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestShowsUsageLimit(t *testing.T) {
	claude, codex := claude{}, codex{}
	if !claude.ShowsUsageLimit(readFixture(t, "usage-limit-claude.txt")) {
		t.Error("claude usage-limit screen not detected")
	}
	if !codex.ShowsUsageLimit(readFixture(t, "usage-limit-codex.txt")) {
		t.Error("codex usage-limit screen not detected")
	}
	// Trailing blank rows below the agent's output do not push it out.
	if !claude.ShowsUsageLimit(readFixture(t, "usage-limit-claude.txt") + strings.Repeat("   \n", 30)) {
		t.Error("claude screen with blank rows below not detected")
	}
	// The phrases in assistant text, or in a tool's output near the bottom
	// of the screen, are not the agent's own limit line.
	for _, name := range []string{"usage-limit-negative.txt", "usage-limit-claude-tool-output.txt", "usage-limit-codex-tool-output.txt"} {
		screen := readFixture(t, name)
		if claude.ShowsUsageLimit(screen) || codex.ShowsUsageLimit(screen) {
			t.Errorf("%s: detected a usage limit", name)
		}
	}
	// Each agent's line only counts for that agent.
	if codex.ShowsUsageLimit(readFixture(t, "usage-limit-claude.txt")) || claude.ShowsUsageLimit(readFixture(t, "usage-limit-codex.txt")) {
		t.Error("one agent's limit line matched the other agent")
	}
	if claude.ShowsUsageLimit("❯ \n") || codex.ShowsUsageLimit("") {
		t.Error("an ordinary screen was detected")
	}
}

func TestClaudeUsageLimit(t *testing.T) {
	payload := readFixture(t, "stop-failure-claude.json")
	msg, stopFailure := ClaudeUsageLimit([]byte(payload))
	if !stopFailure || msg != "You've hit your session limit · resets 3:10pm (Asia/Bangkok)" {
		t.Errorf("limit payload = %q, %v", msg, stopFailure)
	}
	tests := []struct {
		name, payload string
		stopFailure   bool
	}{
		{"stop hook", `{"hook_event_name":"Stop","last_assistant_message":"You've hit your session limit"}`, false},
		{"not json", `You've hit your session limit`, false},
		{"empty", ``, false},
		{"server error", `{"hook_event_name":"StopFailure","error":"server_error","last_assistant_message":"API Error: 500"}`, true},
		{"capacity", `{"hook_event_name":"StopFailure","error":"rate_limit","last_assistant_message":"API Error: Request rejected (429) · this may be a temporary capacity issue."}`, true},
		{"limit named in another error", `{"hook_event_name":"StopFailure","error":"invalid_request","last_assistant_message":"You've hit your session limit"}`, true},
	}
	for _, tt := range tests {
		msg, stopFailure := ClaudeUsageLimit([]byte(tt.payload))
		if msg != "" || stopFailure != tt.stopFailure {
			t.Errorf("%s = %q, %v; want no limit, %v", tt.name, msg, stopFailure, tt.stopFailure)
		}
	}
	if msg, _ := ClaudeUsageLimit([]byte(`{"hook_event_name":"StopFailure","error":"billing_error","last_assistant_message":"You're out of extra usage · resets 3pm"}`)); msg == "" {
		t.Error("out of extra usage not detected")
	}
}
