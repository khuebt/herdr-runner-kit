package state

import (
	"path/filepath"

	"github.com/khuebt/herdr-runner-kit/internal/fsutil"
)

// lock takes an exclusive flock on the lock file at path, creating it if
// needed, and returns the function that releases it.
func lock(path string) (func(), error) { return fsutil.Lock(path) }

// Lock takes the store-wide lock name, blocking until it is free, and
// returns the function that releases it. It serialises work that spans
// several tasks, such as sending a chain's held-back news once.
func (s *Store) Lock(name string) (func(), error) {
	return lock(filepath.Join(s.dir, name+".lock"))
}

// LockFile takes the lock file name in task id's folder, blocking until it
// is free, and returns the function that releases it.
func (s *Store) LockFile(id, name string) (func(), error) {
	return lock(filepath.Join(s.TaskDir(id), name))
}
