package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/khuebt/herdr-runner-kit/internal/project"
	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/state"
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
			o.Warn = func(line string) { fmt.Fprintln(cmd.ErrOrStderr(), line) }
			t, err := r.Spawn(cmd.Context(), o)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), spawnLine(t)); err != nil {
				return err
			}
			if t.Project != "" {
				parallelNote(r.Store, t.Project, o.Warn)
			}
			if t.Worktree != nil && o.Branch == "" {
				recent := runner.RecentBranches(cmd.Context(), t.RepoRoot, r.Config.WorktreeBranchPrefix, t.Worktree.Branch, 5)
				_, err = fmt.Fprintln(cmd.ErrOrStderr(), branchNote(t.Worktree.Branch, recent))
				return err
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Kind, "kind", "", "task kind, which selects the default agent and worktree (see docs/config.md)")
	f.StringVar(&o.Agent, "agent", "", "agent spec harness[:model[:effort]], overriding the kind's default")
	f.BoolVar(&worktree, "worktree", false, "run in a new git worktree")
	f.BoolVar(&noWorktree, "no-worktree", false, "run in the current folder")
	f.StringVar(&o.Branch, "branch", "", "name of the new worktree branch, following the repository's convention (default hrk/<id>-<slug>)")
	f.StringVar(&o.Base, "base", "", "git ref the worktree branch starts from (default HEAD)")
	f.StringVar(&o.Skill, "skill", "", "skill to invoke, such as ak:cook; rendered per harness")
	f.StringSliceVar(&o.After, "after", nil, "start only when these tasks are done")
	f.StringVar(&gate, "gate", "auto", "auto starts on its own; human waits for `release`")
	f.StringVar(&o.Reason, "reason", "", "with --gate human: why the task waits, such as the dependency D2 it needs; only a release that names the task opens it")
	f.StringVar(&o.ClaudeConfig, "claude-config", "", "Claude config folder of a Claude runner, as a path or a name such as claude2 for ~/.claude2 (default: the main session's)")
	f.StringVar(&o.Language, "language", "", "language of the runner's report summaries and questions, such as Vietnamese (default: the config's language)")
	f.StringVar(&o.Cwd, "cwd", "", "folder the task works in (default: current folder)")
	f.StringVar(&o.Title, "title", "", "short title (default: from the prompt)")
	f.BoolVar(&stdin, "stdin", false, "read the prompt from stdin")
	f.StringVar(&o.Project, "project", "", "registered project the task works for; it runs in the project's folder and reports to project:<name> (default: the project holding --cwd, if any)")
	f.StringVar(&o.Owner, "owner", "", "main session the task reports to (default: git top-level of the current folder, or project:<name> for a project task)")
	return cmd
}

// branchNote tells the main session that a worktree task got the kit's
// default branch name, and which names the repository uses, so the next
// spawn can follow its convention with --branch.
func branchNote(branch string, recent []string) string {
	note := "note: branch " + branch + " is the kit's default"
	if len(recent) == 0 {
		return note + "."
	}
	return note + "; recent branches here: " + strings.Join(recent, ", ") +
		". Pass --branch to follow the repository's convention."
}

// parallelNote tells the main session, through warn, when more runners of
// project name are active than its record's parallelWarning. It is a note
// only: nothing is blocked.
func parallelNote(s *state.Store, name string, warn func(string)) {
	rec, err := project.Load(s, name)
	if errors.Is(err, project.ErrNoRecord) {
		return
	}
	var tasks []*task.Task
	if err == nil {
		tasks, err = s.List()
	}
	if err != nil {
		warn("note: cannot count the active runners of " + name + ": " + err.Error())
		return
	}
	if note := project.ParallelNote(rec, tasks); note != "" {
		warn(note)
	}
}

func spawnLine(t *task.Task) string {
	parts := []string{t.ID + " " + string(t.Status), t.Agent, t.Kind}
	if t.Worktree != nil {
		parts = append(parts, "worktree "+t.Worktree.Branch)
	}
	if len(t.After) > 0 && t.Status == task.Queued {
		parts = append(parts, "after "+strings.Join(t.After, ","))
	}
	if t.HoldReason != "" {
		parts = append(parts, "held for: "+t.HoldReason)
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
