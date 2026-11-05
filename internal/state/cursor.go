package state

import (
	"cmp"
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
	"time"
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

// Retention of RecordMainTranscript. Main sessions are grouped by the Claude
// project folder of their transcript, so a folder's newest main session is
// never pushed out by main sessions of other folders: each folder keeps its
// newest few, sessions without a transcript share one group of their own,
// and mainSessionsMax only bounds the whole file. The auto re-arm hook moves
// a main session's id to the end on every turn, so only a main session idle
// while others of its own folder start falls out.
const (
	mainSessionsKept      = 8  // sessions without a transcript
	mainSessionsPerFolder = 4  // the newest of a folder plus three older ones
	mainSessionsMax       = 64 // safety bound over all folders
)

func (s *Store) mainSessionsPath() string { return filepath.Join(s.dir, mainSessionsFile) }

const mainSessionsFile = "main-sessions"

// MainSession is a recorded main session: its Claude session id and, when
// the hook named them, its transcript file and the owner its hook watches.
type MainSession struct {
	ID         string
	Transcript string
	Owner      string
}

// MainSessions returns the recorded main sessions, oldest first. The file
// holds one per line: the id, then the transcript and the owner, each after
// a tab; lines from before transcripts or owners were kept stop early.
func (s *Store) MainSessions() []MainSession {
	return readMainSessions(s.mainSessionsPath())
}

func readMainSessions(path string) []MainSession {
	data, _ := os.ReadFile(path)
	var out []MainSession
	for line := range strings.SplitSeq(string(data), "\n") {
		id, rest, _ := strings.Cut(line, "\t")
		transcript, owner, _ := strings.Cut(rest, "\t")
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, MainSession{ID: id, Transcript: strings.TrimSpace(transcript), Owner: strings.TrimSpace(owner)})
		}
	}
	return out
}

// RecordMainSession remembers the Claude session id of a main session,
// newest last, so the auto re-arm hook still knows that session after a
// resume that lost its environment. Empty ids are ignored.
func (s *Store) RecordMainSession(id string) error { return s.RecordMainTranscript(id, "", "") }

// RecordMainTranscript records a main session as RecordMainSession does,
// with its transcript, so usage can read it, and the owner its hook
// watches, so spawn can report to it from any folder. An empty transcript or
// owner keeps the one recorded before.
func (s *Store) RecordMainTranscript(id, transcript, owner string) error {
	if id == "" {
		return nil
	}
	unlock, err := s.Lock("main-sessions")
	if err != nil {
		return err
	}
	defer unlock()
	sessions := s.MainSessions()
	if n := len(sessions); n > 0 && sessions[n-1].ID == id && (transcript == "" || sessions[n-1].Transcript == transcript) &&
		(owner == "" || sessions[n-1].Owner == owner) {
		return nil
	}
	sessions = slices.DeleteFunc(sessions, func(m MainSession) bool {
		if m.ID == id {
			transcript, owner = cmp.Or(transcript, m.Transcript), cmp.Or(owner, m.Owner)
		}
		return m.ID == id
	})
	sessions = pruneMainSessions(append(sessions, MainSession{ID: id, Transcript: transcript, Owner: owner}))
	var b strings.Builder
	for _, m := range sessions {
		b.WriteString(m.ID)
		if m.Transcript != "" || m.Owner != "" {
			b.WriteString("\t" + m.Transcript)
		}
		if m.Owner != "" {
			b.WriteString("\t" + m.Owner)
		}
		b.WriteString("\n")
	}
	return writeFileAtomic(s.mainSessionsPath(), []byte(b.String()))
}

// pruneMainSessions drops, from sessions (oldest first), what the retention
// constants leave out, newest first, and keeps the order.
func pruneMainSessions(sessions []MainSession) []MainSession {
	perGroup := map[string]int{}
	keep := make([]bool, len(sessions))
	total := 0
	for i := len(sessions) - 1; i >= 0 && total < mainSessionsMax; i-- {
		group, limit := "", mainSessionsKept
		if t := sessions[i].Transcript; t != "" {
			group, limit = filepath.Dir(t), mainSessionsPerFolder
		}
		if perGroup[group]++; perGroup[group] <= limit {
			keep[i] = true
			total++
		}
	}
	var out []MainSession
	for i, m := range sessions {
		if keep[i] {
			out = append(out, m)
		}
	}
	return out
}

// MainSessionOwner returns the owner recorded for main session id, or "".
func (s *Store) MainSessionOwner(id string) string {
	for _, m := range s.MainSessions() {
		if id != "" && m.ID == id {
			return m.Owner
		}
	}
	return ""
}

// IsMainSession reports whether RecordMainSession recorded id.
func (s *Store) IsMainSession(id string) bool {
	return MainSessionListed(s.dir, id)
}

// MainSessionListed reports whether RecordMainSession recorded id in the
// state folder dir. It only reads, so a hook can ask before it opens the
// state.
func MainSessionListed(dir, id string) bool {
	return id != "" && slices.ContainsFunc(readMainSessions(filepath.Join(dir, mainSessionsFile)), func(m MainSession) bool { return m.ID == id })
}

func (s *Store) mainProjectsPath(owner string) string {
	return filepath.Join(s.dir, "main-projects-"+CursorName(owner))
}

// MainProject is a project attached to a main session, and when.
type MainProject struct {
	Name  string
	Since time.Time
}

// MainProjects returns the names of the projects hrk main --project attached
// to the main session of owner, in the order given.
func (s *Store) MainProjects(owner string) []string {
	var names []string
	for _, p := range s.MainProjectsSince(owner) {
		names = append(names, p.Name)
	}
	return names
}

// MainProjectsSince returns the projects attached to the main session of
// owner with when each was attached. The file holds one per line, the name
// and then the time after a tab; a line without a time reads as attached
// from the start.
func (s *Store) MainProjectsSince(owner string) []MainProject {
	data, _ := os.ReadFile(s.mainProjectsPath(owner))
	var out []MainProject
	for line := range strings.SplitSeq(string(data), "\n") {
		name, at, _ := strings.Cut(line, "\t")
		if name = strings.TrimSpace(name); name != "" {
			p := MainProject{Name: name}
			p.Since, _ = time.Parse(time.RFC3339Nano, strings.TrimSpace(at))
			out = append(out, p)
		}
	}
	return out
}

// SetMainProjects replaces the projects attached to the main session of
// owner. A project already attached keeps its time; the others are
// attached now.
func (s *Store) SetMainProjects(owner string, names []string) error {
	unlock, err := s.Lock("main-projects-" + CursorName(owner))
	if err != nil {
		return err
	}
	defer unlock()
	old := s.MainProjectsSince(owner)
	var b strings.Builder
	for _, name := range names {
		at := s.now().UTC()
		if i := slices.IndexFunc(old, func(p MainProject) bool { return p.Name == name }); i >= 0 {
			at = old[i].Since
		}
		b.WriteString(name + "\t" + at.Format(time.RFC3339Nano) + "\n")
	}
	return writeFileAtomic(s.mainProjectsPath(owner), []byte(b.String()))
}
