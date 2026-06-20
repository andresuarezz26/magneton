package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/andresuarezz26/magneton/internal/agent"
	"github.com/andresuarezz26/magneton/internal/config"
	"github.com/andresuarezz26/magneton/internal/jira"
	"github.com/andresuarezz26/magneton/internal/paths"
	"github.com/andresuarezz26/magneton/internal/secrets"
	"github.com/andresuarezz26/magneton/internal/store"
)

// stoppedAfter is how long a running-state session can be idle (no log/state
// activity) before the monitor treats its process as gone. Generous so a slow
// Gradle gate isn't mislabeled; the real dead ones sit idle for hours/days.
const stoppedAfter = 10 * time.Minute

func init() {
	c := &cobra.Command{
		Use:     "monitor",
		Aliases: []string{"top"},
		Short:   "Live TUI dashboard: watch agents, answer their questions, and resume them",
		RunE: func(_ *cobra.Command, _ []string) error {
			st, err := store.Open(paths.StateDB())
			if err != nil {
				return err
			}
			defer st.Close()

			// Best-effort Jira client for answer-and-resume; nil if not configured.
			var jc *jira.Client
			if cfg, cerr := config.Load(); cerr == nil && cfg.JiraBaseURL != "" {
				jc = jira.New(cfg.JiraBaseURL, cfg.JiraEmail, secrets.Get(secrets.Jira))
			}
			self, _ := os.Executable()
			if self == "" {
				self = "agent"
			}

			m := monitorModel{store: st, jira: jc, selfPath: self}
			m.reload()
			p := tea.NewProgram(m, tea.WithAltScreen())
			_, err = p.Run()
			return err
		},
	}
	rootCmd.AddCommand(c)
}

// ---- liveness & triage -----------------------------------------------------

func isRunningState(state string) bool {
	switch state {
	case "queued", "planning", "working", "reviewing", "building", "testing":
		return true
	}
	return false
}

// freshest is the most recent sign of life for a session: the later of its
// last state write and its log file's mtime (the log streams continuously
// while the agent is actually working).
func freshest(s store.Session) time.Time {
	t := s.UpdatedAt
	if fi, err := os.Stat(paths.LogFor(s.Ticket)); err == nil && fi.ModTime().After(t) {
		t = fi.ModTime()
	}
	return t
}

// isStopped: a running-state session whose process looks gone (idle too long).
func isStopped(s store.Session) bool {
	return isRunningState(s.State) && time.Since(freshest(s)) > stoppedAfter
}

type group struct {
	label    string
	style    lipgloss.Style
	match    func(store.Session) bool
	sessions []store.Session
}

func newGroups() []*group {
	red := lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Bold(true)
	orange := lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	cyan := lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	green := lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	return []*group{
		{label: "NEEDS YOU", style: red, match: func(s store.Session) bool {
			return s.State == "awaiting-answer" || s.State == "needs-you" || s.State == "failed"
		}},
		{label: "STOPPED", style: orange, match: isStopped},
		{label: "RUNNING", style: cyan, match: func(s store.Session) bool {
			return isRunningState(s.State) && !isStopped(s)
		}},
		{label: "DONE", style: green, match: func(s store.Session) bool {
			return s.State == "review" || s.State == "merged" || s.State == "closed"
		}},
	}
}

func glyphFor(s store.Session) string {
	if isStopped(s) {
		return "■"
	}
	switch s.State {
	case "awaiting-answer":
		return "▮"
	case "failed":
		return "✗"
	case "needs-you":
		return "⚑"
	case "review", "merged":
		return "✓"
	case "closed":
		return "·"
	default:
		return "●"
	}
}

func stateLabel(s store.Session) string {
	if isStopped(s) {
		return "stopped"
	}
	return s.State
}

// ---- model -----------------------------------------------------------------

type monitorModel struct {
	store       *store.Store
	jira        *jira.Client
	selfPath    string
	groups      []*group
	flat        []store.Session
	cursor      int
	width       int
	height      int
	lastRefresh time.Time
	err         error

	// answer-and-resume mode
	answering bool
	input     string
	answerKey string
	notice    string // transient status/error line under the footer
}

// answerDoneMsg is returned by submitAnswer once the answer is written and the
// agent relaunched (or on failure).
type answerDoneMsg struct {
	ticket string
	err    error
}

type tickMsg time.Time

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m monitorModel) Init() tea.Cmd { return tick() }

func (m *monitorModel) reload() {
	sessions, err := m.store.List()
	if err != nil {
		m.err = err
		return
	}
	m.err = nil
	m.lastRefresh = time.Now()

	groups := newGroups()
	for _, s := range sessions {
		for _, g := range groups {
			if g.match(s) {
				g.sessions = append(g.sessions, s)
				break
			}
		}
	}
	var flat []store.Session
	for _, g := range groups {
		flat = append(flat, g.sessions...)
	}
	m.groups = groups
	m.flat = flat
	if m.cursor >= len(flat) {
		m.cursor = max(0, len(flat)-1)
	}
}

func (m *monitorModel) selected() *store.Session {
	if m.cursor < 0 || m.cursor >= len(m.flat) {
		return nil
	}
	return &m.flat[m.cursor]
}

func (m monitorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tickMsg:
		m.reload()
		return m, tick()
	case answerDoneMsg:
		if msg.err != nil {
			m.notice = "answer failed: " + msg.err.Error()
		} else {
			m.notice = "answer sent to " + msg.ticket + " — resuming…"
		}
		return m, nil
	case tea.KeyMsg:
		if m.answering {
			return m.updateAnswering(msg)
		}
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.flat)-1 {
				m.cursor++
			}
		case "r":
			m.reload()
		case "o":
			if s := m.selected(); s != nil {
				_ = exec.Command("open", paths.WorktreeFor(s.Ticket)).Start()
			}
		case "enter", "a":
			if s := m.selected(); s != nil && s.State == "awaiting-answer" {
				m.answering = true
				m.input = ""
				m.answerKey = s.Ticket
				m.notice = ""
			}
		}
	}
	return m, nil
}

// updateAnswering handles keystrokes while the answer input box is open.
func (m monitorModel) updateAnswering(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		if strings.TrimSpace(m.input) == "" {
			m.answering = false
			return m, nil
		}
		key, answer := m.answerKey, m.input
		m.answering = false
		m.notice = "sending answer to " + key + "…"
		return m, m.submitAnswer(key, answer)
	case tea.KeyEsc:
		m.answering = false
		m.input = ""
	case tea.KeyBackspace:
		if r := []rune(m.input); len(r) > 0 {
			m.input = string(r[:len(r)-1])
		}
	case tea.KeySpace:
		m.input += " "
	case tea.KeyRunes:
		m.input += string(msg.Runes)
	default:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}
	return m, nil
}

// submitAnswer writes the answer into the Jira description and relaunches the
// agent so it resumes; its output streams to the log the monitor already tails.
func (m monitorModel) submitAnswer(key, answer string) tea.Cmd {
	jc, self := m.jira, m.selfPath
	return func() tea.Msg {
		if jc == nil {
			return answerDoneMsg{ticket: key, err: fmt.Errorf("no Jira configured (local .md answering not yet supported)")}
		}
		issue, err := jc.FetchIssue(key)
		if err != nil {
			return answerDoneMsg{ticket: key, err: err}
		}
		newDesc := strings.TrimSpace(issue.Description) +
			"\n\n---\nAnswers (via magneton monitor):\n" + answer
		if err := jc.SetDescription(key, newDesc); err != nil {
			return answerDoneMsg{ticket: key, err: err}
		}
		// Relaunch detached; output is discarded from this TUI but the run
		// writes to ~/.agent/logs/<key>.log, which the monitor tails live.
		c := exec.Command(self, "run", key)
		c.Stdout, c.Stderr = nil, nil
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := c.Start(); err != nil {
			return answerDoneMsg{ticket: key, err: err}
		}
		return answerDoneMsg{ticket: key}
	}
}

// ---- view ------------------------------------------------------------------

var (
	headerStyle = lipgloss.NewStyle().Bold(true)
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	selStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("231")).Background(lipgloss.Color("236"))
	sepStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	whyStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
)

func (m monitorModel) View() string {
	w := m.width
	if w == 0 {
		w = 80
	}
	var b strings.Builder

	needs := 0
	for _, g := range m.groups {
		if g.label == "NEEDS YOU" || g.label == "STOPPED" {
			needs += len(g.sessions)
		}
	}
	b.WriteString(headerStyle.Render(fmt.Sprintf("magneton · %d agents · %d need you", len(m.flat), needs)))
	b.WriteString(dimStyle.Render("   "+m.lastRefresh.Format("15:04:05")) + "\n")

	if m.err != nil {
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Render("  error: "+m.err.Error()) + "\n")
		return b.String()
	}
	if len(m.flat) == 0 {
		b.WriteString("\n  " + dimStyle.Render("no agents yet — run `agent run <TICKET|FILE>` or `agent start`") + "\n")
		return b.String()
	}

	idx := 0
	listLines := 0
	for _, g := range m.groups {
		if len(g.sessions) == 0 {
			continue
		}
		b.WriteString(g.style.Render(fmt.Sprintf("▾ %s (%d)", g.label, len(g.sessions))) + "\n")
		listLines++
		for _, s := range g.sessions {
			row := m.renderRow(s, w)
			if idx == m.cursor {
				row = selStyle.Render(row)
			}
			b.WriteString(row + "\n")
			idx++
			listLines++
		}
	}

	// Detail pane: why-it-needs-you header + tail of the selected agent's log.
	sel := m.selected()
	if sel != nil {
		b.WriteString(sepStyle.Render(truncate("─ "+sel.Ticket+" "+strings.Repeat("─", w), w)) + "\n")

		why := whyLines(*sel)
		for _, ln := range why {
			b.WriteString(whyStyle.Render(truncate(ln, w)) + "\n")
		}

		if m.answering {
			// Focused input box; the log tail is hidden to keep attention here.
			b.WriteString("\n  " + headerStyle.Render("answer "+m.answerKey) + "\n")
			b.WriteString("  › " + m.input + "▌\n")
			b.WriteString("  " + dimStyle.Render("[enter] send & resume · [esc] cancel") + "\n")
		} else {
			detailH := m.height - listLines - 6 - len(why)
			if detailH < 3 {
				detailH = 3
			}
			lines := tailLines(paths.LogFor(sel.Ticket), detailH)
			if len(lines) == 0 {
				b.WriteString("  " + dimStyle.Render("(no log output yet)") + "\n")
			}
			for _, ln := range lines {
				b.WriteString(truncate(stripPrefix(ln, sel.Ticket), w) + "\n")
			}
		}
	}

	b.WriteString(sepStyle.Render(strings.Repeat("─", w)) + "\n")
	if m.notice != "" {
		b.WriteString(whyStyle.Render(truncate("  "+m.notice, w)) + "\n")
	}
	hint := "↑↓ select · ↵ answer · o open worktree · r refresh · q quit · live every 1s"
	if m.answering {
		hint = "typing… [enter] send & resume · [esc] cancel"
	}
	b.WriteString(dimStyle.Render(hint))
	return b.String()
}

// whyLines explains, for a needs-you/stopped/failed agent, what it's blocked on.
func whyLines(s store.Session) []string {
	if isStopped(s) {
		return []string{
			fmt.Sprintf("■ Stopped — no activity for %s; the process running this looks gone.", age(freshest(s))),
			"  Re-run the ticket to resume, or press o to inspect the worktree.",
		}
	}
	switch s.State {
	case "awaiting-answer":
		if plan, err := agent.ReadPlan(paths.WorktreeFor(s.Ticket)); err == nil && len(plan.Questions) > 0 {
			out := []string{fmt.Sprintf("▮ Needs you — press ↵ enter to answer %d question(s):", len(plan.Questions))}
			for i, q := range plan.Questions {
				out = append(out, fmt.Sprintf("  Q%d %s", i+1, q))
			}
			return out
		}
		return []string{"▮ Needs you — press ↵ enter to respond (see log below)."}
	case "failed":
		return []string{"✗ Failed — " + failReason(s.Ticket)}
	case "needs-you":
		return []string{"⚑ Needs you — the agent got stuck (see log below)."}
	}
	return nil
}

// failReason scrapes the most recent failure line from the log tail.
func failReason(ticket string) string {
	lines := tailLines(paths.LogFor(ticket), 50)
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.ToLower(lines[i])
		if strings.Contains(l, "fail") || strings.Contains(l, "error") || strings.Contains(l, "still red") {
			return truncate(strings.TrimSpace(stripPrefix(lines[i], ticket)), 100)
		}
	}
	return "see log below"
}

func (m monitorModel) renderRow(s store.Session, w int) string {
	activity := cleanActivity(tailLines(paths.LogFor(s.Ticket), 1), s.Ticket)
	if activity == "" {
		activity = s.Summary
	}
	retries := ""
	if s.Retries > 0 {
		retries = fmt.Sprintf(" ×%d", s.Retries)
	}
	left := fmt.Sprintf("  %s %-8s %-11s", glyphFor(s), s.Ticket, stateLabel(s)+retries)
	right := fmt.Sprintf(" %4s", age(s.UpdatedAt))
	flex := w - lipgloss.Width(left) - lipgloss.Width(right) - 1
	if flex < 6 {
		flex = 6
	}
	return left + fmt.Sprintf(" %-*s", flex, truncate(activity, flex)) + right
}

// ---- helpers ---------------------------------------------------------------

// tailLines returns up to the last n non-blank lines of a file (reading ≤64KB).
func tailLines(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	const maxRead = 64 * 1024
	start := int64(0)
	if fi.Size() > maxRead {
		start = fi.Size() - maxRead
	}
	buf := make([]byte, fi.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil {
		return nil
	}
	var out []string
	for _, ln := range strings.Split(strings.TrimRight(string(buf), "\n"), "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

func cleanActivity(lines []string, ticket string) string {
	if len(lines) == 0 {
		return ""
	}
	s := stripPrefix(lines[len(lines)-1], ticket)
	s = strings.TrimLeft(s, " •⚙─-")
	return strings.TrimSpace(s)
}

func stripPrefix(line, ticket string) string {
	return strings.TrimPrefix(line, "["+ticket+"] ")
}

func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	if w <= 1 {
		return "…"
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r))+1 > w {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}
