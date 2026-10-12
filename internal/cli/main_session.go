package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/khuebt/herdr-runner-kit/internal/state"
)

// telegramChannel is the official Telegram channel plugin. The main session
// runs it, so replies on Telegram reach the main agent.
const telegramChannel = "plugin:telegram@claude-plugins-official"

func newMainCommand() *cobra.Command {
	var run bool
	cmd := &cobra.Command{
		Use:   "main",
		Short: "Print, or run, the command that starts the main session with Telegram",
		Long: "main prints the command that starts the main Claude Code session with the " +
			"Telegram channel, so replies on Telegram reach it. Run only one such session per " +
			"bot: it answers tasks of every repository.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			argv := []string{"claude", "--channels", telegramChannel}
			if !run {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), argv[0]+" "+argv[1]+" "+argv[2])
				return err
			}
			path, err := exec.LookPath(argv[0])
			if err != nil {
				return fmt.Errorf("claude is not on PATH: %w", err)
			}
			return syscall.Exec(path, argv, os.Environ())
		},
	}
	cmd.Flags().BoolVar(&run, "exec", false, "start the main session now")
	return cmd
}

func newNotifyCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "__notify <event-json>",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			var ev state.Event
			if err := json.Unmarshal([]byte(args[0]), &ev); err != nil {
				return err
			}
			r, err := openRunner()
			if err != nil {
				return err
			}
			return r.SendNotification(context.Background(), ev)
		},
	}
}
