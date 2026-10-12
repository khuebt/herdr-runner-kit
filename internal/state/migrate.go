package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/khuebt/herdr-runner-kit/internal/buildinfo"
)

const currentSchema = buildinfo.SchemaVersion

// migration moves the state directory from schema version `from` to from+1.
type migration struct {
	from int
	run  func(dir string) error
}

// migrations is the forward-only list, in order. Version 1 is the baseline
// and needs none. A new schema version appends one entry here.
var migrations []migration

type stateMeta struct {
	SchemaVersion int `json:"schemaVersion"`
}

// migrate brings the directory to currentSchema under the migration lock. It
// refuses state written by a newer binary instead of guessing at it.
func (s *Store) migrate() error {
	unlock, err := lock(filepath.Join(s.dir, "migrate.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	return runMigrations(s.dir, migrations, currentSchema)
}

func runMigrations(dir string, list []migration, target int) error {
	path := filepath.Join(dir, "state.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return writeMeta(path, target) // fresh directory
	}
	if err != nil {
		return fmt.Errorf("read state version: %w", err)
	}
	var meta stateMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if meta.SchemaVersion > target {
		return newerSchemaError("state in "+dir, meta.SchemaVersion)
	}
	if meta.SchemaVersion < target {
		if err := prepareMigration(dir, meta.SchemaVersion); err != nil {
			return err
		}
	}
	for _, m := range list {
		if m.from != meta.SchemaVersion || m.from >= target {
			continue
		}
		if err := m.run(dir); err != nil {
			return fmt.Errorf("migrate state from schema %d: %w", m.from, err)
		}
		meta.SchemaVersion = m.from + 1
		if err := writeMeta(path, meta.SchemaVersion); err != nil {
			return err
		}
	}
	if meta.SchemaVersion != target {
		return fmt.Errorf("no migration from state schema %d to %d", meta.SchemaVersion, target)
	}
	return nil
}

// liveStatuses belong to tasks whose runner is still working and will call
// report on the state as it is now.
var liveStatuses = []string{"starting", "running", "needs-human"}

// LiveTasks lists the tasks in the state directory whose runner is still
// working. It reads only the id and status, so it works on any schema.
func LiveTasks(dir string) ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "tasks", "*", "task.json"))
	if err != nil {
		return nil, err
	}
	var live []string
	for _, p := range paths {
		var t struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if data, err := os.ReadFile(p); err == nil && json.Unmarshal(data, &t) == nil && slices.Contains(liveStatuses, t.Status) {
			live = append(live, t.ID)
		}
	}
	return live, nil
}

// prepareMigration refuses while runners are live and then copies the state
// directory to <dir>.bak-<from>. An existing backup is kept: it is the older
// copy, from before an earlier attempt.
func prepareMigration(dir string, from int) error {
	live, err := LiveTasks(dir)
	if err != nil {
		return err
	}
	if len(live) > 0 {
		return fmt.Errorf("state schema %d needs a migration, but runners are still working (%s); "+
			"reinstall the previous release with herdr-runner-kit upgrade --version vX.Y.Z, finish or stop them, then upgrade again",
			from, strings.Join(live, ", "))
	}
	backup := fmt.Sprintf("%s.bak-%d", dir, from)
	if _, err := os.Stat(backup); err == nil {
		return nil
	}
	if err := copyDir(dir, backup); err != nil {
		_ = os.RemoveAll(backup)
		return fmt.Errorf("back up state before migrating: %w", err)
	}
	return nil
}

// copyDir copies the regular files of src into dst, skipping lock files.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, dirPerm)
		case !d.Type().IsRegular() || filepath.Ext(p) == ".lock":
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, filePerm)
	})
}

func writeMeta(path string, version int) error {
	data, err := json.Marshal(stateMeta{SchemaVersion: version})
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

func newerSchemaError(what string, version int) error {
	return fmt.Errorf("%s uses schema %d, newer than this binary supports (%d): upgrade herdr-runner-kit",
		what, version, currentSchema)
}
