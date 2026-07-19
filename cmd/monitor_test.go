package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/andresuarezz26/magneton/internal/paths"
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

// whyLines for a plan-review session renders the approach and steps read from
// the worktree's .agent/plan.json.
func TestWhyLinesPlanReview(t *testing.T) {
	t.Setenv("MAGNETON_HOME", t.TempDir())
	s := store.Session{Ticket: "K-42", State: store.StatePlanReview}

	// Stub a plan.json under the ticket's worktree path.
	wt := paths.WorktreeFor(s.Repo, s.Ticket)
	agentDir := filepath.Join(wt, ".agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	planJSON := `{"plan":"Add pull to refresh on the feed","steps":["Wrap the list in SwipeRefresh","Wire the refresh callback"],"confidence":"high","type":"feature"}`
	if err := os.WriteFile(filepath.Join(agentDir, "plan.json"), []byte(planJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	lines := whyLines(s)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"Plan ready", "approve or give feedback",
		"Add pull to refresh on the feed",
		"1. Wrap the list in SwipeRefresh",
		"2. Wire the refresh callback",
		"Confidence: high", "Type: feature",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("whyLines(plan-review) missing %q in:\n%s", want, joined)
		}
	}
}

// agentActions for a plan-review session offers approve-plan and plan-feedback,
// and does NOT offer resume/ship (plan-review is neither active nor stuck).
func TestAgentActionsPlanReview(t *testing.T) {
	t.Setenv("MAGNETON_HOME", t.TempDir())
	// Build a worktree so studio/claude also show, matching a real plan-review row.
	wt := paths.WorktreeFor("", "K-43")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /x"), 0o644); err != nil {
		t.Fatal(err)
	}

	ids := map[string]bool{}
	for _, it := range agentActions(store.Session{Ticket: "K-43", State: store.StatePlanReview}) {
		ids[it.key] = true
	}
	for _, want := range []string{"approve-plan", "plan-feedback"} {
		if !ids[want] {
			t.Errorf("plan-review menu missing %q", want)
		}
	}
	for _, no := range []string{"resume", "ship"} {
		if ids[no] {
			t.Errorf("plan-review should NOT offer %q", no)
		}
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

// typeAnswer feeds a string into the answer input one rune at a time, as if
// typed, and returns the resulting model.
func typeAnswer(m monitorModel, s string) monitorModel {
	for _, ch := range s {
		got, _ := m.updateAnswering(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{ch}})
		m = got.(monitorModel)
	}
	return m
}

func TestUpdateAnsweringPasteInline(t *testing.T) {
	m := monitorModel{answering: true, answerKey: "K1"}

	// Type a lead-in, then paste a 3-line blob.
	m = typeAnswer(m, "I want this class:")
	got, _ := m.updateAnswering(tea.KeyMsg{Paste: true, Runes: []rune("a\nb\nc")})
	m = got.(monitorModel)

	// The paste is one atom placed after the typed text; cursor sits past it.
	if m.answerCursor != len(m.answerAtoms) {
		t.Errorf("cursor should be at end: cursor=%d len=%d", m.answerCursor, len(m.answerAtoms))
	}
	// A second, independent 2-line paste must count as 2 - not merge with the
	// first paste's last line (the old accumulation bug reported "+1").
	got, _ = m.updateAnswering(tea.KeyMsg{Paste: true, Runes: []rune("d\ne")})
	m = got.(monitorModel)

	var blobs []string
	for _, a := range m.answerAtoms {
		if a.blob != "" {
			blobs = append(blobs, a.blob)
		}
	}
	if len(blobs) != 2 {
		t.Fatalf("want 2 distinct paste atoms, got %d", len(blobs))
	}
	if lineCount(blobs[0]) != 3 || lineCount(blobs[1]) != 2 {
		t.Errorf("paste line counts: got %d and %d, want 3 and 2", lineCount(blobs[0]), lineCount(blobs[1]))
	}

	// Submitting expands pastes to their full bodies interleaved with the text.
	full := answerText(m.answerAtoms)
	if !strings.Contains(full, "I want this class:") || !strings.Contains(full, "a\nb\nc") || !strings.Contains(full, "d\ne") {
		t.Errorf("assembled answer missing parts: %q", full)
	}
}

func TestUpdateAnsweringManyPastes(t *testing.T) {
	m := monitorModel{answering: true, answerKey: "K1"}

	// Interleave a typed word and a paste, five times over. Nothing caps the
	// number of pastes - each is its own atom.
	// All multi-line so each stays a collapsed chip (short single-line pastes
	// inline as text - covered separately).
	pastes := []string{"a\nb", "c\nd\ne", "f\ng", "g\nh\ni\nj", "k\nl"}
	wantLines := []int{2, 3, 2, 4, 2}
	for _, p := range pastes {
		m = typeAnswer(m, "word ")
		got, _ := m.updateAnswering(tea.KeyMsg{Paste: true, Runes: []rune(p)})
		m = got.(monitorModel)
	}

	var got []int
	for _, a := range m.answerAtoms {
		if a.blob != "" {
			got = append(got, lineCount(a.blob))
		}
	}
	if len(got) != len(pastes) {
		t.Fatalf("want %d distinct pastes, got %d", len(pastes), len(got))
	}
	for i := range wantLines {
		if got[i] != wantLines[i] {
			t.Errorf("paste %d line count: got %d, want %d", i, got[i], wantLines[i])
		}
	}
}

func TestUpdateAnsweringShortPasteInlinesAsText(t *testing.T) {
	// A short single-line paste becomes literal typed text - no paste atom, no
	// "[1 line added]" chip.
	m := monitorModel{answering: true, answerKey: "K1"}
	got, _ := m.updateAnswering(tea.KeyMsg{Paste: true, Runes: []rune("https://example.com/x")})
	m = got.(monitorModel)
	for _, a := range m.answerAtoms {
		if a.blob != "" {
			t.Fatalf("short single-line paste should not create a paste atom")
		}
	}
	if answerText(m.answerAtoms) != "https://example.com/x" {
		t.Errorf("short paste text: got %q", answerText(m.answerAtoms))
	}
	if m.answerCursor != len([]rune("https://example.com/x")) {
		t.Errorf("cursor should be past the inlined text: got %d", m.answerCursor)
	}

	// A single line at/over the cutoff stays a collapsed paste atom.
	long := strings.Repeat("z", pasteInlineMaxRunes+1)
	m2 := monitorModel{answering: true, answerKey: "K1"}
	got2, _ := m2.updateAnswering(tea.KeyMsg{Paste: true, Runes: []rune(long)})
	m2 = got2.(monitorModel)
	blobs := 0
	for _, a := range m2.answerAtoms {
		if a.blob != "" {
			blobs++
		}
	}
	if blobs != 1 {
		t.Errorf("long single-line paste should stay one paste atom, got %d", blobs)
	}

	// A multi-line paste, even a short one, stays a paste atom.
	m3 := monitorModel{answering: true, answerKey: "K1"}
	got3, _ := m3.updateAnswering(tea.KeyMsg{Paste: true, Runes: []rune("a\nb")})
	m3 = got3.(monitorModel)
	if len(m3.answerAtoms) != 1 || m3.answerAtoms[0].blob == "" {
		t.Errorf("multi-line paste should stay one paste atom: %+v", m3.answerAtoms)
	}
}

func TestUpdateAnsweringEscClears(t *testing.T) {
	m := monitorModel{answering: true, answerKey: "K1"}
	m = typeAnswer(m, "hello")
	got, _ := m.updateAnswering(tea.KeyMsg{Type: tea.KeyEsc})
	mm := got.(monitorModel)
	if mm.answering || mm.answerAtoms != nil || mm.answerCursor != 0 {
		t.Error("Esc should clear answering, answerAtoms, and answerCursor")
	}
}

func TestUpdateAnsweringCursor(t *testing.T) {
	m := monitorModel{answering: true, answerKey: "K1"}
	m = typeAnswer(m, "abc")
	if answerText(m.answerAtoms) != "abc" || m.answerCursor != 3 {
		t.Errorf("after typing abc: text=%q cursor=%d", answerText(m.answerAtoms), m.answerCursor)
	}

	// Left, then insert "X" mid-string → "abXc".
	got, _ := m.updateAnswering(tea.KeyMsg{Type: tea.KeyLeft})
	m = got.(monitorModel)
	if m.answerCursor != 2 {
		t.Errorf("after Left: cursor=%d, want 2", m.answerCursor)
	}
	m = typeAnswer(m, "X")
	if answerText(m.answerAtoms) != "abXc" || m.answerCursor != 3 {
		t.Errorf("insert mid: text=%q cursor=%d", answerText(m.answerAtoms), m.answerCursor)
	}

	// Backspace deletes "X" → "abc", cursor=2.
	got, _ = m.updateAnswering(tea.KeyMsg{Type: tea.KeyBackspace})
	m = got.(monitorModel)
	if answerText(m.answerAtoms) != "abc" || m.answerCursor != 2 {
		t.Errorf("backspace: text=%q cursor=%d", answerText(m.answerAtoms), m.answerCursor)
	}

	// Left clamps at 0.
	for i := 0; i < 10; i++ {
		got, _ = m.updateAnswering(tea.KeyMsg{Type: tea.KeyLeft})
		m = got.(monitorModel)
	}
	if m.answerCursor != 0 {
		t.Errorf("cursor clamped at 0: got %d", m.answerCursor)
	}
}

func TestUpdateAnsweringBackspaceDeletesWholePaste(t *testing.T) {
	m := monitorModel{answering: true, answerKey: "K1"}
	m = typeAnswer(m, "hi")
	got, _ := m.updateAnswering(tea.KeyMsg{Paste: true, Runes: []rune("x\ny\nz")})
	m = got.(monitorModel)

	// One backspace removes the entire paste chunk, not one character of it.
	got, _ = m.updateAnswering(tea.KeyMsg{Type: tea.KeyBackspace})
	m = got.(monitorModel)
	if answerText(m.answerAtoms) != "hi" {
		t.Errorf("backspace over paste should remove it whole: got %q", answerText(m.answerAtoms))
	}
}

// TestRenderRowDescPreference verifies the priority order for the third column:
// ShortDesc → Summary → log fallback.
func TestRenderRowDescPreference(t *testing.T) {
	m := monitorModel{}
	w := 80

	// ShortDesc wins over Summary.
	s1 := store.Session{
		Ticket:    "K-1",
		State:     "working",
		Summary:   "Long verbose Jira summary that should not appear",
		ShortDesc: "upload paper to storage",
		UpdatedAt: time.Now(),
	}
	row1 := m.renderRow(s1, w, false)
	if !strings.Contains(row1, "upload paper to storage") {
		t.Errorf("ShortDesc should appear in row: %q", row1)
	}
	if strings.Contains(row1, "Long verbose") {
		t.Errorf("Summary should be hidden when ShortDesc is set: %q", row1)
	}

	// Summary shows when ShortDesc is empty.
	s2 := store.Session{
		Ticket:    "K-2",
		State:     "working",
		Summary:   "Fix login crash",
		ShortDesc: "",
		UpdatedAt: time.Now(),
	}
	row2 := m.renderRow(s2, w, false)
	if !strings.Contains(row2, "Fix login crash") {
		t.Errorf("Summary should appear when ShortDesc is empty: %q", row2)
	}
}

// The selected row shows an inline "↵ actions" affordance; unselected rows don't.
func TestRenderRowSelectedHint(t *testing.T) {
	m := monitorModel{}
	s := store.Session{Ticket: "K-1", State: "working", ShortDesc: "do a thing", UpdatedAt: time.Now()}

	sel := m.renderRow(s, 80, true)
	if !strings.Contains(sel, "↵ actions") {
		t.Errorf("selected row should show the ↵ actions hint: %q", sel)
	}
	unsel := m.renderRow(s, 80, false)
	if strings.Contains(unsel, "↵ actions") {
		t.Errorf("unselected row must not show the hint: %q", unsel)
	}
}

// TestHeaderCounts guards the header's two live tallies. "N running" counts
// only the RUNNING group (in-progress agents), and "N need you" counts only
// the NEEDS YOU group. STOPPED and DONE sessions are historical and must
// inflate neither - the header reflects what's happening now, not all time.
func TestHeaderCounts(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	seed := []struct{ ticket, state string }{
		{"KAN-1", "needs-you"},        // need you
		{"KAN-2", "failed"},           // need you
		{"KAN-3", store.StateStopped}, // stopped - counts toward neither
		{"KAN-4", store.StateStopped}, // stopped - counts toward neither
		{"KAN-5", store.StateStopped}, // stopped - counts toward neither
		{"KAN-6", "review"},           // done - counts toward neither
		{"KAN-7", "working"},          // running
		{"KAN-8", "planning"},         // running
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
	if !strings.Contains(out, "2 running") {
		t.Errorf("header should report 2 running (in-progress only); got:\n%s", out)
	}
	if !strings.Contains(out, "2 needs you") {
		t.Errorf("header should report 2 needs you (stopped excluded); got:\n%s", out)
	}
	// The all-time total (8) must not leak into either tally.
	if strings.Contains(out, fmt.Sprintf("%d running", len(seed))) ||
		strings.Contains(out, "5 needs you") {
		t.Errorf("header counted historical sessions as live; got:\n%s", out)
	}
}
