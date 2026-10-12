package state

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ErrAlreadyWaiting is returned when another wait holds the cursor.
var ErrAlreadyWaiting = errors.New("already waiting")

// CursorName returns the default cursor name for a main session's owner, so
// main sessions in different repositories never share a cursor.
func CursorName(owner string) string {
	sum := sha256.Sum256([]byte(owner))
	return hex.EncodeToString(sum[:6])
}

func (s *Store) cursorPath(name string) string {
	return filepath.Join(s.dir, "cursor-"+name)
}

// Cursor returns the saved event log offset of a watcher. ok is false when
// the watcher has never saved one.
func (s *Store) Cursor(name string) (offset int64, ok bool, err error) {
	data, err := os.ReadFile(s.cursorPath(name))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read cursor: %w", err)
	}
	offset, err = strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("parse cursor %s: %w", name, err)
	}
	return offset, true, nil
}

// SaveCursor stores a watcher's event log offset.
func (s *Store) SaveCursor(name string, offset int64) error {
	return writeFileAtomic(s.cursorPath(name), []byte(strconv.FormatInt(offset, 10)+"\n"))
}

// InitCursor saves the current end of the log as the cursor of a watcher
// that has none yet. spawn calls it before writing the task's first event, so
// a main session's first wait sees everything its tasks report.
func (s *Store) InitCursor(name string) error {
	if _, ok, err := s.Cursor(name); err != nil || ok {
		return err
	}
	end, err := s.EventsEnd()
	if err != nil {
		return err
	}
	return s.SaveCursor(name, end)
}

// EventsEnd returns the size of the event log.
func (s *Store) EventsEnd() (int64, error) {
	info, err := os.Stat(s.EventsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// LockCursor takes the watcher's lock without blocking. It returns
// ErrAlreadyWaiting when another wait holds it.
func (s *Store) LockCursor(name string) (func(), error) {
	f, err := os.OpenFile(s.cursorPath(name)+".lock", os.O_CREATE|os.O_RDWR, filePerm)
	if err != nil {
		return nil, fmt.Errorf("open cursor lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyWaiting
		}
		return nil, fmt.Errorf("lock cursor: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
