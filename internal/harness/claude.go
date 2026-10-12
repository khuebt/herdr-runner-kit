package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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
		{Name: "claude-workspace-trust", Match: "Is this a project you created or one you trust", Action: NeedsHuman},
		{Name: "claude-external-imports", Match: "Allow external CLAUDE.md file imports", Action: NeedsHuman},
	}
}

// ClaudeSettings returns the per-task settings file content: one Stop hook
// that calls `<bin> __turn-end`. Claude merges it with the user's own hooks.
func ClaudeSettings(bin string) ([]byte, error) {
	if !filepath.IsAbs(bin) {
		return nil, fmt.Errorf("hook binary must be an absolute path, got %q", bin)
	}
	hook := map[string]any{"type": "command", "command": ShellQuote(bin) + " __turn-end --harness claude"}
	settings := map[string]any{
		"hooks": map[string]any{
			"Stop": []any{map[string]any{"hooks": []any{hook}}},
		},
	}
	return json.MarshalIndent(settings, "", "  ")
}

// ClaudeConfigPath returns the file holding Claude's per-project trust:
// $CLAUDE_CONFIG_DIR/.claude.json, or ~/.claude.json.
func ClaudeConfigPath() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, ".claude.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

const claudeTrustFlag = "hasTrustDialogAccepted"

// claudeImportFlags approve a CLAUDE.md that imports files from outside the
// project. They are security decisions, so they are only ever copied.
var claudeImportFlags = []string{"hasClaudeMdExternalIncludesApproved", "hasClaudeMdExternalIncludesWarningShown"}

// PrepareClaudeTrust lets a runner's Claude start at runnerPath without
// dialogs. Workspace trust is granted to runnerPath, because running spawn
// there is the user's consent, the same rule the kit applies to Codex. The
// external-import approvals are copied from origin's own entry only, so the
// kit never approves imports the user did not. It returns the flags it set.
func PrepareClaudeTrust(configPath, origin, runnerPath string) ([]string, error) {
	info, err := os.Stat(configPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil // Claude has never run with this config dir
	}
	if err != nil {
		return nil, err
	}
	root, err := readJSONObject(configPath)
	if err != nil {
		return nil, err
	}
	projects, _ := root["projects"].(map[string]any)
	src, _ := projects[filepath.Clean(origin)].(map[string]any)
	dst, _ := projects[filepath.Clean(runnerPath)].(map[string]any)
	if dst == nil {
		dst = map[string]any{}
	}
	var set []string
	if dst[claudeTrustFlag] != true {
		dst[claudeTrustFlag] = true
		set = append(set, claudeTrustFlag)
	}
	for _, flag := range claudeImportFlags {
		if src[flag] == true && dst[flag] != true {
			dst[flag] = true
			set = append(set, flag)
		}
	}
	if len(set) == 0 {
		return nil, nil
	}
	projects[filepath.Clean(runnerPath)] = dst

	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := fsutil.WriteFileAtomic(configPath, append(data, '\n'), info.Mode().Perm()); err != nil {
		return nil, fmt.Errorf("write %s: %w", configPath, err)
	}
	// Claude rewrites this file itself, so confirm the flags landed.
	ok, err := ClaudeTrustPresent(configPath, runnerPath, set)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("trust flags for %s were overwritten in %s", runnerPath, configPath)
	}
	return set, nil
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
