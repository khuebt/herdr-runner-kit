package fsutil

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// Lock takes an exclusive flock on path and returns the function that
// releases it. A folder is opened read-only, so no lock file is left in a
// folder another program owns; any other path is a lock file, created if
// needed. flock locks are released by the kernel when the process dies, so
// a crashed command never leaves anything locked. The path must never be
// renamed or removed while it is held, or the next opener locks a new file.
func Lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if errors.Is(err, syscall.EISDIR) {
		f, err = os.Open(path)
	}
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// updateAttempts bounds how often Update writes a change again after
// another program rewrote the file over it.
const updateAttempts = 3

// UpdateWritten runs between Update's write and its check of the result;
// tests rewrite the file there the way another program may.
var UpdateWritten = func(string) {}

// Update rewrites path under a lock on lockAt: it reads the file (nil when
// it is missing), hands it to edit, and writes what edit returns with
// WriteFileAtomic, keeping the file's mode (perm for a new file). An edit
// that returns nil or its input unchanged writes nothing. A non-nil landed
// marks a file another program writes without the lock: the file is read
// again inside the lock, and the whole attempt runs again while landed
// rejects it, up to 3 times.
func Update(path, lockAt string, perm fs.FileMode, edit func(old []byte) ([]byte, error), landed func([]byte) bool) error {
	unlock, err := Lock(lockAt)
	if err != nil {
		return err
	}
	defer unlock()
	for range updateAttempts {
		old, err := os.ReadFile(path)
		mode := perm
		switch {
		case errors.Is(err, fs.ErrNotExist):
			old = nil
		case err != nil:
			return err
		default:
			if info, err := os.Stat(path); err == nil {
				mode = info.Mode().Perm()
			}
		}
		next, err := edit(old)
		if err != nil {
			return err
		}
		if next == nil || old != nil && bytes.Equal(next, old) {
			return nil
		}
		if err := WriteFileAtomic(path, next, mode); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		if landed == nil {
			return nil
		}
		UpdateWritten(path)
		got, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if landed(got) {
			return nil
		}
	}
	return fmt.Errorf("%s was rewritten over the change %d times", path, updateAttempts)
}

// CreateExclusive writes data to path only when nothing is there yet. The
// file appears whole or not at all: a temporary file is linked to path,
// which fails when path exists. It reports whether it created the file.
func CreateExclusive(path string, data []byte, perm fs.FileMode) (bool, error) {
	tmp, err := writeTemp(path, data, perm)
	if err != nil {
		return false, err
	}
	defer func() { _ = os.Remove(tmp) }()
	if err := os.Link(tmp, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
