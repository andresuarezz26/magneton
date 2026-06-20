package cmd

import (
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/andresuarezz26/magneton/internal/paths"
	"github.com/andresuarezz26/magneton/internal/store"
)

// agentActions returns the contextual menu items for one agent (shown when the
// user presses Enter on it). Global commands are appended by paletteItems.
func agentActions(s store.Session) []paletteItem {
	var items []paletteItem
	switch {
	case s.State == "awaiting-answer":
		items = append(items, paletteItem{"answer", "Answer the questions", "reply, then the agent resumes"})
	case s.State == "needs-you" || s.State == "failed" || s.State == store.StateStopped || isStopped(s):
		items = append(items, paletteItem{"resume", "Resume (verify & ship)", "re-run the gate on your fix, then PR"})
	}
	items = append(items,
		paletteItem{"studio", "Open Android Studio", "edit the worktree by hand"},
		paletteItem{"claude", "Open Claude Code", "resume the agent's session in the worktree"},
	)
	if s.State != "review" && s.State != "merged" && s.State != "closed" {
		items = append(items, paletteItem{"stop", "Stop & clean up", "kill the process and remove the worktree"})
	}
	return items
}

// claudeClosedMsg is returned after an interactive Claude Code session exits.
type claudeClosedMsg struct{ err error }

// openClaude hands the terminal to an interactive Claude Code session in the
// ticket's worktree, resuming the agent's stored session when there is one.
// tea.ExecProcess suspends the TUI and restores it when claude exits.
func (m monitorModel) openClaude(s store.Session) tea.Cmd {
	args := []string{}
	if s.SessionID != "" {
		args = []string{"--resume", s.SessionID}
	}
	c := exec.Command("claude", args...)
	c.Dir = paths.WorktreeFor(s.Ticket)
	return tea.ExecProcess(c, func(err error) tea.Msg { return claudeClosedMsg{err: err} })
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
			_ = exec.Command("open", "-a", "Android Studio", paths.WorktreeFor(s.Ticket)).Start()
			m.notice = "opening Android Studio…"
		}
	case "claude":
		if s := m.selected(); s != nil {
			return m, m.openClaude(*s)
		}
	case "resume":
		if s := m.selected(); s != nil {
			m.notice = "resuming " + s.Ticket + " (verify & ship)…"
			return m, m.launchRun(s.Ticket + " --resume")
		}
	case "stop":
		if s := m.selected(); s != nil {
			m.confirming = s.Ticket
			m.notice = ""
		}
	// --- global ---
	case "run":
		m.view = viewRunInput
		m.runText = ""
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
