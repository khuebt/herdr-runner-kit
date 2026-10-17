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
		Use:   "stop <id>...",
		Short: "Quit runners' agents, close their tabs and mark the tasks stopped",
		Long: "stop quits each runner's agent and closes its tab. A worktree task keeps its workspace " +
			"and branch unless --remove-worktree is given, and says so; a dirty worktree is never removed, " +
			"and the branch is never deleted. Untracked files a runner without a worktree may have " +
			"left are named, never deleted. One id that fails does not keep the others running.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRunner()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			var failed []string
			var errs []error
			for _, arg := range args {
				t, err := stopOne(cmd, r, arg, removeWorktree)
				if err != nil {
					failed = append(failed, arg)
					errs = append(errs, fmt.Errorf("%s: %w", arg, err))
					continue
				}
				fmt.Fprintf(out, "%s %s\n", t.ID, t.Status)
				if t.Worktree != nil && t.Worktree.Path != "" && !t.Worktree.Removed {
					fmt.Fprintf(out, "note: workspace kept for %s; hrk cleanup or stop --remove-worktree closes it\n", t.Worktree.Path)
				}
				if dir, n, list := runner.LeftFiles(cmd.Context(), t); n > 0 {
					fmt.Fprintf(out, "note: %d untracked files in %s, which the runner may have left: %s\n", n, dir, list)
				}
			}
			switch {
			case len(errs) == 1 && len(args) == 1:
				return errors.Unwrap(errs[0])
			case len(errs) > 0:
				return fmt.Errorf("could not stop %s: %w", strings.Join(failed, ", "), errors.Join(errs...))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&removeWorktree, "remove-worktree", false, "also remove a clean worktree (the branch is kept)")
	return cmd
}

// stopOne stops the task named by arg.
func stopOne(cmd *cobra.Command, r *runner.Runner, arg string, removeWorktree bool) (*task.Task, error) {
	id, err := parseTaskID(arg)
	if err != nil {
		return nil, err
	}
	return r.Stop(cmd.Context(), id, removeWorktree)
}

func newGCCommand() *cobra.Command {
	var olderThan time.Duration
	var removeWorktree, kept, all bool
	var owner string
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Close runners of finished tasks so they stop using memory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := openRunner()
			if err != nil {
				return err
			}
			// Without --owner or --all, project tasks count along with the
			// folder's, as in list.
			var owners runner.Owners
			switch {
			case owner != "":
				owners.Owner = owner
			case !all:
				owners = runner.Owners{Owner: currentOwner(cmd), Projects: true}
			}
			closed, err := r.GC(cmd.Context(), owners, olderThan, removeWorktree, kept)
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
	cmd.Flags().BoolVar(&kept, "kept", false, "also close kept runners whose task finished at least --older-than ago")
	cmd.Flags().BoolVar(&all, "all", false, "close runners of every main session")
	cmd.Flags().StringVar(&owner, "owner", "", "close runners of this main session only (default: git top-level of the current folder, plus the tasks of every project)")
	cmd.MarkFlagsMutuallyExclusive("owner", "all")
	return cmd
}

func newCleanupCommand() *cobra.Command {
	var yes, noFetch bool
	var owner string
	cmd := &cobra.Command{
		Use:   "cleanup [id]...",
		Short: "List finished worktrees and branches left behind, and remove the merged ones with --yes",
		Long: "cleanup fetches origin, then lists the finished worktree tasks of this main session and of every project whose " +
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
			// Without --owner, project tasks count along with the folder's,
			// as in list.
			owners := runner.Owners{Owner: owner}
			if owner == "" {
				owners = runner.Owners{Owner: currentOwner(cmd), Projects: true}
			}
			items, err := r.Cleanable(cmd.Context(), owners, !noFetch)
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
	cmd.Flags().StringVar(&owner, "owner", "", "main session whose tasks to clean (default: git top-level of the current folder, plus the tasks of every project)")
	return cmd
}

func newListCommand() *cobra.Command {
	var all bool
	var owner string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List tasks with their stored status, herdr's live state and their last event's age",
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
			last, err := lastEvents(r.Store)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			now := time.Now()
			fmt.Fprintln(w, "ID\tKIND\tAGENT\tSTATUS\tLIVE\tTAB\tAGE\tLAST\tTITLE")
			for _, t := range tasks {
				if !all && t.Owner != owner && (!projects || !task.IsProjectOwner(t.Owner)) {
					continue
				}
				state := "-"
				if a, ok := live[t.Herdr.PaneID]; ok && t.Herdr.PaneID != "" && !t.Herdr.Closed && a.Kind != "" {
					state = a.Status
				}
				lastAge := "-"
				if at, ok := last[t.ID]; ok {
					lastAge = age(at)
				}
				if note := compactionNote(r.Compactions(t.ID)); note != "" {
					lastAge += ", " + note
				}
				title := t.Title
				if t.CheckoutOf != "" {
					title += " (in " + t.CheckoutOf + "'s worktree)"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Kind, t.Agent, t.Status, state,
					tabState(t, now, r.Config.KeepFor), age(t.CreatedAt), lastAge, title)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "show tasks of every main session")
	cmd.Flags().StringVar(&owner, "owner", "", "show tasks of this main session only (default: git top-level of the current folder, plus the tasks of every project)")
	return cmd
}

// lastEvents returns the time of each task's latest event.
func lastEvents(store *state.Store) (map[string]time.Time, error) {
	all, _, err := store.ReadEventsSince(0)
	if err != nil {
		return nil, err
	}
	last := map[string]time.Time{}
	for _, ev := range all {
		if ev.Task != "" && ev.TS.After(last[ev.Task]) {
			last[ev.Task] = ev.TS
		}
	}
	return last, nil
}

// tabState says whether a task's tab is open, and when gc closes it.
func tabState(t *task.Task, now time.Time, keepFor time.Duration) string {
	switch {
	case t.Herdr.PaneID == "":
		return "-"
	case t.Herdr.Closed && t.Worktree != nil && !t.Worktree.Removed:
		return "closed, worktree kept"
	case t.Herdr.Closed:
		return "closed"
	}
	at, ok := runner.CloseAt(t, keepFor)
	switch {
	case t.Keep && ok:
		return "kept until " + at.Local().Format("15:04")
	case t.Keep:
		return "kept"
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
		Long: "keep holds a runner's tab open after its task finishes, for keepFor (default 4h; " +
			"0 means until keep --off or stop). keep --off lets gc close it again, at once when " +
			"its close time has passed.",
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
			_, err = fmt.Fprintln(cmd.OutOrStdout(), keepLine(t, time.Now(), r.Config.KeepFor))
			return err
		},
	}
	cmd.Flags().BoolVar(&off, "off", false, "let gc close the runner again")
	return cmd
}

// keepLine tells the user what happens to the runner's tab after keep.
func keepLine(t *task.Task, now time.Time, keepFor time.Duration) string {
	if t.Herdr.Closed {
		return t.ID + " closed now"
	}
	at, ok := runner.CloseAt(t, keepFor)
	switch {
	case t.Keep && keepFor > 0 && !ok:
		return fmt.Sprintf("%s kept open for %s after it finishes; close it sooner with stop %s", t.ID, shortDuration(keepFor), t.ID)
	case t.Keep && ok:
		return fmt.Sprintf("%s kept open until %s; close it sooner with stop %s", t.ID, at.Local().Format("15:04"), t.ID)
	case t.Keep:
		return t.ID + " kept open; close it with stop " + t.ID
	}
	switch {
	case !ok:
		return fmt.Sprintf("%s closes on its own %d minutes after it finishes", t.ID, int(runner.GCAge.Minutes()))
	case now.Before(at):
		return t.ID + " closes on its own at " + at.Local().Format("15:04")
	default:
		return t.ID + " is due to close; it closes once its agent is idle"
	}
}

// shortDuration prints d without zero trailing units, such as 4h or 1h30m.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func newShowCommand() *cobra.Command {
	var events int
	var asJSON, withPrompt bool
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show a task, its recent events and its output folder",
		Long: "show prints a compact view of a task: its state, git line, report, last events and " +
			"output folder. The prompt and inputs show with --prompt. --json prints the task as one " +
			"JSON object for scripts; with --events it prints {task, events}.",
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
			t, err := r.Store.Get(id)
			if err != nil {
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
			mine = mine[max(0, len(mine)-max(events, 0)):]
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if !cmd.Flags().Changed("events") {
					return enc.Encode(t)
				}
				return enc.Encode(struct {
					Task   *task.Task    `json:"task"`
					Events []state.Event `json:"events"`
				}{t, mine})
			}
			var anchor *task.Task
			if t.CheckoutOf != "" {
				anchor, _ = r.Store.Get(t.CheckoutOf) // unreadable: showText names the id only
			}
			_, err = fmt.Fprint(out, showText(t, anchor, mine, r.Store.TaskDir(id), r.Config.KeepFor, r.Compactions(id), withPrompt))
			return err
		},
	}
	cmd.Flags().IntVar(&events, "events", 5, "how many recent events to show")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the task as JSON only")
	cmd.Flags().BoolVar(&withPrompt, "prompt", false, "also print the prompt and the inputs from --after tasks")
	return cmd
}

// maxShowField is the most characters show prints of one field; the
// summary is printed whole.
const maxShowField = 400

// showText is the compact view of a task that show prints. anchor is the
// task whose worktree t works in (t.CheckoutOf), or nil. compactions is
// how many times its runner's sessions compacted their context.
func showText(t *task.Task, anchor *task.Task, events []state.Event, dir string, keepFor time.Duration, compactions int, withPrompt bool) string {
	var b strings.Builder
	field := func(name, value string) {
		if value != "" {
			fmt.Fprintf(&b, "%s: %s\n", name, truncate(value, maxShowField))
		}
	}
	list := func(name string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "%s:\n", name)
		for _, it := range items {
			fmt.Fprintf(&b, "  %s\n", truncate(it, maxShowField))
		}
	}
	fmt.Fprintf(&b, "%s · %s · %s · %s\n", t.ID, t.Status, t.Kind, t.Agent)
	field("title", t.Title)
	if t.Project != "" {
		project := t.Project
		if t.Stage != "" {
			project += ", stage " + t.Stage
		}
		field("project", project)
	}
	when := "created " + t.CreatedAt.Local().Format("2006-01-02 15:04")
	if !t.FinishedAt.IsZero() {
		when += " · finished " + t.FinishedAt.Local().Format("2006-01-02 15:04")
	}
	if tab := tabState(t, time.Now(), keepFor); tab != "-" {
		when += " · tab " + tab
	}
	field("time", when)
	field("context", contextNote(t, compactions))
	if t.Usage != nil {
		field("usage", t.Usage.String())
	}
	worktree := func(w *task.Worktree) string {
		where := w.Path + " on " + w.Branch
		if w.Removed {
			where += " (removed)"
		}
		return where
	}
	switch {
	case t.Worktree != nil && t.Worktree.Path != "":
		field("worktree", worktree(t.Worktree))
	case t.CheckoutOf != "" && anchor != nil && anchor.Worktree != nil && anchor.Worktree.Path != "":
		field("worktree", worktree(anchor.Worktree)+" (of "+t.CheckoutOf+")")
	case t.CheckoutOf != "":
		field("worktree", "shared with "+t.CheckoutOf)
		field("cwd", t.Cwd)
	default:
		field("cwd", t.Cwd)
	}
	if t.Git != nil {
		field("git", t.Git.Short()+" (checked "+t.Git.CheckedAt.Local().Format("15:04")+")")
	}
	field("error", t.Error)
	if t.Summary != "" {
		fmt.Fprintf(&b, "summary: %s\n", t.Summary)
	}
	list("decisions", t.Decisions)
	if t.Question != "" {
		list("question", strings.Split(t.Question, "\n"))
	}
	list("outputs", t.Outputs)
	if p := t.PendingDone; p != nil && !t.Status.Terminal() {
		fmt.Fprintln(&b, "done reported, waiting for the turn to end")
		field("pending summary", p.Summary)
	}
	if withPrompt {
		fmt.Fprintf(&b, "prompt:\n%s\n", t.Prompt)
		if t.Inputs != "" {
			fmt.Fprintf(&b, "inputs:\n%s\n", t.Inputs)
		}
	}
	if len(events) > 0 {
		fmt.Fprintf(&b, "events (last %d):\n", len(events))
		for _, ev := range events {
			line := ev.TS.Local().Format("15:04:05") + "  " + ev.Type
			if ev.Summary != "" {
				line += " · " + ev.Summary
			}
			if ev.Question != "" {
				line += " · Q: " + ev.Question
			}
			fmt.Fprintf(&b, "  %s\n", truncate(strings.Join(strings.Fields(line), " "), maxShowField))
		}
	}
	fmt.Fprintf(&b, "output: %s/out\nlaunch log: %s/launch.log\n", dir, dir)
	return b.String()
}

// contextNote says how a runner's context fared: its attempt past the
// first, and its compactions. It is empty for a runner that has neither.
func contextNote(t *task.Task, compactions int) string {
	var parts []string
	if n := task.AttemptOf(t); n > 1 {
		parts = append(parts, fmt.Sprintf("attempt %d (handed off from %s)", n, strings.Join(t.Handoffs, ", ")))
	}
	if note := compactionNote(compactions); note != "" {
		parts = append(parts, note)
	}
	return strings.Join(parts, " · ")
}

// compactionNote counts a runner's compactions, such as "3 compactions",
// and is empty for none.
func compactionNote(n int) string {
	switch {
	case n == 1:
		return "1 compaction"
	case n > 1:
		return fmt.Sprintf("%d compactions", n)
	}
	return ""
}

// truncate cuts s to at most n characters, marking a cut with an ellipsis.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
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
