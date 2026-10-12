// Package config loads the user's herdr-runner-kit configuration and merges it
// over the built-in defaults.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// Kind is the default agent and worktree choice for one task kind. Worktree
// means "use a worktree when the task's cwd is inside a git repository".
type Kind struct {
	Agent    string
	Worktree bool
}

// Telegram configures notifications. Empty fields use the Telegram channel
// plugin's own files, which are resolved at send time.
type Telegram struct {
	StateDir string   // overrides the plugin's channels/telegram directory
	ChatID   string   // overrides the allowFrom list of the plugin's access.json
	Notify   []string // event types that send a message
}

// Config is the effective configuration after merging the file over Default.
type Config struct {
	SchemaVersion        int
	Kinds                map[string]Kind
	DefaultKind          string
	WorktreeBranchPrefix string
	Telegram             Telegram
}

// NotifyEvents lists the event types the telegram.notify key accepts.
var NotifyEvents = []string{"needs-human", "failed", "stalled", "held", "done"}

// The file types use pointers so that an absent key keeps its default.
type fileConfig struct {
	SchemaVersion        *int                `yaml:"schemaVersion"`
	Kinds                map[string]fileKind `yaml:"kinds"`
	DefaultKind          *string             `yaml:"defaultKind"`
	WorktreeBranchPrefix *string             `yaml:"worktreeBranchPrefix"`
	Telegram             *fileTelegram       `yaml:"telegram"`
}

type fileKind struct {
	Agent    *string `yaml:"agent"`
	Worktree *bool   `yaml:"worktree"`
}

type fileTelegram struct {
	StateDir *string  `yaml:"stateDir"`
	ChatID   *string  `yaml:"chatId"`
	Notify   []string `yaml:"notify"`
}

// Path returns the config file location:
// ${XDG_CONFIG_HOME:-~/.config}/herdr-runner-kit/config.yaml.
func Path() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate config: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "herdr-runner-kit", "config.yaml"), nil
}

// Load reads the config file at path. A missing file yields the defaults.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// Parse merges YAML data over the defaults and validates the result. An
// unknown key is an error that names the key.
func Parse(data []byte) (Config, error) {
	var file fileConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, cleanYAMLError(err)
	}

	cfg := Default()
	if file.SchemaVersion != nil {
		cfg.SchemaVersion = *file.SchemaVersion
	}
	for name, fk := range file.Kinds {
		k := cfg.Kinds[name]
		if fk.Agent != nil {
			k.Agent = *fk.Agent
		}
		if fk.Worktree != nil {
			k.Worktree = *fk.Worktree
		}
		cfg.Kinds[name] = k
	}
	if file.DefaultKind != nil {
		cfg.DefaultKind = *file.DefaultKind
	}
	if file.WorktreeBranchPrefix != nil {
		cfg.WorktreeBranchPrefix = *file.WorktreeBranchPrefix
	}
	if t := file.Telegram; t != nil {
		if t.StateDir != nil {
			cfg.Telegram.StateDir = *t.StateDir
		}
		if t.ChatID != nil {
			cfg.Telegram.ChatID = *t.ChatID
		}
		if t.Notify != nil {
			cfg.Telegram.Notify = t.Notify
		}
	}
	return cfg, cfg.Validate()
}

// Validate checks every value that later code relies on.
func (c Config) Validate() error {
	if c.SchemaVersion > SchemaVersion {
		return fmt.Errorf("schemaVersion %d is newer than this binary supports (%d): upgrade herdr-runner-kit",
			c.SchemaVersion, SchemaVersion)
	}
	if c.SchemaVersion < 1 {
		return fmt.Errorf("schemaVersion must be at least 1, got %d", c.SchemaVersion)
	}
	for name, k := range c.Kinds {
		if name == "" || strings.ContainsAny(name, " \t:") {
			return fmt.Errorf("kinds: invalid kind name %q", name)
		}
		if k.Agent == "" {
			return fmt.Errorf("kinds.%s.agent is required", name)
		}
		if _, err := task.ParseAgentSpec(k.Agent); err != nil {
			return fmt.Errorf("kinds.%s.agent: %w", name, err)
		}
	}
	if _, ok := c.Kinds[c.DefaultKind]; !ok {
		return fmt.Errorf("defaultKind %q is not defined under kinds", c.DefaultKind)
	}
	if strings.ContainsAny(c.WorktreeBranchPrefix, " \t~^:?*[\\") {
		return fmt.Errorf("worktreeBranchPrefix %q is not a valid git branch prefix", c.WorktreeBranchPrefix)
	}
	for _, ev := range c.Telegram.Notify {
		if !slices.Contains(NotifyEvents, ev) {
			return fmt.Errorf("telegram.notify: unknown event %q (known: %s)", ev, strings.Join(NotifyEvents, ", "))
		}
	}
	return nil
}

// cleanYAMLError turns yaml.v3's "field x not found in type config.fileKind"
// into a message that names the key without exposing internal type names.
func cleanYAMLError(err error) error {
	msg := err.Error()
	if !strings.Contains(msg, "not found in type") {
		return err
	}
	var lines []string
	for line := range strings.SplitSeq(msg, "\n") {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, " not found in type"); i >= 0 {
			line = strings.Replace(line[:i], "field ", "unknown key ", 1)
		}
		if line != "" && !strings.HasPrefix(line, "yaml: unmarshal errors") {
			lines = append(lines, line)
		}
	}
	return errors.New(strings.Join(lines, "; "))
}
