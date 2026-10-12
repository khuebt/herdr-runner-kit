// Package state keeps durable task state on disk:
//
//	<dir>/state.json          schema version of the whole directory
//	<dir>/seq                 last issued task number, guarded by seq.lock
//	<dir>/tasks/<id>/task.json one task, guarded by tasks/<id>/task.lock
//	<dir>/events.jsonl        global append-only event log
//
// Prompts are stored here, so the directory is 0700 and every file is 0600.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/fsutil"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

const (
	dirPerm  = 0o700
	filePerm = 0o600
)

// ErrNotFound is returned for an unknown task id.
var ErrNotFound = errors.New("task not found")

// Store is the on-disk state directory.
type Store struct {
	dir string
	now func() time.Time
}

// DefaultDir returns ${XDG_STATE_HOME:-~/.local/state}/herdr-runner-kit.
func DefaultDir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate state: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "herdr-runner-kit"), nil
}

// Open prepares the state directory and brings it to the current schema.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "tasks"), dirPerm); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	// MkdirAll leaves an existing directory's mode alone; prompts live here.
	if err := os.Chmod(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("secure state dir: %w", err)
	}
	s := &Store{dir: dir, now: time.Now}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

// Dir returns the state directory.
func (s *Store) Dir() string { return s.dir }

// TaskDir returns the directory that holds one task's files.
func (s *Store) TaskDir(id string) string { return filepath.Join(s.dir, "tasks", id) }

// Create assigns the next id to t, stamps it and writes it. The caller fills
// every other field.
func (s *Store) Create(t *task.Task) error {
	id, err := s.nextID()
	if err != nil {
		return err
	}
	now := s.now().UTC()
	t.ID = id
	t.SchemaVersion = currentSchema
	t.CreatedAt, t.UpdatedAt = now, now
	if err := os.MkdirAll(s.TaskDir(id), dirPerm); err != nil {
		return fmt.Errorf("create task dir: %w", err)
	}
	return s.write(t)
}

// Get reads one task.
func (s *Store) Get(id string) (*task.Task, error) {
	data, err := os.ReadFile(filepath.Join(s.TaskDir(id), "task.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("read task %s: %w", id, err)
	}
	var t task.Task
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("parse task %s: %w", id, err)
	}
	if t.SchemaVersion > currentSchema {
		return nil, newerSchemaError("task "+id, t.SchemaVersion)
	}
	return &t, nil
}

// Update applies fn to the task under its lock and writes the result. If fn
// returns an error, nothing is written.
func (s *Store) Update(id string, fn func(*task.Task) error) (*task.Task, error) {
	if _, err := os.Stat(s.TaskDir(id)); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", id, ErrNotFound)
	}
	unlock, err := lock(filepath.Join(s.TaskDir(id), "task.lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()
	t, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if err := fn(t); err != nil {
		return nil, err
	}
	t.UpdatedAt = s.now().UTC()
	if err := s.write(t); err != nil {
		return nil, err
	}
	return t, nil
}

// SetStatus moves a task to status `to` only if its current status is one of
// `from` (any status when from is empty). A terminal status is never left, so
// a slow launcher writing "running" can not overwrite a fast runner's "done".
// It returns the task as stored and whether the status changed.
func (s *Store) SetStatus(id string, to task.Status, from ...task.Status) (*task.Task, bool, error) {
	return s.Transition(id, to, from, nil)
}

// Transition is SetStatus with extra changes applied in the same write when,
// and only when, the status changes. Reaching a terminal status stamps
// FinishedAt.
func (s *Store) Transition(id string, to task.Status, from []task.Status, mutate func(*task.Task)) (*task.Task, bool, error) {
	if !to.Valid() {
		return nil, false, fmt.Errorf("invalid status %q", to)
	}
	errUnchanged := errors.New("unchanged")
	t, err := s.Update(id, func(t *task.Task) error {
		if t.Status.Terminal() || t.Status == to {
			return errUnchanged
		}
		if len(from) > 0 && !slices.Contains(from, t.Status) {
			return errUnchanged
		}
		t.Status = to
		if to.Terminal() {
			t.FinishedAt = s.now().UTC()
		}
		if mutate != nil {
			mutate(t)
		}
		return nil
	})
	if errors.Is(err, errUnchanged) {
		current, getErr := s.Get(id)
		return current, false, getErr
	}
	if err != nil {
		return nil, false, err
	}
	return t, true, nil
}

// List returns every task, ordered by task number.
func (s *Store) List() ([]*task.Task, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "tasks"))
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	type numbered struct {
		n int
		t *task.Task
	}
	var found []numbered
	for _, e := range entries {
		id, n, err := task.ParseID(e.Name())
		if !e.IsDir() || err != nil || id != e.Name() {
			continue
		}
		t, err := s.Get(id)
		if errors.Is(err, ErrNotFound) {
			continue // created but not yet written
		}
		if err != nil {
			return nil, err
		}
		found = append(found, numbered{n, t})
	}
	slices.SortFunc(found, func(a, b numbered) int { return a.n - b.n })
	out := make([]*task.Task, len(found))
	for i, f := range found {
		out[i] = f.t
	}
	return out, nil
}

func (s *Store) nextID() (string, error) {
	unlock, err := lock(filepath.Join(s.dir, "seq.lock"))
	if err != nil {
		return "", err
	}
	defer unlock()
	path := filepath.Join(s.dir, "seq")
	n := 0
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return "", fmt.Errorf("read seq: %w", err)
	default:
		if n, err = strconv.Atoi(strings.TrimSpace(string(data))); err != nil {
			return "", fmt.Errorf("parse seq: %w", err)
		}
	}
	n++
	if err := writeFileAtomic(path, []byte(strconv.Itoa(n)+"\n")); err != nil {
		return "", err
	}
	return task.FormatID(n), nil
}

func (s *Store) write(t *task.Task) error {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("encode task %s: %w", t.ID, err)
	}
	return writeFileAtomic(filepath.Join(s.TaskDir(t.ID), "task.json"), append(data, '\n'))
}

func writeFileAtomic(path string, data []byte) error {
	if err := fsutil.WriteFileAtomic(path, data, filePerm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
