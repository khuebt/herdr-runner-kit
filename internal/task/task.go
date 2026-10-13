// Package task defines the task model shared by the CLI, the store and the
// launcher. A task is one runner working on one prompt.
package task

import (
	"fmt"
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
	Herdr           Herdr     `json:"herdr"`
	// Error is the reason a task failed or needs a human, when the kit itself
	// set that status.
	Error string `json:"error,omitempty"`
	// Summary, Outputs and Question come from the runner's last report.
	Summary  string   `json:"summary,omitempty"`
	Outputs  []string `json:"outputs,omitempty"`
	Question string   `json:"question,omitempty"`
	// Inputs are the results of the --after tasks, added to the first prompt.
	Inputs string `json:"inputs,omitempty"`
	// Unreported counts turns that ended without a report since the last
	// prompt from the main session: the first is nudged, the second is news.
	Unreported int `json:"unreported,omitempty"`
	// CodexThread is the thread of the runner's first Codex turn. Notify
	// calls from other threads, such as title generation, are ignored.
	CodexThread string `json:"codexThread,omitempty"`
	// FallbackFrom lists, in order, the agents that hit a usage limit on
	// this task before it switched to its current one.
	FallbackFrom []string `json:"fallbackFrom,omitempty"`
	// Stalled is set while herdr reports the agent blocked on a prompt.
	Stalled bool `json:"stalled,omitempty"`
	// Language is the language the runner writes its report summaries and
	// questions in. Empty leaves it to the runner.
	Language string `json:"language,omitempty"`
	// Git is the checkout's state at the runner's last report.
	Git *GitState `json:"git,omitempty"`
	// Keep holds a finished runner's tab open: gc leaves it until keep is
	// turned off or the task is stopped.
	Keep bool `json:"keep,omitempty"`
	// FinishedAt is set when the task reaches a terminal status.
	FinishedAt     time.Time `json:"finishedAt,omitzero"`
	Status         Status    `json:"status"`
	LauncherPID    int       `json:"launcherPid,omitempty"`
	LaunchDeadline time.Time `json:"launchDeadline,omitzero"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

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
	Upstream    string    `json:"upstream,omitempty"`
	AheadOfBase int       `json:"aheadOfBase,omitempty"`
	Ahead       int       `json:"ahead,omitempty"`
	Behind      int       `json:"behind,omitempty"`
	Uncommitted int       `json:"uncommitted,omitempty"`
	Untracked   int       `json:"untracked,omitempty"`
	CheckedAt   time.Time `json:"checkedAt,omitzero"`
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
		parts = append(parts, fmt.Sprintf("%d untracked", g.Untracked))
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
	return head + ", " + strings.Join(parts, ", ")
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
