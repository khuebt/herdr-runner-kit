package harness

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func TestMainUsageSupport(t *testing.T) {
	path := filepath.Join("testdata", "transcript-claude", "s1.jsonl")
	for _, h := range []task.Harness{"", task.Claude, task.Pi, task.Omp, task.Codex} {
		u, supported, err := MainUsage(h, path, time.Time{}, time.Time{})
		want := h == "" || h == task.Claude
		if err != nil || supported != want || (u != nil) != want {
			t.Fatalf("%q: usage=%+v, supported=%v, err=%v", h, u, supported, err)
		}
		if want && u.Calls != 4 {
			t.Errorf("%q: calls=%d", h, u.Calls)
		}
	}
	if _, _, err := MainUsage("unknown", path, time.Time{}, time.Time{}); err == nil {
		t.Fatal("unknown harness accepted")
	}
	u, supported, err := MainUsage("", filepath.Join(t.TempDir(), "missing"), time.Time{}, time.Time{})
	if !supported || u != nil || err == nil {
		t.Fatalf("missing transcript confused with unsupported: %+v, %v, %v", u, supported, err)
	}
}
