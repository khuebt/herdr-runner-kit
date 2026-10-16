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
// a pasted prompt arrives wrapped, with its tabs turned into spaces.

// PromptTokenTTL is how long a token lets a submit through.
const PromptTokenTTL = 30 * time.Second

// AddPromptToken lets one more prompt into task id's runner within
// PromptTokenTTL.
func (s *Store) AddPromptToken(id string) error {
	_, err := s.promptTokens(id, func(now time.Time, live []time.Time) ([]time.Time, bool) {
		return append(live, now.Add(PromptTokenTTL)), true
	})
	return err
}

// TakePromptToken uses up the oldest valid token of task id, dropping the
// expired ones, and reports whether there was one.
func (s *Store) TakePromptToken(id string) (bool, error) {
	return s.promptTokens(id, func(_ time.Time, live []time.Time) ([]time.Time, bool) {
		if len(live) == 0 {
			return live, false
		}
		return live[1:], true
	})
}

// promptTokens reads task id's valid tokens, oldest first, under the token
// lock, and writes back what change returns.
func (s *Store) promptTokens(id string, change func(now time.Time, live []time.Time) ([]time.Time, bool)) (bool, error) {
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
	var live []time.Time
	for line := range strings.SplitSeq(string(data), "\n") {
		if n, err := strconv.ParseInt(strings.TrimSpace(line), 10, 64); err == nil && time.Unix(0, n).After(now) {
			live = append(live, time.Unix(0, n))
		}
	}
	slices.SortFunc(live, time.Time.Compare)
	live, found := change(now, live)
	var b strings.Builder
	for _, exp := range live {
		b.WriteString(strconv.FormatInt(exp.UnixNano(), 10) + "\n")
	}
	return found, fsutil.WriteFileAtomic(path, []byte(b.String()), filePerm)
}
