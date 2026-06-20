package cmd

import (
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/andresuarezz26/magneton/internal/paths"
	"github.com/andresuarezz26/magneton/internal/store"
)

// actionBtn is one explicit call-to-action rendered as a labeled button. The key
// still works for keyboard users; the label + click are for everyone else.
type actionBtn struct {
	label   string
	key     string
	id      string
	primary bool
}

type hitbox struct {
	id     string
	x0, x1 int // inclusive cell-column range on the action-bar row
}

var (
	btnStyle   = lipgloss.NewStyle().Padding(0, 1).Background(lipgloss.Color("238")).Foreground(lipgloss.Color("253"))
	btnPrimary = lipgloss.NewStyle().Padding(0, 1).Background(lipgloss.Color("39")).Foreground(lipgloss.Color("231")).Bold(true)
)

// currentActions returns the explicit CTA buttons for the current view/state.
func (m monitorModel) currentActions() []actionBtn {
	if m.confirming != "" {
		return []actionBtn{
			{label: "Yes, stop it", key: "y", id: "confirm-yes", primary: true},
			{label: "Cancel", key: "n", id: "confirm-no"},
		}
	}
	if m.answering {
		return []actionBtn{
			{label: "Send & resume", key: "↵", id: "submit", primary: true},
			{label: "Cancel", key: "esc", id: "cancel"},
		}
	}
	switch m.view {
	case viewForm:
		return []actionBtn{{label: "Save", key: "↵", id: "submit", primary: true}, {label: "Cancel", key: "esc", id: "cancel"}}
	case viewRunInput:
		return []actionBtn{{label: "Launch", key: "↵", id: "submit", primary: true}, {label: "Cancel", key: "esc", id: "cancel"}}
	case viewPalette:
		return []actionBtn{{label: "Run", key: "↵", id: "submit", primary: true}, {label: "Close", key: "esc", id: "cancel"}}
	case viewOutput:
		return []actionBtn{{label: "Close", key: "esc", id: "cancel", primary: true}}
	}

	// Dashboard: contextual buttons for the selected agent + global actions.
	var btns []actionBtn
	if s := m.selected(); s != nil {
		switch {
		case s.State == "awaiting-answer":
			btns = append(btns, actionBtn{label: "Answer", key: "↵", id: "answer", primary: true})
		case s.State == "needs-you" || s.State == "failed" || s.State == store.StateStopped || isStopped(*s):
			btns = append(btns, actionBtn{label: "Resume", key: "R", id: "resume", primary: true})
		}
		btns = append(btns,
			actionBtn{label: "Open Android Studio", key: "o", id: "studio"},
			actionBtn{label: "Open Claude Code", key: "c", id: "claude"},
		)
		if s.State != "review" && s.State != "merged" && s.State != "closed" {
			btns = append(btns, actionBtn{label: "Stop", key: "x", id: "stop"})
		}
	}
	return append(btns,
		actionBtn{label: "Run new", key: "n", id: "run"},
		actionBtn{label: "Menu", key: ":", id: "menu"},
		actionBtn{label: "Quit", key: "q", id: "quit"},
	)
}

// renderActionBar draws the buttons and returns their click hitboxes.
func (m monitorModel) renderActionBar() (string, []hitbox) {
	var sb strings.Builder
	var boxes []hitbox
	x := 1
	sb.WriteString(" ")
	for i, b := range m.currentActions() {
		label := b.label
		if b.key != "" {
			label = b.label + " " + b.key
		}
		st := btnStyle
		if b.primary {
			st = btnPrimary
		}
		rendered := st.Render(label)
		wdt := lipgloss.Width(rendered)
		boxes = append(boxes, hitbox{id: b.id, x0: x, x1: x + wdt - 1})
		sb.WriteString(rendered)
		x += wdt
		if i < len(m.currentActions())-1 {
			sb.WriteString(" ")
			x++
		}
	}
	return sb.String(), boxes
}

// handleMouse dispatches a left-click on the action bar to the matching action.
func (m monitorModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, nil
	}
	if m.height > 0 && msg.Y != m.height-1 {
		return m, nil // clicks land only on the bottom action bar
	}
	_, boxes := m.renderActionBar()
	for _, b := range boxes {
		if msg.X >= b.x0 && msg.X <= b.x1 {
			return m.doAction(b.id)
		}
	}
	return m, nil
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

// doAction runs a CTA by id. Buttons (click) and keys both route here.
func (m monitorModel) doAction(id string) (tea.Model, tea.Cmd) {
	switch id {
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
	case "run":
		m.view = viewRunInput
		m.runText = ""
		m.notice = ""
	case "menu":
		m.view = viewPalette
		m.paletteCursor = 0
		m.notice = ""
	case "refresh":
		m.reload()
	case "quit":
		return m, tea.Quit
	case "confirm-yes":
		return m.updateConfirming(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	case "confirm-no":
		m.confirming = ""
	case "submit":
		return m.dispatchKey(tea.KeyMsg{Type: tea.KeyEnter})
	case "cancel":
		return m.dispatchKey(tea.KeyMsg{Type: tea.KeyEsc})
	}
	return m, nil
}
