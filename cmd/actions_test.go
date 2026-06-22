package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/andresuarezz26/magneton/internal/paths"
	"github.com/andresuarezz26/magneton/internal/store"
)

func itemIDs(items []paletteItem) map[string]bool {
	m := map[string]bool{}
	for _, it := range items {
		m[it.key] = true
	}
	return m
}

// mkWorktree creates a fake worktree (a dir with a .git link) for a ticket.
func mkWorktree(t *testing.T, ticket string) {
	t.Helper()
	wt := paths.WorktreeFor(ticket)
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAgentActionsWithWorktree(t *testing.T) {
	t.Setenv("MAGNETON_HOME", t.TempDir())
	mkWorktree(t, "K1")

	// failed + worktree → resume, studio, claude, stop.
	ids := itemIDs(agentActions(store.Session{Ticket: "K1", State: "failed"}))
	for _, want := range []string{"resume", "studio", "claude", "stop"} {
		if !ids[want] {
			t.Errorf("failed+worktree: missing %q", want)
		}
	}
	if ids["rerun"] {
		t.Error("failed+worktree should not offer rerun")
	}

	// awaiting + worktree → answer + studio/claude, not resume.
	ids = itemIDs(agentActions(store.Session{Ticket: "K1", State: "awaiting-answer"}))
	if !ids["answer"] || !ids["studio"] || !ids["claude"] {
		t.Errorf("awaiting+worktree menu = %v", ids)
	}
	if ids["resume"] {
		t.Error("awaiting should not offer resume")
	}
}

func TestAgentActionsStoppedNoWorktree(t *testing.T) {
	t.Setenv("MAGNETON_HOME", t.TempDir()) // no worktree on disk

	// stopped + no worktree → Run again (fresh) only; no resume/studio/claude/stop.
	ids := itemIDs(agentActions(store.Session{Ticket: "K1", State: store.StateStopped}))
	if !ids["rerun"] {
		t.Error("stopped+no worktree should offer rerun")
	}
	for _, no := range []string{"resume", "studio", "claude", "stop"} {
		if ids[no] {
			t.Errorf("stopped+no worktree should NOT offer %q", no)
		}
	}

	// review (terminal) → no Stop.
	if itemIDs(agentActions(store.Session{Ticket: "K1", State: "review"}))["stop"] {
		t.Error("review (terminal) should not offer stop")
	}
}

func TestPaletteItemsIncludeGlobals(t *testing.T) {
	t.Setenv("MAGNETON_HOME", t.TempDir()) // no pidfile → daemon stopped
	m := monitorModel{flat: []store.Session{{Ticket: "K1", State: "awaiting-answer"}}, cursor: 1}
	ids := itemIDs(m.paletteItems())
	for _, want := range []string{"answer", "run", "doctor", "config", "setup", "daemon-start", "quit"} {
		if !ids[want] {
			t.Errorf("menu missing %q", want)
		}
	}
	if ids["daemon-stop"] {
		t.Error("daemon stopped should not offer daemon-stop")
	}
}

func TestDoActionTransitions(t *testing.T) {
	if mm, _ := (monitorModel{}).doAction("menu"); mm.(monitorModel).view != viewPalette {
		t.Error("menu → palette view")
	}
	if mm, _ := (monitorModel{}).doAction("run"); mm.(monitorModel).view != viewRunInput {
		t.Error("run → run-input view")
	}
	m := monitorModel{flat: []store.Session{{Ticket: "K1", State: "failed"}}, cursor: 1}
	if mm, _ := m.doAction("stop"); mm.(monitorModel).confirming != "K1" {
		t.Error("stop → confirming set to the selected ticket")
	}
}
