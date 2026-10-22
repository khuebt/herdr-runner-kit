package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/khuebt/herdr-runner-kit/internal/harness"
	"github.com/khuebt/herdr-runner-kit/internal/herdr"
	"github.com/khuebt/herdr-runner-kit/internal/project"
	"github.com/khuebt/herdr-runner-kit/internal/redact"
	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
	"github.com/khuebt/herdr-runner-kit/internal/wait"
)

func newProjectCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Register and manage the projects the kit drives",
		Long: "A project is a git repository the kit keeps a record for. Its runners " +
			"report to the owner project:<name>. Their news reaches a main session working " +
			"inside the project's repository, and one started with hrk main --project <name|path>.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newProjectStatusCommand(), newProjectRegisterCommand(), newProjectForgetCommand(),
		newProjectRunCommand(), newProjectShareCommand(),
		newProjectGateCommand("approve"), newProjectGateCommand("changes"), newProjectGateCommand("reject"),
		newProjectDecideCommand(), newProjectCRCommand(), newProjectBacklogCommand(), newProjectDepCommand(),
		newProjectMilestoneCommand(), newProjectGraduateCommand())
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
			return hintAttach(cmd, store, c.Name)
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
	fmt.Fprintf(w, "next: hrk project status %s\n", c.Name)
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
				return printProjectList(cmd.Context(), cmd.OutOrStdout(), store)
			}
			if err != nil {
				return err
			}
			r, err := projectReport(cmd.Context(), store, p)
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
		return state.Project{}, fmt.Errorf("%w: no project is registered; start one with hrk new, or add one with hrk project register", errNoProjectResolved)
	case 1:
		return only, nil
	}
	return state.Project{}, fmt.Errorf("%w: name one of %s", errNoProjectResolved, strings.Join(names, ", "))
}

// hintAttach tells how to get the news of project name in the main session
// of the current folder, unless that session already gets it.
func hintAttach(cmd *cobra.Command, s *state.Store, name string) error {
	if slices.Contains(wait.AttachedProjects(s, currentOwner(cmd)), name) {
		return nil
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "to get its news in this session, run hrk main --project %s\n", name)
	return err
}

// resolveProjects returns the names of the projects the --project values
// pick, each once: a value is a registered project's name, or a path inside
// its repository. An unknown value is refused with the registered projects.
func resolveProjects(s *state.Store, values []string) ([]string, error) {
	var names []string
	for _, v := range values {
		p, err := s.ProjectByName(v)
		if errors.Is(err, state.ErrUnknownProject) {
			var ok bool
			if p, ok, err = s.ProjectFor(v); err == nil && !ok {
				return nil, unknownProject(s, v)
			}
		}
		if err != nil {
			return nil, err
		}
		if !slices.Contains(names, p.Name) {
			names = append(names, p.Name)
		}
	}
	return names, nil
}

// unknownProject refuses v and lists the registered projects.
func unknownProject(s *state.Store, v string) error {
	list, err := s.Projects()
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return fmt.Errorf("unknown project %q: no project is registered; start one with hrk new, or add one with hrk project register", v)
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tPATH")
	for _, p := range list {
		fmt.Fprintf(w, "%s\t%s\n", p.Name, p.Path)
	}
	_ = w.Flush()
	return fmt.Errorf("unknown project %q: name a registered project, or a path inside its repository:\n%s", v, strings.TrimRight(b.String(), "\n"))
}

// projectReport builds the status of p from the kit's record and tasks. A
// missing record is part of the report, not an error. Once the running
// milestone's plan runner is done, it reads the committed plan for the
// questions it leaves unanswered.
func projectReport(ctx context.Context, s *state.Store, p state.Project) (project.Report, error) {
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
	r := project.Status(rec, p, tasks, time.Now())
	if rec != nil {
		r.Drift = project.GateDrift(ctx, p.Path, *rec)
	}
	if r.Running != nil && r.PlanDone {
		b, err := project.ReadPlan(ctx, p.Path, *rec, r.Running.ID)
		if err != nil {
			r.PlanErr = err.Error()
		} else {
			r.Unanswered = project.Unanswered(b)
		}
	}
	return r, nil
}

// printProjectList prints one line per registered project.
func printProjectList(ctx context.Context, out io.Writer, s *state.Store) error {
	list, err := s.Projects()
	if err != nil {
		return err
	}
	if len(list) == 0 {
		_, err := fmt.Fprintln(out, "no project is registered; start one with hrk new, or add one with hrk project register")
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
		r, err := projectReport(ctx, s, p)
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
		if _, err := fmt.Fprintf(out, "%s is being set up; if hrk new was interrupted, drop it with: hrk project forget %s\n", name, name); err != nil {
			return err
		}
	}
	return nil
}

func newProjectRegisterCommand() *cobra.Command {
	var name, reference string
	cmd := &cobra.Command{
		Use:   "register <path>",
		Short: "Register an existing git repository as a project",
		Long: "register adds the repository to the kit's registry. It imports the record " +
			"from a committed handbook/product/project.md when there is one, else it starts " +
			"a fresh record. A committed record names its reference repository without a local " +
			"path; pass --reference PATH to say where it is on this machine. Nothing is " +
			"written to the repository.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := project.RefuseInsideRunner(os.Getenv); err != nil {
				return err
			}
			store, err := openStore()
			if err != nil {
				return err
			}
			p, rec, err := project.Register(cmd.Context(), store, args[0], name, reference)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "registered %s at %s (%s, %s track); its runners report to %s\n",
				p.Name, p.Path, rec.Type, rec.Track, task.ProjectOwner(p.Name))
			if err == nil {
				err = hintAttach(cmd, store, p.Name)
			}
			if err == nil && project.ReferenceWithoutPath(rec) {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "note: the reference repository %s has no path on this machine; runners see its name only. to give one, run project forget, then register again with --reference PATH\n",
					project.RedactURL(rec.ReferenceRepo))
			}
			return err
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "project name (default: the committed record's name, else the folder name)")
	cmd.Flags().StringVar(&reference, "reference", "", "path or URL of the reference repository on this machine (default: the committed record's)")
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
		Example: `  hrk project run intake
  echo "Focus on the admin flows first" | hrk project run ux --stdin --after t4
  echo "Add a CSV export for admins" | hrk project run change --stdin`,
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
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), spawnLine(t)); err != nil {
				return err
			}
			parallelNote(r.Store, t.Project, o.Warn)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Project, "project", "", "project to run the stage for (default: the project holding the current folder, else the only one)")
	f.BoolVar(&stdin, "stdin", false, "read notes for the runner from stdin; they reach it as untrusted data")
	f.StringSliceVar(&o.After, "after", nil, "start only when these tasks are done")
	f.StringVar(&o.Agent, "agent", "", "agent spec harness[:model[:effort]], overriding the stage kind's default")
	f.StringVar(&o.ClaudeConfig, "claude-config", "", "Claude config folder of a Claude runner, as a path or a name such as claude2")
	f.StringVar(&o.Language, "language", "", "language of the runner's report summaries and questions")
	f.BoolVar(&o.Docx, "docx", false, "pack stage only: export the pack as DOCX too, with LibreOffice")
	f.StringVar(&o.Gate, "gate", "", "auto starts on its own; human waits for `release` (default: human for the pack stage given --after, else auto)")
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
	// Docx asks the pack stage for a DOCX export as well.
	Docx bool
	// Gate is auto or human; empty means human for a pack stage given
	// After, so the earlier stages are reviewed first, else auto.
	Gate string
	// TaskStage is the stage recorded on the task, such as "M2/F1";
	// empty means the template's name.
	TaskStage string
	// Branch and Base name the worktree branch and its base; empty means
	// the spawn defaults.
	Branch, Base string
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
	if stage.Milestone {
		return nil, fmt.Errorf("stage %s belongs to a milestone: start it with hrk project milestone start|chain <M>", stage.Name)
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
	if err := project.CheckRequires(ctx, p.Path, rec, stage); err != nil {
		return nil, err
	}
	if o.Docx && stage.Name != project.GatePack {
		return nil, errors.New("--docx goes only with the pack stage")
	}
	h, err := stageHarness(r, stage, o.Agent)
	if err != nil {
		return nil, err
	}
	data := project.NewStageData(rec, p.Path, o.Tools, o.Notes, time.Now())
	data.Docx = o.Docx
	if stage.Name == project.GraduateStage {
		if err := project.GraduateData(ctx, &data, p.Path, rec); err != nil {
			return nil, err
		}
	}
	title := p.Name + " " + stage.Name + ": " + stage.Title
	var change project.Change
	if stage.Name == changeStage {
		if data.Notes == "" {
			return nil, errors.New("describe the change on stdin with --stdin")
		}
		tasks, err := r.Store.List()
		if err != nil {
			return nil, err
		}
		// The title and the file name are committed, so neither may carry
		// a secret from the request.
		cr := redact.Secrets(runner.TitleFrom(data.Notes))
		if change, err = project.AddChange(r.Store, p.Name, cr, runner.Slug(cr, 40), time.Now()); err != nil {
			return nil, err
		}
		data.Change, data.ChangePath, data.Tasks = change.ID, change.Path, project.ActiveTasks(tasks, p.Name)
		title = p.Name + " " + change.ID + ": " + change.Title
	}
	t, err := spawnStage(ctx, r, o, p, stage, h, data, title)
	if change.ID == "" {
		return t, err
	}
	if err != nil {
		if derr := project.DropChange(r.Store, p.Name, change.ID); derr != nil {
			return nil, fmt.Errorf("%w; %s stays open without a runner: %v", err, change.ID, derr)
		}
		return nil, err
	}
	if err := project.SetChangeTask(r.Store, p.Name, change.ID, t.ID); err != nil {
		return t, fmt.Errorf("%s started for %s, but the record does not name it: %w", t.ID, change.ID, err)
	}
	return t, nil
}

// stageHarness returns the harness of the agent that runs stage: the agent
// override when given, else its kind's agent.
func stageHarness(r *runner.Runner, stage project.Stage, agent string) (task.Harness, error) {
	kind, found := r.Config.Kinds[stage.Kind]
	if !found {
		return "", fmt.Errorf("stage %s runs as kind %q, which the config does not define", stage.Name, stage.Kind)
	}
	if agent == "" {
		agent = kind.Agent
	}
	spec, err := task.ParseAgentSpec(agent)
	if err != nil {
		return "", err
	}
	return spec.Harness, nil
}

// changeStage is the stage that assesses a change request.
const changeStage = "change"

// spawnStage renders a stage's prompt and spawns its runner in project p.
func spawnStage(ctx context.Context, r *runner.Runner, o stageRun, p state.Project, stage project.Stage,
	h task.Harness, data project.StageData, title string,
) (*task.Task, error) {
	kind := r.Config.Kinds[stage.Kind]
	if stage.Name == project.GatePack {
		rec, err := project.Load(r.Store, p.Name)
		if err != nil {
			return nil, err
		}
		n, err := project.CommittedPackVersion(ctx, p.Path, rec)
		if err != nil {
			return nil, err
		}
		data.PackVersion = n + 1
	}
	prompt, err := stage.Render(h, data)
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
	taskStage := o.TaskStage
	if taskStage == "" {
		taskStage = stage.Name
	}
	gate, reason := task.Gate(o.Gate), ""
	if gate == "" && stage.Name == project.GatePack && len(o.After) > 0 {
		gate, reason = task.GateHuman, packHoldReason
	}
	t, err := r.Spawn(ctx, runner.SpawnOptions{
		Kind: stage.Kind, Agent: o.Agent, After: after, Title: title,
		Prompt: prompt, ClaudeConfig: o.ClaudeConfig, Language: o.Language,
		Project: p.Name, Stage: taskStage, Keep: stage.Keep, Warn: o.Warn,
		Branch: o.Branch, Base: o.Base, Gate: gate, Reason: reason,
	})
	if err != nil {
		return nil, err
	}
	releaseEarlierStage(ctx, r, t, o.Warn)
	return t, nil
}

// changeApplyText asks the runner of a change decided now to apply it. The
// kit updates the roadmap itself at the next gate, so only a running
// milestone's plan is the runner's to update.
func changeApplyText(id string, milestoneRunning bool) string {
	if milestoneRunning {
		return id + " is decided now: raise the spec version, add it to the change log, update the roadmap and the running milestone plan, commit and report again"
	}
	return id + " is decided now: raise the spec version, add it to the change log, commit and report again; the roadmap is updated by the kit at the next gate"
}

// packHoldReason is why a pack chained after other stages waits for a
// release: its work is wasted when an earlier stage needs changes.
const packHoldReason = "review the earlier stages before the pack; release to build it"

// releaseEarlierStage lets gc close the kept tabs of the finished tasks t
// replaces: the earlier runs of its project stage. A tab already past its
// close time closes at once.
func releaseEarlierStage(ctx context.Context, r *runner.Runner, t *task.Task, warn func(string)) {
	tasks, err := r.Store.List()
	if err != nil {
		return
	}
	for _, old := range tasks {
		if old.ID == t.ID || old.Project != t.Project || old.Stage != t.Stage || !old.Keep ||
			!old.Status.Terminal() || old.Herdr.Closed {
			continue
		}
		_, err := r.Keep(ctx, old.ID, false)
		if warn == nil {
			continue
		}
		if err != nil {
			warn(fmt.Sprintf("note: %s (earlier %s) keeps its tab open: %v", old.ID, t.Stage, err))
		} else {
			warn(fmt.Sprintf("note: %s (earlier %s) will close", old.ID, t.Stage))
		}
	}
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
					At: time.Now(), Text: "share " + args[0] + " switched " + args[1] + " by the user",
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

// gateVerbs describes the commands that record a gate outcome.
var gateVerbs = map[string]struct{ use, short, long string }{
	"approve": {
		use:   "approve <spec|pack>",
		short: "Record that the spec or the stakeholder pack is approved",
		long: "approve records who approved which version, and when, then commits the project's " +
			"records in one commit. The version is read from the committed artifact: the spec's " +
			"version, or the highest committed pack folder. It refuses an artifact that is not " +
			"committed, has uncommitted changes or already has an outcome. An outcome holds for the " +
			"committed content: when that content changes under the same version, the gate opens " +
			"again and a new outcome replaces the old one. Approving the pack " +
			"imports the roadmap from the committed roadmap proposal. A lite-track project may " +
			"skip the pack only with --waive-pack at spec approval, before the pack has an outcome; " +
			"the spec gate is never waived.",
	},
	"changes": {
		use:   "changes <spec|pack> --stdin",
		short: "Record that changes were requested, and send the feedback to the stage runner",
		long: "changes records the outcome for the committed version, commits the records, and " +
			"sends the feedback from stdin to the stage's runner when its tab is open, so it " +
			"revises in the same session; otherwise it prints the command that starts a revision.",
	},
	"reject": {
		use:   "reject <spec|pack> --note WHY",
		short: "Record that the spec or the stakeholder pack was rejected",
		long:  "reject records the outcome and its reason for the committed version, and commits the records.",
	},
}

func newProjectGateCommand(verb string) *cobra.Command {
	v := gateVerbs[verb]
	o := project.DecideOptions{Outcome: verb}
	var name string
	var stdin bool
	cmd := &cobra.Command{
		Use: v.use, Short: v.short, Long: v.long,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{project.GateSpec, project.GatePack},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := project.RefuseInsideRunner(os.Getenv); err != nil {
				return err
			}
			o.Gate = args[0]
			feedback := ""
			if verb == "changes" {
				if !stdin {
					return errors.New("give the feedback on stdin with --stdin")
				}
				var err error
				if feedback, err = promptText(cmd, nil, true); err != nil {
					return err
				}
				if feedback == "" {
					return errors.New("the feedback on stdin is empty")
				}
			}
			r, err := openRunner()
			if err != nil {
				return err
			}
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("find the current folder: %w", err)
			}
			p, err := resolveProject(r.Store, name, cwd)
			if err != nil {
				return err
			}
			if o.By == "" {
				o.By = project.Approver(cmd.Context(), p.Path)
			}
			return decideGate(cmd, r, p, o, feedback)
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "project", "", "project of the gate (default: the project holding the current folder, else the only one)")
	f.StringVar(&o.By, "by", "", "who decided (default: git's author name in the project)")
	switch verb {
	case "changes":
		f.BoolVar(&stdin, "stdin", false, "read the feedback from stdin")
	default:
		f.StringVar(&o.Note, "note", "", "a short note kept with the outcome")
	}
	if verb == "approve" {
		f.BoolVar(&o.WaivePack, "waive-pack", false, "with approve spec on a lite-track project: skip the stakeholder pack by your explicit choice")
	}
	return cmd
}

// decideGate records a gate outcome, then closes or reaches the stage's
// runner: an approval or a rejection lets its kept tab close, and changes
// go to it as a revision request.
func decideGate(cmd *cobra.Command, r *runner.Runner, p state.Project, o project.DecideOptions, feedback string) error {
	ctx, out := cmd.Context(), cmd.OutOrStdout()
	d, err := project.Decide(ctx, r.Store, p.Name, o)
	if d.Version == "" {
		return err
	}
	fmt.Fprintf(out, "%s %s %s of %s by %s", o.Gate, d.Version, strings.ReplaceAll(d.Status, "-", " "), p.Name, oneLineText(o.By))
	if d.Commit != "" {
		fmt.Fprintf(out, "; records committed in %s", d.Commit[:min(12, len(d.Commit))])
	}
	fmt.Fprintln(out)
	if err != nil {
		return err
	}
	if d.Replaced != "" {
		fmt.Fprintf(out, "note: %s %s changed since it was %s under the same version; this outcome replaces that one\n",
			d.Gate, d.Version, strings.ReplaceAll(d.Replaced, "-", " "))
	}
	if d.Waived {
		fmt.Fprintln(out, "pack gate: waived for the lite track by your choice; recorded in decisions.md")
	}
	if d.Roadmap {
		fmt.Fprintf(out, "roadmap: imported %d milestone(s) and %d dependency(ies) from %s into %s\n",
			d.Milestones, d.Dependencies, project.RoadmapProposalPath, project.RoadmapPath)
	}
	if d.Gate == project.GatePack && d.NoPDF {
		fmt.Fprintf(out, "note: %s holds no %s\n", d.Folder, project.PackPDF)
	}
	tasks, err := r.Store.List()
	if err != nil {
		return err
	}
	var latest *task.Task
	for _, t := range tasks {
		if t.Project != p.Name || t.Stage != o.Gate {
			continue
		}
		latest = t
		if d.Status != project.GateChangesRequested && t.Keep {
			if _, err := r.Keep(ctx, t.ID, false); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: %s keeps its tab open: %v\n", t.ID, err)
			}
		}
	}
	if d.Status == project.GateChangesRequested {
		sendFeedback(cmd, r, p, latest, d, feedback)
	}
	rep, err := projectReport(ctx, r.Store, p)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "next: %s\n", rep.Next())
	return err
}

// sendFeedback asks the stage's open runner to revise, else prints the
// command that starts a revision.
func sendFeedback(cmd *cobra.Command, r *runner.Runner, p state.Project, t *task.Task, d project.Decided, feedback string) {
	out := cmd.OutOrStdout()
	revise := "Revise " + project.SpecPath + ", raise its version by one, commit and report again."
	if d.Gate == project.GatePack {
		revise = fmt.Sprintf("Write the revision as %s/, commit it and report again; leave committed pack folders unchanged.",
			project.PackDir(versionOf(d.Version)+1))
	}
	// A revision of pack v1 brings the roadmap proposal to the pack's scope,
	// which only a new pack run may commit, so v1's runner does not get it.
	first := d.Gate == project.GatePack && versionOf(d.Version) == 1
	if first {
		if rec, err := project.Load(r.Store, p.Name); err == nil && rec.Graduated != nil {
			first = false
		}
	}
	if !first && t != nil && t.Herdr.PaneID != "" && !t.Herdr.Closed && t.Status != task.Stopped {
		msg := fmt.Sprintf("The user requested changes to %s %s. Feedback:\n\n%s\n\n%s", d.Gate, d.Version, feedback, revise)
		working, err := r.Send(cmd.Context(), t.ID, msg)
		if err == nil {
			how := "it is revising"
			if !working {
				how = "herdr has not seen it start a turn yet"
			}
			fmt.Fprintf(out, "feedback: sent to %s; %s\n", t.ID, how)
			return
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "note: could not send the feedback to %s: %v\n", t.ID, err)
	}
	if first {
		fmt.Fprintf(out, "feedback: pack v2 must also update %s; start it with the same feedback on stdin: hrk project run pack --project %s --stdin\n",
			project.RoadmapProposalPath, p.Name)
		return
	}
	fmt.Fprintf(out, "feedback: no open %s runner; start a revision with the same feedback on stdin: hrk project run %s --project %s --stdin\n",
		d.Gate, d.Gate, p.Name)
}

func versionOf(v string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(v, "v")) // versions come from project.Version
	return n
}

func oneLineText(s string) string { return strings.Join(strings.Fields(s), " ") }

func newProjectDecideCommand() *cobra.Command {
	var name string
	var stdin bool
	cmd := &cobra.Command{
		Use:   "decide --stdin",
		Short: "Keep a decision of yours in the project's decision log",
		Long: "decide reads one decision from stdin and keeps it in the kit's record of the " +
			"project. It reaches decisions.md with the next gate outcome or milestone commit.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := project.RefuseInsideRunner(os.Getenv); err != nil {
				return err
			}
			if !stdin {
				return errors.New("give the decision on stdin with --stdin")
			}
			text, err := promptText(cmd, nil, true)
			if err != nil {
				return err
			}
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
			if err := project.AddDecision(store, p.Name, text, time.Now()); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "recorded the decision for %s; it is committed with the next gate outcome\n", p.Name)
			return err
		},
	}
	cmd.Flags().StringVar(&name, "project", "", "project of the decision (default: the project holding the current folder, else the only one)")
	cmd.Flags().BoolVar(&stdin, "stdin", false, "read the decision from stdin")
	return cmd
}

// openProject opens the state directory and picks the project a command
// changes: the one named, else the project holding the current folder,
// else the only one.
func openProject(name string) (*state.Store, state.Project, error) {
	store, err := openStore()
	if err != nil {
		return nil, state.Project{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, state.Project{}, fmt.Errorf("find the current folder: %w", err)
	}
	p, err := resolveProject(store, name, cwd)
	if err != nil {
		return nil, state.Project{}, err
	}
	if p.Reserved {
		return nil, state.Project{}, fmt.Errorf("project %s is still being set up", p.Name)
	}
	return store, p, nil
}

const projectFlagHelp = "project to change (default: the project holding the current folder, else the only one)"

func newProjectCRCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cr",
		Short: "Decide on change requests",
		Long: "A change request (CR) starts with: hrk project run change --stdin. " +
			"Its impact runner writes the CR file and reports the class; cr decide records your decision.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newProjectCRDecideCommand())
	return cmd
}

func newProjectCRDecideCommand() *cobra.Command {
	var o project.ChangeDecision
	var name string
	cmd := &cobra.Command{
		Use:   "decide <n> <now|next|backlog|reject>",
		Short: "Record your decision on a change request",
		Long: "cr decide records whether the change goes into the running milestone (now), the " +
			"next one (next), the backlog, or is rejected. The class comes from the impact " +
			"runner's report. The running milestone keeps its scope: a scope change decided now " +
			"needs --blocking while a milestone is in progress. A scope change decided now or next " +
			"after the pack was approved needs a new pack approval before the next milestone " +
			"starts; the running milestone and graduation go on. Nothing is committed: the record " +
			"reaches the repository with the next gate outcome or milestone.",
		Example: `  hrk project cr decide 3 next
  hrk project cr decide CR-4 now --blocking --note "the launch needs it"`,
		Args:      cobra.ExactArgs(2),
		ValidArgs: project.ChangeOutcomes,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := project.RefuseInsideRunner(os.Getenv); err != nil {
				return err
			}
			id, err := project.ParseChangeID(args[0])
			if err != nil {
				return err
			}
			o.ID, o.Outcome = id, args[1]
			store, p, err := openProject(name)
			if err != nil {
				return err
			}
			if o.By == "" {
				o.By = project.Approver(cmd.Context(), p.Path)
			}
			d, err := project.DecideChange(store, p.Name, o)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			c := d.Change
			fmt.Fprintf(out, "%s (%s) of %s decided %s", c.ID, c.Class, p.Name, c.Status)
			if c.Milestone != "" {
				fmt.Fprintf(out, " for %s", c.Milestone)
			}
			fmt.Fprintln(out, "; committed with the next gate outcome or milestone")
			if d.Backlog != "" {
				fmt.Fprintf(out, "backlog: kept as %s\n", d.Backlog)
			}
			if d.Reapproval {
				fmt.Fprintf(out, "re-approval: the pack needs a new approval before the next milestone starts: hrk project run pack --project %s\n", p.Name)
			}
			if c.Class == project.ClassTweak && c.Status == project.ChangeNow {
				fmt.Fprintln(out, "next: a tweak goes to the runner building that part: hrk send <id> \"<the change>\"")
			} else if c.Status == project.ChangeNow && c.Task != "" {
				rec, err := project.Load(store, p.Name)
				running := err == nil && project.RunningMilestone(rec) != nil
				fmt.Fprintf(out, "next: ask %s to apply it: hrk send %s \"%s\"\n", c.Task, c.Task, changeApplyText(c.ID, running))
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "project", "", projectFlagHelp)
	f.BoolVar(&o.Blocking, "blocking", false, "with now: the change may change the running milestone, which otherwise keeps its scope")
	f.StringVar(&o.By, "by", "", "who decided (default: git's author name in the project)")
	f.StringVar(&o.Note, "note", "", "a short note kept with the decision; required with reject")
	return cmd
}

func newProjectBacklogCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backlog",
		Short: "Keep ideas for later",
		Args:  cobra.NoArgs,
	}
	var name string
	var stdin bool
	add := &cobra.Command{
		Use:   "add --stdin",
		Short: "Add an idea to the project's backlog",
		Long: "backlog add keeps the idea read from stdin as the next backlog item. No runner " +
			"starts and nothing is committed: backlog.md is written with the next gate outcome or milestone.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := project.RefuseInsideRunner(os.Getenv); err != nil {
				return err
			}
			if !stdin {
				return errors.New("give the idea on stdin with --stdin")
			}
			text, err := promptText(cmd, nil, true)
			if err != nil {
				return err
			}
			store, p, err := openProject(name)
			if err != nil {
				return err
			}
			it, err := project.AddBacklog(store, p.Name, text, time.Now())
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s added to the backlog of %s; committed with the next gate outcome or milestone\n", it.ID, p.Name)
			return err
		},
	}
	add.Flags().StringVar(&name, "project", "", projectFlagHelp)
	add.Flags().BoolVar(&stdin, "stdin", false, "read the idea from stdin")
	cmd.AddCommand(add)
	return cmd
}

func newProjectDepCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dep",
		Short: "Track what the project waits on outside itself",
		Long: "A dependency is something outside the project, such as sandbox keys or a vendor " +
			"contract. Runners that need one are spawned with --gate human --reason \"needs D<n>\" " +
			"and released when it is ready. Record only where a secret is kept, never its value.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newProjectDepAddCommand(), newProjectDepSetCommand())
	return cmd
}

func newProjectDepAddCommand() *cobra.Command {
	var o project.DependencyOptions
	var name string
	cmd := &cobra.Command{
		Use:   "add <name> --owner user|vendor|runner",
		Short: "Record a dependency",
		Long: "dep add records the dependency as needed. --secret-location names where a secret " +
			"is kept, such as \"1Password: payments sandbox\"; a value that looks like a secret is " +
			"refused. Nothing is committed until the next gate outcome or milestone.",
		Example: `  hrk project dep add "Payment sandbox keys" --owner vendor --milestone M2 --secret-location "1Password: payments sandbox"`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := project.RefuseInsideRunner(os.Getenv); err != nil {
				return err
			}
			o.Name = args[0]
			store, p, err := openProject(name)
			if err != nil {
				return err
			}
			d, err := project.AddDependency(store, p.Name, o)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %s recorded for %s as %s; hold runners that need it with: hrk spawn --gate human --reason \"needs %s\" ...\n",
				d.ID, d.Name, p.Name, d.Status, d.ID)
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "project", "", projectFlagHelp)
	f.StringVar(&o.Owner, "owner", "", "who provides it: "+strings.Join(project.DependencyOwners, ", "))
	f.StringVar(&o.Milestone, "milestone", "", "milestone that needs it, such as M2")
	f.StringVar(&o.Workaround, "workaround", "", "how work goes on until it is ready")
	f.StringVar(&o.SecretLocation, "secret-location", "", "where its secret is kept, never the secret itself")
	return cmd
}

func newProjectDepSetCommand() *cobra.Command {
	var name, status string
	cmd := &cobra.Command{
		Use:   "set <D<n>> --status needed|requested|ready",
		Short: "Change a dependency's status",
		Long: "dep set records the new status. When a dependency is ready, it lists the held " +
			"tasks whose reason names it, with the command that releases each.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := project.RefuseInsideRunner(os.Getenv); err != nil {
				return err
			}
			id, err := project.ParseDependencyID(args[0])
			if err != nil {
				return err
			}
			store, p, err := openProject(name)
			if err != nil {
				return err
			}
			d, err := project.SetDependencyStatus(store, p.Name, id, status)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s %s of %s is %s; committed with the next gate outcome or milestone\n", d.ID, d.Name, p.Name, d.Status)
			if d.Status != project.DependencyReady {
				return nil
			}
			tasks, err := store.List()
			if err != nil {
				return err
			}
			for _, t := range project.HeldFor(tasks, p.Name, d.ID) {
				fmt.Fprintf(out, "held: %s %s (%s); release it with: hrk release %s\n", t.ID, t.Title, t.HoldReason, t.ID)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "project", "", projectFlagHelp)
	cmd.Flags().StringVar(&status, "status", "", "new status: "+strings.Join(project.DependencyStatuses, ", "))
	return cmd
}

func newProjectMilestoneCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "milestone",
		Short: "Start, chain, finish, reopen and close milestones",
		Long: "A milestone starts after the pack is approved (or waived on the lite track). M0 " +
			"builds the repository standard and an empty app; every later milestone starts with a " +
			"plan runner whose plan becomes a chain of feature runners, integrate runners and a close " +
			"runner that prepares the release branch and its notes. You merge that branch through a " +
			"pull request; done then sees the merge, commits the records and tags the release " +
			"locally. The kit never merges into the default branch and never pushes.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newMilestoneStartCommand(), newMilestoneChainCommand(), newMilestoneDoneCommand(),
		newMilestoneReopenCommand(), newMilestoneCloseCommand())
	return cmd
}

// milestoneRun is the input of milestone start and chain.
type milestoneRun struct {
	Project      string
	Agent        string
	ClaudeConfig string
	Language     string
	// Cwd picks the project when Project is empty.
	Cwd  string
	Warn func(string)
}

// milestoneFlags adds the flags milestone start and chain share.
func milestoneFlags(cmd *cobra.Command, o *milestoneRun) {
	f := cmd.Flags()
	f.StringVar(&o.Project, "project", "", projectFlagHelp)
	f.StringVar(&o.Agent, "agent", "", "agent spec harness[:model[:effort]] of every runner, overriding the stage kind's default")
	f.StringVar(&o.ClaudeConfig, "claude-config", "", "Claude config folder of Claude runners, as a path or a name such as claude2")
	f.StringVar(&o.Language, "language", "", "language of the runners' report summaries and questions")
}

// runMilestone runs fn for the project of o after the guard, holding the
// project's milestone lock so two starts or chains never race.
func runMilestone(cmd *cobra.Command, o *milestoneRun, fn func(*runner.Runner, state.Project) error) error {
	if err := project.RefuseInsideRunner(os.Getenv); err != nil {
		return err
	}
	var err error
	if o.Cwd, err = os.Getwd(); err != nil {
		return fmt.Errorf("find the current folder: %w", err)
	}
	o.Warn = func(line string) { fmt.Fprintln(cmd.ErrOrStderr(), line) }
	r, err := openRunner()
	if err != nil {
		return err
	}
	p, err := milestoneProject(r, *o)
	if err != nil {
		return err
	}
	unlock, err := r.Store.LockProject(p.Name, "milestone")
	if err != nil {
		return err
	}
	defer unlock()
	return fn(r, p)
}

// milestoneProject picks the project of a milestone command.
func milestoneProject(r *runner.Runner, o milestoneRun) (state.Project, error) {
	p, err := resolveProject(r.Store, o.Project, o.Cwd)
	if err != nil {
		return state.Project{}, err
	}
	if p.Reserved {
		return state.Project{}, fmt.Errorf("project %s is still being set up", p.Name)
	}
	return p, nil
}

func newMilestoneStartCommand() *cobra.Command {
	var o milestoneRun
	cmd := &cobra.Command{
		Use:   "start <M>",
		Short: "Start a milestone: M0's scaffold runner, or a later milestone's plan runner",
		Long: "start refuses until the pack is approved (or waived on the lite track), while the pack " +
			"needs a new approval after a scope change, and until the milestone before it in roadmap " +
			"order is done; reordering goes through a change request and a new pack approval. With the pack " +
			"waived on the lite track, M0 needs no roadmap, and the first start that finds the roadmap " +
			"proposal committed imports it, as a pack approval would; M1 and later refuse until it is " +
			"(hrk project run roadmap). It marks " +
			"the milestone in progress in the record, committed when the milestone is done. M0 spawns " +
			"the scaffold runner and the close runner after it; later milestones spawn the plan runner, whose plan answers its open " +
			"questions with the recommended options and lists them as decisions. When the first runner " +
			"of a milestone in progress failed or stopped, start runs it again.",
		Example: `  hrk project milestone start M0
  hrk project milestone start M1 --project demo-app`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMilestone(cmd, &o, func(r *runner.Runner, p state.Project) error {
				return startMilestone(cmd.Context(), cmd.OutOrStdout(), r, p, args[0], o)
			})
		},
	}
	milestoneFlags(cmd, &o)
	return cmd
}

// startMilestone marks milestone id of p in progress and spawns its first
// runner, or runs that runner again.
func startMilestone(ctx context.Context, out io.Writer, r *runner.Runner, p state.Project, id string, o milestoneRun) error {
	tasks, err := r.Store.List()
	if err != nil {
		return err
	}
	before, err := project.Load(r.Store, p.Name)
	if err != nil {
		return err
	}
	m, again, err := project.StartMilestone(ctx, r.Store, p, id, tasks, time.Now())
	if err != nil {
		return err
	}
	rec, err := project.Load(r.Store, p.Name)
	if err != nil {
		return err
	}
	if !project.HasRoadmap(before) && project.HasRoadmap(rec) {
		fmt.Fprintf(out, "imported the roadmap from %s (%d milestones), the pack gate being waived\n", project.RoadmapProposalPath, len(rec.Milestones))
	}
	stage, err := project.LoadStage(project.FirstTemplate(id))
	if err != nil {
		return err
	}
	h, err := stageHarness(r, stage, o.Agent)
	if err != nil {
		return err
	}
	def, err := project.DefaultBranch(ctx, p.Path)
	if err != nil {
		return err
	}
	data := project.NewStageData(rec, p.Path, project.Tools{}, "", time.Now())
	project.MilestoneData(&data, rec, m, def)
	so := stageRun{
		Project: p.Name, Agent: o.Agent, ClaudeConfig: o.ClaudeConfig, Language: o.Language,
		TaskStage: project.FirstStage(id), Warn: o.Warn,
	}
	if r.Config.Kinds[stage.Kind].Worktree {
		var own []*task.Task
		for _, t := range tasks {
			if t.Project == p.Name && t.Stage == so.TaskStage {
				own = append(own, t)
			}
		}
		var note string
		if so.Branch, so.Base, note, err = project.PickBranch("feat/"+id+"-scaffold", own, branchTaken(ctx, p.Path)); err != nil {
			return err
		}
		if note != "" {
			o.Warn(note)
		}
		if so.Base == "" {
			so.Base = def
		}
	}
	t, err := spawnStage(ctx, r, so, p, stage, h, data, p.Name+" "+id+": "+stage.Title)
	if err != nil {
		return fmt.Errorf("%s is in progress, but its runner did not start: %w; run start again", id, err)
	}
	if again != nil {
		fmt.Fprintf(out, "%s %s: runs %s again after %s %s\n", id, oneLineText(m.Title), stage.Name, again.ID, again.Status)
	} else {
		fmt.Fprintf(out, "%s %s is in progress since %s; recorded with the next commit point\n", id, oneLineText(m.Title), m.Started.Format(time.DateOnly))
	}
	if _, err := fmt.Fprintln(out, spawnLine(t)); err != nil {
		return err
	}
	if id == project.ScaffoldMilestone && so.Branch != "" {
		// The scaffold milestone has no plan: its close runner follows the
		// scaffold runner straight away.
		if closer := latestStageTask(tasks, p.Name, project.CloseStage(id)); closer == nil || (closer.Status.Terminal() && closer.Status != task.Done) {
			step, err := project.CloseStep(p.Name, id, "scaffold", m.Branch, tasks, branchTaken(ctx, p.Path))
			if err != nil {
				return fmt.Errorf("%s started, but its close runner cannot: %w", t.ID, err)
			}
			ct, err := spawnClose(ctx, r, p, rec, m, def, step, []string{t.ID}, o)
			if err != nil {
				return fmt.Errorf("%s started, but its close runner did not: %w", t.ID, err)
			}
			if _, err := fmt.Fprintln(out, spawnLine(ct)); err != nil {
				return err
			}
		}
	}
	parallelNote(r.Store, p.Name, o.Warn)
	return nil
}

// latestStageTask returns the newest task of project name in stage, or nil.
func latestStageTask(tasks []*task.Task, name, stage string) *task.Task {
	var latest *task.Task
	for _, t := range tasks {
		if t.Project == name && t.Stage == stage {
			latest = t
		}
	}
	return latest
}

// branchTaken returns project.BranchTaken for the repository at root.
func branchTaken(ctx context.Context, root string) func(string) string {
	return func(b string) string { return project.BranchTaken(ctx, root, b) }
}

// spawnClose spawns the close runner of milestone m laid out in step, after
// the tasks in after.
func spawnClose(ctx context.Context, r *runner.Runner, p state.Project, rec project.Record, m project.Milestone, def string,
	step project.ChainStep, after []string, o milestoneRun,
) (*task.Task, error) {
	stage, err := project.LoadStage(project.NodeTemplate(step.Node))
	if err != nil {
		return nil, err
	}
	h, err := stageHarness(r, stage, o.Agent)
	if err != nil {
		return nil, err
	}
	if !r.Config.Kinds[stage.Kind].Worktree {
		return nil, fmt.Errorf("the close runner runs as kind %s, which the config runs without a worktree; it needs one for its release branch", stage.Kind)
	}
	if step.Note != "" {
		o.Warn(step.Note)
	}
	data := project.NewStageData(rec, p.Path, project.Tools{}, "", time.Now())
	project.MilestoneData(&data, rec, m, def)
	project.NodeData(&data, step)
	so := stageRun{
		Project: p.Name, Agent: o.Agent, ClaudeConfig: o.ClaudeConfig, Language: o.Language,
		TaskStage: step.Stage, Warn: o.Warn, After: after, Branch: step.Branch, Base: step.Base,
	}
	t, err := spawnStage(ctx, r, so, p, stage, h, data, p.Name+" "+step.Stage+": "+stage.Title)
	if err == nil {
		recordNode(r, t, step)
	}
	return t, err
}

func newMilestoneChainCommand() *cobra.Command {
	var o milestoneRun
	cmd := &cobra.Command{
		Use:   "chain <M>",
		Short: "Spawn a milestone's feature runners from its committed plan",
		Long: "chain reads the yaml block of the milestone's committed plan once its plan runner is " +
			"done, and refuses while an open question has no answer. A feature has at most one parent " +
			"and an integrate node any number; a final integrate node over every leaf is added when " +
			"the plan does not end in one, and a close runner after it. Runners are spawned in " +
			"dependency order with --after their parents: root features and integrate nodes start from " +
			"the default branch, a feature with a parent from its parent's branch, and the close runner " +
			"from the final integrate node's branch. Integrate nodes work on integrate/<M>-<id>, the " +
			"close runner on release/<M>. Backlog items the plan takes are marked planned. Run it " +
			"again after a runner failed, even while others still run: done and unfinished nodes are " +
			"kept, failed and stopped nodes run again, and a queued node waits for its parent's new " +
			"runner. A failed node whose runner created its branch continues on <branch>-r<k> from " +
			"that branch, and any other existing branch is refused before anything is spawned.",
		Example: `  hrk project milestone chain M1`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMilestone(cmd, &o, func(r *runner.Runner, p state.Project) error {
				return chainMilestone(cmd.Context(), cmd.OutOrStdout(), r, p, args[0], o)
			})
		},
	}
	milestoneFlags(cmd, &o)
	return cmd
}

// chainMilestone spawns the runners of milestone id's plan in dependency
// order. Every check runs before the first spawn.
func chainMilestone(ctx context.Context, out io.Writer, r *runner.Runner, p state.Project, id string, o milestoneRun) error {
	rec, err := project.Load(r.Store, p.Name)
	if err != nil {
		return err
	}
	tasks, err := r.Store.List()
	if err != nil {
		return err
	}
	if err := project.CheckChainable(rec, p.Name, id, tasks); err != nil {
		return err
	}
	plan, err := project.ReadPlan(ctx, p.Path, rec, id)
	if err != nil {
		return err
	}
	if qs := project.Unanswered(plan); len(qs) > 0 {
		return fmt.Errorf("the plan of %s leaves %d open question(s) unanswered:\n- %s\nask its plan runner to answer each with the recommended option, commit and report again",
			id, len(qs), strings.Join(qs, "\n- "))
	}
	if err := project.CheckBacklogMoves(rec, id, plan.Backlog); err != nil {
		return err
	}
	def, err := project.DefaultBranch(ctx, p.Path)
	if err != nil {
		return err
	}
	steps, err := project.PlanChain(project.ChainInput{
		Project: p.Name, Milestone: id, Plan: plan, Tasks: tasks, DefaultBranch: def, Slug: runner.Slug,
		Title: func(stage, title string) string { return nodeTitle(p, stage, title) }, BranchTaken: branchTaken(ctx, p.Path),
		Merged: rec.Milestones[slices.IndexFunc(rec.Milestones, func(m project.Milestone) bool { return m.ID == id })].Branch,
	})
	if err != nil {
		return err
	}
	stages := map[string]project.Stage{}
	harnesses := map[string]task.Harness{}
	for _, st := range steps {
		name := project.NodeTemplate(st.Node)
		if _, loaded := stages[name]; loaded {
			continue
		}
		if stages[name], err = project.LoadStage(name); err != nil {
			return err
		}
		if harnesses[name], err = stageHarness(r, stages[name], o.Agent); err != nil {
			return err
		}
	}
	m := rec.Milestones[slices.IndexFunc(rec.Milestones, func(m project.Milestone) bool { return m.ID == id })]
	moved, err := project.ApplyBacklogMoves(r.Store, p.Name, id, plan.Backlog)
	if err != nil {
		return err
	}
	if moved > 0 {
		fmt.Fprintf(out, "backlog: %d item(s) planned into %s; recorded with the next commit point\n", moved, id)
	}
	byNode := map[string]*task.Task{}
	spawned, live := 0, 0
	for _, st := range steps {
		if st.Done != nil {
			byNode[st.Node.ID] = st.Done
			fmt.Fprintf(out, "kept %s: %s is done\n", st.Stage, st.Done.ID)
			continue
		}
		if st.Live != nil {
			byNode[st.Node.ID] = st.Live
			live++
			line := fmt.Sprintf("kept %s: %s is %s", st.Stage, st.Live.ID, st.Live.Status)
			if st.Live.Status == task.Queued {
				// Its parents may all be done with the follow-up that starts
				// it lost; settling starts it then, and else changes nothing.
				if err := r.Settle(ctx, st.Live.ID); err != nil {
					return fmt.Errorf("settle %s: %w; the %d runner(s) above were spawned", st.Live.ID, err, spawned)
				}
				if t, err := r.Store.Get(st.Live.ID); err == nil && t.Status != task.Queued {
					line += ", now " + string(t.Status)
				}
			}
			fmt.Fprintln(out, line)
			continue
		}
		if q := st.Replace; q != nil {
			// A waiting runner has done no work and made no branch. Stopping
			// it fails what waits on it, which this run replaces too.
			if _, err := r.Stop(ctx, q.ID, false); err != nil {
				return fmt.Errorf("replace %s: stop %s: %w; the %d runner(s) above were spawned", st.Stage, q.ID, err, spawned)
			}
		}
		if st.Added {
			fmt.Fprintf(out, "added %s: the plan does not end in one integrate node, so one integrates %s\n",
				st.Stage, strings.Join(st.Node.DependsOn, ", "))
		}
		if st.Note != "" {
			o.Warn(st.Note)
		}
		stage := stages[project.NodeTemplate(st.Node)]
		data := project.NewStageData(rec, p.Path, project.Tools{}, "", time.Now())
		project.MilestoneData(&data, rec, m, def)
		project.NodeData(&data, st)
		so := stageRun{
			Project: p.Name, Agent: o.Agent, ClaudeConfig: o.ClaudeConfig, Language: o.Language,
			TaskStage: st.Stage, Warn: o.Warn,
		}
		for _, parent := range st.Node.DependsOn {
			so.After = append(so.After, byNode[parent].ID)
		}
		if r.Config.Kinds[stage.Kind].Worktree {
			so.Branch, so.Base = st.Branch, st.Base
		}
		t, err := spawnStage(ctx, r, so, p, stage, harnesses[stage.Name], data, nodeTitle(p, st.Stage, st.Node.Title))
		if err != nil {
			if spawned > 0 || st.Replace != nil {
				return fmt.Errorf("spawn %s: %w; the %d runner(s) above were spawned: run the chain again for the rest", st.Stage, err, spawned)
			}
			return fmt.Errorf("spawn %s: %w", st.Stage, err)
		}
		recordNode(r, t, st)
		byNode[st.Node.ID] = t
		spawned++
		if st.Replace != nil {
			fmt.Fprintf(out, "replaced %s: %s → %s\n", st.Stage, st.Replace.ID, t.ID)
		}
		fmt.Fprintln(out, spawnLine(t))
	}
	switch {
	case spawned == 0 && live > 0:
		_, err := fmt.Fprintf(out, "no node of %s failed; nothing to run again\n", id)
		return err
	case spawned == 0:
		_, err := fmt.Fprintf(out, "every node of %s is done\n", id)
		return err
	}
	parallelNote(r.Store, p.Name, o.Warn)
	return nil
}

// nodeTitle is the task title of a runner of project p's chain stage for a
// node titled title.
func nodeTitle(p state.Project, stage, title string) string {
	return p.Name + " " + stage + ": " + runner.TitleFrom(title)
}

// recordNode records on t, a runner chain just spawned, the node of step
// st as the plan holds it now, so the next chain sees a node the plan
// changed. A runner that misses it is matched by its title.
func recordNode(r *runner.Runner, t *task.Task, st project.ChainStep) {
	if nt, err := r.Store.Update(t.ID, func(t *task.Task) error {
		t.Node = st.TaskNode()
		return nil
	}); err == nil {
		*t = *nt
	}
}

func newMilestoneDoneCommand() *cobra.Command {
	var o milestoneRun
	var merged string
	cmd := &cobra.Command{
		Use:   "done <M>",
		Short: "Record a milestone done once you merged its release branch, and tag it locally",
		Long: "done fetches origin when there is one and looks for the close runner's release branch " +
			"in the local default branch. It refuses until you merged that branch through your pull " +
			"request and pulled, while the main checkout is on another branch, behind origin, in the " +
			"middle of a merge, or holds changes to the files the kit renders. Then it imports the " +
			"release's roadmap update, if any and not imported by an earlier done of the milestone, " +
			"records the milestone done, commits the records in one commit and creates the annotated " +
			"tag tagFormat (v0.N.0 by default) on it. The tag is local: push it yourself if you want " +
			"it on origin. After a squash or rebase merge, pass the merged commit, which must hold the " +
			"release notes of the release branch's tip. Release notes merged before a reopen never " +
			"count as a new release.",
		Example: `  hrk project milestone done M1
  hrk project milestone done M2 --merged-commit 3f9c2ab`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMilestone(cmd, &o, func(r *runner.Runner, p state.Project) error {
				return finishMilestone(cmd.Context(), cmd.OutOrStdout(), r, p, args[0], merged, o)
			})
		},
	}
	cmd.Flags().StringVar(&o.Project, "project", "", projectFlagHelp)
	cmd.Flags().StringVar(&merged, "merged-commit", "", "after a squash or rebase merge: the commit on the default branch that holds the release")
	return cmd
}

// finishMilestone records milestone id of p done after its merge and tags
// it locally.
func finishMilestone(ctx context.Context, out io.Writer, r *runner.Runner, p state.Project, id, merged string, o milestoneRun) error {
	tasks, err := r.Store.List()
	if err != nil {
		return err
	}
	d, err := project.FinishMilestone(ctx, r.Store, p, id, tasks, project.FinishOptions{MergedCommit: merged})
	if d.Fetch != "" {
		o.Warn("note: could not fetch origin (" + d.Fetch + "); checked against what was fetched before")
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s %s is done: %s is merged into %s\n", id, oneLineText(d.Milestone.Title), d.Branch, d.DefaultBranch)
	if d.Roadmap > 0 {
		fmt.Fprintf(out, "roadmap: updated %d milestone(s) from %s\n", d.Roadmap, project.RoadmapUpdatePath(id))
	}
	fmt.Fprintf(out, "records: committed in %s\n", shortCommit(d.Commit))
	fmt.Fprintf(out, "tag: %s on %s, local only; push it yourself when you want it on origin: git push origin %s\n", d.Tag, shortCommit(d.Commit), d.Tag)
	_, err = fmt.Fprintf(out, "next: hrk project status %s\n", p.Name)
	return err
}

func shortCommit(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func newMilestoneReopenCommand() *cobra.Command {
	var o milestoneRun
	var reason string
	cmd := &cobra.Command{
		Use:   "reopen <M> --reason WHY",
		Short: "Put a done milestone back in progress for fixes, deleting its local tag",
		Long: "reopen refuses when the milestone's tag is on origin, since a pushed tag is not taken " +
			"back, while another milestone is in progress, and once a later milestone is done. It " +
			"deletes the local tag, sets the milestone in progress and keeps the reason in the decision " +
			"log, committed with the next commit point. The merged release stays on the default " +
			"branch: revert it through a pull request if it must come out. After the fixes, close " +
			"prepares a new release branch.",
		Example: `  hrk project milestone reopen M1 --reason "export drops the last row"`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMilestone(cmd, &o, func(r *runner.Runner, p state.Project) error {
				return reopenMilestone(cmd.Context(), cmd.OutOrStdout(), r, p, args[0], reason)
			})
		},
	}
	cmd.Flags().StringVar(&o.Project, "project", "", projectFlagHelp)
	cmd.Flags().StringVar(&reason, "reason", "", "why the milestone is reopened; kept in the decision log")
	return cmd
}

// reopenMilestone puts done milestone id of p back in progress.
func reopenMilestone(ctx context.Context, out io.Writer, r *runner.Runner, p state.Project, id, reason string) error {
	re, err := project.ReopenMilestone(ctx, r.Store, p, id, reason, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s %s is in progress again; recorded with the next commit point\n", id, oneLineText(re.Milestone.Title))
	switch {
	case re.Deleted:
		fmt.Fprintf(out, "tag: deleted the local tag %s\n", re.Tag)
	case re.Tag != "":
		fmt.Fprintf(out, "tag: %s was not in %s\n", re.Tag, p.Path)
	}
	if re.Milestone.Branch != "" {
		fmt.Fprintf(out, "%s stays merged; if its work must come out, revert that merge through a pull request: the kit never does\n", re.Milestone.Branch)
	}
	_, err = fmt.Fprintf(out, "next: fix on the branch the release came from, then prepare a new release: hrk project milestone close %s --project %s\n", id, p.Name)
	return err
}

func newMilestoneCloseCommand() *cobra.Command {
	var o milestoneRun
	cmd := &cobra.Command{
		Use:   "close <M>",
		Short: "Spawn a new close runner after a reopen or a failed close",
		Long: "close spawns a new close runner on release/<M>-r<k>, from the current branch of the " +
			"step the last close runner waited for (the final integrate node, or M0's scaffold), so " +
			"the new release holds the fixes made there. It is refused unless the milestone is in " +
			"progress and its close runner failed, stopped, or prepared a release merged before the " +
			"milestone was reopened.",
		Example: `  hrk project milestone close M1`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMilestone(cmd, &o, func(r *runner.Runner, p state.Project) error {
				return recloseMilestone(cmd.Context(), cmd.OutOrStdout(), r, p, args[0], o)
			})
		},
	}
	milestoneFlags(cmd, &o)
	return cmd
}

// recloseMilestone spawns a new close runner of milestone id.
func recloseMilestone(ctx context.Context, out io.Writer, r *runner.Runner, p state.Project, id string, o milestoneRun) error {
	rec, err := project.Load(r.Store, p.Name)
	if err != nil {
		return err
	}
	tasks, err := r.Store.List()
	if err != nil {
		return err
	}
	step, parent, err := project.Reclose(rec, p.Name, id, tasks, branchTaken(ctx, p.Path))
	if err != nil {
		return err
	}
	def, err := project.DefaultBranch(ctx, p.Path)
	if err != nil {
		return err
	}
	m := rec.Milestones[slices.IndexFunc(rec.Milestones, func(m project.Milestone) bool { return m.ID == id })]
	t, err := spawnClose(ctx, r, p, rec, m, def, step, []string{parent.ID}, o)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, spawnLine(t)); err != nil {
		return err
	}
	parallelNote(r.Store, p.Name, o.Warn)
	return nil
}

func newProjectGraduateCommand() *cobra.Command {
	var name string
	var o project.GraduateOptions
	var yes bool
	cmd := &cobra.Command{
		Use:   "graduate [--export DIR [--include-original]] [--yes]",
		Short: "Remove the discovery folder from the tree once the evergreen docs hold what it decided",
		Long: "graduate removes " + project.DiscoveryDir + "/ from the tree in one commit with the " +
			"project's records, and tags the commit before it " + project.ArchiveTag + " (or " +
			project.ArchiveTag + "-<n>), locally and last. Prepare it first with: hrk " +
			"project run " + project.GraduateStage + ", which writes " + project.RequirementsPath + ", " +
			"keeps the approved pack in " + project.GraduatedApprovalDir + "/ and the open change " +
			"requests in " + project.GraduatedChangesDir + "/, and rewrites links into the folder. " +
			"Without --yes it only checks and prints what it would do; with --yes it checks everything " +
			"again under the checkout lock first. It refuses until both gates passed, the discovery " +
			"stages finished, the requirements are committed, the folder has no uncommitted or " +
			"untracked files, no merge, rebase or cherry-pick is in progress, no markdown or HTML file " +
			"outside the folder links into it, and the export folder is absent or empty. A refused " +
			"commit restores the folder and leaves no tag. Graduation does not purge history: every " +
			"committed file stays reachable through the tag. The original brief was never committed " +
			"and stays on disk, untracked.",
		Example: `  hrk project graduate
  hrk project graduate --export ~/archive/demo-app-discovery --yes`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := project.RefuseInsideRunner(os.Getenv); err != nil {
				return err
			}
			s, p, err := openProject(name)
			if err != nil {
				return err
			}
			return graduateProject(cmd.Context(), cmd.OutOrStdout(), s, p, o, yes)
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "project", "", projectFlagHelp)
	f.StringVar(&o.Export, "export", "", "also copy the discovery folder as committed into DIR, which must be absent or empty; symlinks stay links")
	f.BoolVar(&o.IncludeOriginal, "include-original", false, "with --export: copy the git-ignored original brief too (regular files only)")
	f.BoolVar(&yes, "yes", false, "graduate; without it, only check and print what would happen")
	return cmd
}

// graduateProject checks the graduation of p and, with yes, carries it out.
func graduateProject(ctx context.Context, out io.Writer, s *state.Store, p state.Project, o project.GraduateOptions, yes bool) error {
	if !yes {
		plan, err := project.PlanGraduation(ctx, s, p, o)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "graduate %s: %d committed file(s) under %s/ leave the tree in one commit with the records\n",
			p.Name, plan.Files, project.DiscoveryDir)
		fmt.Fprintf(out, "archive: tag %s on %s, local only\n", plan.Tag, shortCommit(plan.Head))
		printGraduation(out, plan, o)
		_, err = fmt.Fprintf(out, "nothing changed yet; graduate with: hrk project graduate --project %s%s --yes\n",
			p.Name, exportFlags(plan, o))
		return err
	}
	g, err := project.Graduate(ctx, s, p, o)
	if g.Commit == "" {
		return err
	}
	fmt.Fprintf(out, "graduated %s: %s/ removed in %s\n", p.Name, project.DiscoveryDir, shortCommit(g.Commit))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "archive: tag %s on %s, local only; push it yourself when you want it on origin: git push origin %s\n",
		g.Tag, shortCommit(g.Head), g.Tag)
	printGraduation(out, g.GraduatePlan, o)
	fmt.Fprintf(out, "the way back: git -C %s show %s:%s\n", harness.ShellQuote(p.Path), g.Tag, project.SpecPath)
	_, err = fmt.Fprintf(out, "undo: git -C %s revert %s\n", harness.ShellQuote(p.Path), shortCommit(g.Commit))
	return err
}

// printGraduation prints what a graduation moves, exports and leaves, and
// that it does not purge history.
func printGraduation(out io.Writer, plan project.GraduatePlan, o project.GraduateOptions) {
	ids := make([]string, 0, len(plan.Moved))
	for id := range plan.Moved {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		fmt.Fprintf(out, "change request: %s now points at %s\n", id, plan.Moved[id])
	}
	if plan.Export != "" {
		what := "the folder as committed, symlinks kept as links"
		if o.IncludeOriginal {
			what += ", and the original brief"
		}
		fmt.Fprintf(out, "export: %s into %s\n", what, plan.Export)
	}
	fmt.Fprintf(out, "history: graduation does not purge history; every committed file of %s/ stays in git history and at the tag\n",
		project.DiscoveryDir)
	for _, f := range plan.Originals {
		fmt.Fprintf(out, "untracked: %s was never committed and stays on disk at its path\n", f)
	}
}

// exportFlags repeats the export flags of a dry run for its --yes command.
func exportFlags(plan project.GraduatePlan, o project.GraduateOptions) string {
	if plan.Export == "" {
		return ""
	}
	flags := " --export " + harness.ShellQuote(plan.Export)
	if o.IncludeOriginal {
		flags += " --include-original"
	}
	return flags
}
