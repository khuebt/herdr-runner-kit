package state

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/fsutil"
)

// Prompt tokens tell the prompt gate of a runner with locked input that a
// submitted prompt came from the kit. <task dir>/prompt-tokens holds one
// expiry per line, in Unix nanoseconds: the kit adds one just before it
// types a prompt, and the gate takes one for each submit it lets through.
// A token, not the prompt's text, because the agent rewrites what it gets:
// a pasted prompt arrives wrapped, with its tabs turned into spaces. An
// expired token stays on file until a submit gets through, since its
// prompt never reached the gate and may still sit in the input box.

// PromptTokenTTL is how long a token lets a submit through.
const PromptTokenTTL = 30 * time.Second

// AddPromptToken lets one more prompt into task id's runner within
// PromptTokenTTL.
func (s *Store) AddPromptToken(id string) error {
	_, err := s.promptTokens(id, func(now time.Time, live, stale []time.Time) ([]time.Time, []time.Time, bool) {
		return append(live, now.Add(PromptTokenTTL)), stale, true
	})
	return err
}

// TakePromptToken uses up the oldest valid token of task id, and reports
// whether there was one. A submit it lets through carries whatever sat in
// the input box, so it also drops the expired ones.
func (s *Store) TakePromptToken(id string) (bool, error) {
	return s.promptTokens(id, func(_ time.Time, live, stale []time.Time) ([]time.Time, []time.Time, bool) {
		if len(live) == 0 {
			return live, stale, false
		}
		return live[1:], nil, true
	})
}

// ReturnPromptToken takes back the newest valid token of task id, for a
// prompt the kit failed to type, and keeps the expired ones.
func (s *Store) ReturnPromptToken(id string) error {
	_, err := s.promptTokens(id, func(_ time.Time, live, stale []time.Time) ([]time.Time, []time.Time, bool) {
		return live[:max(len(live)-1, 0)], stale, true
	})
	return err
}

// PromptPending reports whether task id holds a token no submit used,
// expired or not: the kit's last prompt never reached the gate, so its
// text may still sit in the agent's input box.
func (s *Store) PromptPending(id string) (bool, error) {
	return s.promptTokens(id, func(_ time.Time, live, stale []time.Time) ([]time.Time, []time.Time, bool) {
		return live, stale, len(live)+len(stale) > 0
	})
}

// SetPendingPrompt keeps text, the kit prompt that may sit in task id's
// input box, for the next prompt to compare the box with.
func (s *Store) SetPendingPrompt(id, text string) error {
	return fsutil.WriteFileAtomic(filepath.Join(s.TaskDir(id), "prompt-pending"), []byte(text), filePerm)
}

// PendingPrompt returns the text SetPendingPrompt kept, empty when none.
func (s *Store) PendingPrompt(id string) (string, error) {
	data, err := os.ReadFile(filepath.Join(s.TaskDir(id), "prompt-pending"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return string(data), err
}

// promptTokens reads task id's valid and expired tokens, each oldest
// first, under the token lock, and writes back what change returns.
func (s *Store) promptTokens(id string, change func(now time.Time, live, stale []time.Time) ([]time.Time, []time.Time, bool)) (bool, error) {
	unlock, err := s.LockFile(id, "prompt-tokens.lock")
	if err != nil {
		return false, err
	}
	defer unlock()
	path := filepath.Join(s.TaskDir(id), "prompt-tokens")
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	now := s.now()
	var live, stale []time.Time
	for line := range strings.SplitSeq(string(data), "\n") {
		n, err := strconv.ParseInt(strings.TrimSpace(line), 10, 64)
		switch {
		case err != nil:
		case time.Unix(0, n).After(now):
			live = append(live, time.Unix(0, n))
		default:
			stale = append(stale, time.Unix(0, n))
		}
	}
	slices.SortFunc(live, time.Time.Compare)
	slices.SortFunc(stale, time.Time.Compare)
	live, stale, found := change(now, live, stale)
	var b strings.Builder
	for _, exp := range slices.Concat(stale, live) {
		b.WriteString(strconv.FormatInt(exp.UnixNano(), 10) + "\n")
	}
	return found, fsutil.WriteFileAtomic(path, []byte(b.String()), filePerm)
}
