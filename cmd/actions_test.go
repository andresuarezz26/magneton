package cmd

import (
	"os"
	"os/exec"
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
// Empty repo → the agent-home fallback path, matching the repo-less Sessions
// the action tests build.
func mkWorktree(t *testing.T, ticket string) {
	t.Helper()
	wt := paths.WorktreeFor("", ticket)
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

func TestProjectDirIn(t *testing.T) {
	// Git root with the Android project in a subdirectory (monorepo layout).
	root := t.TempDir()
	repo := filepath.Join(root, "Compass")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Skip("git unavailable:", err)
	}

	// The worktree mirrors the repo, so the project sits at <wt>/Compass.
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, "Compass"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := projectDirIn(wt, repo), filepath.Join(wt, "Compass"); got != want {
		t.Errorf("subdir repo: got %q, want %q", got, want)
	}

	// Repo path IS the git root → open the worktree root itself.
	if got := projectDirIn(wt, root); got != wt {
		t.Errorf("root repo: got %q, want %q", got, wt)
	}

	// Not a git repo at all → fall back to the worktree root.
	if got := projectDirIn(wt, t.TempDir()); got != wt {
		t.Errorf("non-git repo: got %q, want %q", got, wt)
	}
}

func TestPaletteItemsIncludeGlobals(t *testing.T) {
	t.Setenv("MAGNETON_HOME", t.TempDir()) // no pidfile → daemon stopped
	m := monitorModel{flat: []store.Session{{Ticket: "K1", State: "awaiting-answer"}}, cursor: 2}
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
	// No Jira configured → a single method, so run skips the picker and goes
	// straight into the paste-content input.
	runM, _ := (monitorModel{}).doAction("run")
	if hub := runM.(monitorModel); hub.view != viewRunInput || hub.runMode != "content" {
		t.Errorf("run (single method) → content input; got view=%d mode=%q", hub.view, hub.runMode)
	}
	m := monitorModel{flat: []store.Session{{Ticket: "K1", State: "failed"}}, cursor: 2}
	mm, _ := m.doAction("stop")
	got := mm.(monitorModel)
	if got.confirming != "K1" {
		t.Error("stop → confirming set to the selected ticket")
	}
	if got.confirmCursor != 0 {
		t.Error("stop → confirmCursor reset to 0 (Yes)")
	}
}

