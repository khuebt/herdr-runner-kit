// Package harness keeps every agent CLI detail in one place: launch flags,
// skill syntax, turn-end hooks, trust preparation and known startup dialogs.
// The launch rules follow a live probe of herdr 0.9.1, Claude Code 2.1.296
// and codex-cli 0.162.1:
//
//   - The agent starts with no prompt. A prompt passed as a launch argument
//     makes `herdr agent start` wait for the whole first turn.
//   - The first prompt goes through `herdr agent prompt`. herdr uses bracketed
//     paste, so `/ak:plan` and `$ak-plan` both submit without a settle delay.
//   - A turn-end hook calls `herdr-runner-kit __turn-end`. A turn-end is only
//     a hint, because agents end turns while background work continues.
package harness

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// LaunchOptions are the per-task inputs to an adapter's launch argv.
type LaunchOptions struct {
	Spec task.AgentSpec
	// Bin is the absolute path of the herdr-runner-kit binary, called by the
	// turn-end hook.
	Bin string
	// SettingsPath is the per-task Claude settings file holding the Stop hook.
	SettingsPath string
	// ContractPath is a file holding the report contract. Claude appends it
	// to its system prompt, which survives compaction. Codex receives the
	// contract in its first prompt instead. A file keeps the long text out of
	// the shell line herdr types into the pane.
	ContractPath string
}

// DialogAction says how the launcher handles a startup dialog.
type DialogAction int

// Dialog actions.
const (
	// NeedsHuman leaves the dialog for the user and marks the task
	// needs-human. Trust dialogs are never answered by keys: Enter declines
	// on Claude, and Esc or Ctrl+C can accept on Codex.
	NeedsHuman DialogAction = iota
	// SendKeys answers the dialog with Keys and records a warning.
	SendKeys
)

// Dialog is a startup screen the launcher recognises by its text.
type Dialog struct {
	Name   string
	Match  string // phrase on the visible screen, compared after Normalize
	Action DialogAction
	Keys   []string // herdr logical keys, for SendKeys
}

// Adapter describes one agent CLI.
type Adapter interface {
	Harness() task.Harness
	// HerdrKind is the value for `herdr agent start --kind`.
	HerdrKind() string
	// LaunchArgs returns the agent argv that follows `--`. It never contains
	// the prompt.
	LaunchArgs(LaunchOptions) []string
	// RenderSkill turns "ak:plan" into this harness's invocation syntax.
	RenderSkill(skill string) string
	// FirstPrompt composes the text sent with `herdr agent prompt`.
	FirstPrompt(skill, prompt, contract string) string
	// Dialogs lists the known startup dialogs.
	Dialogs() []Dialog
	// UsageLimit lists the phrases the agent shows when its account hit a
	// usage limit, compared after Normalize.
	UsageLimit() []string
}

// For returns the adapter for harness h.
func For(h task.Harness) (Adapter, error) {
	switch h {
	case task.Claude:
		return claude{}, nil
	case task.Codex:
		return codex{}, nil
	}
	return nil, fmt.Errorf("unsupported harness %q", h)
}

// DetectDialog returns the first known dialog whose text is on screen.
func DetectDialog(a Adapter, screen string) (Dialog, bool) {
	text := Normalize(screen)
	for _, d := range a.Dialogs() {
		if strings.Contains(text, Normalize(d.Match)) {
			return d, true
		}
	}
	return Dialog{}, false
}

// Shows reports whether dialog d is on screen.
func Shows(screen string, d Dialog) bool {
	return d.Match != "" && strings.Contains(Normalize(screen), Normalize(d.Match))
}

// Normalize lowercases s and turns every run of characters that are not
// letters or digits into one space, so a phrase still matches when the agent
// wraps it across lines, draws box borders through it or changes its case.
func Normalize(s string) string {
	var b strings.Builder
	space := false
	for _, c := range s {
		if !unicode.IsLetter(c) && !unicode.IsDigit(c) {
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(unicode.ToLower(c))
	}
	return b.String()
}

// skillName strips an invocation prefix, so "/ak:plan", "$ak-plan" and
// "ak:plan" are all accepted.
func skillName(skill string) string {
	return strings.TrimLeft(strings.TrimSpace(skill), "/$")
}

// joinPrompt puts the rendered skill, the prompt and the contract together.
func joinPrompt(rendered, prompt, contract string) string {
	var parts []string
	head := strings.TrimSpace(strings.TrimSpace(rendered) + " " + strings.TrimSpace(prompt))
	if head != "" {
		parts = append(parts, head)
	}
	if c := strings.TrimSpace(contract); c != "" {
		parts = append(parts, c)
	}
	return strings.Join(parts, "\n\n")
}
