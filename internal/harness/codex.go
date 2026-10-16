package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/pelletier/go-toml/v2"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

type codex struct{}

func (codex) Harness() task.Harness { return task.Codex }
func (codex) HerdrKind() string     { return "codex" }

// LaunchArgs runs Codex with no approvals and no sandbox, the user's chosen
// trade-off for runners that never stall. notify replaces any notify the
// user set in config.toml; __turn-end calls the user's command in turn.
func (codex) LaunchArgs(o LaunchOptions) []string {
	args := []string{"--dangerously-bypass-approvals-and-sandbox"}
	if o.Spec.Model != "" {
		args = append(args, "-m", o.Spec.Model)
	}
	if o.Spec.Effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+tomlString(o.Spec.Effort))
	}
	if o.Bin != "" {
		args = append(args, "-c", "notify="+tomlStringArray(o.Bin, "__turn-end", "--harness", "codex"))
	}
	return args
}

// RenderSkill uses Codex's "$name" syntax, where a colon becomes a dash.
func (codex) RenderSkill(skill string) string {
	name := skillName(skill)
	if name == "" {
		return ""
	}
	return "$" + strings.ReplaceAll(name, ":", "-")
}

// FirstPrompt carries the contract, because Codex has no appended system
// prompt flag.
func (c codex) FirstPrompt(skill, prompt, contract string) string {
	return joinPrompt(c.RenderSkill(skill), prompt, contract)
}

func (codex) Dialogs() []Dialog {
	return []Dialog{
		{Name: "codex-trust", Match: "Trust this folder?", Action: NeedsHuman},
		// codex-cli shows this when a newer version is out. Enter would run
		// the update; Esc skips it for this launch and persists nothing.
		// "Skip until next version" would write the user's Codex state.
		{Name: "codex-update", Match: "Skip until next version", Action: SendKeys, Keys: []string{"esc"}},
		// Trusting hooks lets them run outside the sandbox in every Codex
		// session, so the user decides. herdr reports the screen as idle. A
		// digit picks its option at once; Esc continues without trusting,
		// for this session only. "1. Review hooks" is not used: Esc from the
		// review closes the dialog as a skip. codex-cli 0.162.1 opened it
		// 0.6 to 1.2 s after its input showed, and queues a prompt sent in
		// between behind it.
		{
			Name: "codex-hooks-review", Match: "Hooks need review", Action: Ask, IdleWhileShown: true, OpensLate: 3 * time.Second,
			Answers:  map[string][]string{"trust": {"2"}, "skip": {"esc"}},
			Question: codexHooksQuestion,
		},
	}
}

// codexHooksCount reads "4 hooks are new or changed." off the hooks
// review.
var codexHooksCount = regexp.MustCompile(`(\d+) hooks? (?:is|are) new or changed`)

// codexHooksQuestion asks the user whether a runner's Codex may trust the
// hooks its review lists. Codex does not show them on the dialog, so it
// names the user's hooks that have no trust entry at all; the hash of a
// changed one is Codex's own, so those are only counted.
func codexHooksQuestion(id, screen string, limit int) string {
	head := "Codex in " + id + " asks to trust its new or changed hooks."
	count := -1
	if m := codexHooksCount.FindStringSubmatch(strings.Join(strings.Fields(screen), " ")); m != nil {
		count, _ = strconv.Atoi(m[1])
		head = fmt.Sprintf("Codex in %s asks to trust %d new or changed hooks.", id, count)
	}
	tail := " Trusting lets them run outside the sandbox in every Codex session. Answer trust or skip (recommended: skip unless you know these hooks)."
	home, hooks := untrustedCodexHooks()
	if n := count - len(hooks); count >= 0 && n > 0 {
		tail = fmt.Sprintf(" %d changed since they were trusted.", n) + tail
	}
	if len(hooks) == 0 {
		return head + tail
	}
	intro := " New in " + home + " (no trust entry): "
	room := limit - len([]rune(head+intro+tail)) - 1
	var list []string
	for _, h := range hooks {
		list = append(list, h.event+": "+h.command)
	}
	return head + intro + clipRunes(strings.Join(list, "; "), room) + "." + tail
}

// codexHook is one of the user's Codex hooks with no trust entry.
type codexHook struct{ event, command string }

// untrustedCodexHooks returns the folder holding the user's Codex config,
// as it reads to the user, and the hooks in its hooks.json and the inline
// [hooks] of its config.toml that config.toml holds no trust entry for.
// Codex keys trust as "<source file>:<event in snake case>:<group>:<hook>".
func untrustedCodexHooks() (string, []codexHook) {
	cfgPath, err := CodexConfigPath()
	if err != nil {
		return "", nil
	}
	dir := filepath.Dir(cfgPath)
	var cfg struct {
		Hooks map[string]any `toml:"hooks"`
	}
	data, _ := os.ReadFile(cfgPath)
	if toml.Unmarshal(data, &cfg) != nil {
		return tildeHome(dir), nil
	}
	trusted := map[string]bool{}
	if state, ok := cfg.Hooks["state"].(map[string]any); ok {
		for k := range state {
			trusted[k] = true
		}
	}
	delete(cfg.Hooks, "state")
	var hooks []codexHook
	add := func(source string, events map[string][]codexHookGroup) {
		for _, event := range slices.Sorted(maps.Keys(events)) {
			for i, g := range events[event] {
				for j, h := range g.Hooks {
					if h.Command != "" && !trusted[fmt.Sprintf("%s:%s:%d:%d", source, snakeCase(event), i, j)] {
						hooks = append(hooks, codexHook{event, shortCommand(h.Command)})
					}
				}
			}
		}
	}
	var file struct {
		Hooks map[string][]codexHookGroup `json:"hooks"`
	}
	hooksPath := filepath.Join(dir, "hooks.json")
	if data, err := os.ReadFile(hooksPath); err == nil && json.Unmarshal(data, &file) == nil {
		add(hooksPath, file.Hooks)
	}
	// The inline hooks went through TOML's dynamic types; JSON gives them
	// their shape back.
	var inline map[string][]codexHookGroup
	if b, err := json.Marshal(cfg.Hooks); err == nil && json.Unmarshal(b, &inline) == nil {
		add(cfgPath, inline)
	}
	return tildeHome(dir), hooks
}

// codexHookGroup is one matcher group of a Codex hook event.
type codexHookGroup struct {
	Hooks []struct {
		Command string `json:"command"`
	} `json:"hooks"`
}

// snakeCase turns "UserPromptSubmit" into "user_prompt_submit".
func snakeCase(s string) string {
	var b strings.Builder
	for i, c := range s {
		if unicode.IsUpper(c) {
			if i > 0 {
				b.WriteByte('_')
			}
			c = unicode.ToLower(c)
		}
		b.WriteRune(c)
	}
	return b.String()
}

// shortCommand shows a hook command on one short line, with the home
// folder as ~.
func shortCommand(cmd string) string {
	return clipRunes(tildeHome(strings.Join(strings.Fields(cmd), " ")), 60)
}

// tildeHome writes the user's home folder in s as ~.
func tildeHome(s string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && home != "/" {
		return strings.ReplaceAll(s, home, "~")
	}
	return s
}

// clipRunes cuts s to at most n characters, ending a cut one with "…".
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return string(r[:n-1]) + "…"
}

// codexLimit is the error codex-cli 0.162.1 draws at the left edge when the
// account's usage limit stops a turn: "■ You've hit your usage limit. Visit
// … or try again at 3:05 PM." Its other output is indented or starts with
// "•", so text an assistant or a tool prints never matches.
var codexLimit = limitLine{
	marker:  "■",
	phrases: []string{"You've hit your usage limit", "You've reached your usage limit"},
}

func (codex) ShowsUsageLimit(screen string) bool { return codexLimit.shown(screen) }

// codexStatus is the status line Codex draws above its input while a turn
// runs, "• Working (44s • esc to interrupt)", and the tip it may draw
// under it.
var codexStatus = regexp.MustCompile(`^\s*(?:•\s.*\besc to interrupt\b|└ Tip:)`)

// codexRetry matches the status Codex shows while it retries a model
// request. The phrases are codex-cli 0.162.1's own strings ("Reconnecting...
// waiting for network", "stream disconnected - retrying sampling request");
// no live screen of them was captured.
var codexRetry = regexp.MustCompile(`(?i)\bReconnecting\.\.\.|\bretrying sampling request\b`)

// ProgressText keeps what is above Codex's input line and its status line,
// which hold the conversation as Codex prints it.
func (codex) ProgressText(screen string) string {
	lines := strings.Split(screen, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(lines[i], "›") {
			lines = lines[:i]
			break
		}
	}
	var out []string
	for _, l := range lines {
		if !codexStatus.MatchString(l) {
			out = append(out, l)
		}
	}
	return joinLines(out)
}

// InputState is always unknown: Codex can report a stale state after
// skipped hooks, and its input line is not read.
func (codex) InputState(string, string) InputState { return InputUnknown }

func (codex) ShowsRetry(screen string) bool {
	return anyLineMatches(lastLines(screen, statusLines), codexRetry)
}

func (codex) SelfResolvingPrompts() []SelfResolvingPrompt { return nil }

// codexContext matches the context part of the footer under Codex's input
// line, as codex-cli 0.162.1 draws it: "100% context left" by default, and
// "Context 13% left" or "Context 87% used" from a status line with the
// context-remaining or context-used item. The word context sets it apart
// from the usage limits, such as "weekly 43% left".
var codexContext = regexp.MustCompile(`(?i)\bcontext\s+(\d{1,3})%\s+(left|used)\b|\b(\d{1,3})%\s+context\s+left\b`)

// ContextLeft reads the footer under the last input line only, so text in
// the conversation never counts.
func (codex) ContextLeft(screen string) (int, bool) {
	lines := strings.Split(screen, "\n")
	input := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(lines[i], "›") {
			input = i
			break
		}
	}
	if input < 0 {
		return 0, false
	}
	for _, l := range lines[input+1:] {
		m := codexContext.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		if m[3] != "" {
			n, err := strconv.Atoi(m[3])
			return n, err == nil && n <= 100
		}
		n, err := strconv.Atoi(m[1])
		if err != nil || n > 100 {
			return 0, false
		}
		if strings.EqualFold(m[2], "used") {
			n = 100 - n
		}
		return n, true
	}
	return 0, false
}

// Files is empty: Codex takes the contract in its first prompt and notify
// as a launch flag.
func (codex) Files(FilesInput) ([]File, error) { return nil, nil }

// PrepareTrust persists trust for the repository spawn ran in; see
// EnsureCodexTrust.
func (codex) PrepareTrust(in TrustInput) (TrustResult, error) {
	root := in.RepoRoot
	if root == "" {
		root = in.Cwd
	}
	path, err := CodexConfigPath()
	if err != nil {
		return TrustResult{}, err
	}
	added, err := EnsureCodexTrust(path, root)
	if err != nil || !added {
		return TrustResult{}, err
	}
	return TrustResult{Event: "codex-trusted", Detail: "added " + root + " to " + path + " as trusted"}, nil
}

// codexTitlePrompt starts the input of Codex's internal title-generation
// turn, which also fires notify.
const codexTitlePrompt = "Generate a concise, single-line task title"

// ReadTurnEnd reads a notify payload. Only agent-turn-complete calls of
// real turns count; Codex shows a usage limit and its context only on
// screen.
func (c codex) ReadTurnEnd(in TurnInput) (TurnSignal, error) {
	var n struct {
		Type   string   `json:"type"`
		Thread string   `json:"thread-id"`
		Input  []string `json:"input-messages"`
	}
	if err := json.Unmarshal(in.Payload, &n); err != nil {
		return TurnSignal{}, fmt.Errorf("parse codex notify: %w", err)
	}
	if n.Type != "agent-turn-complete" {
		return TurnSignal{Ignore: true}, nil
	}
	if len(n.Input) > 0 && strings.HasPrefix(strings.TrimSpace(n.Input[0]), codexTitlePrompt) {
		return TurnSignal{Ignore: true}, nil
	}
	sig := TurnSignal{Thread: n.Thread}
	// A screen that cannot be read shows no limit and no context.
	if screen, err := in.Screen(40); err == nil {
		sig.LimitOnScreen = c.ShowsUsageLimit(screen)
		sig.ContextLeft, sig.ContextKnown = c.ContextLeft(screen)
	}
	return sig, nil
}

// NudgeInHook is false: notify takes no reply, so the nudge is typed.
func (codex) NudgeInHook() bool { return false }

// HookPayload reads the notify JSON Codex passes as the last argument, and
// calls the notify command the user set in config.toml with it, which the
// runner's own notify replaced.
func (codex) HookPayload(args []string, _ io.Reader) ([]byte, error) {
	if len(args) == 0 {
		return nil, nil
	}
	callUserCodexNotify(args[0])
	return []byte(args[0]), nil
}

func (codex) HookReply(string) string      { return "" }
func (codex) SessionContext(string) string { return "" }

// WorksInBackground is false: Codex draws no background count.
func (codex) WorksInBackground(Screen) (bool, error) { return false, nil }

// DoneAtTurnEnd is true: a Codex runner reports inside its turn and may go
// on to commit, or to read a message sent into the running turn, before the
// turn ends. codex-cli 0.162.1 adds a prompt typed while it works to the
// running turn.
func (codex) DoneAtTurnEnd() bool { return true }

// codexWorking is the status line of a running turn, without the tip.
var codexWorking = regexp.MustCompile(`^\s*•\s.*\besc to interrupt\b`)

// ShowsWorking looks for the status line among the last lines, where Codex
// draws it above its input.
func (codex) ShowsWorking(screen string) bool {
	return anyLineMatches(lastLines(screen, statusLines), codexWorking)
}

// LocksInput is false: a per-run prompt hook loads, but Codex skips it
// until the user trusts its hash, which would add a trust step for every
// Codex user.
func (codex) LocksInput() bool                        { return false }
func (codex) ClearInputKeys() []string                { return nil }
func (codex) GatePrompt([]byte) (string, bool, error) { return "", false, nil }
func (codex) GateBlock(string) string                 { return "" }

func (codex) Checks() Checks {
	return Checks{
		Bin:         "codex",
		Flags:       []string{"--dangerously-bypass-approvals-and-sandbox", "--model", "--config"},
		Integration: "codex",
		Required:    true,
	}
}

// callUserCodexNotify runs the notify command from the user's config.toml,
// detached, with the same argument Codex passed.
func callUserCodexNotify(arg string) {
	path, err := CodexConfigPath()
	if err != nil {
		return
	}
	argv, err := CodexUserNotify(path)
	if err != nil || len(argv) == 0 {
		return
	}
	c := exec.Command(argv[0], append(argv[1:], arg)...)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if c.Start() == nil {
		_ = c.Process.Release()
	}
}

// CodexConfigPath returns ${CODEX_HOME:-~/.codex}/config.toml.
func CodexConfigPath() (string, error) {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return filepath.Join(dir, "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

type codexConfig struct {
	Projects map[string]struct {
		TrustLevel *string `toml:"trust_level"`
	} `toml:"projects"`
}

// EnsureCodexTrust makes sure Codex trusts repoRoot, the repository that
// `spawn` ran in; spawning there is the user's consent. Codex keys trust on
// the repository root, ignores trusted ancestors, and ignores a per-run -c
// override, so the entry must be persisted. It appends one table and never
// edits existing content. A root the user explicitly marked otherwise is an
// error, not an override. It returns true when it added the entry.
func EnsureCodexTrust(configPath, repoRoot string) (bool, error) {
	root := filepath.Clean(repoRoot)
	if !filepath.IsAbs(root) {
		return false, fmt.Errorf("repository root must be absolute, got %q", repoRoot)
	}
	data, err := os.ReadFile(configPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	var cfg codexConfig
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return false, fmt.Errorf("parse %s: %w", configPath, err)
	}
	if entry, ok := cfg.Projects[root]; ok {
		switch {
		case entry.TrustLevel != nil && *entry.TrustLevel == "trusted":
			return false, nil
		case entry.TrustLevel != nil:
			return false, fmt.Errorf("codex marks %s as %q in %s; trust it in Codex first", root, *entry.TrustLevel, configPath)
		default:
			return false, fmt.Errorf("codex has an entry for %s without trust_level in %s; trust it in Codex first", root, configPath)
		}
	}

	block := "\n[projects." + tomlString(root) + "]\ntrust_level = \"trusted\"\n"
	if len(data) > 0 && data[len(data)-1] != '\n' {
		block = "\n" + block
	}
	// Validate the result before touching the user's file.
	var check codexConfig
	if err := toml.Unmarshal(append(append([]byte{}, data...), block...), &check); err != nil {
		return false, fmt.Errorf("refusing to edit %s: result would not parse: %w", configPath, err)
	}
	if e := check.Projects[root]; e.TrustLevel == nil || *e.TrustLevel != "trusted" {
		return false, fmt.Errorf("refusing to edit %s: trust entry did not parse back", configPath)
	}

	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return false, err
	}
	f, err := os.OpenFile(configPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return false, err
	}
	if _, err := f.WriteString(block); err != nil {
		_ = f.Close()
		return false, fmt.Errorf("append trust to %s: %w", configPath, err)
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	return true, nil
}

// tomlString encodes s as a TOML basic string. JSON string escapes are a
// subset of TOML basic string escapes.
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func tomlStringArray(items ...string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = tomlString(s)
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

// CodexUserNotify returns the notify command set in the user's config.toml.
// A runner's -c notify replaces it, so __turn-end calls it in turn.
func CodexUserNotify(configPath string) ([]string, error) {
	data, err := os.ReadFile(configPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Notify []string `toml:"notify"`
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", configPath, err)
	}
	return cfg.Notify, nil
}
