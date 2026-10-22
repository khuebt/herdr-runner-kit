// Package task defines the task model shared by the CLI, the store and the
// launcher. A task is one runner working on one prompt.
package task

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Status is the lifecycle state of a task.
type Status string

// Task statuses. Done, Failed and Stopped are terminal: once a task reaches one
// of them, no later transition may change it.
const (
	Queued     Status = "queued"      // waiting for its --after tasks
	Held       Status = "held"        // human gate before start
	Starting   Status = "starting"    // launcher is opening the runner
	Running    Status = "running"     // runner is working
	NeedsHuman Status = "needs-human" // runner is waiting for a decision
	Done       Status = "done"
	Failed     Status = "failed"
	Stopped    Status = "stopped"
)

var allStatuses = []Status{Queued, Held, Starting, Running, NeedsHuman, Done, Failed, Stopped}

// Terminal reports whether s is a final status.
func (s Status) Terminal() bool {
	return s == Done || s == Failed || s == Stopped
}

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	for _, known := range allStatuses {
		if s == known {
			return true
		}
	}
	return false
}

// Gate controls whether a task starts on its own.
type Gate string

// Gates. Auto starts as soon as the task's dependencies are done. Human holds
// the task until the user releases it.
const (
	GateAuto  Gate = "auto"
	GateHuman Gate = "human"
)

// Valid reports whether g is a known gate.
func (g Gate) Valid() bool {
	return g == GateAuto || g == GateHuman
}

// Node is a milestone plan node as a chain runner was spawned for it.
type Node struct {
	ID        string   `json:"id"`
	Type      string   `json:"type"`
	Title     string   `json:"title"`
	DependsOn []string `json:"dependsOn,omitempty"`
}

// Worktree describes the git worktree a task runs in.
type Worktree struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
	Base   string `json:"base,omitempty"`
	// Removed is set once the worktree was removed.
	Removed bool `json:"removed,omitempty"`
	// BranchDeleted is set once cleanup deleted the merged branch.
	BranchDeleted bool `json:"branchDeleted,omitempty"`
	// Offered is set once the main session was told the worktree and
	// branch can be cleaned up, so it is told only once.
	Offered bool `json:"offered,omitempty"`
	// BranchCreated is set to true once the launcher created the branch
	// with the worktree, and is false before that. Tasks from older
	// versions have no value.
	BranchCreated *bool `json:"branchCreated,omitempty"`
	// MergedTip is the branch's commit the kit found merged: set when
	// cleanup's check finds the branch merged or removes it, or when
	// milestone done finds the release branch merged. Cleanup counts a
	// branch of the same owner and repository whose tip is an ancestor of it
	// as merged too: the branch itself, once later commits on the default
	// branch change the lines it changed, and the branches stacked under a
	// squash merge, which it leaves off the default branch.
	MergedTip string `json:"mergedTip,omitempty"`
	// OlderMergedTips are tips recorded before MergedTip that are not
	// ancestors of it, which still count.
	OlderMergedTips []string `json:"olderMergedTips,omitempty"`
}

// MergedTipRef is the ref that keeps task id's MergedTip from being pruned
// once no branch holds it.
func MergedTipRef(id string) string {
	return "refs/hrk/merged/" + id
}

// MergedTips returns every tip recorded as merged, the newest first.
func (w *Worktree) MergedTips() []string {
	if w.MergedTip == "" {
		return nil
	}
	return append([]string{w.MergedTip}, w.OlderMergedTips...)
}

// AddMergedTip records tip as the newest merged tip, keeping every recorded
// tip that is not an ancestor of it, so a tip other branches rely on is never
// dropped. It changes nothing and reports false when tip is at or under a
// recorded tip. isAncestor(a, b) reports whether a is b or an ancestor of it.
func (w *Worktree) AddMergedTip(tip string, isAncestor func(a, b string) bool) bool {
	old := w.MergedTips()
	for _, o := range old {
		if isAncestor(tip, o) {
			return false
		}
	}
	var keep []string
	for _, o := range old {
		if !isAncestor(o, tip) {
			keep = append(keep, o)
		}
	}
	w.MergedTip, w.OlderMergedTips = tip, keep
	return true
}

// MergedTipRefs returns the refs that keep each recorded tip from being
// pruned, as ref and commit pairs, the older tips first, so a tip that moves
// out of MergedTipRef gets its own ref before that ref moves.
func (w *Worktree) MergedTipRefs(id string) [][2]string {
	var refs [][2]string
	for _, o := range w.OlderMergedTips {
		refs = append(refs, [2]string{MergedTipRef(id) + "-" + o, o})
	}
	if w.MergedTip != "" {
		refs = append(refs, [2]string{MergedTipRef(id), w.MergedTip})
	}
	return refs
}

// OwnsBranch reports whether the task created its branch, so cleanup may
// delete it. A task from an older version owns it when a worktree path was
// recorded, since the launcher created the branch with the worktree.
func (w *Worktree) OwnsBranch() bool {
	if w.BranchCreated != nil {
		return *w.BranchCreated
	}
	return w.Path != ""
}

// Herdr holds the herdr handles of the runner. They are filled in by the
// launcher as soon as herdr returns them, so stop can always clean up.
type Herdr struct {
	WorkspaceID string `json:"workspaceId,omitempty"`
	TabID       string `json:"tabId,omitempty"`
	PaneID      string `json:"paneId,omitempty"`
	AgentName   string `json:"agentName,omitempty"`
	// Closed is set once gc or stop has ended the runner's agent.
	Closed bool `json:"closed,omitempty"`
	// ClosingSince is when gc, stop or cleanup claimed the runner to close
	// it. The claim keeps send away while the agent quits, and expires on
	// its own, so a close that was abandoned never blocks the runner.
	ClosingSince time.Time `json:"closingSince,omitzero"`
}

// ProjectOwner is the owner of every task of a project, so its news reaches
// the main session whichever folder that session works in.
func ProjectOwner(name string) string { return projectOwnerPrefix + name }

// IsProjectOwner reports whether owner is the owner of a project's tasks.
func IsProjectOwner(owner string) bool { return strings.HasPrefix(owner, projectOwnerPrefix) }

const projectOwnerPrefix = "project:"

// Task is the durable record stored in tasks/<id>/task.json.
type Task struct {
	SchemaVersion int    `json:"schemaVersion"`
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	Agent         string `json:"agent"`
	Prompt        string `json:"prompt"`
	Skill         string `json:"skill,omitempty"`
	Title         string `json:"title,omitempty"`
	Cwd           string `json:"cwd"`
	// Owner is the main session the task reports to: the git top-level of
	// the folder spawn ran in, or ProjectOwner of the task's project.
	Owner string `json:"owner"`
	// Project is the registered project the task works for, and Stage the
	// project stage it runs, if any.
	Project string `json:"project,omitempty"`
	Stage   string `json:"stage,omitempty"`
	// Node is the milestone plan node a chain runner was spawned for, as
	// the plan held it then, so a later chain sees a node the plan changed.
	Node *Node `json:"node,omitempty"`
	// RepoRoot is the main repository root of Cwd, empty outside git. Trust
	// is prepared for it, and worktrees are created from it.
	RepoRoot string `json:"repoRoot,omitempty"`
	// MainWorkspace is the herdr workspace of the main session that spawned
	// the task. Tabs open there, never in whatever workspace has focus.
	MainWorkspace string `json:"mainWorkspace,omitempty"`
	// ClaudeConfigDir is the main session's CLAUDE_CONFIG_DIR. herdr panes do
	// not inherit it, so runners get it explicitly.
	ClaudeConfigDir string    `json:"claudeConfigDir,omitempty"`
	UseWorktree     bool      `json:"useWorktree,omitempty"`
	Worktree        *Worktree `json:"worktree,omitempty"`
	After           []string  `json:"after,omitempty"`
	Gate            Gate      `json:"gate"`
	// HoldReason says why a task with the human gate waits, such as a
	// dependency it needs. Only a release that names the task opens it.
	HoldReason string `json:"holdReason,omitempty"`
	Herdr      Herdr  `json:"herdr"`
	// Error is the reason a task failed or needs a human, when the kit itself
	// set that status.
	Error string `json:"error,omitempty"`
	// Summary, Outputs, Question and Decisions come from the runner's last
	// report. A round of several questions is stored numbered, one per line
	// (see JoinQuestions). Decisions are the open questions a done runner
	// settled itself, each as "<question> -> <choice>".
	Summary   string   `json:"summary,omitempty"`
	Outputs   []string `json:"outputs,omitempty"`
	Question  string   `json:"question,omitempty"`
	Decisions []string `json:"decisions,omitempty"`
	// DialogAnswers are the answers the startup dialog the task waits on
	// takes from `hrk answer`, each with the herdr keys that give it; the
	// dialog's name is in Error and its question in Question.
	DialogAnswers map[string][]string `json:"dialogAnswers,omitempty"`
	// Inputs are the results of the --after tasks, added to the first prompt.
	Inputs string `json:"inputs,omitempty"`
	// PendingDone is a done report held until the turn it came in ends,
	// for an agent that may keep working after it. The task stays running
	// meanwhile.
	PendingDone *PendingDone `json:"pendingDone,omitempty"`
	// QueuedSend is when send typed a message into an agent that was still
	// working. A done held from before it does not answer it. The next turn
	// end clears it.
	QueuedSend time.Time `json:"queuedSend,omitzero"`
	// Unreported counts turns that ended without a report since the last
	// prompt from the main session: the first is nudged, the second is news.
	Unreported int `json:"unreported,omitempty"`
	// Background says whether the agent's last turn ended with background
	// work of its own still running, as its turn-end hook listed it. It is
	// nil when the hook listed none, and cleared with Unreported.
	Background *bool `json:"background,omitempty"`
	// APIErrors counts turns that ended on a transient API error since the
	// runner last ended a turn normally, reported, was prompted by the main
	// session or switched agents: the first is continued, the second is news.
	APIErrors int `json:"apiErrors,omitempty"`
	// CodexThread is the thread of the runner's first Codex turn. Notify
	// calls from other threads, such as title generation, are ignored.
	CodexThread string `json:"codexThread,omitempty"`
	// FallbackFrom lists, in order, the agents that hit a usage limit on
	// this task before it switched to its current one.
	FallbackFrom []string `json:"fallbackFrom,omitempty"`
	// Relaunched says why the launcher last restarted the agent in the
	// task's pane: RelaunchUsageLimit or RelaunchHandoff. Empty with
	// FallbackFrom set means a usage limit, from before the field existed.
	Relaunched string `json:"relaunched,omitempty"`
	// Attempt numbers the agent sessions of the task: 0 or 1 is the first,
	// and each context handoff starts the next. Handoffs lists the handoff
	// files of the sessions that ended, in order.
	Attempt  int      `json:"attempt,omitempty"`
	Handoffs []string `json:"handoffs,omitempty"`
	// Stalled is set while herdr reports the agent blocked on a prompt.
	Stalled bool `json:"stalled,omitempty"`
	// InputLocked says the runner takes prompts only from the kit, when its
	// agent can lock its input: the runnerInput config at spawn.
	InputLocked bool `json:"inputLocked,omitempty"`
	// MCP keeps the user's MCP servers in the runner, from spawn --mcp or
	// the kind; without it a Claude runner starts with none.
	MCP bool `json:"mcp,omitempty"`
	// Language is the language the runner writes its report summaries and
	// questions in. Empty leaves it to the runner.
	Language string `json:"language,omitempty"`
	// Git is the checkout's state at the runner's last report.
	Git *GitState `json:"git,omitempty"`
	// RoundHead is the full sha of the checkout's HEAD when the current
	// round began: at launch, and at each send or answer.
	RoundHead string `json:"roundHead,omitempty"`
	// Plan is the absolute folder of the AgentKit plan the task runs one
	// phase of, and Phase that phase's number.
	Plan  string `json:"plan,omitempty"`
	Phase int    `json:"phase,omitempty"`
	// CheckoutOf is the plan task whose worktree this task works in: the
	// phases of one plan run share one checkout and one branch.
	CheckoutOf string `json:"checkoutOf,omitempty"`
	// PhaseStart is the full sha of the checkout's HEAD when the task's
	// runner first launched; a plan phase is checked against it and a review
	// diffs from it. PhaseCheck is what the kit found once a phase was done.
	PhaseStart string      `json:"phaseStart,omitempty"`
	PhaseCheck *PhaseCheck `json:"phaseCheck,omitempty"`
	// Review is set on a review task: a read-only runner that reviews the
	// work of task Review.Of, or of its plan run when Of is empty. FixupOf is
	// the review whose verified findings a plan's fixup task fixes.
	Review  *Review `json:"review,omitempty"`
	FixupOf string  `json:"fixupOf,omitempty"`
	// Scopes are the cgroups of the memory-capped systemd scopes the task's
	// agent sessions ran in, oldest first.
	Scopes []string `json:"scopes,omitempty"`
	// ShellPID is the pane shell's pid when the latest scope was made. A
	// different one means herdr restarted the pane.
	ShellPID int `json:"shellPid,omitempty"`
	// Keep holds a finished runner's tab open: gc leaves it until keep is
	// turned off or the task is stopped.
	Keep bool `json:"keep,omitempty"`
	// Transcripts are the agent session transcripts the task's turn ends
	// named, oldest first: every attempt and agent switch of a harness that
	// keeps one. Usage is the token usage read from them, last at At.
	Transcripts []Transcript `json:"transcripts,omitempty"`
	Usage       *Usage       `json:"usage,omitempty"`
	// StartedAt is when the agent first got its prompt, as the started event
	// says. Tasks an older version started have none.
	StartedAt time.Time `json:"startedAt,omitzero"`
	// FinishedAt is set when the task reaches a terminal status.
	FinishedAt     time.Time `json:"finishedAt,omitzero"`
	Status         Status    `json:"status"`
	LauncherPID    int       `json:"launcherPid,omitempty"`
	LaunchDeadline time.Time `json:"launchDeadline,omitzero"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// Transcript is one agent session's transcript file and the harness that
// wrote it, which knows its format.
type Transcript struct {
	Harness Harness `json:"harness"`
	Path    string  `json:"path"`
}

// Usage is the token usage of a task's agent sessions and their subagents.
// Units weigh the tokens by list-price ratios to an input token (see
// UnitWeights); they compare runs, they are not money.
type Usage struct {
	Calls        int64 `json:"calls"`
	Input        int64 `json:"input"`
	CacheRead    int64 `json:"cacheRead"`
	CacheWrite   int64 `json:"cacheWrite"` // every cache write, the 1-hour ones included
	CacheWrite1h int64 `json:"cacheWrite1h"`
	Output       int64 `json:"output"`
	// MaxContext is the largest context one call of the task's own sessions
	// carried: input, cache read and cache write.
	MaxContext int64 `json:"maxContext"`
	// Subagents counts the subagents the sessions ran, and SubagentUnits
	// their share of Units.
	Subagents     int64     `json:"subagents"`
	Units         float64   `json:"units"`
	SubagentUnits float64   `json:"subagentUnits"`
	At            time.Time `json:"at,omitzero"`
}

// UnitWeights are the cost of one token in input-token units, from the
// list-price ratios: a cache read 0.1, a 5-minute cache write 1.25, a
// 1-hour write 2, an output token 5.
var UnitWeights = struct{ Input, CacheRead, CacheWrite5m, CacheWrite1h, Output float64 }{1, 0.1, 1.25, 2, 5}

// AddCall counts one model call, and its context toward MaxContext unless
// it is a subagent's.
func (u *Usage) AddCall(input, cacheRead, cacheWrite, cacheWrite1h, output int64, subagent bool) {
	w := UnitWeights
	units := float64(input)*w.Input + float64(cacheRead)*w.CacheRead +
		float64(cacheWrite-cacheWrite1h)*w.CacheWrite5m + float64(cacheWrite1h)*w.CacheWrite1h + float64(output)*w.Output
	u.Calls++
	u.Input += input
	u.CacheRead += cacheRead
	u.CacheWrite += cacheWrite
	u.CacheWrite1h += cacheWrite1h
	u.Output += output
	u.Units += units
	if subagent {
		u.SubagentUnits += units
	} else {
		u.MaxContext = max(u.MaxContext, input+cacheRead+cacheWrite)
	}
}

// Add adds o's counts to u.
func (u *Usage) Add(o *Usage) {
	u.Calls += o.Calls
	u.Input += o.Input
	u.CacheRead += o.CacheRead
	u.CacheWrite += o.CacheWrite
	u.CacheWrite1h += o.CacheWrite1h
	u.Output += o.Output
	u.MaxContext = max(u.MaxContext, o.MaxContext)
	u.Subagents += o.Subagents
	u.Units += o.Units
	u.SubagentUnits += o.SubagentUnits
}

// String reads like "53 calls, max context 187k, 1.4M units (2 subagents,
// 0.3M)".
func (u *Usage) String() string {
	s := fmt.Sprintf("%d calls, max context %s, %s units", u.Calls, Count(float64(u.MaxContext)), Count(u.Units))
	if u.Subagents > 0 {
		s += fmt.Sprintf(" (%d subagents, %s)", u.Subagents, Count(u.SubagentUnits))
	}
	return s
}

// Count prints n shortly: 950, 187k, 1.4M.
func Count(n float64) string {
	switch {
	case n >= 1e6:
		return strconv.FormatFloat(n/1e6, 'f', 1, 64) + "M"
	case n >= 1e3:
		return strconv.FormatFloat(n/1e3, 'f', 0, 64) + "k"
	}
	return strconv.FormatFloat(n, 'f', 0, 64)
}

// PendingDone is a held done report.
type PendingDone struct {
	Summary   string   `json:"summary"`
	Decisions []string `json:"decisions,omitempty"`
	Outputs   []string `json:"outputs,omitempty"`
	// Since is when the done was reported, or when a queued message
	// superseded it.
	Since time.Time `json:"since"`
	// Superseded says a message sent while the agent worked came after this
	// done: the runner reports again on it, and no turn end finalizes this
	// one.
	Superseded bool `json:"superseded,omitempty"`
	// Subagents says a turn ended with background subagents still running,
	// so only the turn end that finds none makes this done final.
	Subagents bool `json:"subagents,omitempty"`
	// TurnStart is when a turn started while this done was held, such as
	// one Claude's note that a subagent ended wakes; the turn end clears it.
	TurnStart time.Time `json:"turnStart,omitzero"`
}

// Reasons a runner's agent was restarted in its pane, as Relaunched
// records them.
const (
	RelaunchUsageLimit = "usage-limit"
	RelaunchHandoff    = "handoff"
)

// AttemptOf returns the number of t's current agent session, 1 for the
// first.
func AttemptOf(t *Task) int {
	return max(t.Attempt, 1)
}

// Verdicts of a PhaseCheck. Only PhaseOutside and PhaseDirty hold the
// phases that wait for it.
const (
	PhaseAccepted        = "accepted"
	PhaseOutside         = "outside-scope"
	PhaseDirty           = "dirty"
	PhaseNoChanges       = "no-changes"
	PhaseNoDeclaredScope = "no-declared-scope"
	PhaseUnknown         = "unknown"
)

// PhaseCheck is the kit's check of a done plan phase: what changed in its
// checkout since the phase started, measured against its declared scope.
type PhaseCheck struct {
	// Changed are the files committed since the phase started, and those
	// left uncommitted; Outside the ones outside the declared scope.
	Changed []string `json:"changed,omitempty"`
	Outside []string `json:"outside,omitempty"`
	Commits int      `json:"commits"`
	// Dirty lists the files left uncommitted in the checkout, including
	// those an earlier phase left, and Clean says there are none.
	Dirty   []string `json:"dirty,omitempty"`
	Clean   bool     `json:"clean"`
	Verdict string   `json:"verdict"`
	// Note says why the verdict is unknown, or which files left the
	// checkout dirty.
	Note string `json:"note,omitempty"`
}

// Holds reports whether the check holds the phases that wait for it.
func (c *PhaseCheck) Holds() bool {
	return c != nil && (c.Verdict == PhaseOutside || c.Verdict == PhaseDirty)
}

// Review is what a review task reviews and what it found.
type Review struct {
	Of    string `json:"of,omitempty"`    // the reviewed task; empty for a plan review
	Final bool   `json:"final,omitempty"` // a plan's review before plan-done
	// Writer is the writer's agent key; quota fallback never switches the
	// reviewer to it.
	Writer string `json:"writer,omitempty"`
	// Base is the commit the reviewed diff starts from, File the report
	// the reviewer writes, Earlier the reports of the plan's earlier
	// reviews; all set when the review launches.
	Base    string   `json:"base,omitempty"`
	File    string   `json:"file,omitempty"`
	Earlier []string `json:"earlier,omitempty"`
	// Verdict is ReviewPass, ReviewFix or ReviewUnknown, read from File
	// when the reviewer reports done; Note says why it is unknown, or
	// that the checkout changed during the review.
	Verdict string `json:"verdict,omitempty"`
	Note    string `json:"note,omitempty"`
}

// Review verdicts.
const (
	ReviewPass    = "pass"
	ReviewFix     = "fix"
	ReviewUnknown = "unknown"
)

// GitState is a snapshot of a task's checkout, taken when its runner reports.
type GitState struct {
	// Worktree is set for the task's own worktree, and false for the
	// checkout it was spawned in, which may hold other work too.
	Worktree bool   `json:"worktree,omitempty"`
	Branch   string `json:"branch,omitempty"`
	// Base is the default branch AheadOfBase counts against.
	Base string `json:"base,omitempty"`
	// Upstream is empty when the branch tracks nothing; Ahead and Behind
	// count commits against it.
	Upstream    string `json:"upstream,omitempty"`
	AheadOfBase int    `json:"aheadOfBase,omitempty"`
	Ahead       int    `json:"ahead,omitempty"`
	Behind      int    `json:"behind,omitempty"`
	Uncommitted int    `json:"uncommitted,omitempty"`
	Untracked   int    `json:"untracked,omitempty"`
	// UntrackedPlans is the share of Untracked under plans/.
	UntrackedPlans int `json:"untrackedPlans,omitempty"`
	// Head is the short sha of HEAD.
	Head string `json:"head,omitempty"`
	// SinceStart counts the commits made since the round began. Only
	// snapshots of the checkout a task was spawned in read it: there
	// AheadOfBase says nothing about a runner that commits on the default
	// branch itself.
	SinceStart int `json:"sinceStart,omitempty"`
	// Stacked lists the local branches the worktree's branch is built on:
	// merged into HEAD but not yet into Base. Only worktree snapshots
	// with a known Base read them.
	Stacked   []string  `json:"stacked,omitempty"`
	CheckedAt time.Time `json:"checkedAt,omitzero"`
}

// Short describes the snapshot in one line, such as "on hrk/t3-fix, 2
// uncommitted, 1 commit not pushed". A branch that diverged from its
// upstream is described as such; whether to rewrite the remote is the
// user's call, so the text never suggests it.
func (g *GitState) Short() string {
	head := "on " + g.Branch
	if g.Branch == "" || g.Branch == "(detached)" {
		head = "detached HEAD"
	}
	if !g.Worktree {
		head = "repo state, " + head
	}
	var parts []string
	if g.Uncommitted > 0 {
		parts = append(parts, fmt.Sprintf("%d uncommitted", g.Uncommitted))
	}
	if g.Untracked > 0 {
		untracked := fmt.Sprintf("%d untracked", g.Untracked)
		if g.UntrackedPlans > 0 {
			untracked += fmt.Sprintf(" (%d under plans/)", g.UntrackedPlans)
		}
		parts = append(parts, untracked)
	}
	switch {
	case g.Upstream != "" && g.Ahead > 0 && g.Behind > 0:
		parts = append(parts, fmt.Sprintf("diverged from %s (%d ahead, %d behind)", g.Upstream, g.Ahead, g.Behind))
	case g.Upstream != "" && g.Ahead > 0:
		parts = append(parts, plural(g.Ahead, "commit")+" not pushed")
	case g.Upstream != "" && g.Behind > 0:
		parts = append(parts, fmt.Sprintf("%d behind %s", g.Behind, g.Upstream))
	case g.Upstream == "" && g.AheadOfBase > 0:
		parts = append(parts, plural(g.AheadOfBase, "commit")+", no upstream")
	}
	headShown := false
	if g.SinceStart > 0 {
		since := plural(g.SinceStart, "commit") + " since start"
		if g.Head != "" {
			since += ", head " + g.Head
		}
		parts, headShown = append(parts, since), true
	}
	if len(parts) == 0 {
		switch {
		case g.AheadOfBase == 0 && g.Base != "":
			parts = append(parts, "no new commits")
		case g.Upstream != "":
			parts = append(parts, "clean, pushed")
		default:
			parts = append(parts, "clean")
		}
	}
	if len(g.Stacked) > 0 {
		parts = append(parts, "stacked: "+strings.Join(g.Stacked, ", "))
	}
	if g.Head != "" && !headShown {
		parts = append(parts, "head "+g.Head)
	}
	return head + ", " + strings.Join(parts, ", ")
}

// JoinQuestions stores the questions of one needs-human report as a single
// question: one question as it is, a round of several numbered one per line,
// each on a single line, such as "1. Which database?\n2. Which host?".
func JoinQuestions(qs []string) string {
	if len(qs) == 1 {
		return qs[0]
	}
	lines := make([]string, len(qs))
	for i, q := range qs {
		lines[i] = fmt.Sprintf("%d. %s", i+1, strings.Join(strings.Fields(q), " "))
	}
	return strings.Join(lines, "\n")
}

// QuestionRound returns the questions of a round stored by JoinQuestions,
// without their numbers, or nil for a single question.
func QuestionRound(q string) []string {
	lines := strings.Split(q, "\n")
	if len(lines) < 2 {
		return nil
	}
	qs := make([]string, len(lines))
	for i, l := range lines {
		rest, ok := strings.CutPrefix(l, fmt.Sprintf("%d. ", i+1))
		if !ok {
			return nil
		}
		qs[i] = rest
	}
	return qs
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// HeldSummary is the news line of a task that waits for release.
func HeldSummary(t *Task) string {
	s := "held for release: " + t.Title
	if t.HoldReason != "" {
		s += " (" + t.HoldReason + ")"
	}
	return s
}
