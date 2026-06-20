package store

import (
	"path/filepath"
	"sync"
	"testing"
)

func TestClaimIsAtomic(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 10 concurrent workers race to claim the same ticket; exactly one wins.
	const n = 10
	var wg sync.WaitGroup
	wins := make([]bool, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			ok, err := s.Claim("PROJ-1", "/repo", "summary")
			if err != nil {
				t.Errorf("claim: %v", err)
			}
			wins[i] = ok
		}(i)
	}
	wg.Wait()

	count := 0
	for _, w := range wins {
		if w {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 winning claim, got %d", count)
	}
}

func TestStateAndList(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.Claim("PROJ-2", "/repo", "do thing"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetState("PROJ-2", StateReview, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFields("PROJ-2", "ai/proj-2", "/wt", "http://pr/1"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("PROJ-2")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateReview || got.Retries != 2 || got.Branch != "ai/proj-2" || got.PRURL != "http://pr/1" {
		t.Fatalf("unexpected session: %+v", got)
	}
	list, err := s.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v (err %v), want 1", list, err)
	}
}

func TestSetPIDRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.Claim("PROJ-9", "/repo", "x"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPID("PROJ-9", 4242); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("PROJ-9")
	if err != nil {
		t.Fatal(err)
	}
	if got.PID != 4242 {
		t.Errorf("PID = %d, want 4242", got.PID)
	}
}
