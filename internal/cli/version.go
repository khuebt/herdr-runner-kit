package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/khuebt/herdr-runner-kit/internal/buildinfo"
)

func newVersionCommand() *cobra.Command {
	var asJSON, check bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version, build and contract information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := buildinfo.Get()
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(info)
			}
			if _, err := fmt.Fprintf(out, "herdr-runner-kit %s (commit %s, built %s, schema v%d, contract v%d)\n",
				info.Version, info.Commit, info.Date, info.SchemaVersion, info.ContractVersion); err != nil || !check {
				return err
			}
			latest, newer, err := updateNote(cmd.Context(), 0)
			switch {
			case err != nil:
				return fmt.Errorf("check for a newer release: %w", err)
			case newer:
				_, err = fmt.Fprintf(out, "%s is available; run hrk upgrade\n", latest)
			default:
				_, err = fmt.Fprintf(out, "up to date (latest release %s)\n", latest)
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	cmd.Flags().BoolVar(&check, "check", false, "also ask GitHub whether a newer release exists")
	return cmd
}
