package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// TestOpenClaudeInteractiveOverride verifies the interactive override is a
// non-empty system-prompt string (passed via --append-system-prompt, never
// written to a worktree file, so it can't pollute the user's PR commit).
func TestOpenClaudeInteractiveOverride(t *testing.T) {
	if interactiveOverride == "" {
		t.Fatal("interactiveOverride must be a non-empty instruction")
	}
	if !strings.Contains(interactiveOverride, "interactive") {
		t.Errorf("override should mention the interactive session: %q", interactiveOverride)
	}
	// A single-quoted shell arg must survive without breaking (no embedded
	// single quotes that shellQuote can't handle cleanly).
	quoted := shellQuote(interactiveOverride)
	if !strings.HasPrefix(quoted, "'") || !strings.HasSuffix(quoted, "'") {
		t.Errorf("override not safely shell-quoted: %q", quoted)
	}
	// Must be a single line so it embeds safely in the AppleScript `do script`.
	if strings.Contains(interactiveOverride, "\n") {
		t.Error("override must be a single line for safe terminal embedding")
	}
}

func TestTerminalLaunchScript(t *testing.T) {
	cmd := "cd '/x' && claude"
	title := "TICKET-1 · ai/branch"

	// Apple Terminal (and any unknown terminal) → try a new tab (Cmd+T), but only
	// run "in front window" when a new tab is verified to exist; otherwise open a
	// NEW window. The tab-count guard is what prevents injecting into magneton's
	// own tab when Accessibility permission is missing.
	for _, tp := range []string{"Apple_Terminal", "", "vscode", "ghostty", "WezTerm"} {
		got := terminalLaunchScript(tp, cmd, title)
		if !strings.Contains(got, `application "Terminal"`) {
			t.Errorf("%q: expected Terminal.app target:\n%s", tp, got)
		}
		// Both branches present: tab (in front window) and window fallback.
		if !strings.Contains(got, `do script "cd '/x' && claude" in front window`) {
			t.Errorf("%q: expected the new-tab branch:\n%s", tp, got)
		}
		if !strings.Contains(got, `do script "cd '/x' && claude"`+"\n") {
			t.Errorf("%q: expected the new-window fallback branch:\n%s", tp, got)
		}
		// The tab branch must be guarded by the before/after tab-count check so it
		// never injects into magneton's tab when the keystroke is a no-op.
		if !strings.Contains(got, "tabsBefore") || !strings.Contains(got, "count of tabs of front window") {
			t.Errorf("%q: tab branch must be guarded by a tab-count check:\n%s", tp, got)
		}
		if !strings.Contains(got, "keystroke \"t\" using command down") {
			t.Errorf("%q: expected the Cmd+T tab attempt:\n%s", tp, got)
		}
		if !strings.Contains(got, "set custom title") {
			t.Errorf("%q: title not set:\n%s", tp, got)
		}
		// Reports which path ran so the UI can guide the user toward tabs.
		if !strings.Contains(got, "return outcome") {
			t.Errorf("%q: script should return its outcome:\n%s", tp, got)
		}
	}

	// iTerm2 → a new tab via its native API (no System Events / accessibility).
	iterm := terminalLaunchScript("iTerm.app", cmd, title)
	if !strings.Contains(iterm, `application "iTerm"`) {
		t.Errorf("iTerm: expected iTerm target:\n%s", iterm)
	}
	if !strings.Contains(iterm, "create tab with default profile") {
		t.Errorf("iTerm: expected a new tab:\n%s", iterm)
	}
	if !strings.Contains(iterm, "write text") || !strings.Contains(iterm, "cd '/x' && claude") {
		t.Errorf("iTerm: command not run via write text:\n%s", iterm)
	}
	if strings.Contains(iterm, "System Events") {
		t.Errorf("iTerm: must not need System Events keystrokes:\n%s", iterm)
	}
}

