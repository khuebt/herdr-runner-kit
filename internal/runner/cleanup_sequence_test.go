package runner

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// Cleanup sequences run worktree tasks in a real repository with an origin,
// merge their branches into origin/main, and clean up between the steps of
// the task lifecycle. After every step the rules of docs/state-model.md
// hold, and the ones that need git: the branch a waiting task starts from
// exists, a live task's worktree is on disk, and every recorded merged tip
// has its ref.

// cleanupScenario is a sequence of cleanup steps.
type cleanupScenario []seqStep

var cleanupOps = []seqOp{
	{"spawn", 3},
	{"run-launcher", 3},
	{"report", 3},
	{"run-follow-up", 3},
	{"merge", 2},
	{"edit-main", 1},
	{"cleanup", 2},
	{"remove-outside", 1},
	{"send", 1},
	{"release", 1},
	{"settle-sweep", 1},
}

func (cleanupScenario) Generate(rand *rand.Rand, size int) reflect.Value {
	return reflect.ValueOf(cleanupScenario(genSteps(rand, size, cleanupOps)))
}

// cleanupWorld is a seqWorld whose worktrees are real.
type cleanupWorld struct {
	*seqWorld
	wtRoot string
	edits  int
	// outside holds the worktrees removed outside the kit, which no kit
	// rule protects.
	outside map[string]bool
}

// newCleanupWorld clones origin into a new origin and repository, and has
// herdr's worktree create make a real git worktree.
func newCleanupWorld(t *testing.T, origin string) *cleanupWorld {
	dir := t.TempDir()
	git(t, dir, "clone", "-q", "--bare", origin, "origin.git")
	git(t, dir, "clone", "-q", "origin.git", "repo")
	repo, err := filepath.EvalSymlinks(filepath.Join(dir, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	w := &cleanupWorld{seqWorld: newSeqWorld(t, repo), wtRoot: filepath.Join(dir, "worktrees"), outside: map[string]bool{}}
	w.fake.on("worktree create", func(args []string) (string, error) {
		branch, base := flagValue(args, "--branch"), flagValue(args, "--base")
		path := filepath.Join(w.wtRoot, strings.ReplaceAll(branch, "/", "-"))
		if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-q", "-b", branch, path, base).CombinedOutput(); err != nil {
			return "", fmt.Errorf("git worktree add: %v: %s", err, out)
		}
		w.mu.Lock()
		w.panes++
		n := w.panes + 1
		w.mu.Unlock()
		return fmt.Sprintf(`{"result":{"root_pane":{"pane_id":"w%d:p1","tab_id":"w%d:t1","workspace_id":"w%d"},"worktree":{"path":%q,"branch":%q}}}`,
			n, n, n, path, branch), nil
	})
	return w
}

func flagValue(args []string, name string) string {
	if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// gitTry runs git in dir and returns its error with the output.
func gitTry(dir string, args ...string) error {
	args = append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (w *cleanupWorld) step(s seqStep) string {
	ctx, r := w.ctx, w.r
	switch s.Op {
	case "spawn":
		return w.spawn(s)
	case "report":
		return w.report(s)
	case "merge":
		return w.merge(s)
	case "edit-main":
		w.edits++
		editMain(w.t, w.repo, "a.txt", fmt.Sprintf("main edit %d\n", w.edits))
		return "edit a.txt on main"
	case "cleanup":
		items, err := r.Cleanable(ctx, Owners{}, true)
		if err != nil {
			return "cleanable: " + err.Error()
		}
		var listed []string
		for _, it := range items {
			listed = append(listed, it.Task.ID+" "+it.Verdict)
		}
		cleaned, err := r.Cleanup(ctx, items)
		return fmt.Sprintf("cleanup %v: cleaned %v, %v", listed, cleaned, err)
	case "remove-outside":
		t := w.pick(s.R[0], func(t *task.Task) bool {
			return t.Status.Terminal() && t.Worktree != nil && isDir(t.Worktree.Path)
		})
		if t == nil {
			return "remove-outside: no worktree"
		}
		git(w.t, w.repo, "worktree", "remove", "--force", t.Worktree.Path)
		w.outside[t.Worktree.Path] = true
		return "removed the worktree of " + t.ID + " outside the kit"
	}
	return w.seqWorld.step(s)
}

// spawn spawns a worktree task with no parent or one, and an empty base or
// another task's branch that exists, sometimes for release.
func (w *cleanupWorld) spawn(s seqStep) string {
	all := w.tasks()
	o := SpawnOptions{Prompt: "work", Cwd: w.repo, Worktree: new(true)}
	if len(all) > 0 && s.R[0]%2 == 1 {
		o.After = []string{all[s.R[1]%len(all)].ID}
	}
	// spawn takes any --base; one already gone fails only the launch.
	var branches []string
	for _, t := range all {
		if t.Worktree != nil && BranchExists(w.ctx, w.repo, t.Worktree.Branch) {
			branches = append(branches, t.Worktree.Branch)
		}
	}
	if len(branches) > 0 && s.R[2]%3 == 0 {
		o.Base = branches[(s.R[2]/3)%len(branches)]
	}
	if s.R[3]%4 == 0 {
		o.Gate = task.GateHuman
	}
	t, err := w.r.Spawn(w.ctx, o)
	desc := fmt.Sprintf("spawn after=%v base=%q gate=%s", o.After, o.Base, o.Gate)
	if err != nil {
		return desc + ": " + err.Error()
	}
	return desc + ": " + t.ID + " " + string(t.Status)
}

// report commits work in a live task's worktree, to a file of its own or
// sometimes to a.txt, which every task and main share, and reports done or failed.
func (w *cleanupWorld) report(s seqStep) string {
	t := w.pick(s.R[0], func(t *task.Task) bool {
		return t.Status == task.Starting || t.Status == task.Running || t.Status == task.NeedsHuman
	})
	if t == nil {
		return "report: no live task"
	}
	if t.Worktree != nil && isDir(t.Worktree.Path) {
		name := t.ID + ".txt"
		if s.R[2]%4 == 0 {
			name = "a.txt"
		}
		if err := os.WriteFile(filepath.Join(t.Worktree.Path, name), []byte(fmt.Sprintf("%s %d\n", t.ID, s.R[3])), 0o600); err != nil {
			w.t.Fatal(err)
		}
		git(w.t, t.Worktree.Path, "add", name)
		git(w.t, t.Worktree.Path, "commit", "-q", "-m", "feat: "+t.ID)
	}
	status := []task.Status{task.Done, task.Done, task.Failed}[s.R[1]%3]
	_, err := w.r.Report(t.ID, ReportOptions{Status: status, Summary: "did it"})
	return fmt.Sprintf("report %s %s: %v", t.ID, status, err)
}

// merge lands a done task's branch on origin/main: a merge, fast-forward
// when it can be, a squash, or a squash of a branch stacked on another
// task's.
func (w *cleanupWorld) merge(s seqStep) string {
	kind := s.R[1] % 3
	all := w.tasks()
	branchOf := map[string]bool{}
	for _, t := range all {
		if t.Worktree != nil {
			branchOf[t.Worktree.Branch] = true
		}
	}
	t := w.pick(s.R[0], func(t *task.Task) bool {
		return t.Status == task.Done && t.Worktree != nil && BranchExists(w.ctx, w.repo, t.Worktree.Branch) &&
			(kind != 2 || branchOf[t.Worktree.Base])
	})
	how := []string{"merge", "squash", "stacked squash"}[kind]
	if t == nil {
		return how + ": no branch to merge"
	}
	branch := t.Worktree.Branch
	land := filepath.Join(w.t.TempDir(), "land")
	git(w.t, w.repo, "worktree", "add", "-q", "--detach", land, "origin/main")
	defer git(w.t, w.repo, "worktree", "remove", "--force", land)
	var err error
	if kind == 0 {
		err = gitTry(land, "merge", "-q", "--no-edit", branch)
	} else if err = gitTry(land, "merge", "-q", "--squash", branch); err == nil {
		err = gitTry(land, "commit", "-q", "-m", "feat: squashed "+branch)
	}
	if err != nil {
		return fmt.Sprintf("%s %s of %s: not merged: %v", how, branch, t.ID, firstLine(err.Error(), 80))
	}
	git(w.t, land, "push", "-q", "origin", "HEAD:main")
	git(w.t, w.repo, "fetch", "-q", "origin")
	return fmt.Sprintf("%s %s of %s into origin/main", how, branch, t.ID)
}

// checkGit returns what the repository breaks of the rules that need git.
func (w *cleanupWorld) checkGit() []Violation {
	all := w.tasks()
	byID := map[string]*task.Task{}
	owner := map[string]*task.Task{}
	for _, t := range all {
		byID[t.ID] = t
		if t.Worktree != nil {
			owner[t.Worktree.Branch] = t
		}
	}
	var out []Violation
	for _, t := range all {
		if t.Worktree == nil {
			continue
		}
		if from := startBranch(t, byID); from != "" {
			o := owner[from]
			made := o == nil || o.Worktree.OwnsBranch() // a task's branch counts once created
			if made && gitOut(w.ctx, w.repo, "rev-parse", "--verify", "--quiet", from+"^{commit}") == "" {
				out = append(out, Violation{Rule: "base-branch-exists", Task: t.ID, Detail: fmt.Sprintf("%s to start from %s, which is gone", t.Status, from)})
			}
		}
		if !t.Status.Terminal() && t.Worktree.Path != "" && !w.outside[t.Worktree.Path] && !isDir(t.Worktree.Path) {
			out = append(out, Violation{Rule: "live-task-has-checkout", Task: t.ID, Detail: string(t.Status) + ", but " + t.Worktree.Path + " is gone"})
		}
		for _, ref := range t.Worktree.MergedTipRefs(t.ID) {
			if got := gitOut(w.ctx, w.repo, "rev-parse", "--verify", "--quiet", ref[0]); got != ref[1] {
				out = append(out, Violation{Rule: "merged-tip-has-ref", Task: t.ID, Detail: fmt.Sprintf("%s is %q, want %s", ref[0], got, ref[1])})
			}
		}
	}
	return out
}

// startBranch returns the branch a live task with no worktree yet will
// start from: its base, else the branch of its single parent with one,
// unless that branch was deleted after its merge, when it starts from the
// default branch. It returns "" for any other task.
func startBranch(t *task.Task, byID map[string]*task.Task) string {
	if t.Status.Terminal() || t.Worktree == nil || t.Worktree.Path != "" {
		return ""
	}
	if t.Worktree.Base != "" {
		return t.Worktree.Base
	}
	var parents []*task.Task
	for _, id := range t.After {
		if p := byID[id]; p != nil && p.Worktree != nil && p.Worktree.Branch != "" {
			parents = append(parents, p)
		}
	}
	if len(parents) != 1 || (parents[0].Worktree.BranchDeleted && parents[0].Worktree.MergedTip != "") {
		return ""
	}
	return parents[0].Worktree.Branch
}

func (w *cleanupWorld) checkAll() []Violation {
	return append(w.check(), w.checkGit()...)
}

func TestCleanupSequences(t *testing.T) {
	src := gitRepo(t)
	writeFile(t, filepath.Join(src, "a.txt"))
	git(t, src, "add", "a.txt")
	git(t, src, "commit", "-q", "-m", "chore: a")
	origin := filepath.Join(t.TempDir(), "origin.git")
	git(t, ".", "clone", "-q", "--bare", src, origin)
	checkSequences[cleanupScenario](t, 30, func(steps []seqStep) ([]string, string) {
		w := newCleanupWorld(t, origin)
		var trace []string
		for i, s := range steps {
			trace = append(trace, w.step(s))
			if vs := w.checkAll(); len(vs) > 0 {
				return trace, fmt.Sprintf("after step %d:\n%s", i+1, formatViolations(vs))
			}
		}
		for range 100 {
			var op string
			switch {
			case len(w.afters) > 0:
				op = "run-follow-up"
			case len(w.launchers) > 0:
				op = "run-launcher"
			case len(w.lost) > 0:
				op = "settle-sweep"
			default:
				return trace, ""
			}
			trace = append(trace, w.step(seqStep{Op: op}))
			if vs := w.checkAll(); len(vs) > 0 {
				return trace, "while draining the queues:\n" + formatViolations(vs)
			}
		}
		t.Fatal("the queues never drained")
		return nil, ""
	})
}
