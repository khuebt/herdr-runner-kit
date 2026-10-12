package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func TestTimingsOfOneTaskUseItsParentAndWake(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	events := []state.Event{
		{TS: at(0), Task: "t1", Type: "spawned"},
		{TS: at(100), Task: "t2", Type: "spawned"},
		{TS: at(5000), Task: "t1", Type: "done"},
		{TS: at(5200), Type: "woke", Summary: "t1"},
		{TS: at(5300), Task: "t2", Type: "prompted"},
		{TS: at(9000), Task: "t2", Type: "done"},
		{TS: at(9150), Type: "woke", Summary: "t3,t2"},
	}
	// t2 was spawned before t1 finished, so it has no chain latency; the
	// row must still show its wake-up, which no t2 event carries.
	var out strings.Builder
	if err := printTimings(&out, events, []*task.Task{{ID: "t2", After: []string{"t1"}, CreatedAt: at(100)}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "t2       5200            -                    150") {
		t.Fatalf("got\n%s", out.String())
	}
	if !forTasks([]string{"t2"}, events[6]) || forTasks([]string{"t2"}, events[3]) || !forTasks(nil, events[3]) {
		t.Fatal("forTasks must keep the wake-ups that include the task")
	}
}
