// Package cli wires the herdr-runner-kit commands.
package cli

import (
	"github.com/spf13/cobra"
)

// NewRootCommand builds the command tree.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "hrk",
		Short: "Run every coding, research and report task as a visible herdr runner",
		Long: "herdr-runner-kit opens Claude Code and Codex runners in herdr tabs, " +
			"reports their results to the main agent as events, and routes human " +
			"decisions through the main chat or Telegram.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		newVersionCommand(),
		newUpgradeCommand(),
		newSpawnCommand(),
		newLaunchCommand(),
		newListCommand(),
		newShowCommand(),
		newPeekCommand(),
		newSendCommand(),
		newStopCommand(),
		newKeepCommand(),
		newFocusCommand(),
		newGCCommand(),
		newCleanupCommand(),
		newSyncCommand(),
		newNewCommand(),
		newProjectCommand(),
		newPlanCommand(),
		newReportCommand(),
		newAfterCommand(),
		newUsageReaderCommand(),
		newTurnEndCommand(),
		newContextCommand(),
		newPromptGateCommand(),
		newWaitCommand(),
		newHeavyCommand(),
		newAnswerCommand(),
		newReleaseCommand(),
		newEventsCommand(),
		newUsageCommand(),
		newMainCommand(),
		newNotifyCommand(),
		newAutoarmCommand(),
		newSetupCommand(),
		newDoctorCommand(),
		newUninstallCommand(),
	)
	return root
}
