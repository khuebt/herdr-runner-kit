package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/khuebt/herdr-runner-kit/internal/herdr"
	"github.com/khuebt/herdr-runner-kit/internal/project"
	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func newProjectCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Register and manage the projects the kit drives",
		Long: "A project is a git repository the kit keeps a record for. Its runners " +
			"report to the owner project:<name>, so their news reaches the main session " +
			"from any folder.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newProjectStatusCommand(), newProjectRegisterCommand(), newProjectForgetCommand(),
		newProjectRunCommand(), newProjectShareCommand())
	return cmd
}

func newNewCommand() *cobra.Command {
	var o project.CreateOptions
	var stdin bool
	cmd := &cobra.Command{
		Use:   "new <path> (--brief FILE | --stdin)",
		Short: "Start a project: a git repository with its record, first commit and herdr workspace",
		Long: "new creates the folder and a git repository in it, keeps the original brief " +
			"(or the idea read from stdin) git-ignored under " + project.BriefDir + ", writes " +
			"the project record and makes one first commit of exactly the files it wrote. It " +
			"adds origin when --remote is given (it never pushes), opens a herdr workspace " +
			"and registers the project. It refuses /, your home folder, folders inside a git " +
			"repository or holding a project, non-empty folders without --allow-non-empty and " +
			"URLs that hold credentials. Nothing is deleted when a step fails.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := project.RefuseInsideRunner(os.Getenv); err != nil {
				return err
			}
			if stdin == (o.BriefFile != "") {
				return errors.New("give the brief with either --brief FILE or --stdin")
			}
			if stdin {
				idea, err := promptText(cmd, nil, true)
				if err != nil {
					return err
				}
				if idea == "" {
					return errors.New("the idea on stdin is empty")
				}
				o.Idea = idea
			}
			o.Path = args[0]
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("find your home folder: %w", err)
			}
			o.Home = home
			store, err := openStore()
			if err != nil {
				return err
			}
			c, err := project.Create(cmd.Context(), store, herdr.New(), o)
			if err != nil {
				return err
			}
			printCreated(cmd, c)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Name, "name", "", "project name (default: the folder name)")
	f.StringVar(&o.Remote, "remote", "", "URL added as origin; never pushed, and refused when it holds credentials")
	f.StringVar(&o.BriefFile, "brief", "", "file holding the brief, kept git-ignored in the project")
	f.BoolVar(&stdin, "stdin", false, "read the idea or brief from stdin")
	f.StringVar(&o.Reference, "reference", "", "path or URL of a repository to learn from")
	f.StringVar(&o.InitialBranch, "initial-branch", "", "name of the first branch (default: git's init.defaultBranch, else main)")
	f.BoolVar(&o.AllowNonEmpty, "allow-non-empty", false, "start the project in a folder that already holds files")
	return cmd
}

func printCreated(cmd *cobra.Command, c project.Created) {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "created %s at %s (commit %s, branch %s)\n", c.Name, c.Path, c.Commit[:min(12, len(c.Commit))], c.Branch)
	switch {
	case c.RemoteErr != "":
		fmt.Fprintf(w, "remote: not added: %s\n", c.RemoteErr)
	case c.Remote != "":
		fmt.Fprintf(w, "remote: origin %s\n", c.Remote)
	default:
		fmt.Fprintln(w, "remote: none")
	}
	if c.WorkspaceErr != "" {
		fmt.Fprintf(w, "workspace: not created: %s\n", c.WorkspaceErr)
	} else {
		fmt.Fprintf(w, "workspace: %s\n", c.Workspace)
	}
	if c.RegistryErr != "" {
		fmt.Fprintf(w, "registry: not registered: %s\n", c.RegistryErr)
	}
	fmt.Fprintf(w, "brief: original kept untracked at %s\n", c.Brief)
	if c.GitignoreNote != "" {
		fmt.Fprintf(w, "gitignore: %s\n", c.GitignoreNote)
	}
	fmt.Fprintf(w, "next: herdr-runner-kit project status %s\n", c.Name)
}

func newProjectStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status [name]",
		Short: "Show where a project stands and the one step to take next",
		Long: "status reads the kit's record of the project and its tasks, never herdr, so " +
			"it works with herdr down; it changes nothing. Without a name it shows the " +
			"project holding the current folder, else the only registered project, else " +
			"one line per registered project.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openStore()
			if err != nil {
				return err
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("find the current folder: %w", err)
			}
			p, err := resolveProject(store, name, cwd)
			if errors.Is(err, errNoProjectResolved) {
				return printProjectList(cmd.OutOrStdout(), store)
			}
			if err != nil {
				return err
			}
			r, err := projectReport(store, p)
			if err != nil {
				return err
			}
			if _, err := os.Stat(p.Path); errors.Is(err, os.ErrNotExist) {
				r.PathMissing = true
			} else {
				r.Remote = project.Origin(cmd.Context(), p.Path)
			}
			_, err = io.WriteString(cmd.OutOrStdout(), r.Text())
			return err
		},
	}
}

// errNoProjectResolved is returned by resolveProject when no name was given
// and neither the current folder nor a single registration picks one.
var errNoProjectResolved = errors.New("no project picked")

// resolveProject picks the project a command works on: the one named, else
// the registered project holding cwd, else the only registered project.
// Projects still being set up are never picked without their name.
func resolveProject(s *state.Store, name, cwd string) (state.Project, error) {
	if name != "" {
		return s.ProjectByName(name)
	}
	if p, ok, err := s.ProjectFor(cwd); err != nil || ok {
		return p, err
	}
	list, err := s.Projects()
	if err != nil {
		return state.Project{}, err
	}
	var names []string
	var only state.Project
	for _, p := range list {
		if !p.Reserved {
			names = append(names, p.Name)
			only = p
		}
	}
	switch len(names) {
	case 0:
		return state.Project{}, fmt.Errorf("%w: no project is registered; start one with herdr-runner-kit new, or add one with herdr-runner-kit project register", errNoProjectResolved)
	case 1:
		return only, nil
	}
	return state.Project{}, fmt.Errorf("%w: name one of %s", errNoProjectResolved, strings.Join(names, ", "))
}

// projectReport builds the status of p from the kit's record and tasks. A
// missing record is part of the report, not an error.
func projectReport(s *state.Store, p state.Project) (project.Report, error) {
	var rec *project.Record
	loaded, err := project.Load(s, p.Name)
	switch {
	case errors.Is(err, project.ErrNoRecord):
	case err != nil:
		return project.Report{}, err
	default:
		rec = &loaded
	}
	tasks, err := s.List()
	if err != nil {
		return project.Report{}, err
	}
	return project.Status(rec, p, tasks, time.Now()), nil
}

// printProjectList prints one line per registered project.
func printProjectList(out io.Writer, s *state.Store) error {
	list, err := s.Projects()
	if err != nil {
		return err
	}
	if len(list) == 0 {
		_, err := fmt.Fprintln(out, "no project is registered; start one with herdr-runner-kit new, or add one with herdr-runner-kit project register")
		return err
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tPATH\tSTAGE\tMILESTONE\tACTIVE")
	var reserved []string
	for _, p := range list {
		if p.Reserved {
			fmt.Fprintf(w, "%s\t%s\t(being set up)\t-\t-\n", p.Name, p.Path)
			reserved = append(reserved, p.Name)
			continue
		}
		r, err := projectReport(s, p)
		if err != nil {
			return err
		}
		stage := r.Stage
		if r.Record == nil {
			stage = "(no record)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n", p.Name, p.Path, stage, r.Milestone(), len(r.Runners)+len(r.Waiting))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, name := range reserved {
		if _, err := fmt.Fprintf(out, "%s is being set up; if herdr-runner-kit new was interrupted, drop it with: herdr-runner-kit project forget %s\n", name, name); err != nil {
			return err
		}
	}
	return nil
}

func newProjectRegisterCommand() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "register <path>",
		Short: "Register an existing git repository as a project",
		Long: "register adds the repository to the kit's registry. It imports the record " +
			"from a committed handbook/product/project.md when there is one, else it starts " +
			"a fresh record. Nothing is written to the repository.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := project.RefuseInsideRunner(os.Getenv); err != nil {
				return err
			}
			store, err := openStore()
			if err != nil {
				return err
			}
			p, rec, err := project.Register(cmd.Context(), store, args[0], name)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "registered %s at %s (%s, %s track); its runners report to %s\n",
				p.Name, p.Path, rec.Type, rec.Track, task.ProjectOwner(p.Name))
			return err
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "project name (default: the committed record's name, else the folder name)")
	return cmd
}

func newProjectForgetCommand() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "forget <name>",
		Short: "Remove a project from the registry, leaving its repository alone",
		Long: "forget removes the registry entry and the kit's record of the project. It " +
			"refuses while the project has unfinished tasks or changes not yet committed " +
			"to the repository, unless --yes is given.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := project.RefuseInsideRunner(os.Getenv); err != nil {
				return err
			}
			store, err := openStore()
			if err != nil {
				return err
			}
			if err := project.Forget(store, args[0], yes); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "forgot %s; its repository is unchanged\n", args[0])
			return err
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "forget even with unfinished tasks or uncommitted changes")
	return cmd
}

func newProjectRunCommand() *cobra.Command {
	var o stageRun
	var stdin bool
	cmd := &cobra.Command{
		Use:   "run <stage> [--stdin]",
		Short: "Start a project stage runner from the kit's stage template",
		Long: "run renders the stage's template with the project's record and the agent's skill " +
			"syntax, adds your notes from stdin as untrusted data, and spawns the runner in the " +
			"project. It refuses a stage whose gate is not approved yet. A stage that works in " +
			"the main checkout waits for the project's other main-checkout runners, so only one " +
			"of them commits there at a time. Stages: " + strings.Join(project.StageNames(), ", ") + ".",
		Example: `  herdr-runner-kit project run intake
  echo "Focus on the admin flows first" | herdr-runner-kit project run ux --stdin --after t4`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := project.RefuseInsideRunner(os.Getenv); err != nil {
				return err
			}
			var err error
			if stdin {
				if o.Notes, err = promptText(cmd, nil, true); err != nil {
					return err
				}
			}
			o.Stage = args[0]
			if o.Cwd, err = os.Getwd(); err != nil {
				return fmt.Errorf("find the current folder: %w", err)
			}
			r, err := openRunner()
			if err != nil {
				return err
			}
			dirs, err := claudeConfigs(nil)
			if err != nil {
				return err
			}
			o.Tools, _ = newToolProbe(dirs).detect(cmd.Context())
			o.Warn = func(line string) { fmt.Fprintln(cmd.ErrOrStderr(), line) }
			t, err := runStage(cmd.Context(), r, o)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), spawnLine(t))
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Project, "project", "", "project to run the stage for (default: the project holding the current folder, else the only one)")
	f.BoolVar(&stdin, "stdin", false, "read notes for the runner from stdin; they reach it as untrusted data")
	f.StringSliceVar(&o.After, "after", nil, "start only when these tasks are done")
	f.StringVar(&o.Agent, "agent", "", "agent spec harness[:model[:effort]], overriding the stage kind's default")
	f.StringVar(&o.ClaudeConfig, "claude-config", "", "Claude config folder of a Claude runner, as a path or a name such as claude2")
	f.StringVar(&o.Language, "language", "", "language of the runner's report summaries and questions")
	return cmd
}

// stageRun is the input of one project stage run.
type stageRun struct {
	Project      string
	Stage        string
	Notes        string
	After        []string
	Agent        string
	ClaudeConfig string
	Language     string
	// Cwd picks the project when Project is empty.
	Cwd   string
	Tools project.Tools
	Warn  func(string)
}

// runStage checks the stage's gates, renders its template for the agent
// that will run it and spawns the runner in the project.
func runStage(ctx context.Context, r *runner.Runner, o stageRun) (*task.Task, error) {
	stage, err := project.LoadStage(o.Stage)
	if err != nil {
		return nil, err
	}
	p, err := resolveProject(r.Store, o.Project, o.Cwd)
	if err != nil {
		return nil, err
	}
	if p.Reserved {
		return nil, fmt.Errorf("project %s is still being set up", p.Name)
	}
	rec, err := project.Load(r.Store, p.Name)
	if err != nil {
		return nil, err
	}
	if err := project.CheckRequires(rec, stage); err != nil {
		return nil, err
	}
	kind, found := r.Config.Kinds[stage.Kind]
	if !found {
		return nil, fmt.Errorf("stage %s runs as kind %q, which the config does not define", stage.Name, stage.Kind)
	}
	agent := kind.Agent
	if o.Agent != "" {
		agent = o.Agent
	}
	spec, err := task.ParseAgentSpec(agent)
	if err != nil {
		return nil, err
	}
	prompt, err := stage.Render(spec.Harness, project.NewStageData(rec, p.Path, o.Tools, o.Notes, time.Now()))
	if err != nil {
		return nil, err
	}
	after := make([]string, 0, len(o.After))
	for _, a := range o.After {
		id, _, err := task.ParseID(a)
		if err != nil {
			return nil, err
		}
		after = append(after, id)
	}
	if !kind.Worktree {
		busy, err := mainCheckoutTasks(r.Store, p.Name)
		if err != nil {
			return nil, err
		}
		for _, id := range busy {
			if !slices.Contains(after, id) {
				after = append(after, id)
				if o.Warn != nil {
					o.Warn(fmt.Sprintf("note: %s waits for %s, which works in the main checkout of %s too", stage.Name, id, p.Name))
				}
			}
		}
	}
	return r.Spawn(ctx, runner.SpawnOptions{
		Kind: stage.Kind, Agent: o.Agent, After: after, Title: p.Name + " " + stage.Name + ": " + stage.Title,
		Prompt: prompt, ClaudeConfig: o.ClaudeConfig, Language: o.Language,
		Project: p.Name, Stage: stage.Name, Keep: stage.Keep, Warn: o.Warn,
	})
}

// mainCheckoutTasks returns the unfinished tasks of a project that work in
// its main checkout rather than in a worktree of their own.
func mainCheckoutTasks(s *state.Store, name string) ([]string, error) {
	tasks, err := s.List()
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, t := range tasks {
		if t.Project == name && !t.UseWorktree && !t.Status.Terminal() {
			ids = append(ids, t.ID)
		}
	}
	return ids, nil
}

// shareTargets maps project share's argument to the record field it sets.
var shareTargets = map[string]func(*project.Share) *bool{
	"stitch":        func(s *project.Share) *bool { return &s.Stitch },
	"lavish-hosted": func(s *project.Share) *bool { return &s.LavishHosted },
}

func newProjectShareCommand() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "share <stitch|lavish-hosted> <on|off>",
		Short: "Allow or forbid a project's runners to share work outside this machine",
		Long: "share records your choice in the project's record and decision log. Runners " +
			"never share outside the machine unless you switched that share on here: " +
			"environment variables, notes and files cannot switch it on. The record is " +
			"committed with the next gate outcome.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := project.RefuseInsideRunner(os.Getenv); err != nil {
				return err
			}
			field, known := shareTargets[args[0]]
			if !known {
				return fmt.Errorf("unknown share %q: want stitch or lavish-hosted", args[0])
			}
			if args[1] != "on" && args[1] != "off" {
				return fmt.Errorf("want on or off, not %q", args[1])
			}
			on := args[1] == "on"
			store, err := openStore()
			if err != nil {
				return err
			}
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("find the current folder: %w", err)
			}
			p, err := resolveProject(store, name, cwd)
			if err != nil {
				return err
			}
			changed := false
			if _, err := project.Update(store, p.Name, func(rec *project.Record) error {
				target := field(&rec.Share)
				if *target == on {
					return errUnchanged
				}
				*target, changed = on, true
				rec.Decisions = append(rec.Decisions, project.Decision{
					At: time.Now().UTC(), Text: "share " + args[0] + " switched " + args[1] + " by the user",
				})
				return nil
			}); err != nil && !errors.Is(err, errUnchanged) {
				return err
			}
			if !changed {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s sharing of %s is already %s\n", args[0], p.Name, args[1])
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s sharing of %s is now %s; runners launched from now on follow it\n", args[0], p.Name, args[1])
			return err
		},
	}
	cmd.Flags().StringVar(&name, "project", "", "project to change (default: the project holding the current folder, else the only one)")
	return cmd
}

// errUnchanged stops a record update that would change nothing.
var errUnchanged = errors.New("unchanged")

// openStore opens the state directory without loading the config.
func openStore() (*state.Store, error) {
	dir, err := stateDir()
	if err != nil {
		return nil, err
	}
	return state.Open(dir)
}
