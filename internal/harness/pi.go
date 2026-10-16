package harness

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// pi serves pi and omp (oh-my-pi), which share their launch flags, skill
// syntax and extension API. The rules follow a live probe of pi 1.2.0,
// omp 18.2.10 and herdr 0.9.1. A variant differs only in the fields below.
type pi struct {
	harness  task.Harness
	kind     string // the value for `herdr agent start --kind`
	approve  string // the flag that skips trust and approval prompts for the run
	overlay  string // a config overlay in the task folder, passed with --config
	extFlag  string // the flag that loads the runner extension
	dialogs  []Dialog
	progress func(screen string) string
}

// The registered variants.
var (
	piAgent = pi{
		harness: task.Pi, kind: "pi", approve: "--approve", extFlag: "-e",
		dialogs: []Dialog{piProjectTrust}, progress: piProgress,
	}
	ompAgent = pi{
		harness: task.Omp, kind: "omp", approve: "--auto-approve",
		overlay: ompOverlayFile, extFlag: "--hook", progress: ompProgress,
	}
)

// The per-task files a pi or omp runner starts with.
const (
	piContractFile  = "contract.md"
	piExtensionFile = "hrk-runner.ts" // reports turn ends and compactions, and gates prompts
	ompOverlayFile  = "omp-runner.yml"
)

// ompOverlay turns off omp's ask tool, which waits for an answer forever,
// and the run-state spinner in the terminal title: herdr counts each title
// change as a new agent revision, so the spinner would read as progress and
// hide a hung runner from the stall ladder.
const ompOverlay = "ask:\n  enabled: false\ntui:\n  titleState: false\n"

// piProjectTrust is the dialog pi 1.2.0 shows in a folder with project
// settings, extensions or skills. --approve skips it, so it shows only if
// that flag stops working; herdr then reports the agent idle.
var piProjectTrust = Dialog{Name: "pi-project-trust", Match: "Trust project folder?", Action: NeedsHuman, IdleWhileShown: true}

func (p pi) Harness() task.Harness { return p.harness }
func (p pi) HerdrKind() string     { return p.kind }

// LaunchArgs skips trust and approval prompts for the run, passes the
// contract as an appended system prompt, which survives compaction, and
// loads the runner extension and omp's config overlay from the task folder.
func (p pi) LaunchArgs(o LaunchOptions) []string {
	var args []string
	if o.Spec.Model != "" {
		args = append(args, "--model", o.Spec.Model)
	}
	if o.Spec.Effort != "" {
		args = append(args, "--thinking", o.Spec.Effort)
	}
	args = append(args, p.approve)
	if o.Dir != "" {
		if p.overlay != "" {
			args = append(args, "--config", filepath.Join(o.Dir, p.overlay))
		}
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
// become the kit binary and the harness, as JSON strings, and the lock
// placeholder true or false.
//
//go:embed hrk-runner.ts
var piExtension string

const (
	piBinPlaceholder     = `"__HRK_BIN__"`
	piHarnessPlaceholder = `"__HRK_HARNESS__"`
	piLockPlaceholder    = `__HRK_INPUT_LOCK__`
)

func (p pi) Files(in FilesInput) ([]File, error) {
	if !filepath.IsAbs(in.Bin) {
		return nil, fmt.Errorf("hook binary must be an absolute path, got %q", in.Bin)
	}
	bin, _ := json.Marshal(in.Bin)
	h, _ := json.Marshal(string(p.harness))
	ext := strings.NewReplacer(piBinPlaceholder, string(bin), piHarnessPlaceholder, string(h),
		piLockPlaceholder, strconv.FormatBool(in.InputLocked)).Replace(piExtension)
	files := []File{
		{Name: piContractFile, Data: []byte(in.Contract + "\n")},
		{Name: piExtensionFile, Data: []byte(ext)},
	}
	if p.overlay != "" {
		files = append(files, File{Name: p.overlay, Data: []byte(ompOverlay)})
	}
	return files, nil
}

func (p pi) Dialogs() []Dialog { return p.dialogs }

// ShowsUsageLimit is always false: a screen never confirms a pi or omp
// limit. pi 1.2.0 draws a turn's error as " Error: <message>" under a blank
// line, and a tool's output line such as " Error: disk quota exceeded" looks
// the same. A limit only comes from a model request, so the runner
// extension's turn-end payload carries it, and ReadTurnEnd confirms it from
// the payload's errorMessage.
func (pi) ShowsUsageLimit(string) bool { return false }

// ProgressText keeps the conversation, which each variant cuts from the
// status, input and footer lines it draws under it.
func (p pi) ProgressText(screen string) string { return p.progress(screen) }

// piRetry matches the status both agents draw while they retry a model
// request: "Retrying (1/3) in 2s... (esc to cancel)" in pi 1.2.0 and
// "Retrying (1/3) in 2s…" in omp 18.2.10.
var piRetry = regexp.MustCompile(`Retrying \(\d+/\d+\) in`)

func (pi) ShowsRetry(screen string) bool {
	return anyLineMatches(lastLines(screen, statusLines), piRetry)
}

// InputState is always unknown: the input of pi and omp is not read.
func (pi) InputState(string, string) InputState { return InputUnknown }

func (pi) SelfResolvingPrompts() []SelfResolvingPrompt { return nil }

// piRule matches the full-width rules pi 1.2.0 draws above and below its
// input. While a turn runs the top one carries the status, as
// "── ⠇ Working ──…", "── ⠧ Compacting context... (escape to cancel) ──…"
// or "── ⠋ Retrying (1/3) in 2s... (esc to cancel) ──…".
var piRule = regexp.MustCompile(`^──.*─\s*$`)

// piProgress keeps what is above pi's input, whose top rule is the
// second-to-last rule on screen; the cwd and usage footer sit under the
// bottom one.
func piProgress(screen string) string {
	lines := strings.Split(screen, "\n")
	for i, rules := len(lines)-1, 0; i >= 0; i-- {
		if piRule.MatchString(lines[i]) {
			if rules++; rules == 2 {
				lines = lines[:i]
				break
			}
		}
	}
	return joinLines(lines)
}

// ompBand matches the first line of omp 18.2.10's prompt band, " π > ⬢ …"
// when idle and " ⠼ 4s > ⬢ …" while a turn runs. Its layout follows the
// user's config, so nothing from it down is read.
var ompBand = regexp.MustCompile(`^\s*(?:π|\S+\s+\d+[smh])\s+>`)

// ompStatus matches the status lines omp 18.2.10 draws above its band:
// "  ⎋ <intent>" with the session title on its right, and
// " ⠼ Auto server compaction… (esc to cancel)".
var ompStatus = regexp.MustCompile(`^\s*(?:⎋\s|\S+\s.*\(esc to cancel\)\s*$)`)

// ompProgress keeps what is above omp's prompt band, without its status
// lines. The stat line under each message stays: it is drawn once.
func ompProgress(screen string) string {
	lines := strings.Split(screen, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if ompBand.MatchString(lines[i]) {
			lines = lines[:i]
			break
		}
	}
	var out []string
	for _, l := range lines {
		if !ompStatus.MatchString(l) {
			out = append(out, l)
		}
	}
	return joinLines(out)
}

// ContextLeft is unknown: the runner extension reports context on turn end.
func (pi) ContextLeft(string) (int, bool) { return 0, false }

// PrepareTrust writes nothing: pi's --approve and omp's --auto-approve
// cover trust for the run and save nothing, so no file under ~/.pi or ~/.omp
// is touched. omp has no trust dialog.
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
// starts "You have hit your". piMaybeLimitPhrases also name a provider's
// rate limit, such as a 429 RESOURCE_EXHAUSTED that pi gave up retrying, so
// they only suggest a limit: a fallback takes over if one is left, and the
// task is never failed on them.
var (
	piLimitPhrases      = []string{"usage limit", "You have hit your", "out of credits", "insufficient quota", "insufficient_quota"}
	piMaybeLimitPhrases = []string{"quota", "exhausted"}
)

// ReadTurnEnd reads the extension's turn end. An aborted run is the user's
// Esc; an error naming a usage limit is the agent's own, so it confirms the
// limit, and one that only may be a limit counts as a limit on screen.
func (pi) ReadTurnEnd(in TurnInput) (TurnSignal, error) {
	var t piTurn
	if err := json.Unmarshal(in.Payload, &t); err != nil {
		return TurnSignal{}, fmt.Errorf("parse pi turn end: %w", err)
	}
	if t.Aborted {
		return TurnSignal{Ignore: true}, nil
	}
	msg := Normalize(t.ErrorMessage)
	if containsAny(msg, piLimitPhrases) {
		return TurnSignal{LimitConfirmed: true}, nil
	}
	sig := TurnSignal{LimitOnScreen: containsAny(msg, piMaybeLimitPhrases)}
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

// DoneAtTurnEnd is false: the runner extension reports the turn end only
// after the run settles. Turn it on once a live run shows pi or omp working
// on after a done.
func (pi) DoneAtTurnEnd() bool      { return false }
func (pi) ShowsWorking(string) bool { return false }

// LocksInput is true: the runner extension's input handler asks
// `hrk __prompt-gate` about every prompt submitted in the tab, since pi and
// omp give the kit's typed prompt and a human's the same source. omp sends
// ! bash and $ python through it too; for pi's ! bash a user_bash handler
// asks the same way.
func (pi) LocksInput() bool { return true }

// ClearInputKeys is nil: pi and omp clear a prompt the extension refused.
func (pi) ClearInputKeys() []string { return nil }

// GatePrompt reads the extension's gate payload. Nothing in it is the
// agent's own: the extension lets those through without asking.
func (pi) GatePrompt(payload []byte) (string, bool, error) {
	var in struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return "", false, fmt.Errorf("parse runner extension gate input: %w", err)
	}
	return in.Text, false, nil
}

// GateBlock is the reason alone: the extension shows it as a warning.
func (pi) GateBlock(reason string) string { return reason }

// Checks keeps pi and omp optional, so a config without them sees warnings
// only. The flags are long forms: doctor matches them as substrings of
// --help, where "-e" would match anything.
func (p pi) Checks() Checks {
	flags := []string{"--model", "--thinking", p.approve, "--append-system-prompt", "--extension"}
	if p.overlay != "" { // omp
		flags = []string{"--model", "--thinking", p.approve, "--append-system-prompt", "--config", "--hook"}
	}
	return Checks{Bin: p.kind, Flags: flags, Integration: p.kind}
}
