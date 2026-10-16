// Package harness keeps every agent CLI detail in one place: launch flags,
// skill syntax, turn-end hooks, trust preparation and known startup dialogs.
// The launch rules follow a live probe of herdr 0.9.1, Claude Code 2.1.296
// and codex-cli 0.162.1:
//
//   - The agent starts with no prompt. A prompt passed as a launch argument
//     makes `herdr agent start` wait for the whole first turn.
//   - The first prompt goes through `herdr agent prompt`. herdr uses bracketed
//     paste, so `/ak:plan` and `$ak-plan` both submit without a settle delay.
//   - A turn-end hook calls `hrk __turn-end`. A turn-end is only
//     a hint, because agents end turns while background work continues.
package harness

import (
	"fmt"
	"io"
	"slices"
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
	// Dir is the task folder holding the files Files returned; empty when
	// none were written.
	Dir string
}

// FilesInput are the inputs to an adapter's per-task runner files.
type FilesInput struct {
	Bin      string // absolute path of the kit binary the hooks call
	Contract string // the report contract text
}

// File is one per-task runner file, written into the task folder with mode
// 0600.
type File struct {
	Name string
	Data []byte
}

// TrustInput says where a runner starts, for an adapter's trust
// preparation.
type TrustInput struct {
	Cwd        string // the folder spawn ran in
	RepoRoot   string // its repository root, or empty outside one
	RunnerPath string // the folder the agent starts in: the worktree or Cwd
	// ClaudeConfigDir is the task's Claude config folder; empty is Claude's
	// default.
	ClaudeConfigDir string
}

// TrustResult is the event the launcher records after trust preparation;
// an empty Event records none.
type TrustResult struct {
	Event  string
	Detail string
}

// Screen reads the last lines of the runner's pane.
type Screen func(lines int) (string, error)

// TurnInput is what a turn-end hook gave the kit.
type TurnInput struct {
	Payload []byte
	Screen  Screen
}

// TurnSignal is an adapter's reading of one turn end. The runner acts on it
// in a fixed order: a confirmed limit, an ignored turn, the thread, a limit
// on screen, low context, background work, and only then the nudge.
type TurnSignal struct {
	// Ignore says the hook is not the end of one of the runner's own turns,
	// or ended it with nothing the kit can act on.
	Ignore bool
	// Thread is the agent's conversation thread. The first one seen is the
	// runner's; turns of any other thread are ignored.
	Thread string
	// LimitConfirmed says the agent itself ended the turn on a usage limit.
	LimitConfirmed bool
	// LimitOnScreen says the agent's own usage-limit line shows.
	LimitOnScreen bool
	// Background says the agent ended its turn with background work still
	// running, so it wakes on its own later.
	Background bool
	// ContextLeft is the percent of its context the agent has left, valid
	// when ContextKnown.
	ContextLeft  int
	ContextKnown bool
}

// Checks is what doctor checks for one harness.
type Checks struct {
	Bin         string   // the agent's command
	Flags       []string // flags its --help must still list
	Integration string   // the herdr integration that reports its state
	// Required makes a missing binary or integration a failure rather than a
	// warning.
	Required bool
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
	// IdleWhileShown says herdr reports the agent idle while the dialog is
	// still up, so an answer shows only as the dialog leaving the screen.
	IdleWhileShown bool
}

// InputState is an adapter's reading of the agent's input box.
type InputState int

// Input states. InputUnknown is the safe answer: the launcher never
// resubmits on it.
const (
	InputUnknown InputState = iota
	// InputEmpty says the input box holds nothing.
	InputEmpty
	// InputHolds says the input box still holds the prompt that was sent.
	InputHolds
)

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
	// ShowsUsageLimit reports whether the last lines of screen hold the
	// error line the agent itself draws when its account hit a usage limit.
	// Conversation text and tool output never count.
	ShowsUsageLimit(screen string) bool
	// InputState reads screen's input box against prompt, the first prompt
	// the kit sent. It answers InputHolds only when the box shows that
	// prompt, so one extra Enter submits it; an agent whose input cannot be
	// read reliably answers InputUnknown.
	InputState(screen, prompt string) InputState
	// ProgressText returns the part of a working agent's screen that
	// changes only when the agent makes progress: the agent's own animated
	// status lines, input box and footer are dropped, and the elapsed-time
	// counters left are masked.
	ProgressText(screen string) string
	// ShowsRetry reports whether the agent's own status line says it is
	// retrying a failed request, which is progress of a kind.
	ShowsRetry(screen string) bool
	// SelfResolvingPrompts lists the prompts the agent answers itself
	// after a timeout.
	SelfResolvingPrompts() []SelfResolvingPrompt
	// ContextLeft reads how much of its context window the agent's own
	// footer says is left, in percent. ok is false when the screen shows
	// none, or the agent tells the kit through hooks instead.
	ContextLeft(screen string) (pct int, ok bool)
	// Files returns the per-task runner files LaunchArgs refers to.
	Files(FilesInput) ([]File, error)
	// PrepareTrust makes sure the agent trusts the folder spawn ran in, so
	// the runner starts without a trust dialog.
	PrepareTrust(TrustInput) (TrustResult, error)
	// ReadTurnEnd reads a turn-end hook's payload and, when it needs to, the
	// screen.
	ReadTurnEnd(TurnInput) (TurnSignal, error)
	// NudgeInHook reports whether the turn-end hook's reply carries the
	// nudge; otherwise the kit types it into the pane.
	NudgeInHook() bool
	// HookPayload reads a hook's input from its arguments or stdin. nil
	// means there is nothing to handle.
	HookPayload(args []string, stdin io.Reader) ([]byte, error)
	// HookReply is what the turn-end hook prints for the nudge block, or
	// "" for nothing.
	HookReply(block string) string
	// SessionContext is what the compaction hook prints to add request to
	// the model's context, or "" when the agent takes none.
	SessionContext(request string) string
	// WorksInBackground reports whether the agent's screen shows background
	// work still running. It returns screen's error when it cannot tell.
	WorksInBackground(screen Screen) (bool, error)
	// Checks lists what doctor checks.
	Checks() Checks
}

// adapters holds every harness's adapter, in task.Harnesses order.
var adapters = []Adapter{claude{}, codex{}, piAgent, ompAgent}

// All returns every adapter, in task.Harnesses order.
func All() []Adapter { return slices.Clone(adapters) }

// For returns the adapter for harness h.
func For(h task.Harness) (Adapter, error) {
	for _, a := range adapters {
		if a.Harness() == h {
			return a, nil
		}
	}
	return nil, fmt.Errorf("unsupported harness %q", h)
}

// readStdin reads a hook input from stdin, up to 1 MiB.
func readStdin(stdin io.Reader) ([]byte, error) {
	data, _ := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if data == nil {
		data = []byte{}
	}
	return data, nil
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

// usageLimitLines is how many of the pane's last lines are searched for an
// agent's usage-limit line. The agent draws it under the turn it stopped.
const usageLimitLines = 12

// limitLine describes the error line an agent draws under a turn that its
// account's usage limit stopped.
type limitLine struct {
	marker string // the glyph that starts the line
	indent int    // the most spaces the marker may follow
	// afterBlank requires a blank line above, as for a message of its own
	// rather than the first line of a tool's output.
	afterBlank bool
	phrases    []string // one must appear in the line or the lines wrapped under it
	context    []string // when set, one of these must appear as well
}

// shown reports whether the last usageLimitLines lines of screen hold l.
func (l limitLine) shown(screen string) bool {
	lines := strings.Split(strings.TrimRight(screen, " \t\r\n"), "\n")
	for i := max(0, len(lines)-usageLimitLines); i < len(lines); i++ {
		body := strings.TrimLeft(lines[i], " ")
		if !strings.HasPrefix(body, l.marker) || len(lines[i])-len(body) > l.indent {
			continue
		}
		if l.afterBlank && i > 0 && strings.TrimSpace(lines[i-1]) != "" {
			continue
		}
		block := []string{body}
		for j := i + 1; j < len(lines) && strings.TrimSpace(lines[j]) != ""; j++ {
			block = append(block, lines[j])
		}
		text := Normalize(strings.Join(block, "\n"))
		if containsAny(text, l.phrases) && (len(l.context) == 0 || containsAny(text, l.context)) {
			return true
		}
	}
	return false
}

// containsAny reports whether normalized text holds one of phrases.
func containsAny(text string, phrases []string) bool {
	for _, p := range phrases {
		if p := Normalize(p); p != "" && strings.Contains(text, p) {
			return true
		}
	}
	return false
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
