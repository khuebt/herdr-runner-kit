package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/khuebt/herdr-runner-kit/internal/planrun"
	"github.com/khuebt/herdr-runner-kit/internal/project"
	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func newPlanCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Run an AgentKit plan one runner per phase",
	}
	cmd.AddCommand(newPlanRunCommand())
	return cmd
}

// planRunOptions are the inputs of plan run.
type planRunOptions struct {
	Parallel     int
	Branch       string
	Kind         string
	Agent        string
	ClaudeConfig string
	Language     string
	Skill        string
	Gate         string
	DryRun       bool
	Warn         func(string)
}

func newPlanRunCommand() *cobra.Command {
	var o planRunOptions
	cmd := &cobra.Command{
		Use:   "run <plan.md|plan dir>",
		Short: "Spawn a fresh runner per phase of an AgentKit plan, in waves on one branch",
		Long: "run reads plan.md and its phase-NN-*.md files: each phase's dependencies from its " +
			"frontmatter (the previous phase when absent) and its write scope from the backticked " +
			"paths under headings that name files. The first phase runs alone in a new worktree on " +
			"--branch; every later phase works in that same checkout, in waves of at most --parallel " +
			"phases whose scopes are declared and disjoint. When a phase is done the kit checks what " +
			"it changed against its scope, marks its file completed, and holds the next wave for " +
			"`release` when it changed files outside its scope or left the checkout dirty. Once every " +
			"phase is done, plan-done points to a report in the plan's reports folder. Run it again " +
			"after a phase failed or was stopped: done phases are kept and the run's checkout is reused.",
		Example: `  herdr-runner-kit plan run plans/260101-auth --dry-run
  herdr-runner-kit plan run plans/260101-auth/plan.md --branch feat/auth`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := project.RefuseInsideRunner(os.Getenv); err != nil {
				return err
			}
			if o.Parallel < 1 {
				return fmt.Errorf("--parallel must be at least 1, got %d", o.Parallel)
			}
			if !task.Gate(o.Gate).Valid() {
				return fmt.Errorf("invalid gate %q: want auto or human", o.Gate)
			}
			o.Warn = func(line string) { fmt.Fprintln(cmd.ErrOrStderr(), line) }
			plan, err := planrun.Read(args[0])
			if err != nil {
				return err
			}
			root := runner.Toplevel(cmd.Context(), plan.Dir)
			if root == "" {
				return fmt.Errorf("plan %s is not in a git repository", plan.Dir)
			}
			r, err := openRunner()
			if err != nil {
				return err
			}
			unlock, err := r.Store.Lock(runner.PlanLock(plan.Dir))
			if err != nil {
				return err
			}
			defer unlock()
			return runPlan(cmd.Context(), cmd.OutOrStdout(), r, plan, root, o)
		},
	}
	f := cmd.Flags()
	f.IntVar(&o.Parallel, "parallel", 2, "most phases that run at once in the shared checkout")
	f.StringVar(&o.Branch, "branch", "", "branch of the run's new worktree (default hrk/<id>-<plan slug>)")
	f.StringVar(&o.Kind, "kind", "flow", "task kind of every phase runner (see docs/config.md)")
	f.StringVar(&o.Agent, "agent", "", "agent spec harness[:model[:effort]], overriding the kind's default")
	f.StringVar(&o.ClaudeConfig, "claude-config", "", "Claude config folder of Claude runners, as a path or a name such as claude2")
	f.StringVar(&o.Language, "language", "", "language of the runners' report summaries and questions")
	f.StringVar(&o.Skill, "skill", "ak:cook", "skill each phase runner invokes on its phase file")
	f.StringVar(&o.Gate, "gate", "auto", "human holds every phase after the first for `release`")
	f.BoolVar(&o.DryRun, "dry-run", false, "print the waves and spawn nothing")
	return cmd
}

// runPlan spawns the phases of plan that are not done, wave by wave, each
// wave --after the previous one. Every check runs before the first spawn.
func runPlan(ctx context.Context, out io.Writer, r *runner.Runner, plan *planrun.Plan, root string, o planRunOptions) error {
	tasks, err := r.Store.List()
	if err != nil {
		return err
	}
	own := runner.PlanTasks(tasks, plan.Dir)
	for _, t := range own {
		switch {
		case t.Status == task.Held:
			return fmt.Errorf("%s (phase %d) is held; release or stop %s first", t.ID, t.Phase, t.ID)
		case !t.Status.Terminal():
			return fmt.Errorf("%s (phase %d) is %s; wait or stop it", t.ID, t.Phase, t.Status)
		}
	}
	latest := runner.LatestPhaseTasks(own)
	done := func(n int) bool {
		ph, ok := plan.Phase(n)
		return ok && runner.PhaseDone(ph, latest)
	}
	anchor := runner.PlanAnchor(own)
	waves, err := planrun.Waves(plan, o.Parallel, done)
	if err != nil {
		return err
	}
	if anchor == nil && len(waves) > 0 && len(waves[0]) > 1 {
		// The phase that creates the checkout runs before any that share it.
		waves = append([][]planrun.Phase{waves[0][:1], waves[0][1:]}, waves[1:]...)
	}
	// A run whose worktree is gone continues from its branch, so the work
	// of the done phases is kept.
	base := ""
	if anchor == nil {
		base = lostRunBranch(ctx, root, own)
	}
	if anchor != nil && o.Branch != "" {
		return fmt.Errorf("--branch names a new checkout, but this run already works in %s on %s; drop --branch", anchor.Worktree.Path, anchor.Worktree.Branch)
	}

	fmt.Fprintf(out, "plan %s: %d phases, %d to run\n", plan.Title, len(plan.Phases), countPhases(waves))
	for _, ph := range plan.Phases {
		if !done(ph.Number) {
			continue
		}
		if t := latest[ph.Number]; t != nil {
			fmt.Fprintf(out, "kept phase %d: %s is done\n", ph.Number, t.ID)
		} else {
			fmt.Fprintf(out, "kept phase %d: its file says %s\n", ph.Number, ph.Status)
		}
	}
	switch {
	case len(waves) == 0:
	case anchor != nil:
		fmt.Fprintf(out, "checkout: %s on %s, from %s\n", anchor.Worktree.Path, anchor.Worktree.Branch, anchor.ID)
	case base != "":
		fmt.Fprintf(out, "checkout: a new worktree from %s, whose worktree is gone\n", base)
	}
	if o.DryRun || len(waves) == 0 {
		for i, wave := range waves {
			for _, ph := range wave {
				fmt.Fprintln(out, waveLine(i, wave, ph, anchor == nil, o.Parallel))
			}
		}
		if len(waves) == 0 {
			_, err := fmt.Fprintf(out, "every phase of %s is done\n", plan.Title)
			return err
		}
		return nil
	}

	for _, ph := range plan.Phases {
		if t := latest[ph.Number]; t != nil && t.Status == task.Done {
			if err := planrun.SetStatus(ph.File, "completed"); err != nil {
				o.Warn("warning: " + err.Error())
			}
		}
	}
	if err := planrun.SetStatus(plan.File, "in-progress"); err != nil {
		o.Warn("warning: " + err.Error())
	}
	anchorID := ""
	if anchor != nil {
		anchorID = anchor.ID
	}
	var prev []string
	spawned := 0
	for _, wave := range waves {
		var cur []string
		for _, ph := range wave {
			after := slices.Clone(prev)
			for _, d := range ph.Deps {
				if t := latest[d]; t != nil && t.Status == task.Done && !slices.Contains(after, t.ID) {
					after = append(after, t.ID)
				}
			}
			var parallel []planrun.Phase
			for _, other := range wave {
				if other.Number != ph.Number {
					parallel = append(parallel, other)
				}
			}
			so := runner.SpawnOptions{
				Kind: o.Kind, Agent: o.Agent, Skill: o.Skill, ClaudeConfig: o.ClaudeConfig, Language: o.Language,
				Cwd: root, After: after, Title: fmt.Sprintf("phase %d: %s", ph.Number, ph.Title),
				Prompt: runner.PhasePrompt(plan, ph, parallel, latest[ph.Number]),
				Plan:   plan.Dir, Phase: ph.Number, Warn: o.Warn,
			}
			if o.Gate == string(task.GateHuman) && spawned > 0 {
				so.Gate, so.Reason = task.GateHuman, "plan run --gate human"
			}
			if anchorID == "" {
				so.Worktree, so.Branch, so.BranchTitle, so.Base = new(true), o.Branch, plan.Title, base
			} else {
				so.CheckoutOf = anchorID
			}
			t, err := r.Spawn(ctx, so)
			if err != nil {
				if spawned > 0 {
					return fmt.Errorf("spawn phase %d: %w; the %d runner(s) above were spawned: once they finish, run plan run again", ph.Number, err, spawned)
				}
				return fmt.Errorf("spawn phase %d: %w", ph.Number, err)
			}
			if anchorID == "" {
				anchorID = t.ID
			}
			spawned++
			cur = append(cur, t.ID)
			fmt.Fprintln(out, spawnLine(t))
		}
		prev = cur
	}
	return nil
}

// lostRunBranch returns the branch of the plan's newest worktree that the
// run created and that still exists, or "".
func lostRunBranch(ctx context.Context, root string, planTasks []*task.Task) string {
	for _, t := range slices.Backward(planTasks) {
		w := t.Worktree
		if w != nil && w.BranchCreated != nil && *w.BranchCreated && w.Branch != "" && runner.BranchExists(ctx, root, w.Branch) {
			return w.Branch
		}
	}
	return ""
}

// waveLine describes one phase of a wave for --dry-run, and why it runs
// alone when it does.
func waveLine(i int, wave []planrun.Phase, ph planrun.Phase, newCheckout bool, parallel int) string {
	deps := "no dependencies"
	if len(ph.Deps) > 0 {
		var nums []string
		for _, d := range ph.Deps {
			nums = append(nums, strconv.Itoa(d))
		}
		deps = "after " + strings.Join(nums, ", ")
	}
	scope := "no declared scope"
	if n := len(ph.Scope); n > 0 {
		scope = fmt.Sprintf("scope %d path(s)", n)
	}
	parts := []string{fmt.Sprintf("wave %d", i+1), fmt.Sprintf("phase %d %s", ph.Number, ph.Title), deps, scope}
	if len(wave) == 1 {
		switch {
		case i == 0 && newCheckout:
			parts = append(parts, "alone: it creates the run's checkout")
		case len(ph.Scope) == 0:
			parts = append(parts, "alone: it declares no write scope")
		case parallel == 1:
			parts = append(parts, "alone: --parallel 1")
		default:
			parts = append(parts, "alone: no other ready phase has a disjoint scope")
		}
	}
	return strings.Join(parts, " · ")
}

func countPhases(waves [][]planrun.Phase) int {
	n := 0
	for _, w := range waves {
		n += len(w)
	}
	return n
}
