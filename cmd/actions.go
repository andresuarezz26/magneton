package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/andresuarezz26/magneton/internal/paths"
	"github.com/andresuarezz26/magneton/internal/store"
)

// shellQuote single-quotes a string for safe embedding in a shell command line.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// worktreeExists reports whether the ticket still has a usable git worktree.
// "Stop & clean up" removes it, so Resume / Open Studio / Open Claude only make
// sense when this is true.
func worktreeExists(repo, ticket string) bool {
	_, err := os.Stat(filepath.Join(paths.WorktreeFor(repo, ticket), ".git"))
	return err == nil
}

// agentActions returns the menu items that make sense for one agent's state and
// whether its worktree still exists. Shown when the user presses Enter on it.
func agentActions(s store.Session) []paletteItem {
	var items []paletteItem
	hasWT := worktreeExists(s.Repo, s.Ticket)
	active := store.IsActive(s.State)
	stuck := s.State == "needs-you" || s.State == "failed" || s.State == store.StateStopped || isStopped(s)
	done := s.State == "review" || s.State == "merged" || s.State == "closed"

	if s.State == "awaiting-answer" {
		items = append(items, paletteItem{"answer", "Answer the questions", "reply, then the agent resumes"})
	}
	// Pause a live run: stop the agent but keep the worktree, dropping the ticket
	// to NEEDS YOU so you can take over by hand (then Resume / Open a PR).
	if active {
		items = append(items, paletteItem{"pause", "Pause (move to NEEDS YOU)", "stop the running agent but keep the worktree so you can take over"})
	}
	if hasWT {
		if stuck {
			items = append(items,
				paletteItem{"resume", "Resume from last stage", "re-run verification on your fix, then open the PR"},
				paletteItem{"ship", "Open a PR", "skip verification - commit + push + PR (when the gate itself is unreliable here)"},
			)
		}
		items = append(items, paletteItem{"studio", "Open Android Studio", "open the worktree as a project"})
		// "Open in Claude Code" resumes the agent's session. Only offer it when no
		// run is active - otherwise the headless agent and this interactive session
		// would both write to the same session on disk and diverge.
		if !active {
			items = append(items, paletteItem{"claude", "Open in Claude Code", "resume the agent's session in a new terminal"})
		}
	} else if stuck || done {
		// Worktree is gone — only a fresh run is possible.
		items = append(items, paletteItem{"rerun", "Run again (fresh)", "no worktree left - start this ticket from scratch"})
	}
	// Stop is available for live/stuck rows (kills the process, removes the
	// worktree, and clears it into STOPPED) - but not for finished or already-
	// stopped rows.
	if !done && s.State != store.StateStopped {
		items = append(items, paletteItem{"stop", "Stop & clean up", "kill the process and remove the worktree"})
	}
	return items
}

// claudeClosedMsg is returned after launching a Claude Code terminal.
type claudeClosedMsg struct{ err error }

// openClaude opens a NEW terminal window running an interactive Claude Code
// session in the ticket's worktree, resuming the agent's stored session when
// there is one. The dashboard keeps running.
func (m monitorModel) openClaude(s store.Session) tea.Cmd {
	cmdline := "cd " + shellQuote(paths.WorktreeFor(s.Repo, s.Ticket)) + " && claude"
	if s.SessionID != "" {
		// Resumed sessions keep the model they were saved with (per Claude Code
		// docs), so without an override the interactive window would reopen on
		// whatever model the headless stage ran - not what the user expects.
		// "--model default" clears that and reverts to the user's/org's default.
		cmdline += " --resume " + shellQuote(s.SessionID) + " --model default"
	}
	// When the ticket is stuck (needs-you/failed/stopped) the user opens this
	// session to fix it by hand. Chain magneton's own gate+PR after the
	// interactive session exits, so finishing the fix automatically re-runs
	// verification and opens the PR - no need to come back and tap "Create PR
	// from my fix". `run --resume` writes to the store, so the dashboard reflects
	// the outcome. (If the user closes the window instead of exiting claude the
	// chained command can't run - an inherent limit of doing this in-terminal.)
	stuck := s.State == "needs-you" || s.State == "failed" || s.State == store.StateStopped || isStopped(s)
	if self, err := os.Executable(); err == nil && stuck {
		cmdline += "; " + shellQuote(self) + " run " + shellQuote(s.Ticket) + " --resume"
	}
	script := "tell application \"Terminal\"\n\tdo script \"" + cmdline + "\"\n\tactivate\nend tell"
	return func() tea.Msg {
		return claudeClosedMsg{err: exec.Command("osascript", "-e", script).Start()}
	}
}

// doAction runs an action by id. The Enter menu and the keyboard shortcuts both
// route here so they stay in sync.
func (m monitorModel) doAction(id string) (tea.Model, tea.Cmd) {
	switch id {
	// --- per-agent ---
	case "answer":
		if s := m.selected(); s != nil && s.State == "awaiting-answer" {
			m.answering = true
			m.input = ""
			m.answerKey = s.Ticket
			m.notice = ""
		}
	case "studio":
		if s := m.selected(); s != nil {
			wt := paths.WorktreeFor(s.Repo, s.Ticket)
			// Prefer the JetBrains `studio` launcher (opens the dir as a project);
			// fall back to the macOS app.
			if _, err := exec.LookPath("studio"); err == nil {
				_ = exec.Command("studio", wt).Start()
			} else {
				_ = exec.Command("open", "-a", "Android Studio", wt).Start()
			}
			m.notice = "opening Android Studio in the worktree…"
		}
	case "claude":
		if s := m.selected(); s != nil {
			return m, m.openClaude(*s)
		}
	case "resume":
		if s := m.selected(); s != nil {
			m.notice = "resuming " + s.Ticket + " from the last stage…"
			arg := s.Ticket
			if s.SourcePath != "" {
				arg = s.SourcePath
			}
			return m, m.spawnRun(arg, "--resume")
		}
	case "ship":
		if s := m.selected(); s != nil {
			m.notice = "opening a PR for " + s.Ticket + " without re-verifying…"
			arg := s.Ticket
			if s.SourcePath != "" {
				arg = s.SourcePath
			}
			return m, m.spawnRun(arg, "--ship")
		}
	case "rerun":
		if s := m.selected(); s != nil {
			m.notice = "starting " + s.Ticket + " fresh…"
			arg := s.Ticket
			if s.SourcePath != "" {
				arg = s.SourcePath
			}
			return m, m.spawnRun(arg)
		}
	case "pause":
		if s := m.selected(); s != nil {
			m.notice = "pausing " + s.Ticket + " - moving to NEEDS YOU…"
			return m, m.pauseAgent(*s)
		}
	case "stop":
		if s := m.selected(); s != nil {
			m.confirming = s.Ticket
			m.notice = ""
		}
	// --- global ---
	case "run":
		m.view = viewRunMethod
		m.runMode = ""
		m.runMethodCursor = 0
		m.runText = ""
		m.runTickets = nil
		m.runIDPrompt = -1
		m.runImgPrompt = -1
		m.notice = ""
	case "doctor":
		m.notice = "running doctor…"
		return m, m.runDoctor()
	case "config":
		m.openConfigForm()
	case "setup":
		m.openSetupForm()
	case "daemon-start":
		m.notice = "starting daemon…"
		return m, m.startDaemon()
	case "daemon-stop":
		m.notice = "stopping daemon…"
		return m, m.stopDaemon()
	case "menu":
		m.view = viewPalette
		m.paletteCursor = 0
		m.notice = ""
	case "refresh":
		m.reload()
	case "quit":
		return m, tea.Quit
	// --- confirmations ---
	case "confirm-yes":
		return m.updateConfirming(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	case "confirm-no":
		m.confirming = ""
	}
	return m, nil
}
