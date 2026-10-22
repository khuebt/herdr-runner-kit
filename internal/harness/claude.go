package harness

import (
	"bytes"
	"encoding/json"
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
	if o.Dir != "" {
		// The contract goes into the system prompt, which survives
		// compaction; a file keeps the long text out of the shell line herdr
		// types into the pane.
		args = append(args, "--settings", filepath.Join(o.Dir, claudeSettingsFile),
			"--append-system-prompt-file", filepath.Join(o.Dir, claudeContractFile))
		if !o.MCP {
			// No runner has called an MCP tool, and the servers' tool
			// schemas ride on every call.
			args = append(args, "--strict-mcp-config")
		}
	}
	return args
}

// The per-task files a Claude runner starts with.
const (
	claudeSettingsFile = "claude-settings.json" // the turn-end, compaction and session-start hooks
	claudeContractFile = "contract.md"
)

func (claude) Files(in FilesInput) ([]File, error) {
	settings, err := ClaudeSettings(ClaudeSettingsInput{Bin: in.Bin, InputLocked: in.InputLocked})
	if err != nil {
		return nil, err
	}
	return []File{
		{Name: claudeSettingsFile, Data: settings},
		{Name: claudeContractFile, Data: []byte(in.Contract + "\n")},
	}, nil
}

// PrepareTrust grants workspace trust to the runner's folder in the task's
// Claude config; see PrepareClaudeTrust.
func (claude) PrepareTrust(in TrustInput) (TrustResult, error) {
	path, err := ClaudeConfigPath(in.ClaudeConfigDir)
	if err != nil {
		return TrustResult{}, err
	}
	set, skipped, err := PrepareClaudeTrust(path, in.Cwd, in.RunnerPath)
	if err != nil {
		return TrustResult{}, fmt.Errorf("prepare Claude trust: %w", err)
	}
	switch skipped {
	case TrustSkipNoConfig:
		return TrustResult{Event: "claude-trust-skipped", Detail: "Claude has never run with this config dir: " + path + " is missing"}, nil
	case TrustSkipAlreadyTrusted:
		return TrustResult{Event: "claude-trust-skipped", Detail: in.RunnerPath + " is already trusted in " + path}, nil
	}
	return TrustResult{Event: "claude-trusted", Detail: "set " + strings.Join(set, ", ") + " for " + in.RunnerPath}, nil
}

// ReadTurnEnd reads a Stop or StopFailure hook input. A StopFailure is a
// confirmed usage limit, an API error that may pass, or a failure the kit
// cannot act on; Claude ignores a StopFailure hook's output in every case.
func (c claude) ReadTurnEnd(in TurnInput) (TurnSignal, error) {
	if message, stopFailure := ClaudeUsageLimit(in.Payload); stopFailure {
		if message != "" {
			return TurnSignal{LimitConfirmed: true}, nil
		}
		if apiErr, ok := ClaudeTransientError(in.Payload); ok {
			return TurnSignal{APIError: apiErr}, nil
		}
		return TurnSignal{Ignore: true}, nil
	}
	subagents, _ := ClaudeBackgroundEnding(in.Payload)
	return TurnSignal{Background: c.background(in), Subagents: subagents > 0}, nil
}

// HookTranscript reads the session transcript a hook input names: every
// Claude hook input carries it.
func (claude) HookTranscript(payload []byte) string {
	var in struct {
		TranscriptPath string `json:"transcript_path"`
	}
	_ = json.Unmarshal(payload, &in)
	return in.TranscriptPath
}

// background reports whether Claude ended its turn with shells or subagents
// still running, so it wakes on its own later. The Stop hook input lists
// them; the mode line is read only when the input has no list, because it
// draws no count for subagents that run no shell, nor for a second or more
// after they start.
func (c claude) background(in TurnInput) bool {
	if running, known := ClaudeBackgroundWork(in.Payload); known {
		return running > 0
	}
	busy, _ := c.WorksInBackground(in.Screen)
	return busy
}

// NudgeInHook is true: the Stop hook returns the nudge as a block.
func (claude) NudgeInHook() bool { return true }

// DoneAtTurnEnd is true: a Claude runner may report done while a background
// subagent still runs, and works on when the subagent's result wakes it.
// The Stop hook makes the done final at the first turn end with no
// subagent running.
func (claude) DoneAtTurnEnd() bool      { return true }
func (claude) ShowsWorking(string) bool { return false }

func (claude) HookPayload(_ []string, stdin io.Reader) ([]byte, error) { return readStdin(stdin) }

func (claude) HookReply(block string) string {
	if block == "" {
		return ""
	}
	return ClaudeBlock(block)
}

func (claude) SessionContext(request string) string { return ClaudeSessionContext(request) }

// claudeBackgroundCount matches the count Claude shows on its mode line ("⏵⏵
// bypass permissions on · 1 shell") while background shells or agents still
// run. Claude ends its turn then and wakes on its own later.
var claudeBackgroundCount = regexp.MustCompile(`(?i)\b\d+\s+(background\s+)?(shells?|bash(es)?|tasks?|agents?)\b`)

// WakesOnBackground is true: Claude starts a turn when a background shell
// or agent it left running finishes.
func (claude) WakesOnBackground() bool { return true }

// WorksInBackground looks only at Claude's mode line, so text in the
// conversation can never match.
func (claude) WorksInBackground(screen Screen) (bool, error) {
	text, err := screen(8)
	if err != nil {
		return false, err
	}
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(line, "⏵") && claudeBackgroundCount.MatchString(line) {
			return true, nil
		}
	}
	return false, nil
}

// LocksInput is true: a UserPromptSubmit hook blocks a prompt typed in the
// tab, idle or mid-turn, without interrupting a running turn (Claude Code
// 2.1.301). Dialog keys, built-in slash commands and ! bash mode never
// reach it.
func (claude) LocksInput() bool { return true }

// claudeClearLines is how many lines ClearInputKeys clears: enough for a
// typed prompt the gate put back, since a long paste shows as one "[Pasted
// text …]" line.
const claudeClearLines = 12

// ClearInputKeys clears the input one line at a time: ctrl+u empties the
// current line and backspace joins the one above. esc would clear it all,
// but interrupts a running turn.
func (claude) ClearInputKeys() []string {
	keys := make([]string, 0, 2*claudeClearLines)
	for range claudeClearLines {
		keys = append(keys, "ctrl+u", "backspace")
	}
	return keys
}

// claudeSynthetic starts a prompt Claude submits itself through
// UserPromptSubmit: the note that a background shell or subagent ended,
// with no field that marks it as Claude's own.
const claudeSynthetic = "<task-notification>"

func (claude) GatePrompt(payload []byte) (string, bool, error) {
	var in struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return "", false, fmt.Errorf("parse claude hook input: %w", err)
	}
	return in.Prompt, strings.HasPrefix(strings.TrimSpace(in.Prompt), claudeSynthetic), nil
}

// GateBlock is a UserPromptSubmit block: Claude shows reason, drops the
// prompt from the conversation and puts its text back into the input.
func (claude) GateBlock(reason string) string { return ClaudeBlock(reason) }

func (claude) Checks() Checks {
	return Checks{
		Bin:         "claude",
		Flags:       []string{"--dangerously-skip-permissions", "--settings", "--model", "--effort", "--strict-mcp-config"},
		Integration: "claude",
		Required:    true,
	}
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

// claudePasted is what Claude's input box shows for a pasted prompt: "[Pasted
// text #1 +12 lines]".
var claudePasted = regexp.MustCompile(`\[Pasted text #\d+`)

// claudePromptHead is how many characters of the prompt InputState compares
// with a typed one.
const claudePromptHead = 40

// claudeInputBox returns the text in the box between the last two input box
// edges, with whitespace normalised, and false when no box is on screen.
func claudeInputBox(screen string) (string, bool) {
	lines := strings.Split(screen, "\n")
	end := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if !claudeBoxEdge.MatchString(lines[i]) {
			continue
		}
		if end < 0 {
			end = i
			continue
		}
		box := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(strings.Join(lines[i+1:end], "\n")), "❯"))
		return strings.Join(strings.Fields(box), " "), true
	}
	return "", false
}

// InputState reads the box between the last two input box edges. A pasted
// prompt shows as "[Pasted text #N …]" and a short one as its own text.
// Anything else in the box, or no box on screen, is unknown.
func (claude) InputState(screen, prompt string) InputState {
	box, ok := claudeInputBox(screen)
	if !ok {
		return InputUnknown
	}
	head := []rune(strings.Join(strings.Fields(prompt), " "))
	head = head[:min(claudePromptHead, len(head))]
	switch {
	case box == "":
		return InputEmpty
	case claudePasted.MatchString(box), len(head) > 0 && strings.HasPrefix(box, string(head)):
		return InputHolds
	}
	return InputUnknown
}

// InputIs reports whether the box shows text and nothing else. A pasted
// placeholder never does: it hides what was pasted.
func (claude) InputIs(screen, text string) bool {
	box, ok := claudeInputBox(screen)
	return ok && box != "" && box == strings.Join(strings.Fields(text), " ")
}

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
		// The bullet of a running tool call blinks.
		out = append(out, strings.NewReplacer("●", "", "⏺", "").Replace(l))
	}
	return joinLines(out)
}

// ContextLeft is false: Claude tells the kit about its context through its
// compaction hooks, and its footer may be the user's own status line.
func (claude) ContextLeft(string) (int, bool) { return 0, false }

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

// ClaudeTransientError reads a Claude StopFailure input and returns the
// message of an API error that may pass: a server error (which includes a
// response lost mid-stream), an overloaded API, or a rate limit that is not
// a usage limit. ok is false for any other input.
func ClaudeTransientError(payload []byte) (message string, ok bool) {
	var in struct {
		Event   string `json:"hook_event_name"`
		Error   string `json:"error"`
		Message string `json:"last_assistant_message"`
	}
	if json.Unmarshal(payload, &in) != nil || in.Event != "StopFailure" {
		return "", false
	}
	switch in.Error {
	case "server_error", "overloaded":
	case "rate_limit":
		if limit, _ := ClaudeUsageLimit(payload); limit != "" {
			return "", false
		}
	default:
		return "", false
	}
	if message = strings.TrimSpace(in.Message); message == "" {
		message = "API error: " + in.Error
	}
	return message, true
}

// ClaudeBackgroundWork reads the background_tasks list of a Claude Stop
// hook input, which holds the session's shells and subagents with their
// status, and returns how many are still running. Claude Code 2.1.300 sends
// it; known is false when the input has no such list, as from an older
// Claude, or does not parse.
func ClaudeBackgroundWork(payload []byte) (running int, known bool) {
	return claudeBackgroundRunning(payload, func(string) bool { return true })
}

// ClaudeBackgroundEnding is ClaudeBackgroundWork for the work that ends and
// wakes the runner with its result: subagents, workflows and any type the
// kit does not know. A shell or a monitor, such as a dev server, may run
// for as long as the session does, and Claude's own housekeeping (memory
// consolidation, an auto-mode scan, a memory import) ends without waking it.
func ClaudeBackgroundEnding(payload []byte) (running int, known bool) {
	return claudeBackgroundRunning(payload, func(kind string) bool {
		switch kind {
		case "shell", "monitor", "dream", "auto-mode scan", "memory import":
			return false
		}
		return true
	})
}

// claudeBackgroundRunning counts the running entries of the list whose type
// counts.
func claudeBackgroundRunning(payload []byte, counts func(kind string) bool) (running int, known bool) {
	var in struct {
		Tasks *[]struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"background_tasks"`
	}
	if json.Unmarshal(payload, &in) != nil || in.Tasks == nil {
		return 0, false
	}
	for _, t := range *in.Tasks {
		if counts(t.Type) && (t.Status == "running" || t.Status == "pending") {
			running++
		}
	}
	return running, true
}

// ClaudeSettings returns the per-task settings file content: Stop and
// StopFailure hooks that call `<bin> __turn-end`, which tells the two apart
// by the hook input, and PreCompact and SessionStart(compact) hooks that
// call `<bin> __context`. PreCompact has no matcher: a manual compaction
// must cancel an automatic one Claude announced but never ran. A
// UserPromptSubmit hook calls `<bin> __prompt-gate`, which records the
// session's transcript and each turn start, and with InputLocked refuses a
// prompt typed in the tab. Claude merges them with the user's own hooks. It
// also denies the tools a runner has no use for, whose schemas would ride
// on every call.
func ClaudeSettings(in ClaudeSettingsInput) ([]byte, error) {
	bin := in.Bin
	if !filepath.IsAbs(bin) {
		return nil, fmt.Errorf("hook binary must be an absolute path, got %q", bin)
	}
	turnEnd := map[string]any{"type": "command", "command": ShellQuote(bin) + " __turn-end --harness claude"}
	onCompact := map[string]any{"type": "command", "command": ShellQuote(bin) + " __context --harness claude"}
	gate := map[string]any{"type": "command", "command": ShellQuote(bin) + " __prompt-gate --harness claude"}
	hooks := map[string]any{
		"Stop":             []any{map[string]any{"hooks": []any{turnEnd}}},
		"StopFailure":      []any{map[string]any{"hooks": []any{turnEnd}}},
		"PreCompact":       []any{map[string]any{"hooks": []any{onCompact}}},
		"SessionStart":     []any{map[string]any{"matcher": "compact", "hooks": []any{onCompact}}},
		"UserPromptSubmit": []any{map[string]any{"hooks": []any{gate}}},
	}
	deny := slices.Clone(claudeRunnerDeny)
	if in.InputLocked {
		deny = append(deny, claudeLockedDeny...)
	}
	settings := map[string]any{"hooks": hooks, "permissions": map[string]any{"deny": deny}}
	return json.MarshalIndent(settings, "", "  ")
}

// ClaudeSettingsInput are the inputs to ClaudeSettings.
type ClaudeSettingsInput struct {
	Bin         string // absolute path of the kit binary the hooks call
	InputLocked bool   // the runner takes prompts only from the kit
}

// claudeRunnerDeny are the tools no runner gets: Artifact publishes pages
// for a person reading along, SendFeedback and ShareOnboardingGuide report
// on or share Claude Code itself, and ListAgents finds other sessions to
// message, while a runner talks only to the kit through its report.
var claudeRunnerDeny = []string{"Artifact", "SendFeedback", "ShareOnboardingGuide", "ListAgents"}

// claudeLockedDeny are denied too when the runner's input is locked:
// nobody answers AskUserQuestion there, the channel to a human is a
// needs-human report, and ScheduleWakeup paces a /loop no locked runner
// is given.
var claudeLockedDeny = []string{"AskUserQuestion", "ScheduleWakeup"}

// ClaudeCompaction is what a PreCompact or SessionStart hook input says
// about a compaction, as Claude Code 2.1.300 sends it: PreCompact carries
// "trigger" ("auto" or "manual"), SessionStart carries "source" ("compact"
// after any compaction). Claude may announce an automatic compaction with
// PreCompact more than once, or without running one; only SessionStart
// with source compact means one ran.
type ClaudeCompaction struct {
	Event   string `json:"hook_event_name"`
	Trigger string `json:"trigger"`
	Source  string `json:"source"`
}

// ParseClaudeCompaction reads a PreCompact or SessionStart hook input.
func ParseClaudeCompaction(payload []byte) (ClaudeCompaction, error) {
	var c ClaudeCompaction
	if err := json.Unmarshal(payload, &c); err != nil {
		return c, fmt.Errorf("parse claude hook input: %w", err)
	}
	return c, nil
}

// ClaudeSessionContext is a SessionStart hook's output that adds text to
// the model's context. Claude passes it on after a compaction, also when the
// hook comes from --settings.
func ClaudeSessionContext(text string) string {
	out, _ := json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]string{"hookEventName": "SessionStart", "additionalContext": text},
	})
	return string(out)
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
	if _, err := os.Stat(configPath); errors.Is(err, fs.ErrNotExist) {
		return nil, TrustSkipNoConfig, nil
	}
	// Claude rewrites this file without the lock, so the flags set by every
	// attempt are checked after the write, and merged in again when gone.
	err = fsutil.Update(configPath, filepath.Dir(configPath), 0o600, func(data []byte) ([]byte, error) {
		next, added, err := mergeClaudeTrust(configPath, data, origin, runnerPath)
		for _, flag := range added {
			if !slices.Contains(set, flag) {
				set = append(set, flag)
			}
		}
		return next, err
	}, func(data []byte) bool {
		root, err := parseJSONObject(configPath, data)
		return err == nil && trustIn(root, runnerPath, set)
	})
	if err != nil {
		return nil, "", fmt.Errorf("trust flags for %s: %w", runnerPath, err)
	}
	if len(set) == 0 {
		return nil, TrustSkipAlreadyTrusted, nil
	}
	return set, "", nil
}

// mergeClaudeTrust adds the trust flags runnerPath lacks to the config
// data and returns the new data, or nil when none is missing, and the flags
// it added.
func mergeClaudeTrust(configPath string, data []byte, origin, runnerPath string) ([]byte, []string, error) {
	if data == nil {
		return nil, nil, fmt.Errorf("%s: %w", configPath, fs.ErrNotExist) // never created by the kit
	}
	root, err := parseJSONObject(configPath, data)
	if err != nil {
		return nil, nil, err
	}
	projects, _ := root["projects"].(map[string]any)
	src, _ := projects[filepath.Clean(origin)].(map[string]any)
	dst, _ := projects[filepath.Clean(runnerPath)].(map[string]any)
	if dst == nil {
		dst = map[string]any{}
	}
	var added []string
	if dst[ClaudeTrustFlag] != true {
		dst[ClaudeTrustFlag] = true
		added = append(added, ClaudeTrustFlag)
	}
	for _, flag := range claudeImportFlags {
		if src[flag] == true && dst[flag] != true {
			dst[flag] = true
			added = append(added, flag)
		}
	}
	if len(added) == 0 {
		return nil, nil, nil
	}
	projects[filepath.Clean(runnerPath)] = dst

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return append(out, '\n'), added, nil
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
	return trustIn(root, path, flags), nil
}

// trustIn reports whether every flag is true for path in a parsed config.
func trustIn(root map[string]any, path string, flags []string) bool {
	projects, _ := root["projects"].(map[string]any)
	entry, _ := projects[filepath.Clean(path)].(map[string]any)
	for _, flag := range flags {
		if entry[flag] != true {
			return false
		}
	}
	return true
}

// readJSONObject decodes with UseNumber so large integers survive a rewrite.
func readJSONObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseJSONObject(path, data)
}

// parseJSONObject decodes the JSON object read from path, keeping numbers
// as written, and makes sure it has a projects object.
func parseJSONObject(path string, data []byte) (map[string]any, error) {
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
// reason as its next instruction. A UserPromptSubmit hook refuses a prompt
// with the same output.
func ClaudeBlock(reason string) string {
	out, _ := json.Marshal(map[string]string{"decision": "block", "reason": reason})
	return string(out)
}
