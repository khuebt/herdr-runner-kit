package project

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/redact"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// nodeClose is the type of the close step the kit adds after the work of
// a milestone. A plan cannot hold it: the block parser accepts only
// features and integrate nodes.
const nodeClose = "close"

// Files the close runner writes into the milestone's folder.
const (
	releaseNotesFile  = "release-notes.md"
	roadmapUpdateFile = "roadmap-update.md"
)

// RemoteTimeout bounds one git command that talks to origin.
var RemoteTimeout = 10 * time.Second

// CloseStage is the task stage of milestone id's close runner.
func CloseStage(id string) string { return id + "/" + stepClose }

// ReleaseBranch is the branch the close runner of milestone id prepares
// for the user's pull request.
func ReleaseBranch(id string) string { return "release/" + id }

// ReleaseNotesPath is the release notes of milestone id.
func ReleaseNotesPath(id string) string { return MilestoneDir(id) + "/" + releaseNotesFile }

// RoadmapUpdatePath is the roadmap update the close runner of milestone id
// writes when work moved to another milestone.
func RoadmapUpdatePath(id string) string { return MilestoneDir(id) + "/" + roadmapUpdateFile }

func closeNode(milestone, parent string) BlockNode {
	n := BlockNode{ID: stepClose, Type: nodeClose, Title: "Close " + milestone}
	if parent != "" {
		n.DependsOn = []string{parent}
	}
	return n
}

// CloseStep lays out a close runner of milestone that starts after the
// step parent, from base, the branch where the milestone's work ends: the
// final integrate node's, or the scaffold's. It gets release/<M>, or
// release/<M>-r<k> when an earlier close runner created that branch or
// the record holds it as merged, so a new release never reuses one.
func CloseStep(name, milestone, parent, base, merged string, tasks []*task.Task, exists func(string) bool) (ChainStep, error) {
	stage := CloseStage(milestone)
	own := stageTasks(tasks, name, stage)
	taken := func(b string) bool { return (merged != "" && b == merged) || exists(b) }
	branch, _, err := PickBranch(ReleaseBranch(milestone), own, taken)
	if err != nil {
		return ChainStep{}, fmt.Errorf("close of %s: %w", milestone, err)
	}
	return ChainStep{Node: closeNode(milestone, parent), Stage: stage, Branch: branch, Base: base}, nil
}

// CloseStale reports whether the close runner of m prepared the release
// branch that was merged before m was reopened, so m needs a new one.
func CloseStale(m Milestone, closer *task.Task) bool {
	return m.Status == MilestoneInProgress && m.Branch != "" && closer != nil && closer.Status == task.Done &&
		taskBranch(closer) == m.Branch
}

// Reclose checks that milestone id of project name may get a new close
// runner and lays it out. It is refused unless the milestone is in
// progress and its close runner failed, stopped, or prepared a release
// that was merged before the milestone was reopened. The new runner starts
// after the latest done runner of the step the old one waited for, from
// its branch, which holds the fixes made since; that runner is returned
// for --after.
func Reclose(rec Record, name, id string, tasks []*task.Task, exists func(string) bool) (ChainStep, *task.Task, error) {
	i, err := findMilestone(rec, id)
	if err != nil {
		return ChainStep{}, nil, err
	}
	m := rec.Milestones[i]
	if m.Status != MilestoneInProgress {
		return ChainStep{}, nil, fmt.Errorf("milestone %s is %s; only a milestone in progress is closed again (reopen a done one first with herdr-runner-kit project milestone reopen %s --reason WHY)",
			id, orDash(m.Status), id)
	}
	for _, t := range tasks {
		if t.Project == name && strings.HasPrefix(t.Stage, id+"/") && !t.Status.Terminal() {
			return ChainStep{}, nil, fmt.Errorf("%s of %s is %s; wait until it finishes, or stop it, before closing %s", t.ID, t.Stage, t.Status, id)
		}
	}
	closer := latestTask(tasks, name, CloseStage(id))
	switch {
	case closer == nil:
		return ChainStep{}, nil, fmt.Errorf("milestone %s has no close runner yet; it is spawned with the milestone's chain", id)
	case closer.Status == task.Done && !CloseStale(m, closer):
		return ChainStep{}, nil, fmt.Errorf("the close runner %s of %s is done; merge %s through your pull request, pull, then run herdr-runner-kit project milestone done %s",
			closer.ID, id, orDash(taskBranch(closer)), id)
	case closer.Status != task.Done && closer.Status != task.Failed && closer.Status != task.Stopped:
		return ChainStep{}, nil, fmt.Errorf("the close runner %s of %s is %s", closer.ID, id, closer.Status)
	}
	parent, err := closeParent(tasks, name, closer)
	if err != nil {
		return ChainStep{}, nil, fmt.Errorf("milestone %s: %w", id, err)
	}
	step, err := CloseStep(name, id, strings.TrimPrefix(parent.Stage, id+"/"), taskBranch(parent), m.Branch, tasks, exists)
	if err != nil {
		return ChainStep{}, nil, err
	}
	return step, parent, nil
}

// closeParent returns the latest done runner of the step closer waited
// for: the final integrate node, or the scaffold.
func closeParent(tasks []*task.Task, name string, closer *task.Task) (*task.Task, error) {
	if len(closer.After) != 1 {
		return nil, fmt.Errorf("the close runner %s waited for %d runners, not one; run the chain again instead", closer.ID, len(closer.After))
	}
	i := slices.IndexFunc(tasks, func(t *task.Task) bool { return t.ID == closer.After[0] })
	if i < 0 {
		return nil, fmt.Errorf("the runner %s that %s waited for is gone", closer.After[0], closer.ID)
	}
	parent := latestTask(tasks, name, tasks[i].Stage)
	switch {
	case parent == nil:
		return nil, fmt.Errorf("the runner %s that %s waited for is not a runner of %s", closer.After[0], closer.ID, name)
	case parent.Status != task.Done:
		return nil, fmt.Errorf("%s of %s is %s, not done; finish it first", parent.ID, parent.Stage, parent.Status)
	case taskBranch(parent) == "":
		return nil, fmt.Errorf("%s of %s worked without a branch of its own, so there is nothing to release", parent.ID, parent.Stage)
	}
	return parent, nil
}

// milestoneNumber reads the number of a milestone id such as M3.
var milestoneNumber = regexp.MustCompile(`^M([0-9]{1,6})$`)

// TagName is the tag of milestone id, the index-th of the roadmap:
// format with {N} replaced by the number of an id such as M3, else by
// index.
func TagName(format, id string, index int) (string, error) {
	if format == "" {
		format = DefaultTagFormat
	}
	n := index
	if m := milestoneNumber.FindStringSubmatch(id); m != nil {
		n, _ = strconv.Atoi(m[1]) // the pattern holds only digits
	}
	tag := strings.ReplaceAll(format, "{N}", strconv.Itoa(n))
	if !validBranch(tag) {
		return "", fmt.Errorf("tagFormat %q gives %q for %s, which is not a valid tag name", format, tag, id)
	}
	return tag, nil
}

// FinishOptions are the inputs of FinishMilestone.
type FinishOptions struct {
	// MergedCommit is the commit on the default branch that holds the
	// release, for a pull request merged by squash or rebase, whose
	// commits are not the release branch's.
	MergedCommit string
	Now          time.Time
}

// Finished is a milestone the kit saw merged, recorded, committed and tagged.
type Finished struct {
	Milestone Milestone
	// Branch is the release branch; Merged the commit found on the
	// default branch, its tip or the given merged commit.
	Branch, Merged, DefaultBranch string
	// Roadmap counts the milestones imported from the roadmap update, 0
	// when the release holds none.
	Roadmap int
	// Commit holds the records; Tag is the local annotated tag on it.
	Commit, Tag string
	// Fetch says why origin could not be fetched, or is "".
	Fetch string
}

// FinishMilestone records milestone id of p done once the user merged its
// release branch into the default branch through a pull request. It
// fetches origin when there is one, finds the release on the local default
// branch, checks that the main checkout is on that branch, up to date with
// origin, with no merge in progress and no kit-rendered file changed, then
// imports the release's roadmap update, records the milestone done, makes
// one records commit and tags it locally, last, so a refused commit leaves
// no tag and the milestone in progress. It never merges, pushes or tags
// anything on origin.
func FinishMilestone(ctx context.Context, s *state.Store, p state.Project, id string, tasks []*task.Task, o FinishOptions) (Finished, error) {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	rec, err := Load(s, p.Name)
	if err != nil {
		return Finished{}, err
	}
	i, err := findMilestone(rec, id)
	if err != nil {
		return Finished{}, err
	}
	m := rec.Milestones[i]
	switch m.Status {
	case MilestoneDone:
		return Finished{}, fmt.Errorf("milestone %s is done already, tagged %s", id, orDash(m.Tag))
	case MilestoneInProgress:
	default:
		return Finished{}, fmt.Errorf("milestone %s is %s; start it with herdr-runner-kit project milestone start %s", id, orDash(m.Status), id)
	}
	closer := latestTask(tasks, p.Name, CloseStage(id))
	switch {
	case closer == nil:
		return Finished{}, fmt.Errorf("milestone %s has no close runner yet; its release branch comes from the close runner", id)
	case CloseStale(m, closer):
		return Finished{}, fmt.Errorf("milestone %s was reopened after %s was merged; prepare a new release with herdr-runner-kit project milestone close %s", id, m.Branch, id)
	case closer.Status != task.Done:
		return Finished{}, fmt.Errorf("the close runner %s of %s is %s, not done", closer.ID, id, closer.Status)
	case taskBranch(closer) == "":
		return Finished{}, fmt.Errorf("the close runner %s of %s has no release branch", closer.ID, id)
	}
	d := Finished{Branch: taskBranch(closer)}
	if d.Tag, err = TagName(rec.TagFormat, id, i); err != nil {
		return Finished{}, err
	}
	if _, err := git(ctx, p.Path, "rev-parse", "--verify", "--quiet", "refs/tags/"+d.Tag); err == nil {
		return Finished{}, fmt.Errorf("tag %s exists already in %s; set another tagFormat or remove the tag", d.Tag, p.Path)
	}
	unlock, err := s.LockProject(p.Name, "checkout")
	if err != nil {
		return Finished{}, err
	}
	defer unlock()
	if d.DefaultBranch, err = DefaultBranch(ctx, p.Path); err != nil {
		return Finished{}, err
	}
	remote := Origin(ctx, p.Path) != ""
	if remote {
		if _, err := gitRemote(ctx, p.Path, "fetch", "--quiet", "--no-tags", "origin"); err != nil {
			d.Fetch = err.Error()
		}
	}
	if d.Merged, err = findMerge(ctx, p.Path, id, d.Branch, d.DefaultBranch, o.MergedCommit); err != nil {
		return Finished{}, err
	}
	if err := checkMainCheckout(ctx, s, p, d.DefaultBranch, "done"); err != nil {
		return Finished{}, err
	}
	roadmap, hasRoadmap, err := committedFile(ctx, p.Path, RoadmapUpdatePath(id))
	if err != nil {
		return Finished{}, err
	}
	decision := Decision{At: o.Now.UTC(), Text: fmt.Sprintf("%s done: %s is merged into %s; tagged %s locally", id, d.Branch, d.DefaultBranch, d.Tag)}
	var before []Milestone
	if _, err := update(s, p.Name, func(rec *Record) error {
		before = slices.Clone(rec.Milestones)
		if hasRoadmap {
			b, err := ParseBlock(roadmap, RoadmapUpdate, *rec)
			if err != nil {
				return fmt.Errorf("%s: %w", RoadmapUpdatePath(id), err)
			}
			d.Roadmap, _ = applyRoadmap(rec, b)
			decision.Text += "; roadmap updated from " + RoadmapUpdatePath(id)
		}
		j, err := findMilestone(*rec, id)
		if err != nil {
			return err
		}
		ms := &rec.Milestones[j]
		if ms.Status != MilestoneInProgress {
			return fmt.Errorf("milestone %s is %s", id, orDash(ms.Status))
		}
		ms.Status, ms.Closed, ms.Tag, ms.Branch = MilestoneDone, o.Now.UTC(), d.Tag, d.Branch
		d.Milestone = *ms
		rec.Decisions = append(rec.Decisions, decision)
		rec.Pending++
		return nil
	}); err != nil {
		return Finished{}, err
	}
	d.Commit, err = commitRendered(ctx, s, p, fmt.Sprintf("docs(project): %s done", id), RenderedPaths)
	if err != nil {
		return Finished{}, undoDone(ctx, s, p, id, before, decision, err)
	}
	if d.Commit == "" {
		if d.Commit, err = git(ctx, p.Path, "rev-parse", "HEAD"); err != nil {
			return d, err
		}
	}
	args := []string{"tag", "-a", d.Tag, "-m", id + " " + oneLine(m.Title), d.Commit}
	if out, err := gitCombined(ctx, p.Path, args...); err != nil {
		return d, fmt.Errorf("%s is done and its records are committed in %s, but the tag failed: %s\ncreate it with:\n  %s",
			id, d.Commit, firstLine(out, err), shellCommand(append([]string{"git", "-C", p.Path}, args...)))
	}
	return d, nil
}

// undoDone puts milestone id back in progress after its records commit
// failed, and restores the rendered files, so done can run again once the
// cause is fixed.
func undoDone(ctx context.Context, s *state.Store, p state.Project, id string, before []Milestone, decision Decision, cause error) error {
	reason := cause.Error()
	var ce *commitError
	if errors.As(cause, &ce) {
		reason = ce.reason
	}
	msg := fmt.Sprintf("%s stays in progress and is not tagged: committing its records failed: %s", id, reason)
	var errs []string
	if _, err := update(s, p.Name, func(rec *Record) error {
		rec.Milestones = before
		if k := len(rec.Decisions) - 1; k >= 0 && rec.Decisions[k] == decision {
			rec.Decisions = rec.Decisions[:k]
		}
		rec.Pending = max(rec.Pending-1, 0)
		return nil
	}); err != nil {
		errs = append(errs, "the kit's record still says done: "+err.Error())
	}
	if err := restoreRendered(ctx, p.Path); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s; undoing it failed too: %s", msg, strings.Join(errs, "; "))
	}
	return fmt.Errorf("%s; fix the cause and run done again", msg)
}

// restoreRendered puts the rendered files of root back as HEAD holds them,
// in the index and on disk. done checked they had no changes first.
func restoreRendered(ctx context.Context, root string) error {
	for _, rel := range RenderedPaths {
		if _, err := git(ctx, root, "cat-file", "-e", "HEAD:"+rel); err == nil {
			if out, err := gitLocked(ctx, root, "restore", "--source=HEAD", "--staged", "--worktree", "--", rel); err != nil {
				return fmt.Errorf("restore %s: %s", rel, firstLine(out, err))
			}
			continue
		}
		if out, err := gitLocked(ctx, root, "rm", "-q", "--cached", "--ignore-unmatch", "--", rel); err != nil {
			return fmt.Errorf("unstage %s: %s", rel, firstLine(out, err))
		}
		if err := os.Remove(filepath.Join(root, rel)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", rel, err)
		}
	}
	return nil
}

// commitID matches an abbreviated or full commit id.
var commitID = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// findMerge returns the commit that shows the release of milestone id on
// the local default branch: the tip of its release branch, or merged, a
// commit given for a squash or rebase merge, which must hold the release
// notes. A release found only on origin's default branch asks for a pull.
func findMerge(ctx context.Context, root, id, branch, def, merged string) (string, error) {
	notes := ReleaseNotesPath(id)
	var commit string
	if merged != "" {
		if !commitID.MatchString(merged) {
			return "", fmt.Errorf("--merged-commit %q is not a commit id", merged)
		}
		c, err := git(ctx, root, "rev-parse", "--verify", "--quiet", merged+"^{commit}")
		if err != nil {
			return "", fmt.Errorf("--merged-commit %s is not a commit of %s; pull first", merged, root)
		}
		if _, err := git(ctx, root, "cat-file", "-e", c+":"+notes); err != nil {
			return "", fmt.Errorf("commit %s holds no %s, so it is not the merge of %s", merged, notes, branch)
		}
		commit = c
	} else {
		for _, ref := range []string{"refs/heads/" + branch, "refs/remotes/origin/" + branch} {
			if c, err := git(ctx, root, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil {
				commit = c
				break
			}
		}
		if commit == "" {
			return "", fmt.Errorf("branch %s is gone from %s and origin; pass the commit on %s that holds it with --merged-commit <sha>", branch, root, def)
		}
	}
	if ancestor(ctx, root, commit, "refs/heads/"+def) {
		return commit, nil
	}
	if ancestor(ctx, root, commit, "refs/remotes/origin/"+def) {
		return "", fmt.Errorf("%s is merged into origin/%s but not into your local %s: pull %s, then run done again", branch, def, def, def)
	}
	if merged != "" {
		return "", fmt.Errorf("commit %s is not on %s: merge %s through your pull request, then pull", merged, def, branch)
	}
	return "", fmt.Errorf("%s is not merged into %s: merge %s through your pull request, then pull; after a squash or rebase merge, pass the merged commit with --merged-commit <sha>",
		branch, def, branch)
}

// ancestor reports whether commit is reachable from ref.
func ancestor(ctx context.Context, root, commit, ref string) bool {
	if _, err := git(ctx, root, "rev-parse", "--verify", "--quiet", ref); err != nil {
		return false
	}
	return exec.CommandContext(ctx, "git", "-C", root, "merge-base", "--is-ancestor", commit, ref).Run() == nil
}

// checkMainCheckout refuses a records commit on a main checkout that is
// not on the default branch, is behind or apart from origin's, has an
// operation such as a merge in progress or a runner at work, or holds
// changes to files the kit renders, which the commit would overwrite.
// command names the command to run again.
func checkMainCheckout(ctx context.Context, s *state.Store, p state.Project, def, command string) error {
	head, err := git(ctx, p.Path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || head != def {
		return fmt.Errorf("the main checkout %s is not on %s; check out %s there, pull, then run %s again", p.Path, def, def, command)
	}
	if _, err := git(ctx, p.Path, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+def); err == nil &&
		!ancestor(ctx, p.Path, "refs/remotes/origin/"+def, "HEAD") {
		return fmt.Errorf("your %s is behind origin/%s; pull, then run %s again", def, def, command)
	}
	if err := checkCheckout(ctx, s, p); err != nil {
		return err
	}
	var changed []string
	for _, args := range [][]string{{"diff", "--name-only", "HEAD"}, {"ls-files", "--others"}} {
		out, err := git(ctx, p.Path, append(append(args, "--"), RenderedPaths...)...)
		if err != nil {
			return err
		}
		changed = append(changed, strings.Fields(out)...)
	}
	if len(changed) > 0 {
		return fmt.Errorf("files the kit renders have changes not committed: %s; the records commit rewrites them: commit or discard the changes first",
			strings.Join(changed, ", "))
	}
	return nil
}

// gitRemote runs a git command that talks to origin, within RemoteTimeout
// and without prompting for credentials. Errors hide anything that looks
// like a secret.
func gitRemote(ctx context.Context, root string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, RemoteTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", append([]string{"-C", root}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if os.Getenv("GIT_SSH_COMMAND") == "" {
		cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	}
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("git %s origin did not answer in %s", args[0], RemoteTimeout)
	}
	if err != nil {
		return "", fmt.Errorf("git %s origin: %s", args[0], redact.Secrets(firstLine(string(out), err)))
	}
	return strings.TrimSpace(string(out)), nil
}

// Reopened is a done milestone put back in progress.
type Reopened struct {
	Milestone Milestone
	// Tag was the milestone's tag; Deleted is set when it existed locally
	// and was deleted.
	Tag     string
	Deleted bool
}

// ReopenMilestone puts done milestone id of p back in progress, for fixes
// found after its release was merged. It refuses while another milestone
// is in progress or a later one is done, and when the milestone's tag is
// on origin, since a pushed tag is not taken back. It deletes the local
// tag, keeps the merged release branch in the record so the close runner
// counts as stale, and keeps the reason in the decision log, committed
// with the next commit point. The merge on the default branch stays: the
// user reverts it through a pull request when needed.
func ReopenMilestone(ctx context.Context, s *state.Store, p state.Project, id, reason string, now time.Time) (Reopened, error) {
	reason = clip(redact.Secrets(oneLine(reason)), maxNote)
	if reason == "" {
		return Reopened{}, errors.New("say why with --reason")
	}
	rec, err := Load(s, p.Name)
	if err != nil {
		return Reopened{}, err
	}
	if err := checkReopen(rec, id); err != nil {
		return Reopened{}, err
	}
	m := rec.Milestones[slices.IndexFunc(rec.Milestones, func(m Milestone) bool { return m.ID == id })]
	r := Reopened{Tag: m.Tag}
	if r.Tag != "" && Origin(ctx, p.Path) != "" {
		out, err := gitRemote(ctx, p.Path, "ls-remote", "--tags", "origin", "refs/tags/"+r.Tag)
		if err != nil {
			return Reopened{}, fmt.Errorf("cannot tell whether origin holds tag %s: %w; retry when origin answers", r.Tag, err)
		}
		if out != "" {
			return Reopened{}, fmt.Errorf("tag %s is on origin, and a pushed tag is not taken back; fix forward in the next milestone instead", r.Tag)
		}
	}
	if r.Tag != "" {
		if _, err := git(ctx, p.Path, "rev-parse", "--verify", "--quiet", "refs/tags/"+r.Tag); err == nil {
			if _, err := git(ctx, p.Path, "tag", "-d", r.Tag); err != nil {
				return Reopened{}, err
			}
			r.Deleted = true
		}
	}
	text := fmt.Sprintf("%s reopened: %s", id, reason)
	if r.Tag != "" {
		text += "; local tag " + r.Tag + " deleted"
	}
	rec, err = update(s, p.Name, func(rec *Record) error {
		if err := checkReopen(*rec, id); err != nil {
			return err
		}
		ms := &rec.Milestones[slices.IndexFunc(rec.Milestones, func(m Milestone) bool { return m.ID == id })]
		ms.Status, ms.Tag, ms.Closed = MilestoneInProgress, "", time.Time{}
		rec.Decisions = append(rec.Decisions, Decision{At: now.UTC(), Text: text})
		rec.Pending++
		return nil
	})
	if err != nil {
		return r, err
	}
	r.Milestone = rec.Milestones[slices.IndexFunc(rec.Milestones, func(m Milestone) bool { return m.ID == id })]
	return r, nil
}

// checkReopen refuses to reopen milestone id unless it is done, no other
// milestone is in progress and none after it is done.
func checkReopen(rec Record, id string) error {
	i, err := findMilestone(rec, id)
	if err != nil {
		return err
	}
	if st := rec.Milestones[i].Status; st != MilestoneDone {
		return fmt.Errorf("milestone %s is %s; only a done milestone is reopened", id, orDash(st))
	}
	for j, other := range rec.Milestones {
		switch {
		case other.Status == MilestoneInProgress:
			return fmt.Errorf("milestone %s is in progress; reopen %s after it is done", other.ID, id)
		case j > i && other.Status == MilestoneDone:
			return fmt.Errorf("milestone %s after %s is done already; fix forward in the next milestone instead", other.ID, id)
		}
	}
	return nil
}
