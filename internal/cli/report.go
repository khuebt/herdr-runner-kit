package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

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
		Example: `  hrk report done --summary "Login fixed; tests pass" --output out/notes.md
  hrk report done --summary "Export added" --decision "CSV or JSON? -> CSV"
  hrk report needs-human --question "Drop the legacy endpoint? (recommended: yes)"
  hrk report needs-human --question "Which database? a) Postgres (recommended) b) SQLite" --question "Host? a) VPS (recommended) b) PaaS"
  hrk report failed --summary "Build needs Go 1.27; only 1.25 is installed"
  hrk report handoff --output out/handoff-a2.md --summary "Parser done; exporter left"`,
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
			line := fmt.Sprintf("%s %s reported to the main session", t.ID, t.Status)
			if o.Status == task.Done && t.Status != task.Done {
				line = fmt.Sprintf("%s done recorded; it reaches the main session when this turn ends", t.ID)
			}
			if _, err = fmt.Fprintln(cmd.OutOrStdout(), line); err != nil {
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

func newUsageReaderCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "__usage <id>",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			r, err := openRunner()
			if err != nil {
				return err
			}
			r.AwaitUsage(args[0])
			return nil
		},
	}
}

// newTurnEndCommand is the runners' turn-end hook. It never fails the hook:
// errors go to stderr and the command exits 0.
func newTurnEndCommand() *cobra.Command {
	var h string
	cmd := &cobra.Command{
		Use:    "__turn-end [notify-json]",
		Hidden: true,
		Args:   cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			adapter, payload, ok := hookInput(cmd, "__turn-end", h, args)
			if !ok {
				return nil
			}
			r, err := openRunner()
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "hrk __turn-end:", err)
				return nil
			}
			block, err := r.TurnEnd(cmd.Context(), os.Getenv("HRK_TASK"), adapter.Harness(), payload)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "hrk __turn-end:", err)
			}
			if reply := adapter.HookReply(block); reply != "" {
				fmt.Fprintln(cmd.OutOrStdout(), reply)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&h, "harness", "claude", harnessFlagUsage())
	return cmd
}

// newContextCommand is the runners' compaction and session-start hook, fed
// Claude-shaped PreCompact and SessionStart inputs. Like the turn-end hook it
// never fails: errors go to stderr, and it prints nothing unless it asks for
// a handoff and the agent takes added context.
func newContextCommand() *cobra.Command {
	var h string
	cmd := &cobra.Command{
		Use:    "__context",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			adapter, payload, ok := hookInput(cmd, "__context", h, args)
			if !ok {
				return nil
			}
			r, err := openRunner()
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "hrk __context:", err)
				return nil
			}
			out, err := r.ContextHook(cmd.Context(), os.Getenv("HRK_TASK"), adapter.Harness(), payload)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "hrk __context:", err)
			}
			if out != "" {
				fmt.Fprintln(cmd.OutOrStdout(), out)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&h, "harness", "claude", harnessFlagUsage())
	return cmd
}

// hookInput reads a hook's input through the adapter of harness h. ok is
// false when there is nothing to handle: no payload, or no runner task.
func hookInput(cmd *cobra.Command, name, h string, args []string) (harness.Adapter, []byte, bool) {
	adapter, err := harness.For(task.Harness(h))
	if err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "hrk "+name+":", err)
		return nil, nil, false
	}
	payload, err := adapter.HookPayload(args, cmd.InOrStdin())
	if err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "hrk "+name+":", err)
		return nil, nil, false
	}
	if payload == nil || os.Getenv("HRK_TASK") == "" {
		return nil, nil, false
	}
	return adapter, payload, true
}

func harnessFlagUsage() string {
	names := make([]string, 0, len(task.Harnesses()))
	for _, h := range task.Harnesses() {
		names = append(names, string(h))
	}
	return "the runner's harness: " + strings.Join(names, ", ")
}
