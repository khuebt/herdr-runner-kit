package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// newPromptGateCommand is the prompt-submit hook of a runner with locked
// input: Claude's UserPromptSubmit hook, or the input handler of the pi and
// omp runner extension, which shows what it prints as a warning. It prints the block reply for a prompt typed in the runner's tab,
// and nothing for one the kit typed or the agent's own. It fails open:
// errors go to stderr, the prompt goes through, and the command exits 0.
func newPromptGateCommand() *cobra.Command {
	var h string
	cmd := &cobra.Command{
		Use:    "__prompt-gate",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			adapter, payload, ok := hookInput(cmd, "__prompt-gate", h, args)
			if !ok {
				return nil
			}
			r, err := openRunner()
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "hrk __prompt-gate:", err)
				return nil
			}
			block, err := r.GatePrompt(os.Getenv("HRK_TASK"), adapter, payload)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "hrk __prompt-gate:", err)
			}
			if block != "" {
				fmt.Fprintln(cmd.OutOrStdout(), block)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&h, "harness", "claude", harnessFlagUsage())
	return cmd
}
