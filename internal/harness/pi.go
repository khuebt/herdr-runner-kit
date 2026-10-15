package harness

import (
	"io"
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

func (pi) Files(in FilesInput) ([]File, error) {
	return []File{{Name: piContractFile, Data: []byte(in.Contract + "\n")}}, nil
}

// The screen and turn-end methods below are neutral until the
// runner extension and promoted captures give them something to read.

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

func (pi) ReadTurnEnd(TurnInput) (TurnSignal, error) { return TurnSignal{Ignore: true}, nil }

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
