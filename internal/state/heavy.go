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
// The kernel drops a flock when its process dies, so a crashed holder frees
// its slot at once. Readers skip records whose pid is gone.

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
// all are taken, and returns the function that frees it. It fills rec's pid
// and start time. While it waits it lists rec as waiting and calls notify at
// once and every HeavyNotify. With slots 0 or less it never waits.
//
// ponytail: polling with no fairness; add a ticket queue if starvation is
// ever seen.
func (s *Store) AcquireHeavy(ctx context.Context, slots int, rec HeavyRecord, notify func(HeavyStatus)) (func(), error) {
	if slots <= 0 {
		return func() {}, nil
	}
	if err := os.MkdirAll(filepath.Join(s.heavyDir(), "waiting"), dirPerm); err != nil {
		return nil, fmt.Errorf("create heavy dir: %w", err)
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
			release, err := s.tryHeavySlot(n, rec)
			if err != nil || release != nil {
				return release, err
			}
		}
		now := time.Now()
		if !waiting {
			rec.Since = now
			if err := writeJSON(s.waitingPath(rec.PID), rec); err != nil {
				return nil, err
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
			return nil, ctx.Err()
		case <-time.After(HeavyPoll):
		}
	}
}

// tryHeavySlot takes slot n for rec if it is free. It returns a nil release
// when the slot is taken.
func (s *Store) tryHeavySlot(n int, rec HeavyRecord) (func(), error) {
	path := filepath.Join(s.heavyDir(), fmt.Sprintf("slot-%d.lock", n))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, filePerm)
	if err != nil {
		return nil, fmt.Errorf("open heavy slot: %w", err)
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
			return nil, nil
		}
		return nil, fmt.Errorf("lock heavy slot: %w", err)
	}
	rec.Slot, rec.Since = n, time.Now()
	holder := strings.TrimSuffix(path, ".lock") + ".json"
	if err := writeJSON(holder, rec); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = os.Remove(holder)
		_ = f.Close() // closing the last fd drops the flock
	}, nil
}

// HeavyStatus reads the live holders of slots 1 to slots and the live
// waiters.
func (s *Store) HeavyStatus(slots int) (HeavyStatus, error) {
	st := HeavyStatus{Slots: slots, Holders: []HeavyRecord{}, Waiting: []HeavyRecord{}}
	for n := 1; n <= slots; n++ {
		if rec, ok := readHeavy(filepath.Join(s.heavyDir(), fmt.Sprintf("slot-%d.json", n))); ok {
			st.Holders = append(st.Holders, rec)
		}
	}
	paths, err := filepath.Glob(filepath.Join(s.heavyDir(), "waiting", "*.json"))
	if err != nil {
		return st, err
	}
	for _, p := range paths {
		if rec, ok := readHeavy(p); ok {
			st.Waiting = append(st.Waiting, rec)
		}
	}
	slices.SortFunc(st.Waiting, func(a, b HeavyRecord) int { return a.Since.Compare(b.Since) })
	return st, nil
}

// HeavyWaiting reports whether a live process of task id waits for a heavy
// command slot.
func (s *Store) HeavyWaiting(id string) bool {
	paths, _ := filepath.Glob(filepath.Join(s.heavyDir(), "waiting", "*.json"))
	for _, p := range paths {
		if rec, ok := readHeavy(p); ok && rec.Task == id {
			return true
		}
	}
	return false
}

// readHeavy reads one record, and reports false for a missing or unreadable
// file or a process that is gone.
func readHeavy(path string) (HeavyRecord, bool) {
	var rec HeavyRecord
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &rec) != nil || !alive(rec.PID) {
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
