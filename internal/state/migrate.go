package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

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
