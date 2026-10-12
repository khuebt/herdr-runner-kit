// Package cli wires the herdr-runner-kit commands.
package cli

import (
	"github.com/spf13/cobra"
)

// NewRootCommand builds the command tree.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "herdr-runner-kit",
		Short: "Run every coding, research and report task as a visible herdr runner",
		Long: "herdr-runner-kit opens Claude Code and Codex runners in herdr tabs, " +
			"reports their results to the main agent as events, and routes human " +
			"decisions through the main chat or Telegram.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		newVersionCommand(),
		newSpawnCommand(),
		newLaunchCommand(),
		newListCommand(),
		newShowCommand(),
		newSendCommand(),
		newStopCommand(),
		newFocusCommand(),
		newGCCommand(),
		newReportCommand(),
		newAfterCommand(),
		newTurnEndCommand(),
		newWaitCommand(),
		newAnswerCommand(),
		newReleaseCommand(),
		newEventsCommand(),
		newMainCommand(),
		newNotifyCommand(),
		newAutoarmCommand(),
		newSetupCommand(),
		newDoctorCommand(),
		newUninstallCommand(),
	)
	return root
}
