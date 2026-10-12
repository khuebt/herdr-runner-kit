package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func parseTaskID(arg string) (string, error) {
	id, _, err := task.ParseID(arg)
	return id, err
}

func newSendCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "send <id> <text>",
		Short: "Type a new prompt into a running runner",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseTaskID(args[0])
			if err != nil {
				return err
			}
			r, err := openRunner()
			if err != nil {
				return err
			}
			working, err := r.Send(cmd.Context(), id, strings.Join(args[1:], " "))
			if err != nil {
				return err
			}
			msg := id + " received the prompt and is working"
			if !working {
				msg = id + " received the prompt; herdr has not seen it start a turn yet"
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), msg)
			return err
		},
	}
}

func newFocusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "focus <id>",
		Short: "Switch herdr to the runner's tab",
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
			return r.Focus(cmd.Context(), id)
		},
	}
}

func newStopCommand() *cobra.Command {
	var removeWorktree bool
	cmd := &cobra.Command{
		Use:   "stop <id>",
		Short: "Quit a runner's agent, close its tab and mark the task stopped",
		Long: "stop quits the agent and closes its tab. A worktree task keeps its workspace " +
			"and branch unless --remove-worktree is given; a dirty worktree is never removed, " +
			"and the branch is never deleted.",
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
			t, err := r.Stop(cmd.Context(), id, removeWorktree)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", t.ID, t.Status)
			return err
		},
	}
	cmd.Flags().BoolVar(&removeWorktree, "remove-worktree", false, "also remove a clean worktree (the branch is kept)")
	return cmd
}

func newGCCommand() *cobra.Command {
	var olderThan time.Duration
	var removeWorktree bool
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Close runners of finished tasks so they stop using memory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := openRunner()
			if err != nil {
				return err
			}
			closed, err := r.GC(cmd.Context(), olderThan, removeWorktree)
			if len(closed) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "closed %s\n", strings.Join(closed, ", "))
			} else if err == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "nothing to close")
			}
			return err
		},
	}
	cmd.Flags().DurationVar(&olderThan, "older-than", runner.GCAge, "close runners whose task finished at least this long ago")
	cmd.Flags().BoolVar(&removeWorktree, "remove-worktree", false, "also remove clean worktrees (branches are kept)")
	return cmd
}

func newListCommand() *cobra.Command {
	var all bool
	var owner string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List tasks with their stored status and herdr's live state",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := openRunner()
			if err != nil {
				return err
			}
			tasks, err := r.Store.List()
			if err != nil {
				return err
			}
			if !all && owner == "" {
				owner = currentOwner(cmd)
			}
			live, _ := r.Herdr.AgentList(cmd.Context()) // herdr down: show stored state only
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tKIND\tAGENT\tSTATUS\tLIVE\tAGE\tTITLE")
			for _, t := range tasks {
				if !all && t.Owner != owner {
					continue
				}
				state := "-"
				if a, ok := live[t.Herdr.PaneID]; ok && t.Herdr.PaneID != "" && !t.Herdr.Closed && a.Kind != "" {
					state = a.Status
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Kind, t.Agent, t.Status, state,
					age(t.CreatedAt), t.Title)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "show tasks of every main session")
	cmd.Flags().StringVar(&owner, "owner", "", "show tasks of this main session (default: git top-level of the current folder)")
	return cmd
}

func newShowCommand() *cobra.Command {
	var events int
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show a task, its recent events and its output folder",
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
			t, err := r.Store.Get(id)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			if err := enc.Encode(t); err != nil {
				return err
			}
			all, _, err := r.Store.ReadEventsSince(0)
			if err != nil {
				return err
			}
			var mine []state.Event
			for _, ev := range all {
				if ev.Task == id {
					mine = append(mine, ev)
				}
			}
			fmt.Fprintf(out, "\nevents (last %d):\n", min(events, len(mine)))
			for _, ev := range mine[max(0, len(mine)-events):] {
				line := ev.TS.Local().Format("15:04:05") + "  " + ev.Type
				if ev.Summary != "" {
					line += " · " + ev.Summary
				}
				if ev.Question != "" {
					line += " · Q: " + ev.Question
				}
				fmt.Fprintln(out, "  "+line)
			}
			dir := r.Store.TaskDir(id)
			fmt.Fprintf(out, "\noutput: %s/out\nlaunch log: %s/launch.log\n", dir, dir)
			return nil
		},
	}
	cmd.Flags().IntVar(&events, "events", 10, "how many recent events to show")
	return cmd
}

func currentOwner(cmd *cobra.Command) string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	if top := runner.Toplevel(cmd.Context(), cwd); top != "" {
		return top
	}
	return cwd
}

func age(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return d.String()
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
