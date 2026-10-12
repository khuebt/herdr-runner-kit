package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/khuebt/herdr-runner-kit/internal/harness"
	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func newReportCommand() *cobra.Command {
	var o runner.ReportOptions
	var id string
	cmd := &cobra.Command{
		Use:   "report <done|needs-human|failed>",
		Short: "Report a runner's result to the main session",
		Long: "report is called by runners. It records the result, wakes the main " +
			"session's wait, and starts tasks that waited for this one. The task id " +
			"comes from $HRK_TASK, which spawn sets in the runner's tab.",
		Example: `  herdr-runner-kit report done --summary "Login fixed; tests pass" --output out/notes.md
  herdr-runner-kit report needs-human --question "Drop the legacy endpoint? (recommended: yes)"
  herdr-runner-kit report failed --summary "Build needs Go 1.27; only 1.25 is installed"`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"done", "needs-human", "failed"},
		RunE: func(cmd *cobra.Command, args []string) error {
			o.Status = task.Status(args[0])
			if id == "" {
				id = os.Getenv("HRK_TASK")
			} else {
				o.Manual = true
			}
			if id == "" {
				return errors.New("no task: run report inside a runner, or pass --task")
			}
			id, err := parseTaskID(id)
			if err != nil {
				return err
			}
			r, err := openRunner()
			if err != nil {
				return err
			}
			t, err := r.Report(id, o)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %s reported to the main session\n", t.ID, t.Status)
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Summary, "summary", "", "one or two sentences: what was done, or what failed and why")
	f.StringVar(&o.Question, "question", "", "for needs-human: one question with the options and your recommendation")
	f.StringArrayVar(&o.Outputs, "output", nil, "file for the main session (repeatable)")
	f.StringVar(&id, "task", "", "task id, for manual recovery outside a runner")
	return cmd
}

func newAfterCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "__after <id>",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			r, err := openRunner()
			if err != nil {
				return err
			}
			return r.After(context.Background(), args[0])
		},
	}
}

// newTurnEndCommand is the runners' turn-end hook. It never fails the hook:
// errors go to stderr and the command exits 0.
func newTurnEndCommand() *cobra.Command {
	var h string
	cmd := &cobra.Command{
		Use:    "__turn-end [codex-notify-json]",
		Hidden: true,
		Args:   cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var payload []byte
			if task.Harness(h) == task.Codex {
				if len(args) == 0 {
					return nil
				}
				payload = []byte(args[0])
				callUserCodexNotify(args[0])
			} else {
				payload, _ = io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
			}
			id := os.Getenv("HRK_TASK")
			if id == "" {
				return nil
			}
			r, err := openRunner()
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "herdr-runner-kit __turn-end:", err)
				return nil
			}
			block, err := r.TurnEnd(cmd.Context(), id, task.Harness(h), payload)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "herdr-runner-kit __turn-end:", err)
			}
			if block != "" && task.Harness(h) == task.Claude {
				fmt.Fprintln(cmd.OutOrStdout(), harness.ClaudeBlock(block))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&h, "harness", "claude", "claude or codex")
	return cmd
}

// callUserCodexNotify runs the notify command from the user's config.toml,
// detached, with the same argument Codex passed.
func callUserCodexNotify(arg string) {
	path, err := harness.CodexConfigPath()
	if err != nil {
		return
	}
	argv, err := harness.CodexUserNotify(path)
	if err != nil || len(argv) == 0 {
		return
	}
	c := exec.Command(argv[0], append(argv[1:], arg)...)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if c.Start() == nil {
		_ = c.Process.Release()
	}
}
