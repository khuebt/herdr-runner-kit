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
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
	// EnvFile is a dotenv file holding TELEGRAM_BOT_TOKEN and
	// TELEGRAM_CHAT_ID, used instead of the plugin's files.
	EnvFile  string
	StateDir string   // overrides the plugin's channels/telegram directory
	ChatID   string   // overrides the allowFrom list of the plugin's access.json
	Notify   []string // event types that send a message
}

// Fallback is an agent a runner switches to when its agent hits a usage
// limit.
type Fallback struct {
	Agent string
	// ClaudeConfig is the Claude config folder, as a path or a bare name
	// such as claude2, resolved when the runner switches. Empty keeps the
	// runner's own.
	ClaudeConfig string
}

// Config is the effective configuration after merging the file over Default.
type Config struct {
	SchemaVersion        int
	Kinds                map[string]Kind
	DefaultKind          string
	WorktreeBranchPrefix string
	Telegram             Telegram
	UpdateCheck          bool // doctor's daily look for a newer release
	// Language is the language runners write report summaries and
	// questions in, as free text such as "Vietnamese". Empty adds no rule.
	Language string
	// QuotaFallback lists, in order, the agents a runner switches to when
	// its agent hits a usage limit. Empty fails the task instead.
	QuotaFallback []Fallback
	// KeepFor is how long a kept runner's tab stays open after its task
	// finished. Zero keeps it until keep --off or stop.
	KeepFor time.Duration
	// StallAfter is how long a working agent may show no progress before
	// the kit nudges it. Zero turns the check off.
	StallAfter time.Duration
	// ContextHandoff lets the kit ask a runner near its context limit to
	// hand off once to a fresh session of its task. ContextHandoffAt is the
	// context left, in percent, at or below which a Codex runner is asked.
	ContextHandoff   bool
	ContextHandoffAt int
	// RunnerMemoryMax caps each runner's memory, as a systemd size such as
	// 16G or a share of RAM such as 25%, by running its agent in its own
	// systemd scope. MemoryOff starts agents without one.
	RunnerMemoryMax string
	// HeavySlots is how many heavy commands (tests, lint, typecheck,
	// builds) wrapped in hrk heavy run at once across every runner and the
	// main session. Zero runs them at once with no queue.
	HeavySlots int
	// WorktreeSetup maps a repository's main checkout, as a clean absolute
	// path, to the shell commands that run in each new runner worktree of
	// it before its agent starts.
	WorktreeSetup map[string][]string
	// RunnerInput is InputLocked or InputOpen: whether a runner whose agent
	// can lock its input takes prompts only from the kit, so text typed in
	// its tab never reaches the model.
	RunnerInput string
}

// The runnerInput values.
const (
	InputLocked = "locked"
	InputOpen   = "open"
)

// MemoryOff is the runnerMemoryMax value that turns the memory cap off.
const MemoryOff = "off"

// memorySize matches the runnerMemoryMax values systemd's MemoryMax takes
// that this kit accepts: a size in K, M, G or T, or 1 to 100 percent.
var memorySize = regexp.MustCompile(`^(?:[1-9][0-9]*[KMGT]|(?:[1-9][0-9]?|100)%)$`)

// The bounds of contextHandoffAt, in percent of the context left.
const (
	MinContextHandoffAt = 5
	MaxContextHandoffAt = 50
)

// MaxHeavySlots is the most heavySlots accepts.
const MaxHeavySlots = 64

// MinStallAfter is the shortest stallAfter: the progress check samples once
// a minute and needs a few samples before it acts.
const MinStallAfter = 2 * time.Minute

// NotifyEvents lists the event types the telegram.notify key accepts.
var NotifyEvents = []string{"needs-human", "failed", "stalled", "held", "done", "turn-ended-unreported", "usage-limit"}

// The file types use pointers so that an absent key keeps its default.
type fileConfig struct {
	SchemaVersion        *int                `yaml:"schemaVersion"`
	Kinds                map[string]fileKind `yaml:"kinds"`
	DefaultKind          *string             `yaml:"defaultKind"`
	WorktreeBranchPrefix *string             `yaml:"worktreeBranchPrefix"`
	Telegram             *fileTelegram       `yaml:"telegram"`
	UpdateCheck          *bool               `yaml:"updateCheck"`
	Language             *string             `yaml:"language"`
	QuotaFallback        []fileFallback      `yaml:"quotaFallback"`
	KeepFor              *string             `yaml:"keepFor"`
	StallAfter           *string             `yaml:"stallAfter"`
	ContextHandoff       *bool               `yaml:"contextHandoff"`
	ContextHandoffAt     *int                `yaml:"contextHandoffAt"`
	RunnerMemoryMax      *string             `yaml:"runnerMemoryMax"`
	HeavySlots           *int                `yaml:"heavySlots"`
	WorktreeSetup        map[string][]string `yaml:"worktreeSetup"`
	RunnerInput          *string             `yaml:"runnerInput"`
}

type fileFallback struct {
	Agent        string `yaml:"agent"`
	ClaudeConfig string `yaml:"claudeConfig"`
}

type fileKind struct {
	Agent    *string `yaml:"agent"`
	Worktree *bool   `yaml:"worktree"`
}

type fileTelegram struct {
	EnvFile  *string  `yaml:"envFile"`
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
	if file.UpdateCheck != nil {
		cfg.UpdateCheck = *file.UpdateCheck
	}
	if file.Language != nil {
		cfg.Language = strings.TrimSpace(*file.Language)
	}
	if file.QuotaFallback != nil {
		cfg.QuotaFallback = make([]Fallback, len(file.QuotaFallback))
		for i, f := range file.QuotaFallback {
			cfg.QuotaFallback[i] = Fallback{Agent: strings.TrimSpace(f.Agent), ClaudeConfig: strings.TrimSpace(f.ClaudeConfig)}
		}
	}
	if file.KeepFor != nil {
		d, err := parseDuration(*file.KeepFor)
		if err != nil {
			return Config{}, fmt.Errorf("keepFor: %w", err)
		}
		cfg.KeepFor = d
	}
	if file.StallAfter != nil {
		d, err := parseDuration(*file.StallAfter)
		if err != nil {
			return Config{}, fmt.Errorf("stallAfter: %w", err)
		}
		cfg.StallAfter = d
	}
	if file.ContextHandoff != nil {
		cfg.ContextHandoff = *file.ContextHandoff
	}
	if file.ContextHandoffAt != nil {
		cfg.ContextHandoffAt = *file.ContextHandoffAt
	}
	if file.RunnerMemoryMax != nil {
		cfg.RunnerMemoryMax = strings.TrimSpace(*file.RunnerMemoryMax)
	}
	if file.HeavySlots != nil {
		cfg.HeavySlots = *file.HeavySlots
	}
	if file.RunnerInput != nil {
		cfg.RunnerInput = strings.TrimSpace(*file.RunnerInput)
	}
	if file.WorktreeSetup != nil {
		cfg.WorktreeSetup = make(map[string][]string, len(file.WorktreeSetup))
		for repo, cmds := range file.WorktreeSetup {
			key, err := expandHome(strings.TrimSpace(repo))
			if err != nil {
				return Config{}, fmt.Errorf("worktreeSetup: %w", err)
			}
			if key != "" {
				key = filepath.Clean(key)
			}
			cfg.WorktreeSetup[key] = cmds
		}
	}
	if t := file.Telegram; t != nil {
		if t.EnvFile != nil {
			cfg.Telegram.EnvFile = *t.EnvFile
		}
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
	if err := CheckLanguage(c.Language); err != nil {
		return fmt.Errorf("language: %w", err)
	}
	for i, f := range c.QuotaFallback {
		if f.Agent == "" {
			return fmt.Errorf("quotaFallback[%d].agent is required", i)
		}
		spec, err := task.ParseAgentSpec(f.Agent)
		if err != nil {
			return fmt.Errorf("quotaFallback[%d].agent: %w", i, err)
		}
		if f.ClaudeConfig != "" && spec.Harness != task.Claude {
			return fmt.Errorf("quotaFallback[%d].claudeConfig needs a Claude agent, not %s", i, spec.Harness)
		}
		if strings.ContainsFunc(f.ClaudeConfig, unicode.IsControl) {
			return fmt.Errorf("quotaFallback[%d].claudeConfig %q must be one line", i, f.ClaudeConfig)
		}
	}
	if c.KeepFor < 0 {
		return fmt.Errorf("keepFor must not be negative, got %s", c.KeepFor)
	}
	if c.StallAfter != 0 && c.StallAfter < MinStallAfter {
		return fmt.Errorf("stallAfter must be 0 (off) or at least %s, got %s", MinStallAfter, c.StallAfter)
	}
	if c.ContextHandoffAt < MinContextHandoffAt || c.ContextHandoffAt > MaxContextHandoffAt {
		return fmt.Errorf("contextHandoffAt must be from %d to %d (percent of context left), got %d", MinContextHandoffAt, MaxContextHandoffAt, c.ContextHandoffAt)
	}
	if c.RunnerMemoryMax != MemoryOff && !memorySize.MatchString(c.RunnerMemoryMax) {
		return fmt.Errorf("runnerMemoryMax must be a size such as 16G or 8192M, a share of RAM such as 25%%, or off, got %q", c.RunnerMemoryMax)
	}
	if c.HeavySlots < 0 || c.HeavySlots > MaxHeavySlots {
		return fmt.Errorf("heavySlots must be from 0 (off) to %d, got %d", MaxHeavySlots, c.HeavySlots)
	}
	if c.RunnerInput != InputLocked && c.RunnerInput != InputOpen {
		return fmt.Errorf("runnerInput must be %s or %s, got %q", InputLocked, InputOpen, c.RunnerInput)
	}
	for repo, cmds := range c.WorktreeSetup {
		if !filepath.IsAbs(repo) {
			return fmt.Errorf("worktreeSetup: %q is not an absolute path or ~/ path of a repository", repo)
		}
		if len(cmds) == 0 {
			return fmt.Errorf("worktreeSetup.%s: list at least one command", repo)
		}
		for i, cmd := range cmds {
			if strings.TrimSpace(cmd) == "" {
				return fmt.Errorf("worktreeSetup.%s[%d] is empty", repo, i)
			}
		}
	}
	for _, ev := range c.Telegram.Notify {
		if !slices.Contains(NotifyEvents, ev) {
			return fmt.Errorf("telegram.notify: unknown event %q (known: %s)", ev, strings.Join(NotifyEvents, ", "))
		}
	}
	return nil
}

// SetupFor returns the worktree setup commands of the repository whose main
// checkout is root. A key that is a symlink to root matches too.
func (c Config) SetupFor(root string) []string {
	if root == "" {
		return nil
	}
	for repo, cmds := range c.WorktreeSetup {
		if repo == root {
			return cmds
		}
		if resolved, err := filepath.EvalSymlinks(repo); err == nil && resolved == root {
			return cmds
		}
	}
	return nil
}

// expandHome replaces a leading ~/ in path with the home directory.
func expandHome(path string) (string, error) {
	rest, ok := strings.CutPrefix(path, "~/")
	if !ok {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, rest), nil
}

// parseDuration reads a duration such as 4h or 90m; a bare 0 means none.
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration such as 4h or 90m", s)
	}
	return d, nil
}

// maxLanguage is the longest language value, in characters.
const maxLanguage = 40

// CheckLanguage accepts a report language: free text on one line of at most
// 40 characters, since it goes into the runner's contract as is.
func CheckLanguage(s string) error {
	if strings.ContainsFunc(s, unicode.IsControl) {
		return fmt.Errorf("%q must be one line of plain text", s)
	}
	if n := utf8.RuneCountInString(s); n > maxLanguage {
		return fmt.Errorf("%q has %d characters, more than %d", s, n, maxLanguage)
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
