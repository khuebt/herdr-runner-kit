package state

import (
	"sync"
	"testing"
	"time"
)

func TestPromptTokensOnePerSubmitAndExpire(t *testing.T) {
	s := openStore(t)
	id := newTask(t, s).ID
	now := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	if ok, err := s.TakePromptToken(id); err != nil || ok {
		t.Fatalf("take with no token = %v, %v; want false", ok, err)
	}
	for range 2 {
		if err := s.AddPromptToken(id); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 2 {
		if ok, err := s.TakePromptToken(id); err != nil || !ok {
			t.Fatalf("take %d = %v, %v; want a token for each of two prompts", i, ok, err)
		}
	}
	if ok, _ := s.TakePromptToken(id); ok {
		t.Fatal("a third submit used a token two prompts left")
	}

	if err := s.AddPromptToken(id); err != nil {
		t.Fatal(err)
	}
	now = now.Add(PromptTokenTTL)
	if ok, _ := s.TakePromptToken(id); ok {
		t.Fatal("an expired token let a submit through")
	}
}

func TestPromptTokensConcurrentAddAndTake(t *testing.T) {
	s := openStore(t)
	id := newTask(t, s).ID
	const n = 20
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			if err := s.AddPromptToken(id); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	var mu sync.Mutex
	taken := 0
	for range n + 5 {
		wg.Go(func() {
			ok, err := s.TakePromptToken(id)
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				taken++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if taken != n {
		t.Fatalf("took %d tokens, want the %d added", taken, n)
	}
}
