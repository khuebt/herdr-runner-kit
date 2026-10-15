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
		Use:   "report <done|needs-human|failed|handoff>",
		Short: "Report a runner's result to the main session",
		Long: "report is called by runners. It records the result, wakes the main " +
			"session's wait, and starts tasks that waited for this one. The task id " +
			"comes from $HRK_TASK, which spawn sets in the runner's tab. handoff is " +
			"only for a runner the kit told that its context is nearly full: the task " +
			"continues once in a fresh session that reads the handoff file first.",
		Example: `  herdr-runner-kit report done --summary "Login fixed; tests pass" --output out/notes.md
  herdr-runner-kit report done --summary "Export added" --decision "CSV or JSON? -> CSV"
  herdr-runner-kit report needs-human --question "Drop the legacy endpoint? (recommended: yes)"
  herdr-runner-kit report needs-human --question "Which database? a) Postgres (recommended) b) SQLite" --question "Host? a) VPS (recommended) b) PaaS"
  herdr-runner-kit report failed --summary "Build needs Go 1.27; only 1.25 is installed"
  herdr-runner-kit report handoff --output out/handoff-a2.md --summary "Parser done; exporter left"`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"done", "needs-human", "failed", "handoff"},
		RunE: func(cmd *cobra.Command, args []string) error {
			o.Status = task.Status(args[0])
			if id == "" {
				id = os.Getenv("HRK_TASK")
				o.Attempt = runner.AttemptFromEnv(os.Getenv("HRK_ATTEMPT"))
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
			if args[0] == "handoff" {
				return reportHandoff(cmd, r, id, o)
			}
			t, err := r.Report(id, o)
			if err != nil {
				return err
			}
			if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %s reported to the main session\n", t.ID, t.Status); err != nil {
				return err
			}
			for _, note := range r.ReportNotes(t.ID, o) {
				fmt.Fprintln(cmd.ErrOrStderr(), note)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Summary, "summary", "", "one or two sentences: what was done, or what failed and why")
	f.StringArrayVar(&o.Questions, "question", nil,
		"for needs-human: one question with the options and your recommendation (repeatable: a round of up to 5)")
	f.StringArrayVar(&o.Decisions, "decision", nil,
		`for done: an open question you settled with the recommended option, as "<question> -> <choice>" (repeatable, up to 7)`)
	f.StringArrayVar(&o.Outputs, "output", nil, "file for the main session (repeatable); for handoff, the one handoff file")
	f.StringVar(&id, "task", "", "task id, for manual recovery outside a runner")
	return cmd
}

// reportHandoff ends the runner's session near its context limit; a fresh
// session of the same task continues from the handoff file.
func reportHandoff(cmd *cobra.Command, r *runner.Runner, id string, o runner.ReportOptions) error {
	if o.Manual {
		return errors.New("report handoff runs only inside a runner")
	}
	t, err := r.Handoff(id, o)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%s continues in attempt %d; stop now, this session is closing\n", t.ID, task.AttemptOf(t)); err != nil {
		return err
	}
	for _, note := range r.ReportNotes(t.ID, o) {
		fmt.Fprintln(cmd.ErrOrStderr(), note)
	}
	return nil
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

// newContextCommand is the Claude runners' PreCompact and
// SessionStart(compact) hook. Like the turn-end hook it never fails: errors
// go to stderr, and it prints nothing unless it asks for a handoff.
func newContextCommand() *cobra.Command {
	var h string
	cmd := &cobra.Command{
		Use:    "__context",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			payload, _ := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
			id := os.Getenv("HRK_TASK")
			if id == "" || task.Harness(h) != task.Claude {
				return nil
			}
			r, err := openRunner()
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "herdr-runner-kit __context:", err)
				return nil
			}
			out, err := r.ContextHook(cmd.Context(), id, payload)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "herdr-runner-kit __context:", err)
			}
			if out != "" {
				fmt.Fprintln(cmd.OutOrStdout(), out)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&h, "harness", "claude", "claude")
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
