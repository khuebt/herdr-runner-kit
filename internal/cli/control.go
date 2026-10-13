package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// maxPromptBytes bounds --stdin text; a longer prompt is an error, never cut.
// The prompt reaches the agent as one argument of herdr agent prompt, and
// Linux caps one argument at 128 KiB, which must also fit the kit's wrapper.
const maxPromptBytes = 100 << 10

// promptText is the text a command sends: args joined, or stdin with --stdin,
// so user text passes through a quoted heredoc instead of a shell string.
func promptText(cmd *cobra.Command, args []string, stdin bool) (string, error) {
	if !stdin {
		return strings.Join(args, " "), nil
	}
	if len(args) > 0 {
		return "", errors.New("give the text either as arguments or on stdin, not both")
	}
	data, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), maxPromptBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxPromptBytes {
		return "", fmt.Errorf("the text on stdin is over %d KiB; put it in a file and name the file instead", maxPromptBytes>>10)
	}
	return strings.TrimSpace(string(data)), nil
}

func parseTaskID(arg string) (string, error) {
	id, _, err := task.ParseID(arg)
	return id, err
}

func newSendCommand() *cobra.Command {
	var stdin bool
	cmd := &cobra.Command{
		Use:   "send <id> <text> | send <id> --stdin",
		Short: "Type a new prompt into a running runner",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseTaskID(args[0])
			if err != nil {
				return err
			}
			text, err := promptText(cmd, args[1:], stdin)
			if err != nil {
				return err
			}
			if text == "" {
				return errors.New("send needs the text to type")
			}
			r, err := openRunner()
			if err != nil {
				return err
			}
			working, err := r.Send(cmd.Context(), id, text)
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
	cmd.Flags().BoolVar(&stdin, "stdin", false, "read the text from stdin")
	return cmd
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
			closed, err := r.GC(cmd.Context(), "", olderThan, removeWorktree)
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

func newCleanupCommand() *cobra.Command {
	var yes, noFetch bool
	var owner string
	cmd := &cobra.Command{
		Use:   "cleanup [id]...",
		Short: "List finished worktrees and branches left behind, and remove the merged ones with --yes",
		Long: "cleanup fetches origin, then lists this main session's finished worktree tasks whose " +
			"worktree or branch is still there: merged (safe to remove), unmerged, dirty or kept. With --yes " +
			"it removes the worktree and deletes the local branch of each merged one, or of the given " +
			"ids. Unmerged branches and worktrees with changes are never removed.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ids := make([]string, 0, len(args))
			for _, a := range args {
				id, err := parseTaskID(a)
				if err != nil {
					return err
				}
				ids = append(ids, id)
			}
			r, err := openRunner()
			if err != nil {
				return err
			}
			if owner == "" {
				owner = currentOwner(cmd)
			}
			items, err := r.Cleanable(cmd.Context(), owner, !noFetch)
			if err != nil {
				return err
			}
			if len(ids) > 0 {
				items = slices.DeleteFunc(items, func(it runner.CleanupItem) bool { return !slices.Contains(ids, it.Task.ID) })
			}
			out := cmd.OutOrStdout()
			if len(items) == 0 {
				_, err := fmt.Fprintln(out, "nothing to clean up")
				return err
			}
			merged := 0
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			for _, it := range items {
				if it.Verdict == runner.Merged {
					merged++
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", it.Task.ID, it.Verdict, it.Task.Worktree.Branch, it.Detail)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if !yes {
				if merged > 0 {
					_, err = fmt.Fprintf(out, "run cleanup --yes to remove the worktree and branch of %d merged task(s)\n", merged)
				}
				return err
			}
			cleaned, err := r.Cleanup(cmd.Context(), items)
			if len(cleaned) > 0 {
				fmt.Fprintf(out, "removed the worktree and branch of %s\n", strings.Join(cleaned, ", "))
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "remove the worktree and branch of each merged task")
	cmd.Flags().BoolVar(&noFetch, "no-fetch", false, "compare with the local refs, without fetching origin")
	cmd.Flags().StringVar(&owner, "owner", "", "main session whose tasks to clean (default: git top-level of the current folder)")
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
			// Without --owner, project tasks show along with the folder's,
			// as wait watches them.
			projects := owner == ""
			if !all && owner == "" {
				owner = currentOwner(cmd)
			}
			live, _ := r.Herdr.AgentList(cmd.Context()) // herdr down: show stored state only
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			now := time.Now()
			fmt.Fprintln(w, "ID\tKIND\tAGENT\tSTATUS\tLIVE\tTAB\tAGE\tTITLE")
			for _, t := range tasks {
				if !all && t.Owner != owner && (!projects || !task.IsProjectOwner(t.Owner)) {
					continue
				}
				state := "-"
				if a, ok := live[t.Herdr.PaneID]; ok && t.Herdr.PaneID != "" && !t.Herdr.Closed && a.Kind != "" {
					state = a.Status
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Kind, t.Agent, t.Status, state,
					tabState(t, now), age(t.CreatedAt), t.Title)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "show tasks of every main session")
	cmd.Flags().StringVar(&owner, "owner", "", "show tasks of this main session only (default: git top-level of the current folder, plus the tasks of every project)")
	return cmd
}

// tabState says whether a task's tab is open, and when gc closes it.
func tabState(t *task.Task, now time.Time) string {
	switch {
	case t.Herdr.PaneID == "":
		return "-"
	case t.Herdr.Closed:
		return "closed"
	case t.Keep:
		return "kept"
	}
	at, ok := runner.CloseAt(t)
	switch {
	case !ok:
		return "open"
	case now.Before(at):
		return "open, closes " + at.Local().Format("15:04")
	default:
		return "open, due"
	}
}

func newKeepCommand() *cobra.Command {
	var off bool
	cmd := &cobra.Command{
		Use:   "keep <id>",
		Short: "Keep a finished runner's tab open instead of letting gc close it",
		Long: "keep holds a runner's tab open after its task finishes, until keep --off or stop. " +
			"keep --off lets gc close it again, at once when its close time has passed.",
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
			t, err := r.Keep(cmd.Context(), id, !off)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), keepLine(t, time.Now()))
			return err
		},
	}
	cmd.Flags().BoolVar(&off, "off", false, "let gc close the runner again")
	return cmd
}

// keepLine tells the user what happens to the runner's tab after keep.
func keepLine(t *task.Task, now time.Time) string {
	if t.Herdr.Closed {
		return t.ID + " closed now"
	}
	if t.Keep {
		return t.ID + " kept open; close it with stop " + t.ID
	}
	at, ok := runner.CloseAt(t)
	switch {
	case !ok:
		return fmt.Sprintf("%s closes on its own %d minutes after it finishes", t.ID, int(runner.GCAge.Minutes()))
	case now.Before(at):
		return t.ID + " closes on its own at " + at.Local().Format("15:04")
	default:
		return t.ID + " is due to close; it closes once its agent is idle"
	}
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
			if t.Git != nil {
				fmt.Fprintf(out, "\ngit: %s (checked %s)\n", t.Git.Short(), t.Git.CheckedAt.Local().Format("15:04"))
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
	return runner.CallerOwner(cmd.Context(), cwd)
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
