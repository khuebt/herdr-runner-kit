package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/fsutil"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

type claude struct{}

func (claude) Harness() task.Harness { return task.Claude }
func (claude) HerdrKind() string     { return "claude" }

func (claude) LaunchArgs(o LaunchOptions) []string {
	args := []string{"--dangerously-skip-permissions"}
	if o.Spec.Model != "" {
		args = append(args, "--model", o.Spec.Model)
	}
	if o.Spec.Effort != "" {
		args = append(args, "--effort", o.Spec.Effort)
	}
	if o.SettingsPath != "" {
		args = append(args, "--settings", o.SettingsPath)
	}
	if o.ContractPath != "" {
		args = append(args, "--append-system-prompt-file", o.ContractPath)
	}
	return args
}

// RenderSkill keeps the skill name as written: Claude resolves both
// "/ak:plan" and "/ak-plan".
func (claude) RenderSkill(skill string) string {
	name := skillName(skill)
	if name == "" {
		return ""
	}
	return "/" + name
}

// FirstPrompt leaves the contract out: Claude gets it as a system prompt.
func (c claude) FirstPrompt(skill, prompt, _ string) string {
	return joinPrompt(c.RenderSkill(skill), prompt, "")
}

func (claude) Dialogs() []Dialog {
	return []Dialog{
		{Name: "claude-workspace-trust", Match: "is this a project you created or one you trust", Action: NeedsHuman},
		{Name: "claude-external-imports", Match: "Allow external CLAUDE.md file imports", Action: NeedsHuman},
	}
}

// claudeLimits are the starts of the messages Claude Code 2.1.296 shows
// for an account limit, as its own usage-limit check lists them.
var claudeLimits = []string{
	"You've hit your",
	"You've reached your",
	"You're out of extra usage",
	"You're out of usage credits",
}

// claudeLimit is the line Claude draws under a turn its account limit
// stopped: "  ⎿  You've hit your session limit · resets 3pm (Asia/Bangkok)"
// with "/upgrade …" wrapped below. A tool's output starts with the same
// glyph, but right under the tool's own line rather than after a blank one.
var claudeLimit = limitLine{
	marker:     "⎿",
	indent:     2,
	afterBlank: true,
	phrases:    claudeLimits,
	context:    []string{"resets", "upgrade"},
}

func (claude) ShowsUsageLimit(screen string) bool { return claudeLimit.shown(screen) }

// claudeSpinner is the status line Claude draws under a running turn, a
// glyph and a verb with an ellipsis: "✽ Harmonizing… (14s · ↓ 156 tokens)".
var claudeSpinner = regexp.MustCompile(`^\s*\S\s+\S[^…]*…`)

// claudeBoxEdge is a border of the input box, which sits above the footer.
var claudeBoxEdge = regexp.MustCompile(`^─{20,}`)

// claudeRetry is the status line of a request Claude retries, as Claude
// Code 2.1.300 draws it: "✻ Connection refused … · Retrying in 8s ·
// attempt 5/10".
var claudeRetry = regexp.MustCompile(`(?i)\bretrying in \d+\s*s\b.*\battempt \d+/\d+`)

// ProgressText keeps what is above Claude's input box, without the spinner
// line and the tip under it, but with the spinner's token count, which
// grows while the model writes.
func (claude) ProgressText(screen string) string {
	lines := strings.Split(screen, "\n")
	edges := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if claudeBoxEdge.MatchString(lines[i]) {
			if edges++; edges == 2 {
				lines = lines[:i]
				break
			}
		}
	}
	var out []string
	inTip := false
	for _, l := range lines {
		body := strings.TrimSpace(l)
		switch {
		case claudeSpinner.MatchString(l) && strings.Contains(l, "("):
			out = append(out, "spinner "+strings.Join(tokenCount.FindAllString(l, -1), " "))
			inTip = false
			continue
		case strings.HasPrefix(body, "⎿") && strings.Contains(body, "Tip:"):
			inTip = true
			continue
		case inTip && strings.HasPrefix(l, "     ") && body != "":
			continue
		}
		inTip = false
		out = append(out, maskDurations(l))
	}
	return joinLines(out)
}

func (claude) ShowsRetry(screen string) bool {
	return anyLineMatches(lastLines(screen, statusLines), claudeRetry)
}

// claudeAutoDeny is the line of a permission prompt that Claude Code
// answers itself on an unattended session, as it drew it in October 2026
// under its rm safety check: "⚠ Claude Code will automatically deny this
// request in 1:49, to avoid blocking progress on an unattended session".
// The prompt is denied two minutes after it opens.
var claudeAutoDeny = SelfResolvingPrompt{
	Name:    "Claude's safety",
	Match:   "Claude Code will automatically deny this request in",
	Timeout: 2 * time.Minute,
}

func (claude) SelfResolvingPrompts() []SelfResolvingPrompt {
	return []SelfResolvingPrompt{claudeAutoDeny}
}

// ClaudeUsageLimit reads a Claude hook input and returns the message of a
// StopFailure that a usage limit caused: Claude ends such a turn with a
// rate_limit or billing_error and its limit message instead of calling the
// Stop hook. It reports whether payload is a StopFailure at all.
func ClaudeUsageLimit(payload []byte) (message string, stopFailure bool) {
	var in struct {
		Event   string `json:"hook_event_name"`
		Error   string `json:"error"`
		Message string `json:"last_assistant_message"`
	}
	if json.Unmarshal(payload, &in) != nil || in.Event != "StopFailure" {
		return "", false
	}
	if (in.Error == "rate_limit" || in.Error == "billing_error") && containsAny(Normalize(in.Message), claudeLimits) {
		return strings.TrimSpace(in.Message), true
	}
	return "", true
}

// ClaudeSettings returns the per-task settings file content: Stop and
// StopFailure hooks that call `<bin> __turn-end`, which tells the two apart
// by the hook input. Claude merges them with the user's own hooks.
func ClaudeSettings(bin string) ([]byte, error) {
	if !filepath.IsAbs(bin) {
		return nil, fmt.Errorf("hook binary must be an absolute path, got %q", bin)
	}
	hook := map[string]any{"type": "command", "command": ShellQuote(bin) + " __turn-end --harness claude"}
	settings := map[string]any{
		"hooks": map[string]any{
			"Stop":        []any{map[string]any{"hooks": []any{hook}}},
			"StopFailure": []any{map[string]any{"hooks": []any{hook}}},
		},
	}
	return json.MarshalIndent(settings, "", "  ")
}

// DefaultClaudeConfigDir maps a Claude config folder to the value runners
// store: ~/.claude becomes "", because Claude keeps that default account in
// ~/.claude.json, and CLAUDE_CONFIG_DIR=~/.claude would point it at
// ~/.claude/.claude.json instead. Any other value is returned unchanged.
func DefaultClaudeConfigDir(dir string) string {
	home, err := os.UserHomeDir()
	if err != nil || dir == "" {
		return dir
	}
	clean := dir
	if rest, ok := strings.CutPrefix(clean, "~/"); ok {
		clean = filepath.Join(home, rest)
	}
	if filepath.Clean(clean) == filepath.Join(home, ".claude") {
		return ""
	}
	return dir
}

// ClaudeConfigPath returns the file holding Claude's per-project trust for
// the config folder dir: dir/.claude.json, or ~/.claude.json when dir is
// empty, which is how Claude runs without CLAUDE_CONFIG_DIR.
func ClaudeConfigPath(dir string) (string, error) {
	if dir != "" {
		return filepath.Join(dir, ".claude.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

// ClaudeTrustFlag is the per-project key Claude sets when the user accepts
// its workspace trust dialog.
const ClaudeTrustFlag = "hasTrustDialogAccepted"

// claudeImportFlags approve a CLAUDE.md that imports files from outside the
// project. They are security decisions, so they are only ever copied.
var claudeImportFlags = []string{"hasClaudeMdExternalIncludesApproved", "hasClaudeMdExternalIncludesWarningShown"}

// Reasons PrepareClaudeTrust gives for writing nothing.
const (
	// TrustSkipNoConfig: Claude has never run with the config folder, so its
	// config file is missing. The kit never creates it.
	TrustSkipNoConfig = "no-config"
	// TrustSkipAlreadyTrusted: every flag is already set for the folder.
	TrustSkipAlreadyTrusted = "already-trusted"
)

// PrepareClaudeTrust lets a runner's Claude start at runnerPath without
// dialogs. Workspace trust is granted to runnerPath, because running spawn
// there is the user's consent, the same rule the kit applies to Codex. The
// external-import approvals are copied from origin's own entry only, so the
// kit never approves imports the user did not. It returns the flags it set,
// or, when it set none, the reason (TrustSkipNoConfig or
// TrustSkipAlreadyTrusted).
func PrepareClaudeTrust(configPath, origin, runnerPath string) (set []string, skipped string, err error) {
	info, err := os.Stat(configPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, TrustSkipNoConfig, nil
	}
	if err != nil {
		return nil, "", err
	}
	root, err := readJSONObject(configPath)
	if err != nil {
		return nil, "", err
	}
	projects, _ := root["projects"].(map[string]any)
	src, _ := projects[filepath.Clean(origin)].(map[string]any)
	dst, _ := projects[filepath.Clean(runnerPath)].(map[string]any)
	if dst == nil {
		dst = map[string]any{}
	}
	if dst[ClaudeTrustFlag] != true {
		dst[ClaudeTrustFlag] = true
		set = append(set, ClaudeTrustFlag)
	}
	for _, flag := range claudeImportFlags {
		if src[flag] == true && dst[flag] != true {
			dst[flag] = true
			set = append(set, flag)
		}
	}
	if len(set) == 0 {
		return nil, TrustSkipAlreadyTrusted, nil
	}
	projects[filepath.Clean(runnerPath)] = dst

	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, "", err
	}
	if err := fsutil.WriteFileAtomic(configPath, append(data, '\n'), info.Mode().Perm()); err != nil {
		return nil, "", fmt.Errorf("write %s: %w", configPath, err)
	}
	// Claude rewrites this file itself, so confirm the flags landed.
	ok, err := ClaudeTrustPresent(configPath, runnerPath, set)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		return nil, "", fmt.Errorf("trust flags for %s were overwritten in %s", runnerPath, configPath)
	}
	return set, "", nil
}

// ClaudeTrusted returns the first of paths that configPath marks trusted,
// matching exact cleaned keys as PrepareClaudeTrust writes them, or "" when
// none is. A missing file is an error that wraps fs.ErrNotExist.
func ClaudeTrusted(configPath string, paths []string) (string, error) {
	root, err := readJSONObject(configPath)
	if err != nil {
		return "", err
	}
	projects, _ := root["projects"].(map[string]any)
	for _, p := range paths {
		if entry, _ := projects[filepath.Clean(p)].(map[string]any); entry[ClaudeTrustFlag] == true {
			return filepath.Clean(p), nil
		}
	}
	return "", nil
}

// ClaudeTrustPresent reports whether every flag is true for path.
func ClaudeTrustPresent(configPath, path string, flags []string) (bool, error) {
	root, err := readJSONObject(configPath)
	if err != nil {
		return false, err
	}
	projects, _ := root["projects"].(map[string]any)
	entry, _ := projects[filepath.Clean(path)].(map[string]any)
	for _, flag := range flags {
		if entry[flag] != true {
			return false, nil
		}
	}
	return true, nil
}

// readJSONObject decodes with UseNumber so large integers survive a rewrite.
func readJSONObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if root == nil {
		root = map[string]any{}
	}
	if _, ok := root["projects"].(map[string]any); !ok {
		root["projects"] = map[string]any{}
	}
	return root, nil
}

// ShellQuote quotes s for a POSIX shell.
func ShellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+:@") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ClaudeBlock returns the Stop hook output that keeps Claude working, with
// reason as its next instruction.
func ClaudeBlock(reason string) string {
	out, _ := json.Marshal(map[string]string{"decision": "block", "reason": reason})
	return string(out)
}
