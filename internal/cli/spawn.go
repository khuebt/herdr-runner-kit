package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func newSpawnCommand() *cobra.Command {
	var o runner.SpawnOptions
	var worktree, noWorktree bool
	var gate string
	var stdin bool
	cmd := &cobra.Command{
		Use:     "spawn [flags] -- <prompt> | spawn [flags] --stdin",
		Aliases: []string{"run"},
		Short:   "Start a runner for a task and return at once",
		Long: "spawn records a task and opens a runner for it in a new herdr tab, or in a " +
			"new worktree workspace. It returns before the agent is ready; progress " +
			"arrives as events that `wait` reports.",
		Example: `  herdr-runner-kit spawn --kind research -- "Compare fsnotify and polling for our wait loop"
  herdr-runner-kit spawn --skill ak:cook -- plans/260101-auth/plan.md
  herdr-runner-kit spawn --after t3 --agent codex::high -- "Add tests for the new parser"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if worktree && noWorktree {
				return fmt.Errorf("--worktree and --no-worktree are mutually exclusive")
			}
			if worktree || noWorktree {
				o.Worktree = &worktree
			}
			o.Gate = task.Gate(gate)
			var err error
			if o.Prompt, err = promptText(cmd, args, stdin); err != nil {
				return err
			}
			r, err := openRunner()
			if err != nil {
				return err
			}
			t, err := r.Spawn(cmd.Context(), o)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), spawnLine(t))
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Kind, "kind", "", "task kind, which selects the default agent and worktree (see docs/config.md)")
	f.StringVar(&o.Agent, "agent", "", "agent spec harness[:model[:effort]], overriding the kind's default")
	f.BoolVar(&worktree, "worktree", false, "run in a new git worktree")
	f.BoolVar(&noWorktree, "no-worktree", false, "run in the current folder")
	f.StringVar(&o.Base, "base", "", "git ref the worktree branch starts from (default HEAD)")
	f.StringVar(&o.Skill, "skill", "", "skill to invoke, such as ak:cook; rendered per harness")
	f.StringSliceVar(&o.After, "after", nil, "start only when these tasks are done")
	f.StringVar(&gate, "gate", "auto", "auto starts on its own; human waits for `release`")
	f.StringVar(&o.Cwd, "cwd", "", "folder the task works in (default: current folder)")
	f.StringVar(&o.Title, "title", "", "short title (default: from the prompt)")
	f.BoolVar(&stdin, "stdin", false, "read the prompt from stdin")
	f.StringVar(&o.Owner, "owner", "", "main session the task reports to (default: git top-level of the current folder)")
	return cmd
}

func spawnLine(t *task.Task) string {
	parts := []string{t.ID + " " + string(t.Status), t.Agent, t.Kind}
	if t.Worktree != nil {
		parts = append(parts, "worktree "+t.Worktree.Branch)
	}
	if len(t.After) > 0 && t.Status == task.Queued {
		parts = append(parts, "after "+strings.Join(t.After, ","))
	}
	parts = append(parts, t.Title)
	return strings.Join(parts, " · ")
}

func newLaunchCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "__launch <id>",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			r, err := openRunner()
			if err != nil {
				return err
			}
			return r.Launch(context.Background(), args[0])
		},
	}
}
