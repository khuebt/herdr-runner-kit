package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/khuebt/herdr-runner-kit/internal/runner"
)

func newSyncCommand() *cobra.Command {
	var from string
	var noSend bool
	cmd := &cobra.Command{
		Use:   "sync <id>",
		Short: "Merge the default branch into a runner's worktree branch and tell the runner",
		Long: "sync merges origin's default branch (such as origin/main, where pull requests " +
			"merged on the remote land), or --from REF, into the branch checked out in the " +
			"task's worktree, then asks the runner to re-run its checks and report. It refuses " +
			"while the agent is busy, runs background work or cannot be checked, or while the " +
			"worktree has changes; only then does it fetch origin, without pruning. Use --from " +
			"main to merge a local main that origin does not have yet. A conflicting merge is " +
			"aborted, and the text to hand it to the runner is printed. The output names the " +
			"ref merged. sync never pushes, rebases or touches another branch.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseTaskID(args[0])
			if err != nil {
				return err
			}
			r, err := openRunner()
			if err != nil {
				return err
			}
			res, err := r.Sync(cmd.Context(), id, strings.TrimSpace(from), !noSend)
			out := cmd.OutOrStdout()
			if errors.Is(err, runner.ErrSyncConflict) {
				fmt.Fprintf(out, "merging %s (%s) into %s conflicts in:\n", res.Ref, res.SHA, res.Branch)
				for _, f := range res.Conflicts {
					fmt.Fprintf(out, "  %s\n", f)
				}
				fmt.Fprintf(out, "to hand the merge to the runner, send it this (herdr-runner-kit send %s --stdin):\n%s\n",
					id, runner.SyncConflictNote(res.Ref, res.SHA, res.Conflicts))
				return err
			}
			if res == nil {
				return err
			}
			switch {
			case res.UpToDate:
				fmt.Fprintf(out, "%s already contains %s (%s)\n", res.Branch, res.Ref, res.SHA)
			case res.Sent && res.Working:
				fmt.Fprintf(out, "merged %s (%s) into %s; %s is re-running its checks\n", res.Ref, res.SHA, res.Branch, id)
			case res.Sent:
				fmt.Fprintf(out, "merged %s (%s) into %s; %s received the note; herdr has not seen it start a turn yet\n", res.Ref, res.SHA, res.Branch, id)
			default:
				fmt.Fprintf(out, "merged %s (%s) into %s; the runner was not told\n", res.Ref, res.SHA, res.Branch)
			}
			if res.LocalAhead != "" {
				fmt.Fprintf(out, "local %s has commits %s lacks; to merge them, run: herdr-runner-kit sync %s --from %s\n",
					res.LocalAhead, res.Ref, id, res.LocalAhead)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "ref to merge (default: origin's default branch, such as origin/main, after fetching origin)")
	cmd.Flags().BoolVar(&noSend, "no-send", false, "merge without telling the runner")
	return cmd
}
