package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/fsutil"
)

// Heavy command slots live in <dir>/heavy:
//
//	slot-<n>.lock       flock held by the hrk heavy process running in slot n
//	slot-<n>.json       who holds slot n
//	waiting/<pid>.json  one hrk heavy process waiting for a slot
//
// The kernel drops a flock when the last fd of its open file goes, so a
// crashed holder frees its slot at once, unless the command it runs got the
// fd: then the slot stays held until that command exits too. Readers list a
// holder while its slot's flock is held, whatever its pid, and drop the
// records of waiters whose pid is gone. The holder's record carries the
// command's pid as well. A slot whose flock is held while both are gone is
// held by a leftover process, such as a daemon the command started, and is
// listed as orphaned: only the kernel can drop a flock another process
// holds, so such a slot is not treated as free.

// HeavyPoll is how often a waiter tries the slots again, and HeavyNotify
// how often it says it still waits. Variables so tests can shorten them.
var (
	HeavyPoll   = time.Second
	HeavyNotify = time.Minute
)

// HeavyRecord is one holder or waiter of a heavy command slot.
type HeavyRecord struct {
	Slot    int       `json:"slot,omitempty"`
	Task    string    `json:"task"` // the runner's task id, or main
	PID     int       `json:"pid"`
	Cwd     string    `json:"cwd"`
	Command string    `json:"command"`
	Since   time.Time `json:"since"`
	// CommandPID is the pid of the command a holder runs, once started.
	CommandPID int `json:"commandPid,omitempty"`
	// Orphaned is set on a listed holder whose pid and command pid are
	// both gone while the slot's flock is still held.
	Orphaned bool `json:"orphaned,omitempty"`
}

// HeavyStatus lists the live holders, by slot, and the live waiters, oldest
// first.
type HeavyStatus struct {
	Slots   int           `json:"slots"`
	Holders []HeavyRecord `json:"holders"`
	Waiting []HeavyRecord `json:"waiting"`
}

func (s *Store) heavyDir() string { return filepath.Join(s.dir, "heavy") }

func (s *Store) waitingPath(pid int) string {
	return filepath.Join(s.heavyDir(), "waiting", strconv.Itoa(pid)+".json")
}

// AcquireHeavy takes one of slots heavy command slots for rec, waiting while
// all are taken, and returns the function that frees it and the slot's
// locked file. A command given that file holds the slot until it exits,
// even when the caller dies first; release frees the slot for it too. It
// fills rec's pid and start time. While it waits it lists rec as waiting
// and calls notify at once and every HeavyNotify. With slots 0 or less it
// never waits, and the file is nil.
//
// ponytail: polling with no fairness; add a ticket queue if starvation is
// ever seen.
func (s *Store) AcquireHeavy(ctx context.Context, slots int, rec HeavyRecord, notify func(HeavyStatus)) (release func(), lock *os.File, err error) {
	if slots <= 0 {
		return func() {}, nil, nil
	}
	if err := os.MkdirAll(filepath.Join(s.heavyDir(), "waiting"), dirPerm); err != nil {
		return nil, nil, fmt.Errorf("create heavy dir: %w", err)
	}
	rec.PID = os.Getpid()
	var notified time.Time
	waiting := false
	defer func() {
		if waiting {
			_ = os.Remove(s.waitingPath(rec.PID))
		}
	}()
	for {
		for n := 1; n <= slots; n++ {
			release, lock, err := s.tryHeavySlot(n, rec)
			if err != nil || release != nil {
				return release, lock, err
			}
		}
		now := time.Now()
		if !waiting {
			rec.Since = now
			if err := writeJSON(s.waitingPath(rec.PID), rec); err != nil {
				return nil, nil, err
			}
			waiting = true
		}
		if notify != nil && now.Sub(notified) >= HeavyNotify {
			notified = now
			if st, err := s.HeavyStatus(slots); err == nil {
				notify(st)
			}
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(HeavyPoll):
		}
	}
}

// RecordHeavyCommand adds the pid of the command running with lock, the file
// AcquireHeavy returned, to its holder record.
func (s *Store) RecordHeavyCommand(lock *os.File, pid int) error {
	path := strings.TrimSuffix(lock.Name(), ".lock") + ".json"
	rec, ok := readHeavy(path)
	if !ok {
		return fmt.Errorf("read %s", filepath.Base(path))
	}
	rec.CommandPID = pid
	return writeJSON(path, rec)
}

// tryHeavySlot takes slot n for rec if it is free. It returns a nil release
// when the slot is taken.
func (s *Store) tryHeavySlot(n int, rec HeavyRecord) (func(), *os.File, error) {
	f, ok, err := s.lockHeavySlot(n)
	if err != nil || !ok {
		return nil, nil, err
	}
	rec.Slot, rec.Since = n, time.Now()
	holder := s.slotPath(n, ".json")
	if err := writeJSON(holder, rec); err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return func() {
		_ = os.Remove(holder)
		// Unlocking frees the slot also for a command that still has the
		// file open, such as a daemon the heavy command left behind.
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, f, nil
}

func (s *Store) slotPath(n int, ext string) string {
	return filepath.Join(s.heavyDir(), fmt.Sprintf("slot-%d%s", n, ext))
}

// lockHeavySlot opens slot n's lock file and takes its flock without
// waiting. It reports false, with no file, when the slot is taken.
func (s *Store) lockHeavySlot(n int) (*os.File, bool, error) {
	f, err := os.OpenFile(s.slotPath(n, ".lock"), os.O_CREATE|os.O_RDWR, filePerm)
	if err != nil {
		return nil, false, fmt.Errorf("open heavy slot: %w", err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("lock heavy slot: %w", err)
	}
	return f, true, nil
}

// HeavyStatus reads the holders of slots 1 to slots and the live waiters.
func (s *Store) HeavyStatus(slots int) (HeavyStatus, error) {
	st := HeavyStatus{Slots: slots, Holders: []HeavyRecord{}, Waiting: []HeavyRecord{}}
	for n := 1; n <= slots; n++ {
		if rec, ok := s.heavyHolder(n); ok {
			st.Holders = append(st.Holders, rec)
		}
	}
	waiting, err := s.heavyWaiters()
	st.Waiting = waiting
	return st, err
}

// heavyHolder reads who holds slot n. The slot's flock, not the holder's
// pid, says whether it is held: a killed holder's command may hold it
// still. A free slot's leftover record is removed. A held slot whose holder
// and command are both gone is marked orphaned.
func (s *Store) heavyHolder(n int) (HeavyRecord, bool) {
	path := s.slotPath(n, ".json")
	f, free, err := s.lockHeavySlot(n)
	if err != nil {
		return HeavyRecord{}, false
	}
	if free {
		_ = os.Remove(path)
		_ = f.Close()
		return HeavyRecord{}, false
	}
	rec, ok := readHeavy(path)
	rec.Orphaned = ok && rec.CommandPID > 0 && !alive(rec.PID) && !alive(rec.CommandPID)
	return rec, ok
}

// heavyWaiters reads the live waiters, oldest first, and removes the
// records of waiters that are gone, so a reused pid never revives one.
func (s *Store) heavyWaiters() ([]HeavyRecord, error) {
	waiting := []HeavyRecord{}
	paths, err := filepath.Glob(filepath.Join(s.heavyDir(), "waiting", "*.json"))
	if err != nil {
		return waiting, err
	}
	for _, p := range paths {
		rec, ok := readHeavy(p)
		if ok && !alive(rec.PID) {
			_ = os.Remove(p)
			ok = false
		}
		if ok {
			waiting = append(waiting, rec)
		}
	}
	slices.SortFunc(waiting, func(a, b HeavyRecord) int { return a.Since.Compare(b.Since) })
	return waiting, nil
}

// HeavyWaiting reports whether a live process of task id waits for a heavy
// command slot.
func (s *Store) HeavyWaiting(id string) bool {
	waiting, _ := s.heavyWaiters()
	return slices.ContainsFunc(waiting, func(rec HeavyRecord) bool { return rec.Task == id })
}

// readHeavy reads one record, and reports false for a missing or unreadable
// file.
func readHeavy(path string) (HeavyRecord, bool) {
	var rec HeavyRecord
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &rec) != nil {
		return rec, false
	}
	return rec, true
}

// alive reports whether process pid exists. EPERM means it exists under
// another user.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err == nil {
		err = fsutil.WriteFileAtomic(path, data, filePerm)
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}
