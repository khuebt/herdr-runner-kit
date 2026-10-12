package state

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func TestReadEventsSinceEmptyLog(t *testing.T) {
	s := openStore(t)
	evs, off, err := s.ReadEventsSince(0)
	if err != nil || len(evs) != 0 || off != 0 {
		t.Fatalf("got %v, %d, %v", evs, off, err)
	}
}

func TestAppendAndReadWithCursor(t *testing.T) {
	s := openStore(t)
	if err := s.Append(Event{Task: "t1", Owner: "/repo", Type: "spawned"}); err != nil {
		t.Fatal(err)
	}
	evs, off, err := s.ReadEventsSince(0)
	if err != nil || len(evs) != 1 || evs[0].Type != "spawned" || evs[0].V != EventVersion || evs[0].TS.IsZero() {
		t.Fatalf("first read: %+v, %v", evs, err)
	}
	if err := s.Append(Event{Task: "t1", Type: "report", Status: task.Done, Summary: "ok"}); err != nil {
		t.Fatal(err)
	}
	evs, off2, err := s.ReadEventsSince(off)
	if err != nil || len(evs) != 1 || evs[0].Status != task.Done || off2 <= off {
		t.Fatalf("second read: %+v, %d, %v", evs, off2, err)
	}
	evs, off3, _ := s.ReadEventsSince(off2)
	if len(evs) != 0 || off3 != off2 {
		t.Fatalf("third read: %+v, %d", evs, off3)
	}
}

func TestPartialLineWaitsForNextRead(t *testing.T) {
	s := openStore(t)
	if err := s.Append(Event{Task: "t1", Type: "spawned"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(s.EventsPath(), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"v":1,"type":"half`)
	_ = f.Close()

	evs, off, _ := s.ReadEventsSince(0)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want only the complete one", len(evs))
	}
	f, _ = os.OpenFile(s.EventsPath(), os.O_WRONLY|os.O_APPEND, 0)
	_, _ = f.WriteString("\"}\n")
	_ = f.Close()
	evs, _, _ = s.ReadEventsSince(off)
	if len(evs) != 1 || evs[0].Type != "half" {
		t.Fatalf("completed line: %+v", evs)
	}
}

func TestBadLineIsSkipped(t *testing.T) {
	s := openStore(t)
	if err := os.WriteFile(s.EventsPath(), []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Event{Task: "t1", Type: "spawned"}); err != nil {
		t.Fatal(err)
	}
	evs, _, err := s.ReadEventsSince(0)
	if err != nil || len(evs) != 1 {
		t.Fatalf("got %+v, %v", evs, err)
	}
}

func TestConcurrentAppendsNeverInterleave(t *testing.T) {
	s := openStore(t)
	long := strings.Repeat("x", 8000) // larger than one pipe buffer write
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			ev := Event{Task: fmt.Sprintf("t%d", i+1), Type: "report", Summary: long}
			if err := s.Append(ev); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	data, err := os.ReadFile(s.EventsPath())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 50 {
		t.Fatalf("got %d lines, want 50", len(lines))
	}
	evs, _, err := s.ReadEventsSince(0)
	if err != nil || len(evs) != 50 {
		t.Fatalf("parsed %d of 50 lines: %v", len(evs), err)
	}
	seen := map[string]bool{}
	for _, ev := range evs {
		if ev.Summary != long {
			t.Fatalf("event %s summary was corrupted", ev.Task)
		}
		seen[ev.Task] = true
	}
	if len(seen) != 50 {
		t.Fatalf("got %d distinct tasks", len(seen))
	}
}
