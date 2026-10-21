package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
	"github.com/khuebt/herdr-runner-kit/internal/wait"
)

func newWaitCommand() *cobra.Command {
	var o wait.Options
	var ids, projects []string
	cmd := &cobra.Command{
		Use:   "wait",
		Short: "Block until a task has news, then print one line per event",
		Long: "wait blocks until one of your tasks reports done, needs-human or failed, " +
			"is held, stalls, hits a usage limit, ends a turn without reporting, or its plan " +
			"run finishes. It prints one short line per event and exits 0. Quiet news (a " +
			"chain step's or plan phase's done without decisions, stopped, cleanable, " +
			"processes-left) does not wake it: it comes first with the next wake, when " +
			"everything is idle, at the timeout, or 50 minutes after the main session was " +
			"last active. --task, --all-news and wakeOn: all wake for every line. --task keeps " +
			"a cursor of its own, named from the ids, so it never moves the main session's " +
			"cursor and runs beside the auto re-arm hook. Run it in " +
			"the background and rerun it after each wake; a cursor makes sure no event is " +
			"skipped or repeated.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			for _, a := range ids {
				id, err := parseTaskID(a)
				if err != nil {
					return err
				}
				o.Tasks = append(o.Tasks, id)
			}
			r, err := openRunner()
			if err != nil {
				return err
			}
			if o.Owner == "" {
				o.Owner = currentOwner(cmd)
			}
			if o.Projects, err = resolveProjects(r.Store, projects); err != nil {
				return err
			}
			o.FlushOnTimeout = true // the timeout wakes the main session anyway
			_ = wait.MarkActive(r.Store, o)
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			res, err := wait.Run(ctx, r, o)
			out := cmd.OutOrStdout()
			var held *state.HeldError
			switch {
			case errors.As(err, &held):
				_, err = fmt.Fprintln(out, alreadyWaiting(held))
				return err
			case errors.Is(err, context.Canceled):
				return errors.New("wait interrupted")
			case err != nil:
				return err
			case res.TimedOut:
				_, err = fmt.Fprintf(out, "timeout: no news in %s; rerun wait\n", o.Timeout)
			case res.Idle != "":
				_, err = fmt.Fprintln(out, res.Idle)
			default:
				_, err = fmt.Fprintln(out, strings.Join(res.Lines, "\n"))
			}
			return err
		},
	}
	f := cmd.Flags()
	f.StringSliceVar(&ids, "task", nil, "only these tasks")
	f.StringVar(&o.Owner, "owner", "", "main session to wait for (default: git top-level of the current folder); its tasks come with those of the project whose repository holds it and of the projects attached to it")
	f.StringArrayVar(&projects, "project", nil, "also wait for this project, by name or by a path inside its repository (repeatable)")
	f.StringVar(&o.Cursor, "cursor", "", "cursor name (default: derived from the owner, or from the --task ids)")
	f.BoolVar(&o.AllNews, "all-news", false, "wake for every news line, quiet ones too")
	f.DurationVar(&o.Timeout, "timeout", wait.DefaultTimeout, "safety timeout; wait prints `timeout` and exits 0")
	return cmd
}

// alreadyWaiting says who holds the cursor, so the agent knows whether news
// still reaches it.
func alreadyWaiting(held *state.HeldError) string {
	switch held.Holder {
	case autoarmHolder:
		return fmt.Sprintf("already waiting: the main session's auto re-arm hook holds this cursor (pid %d); it wakes you on news", held.PID)
	case "wait":
		return fmt.Sprintf("already waiting: another hrk wait holds this cursor (pid %d)", held.PID)
	}
	return "already waiting: another wait holds this cursor"
}

func newAnswerCommand() *cobra.Command {
	var fromTelegram, stdin bool
	cmd := &cobra.Command{
		Use:   "answer <id>[a<attempt>] [--stdin | <text>] | answer --from-telegram [--stdin | <text>]",
		Short: "Answer a runner that reported needs-human; it continues in the same session",
		Long: "answer types the answer into the runner that asked. With --from-telegram, the " +
			"text is a Telegram reply: a leading #id picks the task; without one, the only task " +
			"waiting for you gets it. A held task is released by ok, yes or duyệt, and any other " +
			"text releases it with that note. An id may name the runner's attempt, such as " +
			"t3a2; an answer for an attempt that ended is refused. Pass the reply with --stdin " +
			"and a quoted heredoc, never inside a shell string.",
		Example: `  hrk answer t3 "Use option A"
  hrk answer --from-telegram --stdin <<'EOF'
  #t3 dùng phương án A
  EOF`,
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRunner()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if fromTelegram {
				text, err := promptText(cmd, args, stdin)
				if err != nil {
					return err
				}
				line, err := r.RouteTelegram(cmd.Context(), text)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(out, line)
				return err
			}
			if len(args) < 1 {
				return errors.New("answer needs <id> and the text")
			}
			id, attempt, err := task.ParseAttemptRef(args[0])
			if err != nil {
				return err
			}
			text, err := promptText(cmd, args[1:], stdin)
			if err != nil {
				return err
			}
			if text == "" {
				return errors.New("answer needs the text")
			}
			if err := r.CheckAttempt(id, attempt); err != nil {
				return err
			}
			working, err := r.Answer(cmd.Context(), id, text)
			if err != nil {
				return err
			}
			msg := id + " got the answer and is working"
			if !working {
				msg = id + " got the answer; herdr has not seen it start a turn yet"
			}
			_, err = fmt.Fprintln(out, msg)
			return err
		},
	}
	cmd.Flags().BoolVar(&fromTelegram, "from-telegram", false, "route a Telegram reply by its #id tag")
	cmd.Flags().BoolVar(&stdin, "stdin", false, "read the text from stdin")
	return cmd
}

func newReleaseCommand() *cobra.Command {
	var note string
	cmd := &cobra.Command{
		Use:   "release <id>",
		Short: "Open a held task's human gate and start it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseTaskID(args[0])
			if err != nil {
				return err
			}
			r, err := openRunner()
			if err != nil {
				return err
			}
			t, err := r.Release(id, note)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %s · %s\n", t.ID, t.Status, t.Title)
			return err
		},
	}
	cmd.Flags().StringVar(&note, "note", "", "text added to the task's prompt")
	return cmd
}
