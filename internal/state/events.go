package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// EventVersion is the version of one event line, written as "v".
const EventVersion = 1

// Event is one line of events.jsonl. Owner is copied from the task so that a
// main session's wait can filter the log without reading every task file.
type Event struct {
	V        int         `json:"v"`
	TS       time.Time   `json:"ts"`
	Task     string      `json:"task,omitempty"`
	Owner    string      `json:"owner,omitempty"`
	Type     string      `json:"type"`
	Status   task.Status `json:"status,omitempty"`
	Summary  string      `json:"summary,omitempty"`
	Output   string      `json:"output,omitempty"`
	Question string      `json:"question,omitempty"`
}

// EventsPath returns the path of the global event log.
func (s *Store) EventsPath() string { return filepath.Join(s.dir, "events.jsonl") }

// Append writes one event as a single line. The write happens under an flock
// on the log itself with O_APPEND, so concurrent writers never interleave.
func (s *Store) Append(ev Event) error {
	ev.V = EventVersion
	if ev.TS.IsZero() {
		ev.TS = s.now().UTC()
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	line = append(line, '\n')

	f, err := os.OpenFile(s.EventsPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return fmt.Errorf("open event log: %w", err)
	}
	defer func() { _ = f.Close() }()
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("lock event log: %w", err)
	}
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	return nil
}

// ReadEventsSince returns the complete events written at or after byte offset
// and the offset just past the last complete line. A trailing partial line is
// left for the next call. A line that does not parse is skipped, so one bad
// line can never wedge every reader.
func (s *Store) ReadEventsSince(offset int64) ([]Event, int64, error) {
	f, err := os.Open(s.EventsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, offset, nil
	}
	if err != nil {
		return nil, offset, fmt.Errorf("open event log: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, fmt.Errorf("seek event log: %w", err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, offset, fmt.Errorf("read event log: %w", err)
	}
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		return nil, offset, nil
	}
	var events []Event
	for line := range bytes.SplitSeq(data[:end], []byte{'\n'}) {
		var ev Event
		if len(line) == 0 || json.Unmarshal(line, &ev) != nil {
			continue
		}
		events = append(events, ev)
	}
	return events, offset + int64(end) + 1, nil
}
