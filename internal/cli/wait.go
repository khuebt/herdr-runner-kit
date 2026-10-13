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
	"github.com/khuebt/herdr-runner-kit/internal/wait"
)

func newWaitCommand() *cobra.Command {
	var o wait.Options
	var ids []string
	cmd := &cobra.Command{
		Use:   "wait",
		Short: "Block until a task has news, then print one line per event",
		Long: "wait blocks until one of your tasks reports done, needs-human or failed, " +
			"is held or stopped, stalls, or ends a turn without reporting. It prints one " +
			"short line per event and exits 0. Run it in the background and rerun it after " +
			"each wake; a cursor makes sure no event is skipped or repeated.",
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
				if o.Also, err = projectOwners(r.Store); err != nil {
					return err
				}
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			res, err := wait.Run(ctx, r, o)
			out := cmd.OutOrStdout()
			switch {
			case errors.Is(err, state.ErrAlreadyWaiting):
				_, err = fmt.Fprintln(out, "already waiting: another wait holds this cursor")
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
	f.StringVar(&o.Owner, "owner", "", "main session to wait for, and only it (default: git top-level of the current folder, plus every registered project)")
	f.StringVar(&o.Cursor, "cursor", "", "cursor name (default: derived from the owner)")
	f.DurationVar(&o.Timeout, "timeout", wait.DefaultTimeout, "safety timeout; wait prints `timeout` and exits 0")
	return cmd
}

func newAnswerCommand() *cobra.Command {
	var fromTelegram, stdin bool
	cmd := &cobra.Command{
		Use:   "answer <id> [--stdin | <text>] | answer --from-telegram [--stdin | <text>]",
		Short: "Answer a runner that reported needs-human; it continues in the same session",
		Long: "answer types the answer into the runner that asked. With --from-telegram, the " +
			"text is a Telegram reply: a leading #id picks the task; without one, the only task " +
			"waiting for you gets it. A held task is released by ok, yes or duyệt, and any other " +
			"text releases it with that note. Pass the reply with --stdin and a quoted heredoc, " +
			"never inside a shell string.",
		Example: `  herdr-runner-kit answer t3 "Use option A"
  herdr-runner-kit answer --from-telegram --stdin <<'EOF'
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
			id, err := parseTaskID(args[0])
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
