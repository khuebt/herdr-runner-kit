package harness

import (
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// MainUsage reads main-session usage through its adapter. Empty harness means
// legacy Claude; supported distinguishes missing usage from an absent parser.
func MainUsage(h task.Harness, transcript string, from, to time.Time) (usage *task.Usage, supported bool, err error) {
	if h == "" {
		h = task.Claude
	}
	a, err := For(h)
	if err != nil {
		return nil, false, err
	}
	if h != task.Claude {
		return nil, false, nil
	}
	u, err := a.Usage([]string{transcript}, from, to)
	return u, true, err
}
