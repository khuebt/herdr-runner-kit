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
	if c := strings.TrimSpace(o.Contract); c != "" {
		args = append(args, "--append-system-prompt", c)
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
	hook := map[string]any{"type": "command", "command": shellQuote(bin) + " __turn-end --harness claude"}
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

// claudeTrustFlags are the per-project flags that suppress Claude's
// workspace-trust and external-import dialogs.
var claudeTrustFlags = []string{
	"hasTrustDialogAccepted",
	"hasClaudeMdExternalIncludesApproved",
	"hasClaudeMdExternalIncludesWarningShown",
}

// CopyClaudeTrust copies the trust flags the user already granted to origin
// onto the worktree path, so a runner's Claude starts without dialogs. Claude
// treats a folder as trusted when it or any parent is, so workspace trust is
// also taken from origin's parents. The import approvals are taken from
// origin's own entry only. It never grants more than the user did, and it
// returns the flags it set.
func CopyClaudeTrust(configPath, origin, worktree string) ([]string, error) {
	info, err := os.Stat(configPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	root, err := readJSONObject(configPath)
	if err != nil {
		return nil, err
	}
	projects, _ := root["projects"].(map[string]any)
	dst, _ := projects[filepath.Clean(worktree)].(map[string]any)
	if dst == nil {
		dst = map[string]any{}
	}
	var set []string
	for _, flag := range claudeTrustFlags {
		if originGrants(projects, origin, flag) && dst[flag] != true {
			dst[flag] = true
			set = append(set, flag)
		}
	}
	if len(set) == 0 {
		return nil, nil
	}
	projects[filepath.Clean(worktree)] = dst

	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(configPath, append(data, '\n'), info.Mode().Perm()); err != nil {
		return nil, fmt.Errorf("write %s: %w", configPath, err)
	}
	// Claude rewrites this file itself, so confirm the flags landed.
	ok, err := ClaudeTrustPresent(configPath, worktree, set)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("trust flags for %s were overwritten in %s", worktree, configPath)
	}
	return set, nil
}

// originGrants reports whether the user granted flag to origin, or for the
// workspace-trust flag, to any of its parents.
func originGrants(projects map[string]any, origin, flag string) bool {
	dir := filepath.Clean(origin)
	for {
		entry, _ := projects[dir].(map[string]any)
		if entry[flag] == true {
			return true
		}
		parent := filepath.Dir(dir)
		if flag != "hasTrustDialogAccepted" || parent == dir {
			return false
		}
		dir = parent
	}
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

// shellQuote quotes s for /bin/sh.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+:@") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
