package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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
		// The user's untrusted global hooks raise this on every launch, and
		// herdr reports the screen as idle. Esc skips them for this session
		// only and persists nothing.
		{Name: "codex-hooks-review", Match: "Hooks need review", Action: SendKeys, Keys: []string{"esc"}},
	}
}

// UsageLimit holds the lines codex-cli 0.162.1 shows when the account's
// usage limit stops a turn.
func (codex) UsageLimit() []string {
	return []string{"You've hit your usage limit", "You've reached your usage limit"}
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
