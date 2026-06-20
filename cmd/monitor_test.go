package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/andresuarezz26/magneton/internal/store"
)

func TestTailLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.log")
	if err := os.WriteFile(p, []byte("a\n\nb\n  \nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := tailLines(p, 2)
	if len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Errorf("tailLines = %#v, want [b c] (blank lines dropped)", got)
	}
	if tailLines(filepath.Join(dir, "missing.log"), 3) != nil {
		t.Error("missing file should return nil")
	}
}

func TestCleanActivityAndStrip(t *testing.T) {
	lines := []string{"[KAN-6] queued → working", "  ⚙ Edit(/path/HomeScreen.kt)"}
	got := cleanActivity(lines, "KAN-6")
	if got != "Edit(/path/HomeScreen.kt)" {
		t.Errorf("cleanActivity = %q", got)
	}
	if stripPrefix("[KAN-6] hi", "KAN-6") != "hi" {
		t.Error("stripPrefix failed")
	}
	if cleanActivity(nil, "X") != "" {
		t.Error("empty input should be empty")
	}
}

func TestTruncate(t *testing.T) {
	if truncate("hello", 10) != "hello" {
		t.Error("no truncation when it fits")
	}
	if got := truncate("hello world", 6); got != "hello…" {
		t.Errorf("truncate = %q, want hello…", got)
	}
}

func TestGlyphFor(t *testing.T) {
	now := time.Now()
	cases := []struct {
		s    store.Session
		want string
	}{
		{store.Session{State: "awaiting-answer", UpdatedAt: now}, "▮"},
		{store.Session{State: "failed", UpdatedAt: now}, "✗"},
		{store.Session{State: "needs-you", UpdatedAt: now}, "⚑"},
		{store.Session{State: "review", UpdatedAt: now}, "✓"},
		{store.Session{State: "working", UpdatedAt: now}, "●"},
		{store.Session{State: "working", UpdatedAt: now.Add(-time.Hour)}, "■"}, // stale → stopped
	}
	for _, c := range cases {
		if got := glyphFor(c.s); got != c.want {
			t.Errorf("glyphFor(%q, age=%s) = %q, want %q", c.s.State, time.Since(c.s.UpdatedAt), got, c.want)
		}
	}
}

func TestIsStopped(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Hour)
	// running state, idle long → stopped (no log file, so freshest = UpdatedAt)
	if !isStopped(store.Session{Ticket: "NOPE-1", State: "working", UpdatedAt: old}) {
		t.Error("idle running session should be stopped")
	}
	// running state, fresh → not stopped
	if isStopped(store.Session{Ticket: "NOPE-2", State: "working", UpdatedAt: now}) {
		t.Error("fresh running session should not be stopped")
	}
	// terminal/needs-you states are never "stopped" even if old
	if isStopped(store.Session{Ticket: "NOPE-3", State: "failed", UpdatedAt: old}) {
		t.Error("failed is not a running state; should never be stopped")
	}
	if isStopped(store.Session{Ticket: "NOPE-4", State: "review", UpdatedAt: old}) {
		t.Error("review is terminal; should never be stopped")
	}
}

func TestReloadGrouping(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	seed := []struct{ ticket, state string }{
		{"KAN-1", "working"},         // running
		{"KAN-2", "awaiting-answer"}, // needs you
		{"KAN-3", "failed"},          // needs you
		{"KAN-4", "review"},          // done
		{"KAN-5", "planning"},        // running
	}
	for _, s := range seed {
		if _, err := st.Claim(s.ticket, "/repo", s.ticket+" summary"); err != nil {
			t.Fatal(err)
		}
		if err := st.SetState(s.ticket, s.state, 0); err != nil {
			t.Fatal(err)
		}
	}

	m := monitorModel{store: st}
	m.reload()

	if m.err != nil {
		t.Fatalf("reload err: %v", m.err)
	}
	want := map[string]int{"NEEDS YOU": 2, "RUNNING": 2, "DONE": 1}
	for _, g := range m.groups {
		if g.label == "NEEDS YOU" || g.label == "RUNNING" || g.label == "DONE" {
			if len(g.sessions) != want[g.label] {
				t.Errorf("group %s has %d, want %d", g.label, len(g.sessions), want[g.label])
			}
		}
	}
	if len(m.flat) != 5 {
		t.Fatalf("flat = %d, want 5", len(m.flat))
	}
	// Flat order must be needs-you first (triage), so the selectable cursor
	// starts on something that needs attention.
	if m.flat[0].State != "awaiting-answer" && m.flat[0].State != "failed" {
		t.Errorf("flat[0] state = %q, want a needs-you state first", m.flat[0].State)
	}
	// selected() bounds + cursor clamp.
	if m.selected() == nil {
		t.Error("selected() should not be nil with 5 rows")
	}
	m.cursor = 99
	m.reload()
	if m.cursor != 4 {
		t.Errorf("cursor not clamped: %d, want 4", m.cursor)
	}
}
