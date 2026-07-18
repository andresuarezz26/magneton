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

func TestSetSessionIDRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Claim("PROJ-7", "/repo", "x"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionID("PROJ-7", "sess-abc-123"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("PROJ-7")
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "sess-abc-123" {
		t.Errorf("SessionID = %q, want sess-abc-123", got.SessionID)
	}
}

func TestSetShortDescRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.Claim("PROJ-20", "/repo", "summary"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetShortDesc("PROJ-20", "upload paper PDF to storage"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("PROJ-20")
	if err != nil {
		t.Fatal(err)
	}
	if got.ShortDesc != "upload paper PDF to storage" {
		t.Errorf("ShortDesc = %q, want %q", got.ShortDesc, "upload paper PDF to storage")
	}
}

func TestShortDescMigrationIdempotent(t *testing.T) {
	// Opening the same DB twice should not fail (ALTER TABLE short_desc is idempotent
	// because the error is silently ignored on the second call).
	path := filepath.Join(t.TempDir(), "state.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s1.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open failed: %v", err)
	}
	defer s2.Close()

	// Basic operations work on the re-opened DB.
	if _, err := s2.Claim("PROJ-21", "/repo", "x"); err != nil {
		t.Fatal(err)
	}
	if err := s2.SetShortDesc("PROJ-21", "short gist"); err != nil {
		t.Fatal(err)
	}
	got, err := s2.Get("PROJ-21")
	if err != nil {
		t.Fatal(err)
	}
	if got.ShortDesc != "short gist" {
		t.Errorf("ShortDesc after reopen = %q", got.ShortDesc)
	}
}
