package harness

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strings"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// pi serves pi and omp (oh-my-pi), which share their launch flags, skill
// syntax and extension API. The rules follow a live probe of pi 1.2.0,
// omp 18.2.10 and herdr 0.9.1. A variant differs only in the fields below.
type pi struct {
	harness task.Harness
	kind    string // the value for `herdr agent start --kind`
	extFlag string // the flag that loads the runner extension
}

// The registered variants.
var (
	piAgent  = pi{harness: task.Pi, kind: "pi", extFlag: "-e"}
	ompAgent = pi{harness: task.Omp, kind: "omp", extFlag: "--hook"}
)

// The per-task files a pi or omp runner starts with.
const (
	piContractFile  = "contract.md"
	piExtensionFile = "hrk-runner.ts" // reports turn ends and compactions
)

func (p pi) Harness() task.Harness { return p.harness }
func (p pi) HerdrKind() string     { return p.kind }

// LaunchArgs passes the contract as an appended system prompt, which
// survives compaction, and loads the runner extension from the task folder.
func (p pi) LaunchArgs(o LaunchOptions) []string {
	var args []string
	if o.Spec.Model != "" {
		args = append(args, "--model", o.Spec.Model)
	}
	if o.Spec.Effort != "" {
		args = append(args, "--thinking", o.Spec.Effort)
	}
	if o.Dir != "" {
		args = append(args, "--append-system-prompt", filepath.Join(o.Dir, piContractFile),
			p.extFlag, filepath.Join(o.Dir, piExtensionFile))
	}
	return args
}

// RenderSkill uses the "/skill:name" syntax, where name is the skill's
// folder, so a colon becomes a dash.
func (pi) RenderSkill(skill string) string {
	name := skillName(skill)
	if name == "" {
		return ""
	}
	return "/skill:" + strings.ReplaceAll(name, ":", "-")
}

// FirstPrompt leaves the contract out: the agent gets it as a system prompt.
func (p pi) FirstPrompt(skill, prompt, _ string) string {
	return joinPrompt(p.RenderSkill(skill), prompt, "")
}

// piExtension is the runner extension template. Its quoted placeholders
// become the kit binary and the harness, as JSON strings.
//
//go:embed hrk-runner.ts
var piExtension string

const (
	piBinPlaceholder     = `"__HRK_BIN__"`
	piHarnessPlaceholder = `"__HRK_HARNESS__"`
)

func (p pi) Files(in FilesInput) ([]File, error) {
	if !filepath.IsAbs(in.Bin) {
		return nil, fmt.Errorf("hook binary must be an absolute path, got %q", in.Bin)
	}
	bin, _ := json.Marshal(in.Bin)
	h, _ := json.Marshal(string(p.harness))
	ext := strings.NewReplacer(piBinPlaceholder, string(bin), piHarnessPlaceholder, string(h)).Replace(piExtension)
	return []File{
		{Name: piContractFile, Data: []byte(in.Contract + "\n")},
		{Name: piExtensionFile, Data: []byte(ext)},
	}, nil
}

// The screen methods below are neutral until promoted captures give them
// something to read.

func (pi) Dialogs() []Dialog                           { return nil }
func (pi) ShowsUsageLimit(string) bool                 { return false }
func (pi) ProgressText(screen string) string           { return screen }
func (pi) ShowsRetry(string) bool                      { return false }
func (pi) SelfResolvingPrompts() []SelfResolvingPrompt { return nil }

// ContextLeft is unknown: the runner extension reports context on turn end.
func (pi) ContextLeft(string) (int, bool) { return 0, false }

// PrepareTrust writes nothing: trust is granted per run by launch flags,
// which persist nothing.
func (pi) PrepareTrust(TrustInput) (TrustResult, error) { return TrustResult{}, nil }

// piTurn is the part of the extension's turn-end payload the kit acts on;
// event, outcome and stopReason are there for the reader of a log.
type piTurn struct {
	Aborted      bool   `json:"aborted"`
	ErrorMessage string `json:"errorMessage"`
	Context      struct {
		Percent *float64 `json:"percent"` // used context; absent right after a compaction
	} `json:"context"`
}

// piLimitPhrases mark a usage-limit error: pi-ai's own ChatGPT message
// starts "You have hit your", and omp treats the rest as limit errors too.
var piLimitPhrases = []string{"usage limit", "You have hit your", "quota", "exhausted", "out of credits"}

// ReadTurnEnd reads the extension's turn end. An aborted run is the user's
// Esc; an error naming a usage limit is the agent's own, so it confirms the
// limit.
func (pi) ReadTurnEnd(in TurnInput) (TurnSignal, error) {
	var t piTurn
	if err := json.Unmarshal(in.Payload, &t); err != nil {
		return TurnSignal{}, fmt.Errorf("parse pi turn end: %w", err)
	}
	if t.Aborted {
		return TurnSignal{Ignore: true}, nil
	}
	if containsAny(Normalize(t.ErrorMessage), piLimitPhrases) {
		return TurnSignal{LimitConfirmed: true}, nil
	}
	var sig TurnSignal
	if p := t.Context.Percent; p != nil {
		sig.ContextLeft, sig.ContextKnown = 100-int(math.Round(*p)), true
	}
	return sig, nil
}

// NudgeInHook is false: the extension's hook command takes no reply, so the
// nudge is typed into the pane.
func (pi) NudgeInHook() bool { return false }

func (pi) HookPayload(_ []string, stdin io.Reader) ([]byte, error) { return readStdin(stdin) }
func (pi) HookReply(string) string                                 { return "" }
func (pi) SessionContext(string) string                            { return "" }
func (pi) WorksInBackground(Screen) (bool, error)                  { return false, nil }

// Checks keeps pi and omp optional, so a config without them sees warnings
// only.
func (p pi) Checks() Checks {
	return Checks{Bin: p.kind, Integration: p.kind}
}
