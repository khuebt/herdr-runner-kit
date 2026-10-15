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
	"time"

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

// DiskSchema returns the schema version recorded in the state directory, or
// 0 when there is no state yet.
func DiskSchema(dir string) (int, error) {
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var meta stateMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return 0, fmt.Errorf("parse %s: %w", filepath.Join(dir, "state.json"), err)
	}
	return meta.SchemaVersion, nil
}

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
		data, err := os.ReadFile(p)
		if err == nil {
			err = json.Unmarshal(data, &t)
		}
		switch {
		case err != nil:
			// Unreadable is treated as live: migrating under it is the risk.
			live = append(live, filepath.Base(filepath.Dir(p))+" (unreadable)")
		case slices.Contains(liveStatuses, t.Status):
			live = append(live, t.ID)
		}
	}
	return live, nil
}

// prepareMigration refuses while runners are live and then copies the state
// directory to <dir>.bak-<from>, or to a time-stamped name beside an older
// backup. The copy is made under a temporary name and renamed into place, so
// a backup that exists is complete.
func prepareMigration(dir string, from int) error {
	live, err := LiveTasks(dir)
	if err != nil {
		return err
	}
	if len(live) > 0 {
		return fmt.Errorf("state schema %d needs a migration, but runners are still working (%s); "+
			"reinstall the previous release (hrk upgrade --version vX.Y.Z, or install.sh with HRK_VERSION=vX.Y.Z), "+
			"finish or stop them, then upgrade again",
			from, strings.Join(live, ", "))
	}
	backup := fmt.Sprintf("%s.bak-%d", dir, from)
	if _, err := os.Lstat(backup); err == nil {
		backup += time.Now().UTC().Format("-20060102T150405Z")
	}
	tmp := backup + ".partial"
	_ = os.RemoveAll(tmp) // left by a crash during an earlier copy
	if err := copyDir(dir, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("back up state before migrating: %w", err)
	}
	if err := os.Rename(tmp, backup); err != nil {
		_ = os.RemoveAll(tmp)
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
