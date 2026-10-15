package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// Peek bounds: the lines it reads by default, and the most it reads.
const (
	defaultPeekLines = 30
	maxPeekLines     = 200
)

func newPeekCommand() *cobra.Command {
	var lines int
	cmd := &cobra.Command{
		Use:   "peek <id>",
		Short: "Print the visible screen of a task's runner, with secrets hidden",
		Long: "peek reads the last lines of a runner's visible pane, with anything that looks like a " +
			"secret hidden. It never types into the pane.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if lines < 1 || lines > maxPeekLines {
				return fmt.Errorf("--lines must be between 1 and %d, got %d", maxPeekLines, lines)
			}
			id, err := parseTaskID(args[0])
			if err != nil {
				return err
			}
			r, err := openRunner()
			if err != nil {
				return err
			}
			screen, err := r.Peek(cmd.Context(), id, lines)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), strings.TrimRight(screen, "\n"))
			return err
		},
	}
	cmd.Flags().IntVar(&lines, "lines", defaultPeekLines, fmt.Sprintf("how many lines to show, at most %d", maxPeekLines))
	return cmd
}
