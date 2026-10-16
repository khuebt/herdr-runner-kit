package harness

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
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
		agent string
		dir   string // task folder holding the runner files
		want  []string
	}{
		{"claude", "", []string{"--dangerously-skip-permissions"}},
		{"claude:claude-opus-5-5", "", []string{"--dangerously-skip-permissions", "--model", "claude-opus-5-5"}},
		{"claude::max", "", []string{"--dangerously-skip-permissions", "--effort", "max"}},
		{
			"claude:claude-sonnet-5-5:medium", "/s/t3",
			[]string{
				"--dangerously-skip-permissions", "--model", "claude-sonnet-5-5", "--effort", "medium",
				"--settings", "/s/t3/claude-settings.json", "--append-system-prompt-file", "/s/t3/contract.md",
			},
		},
	}
	for _, tt := range tests {
		got := claude{}.LaunchArgs(LaunchOptions{Spec: spec(t, tt.agent), Dir: tt.dir})
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
			"codex:gpt-6-astra:xhigh", "/usr/local/bin/hrk",
			[]string{
				"--dangerously-bypass-approvals-and-sandbox", "-m", "gpt-6-astra",
				"-c", `model_reasoning_effort="xhigh"`,
				"-c", `notify=["/usr/local/bin/hrk","__turn-end","--harness","codex"]`,
			},
		},
	}
	for _, tt := range tests {
		got := codex{}.LaunchArgs(LaunchOptions{Spec: spec(t, tt.agent), Bin: tt.bin, Dir: "/ignored"})
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: argv = %q, want %q", tt.agent, got, tt.want)
		}
	}
}

func TestPiLaunchArgs(t *testing.T) {
	tests := []struct {
		a     Adapter
		agent string
		want  []string
	}{
		{piAgent, "pi:openai-codex/gpt-6-luna", []string{
			"--model", "openai-codex/gpt-6-luna", "--approve",
			"--append-system-prompt", "/s/t3/contract.md", "-e", "/s/t3/hrk-runner.ts",
		}},
		{piAgent, "pi:openai-codex/gpt-6-luna:high", []string{
			"--model", "openai-codex/gpt-6-luna", "--thinking", "high", "--approve",
			"--append-system-prompt", "/s/t3/contract.md", "-e", "/s/t3/hrk-runner.ts",
		}},
		{ompAgent, "omp:openai-codex/gpt-6-luna", []string{
			"--model", "openai-codex/gpt-6-luna", "--auto-approve", "--config", "/s/t3/omp-runner.yml",
			"--append-system-prompt", "/s/t3/contract.md", "--hook", "/s/t3/hrk-runner.ts",
		}},
		{ompAgent, "omp:openai-codex/gpt-6-luna:auto", []string{
			"--model", "openai-codex/gpt-6-luna", "--thinking", "auto", "--auto-approve", "--config", "/s/t3/omp-runner.yml",
			"--append-system-prompt", "/s/t3/contract.md", "--hook", "/s/t3/hrk-runner.ts",
		}},
	}
	for _, tt := range tests {
		got := tt.a.LaunchArgs(LaunchOptions{Spec: spec(t, tt.agent), Bin: "/usr/local/bin/hrk", Dir: "/s/t3"})
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: argv = %q, want %q", tt.agent, got, tt.want)
		}
	}
}

func TestPiFiles(t *testing.T) {
	for _, a := range []Adapter{piAgent, ompAgent} {
		files, err := a.Files(FilesInput{Bin: `/opt/my "kit"/hrk`, Contract: "CONTRACT"})
		if err != nil {
			t.Fatal(err)
		}
		if len(files) < 2 || files[0].Name != "contract.md" || string(files[0].Data) != "CONTRACT\n" || files[1].Name != "hrk-runner.ts" {
			t.Fatalf("%s: files = %q", a.Harness(), files)
		}
		ext := string(files[1].Data)
		for _, want := range []string{`const BIN = "/opt/my \"kit\"/hrk";`, `const HARNESS = "` + string(a.Harness()) + `";`} {
			if !strings.Contains(ext, want) {
				t.Errorf("%s: extension lacks %s", a.Harness(), want)
			}
		}
		if strings.Contains(ext, "__HRK_") || !strings.Contains(ext, "const INPUT_LOCKED = false;") {
			t.Errorf("%s: a placeholder is left, or the input is locked", a.Harness())
		}
		checkScript(t, ext)
		locked, _ := a.Files(FilesInput{Bin: "/b/hrk", InputLocked: true})
		if !strings.Contains(string(locked[1].Data), "const INPUT_LOCKED = true;") {
			t.Errorf("%s: a locked task's extension is open", a.Harness())
		}
	}
	if _, err := piAgent.Files(FilesInput{Bin: "hrk"}); err == nil {
		t.Error("a relative binary was accepted")
	}
	pf, _ := piAgent.Files(FilesInput{Bin: "/b/hrk"})
	of, _ := ompAgent.Files(FilesInput{Bin: "/b/hrk"})
	if len(pf) != 2 || len(of) != 3 || of[2].Name != "omp-runner.yml" || string(of[2].Data) != "ask:\n  enabled: false\ntui:\n  titleState: false\n" {
		t.Errorf("pi files = %q, omp files = %q", pf, of)
	}
}

func TestPiDialogs(t *testing.T) {
	d, ok := DetectDialog(piAgent, readFixture(t, "project-trust-pi.txt"))
	if !ok || d.Name != "pi-project-trust" || d.Action != NeedsHuman {
		t.Errorf("trust capture: got %+v, %v", d, ok)
	}
	for _, name := range []string{"working-pi-1.txt", "working-pi-2.txt", "working-pi-3.txt", "longcmd-pi-1.txt", "longcmd-omp-1.txt", "async-job-omp-1.txt"} {
		for _, a := range []Adapter{piAgent, ompAgent} {
			if d, ok := DetectDialog(a, readFixture(t, name)); ok {
				t.Errorf("%s on %s: found %s", a.Harness(), name, d.Name)
			}
		}
	}
	if ompAgent.Dialogs() != nil {
		t.Errorf("omp dialogs = %+v", ompAgent.Dialogs())
	}
}

func TestPiPrepareTrustWritesNothing(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	for _, a := range []Adapter{piAgent, ompAgent} {
		res, err := a.PrepareTrust(TrustInput{Cwd: cwd, RunnerPath: cwd})
		if err != nil || res != (TrustResult{}) {
			t.Errorf("%s: %+v, %v", a.Harness(), res, err)
		}
	}
	for _, dir := range []string{home, cwd} {
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%s has %d entries", dir, len(entries))
		}
	}
}

// checkScript has node parse a rendered extension, when node is installed.
// The extension is plain JavaScript, so it is checked as a module.
func checkScript(t *testing.T, src string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Log("node not on PATH; syntax not checked")
		return
	}
	path := filepath.Join(t.TempDir(), "hrk-runner.mjs")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", path).CombinedOutput(); err != nil {
		t.Errorf("node --check: %v\n%s", err, out)
	}
}

// TestPiExtensionGate runs the rendered extension in node against a fake
// gate that refuses text naming a human and fails on text naming a crash.
func TestPiExtensionGate(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	dir := t.TempDir()
	gate := filepath.Join(dir, "hrk")
	writeFile(t, gate, "#!/bin/sh\nin=$(cat)\ncase \"$in\" in *crash*) echo no; exit 3;; *human*) echo \"refused: $*\";; esac\n")
	if err := os.Chmod(gate, 0o700); err != nil {
		t.Fatal(err)
	}
	driver := `import ext from "./hrk-runner.mjs";
const h = {}, notes = [];
ext({ on: (name, f) => { h[name] = f; } });
const ctx = { ui: { notify: (m, level) => notes.push(level + ": " + m) } };
const run = (name, ev) => (h[name] ? h[name](ev, ctx) ?? "pass" : "none");
console.log(JSON.stringify({
  human: run("input", { text: "human", source: "interactive" }),
  kit: run("input", { text: "kit", source: "interactive" }),
  extension: run("input", { text: "human", source: "extension" }),
  crash: run("input", { text: "human crash", source: "interactive" }),
  bash: run("user_bash", { command: "echo human" }),
  bashKit: run("user_bash", { command: "echo kit" }),
  notes,
}));
`
	writeFile(t, filepath.Join(dir, "driver.mjs"), driver)
	refused := map[string]any{"output": "refused: __prompt-gate --harness pi\n", "exitCode": 1.0, "cancelled": false, "truncated": false}
	locked := func(h string, bash any) map[string]any {
		return map[string]any{
			"human": map[string]any{"action": "handled", "handled": true}, "kit": "pass", "extension": "pass", "crash": "pass",
			"bash": bash, "bashKit": "pass", "notes": []any{"warning: refused: __prompt-gate --harness " + h},
		}
	}
	open := map[string]any{"human": "none", "kit": "none", "extension": "none", "crash": "none", "bash": "none", "bashKit": "none", "notes": []any{}}
	for _, tt := range []struct {
		a      Adapter
		locked bool
		want   map[string]any
	}{
		{piAgent, true, locked("pi", map[string]any{"result": refused})},
		{ompAgent, true, locked("omp", "pass")}, // omp's input handler gates ! bash
		{piAgent, false, open},
	} {
		files, err := tt.a.Files(FilesInput{Bin: gate, InputLocked: tt.locked})
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "hrk-runner.mjs"), string(files[1].Data))
		out, err := exec.Command(node, filepath.Join(dir, "driver.mjs")).Output()
		if err != nil {
			t.Fatalf("%s locked %v: %v", tt.a.Harness(), tt.locked, err)
		}
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s locked %v: %s, %v", tt.a.Harness(), tt.locked, out, err)
		}
	}
}

func TestPiReadTurnEnd(t *testing.T) {
	tests := []struct {
		fixture string
		want    TurnSignal
	}{
		{"turnend-pi-completed.json", TurnSignal{ContextLeft: 95, ContextKnown: true}},
		{"turnend-pi-aborted.json", TurnSignal{Ignore: true}},
		{"turnend-pi-error.json", TurnSignal{ContextLeft: 94, ContextKnown: true}},
		{"turnend-pi-usage-limit.json", TurnSignal{LimitConfirmed: true}},
		{"turnend-pi-context-high.json", TurnSignal{ContextLeft: 10, ContextKnown: true}},
		{"turnend-pi-rate-limit.json", TurnSignal{LimitOnScreen: true, ContextLeft: 88, ContextKnown: true}},
		{"turnend-omp-completed.json", TurnSignal{ContextLeft: 90, ContextKnown: true}},
	}
	for _, a := range []Adapter{piAgent, ompAgent} {
		for _, tt := range tests {
			got, err := a.ReadTurnEnd(TurnInput{Payload: []byte(readFixture(t, tt.fixture))})
			if err != nil || got != tt.want {
				t.Errorf("%s %s = %+v, %v; want %+v", a.Harness(), tt.fixture, got, err, tt.want)
			}
		}
	}
	for msg, want := range map[string]TurnSignal{
		"out of credits":           {LimitConfirmed: true},
		"Usage\nlimit reached":     {LimitConfirmed: true},
		"insufficient_quota":       {LimitConfirmed: true},
		"Your QUOTA is exhausted":  {LimitOnScreen: true},
		"connection reset by peer": {},
	} {
		payload, _ := json.Marshal(map[string]any{"event": "agent_end", "errorMessage": msg})
		if got, _ := ompAgent.ReadTurnEnd(TurnInput{Payload: payload}); got != want {
			t.Errorf("%q = %+v, want %+v", msg, got, want)
		}
	}
	// Right after a compaction the agent knows no percent.
	got, err := piAgent.ReadTurnEnd(TurnInput{Payload: []byte(`{"event":"agent_settled","aborted":false,"context":{"tokens":1200,"window":272000}}`)})
	if err != nil || got != (TurnSignal{}) {
		t.Errorf("no percent = %+v, %v", got, err)
	}
	if _, err := piAgent.ReadTurnEnd(TurnInput{Payload: []byte(`{"event"`)}); err == nil {
		t.Error("a malformed payload was accepted")
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
		{task.Pi, "ak:plan", "/skill:ak-plan"},
		{task.Pi, "/ak:plan", "/skill:ak-plan"},
		{task.Pi, "$ak-plan", "/skill:ak-plan"},
		{task.Pi, "", ""},
		{task.Omp, "ak:plan", "/skill:ak-plan"},
		{task.Omp, "/ak:plan", "/skill:ak-plan"},
		{task.Omp, "$ak-plan", "/skill:ak-plan"},
		{task.Omp, "", ""},
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
	for _, a := range []Adapter{piAgent, ompAgent} {
		if got := a.FirstPrompt("ak:cook", "plans/x/plan.md", "CONTRACT"); got != "/skill:ak-cook plans/x/plan.md" {
			t.Errorf("%s = %q", a.Harness(), got)
		}
	}
	if got := (codex{}).FirstPrompt("", "fix the login bug", ""); got != "fix the login bug" {
		t.Errorf("raw prompt = %q", got)
	}
}

func TestCodexStartupDialogScreens(t *testing.T) {
	a, _ := For(task.Codex)
	for _, tt := range []struct {
		file, name string
		keys       []string
	}{
		// Enter on the update prompt would run the update.
		{"codex-update.txt", "codex-update", []string{"esc"}},
		{"codex-hooks-review.txt", "codex-hooks-review", nil},
		// The user may open the review in the tab; the dialog still waits.
		{"codex-hooks-table.txt", "codex-hooks-review", nil},
	} {
		screen, err := os.ReadFile(filepath.Join("testdata", tt.file))
		if err != nil {
			t.Fatal(err)
		}
		d, ok := DetectDialog(a, string(screen))
		if !ok || d.Name != tt.name || !slices.Equal(d.Keys, tt.keys) {
			t.Errorf("%s: got %+v, %v; want %s", tt.file, d, ok, tt.name)
		}
	}
	d, _ := DetectDialog(a, "Hooks need review")
	if d.Action != Ask || !d.IdleWhileShown || !slices.Equal(d.Answers["trust"], []string{"2", "enter"}) || !slices.Equal(d.Answers["skip"], []string{"esc"}) || len(d.Answers) != 2 {
		t.Errorf("hooks review = %+v", d)
	}
}

func TestCodexHooksQuestion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	hooks := `{"hooks":{
		"SessionStart":[{"hooks":[{"type":"command","command":"gh-axi"},{"type":"command","command":"node  '/opt/hooks/session-init.cjs'"}]}],
		"UserPromptSubmit":[{"matcher":"","hooks":[{"type":"command","command":"api_key=abcd1234 notify"}]}]}}`
	cfg := "[hooks.state.\"" + filepath.Join(home, "hooks.json") + ":session_start:0:0\"]\ntrusted_hash = \"sha256:00\"\n\n" +
		"[[hooks.PreToolUse]]\nhooks = [{ type = \"command\", command = \"inline-check\" }]\n"
	if err := os.WriteFile(filepath.Join(home, "hooks.json"), []byte(hooks), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	screen, _ := os.ReadFile(filepath.Join("testdata", "codex-hooks-review.txt"))
	d, _ := DetectDialog(codex{}, string(screen))
	q := d.Question("t7", string(screen), 1000)
	for _, want := range []string{
		"Codex in t7 asks to trust 4 new or changed hooks.",
		"SessionStart: node '/opt/hooks/session-init.cjs'; UserPromptSubmit: api_key=abcd1234 notify; PreToolUse: inline-check.",
		"1 changed since they were trusted.",
		"Answer trust or skip",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("question lacks %q:\n%s", want, q)
		}
	}
	if strings.Contains(q, "gh-axi") {
		t.Errorf("a trusted hook is listed: %s", q)
	}
	short := d.Question("t7", string(screen), 300)
	if n := len([]rune(short)); n > 300 || !strings.Contains(short, "…") || !strings.HasSuffix(short, "unless you know these hooks).") {
		t.Errorf("clipped question (%d) = %s", n, short)
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
		{task.Codex, "  Hooks need review\n  24 hooks are new or changed.", "codex-hooks-review", Ask},
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
	data, err := ClaudeSettings("/home/u/bin/hrk", false)
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
		if h.Type != "command" || h.Command != "/home/u/bin/hrk __turn-end --harness claude" {
			t.Errorf("hook = %+v", h)
		}
	}
	var compact struct {
		Hooks struct {
			PreCompact, SessionStart []struct {
				Matcher string
				Hooks   []struct{ Type, Command string }
			}
		}
	}
	if err := json.Unmarshal(data, &compact); err != nil {
		t.Fatal(err)
	}
	pre, start := compact.Hooks.PreCompact, compact.Hooks.SessionStart
	if len(pre) != 1 || pre[0].Matcher != "" || len(pre[0].Hooks) != 1 || pre[0].Hooks[0].Command != "/home/u/bin/hrk __context --harness claude" {
		t.Errorf("PreCompact hooks = %s", data)
	}
	if len(start) != 1 || start[0].Matcher != "compact" || len(start[0].Hooks) != 1 || start[0].Hooks[0].Command != "/home/u/bin/hrk __context --harness claude" {
		t.Errorf("SessionStart hooks = %s", data)
	}
	if strings.Contains(string(data), "UserPromptSubmit") {
		t.Errorf("open input has a prompt gate: %s", data)
	}
	data, _ = ClaudeSettings("/opt/it's here/hrk", false)
	if !strings.Contains(string(data), `'/opt/it'\\''s here/hrk' __turn-end`) {
		t.Errorf("unsafe quoting: %s", data)
	}
	if _, err := ClaudeSettings("hrk", false); err == nil {
		t.Error("relative binary accepted")
	}
}

func TestClaudeSettingsPromptGate(t *testing.T) {
	data, err := ClaudeSettings("/home/u/bin/hrk", true)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Hooks struct {
			Stop, UserPromptSubmit []struct {
				Matcher string
				Hooks   []struct{ Type, Command string }
			}
		}
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	gate := s.Hooks.UserPromptSubmit
	if len(gate) != 1 || gate[0].Matcher != "" || len(gate[0].Hooks) != 1 ||
		gate[0].Hooks[0].Type != "command" || gate[0].Hooks[0].Command != "/home/u/bin/hrk __prompt-gate --harness claude" {
		t.Errorf("UserPromptSubmit hooks = %s", data)
	}
	if len(s.Hooks.Stop) != 1 {
		t.Errorf("the gate replaced the other hooks: %s", data)
	}
	files, err := claude{}.Files(FilesInput{Bin: "/home/u/bin/hrk", Contract: "C", InputLocked: true})
	if err != nil || len(files) == 0 || !strings.Contains(string(files[0].Data), "__prompt-gate") {
		t.Errorf("locked Claude files = %q, %v", files, err)
	}
}

func TestInputLockMethods(t *testing.T) {
	for _, a := range All() {
		locks := a.Harness() != task.Codex
		if a.LocksInput() != locks {
			t.Errorf("%s: LocksInput = %v", a.Harness(), a.LocksInput())
		}
		if !locks && (a.ClearInputKeys() != nil || a.GateBlock("x") != "") {
			t.Errorf("%s: an open adapter clears or blocks", a.Harness())
		}
	}
	for _, a := range []Adapter{piAgent, ompAgent} {
		if a.ClearInputKeys() != nil || a.GateBlock("t1 is a runner") != "t1 is a runner" {
			t.Errorf("%s: clear keys %q, block %q", a.Harness(), a.ClearInputKeys(), a.GateBlock("t1 is a runner"))
		}
		prompt, synthetic, err := a.GatePrompt([]byte(`{"text":"hello","source":"interactive"}`))
		if prompt != "hello" || synthetic || err != nil {
			t.Errorf("%s: GatePrompt = %q, %v, %v", a.Harness(), prompt, synthetic, err)
		}
		if _, _, err := a.GatePrompt([]byte("not json")); err == nil {
			t.Errorf("%s: an unreadable payload was read", a.Harness())
		}
	}
	keys := claude{}.ClearInputKeys()
	if len(keys) != 2*claudeClearLines || keys[0] != "ctrl+u" || keys[1] != "backspace" || slices.Contains(keys, "esc") {
		t.Errorf("Claude clear keys = %q", keys)
	}
	if got := (claude{}).GateBlock(`t1 is a runner: "hrk send"`); got != `{"decision":"block","reason":"t1 is a runner: \"hrk send\""}` {
		t.Errorf("Claude block = %s", got)
	}
	for file, want := range map[string]bool{
		"userpromptsubmit-claude.json":                   false,
		"userpromptsubmit-task-notification-claude.json": true,
	} {
		payload, err := os.ReadFile(filepath.Join("testdata", file))
		if err != nil {
			t.Fatal(err)
		}
		prompt, got, err := (claude{}).GatePrompt(payload)
		if err != nil || got != want || prompt == "" {
			t.Errorf("%s: synthetic = %v, %v (prompt %q); want %v", file, got, err, prompt, want)
		}
	}
	if _, _, err := (claude{}).GatePrompt([]byte("not json")); err == nil {
		t.Error("an unreadable payload was read")
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

// A pi or omp screen never confirms a usage limit, not even the agent's own
// limit line: the turn-end payload does (TestPiReadTurnEnd). The tool-quota
// captures are live screens of a tool printing " Error: disk quota
// exceeded", which pi draws like its own error line. The usage-limit
// screens are the captured 401 screens with the error replaced by pi-ai's
// own ChatGPT limit message.
func TestPiScreenNeverShowsUsageLimit(t *testing.T) {
	for _, name := range []string{"tool-quota-pi.txt", "tool-quota-omp.txt", "usage-limit-pi.txt", "usage-limit-omp.txt", "provider-error-pi.txt", "provider-error-omp.txt"} {
		screen := readFixture(t, name)
		if piAgent.ShowsUsageLimit(screen) || ompAgent.ShowsUsageLimit(screen) {
			t.Errorf("%s: detected a usage limit on screen", name)
		}
	}
	if !strings.Contains(readFixture(t, "tool-quota-pi.txt"), "\n\n Error: disk quota exceeded") {
		t.Error("tool-quota-pi.txt lost the tool line that looks like pi's error line")
	}
}

func TestPiContextLeftIsUnknown(t *testing.T) {
	pis, _ := filepath.Glob("testdata/*-pi*.txt")
	omps, _ := filepath.Glob("testdata/*-omp*.txt")
	names := append(pis, omps...)
	if len(pis) == 0 || len(omps) == 0 {
		t.Fatal("no pi or omp captures")
	}
	for _, name := range names {
		screen := readFixture(t, filepath.Base(name))
		for _, a := range []Adapter{piAgent, ompAgent} {
			if got, ok := a.ContextLeft(screen); ok || got != 0 {
				t.Errorf("%s: %s read %d%% left", name, a.Harness(), got)
			}
		}
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

func TestParseClaudeCompaction(t *testing.T) {
	for name, want := range map[string]ClaudeCompaction{
		"precompact-auto.json":      {Event: "PreCompact", Trigger: "auto"},
		"precompact-manual.json":    {Event: "PreCompact", Trigger: "manual"},
		"sessionstart-compact.json": {Event: "SessionStart", Source: "compact"},
	} {
		got, err := ParseClaudeCompaction([]byte(readFixture(t, name)))
		if err != nil || got != want {
			t.Errorf("%s = %+v, %v", name, got, err)
		}
	}
	if _, err := ParseClaudeCompaction([]byte("not json")); err == nil {
		t.Error("bad input parsed")
	}
}

func TestClaudeSessionContext(t *testing.T) {
	var out struct {
		HookSpecificOutput struct{ HookEventName, AdditionalContext string }
	}
	if err := json.Unmarshal([]byte(ClaudeSessionContext("hand off \"now\"")), &out); err != nil {
		t.Fatal(err)
	}
	if out.HookSpecificOutput.HookEventName != "SessionStart" || out.HookSpecificOutput.AdditionalContext != `hand off "now"` {
		t.Errorf("output = %+v", out)
	}
}

func TestContextLeft(t *testing.T) {
	c := codex{}
	for name, want := range map[string]int{
		"codex-context-high.txt":      56,
		"codex-context-low.txt":       13,
		"codex-context-left-high.txt": 56,
		"codex-context-left-low.txt":  13,
		"usage-limit-codex.txt":       100,
	} {
		if got, ok := c.ContextLeft(readFixture(t, name)); !ok || got != want {
			t.Errorf("%s = %d, %v; want %d", name, got, ok, want)
		}
	}
	for name, screen := range map[string]string{
		"conversation text":     "• Context 5% left on the other box\n\n› Ask Codex to do anything\n\n  GPT-6-Luna medium · ~/Work\n",
		"usage limits only":     "› Ask Codex to do anything\n\n  gpt-6 · 5h 3% left · weekly 4% left\n",
		"no input line":         "  Context 5% left · gpt-6\n",
		"cut by the pane width": "› Ask Codex to do anything\n\n  GPT-6-Luna medium · /tmp/a/very/long/path/that/hides/the/cont…\n",
	} {
		if got, ok := c.ContextLeft(screen); ok {
			t.Errorf("%s: read %d%% left", name, got)
		}
	}
	if _, ok := (claude{}).ContextLeft("› x\n  Context 5% left\n"); ok {
		t.Error("Claude's footer was read")
	}
}

func TestRegistryMatchesTheHarnessTable(t *testing.T) {
	var got []task.Harness
	for _, a := range All() {
		got = append(got, a.Harness())
	}
	if want := task.Harnesses(); !reflect.DeepEqual(got, want) {
		t.Errorf("adapters = %q, task.Harnesses() = %q", got, want)
	}
}

// screenOf returns a Screen that shows text, or fails when text is empty.
func screenOf(text string) Screen {
	return func(int) (string, error) {
		if text == "" {
			return "", errors.New("no pane")
		}
		return text, nil
	}
}

func TestReadTurnEnd(t *testing.T) {
	const codexTurn = `{"type":"agent-turn-complete","thread-id":"th1","input-messages":["go on"]}`
	tests := []struct {
		name    string
		a       Adapter
		payload string
		screen  string // fixture name, or "" for an unreadable pane
		want    TurnSignal
	}{
		{"claude limit", claude{}, readFixture(t, "stop-failure-claude.json"), "", TurnSignal{LimitConfirmed: true}},
		{"claude other failure", claude{}, `{"hook_event_name":"StopFailure","error":"server_error"}`, "", TurnSignal{Ignore: true}},
		{"claude agents running", claude{}, readFixture(t, "stop-background-running-claude.json"), "background-agents-claude-1.txt", TurnSignal{Background: true}},
		{"claude all finished", claude{}, readFixture(t, "stop-background-none-claude.json"), "background-agents-claude-2.txt", TurnSignal{}},
		{"claude mode line", claude{}, `{"hook_event_name":"Stop"}`, "background-agents-claude-2.txt", TurnSignal{Background: true}},
		{"claude no list, no pane", claude{}, `{"hook_event_name":"Stop"}`, "", TurnSignal{}},
		{"codex title turn", codex{}, `{"type":"agent-turn-complete","input-messages":["Generate a concise, single-line task title for this"]}`, "", TurnSignal{Ignore: true}},
		{"codex other event", codex{}, `{"type":"approval-requested"}`, "", TurnSignal{Ignore: true}},
		{"codex low context", codex{}, codexTurn, "codex-context-low.txt", TurnSignal{Thread: "th1", ContextLeft: 13, ContextKnown: true}},
		{"codex limit", codex{}, codexTurn, "usage-limit-codex.txt", TurnSignal{Thread: "th1", LimitOnScreen: true}},
		{"codex no pane", codex{}, codexTurn, "", TurnSignal{Thread: "th1"}},
	}
	for _, tt := range tests {
		screen := ""
		if tt.screen != "" {
			screen = readFixture(t, tt.screen)
		}
		got, err := tt.a.ReadTurnEnd(TurnInput{Payload: []byte(tt.payload), Screen: screenOf(screen)})
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if tt.name == "codex limit" {
			got.ContextLeft, got.ContextKnown = 0, false // the capture's footer is not under test here
		}
		if got != tt.want {
			t.Errorf("%s: signal = %+v, want %+v", tt.name, got, tt.want)
		}
	}
	if _, err := (codex{}).ReadTurnEnd(TurnInput{Payload: []byte("{"), Screen: screenOf("")}); err == nil {
		t.Error("codex: a malformed notify payload was accepted")
	}
}

func TestDoneAtTurnEnd(t *testing.T) {
	for _, a := range All() {
		if got, want := a.DoneAtTurnEnd(), a.Harness() == task.Codex; got != want {
			t.Errorf("%s: DoneAtTurnEnd = %v, want %v", a.Harness(), got, want)
		}
	}
}

func TestShowsWorking(t *testing.T) {
	for _, tc := range []struct {
		file string
		want bool
	}{
		{"working-codex-1.txt", true},
		{"working-codex-2.txt", true},
		{"streaming-codex-1.txt", false},
		{"codex-context-low.txt", false},
	} {
		screen, err := os.ReadFile(filepath.Join("testdata", tc.file))
		if err != nil {
			t.Fatal(err)
		}
		if got := (codex{}).ShowsWorking(string(screen)); got != tc.want {
			t.Errorf("%s: ShowsWorking = %v, want %v", tc.file, got, tc.want)
		}
	}
	if (codex{}).ShowsWorking("• Working (3s • esc to interrupt)\n" + strings.Repeat("output\n", 20)) {
		t.Error("a status line far above the input counted")
	}
	working, _ := os.ReadFile(filepath.Join("testdata", "working-codex-1.txt"))
	for _, a := range All() {
		if a.Harness() != task.Codex && a.ShowsWorking(string(working)) {
			t.Errorf("%s reads a working marker", a.Harness())
		}
	}
}
