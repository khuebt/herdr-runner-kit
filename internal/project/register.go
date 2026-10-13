package project

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/khuebt/herdr-runner-kit/internal/state"
)

// Register adds the git repository at path to the registry under name, or
// under the name of its committed snapshot, or its folder name. The record
// is imported from the snapshot committed at HEAD when there is one, else it
// starts fresh with an unknown type. Nothing is written to the repository.
func Register(ctx context.Context, s *state.Store, path, name string) (state.Project, Record, error) {
	dir, err := state.CanonicalPath(path)
	if err != nil {
		return state.Project{}, Record{}, err
	}
	top, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return state.Project{}, Record{}, fmt.Errorf("%s is not a git repository", dir)
	}
	common, err := git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return state.Project{}, Record{}, err
	}
	if filepath.Dir(common) != top {
		return state.Project{}, Record{}, fmt.Errorf("%s is a linked worktree; register the main checkout %s", top, filepath.Dir(common))
	}
	rec, imported, err := committedSnapshot(ctx, top)
	if err != nil {
		return state.Project{}, Record{}, err
	}
	switch {
	case name != "":
	case imported:
		name = rec.Name
	default:
		name = strings.ToLower(filepath.Base(top))
		if state.CheckProjectName(name) != nil {
			return state.Project{}, Record{}, fmt.Errorf("folder name %q is not a valid project name; pass --name", filepath.Base(top))
		}
	}
	if !imported {
		rec = NewRecord(name)
	}
	rec.Name, rec.Pending = name, 0
	p, err := s.ReserveProject(name, top)
	if err != nil {
		return state.Project{}, Record{}, err
	}
	if err := Save(s, rec); err != nil {
		return state.Project{}, Record{}, errors.Join(err, s.ReleaseProject(name))
	}
	if err := s.CommitProject(name); err != nil {
		return state.Project{}, Record{}, errors.Join(err, s.ReleaseProject(name))
	}
	p.Reserved = false
	return p, rec, nil
}

// committedSnapshot reads the record from the project.md committed at HEAD.
// ok is false when HEAD holds none.
func committedSnapshot(ctx context.Context, top string) (Record, bool, error) {
	ref := "HEAD:" + SnapshotPath
	if exec.CommandContext(ctx, "git", "-C", top, "cat-file", "-e", ref).Run() != nil {
		return Record{}, false, nil
	}
	cmd := exec.CommandContext(ctx, "git", "-C", top, "show", ref)
	data, err := cmd.Output()
	if err != nil {
		return Record{}, false, fmt.Errorf("read committed %s: %w", SnapshotPath, err)
	}
	rec, err := ParseSnapshot(data)
	if err != nil {
		return Record{}, false, fmt.Errorf("%s: %w", SnapshotPath, err)
	}
	return rec, true, nil
}

// Forget removes a project from the registry along with the kit's record.
// Unless force is set, it refuses while a task of the project is not
// finished, or the record holds changes not yet committed to the
// repository. The repository is never touched.
func Forget(s *state.Store, name string, force bool) error {
	if _, err := s.ProjectByName(name); err != nil {
		return err
	}
	if !force {
		tasks, err := s.List()
		if err != nil {
			return err
		}
		var active []string
		for _, t := range tasks {
			if t.Project == name && !t.Status.Terminal() {
				active = append(active, t.ID)
			}
		}
		if len(active) > 0 {
			return fmt.Errorf("project %s has unfinished tasks %s; stop them first, or pass --yes", name, strings.Join(active, ", "))
		}
		rec, err := Load(s, name)
		switch {
		case errors.Is(err, ErrNoRecord):
		case err != nil:
			return fmt.Errorf("%w; pass --yes to forget the project anyway", err)
		case rec.Pending > 0:
			return fmt.Errorf("project %s has %d change(s) not yet committed to its repository; pass --yes to drop them", name, rec.Pending)
		}
	}
	return s.ForgetProject(name)
}
