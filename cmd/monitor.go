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
	"github.com/andresuarezz26/magneton/internal/git"
	"github.com/andresuarezz26/magneton/internal/jira"
	"github.com/andresuarezz26/magneton/internal/paths"
	"github.com/andresuarezz26/magneton/internal/secrets"
	"github.com/andresuarezz26/magneton/internal/store"
	"github.com/andresuarezz26/magneton/internal/telemetry"
)

// stoppedAfter is how long a running-state session can be idle (no log/state
// activity) before the monitor treats its process as gone. Generous so a slow
// Gradle gate isn't mislabeled; the real dead ones sit idle for hours/days.
const stoppedAfter = 10 * time.Minute

func init() {
	c := &cobra.Command{
		Use:     "monitor",
		Aliases: []string{"top"},
		Short:   "Live TUI hub: watch agents and run every command (also the default `agent`)",
		RunE:    func(_ *cobra.Command, _ []string) error { return launchHub() },
	}
	rootCmd.AddCommand(c)
}

// launchHub opens the TUI hub. Shared by bare `agent` and `agent monitor`/`top`.
func launchHub() error {
	st, err := store.Open(paths.StateDB())
	if err != nil {
		return err
	}
	defer st.Close()

	// Best-effort Jira client for answer-and-resume; nil if not configured.
	var jc *jira.Client
	cfg, cfgErr := config.Load()
	if cfgErr == nil && cfg.JiraBaseURL != "" {
		jc = jira.New(cfg.JiraBaseURL, cfg.JiraEmail, secrets.Get(secrets.Jira))
	}

	tel := &telemetry.Client{}
	defer tel.Flush()

	initialView := viewDashboard
	if cfgErr == nil && cfg.TelemetryEnabled != nil {
		if *cfg.TelemetryEnabled {
			tel.Configure(true, cfg.DeviceID, telemetry.Version)
			tel.Track("tui_opened", nil)
		}
	} else if cfgErr == nil {
		// Consent not yet given — show the consent screen first.
		initialView = viewConsent
	}

	self, _ := os.Executable()
	if self == "" {
		self = "magneton"
	}

	m := monitorModel{store: st, jira: jc, tel: tel, selfPath: self, view: initialView}
	m.reload()
	_, err = tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
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

// pidAlive reports whether process pid currently exists (deterministic, via
// signal 0). EPERM means it exists but we can't signal it — still alive.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// isStopped: a running-state session whose process is gone. When we know the
// pid (recorded at run start) the check is deterministic; for older rows with
// no pid we fall back to the activity heuristic.
func isStopped(s store.Session) bool {
	if !isRunningState(s.State) {
		return false
	}
	if s.PID > 0 {
		return !pidAlive(s.PID)
	}
	return time.Since(freshest(s)) > stoppedAfter
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
		{label: "STOPPED", style: orange, match: func(s store.Session) bool {
			return s.State == store.StateStopped || isStopped(s)
		}},
		{label: "RUNNING", style: cyan, match: func(s store.Session) bool {
			return isRunningState(s.State) && !isStopped(s)
		}},
		{label: "DONE", style: green, match: func(s store.Session) bool {
			return s.State == "review" || s.State == "merged" || s.State == "closed"
		}},
	}
}

func glyphFor(s store.Session) string {
	if s.State == store.StateStopped || isStopped(s) {
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
	tel         *telemetry.Client
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

	// stop/cleanup confirmation; non-empty = awaiting y/n for this ticket
	confirming string

	// hub views (palette / run-input / doctor output / form). dashboard = zero value.
	view          hubView
	paletteCursor int
	runText       string // run-new input buffer
	outputTitle   string
	outputText    string
	form          *formModel // active form (config/setup), nil otherwise
}

// answerDoneMsg is returned by submitAnswer once the answer is written and the
// agent relaunched (or on failure).
type answerDoneMsg struct {
	ticket string
	err    error
}

// cancelDoneMsg is returned once an agent has been stopped + cleaned up.
type cancelDoneMsg struct{ ticket string }

// pauseDoneMsg is returned once a live run has been paused into NEEDS YOU.
type pauseDoneMsg struct{ ticket string }

// consentDoneMsg carries the result of the telemetry consent save.
type consentDoneMsg struct {
	enabled  bool
	deviceID string
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
	if m.cursor > len(flat)+1 { // 0=Start-new 1=Edit-config agents=2..len(flat)+1
		m.cursor = len(flat) + 1
	}
}

// selected returns the highlighted agent, or nil when the cursor is on one of
// the two pinned rows (0 = Start new, 1 = Edit config). Agents occupy 2..len(flat)+1.
func (m *monitorModel) selected() *store.Session {
	if m.cursor <= 1 || m.cursor > len(m.flat)+1 {
		return nil
	}
	return &m.flat[m.cursor-2]
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
	case cancelDoneMsg:
		m.notice = "stopped " + msg.ticket + " — process killed, worktree removed"
		m.reload()
		return m, nil
	case pauseDoneMsg:
		m.notice = "paused " + msg.ticket + " — agent stopped, worktree kept (NEEDS YOU)"
		m.reload()
		return m, nil
	case doctorDoneMsg:
		m.outputTitle = "doctor — connectivity check"
		m.outputText = msg.out
		m.view = viewOutput
		return m, nil
	case formDoneMsg:
		if msg.err != nil {
			m.notice = "save failed: " + msg.err.Error()
		} else {
			m.notice = msg.notice
		}
		m.reload()
		return m, nil
	case consentDoneMsg:
		m.tel.Configure(msg.enabled, msg.deviceID, telemetry.Version)
		if msg.enabled {
			m.tel.Track("tui_opened", nil)
			m.notice = "telemetry enabled — thanks for helping!"
		}
		m.view = viewDashboard
		return m, nil
	case runLaunchedMsg:
		if msg.err != nil {
			m.notice = "run failed: " + msg.err.Error()
		} else {
			m.notice = "launched run: " + msg.text
		}
		m.reload()
		return m, nil
	case daemonMsg:
		if msg.err != nil {
			m.notice = msg.action + " daemon: " + msg.err.Error()
		} else {
			m.notice = "daemon " + msg.action + "ed"
		}
		return m, nil
	case claudeClosedMsg:
		if msg.err != nil {
			m.notice = "open Claude Code: " + msg.err.Error()
		} else {
			m.notice = "opened Claude Code in a new terminal"
		}
		return m, nil
	case tea.KeyMsg:
		return m.dispatchKey(msg)
	}
	return m, nil
}

// dispatchKey routes a keystroke (real or synthesized by a button click) to the
// active sub-view, else the dashboard keymap. Button-backed actions go through
// doAction so keys and clicks stay in sync.
func (m monitorModel) dispatchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// View-specific handlers take precedence over the dashboard keymap.
	if m.answering {
		return m.updateAnswering(msg)
	}
	if m.confirming != "" {
		return m.updateConfirming(msg)
	}
	switch m.view {
	case viewConsent:
		return m.updateConsent(msg)
	case viewPalette:
		return m.updatePalette(msg)
	case viewRunInput:
		return m.updateRunInput(msg)
	case viewOutput:
		return m.updateOutput(msg)
	case viewForm:
		return m.updateForm(msg)
	}
	switch msg.String() {
	case "q", "ctrl+c", "esc":
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.flat)+1 { // 0=Start-new 1=Edit-config agents=2..len(flat)+1
			m.cursor++
		}
	case ":":
		return m.doAction("menu")
	case "n":
		return m.doAction("run")
	case "r":
		return m.doAction("refresh")
	case "o":
		return m.doAction("studio")
	case "c":
		return m.doAction("claude")
	case "enter":
		if m.cursor == 0 {
			return m.doAction("run")
		}
		if m.cursor == 1 {
			return m.doAction("config")
		}
		return m.doAction("menu")
	case "a":
		return m.doAction("answer")
	case "x":
		return m.doAction("stop")
	case "R":
		return m.doAction("resume")
	}
	return m, nil
}

// updateConfirming handles the y/n stop-confirmation prompt.
func (m monitorModel) updateConfirming(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		key := m.confirming
		m.confirming = ""
		var target *store.Session
		for i := range m.flat {
			if m.flat[i].Ticket == key {
				target = &m.flat[i]
				break
			}
		}
		if target == nil {
			return m, nil
		}
		m.notice = "stopping " + key + "…"
		return m, m.cancelAgent(*target)
	case "n", "N", "esc", "q":
		m.confirming = ""
	}
	return m, nil
}

// pauseAgent halts a live run: it kills the driving process (and its process
// group, so the child claude stops too) but KEEPS the worktree, and drops the
// session to NEEDS YOU so the human can take over by hand and later resume.
func (m monitorModel) pauseAgent(s store.Session) tea.Cmd {
	st := m.store
	return func() tea.Msg {
		if s.PID > 0 && pidAlive(s.PID) {
			// Kill the whole process group first (TUI-launched runs are group
			// leaders, so this also stops the child claude), then the pid itself as
			// a fallback for runs not started as a group leader.
			_ = syscall.Kill(-s.PID, syscall.SIGTERM)
			_ = syscall.Kill(s.PID, syscall.SIGTERM)
		}
		_ = st.SetState(s.Ticket, store.StateNeedsYou, s.Retries)
		return pauseDoneMsg{ticket: s.Ticket}
	}
}

// cancelAgent kills the driving process (if alive), removes the worktree, and
// marks the session stopped so it leaves NEEDS YOU for STOPPED.
func (m monitorModel) cancelAgent(s store.Session) tea.Cmd {
	st := m.store
	return func() tea.Msg {
		if s.PID > 0 && pidAlive(s.PID) {
			_ = syscall.Kill(s.PID, syscall.SIGTERM)
		}
		wt := s.Worktree
		if wt == "" {
			wt = paths.WorktreeFor(s.Repo, s.Ticket)
		}
		if s.Repo != "" {
			_ = git.RemoveWorktree(s.Repo, wt)
		}
		_ = st.SetState(s.Ticket, store.StateStopped, s.Retries)
		return cancelDoneMsg{ticket: s.Ticket}
	}
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

// submitAnswer appends the answer to the .md source file and relaunches. This
// only works for local tickets (SourcePath set). For Jira tickets the action
// is not offered — use "Open in Claude Code" to answer in the session directly.
func (m monitorModel) submitAnswer(key, answer string) tea.Cmd {
	self, st := m.selfPath, m.store
	return func() tea.Msg {
		var sourcePath string
		if st != nil {
			if sess, err := st.Get(key); err == nil && sess != nil {
				sourcePath = sess.SourcePath
			}
		}
		if sourcePath == "" {
			return answerDoneMsg{ticket: key, err: fmt.Errorf("answer via TUI only works for local .md tickets — use \"Open in Claude Code\" to answer in the session")}
		}
		raw, err := os.ReadFile(sourcePath)
		if err != nil {
			return answerDoneMsg{ticket: key, err: fmt.Errorf("read %s: %w", sourcePath, err)}
		}
		updated := strings.TrimSpace(string(raw)) + "\n\n---\nAnswers:\n" + answer
		if err := os.WriteFile(sourcePath, []byte(updated+"\n"), 0o644); err != nil {
			return answerDoneMsg{ticket: key, err: fmt.Errorf("write %s: %w", sourcePath, err)}
		}
		c := exec.Command(self, "run", sourcePath)
		c.Stdout, c.Stderr = nil, nil
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := c.Start(); err != nil {
			return answerDoneMsg{ticket: key, err: err}
		}
		return answerDoneMsg{ticket: key}
	}
}

// ---- telemetry consent -----------------------------------------------------

func (m monitorModel) updateConsent(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch strings.ToLower(msg.String()) {
	case "y":
		return m, func() tea.Msg { return applyConsent(true) }
	case "n", "esc", "q":
		return m, func() tea.Msg { return applyConsent(false) }
	case "ctrl+c":
		return m, tea.Quit
	}
	return m, nil
}

func applyConsent(enabled bool) tea.Msg {
	cfg, err := config.Load()
	if err != nil {
		cfg = &config.Config{}
	}
	cfg.TelemetryEnabled = &enabled
	if enabled && cfg.DeviceID == "" {
		cfg.DeviceID = config.GenerateDeviceID()
	}
	_ = config.Save(cfg)
	return consentDoneMsg{enabled: enabled, deviceID: cfg.DeviceID}
}

func (m monitorModel) renderConsent(w int) string {
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(headerStyle.Render("  Help improve magneton?") + "\n\n")
	b.WriteString("  Share anonymous usage data so we can understand how the tool is used.\n")
	b.WriteString("  We never collect ticket content, file paths, or personal information.\n\n")
	b.WriteString("  What gets shared:\n")
	b.WriteString(dimStyle.Render("    • which commands run (run, doctor, etc.)") + "\n")
	b.WriteString(dimStyle.Render("    • run outcome (success / failed / needs-human)") + "\n")
	b.WriteString(dimStyle.Render("    • OS type and magneton version") + "\n\n")
	b.WriteString("  " + ctaStyle.Render(" Y ") + "  yes — help make magneton better\n\n")
	b.WriteString("  " + dimStyle.Render("N") + "  no thanks\n")
	return b.String()
}

// ---- view ------------------------------------------------------------------

var (
	headerStyle = lipgloss.NewStyle().Bold(true)
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	selStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("231")).Background(lipgloss.Color("236"))
	sepStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	whyStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	ctaStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("231")).Background(lipgloss.Color("36")).Bold(true).Padding(0, 1)
	ctaSelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("36")).Background(lipgloss.Color("231")).Bold(true).Padding(0, 1)
)

func (m monitorModel) View() string {
	w := m.width
	if w == 0 {
		w = 80
	}
	var b strings.Builder

	needs, running := 0, 0
	for _, g := range m.groups {
		switch g.label {
		case "NEEDS YOU":
			needs += len(g.sessions)
		case "RUNNING":
			running += len(g.sessions)
		}
	}
	daemon := "○ daemon stopped"
	if pid, ok := daemonAlive(); ok {
		daemon = fmt.Sprintf("● daemon pid %d", pid)
	}
	b.WriteString(headerStyle.Render(fmt.Sprintf("magneton · %d running · %d needs you", running, needs)))
	b.WriteString(dimStyle.Render("   "+m.lastRefresh.Format("15:04:05")+"  ·  "+daemon) + "\n")

	if m.err != nil {
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Render("  error: "+m.err.Error()) + "\n")
		return b.String()
	}

	// Body for the active view; the action bar is appended at the bottom by frame.
	var body string
	switch m.view {
	case viewConsent:
		body = m.renderConsent(w)
	case viewPalette:
		body = m.renderPalette(w)
	case viewRunInput:
		body = m.renderRunInput(w)
	case viewOutput:
		body = m.renderOutput(w)
	case viewForm:
		if m.form != nil {
			body = m.form.render(w)
		}
	default:
		body = m.renderDashboardBody(w)
	}

	notice := ""
	if m.confirming != "" {
		notice = whyStyle.Render(truncate("  Stop "+m.confirming+"? This kills its process and removes its worktree.", w))
	} else if m.notice != "" {
		notice = whyStyle.Render(truncate("  "+m.notice, w))
	}
	// Footer hint. Modal views render their own hints in the body.
	footer := ""
	if m.view == viewDashboard && !m.answering && m.confirming == "" {
		footer = dimStyle.Render("  ↑↓ select · enter: choose · : commands · q: quit")
	} else if m.view == viewConsent {
		footer = dimStyle.Render("  y: share · n: skip")
	} else if m.confirming != "" {
		footer = dimStyle.Render("  y: yes · n: no")
	} else if m.answering {
		footer = dimStyle.Render("  enter: send & resume · esc: cancel")
	}
	return m.frame(b.String(), body, notice, footer, w)
}

// renderDashboardBody renders the triaged agent list + detail pane (no footer).
func (m monitorModel) renderDashboardBody(w int) string {
	var b strings.Builder
	// Pinned rows: index 0 = Start new, index 1 = Edit config.
	if m.cursor == 0 {
		b.WriteString("  " + ctaSelStyle.Render("＋ Start new ticket(s)") + dimStyle.Render("   press enter") + "\n")
	} else {
		b.WriteString("  " + ctaStyle.Render("＋ Start new ticket(s)") + "\n")
	}
	if m.cursor == 1 {
		b.WriteString("  " + selStyle.Render(" ⚙  Edit config ") + dimStyle.Render("   press enter") + "\n\n")
	} else {
		b.WriteString("  " + dimStyle.Render(" ⚙  Edit config") + "\n\n")
	}

	if len(m.flat) == 0 {
		b.WriteString("  " + dimStyle.Render("no agents running yet — select the row above and press enter to start one"))
		return b.String()
	}
	idx := 2 // agents start at cursor index 2 (0=Start-new 1=Edit-config)
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

	sel := m.selected()
	if sel != nil {
		hdr := "─ " + sel.Ticket
		if sel.PID > 0 {
			hdr += fmt.Sprintf(" · pid %d", sel.PID)
			if pidAlive(sel.PID) {
				hdr += " alive"
			} else {
				hdr += " gone"
			}
		}
		b.WriteString(sepStyle.Render(truncate(hdr+" "+strings.Repeat("─", w), w)) + "\n")
		if sel.Summary != "" {
			b.WriteString(headerStyle.Render(truncate("  "+sel.Summary, w)) + "\n")
		}
		for _, ln := range whyLines(*sel) {
			b.WriteString(whyStyle.Render(truncate(ln, w)) + "\n")
		}
		if m.answering {
			b.WriteString("\n  " + headerStyle.Render("answer "+m.answerKey) + "\n")
			b.WriteString("  › " + m.input + "▌")
		} else {
			detailH := m.height - listLines - 10 - len(whyLines(*sel))
			if detailH < 3 {
				detailH = 3
			}
			lines := tailLines(paths.LogFor(sel.Ticket), detailH)
			if len(lines) == 0 {
				b.WriteString("  " + dimStyle.Render("(no log output yet)"))
			}
			for i, ln := range lines {
				b.WriteString(truncate(stripPrefix(ln, sel.Ticket), w))
				if i < len(lines)-1 {
					b.WriteString("\n")
				}
			}
		}
	}
	return b.String()
}

// frame composes header + body + a bottom-pinned footer (separator, optional
// notice, action bar). Padding keeps the action bar on the last row so clicks
// hit it (mouse Y == height-1).
func (m monitorModel) frame(head, body, notice, bar string, w int) string {
	var b strings.Builder
	b.WriteString(head)
	if !strings.HasSuffix(head, "\n") {
		b.WriteString("\n")
	}
	if body != "" {
		b.WriteString(strings.TrimRight(body, "\n") + "\n")
	}
	footerLines := 2 // separator + bar
	if notice != "" {
		footerLines++
	}
	if m.height > 0 {
		for i := strings.Count(b.String(), "\n") + footerLines; i < m.height; i++ {
			b.WriteString("\n")
		}
	}
	b.WriteString(sepStyle.Render(strings.Repeat("─", w)) + "\n")
	if notice != "" {
		b.WriteString(notice + "\n")
	}
	b.WriteString(bar)
	return b.String()
}

// whyLines explains, for a needs-you/stopped/failed agent, what it's blocked on.
func whyLines(s store.Session) []string {
	if isStopped(s) {
		reason := fmt.Sprintf("no activity for %s", age(freshest(s)))
		if s.PID > 0 {
			reason = fmt.Sprintf("process %d is gone", s.PID)
		}
		return []string{
			fmt.Sprintf("■ Stopped — %s. Re-run the ticket to resume.", reason),
			"  Press o to inspect the worktree.",
		}
	}
	switch s.State {
	case "awaiting-answer":
		if plan, err := agent.ReadPlan(paths.WorktreeFor(s.Repo, s.Ticket)); err == nil && len(plan.Questions) > 0 {
			out := []string{fmt.Sprintf("▮ Needs you — press ↵ enter to answer %d question(s):", len(plan.Questions))}
			for i, q := range plan.Questions {
				out = append(out, fmt.Sprintf("  Q%d %s", i+1, q))
			}
			return out
		}
		return []string{"▮ Needs you — press ↵ enter to respond (see log below)."}
	case "failed":
		return []string{
			"✗ Failed — " + failReason(s.Ticket),
			"  Open Android Studio (o) to fix, then press R to gate & open the PR.",
		}
	case "needs-you":
		return []string{
			"⚑ Needs you — the agent got stuck (see log below).",
			"  Open Android Studio (o) to fix, then press R to gate & open the PR.",
		}
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
	// Show the ticket title so each row says what work it is, at a glance.
	// Fall back to the latest log line only if there's no title.
	desc := s.Summary
	if desc == "" {
		desc = cleanActivity(tailLines(paths.LogFor(s.Ticket), 1), s.Ticket)
	}
	retries := ""
	if s.Retries > 0 {
		retries = fmt.Sprintf(" ×%d", s.Retries)
	}
	left := fmt.Sprintf("  %s %-9s %-11s", glyphFor(s), s.Ticket, stateLabel(s)+retries)
	right := fmt.Sprintf(" %4s", age(s.UpdatedAt))
	flex := w - lipgloss.Width(left) - lipgloss.Width(right) - 1
	if flex < 6 {
		flex = 6
	}
	return left + fmt.Sprintf(" %-*s", flex, truncate(desc, flex)) + right
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
