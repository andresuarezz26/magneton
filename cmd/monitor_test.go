package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestPidAlive(t *testing.T) {
	if !pidAlive(os.Getpid()) {
		t.Error("our own pid should be alive")
	}
	if pidAlive(0) {
		t.Error("pid 0 is not a real process")
	}
	if pidAlive(2_000_000_000) {
		t.Error("an absurd pid should not be alive")
	}
}

func TestIsStopped(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Hour)
	dead := 2_000_000_000 // almost certainly not a live process
	live := os.Getpid()

	// Known pid, dead → stopped (deterministic, even if recently updated).
	if !isStopped(store.Session{State: "working", PID: dead, UpdatedAt: now}) {
		t.Error("running session with a dead pid should be stopped")
	}
	// Known pid, alive → NOT stopped, even if idle a long time.
	if isStopped(store.Session{State: "working", PID: live, UpdatedAt: old}) {
		t.Error("running session with a live pid should not be stopped")
	}
	// No pid (old row), idle long → stopped via the activity-heuristic fallback.
	if !isStopped(store.Session{Ticket: "NOPE-1", State: "working", UpdatedAt: old}) {
		t.Error("idle running session with no pid should be stopped (fallback)")
	}
	// No pid, fresh → not stopped.
	if isStopped(store.Session{Ticket: "NOPE-2", State: "working", UpdatedAt: now}) {
		t.Error("fresh running session should not be stopped")
	}
	// Terminal/needs-you states are never "stopped", even with a dead pid.
	if isStopped(store.Session{State: "failed", PID: dead, UpdatedAt: old}) {
		t.Error("failed is not a running state; never stopped")
	}
	if isStopped(store.Session{State: "review", UpdatedAt: old}) {
		t.Error("review is terminal; never stopped")
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
	// Cursor 0 is the Start-new row → no agent selected.
	m.cursor = 0
	if m.selected() != nil {
		t.Error("cursor 0 (Start-new row) should select no agent")
	}
	// Cursor 1 is the Edit-config row → no agent selected.
	m.cursor = 1
	if m.selected() != nil {
		t.Error("cursor 1 (Edit-config row) should select no agent")
	}
	// Cursor 2 selects the first agent.
	m.cursor = 2
	if m.selected() == nil || m.selected().Ticket != m.flat[0].Ticket {
		t.Error("cursor 2 should select the first agent")
	}
	// Clamp: max cursor is len(flat)+1 (two pinned rows + N agents).
	m.cursor = 99
	m.reload()
	if m.cursor != 6 {
		t.Errorf("cursor not clamped: %d, want 6", m.cursor)
	}
}

func TestCancelAgentMarksStopped(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Claim("KAN-9", "", "x"); err != nil { // empty repo → worktree removal skipped
		t.Fatal(err)
	}
	_ = st.SetState("KAN-9", "failed", 0)

	m := monitorModel{store: st}
	// No pid, no repo: cancelAgent only marks the session stopped.
	msg := m.cancelAgent(store.Session{Ticket: "KAN-9", State: "failed"})()
	if done, ok := msg.(cancelDoneMsg); !ok || done.ticket != "KAN-9" {
		t.Fatalf("unexpected msg: %#v", msg)
	}
	got, err := st.Get("KAN-9")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateStopped {
		t.Errorf("state = %q, want %q", got.State, store.StateStopped)
	}

	// And it now buckets under STOPPED, not NEEDS YOU.
	m.reload()
	for _, g := range m.groups {
		if g.label == "NEEDS YOU" {
			for _, s := range g.sessions {
				if s.Ticket == "KAN-9" {
					t.Error("KAN-9 should have left NEEDS YOU after stop")
				}
			}
		}
	}
}

// TestHeaderNeedYouCount guards the header's "N need you" tally. STOPPED
// sessions are manually cancelled and must NOT inflate the count — only the
// NEEDS YOU group (awaiting-answer / needs-you / failed) counts.
func TestHeaderNeedYouCount(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	seed := []struct{ ticket, state string }{
		{"KAN-1", "needs-you"},        // needs you
		{"KAN-2", "failed"},           // needs you
		{"KAN-3", store.StateStopped}, // stopped — must not count
		{"KAN-4", store.StateStopped}, // stopped — must not count
		{"KAN-5", store.StateStopped}, // stopped — must not count
		{"KAN-6", "review"},           // done
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

	out := m.View()
	if !strings.Contains(out, fmt.Sprintf("%d agents", len(seed))) {
		t.Errorf("header missing agent count %d; got:\n%s", len(seed), out)
	}
	if !strings.Contains(out, "2 need you") {
		t.Errorf("header should report 2 need you (stopped excluded); got:\n%s", out)
	}
	if strings.Contains(out, "5 need you") {
		t.Errorf("header counted STOPPED sessions as need you; got:\n%s", out)
	}
}
