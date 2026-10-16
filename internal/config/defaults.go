package config

import (
	"slices"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/buildinfo"
)

// SchemaVersion is the config file format this binary reads and writes.
const SchemaVersion = buildinfo.SchemaVersion

// Default returns the built-in configuration. Claude runs every kind by
// default, because the ak and other skills are installed for Claude; Codex,
// pi and omp are chosen with --agent or the config. An empty model or effort
// leaves the choice to the CLI's own config.
func Default() Config {
	return Config{
		SchemaVersion: SchemaVersion,
		Kinds: map[string]Kind{
			"code":     {Agent: "claude:claude-sonnet-5-5:medium", Worktree: true},
			"flow":     {Agent: "claude:claude-opus-5-5:high", Worktree: true},
			"fix":      {Agent: "claude:claude-sonnet-5-5:medium", Worktree: false},
			"research": {Agent: "claude:claude-opus-5-5:high", Worktree: false},
			"report":   {Agent: "claude:claude-opus-5-5:high", Worktree: false},
		},
		DefaultKind:          "code",
		WorktreeBranchPrefix: "hrk/",
		Telegram: Telegram{
			Notify: slices.Clone(NotifyEvents),
		},
		UpdateCheck: true,
		KeepFor:     4 * time.Hour,
		StallAfter:  10 * time.Minute,
		// The hooks the handoff relies on were confirmed on Claude Code
		// 2.1.300 and codex-cli 0.162.1.
		ContextHandoff:   true,
		ContextHandoffAt: 15,
		RunnerMemoryMax:  "25%",
		HeavySlots:       2,
		RunnerInput:      InputLocked,
	}
}
