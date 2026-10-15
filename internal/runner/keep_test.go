package runner

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func TestGCClosesAKeptRunnerAfterKeepFor(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	r.Config.KeepFor = 4 * time.Hour
	old := finishedRunner(t, r, fake)
	recent := finishedRunner(t, r, fake)
	for _, id := range []string{old.ID, recent.ID} {
		if _, err := r.Keep(context.Background(), id, true); err != nil {
			t.Fatal(err)
		}
	}
	finishAgo(t, r, old.ID, 5*time.Hour)
	finishAgo(t, r, recent.ID, 3*time.Hour)
	for _, id := range []string{old.ID, recent.ID} { // finishAgo leaves keep as is
		if got, _ := r.Store.Get(id); !got.Keep {
			t.Fatalf("%s lost its keep", id)
		}
	}
	closed, err := r.GC(context.Background(), "", GCAge, false, false)
	if err != nil || !slices.Equal(closed, []string{old.ID}) {
		t.Fatalf("closed = %v, %v", closed, err)
	}
	if evs := eventsOf(t, r, old.ID, "closed"); len(evs) != 1 || !KeptClosed(evs[0]) {
		t.Errorf("closed events = %+v", evs)
	}
	got, _ := r.Store.Get(recent.ID)
	if at, ok := CloseAt(got, r.Config.KeepFor); !ok || !at.Equal(got.FinishedAt.Add(4*time.Hour)) {
		t.Errorf("CloseAt of a kept runner = %v, %v", at, ok)
	}
	if due, ok := NextGC([]*task.Task{got}, "", r.Config.KeepFor); !ok || !due.Equal(got.FinishedAt.Add(4*time.Hour)) {
		t.Errorf("NextGC = %v, %v", due, ok)
	}
}

func TestKeepForZeroKeepsForever(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	r.Config.KeepFor = 0
	tk := finishedRunner(t, r, fake)
	if _, err := r.Keep(context.Background(), tk.ID, true); err != nil {
		t.Fatal(err)
	}
	finishAgo(t, r, tk.ID, 100*time.Hour)
	if closed, err := r.GC(context.Background(), "", GCAge, false, false); err != nil || len(closed) != 0 {
		t.Fatalf("closed = %v, %v", closed, err)
	}
	// gc --kept closes it once it finished --older-than ago.
	if closed, err := r.GC(context.Background(), "", GCAge, false, true); err != nil || !slices.Equal(closed, []string{tk.ID}) {
		t.Fatalf("gc --kept closed %v, %v", closed, err)
	}
}

func TestGCKeptHonoursOlderThan(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := finishedRunner(t, r, fake)
	if _, err := r.Keep(context.Background(), tk.ID, true); err != nil {
		t.Fatal(err)
	}
	finishAgo(t, r, tk.ID, 20*time.Minute)
	if closed, err := r.GC(context.Background(), "", GCAge, false, true); err != nil || len(closed) != 0 {
		t.Fatalf("gc --kept closed a runner that finished 20m ago: %v, %v", closed, err)
	}
	if closed, err := r.GC(context.Background(), "", 10*time.Minute, false, true); err != nil || !slices.Equal(closed, []string{tk.ID}) {
		t.Fatalf("gc --kept --older-than 10m: %v, %v", closed, err)
	}
}

func TestGCLeavesAKeptRunnerInUse(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := finishedRunner(t, r, fake)
	if _, err := r.Keep(context.Background(), tk.ID, true); err != nil {
		t.Fatal(err)
	}
	finishAgo(t, r, tk.ID, 5*time.Hour)
	fake.on("agent get", ok(`{"result":{"agent":{"agent":"claude","agent_status":"working"}}}`))
	if closed, err := r.GC(context.Background(), "", GCAge, false, false); err != nil || len(closed) != 0 || fake.count("agent", "send-keys") != 0 {
		t.Fatalf("closed %v, %v; send-keys %d", closed, err, fake.count("agent", "send-keys"))
	}
}
