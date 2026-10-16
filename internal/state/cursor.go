package state

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// ErrAlreadyWaiting is returned when another wait holds the cursor.
var ErrAlreadyWaiting = errors.New("already waiting")

// HeldError is LockCursor's error when another process holds the lock. It
// wraps ErrAlreadyWaiting and names the holder written in the lock file;
// Holder is empty when the file names none, as with an older binary.
type HeldError struct {
	Holder string
	PID    int
}

func (e *HeldError) Error() string {
	if e.Holder == "" {
		return ErrAlreadyWaiting.Error()
	}
	return fmt.Sprintf("%s: held by %s (pid %d)", ErrAlreadyWaiting, e.Holder, e.PID)
}

func (e *HeldError) Unwrap() error { return ErrAlreadyWaiting }

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

// LockCursor takes the watcher's lock without blocking and writes holder and
// this process's PID into the lock file. When another process holds it, it
// returns a *HeldError naming that holder, which wraps ErrAlreadyWaiting.
func (s *Store) LockCursor(name, holder string) (func(), error) {
	f, err := os.OpenFile(s.cursorPath(name)+".lock", os.O_CREATE|os.O_RDWR, filePerm)
	if err != nil {
		return nil, fmt.Errorf("open cursor lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			held := &HeldError{}
			// ponytail: a holder that has not written yet reads as unnamed.
			if data, err := os.ReadFile(f.Name()); err == nil {
				if who, pid, ok := strings.Cut(strings.TrimSpace(string(data)), " "); ok {
					held.Holder = who
					held.PID, _ = strconv.Atoi(pid)
				}
			}
			return nil, held
		}
		return nil, fmt.Errorf("lock cursor: %w", err)
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(holder+" "+strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// mainSessionsKept is how many main session ids RecordMainSession keeps, so
// a nested claude started from the main session cannot push its id out.
const mainSessionsKept = 8

func (s *Store) mainSessionsPath() string { return filepath.Join(s.dir, mainSessionsFile) }

const mainSessionsFile = "main-sessions"

func (s *Store) mainSessions() []string {
	data, _ := os.ReadFile(s.mainSessionsPath())
	return strings.Fields(string(data))
}

// RecordMainSession remembers the Claude session id of a main session, newest
// last, so the auto re-arm hook still knows that session after a resume that
// lost its environment. Empty ids are ignored.
func (s *Store) RecordMainSession(id string) error {
	if id == "" {
		return nil
	}
	unlock, err := s.Lock("main-sessions")
	if err != nil {
		return err
	}
	defer unlock()
	ids := s.mainSessions()
	if len(ids) > 0 && ids[len(ids)-1] == id {
		return nil
	}
	ids = append(slices.DeleteFunc(ids, func(x string) bool { return x == id }), id)
	ids = ids[max(0, len(ids)-mainSessionsKept):]
	return writeFileAtomic(s.mainSessionsPath(), []byte(strings.Join(ids, "\n")+"\n"))
}

// IsMainSession reports whether RecordMainSession recorded id.
func (s *Store) IsMainSession(id string) bool {
	return MainSessionListed(s.dir, id)
}

// MainSessionListed reports whether RecordMainSession recorded id in the
// state folder dir. It only reads, so a hook can ask before it opens the
// state.
func MainSessionListed(dir, id string) bool {
	data, _ := os.ReadFile(filepath.Join(dir, mainSessionsFile))
	return id != "" && slices.Contains(strings.Fields(string(data)), id)
}
