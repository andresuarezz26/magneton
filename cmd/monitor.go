package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
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
	if err := paths.EnsureDirs(); err != nil {
		return fmt.Errorf("cannot create ~/.magneton directory: %w\nRun `magneton init` to configure your setup", err)
	}
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
		// Consent not yet given - show the consent screen first.
		initialView = viewConsent
	}

	self, _ := os.Executable()
	if self == "" {
		self = "magneton"
	}

	m := monitorModel{
		store: st, jira: jc, tel: tel, selfPath: self, view: initialView,
		runIDPrompt: -1, runImgPrompt: -1, runStackPrompt: -1, runReviewPrompt: -1,
	}
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
// signal 0). EPERM means it exists but we can't signal it - still alive.
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
			return s.State == "awaiting-answer" || s.State == "needs-you" ||
				s.State == "failed" || s.State == store.StatePlanReview
		}},
		{label: "STOPPED", style: orange, match: func(s store.Session) bool {
			return s.State == store.StateStopped || isStopped(s)
		}},
		{label: "RUNNING", style: cyan, match: func(s store.Session) bool {
			return isRunningState(s.State) && !isStopped(s)
		}},
		{label: "READY FOR REVIEW", style: green, match: func(s store.Session) bool {
			return s.State == "review" || s.State == "merged" || s.State == "closed"
		}},
	}
}

func glyphFor(s store.Session) string {
	if s.State == store.StateStopped || isStopped(s) {
		return "■"
	}
	switch s.State {
	case "awaiting-answer", store.StatePlanReview:
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
	if s.State == store.StateReview {
		return "under review"
	}
	if s.State == store.StatePlanReview {
		return "plan ready"
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

	// answer-and-resume mode. The answer is an ordered list of atoms - typed
	// runes and pasted blobs interleaved - so a paste shows inline as
	// "[N lines added]" right where the cursor was, exactly like Claude Code.
	answering    bool
	answerKey    string
	answerAtoms  []inputAtom // typed chars + paste placeholders, in order
	answerCursor int         // index into answerAtoms (0..len)
	notice       string      // transient status/error line under the footer

	// stop/cleanup confirmation; non-empty = awaiting selection for this ticket
	confirming    string
	confirmCursor int // 0 = "Yes, stop" / 1 = "No, keep running"

	// hub views (palette / run-input / doctor output / form). dashboard = zero value.
	view            hubView
	paletteCursor   int
	runMode         string          // "" | content | jira | file (active run-input method)
	runMethodCursor int             // cursor in the run-method picker
	runText         string          // run-new typed buffer (a key/path/id being typed)
	runTickets      []pendingTicket // accumulated ticket chips awaiting launch
	runIDPrompt     int             // index of a content chip confirming its id; -1 = none
	runImgPrompt    int             // index of a content chip attaching images; -1 = none
	runStackPrompt  int             // index of a chip choosing its stack base; -1 = none
	runReviewPrompt int             // index of a chip choosing its plan-review toggle; -1 = none
	reviewCursor    int             // cursor in the plan-review mini palette (0=Yes 1=No)
	stackBranches   []git.Branch    // loaded once when the stack picker opens
	stackDefault    string          // repo's default branch name (main/master/…), for the default row
	stackFilter     string          // search filter in the stack picker
	stackCursor     int             // cursor row in the filtered branch list
	outputTitle     string
	outputText      string
	form            *formModel // active form (config/setup), nil otherwise

	// full-screen plan viewer (viewPlan). planLines is the glamour-rendered
	// markdown, pre-split into rows and re-rendered on resize; planScroll is the
	// top visible row. planMenu is the top action selection (0=feedback 1=approve).
	// When planFeedback is on, an inline input lets the user type corrections while
	// the plan stays visible above.
	planTicket   string
	planScroll   int
	planLines    []string
	planMenu     int
	planFeedback bool
	planFbAtoms  []inputAtom
	planFbCursor int
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

// sessionByTicket returns the session with the given ticket id, or nil.
func (m *monitorModel) sessionByTicket(ticket string) *store.Session {
	for i := range m.flat {
		if m.flat[i].Ticket == ticket {
			return &m.flat[i]
		}
	}
	return nil
}

func (m monitorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// Re-wrap the open plan for the new width (glamour bakes the wrap in).
		if m.view == viewPlan {
			if s := m.sessionByTicket(m.planTicket); s != nil {
				if md, ok := planMarkdownDoc(*s); ok {
					m.planLines = renderMarkdownLines(md, m.paneWidth())
				}
			}
		}
	case tickMsg:
		m.reload()
		return m, tick()
	case answerDoneMsg:
		if msg.err != nil {
			m.notice = "answer failed: " + msg.err.Error()
		} else {
			m.notice = "answer sent to " + msg.ticket + " - resuming…"
		}
		return m, nil
	case cancelDoneMsg:
		m.notice = "stopped " + msg.ticket + " - process killed, worktree removed"
		m.reload()
		return m, nil
	case pauseDoneMsg:
		m.notice = "paused " + msg.ticket + " - agent stopped, worktree kept (NEEDS YOU)"
		m.reload()
		return m, nil
	case doctorDoneMsg:
		m.outputTitle = "doctor - connectivity check"
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
			m.notice = "telemetry enabled - thanks for helping!"
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
			m.notice = "opened Claude Code in a new window"
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
	case viewRunMethod:
		return m.updateRunMethod(msg)
	case viewRunInput:
		return m.updateRunInput(msg)
	case viewOutput:
		return m.updateOutput(msg)
	case viewPlan:
		return m.updatePlan(msg)
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
		// A ticket paused for plan review opens straight into the full-screen
		// plan viewer; everything else opens the actions menu.
		if s := m.selected(); s != nil && s.State == store.StatePlanReview {
			return m.doAction("view-plan")
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

// updateConfirming handles the palette-style stop-confirmation prompt.
// ↑/↓ moves the cursor; Enter selects; y/n are kept for backwards compat.
func (m monitorModel) updateConfirming(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	confirmYes := func() (tea.Model, tea.Cmd) {
		key := m.confirming
		m.confirming = ""
		m.confirmCursor = 0
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
	}
	switch msg.String() {
	case "up", "k":
		m.confirmCursor = 0
	case "down", "j":
		m.confirmCursor = 1
	case "enter":
		if m.confirmCursor == 0 {
			return confirmYes()
		}
		m.confirming = ""
		m.confirmCursor = 0
	case "y", "Y":
		return confirmYes()
	case "n", "N", "esc", "q":
		m.confirming = ""
		m.confirmCursor = 0
	}
	return m, nil
}

// renderConfirm renders the palette-style stop-confirmation view.
func (m monitorModel) renderConfirm(w int) string {
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(headerStyle.Render("  Stop "+m.confirming+"?") + "\n")
	b.WriteString(dimStyle.Render("  kills the process and removes the worktree") + "\n\n")
	items := []string{"Yes, stop and clean up", "No, keep running"}
	for i, item := range items {
		if i == m.confirmCursor {
			b.WriteString(selStyle.Render(" "+item) + "\n")
		} else {
			b.WriteString("  " + item + "\n")
		}
	}
	return b.String()
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

// pasteInlineMaxRunes is the cutoff below which a single-line paste is dropped
// in as literal typed text instead of a collapsed "[1 line added]" chip.
const pasteInlineMaxRunes = 100

// inputAtom is one unit of the answer input. A typed character carries a rune;
// a paste carries its whole body in blob (and r is zero). Each atom is a single
// cursor stop, so backspace over a paste removes the entire "[N lines added]"
// chunk at once - matching Claude Code's paste-as-one-token behaviour.
type inputAtom struct {
	r    rune   // typed character (when blob == "")
	blob string // pasted body (when non-empty)
}

// answerText reassembles the atoms into the string sent to the agent: typed
// runes verbatim, pastes expanded to their full body in place.
func answerText(atoms []inputAtom) string {
	var b strings.Builder
	for _, a := range atoms {
		if a.blob != "" {
			b.WriteString(a.blob)
		} else {
			b.WriteRune(a.r)
		}
	}
	return b.String()
}

// insertAtom inserts one atom at the cursor and returns the advanced cursor.
func insertAtom(atoms []inputAtom, cursor int, a inputAtom) ([]inputAtom, int) {
	out := make([]inputAtom, 0, len(atoms)+1)
	out = append(out, atoms[:cursor]...)
	out = append(out, a)
	out = append(out, atoms[cursor:]...)
	return out, cursor + 1
}

// updateAnswering handles keystrokes while the answer input box is open.
func (m monitorModel) updateAnswering(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Bracketed paste arrives as a KeyMsg with Paste=true regardless of Type.
	// Drop it in as a single atom at the cursor - it renders inline as
	// "[N lines added]" and stays one deletable unit.
	if msg.Paste {
		blob := normalizeNewlines(string(msg.Runes))
		if blob == "" {
			return m, nil
		}
		// A short single-line paste (a URL, an identifier, a phrase) reads better
		// dropped in as literal text than hidden behind an "[1 line added]" chip.
		// Anything multi-line or long stays a collapsed paste atom.
		if oneLine := strings.TrimRight(blob, "\n"); !strings.Contains(oneLine, "\n") && len([]rune(oneLine)) <= pasteInlineMaxRunes {
			for _, ch := range oneLine {
				m.answerAtoms, m.answerCursor = insertAtom(m.answerAtoms, m.answerCursor, inputAtom{r: ch})
			}
		} else {
			m.answerAtoms, m.answerCursor = insertAtom(m.answerAtoms, m.answerCursor, inputAtom{blob: blob})
		}
		return m, nil
	}
	switch msg.Type {
	case tea.KeyEnter:
		full := strings.TrimSpace(answerText(m.answerAtoms))
		if full == "" {
			m.answering = false
			return m, nil
		}
		key := m.answerKey
		m.answering = false
		m.answerAtoms = nil
		m.answerCursor = 0
		m.notice = "sending answer to " + key + "…"
		return m, m.submitAnswer(key, full)
	case tea.KeyEsc:
		m.answering = false
		m.answerAtoms = nil
		m.answerCursor = 0
	case tea.KeyLeft:
		if m.answerCursor > 0 {
			m.answerCursor--
		}
	case tea.KeyRight:
		if m.answerCursor < len(m.answerAtoms) {
			m.answerCursor++
		}
	case tea.KeyBackspace:
		if m.answerCursor > 0 {
			m.answerAtoms = append(m.answerAtoms[:m.answerCursor-1], m.answerAtoms[m.answerCursor:]...)
			m.answerCursor--
		}
	case tea.KeySpace:
		m.answerAtoms, m.answerCursor = insertAtom(m.answerAtoms, m.answerCursor, inputAtom{r: ' '})
	case tea.KeyRunes:
		for _, ch := range msg.Runes {
			m.answerAtoms, m.answerCursor = insertAtom(m.answerAtoms, m.answerCursor, inputAtom{r: ch})
		}
	default:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}
	return m, nil
}

// submitAnswer appends the answer to the .md source file and relaunches. This
// only works for local tickets (SourcePath set). For Jira tickets the action
// is not offered - use "Open in Claude Code" to answer in the session directly.
func (m monitorModel) submitAnswer(key, answer string) tea.Cmd {
	self, st := m.selfPath, m.store
	return func() tea.Msg {
		var sourcePath string
		reviewPlan := false
		if st != nil {
			if sess, err := st.Get(key); err == nil && sess != nil {
				sourcePath = sess.SourcePath
				reviewPlan = sess.ReviewPlan
			}
		}
		if sourcePath == "" {
			return answerDoneMsg{ticket: key, err: fmt.Errorf("answer via TUI only works for local .md tickets - use \"Open in Claude Code\" to answer in the session")}
		}
		raw, err := os.ReadFile(sourcePath)
		if err != nil {
			return answerDoneMsg{ticket: key, err: fmt.Errorf("read %s: %w", sourcePath, err)}
		}
		updated := strings.TrimSpace(string(raw)) + "\n\n---\nAnswers:\n" + answer
		if err := os.WriteFile(sourcePath, []byte(updated+"\n"), 0o644); err != nil {
			return answerDoneMsg{ticket: key, err: fmt.Errorf("write %s: %w", sourcePath, err)}
		}
		// Keep the plan-review gate across the re-run: a ticket that paused for the
		// plan should pause again after re-planning on the answered version.
		args := []string{"run", sourcePath}
		if reviewPlan {
			args = append(args, "--review-plan")
		}
		c := exec.Command(self, args...)
		c.Stdout, c.Stderr = nil, nil
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := c.Start(); err != nil {
			return answerDoneMsg{ticket: key, err: err}
		}
		return answerDoneMsg{ticket: key}
	}
}

// submitPlanFeedback appends the human's feedback to the .md source and re-runs
// with --review-plan so the agent re-plans and pauses again for another review.
// Same local-tickets-only limitation as submitAnswer (pasted tickets always have
// a SourcePath, so the primary flow is covered).
func (m monitorModel) submitPlanFeedback(key, feedback string) tea.Cmd {
	self, st := m.selfPath, m.store
	return func() tea.Msg {
		var sourcePath string
		if st != nil {
			if sess, err := st.Get(key); err == nil && sess != nil {
				sourcePath = sess.SourcePath
			}
		}
		if sourcePath == "" {
			return answerDoneMsg{ticket: key, err: fmt.Errorf("plan feedback via TUI only works for local .md tickets - use \"Open in Claude Code\" to give feedback in the session")}
		}
		raw, err := os.ReadFile(sourcePath)
		if err != nil {
			return answerDoneMsg{ticket: key, err: fmt.Errorf("read %s: %w", sourcePath, err)}
		}
		updated := strings.TrimSpace(string(raw)) + "\n\n---\nPlan feedback:\n" + feedback
		if err := os.WriteFile(sourcePath, []byte(updated+"\n"), 0o644); err != nil {
			return answerDoneMsg{ticket: key, err: fmt.Errorf("write %s: %w", sourcePath, err)}
		}
		c := exec.Command(self, "run", sourcePath, "--review-plan")
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
	b.WriteString("  " + ctaStyle.Render(" Y ") + "  yes - help make magneton better\n\n")
	b.WriteString("  " + dimStyle.Render("N") + "  no thanks\n")
	return b.String()
}

// ---- view ------------------------------------------------------------------

var (
	headerStyle = lipgloss.NewStyle().Bold(true)
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	selStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("231")).Background(lipgloss.Color("236"))
	// hintStyle accents the "↵ actions" affordance on the selected row. Same
	// background as selStyle so the row highlight stays continuous under it.
	hintStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Background(lipgloss.Color("236")).Bold(true)
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
	case viewRunMethod:
		body = m.renderRunMethod(w)
	case viewRunInput:
		body = m.renderRunInput(w)
	case viewOutput:
		body = m.renderOutput(w)
	case viewPlan:
		body = m.renderPlan(w)
	case viewForm:
		if m.form != nil {
			body = m.form.render(w)
		}
	default:
		body = m.renderDashboardBody(w)
	}

	notice := ""
	if m.notice != "" && m.confirming == "" {
		notice = whyStyle.Render(truncate("  "+m.notice, w))
	}
	// Footer hint. Modal views render their own hints in the body.
	footer := ""
	if m.view == viewDashboard && !m.answering && m.confirming == "" {
		footer = dimStyle.Render("  ↑↓ select · enter: choose · : commands · q: quit")
	} else if m.view == viewConsent {
		footer = dimStyle.Render("  y: share · n: skip")
	} else if m.confirming != "" {
		footer = dimStyle.Render("  ↑↓ move · enter select · esc cancel")
	} else if m.view == viewPlan && m.planFeedback {
		footer = dimStyle.Render("  type your correction · enter: send · esc: cancel · ↑↓: scroll plan")
	} else if m.view == viewPlan {
		footer = dimStyle.Render("  ←→ select · enter: choose · ↑↓/PgUp/PgDn: scroll · esc: back")
	} else if m.answering {
		footer = dimStyle.Render("  enter: send & resume · esc: cancel")
	}
	// The confirming state replaces the body with a mini palette.
	if m.confirming != "" {
		body = m.renderConfirm(w)
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
		b.WriteString("  " + dimStyle.Render("no agents running yet - select the row above and press enter to start one"))
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
			b.WriteString(m.renderRow(s, w, idx == m.cursor) + "\n")
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
		if sel.BaseBranch != "" {
			b.WriteString(dimStyle.Render("  stacked on: "+sel.BaseBranch) + "\n")
		}
		for _, ln := range whyLines(*sel) {
			for _, seg := range wrapLine(ln, w) {
				b.WriteString(whyStyle.Render(seg) + "\n")
			}
		}
		if m.answering {
			b.WriteString("\n  " + headerStyle.Render("answer "+m.answerKey) + "\n")
			// Walk the atoms into a single display line: typed runes verbatim,
			// each paste shown inline as "[N lines added]", and "▌" where the
			// cursor sits. Then word-wrap so long answers stay visible.
			var line strings.Builder
			for i, a := range m.answerAtoms {
				if i == m.answerCursor {
					line.WriteString("▌")
				}
				if a.blob != "" {
					n := lineCount(a.blob)
					line.WriteString(fmt.Sprintf("[%d line%s added]", n, plural(n)))
				} else {
					line.WriteRune(a.r)
				}
			}
			if m.answerCursor >= len(m.answerAtoms) {
				line.WriteString("▌")
			}
			const prefix = "  › "
			const cont = "    "
			segs := wrapLine(line.String(), w-len([]rune(prefix)))
			if len(segs) == 0 {
				b.WriteString(prefix + "\n")
			} else {
				for i, seg := range segs {
					if i == 0 {
						b.WriteString(prefix + seg + "\n")
					} else {
						b.WriteString(cont + seg + "\n")
					}
				}
			}
		} else {
			whyRowCount := 0
			for _, ln := range whyLines(*sel) {
				whyRowCount += len(wrapLine(ln, w))
			}
			detailH := m.height - listLines - 10 - whyRowCount
			if detailH < 3 {
				detailH = 3
			}
			// Fetch more raw lines than detailH: each raw line may wrap into
			// several rows, so we need a larger pool to fill the box. After
			// wrapping, trim to exactly detailH rendered rows so the log section
			// never overflows and pushes the ticket list off screen.
			rawLines := tailLines(paths.LogFor(sel.Ticket), detailH*6)
			var segs []string
			for _, ln := range rawLines {
				segs = append(segs, wrapLine(stripPrefix(ln, sel.Ticket), w)...)
			}
			if len(segs) > detailH {
				segs = segs[len(segs)-detailH:]
			}
			if len(segs) == 0 {
				b.WriteString("  " + dimStyle.Render("(no log output yet)"))
			}
			for _, seg := range segs {
				b.WriteString(seg + "\n")
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

// planMarkdownDoc builds a markdown document from a session's plan.json for the
// full-screen viewer. Returns ok=false when there's no readable plan yet.
func planMarkdownDoc(s store.Session) (string, bool) {
	plan, err := agent.ReadPlan(paths.WorktreeFor(s.Repo, s.Ticket))
	if err != nil || plan == nil {
		return "", false
	}
	var b strings.Builder
	title := s.Ticket
	if s.Summary != "" {
		title += " · " + s.Summary
	}
	fmt.Fprintf(&b, "# %s\n\n", title)
	if plan.Confidence != "" || plan.Type != "" {
		fmt.Fprintf(&b, "**Confidence:** %s  ·  **Type:** %s\n\n", plan.Confidence, plan.Type)
	}
	if strings.TrimSpace(plan.Plan) != "" {
		fmt.Fprintf(&b, "## Approach\n\n%s\n\n", plan.Plan)
	}
	if len(plan.Steps) > 0 {
		b.WriteString("## Steps\n\n")
		for i, step := range plan.Steps {
			fmt.Fprintf(&b, "%d. %s\n", i+1, step)
		}
		b.WriteString("\n")
	}
	if len(plan.Questions) > 0 {
		b.WriteString("## Open questions\n\n")
		for _, q := range plan.Questions {
			fmt.Fprintf(&b, "- %s\n", q)
		}
		b.WriteString("\n")
	}
	return b.String(), true
}

// renderMarkdownLines renders markdown to styled terminal rows via glamour,
// wrapped to width w. A fixed "dark" style avoids glamour querying the terminal
// for its background (which would clash with bubbletea's raw-mode screen). On any
// error it falls back to the raw markdown so the viewer is never blank.
func renderMarkdownLines(md string, w int) []string {
	if w < 20 {
		w = 20
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithWordWrap(w),
	)
	if err != nil {
		return strings.Split(md, "\n")
	}
	out, err := r.Render(md)
	if err != nil {
		return strings.Split(md, "\n")
	}
	return strings.Split(strings.TrimRight(out, "\n"), "\n")
}

// planChrome is the number of non-plan rows the viewer draws around the plan
// window: app header (1), title (1), menu block (2), and the frame footer (2).
// When the feedback input is open it adds its block (separator + up to 2 rows).
func (m monitorModel) planChrome() int {
	c := 6
	if m.planFeedback {
		c += 3
	}
	return c
}

// planViewportHeight is how many rows of the plan are visible.
func (m monitorModel) planViewportHeight() int {
	h := m.height - m.planChrome()
	if h < 3 {
		h = 3
	}
	return h
}

// openPlanView renders the session's plan to markdown and switches to the
// full-screen viewer. No-op (with a notice) when there's no plan to show.
func (m monitorModel) openPlanView(s store.Session) (tea.Model, tea.Cmd) {
	md, ok := planMarkdownDoc(s)
	if !ok {
		m.notice = "no plan to show yet for " + s.Ticket
		return m, nil
	}
	m.planTicket = s.Ticket
	m.planScroll = 0
	m.planMenu = 0
	m.planFeedback = false
	m.planFbAtoms = nil
	m.planFbCursor = 0
	m.planLines = renderMarkdownLines(md, m.paneWidth())
	m.view = viewPlan
	m.notice = ""
	return m, nil
}

// openPlanFeedback opens the viewer straight into the feedback input.
func (m monitorModel) openPlanFeedback(s store.Session) (tea.Model, tea.Cmd) {
	mm, cmd := m.openPlanView(s)
	hub := mm.(monitorModel)
	if hub.view == viewPlan { // only if a plan was actually found
		hub.planFeedback = true
	}
	return hub, cmd
}

// paneWidth is the wrap width for the plan viewer (leave a small right margin).
func (m monitorModel) paneWidth() int {
	w := m.width
	if w == 0 {
		w = 80
	}
	return w - 2
}

// clampPlanScroll keeps the scroll offset within [0, max].
func (m *monitorModel) clampPlanScroll() {
	max := len(m.planLines) - m.planViewportHeight()
	if max < 0 {
		max = 0
	}
	if m.planScroll > max {
		m.planScroll = max
	}
	if m.planScroll < 0 {
		m.planScroll = 0
	}
}

// updatePlan handles keys in the full-screen plan viewer. Two modes: the top
// menu (←→ select · enter choose) and the inline feedback input (which keeps the
// plan visible above so the user can scroll it while writing corrections).
func (m monitorModel) updatePlan(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.planFeedback {
		return m.updatePlanFeedback(msg)
	}
	viewH := m.planViewportHeight()
	switch msg.String() {
	case "esc", "q":
		m.view = viewDashboard
	case "ctrl+c":
		return m, tea.Quit
	case "left", "h", "right", "l", "tab":
		m.planMenu = 1 - m.planMenu // toggle between the two menu items
	case "up", "k":
		m.planScroll--
		m.clampPlanScroll()
	case "down", "j":
		m.planScroll++
		m.clampPlanScroll()
	case "pgup", "b":
		m.planScroll -= viewH
		m.clampPlanScroll()
	case "pgdown", " ":
		m.planScroll += viewH
		m.clampPlanScroll()
	case "home", "g":
		m.planScroll = 0
	case "end", "G":
		m.planScroll = len(m.planLines)
		m.clampPlanScroll()
	case "enter":
		if m.planMenu == 1 { // Approve
			m.syncCursorTo(m.planTicket) // a background reload may have moved the row
			return m.doAction("approve-plan")
		}
		m.planFeedback = true // Give feedback: open the inline input
		m.planFbAtoms = nil
		m.planFbCursor = 0
		m.clampPlanScroll()
	}
	return m, nil
}

// updatePlanFeedback edits the inline feedback text. Enter submits (re-plan with
// the correction), esc cancels back to the menu; up/down/PgUp/PgDn still scroll
// the plan so it stays reviewable while typing.
func (m monitorModel) updatePlanFeedback(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Paste {
		blob := normalizeNewlines(string(msg.Runes))
		if blob == "" {
			return m, nil
		}
		if oneLine := strings.TrimRight(blob, "\n"); !strings.Contains(oneLine, "\n") && len([]rune(oneLine)) <= pasteInlineMaxRunes {
			for _, ch := range oneLine {
				m.planFbAtoms, m.planFbCursor = insertAtom(m.planFbAtoms, m.planFbCursor, inputAtom{r: ch})
			}
		} else {
			m.planFbAtoms, m.planFbCursor = insertAtom(m.planFbAtoms, m.planFbCursor, inputAtom{blob: blob})
		}
		return m, nil
	}
	viewH := m.planViewportHeight()
	switch msg.Type {
	case tea.KeyEnter:
		full := strings.TrimSpace(answerText(m.planFbAtoms))
		if full == "" { // empty submit cancels back to the menu
			m.planFeedback = false
			return m, nil
		}
		key := m.planTicket
		m.planFeedback = false
		m.planFbAtoms = nil
		m.planFbCursor = 0
		m.view = viewDashboard
		m.notice = "sending plan feedback to " + key + "…"
		m.syncCursorTo(key)
		return m, m.submitPlanFeedback(key, full)
	case tea.KeyEsc:
		m.planFeedback = false // back to the menu, plan stays open
		m.planFbAtoms = nil
		m.planFbCursor = 0
	case tea.KeyUp:
		m.planScroll--
		m.clampPlanScroll()
	case tea.KeyDown:
		m.planScroll++
		m.clampPlanScroll()
	case tea.KeyPgUp:
		m.planScroll -= viewH
		m.clampPlanScroll()
	case tea.KeyPgDown:
		m.planScroll += viewH
		m.clampPlanScroll()
	case tea.KeyLeft:
		if m.planFbCursor > 0 {
			m.planFbCursor--
		}
	case tea.KeyRight:
		if m.planFbCursor < len(m.planFbAtoms) {
			m.planFbCursor++
		}
	case tea.KeyBackspace:
		if m.planFbCursor > 0 {
			m.planFbAtoms = append(m.planFbAtoms[:m.planFbCursor-1], m.planFbAtoms[m.planFbCursor:]...)
			m.planFbCursor--
		}
	case tea.KeySpace:
		m.planFbAtoms, m.planFbCursor = insertAtom(m.planFbAtoms, m.planFbCursor, inputAtom{r: ' '})
	case tea.KeyRunes:
		for _, ch := range msg.Runes {
			m.planFbAtoms, m.planFbCursor = insertAtom(m.planFbAtoms, m.planFbCursor, inputAtom{r: ch})
		}
	default:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}
	return m, nil
}

// syncCursorTo points the selection at a ticket by id, so cursor-based actions
// act on it even if a background reload reordered the list.
func (m *monitorModel) syncCursorTo(ticket string) {
	for i := range m.flat {
		if m.flat[i].Ticket == ticket {
			m.cursor = i + 2 // 0=Start-new 1=Edit-config agents=2..
			return
		}
	}
}

// renderPlan is the full-screen plan viewer: a top action menu, a scrollable
// window over the glamour-rendered plan, and (when active) an inline feedback
// input below the plan.
func (m monitorModel) renderPlan(w int) string {
	var b strings.Builder

	// Title + scroll position.
	viewH := m.planViewportHeight()
	pct := ""
	if len(m.planLines) > viewH {
		bottom := m.planScroll + viewH
		if bottom > len(m.planLines) {
			bottom = len(m.planLines)
		}
		pct = fmt.Sprintf("  %d–%d of %d", m.planScroll+1, bottom, len(m.planLines))
	}
	b.WriteString(headerStyle.Render(truncate("  Plan · "+m.planTicket, w)) + dimStyle.Render(pct) + "\n")

	// Action menu: two items, the selected one highlighted (dimmed while typing
	// feedback, since the input has focus then).
	items := []string{"Give feedback", "Approve"}
	var menu strings.Builder
	menu.WriteString("  ")
	for i, it := range items {
		if !m.planFeedback && i == m.planMenu {
			menu.WriteString(selStyle.Render(" "+it+" ") + "   ")
		} else {
			menu.WriteString(dimStyle.Render(" "+it+" ") + "   ")
		}
	}
	b.WriteString(menu.String() + "\n\n")

	// Plan window.
	start := m.planScroll
	if start > len(m.planLines)-viewH {
		start = len(m.planLines) - viewH
	}
	if start < 0 {
		start = 0
	}
	end := start + viewH
	if end > len(m.planLines) {
		end = len(m.planLines)
	}
	for _, ln := range m.planLines[start:end] {
		b.WriteString(ln + "\n")
	}

	// Inline feedback input, below the plan.
	if m.planFeedback {
		b.WriteString(sepStyle.Render(strings.Repeat("─", w)) + "\n")
		var line strings.Builder
		for i, a := range m.planFbAtoms {
			if i == m.planFbCursor {
				line.WriteString("▌")
			}
			if a.blob != "" {
				n := lineCount(a.blob)
				line.WriteString(fmt.Sprintf("[%d line%s added]", n, plural(n)))
			} else {
				line.WriteRune(a.r)
			}
		}
		if m.planFbCursor >= len(m.planFbAtoms) {
			line.WriteString("▌")
		}
		const prefix = "  feedback › "
		segs := wrapLine(line.String(), w-len([]rune(prefix)))
		if len(segs) == 0 {
			b.WriteString(prefix + "\n")
		} else {
			for i, seg := range segs {
				if i == 0 {
					b.WriteString(prefix + seg + "\n")
				} else {
					b.WriteString("    " + seg + "\n")
				}
			}
		}
	}
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
			fmt.Sprintf("■ Stopped - %s. Re-run the ticket to resume.", reason),
			"  Press o to inspect the worktree.",
		}
	}
	switch s.State {
	case "awaiting-answer":
		if plan, err := agent.ReadPlan(paths.WorktreeFor(s.Repo, s.Ticket)); err == nil && len(plan.Questions) > 0 {
			out := []string{fmt.Sprintf("▮ Needs you - press ↵ enter to answer %d question(s):", len(plan.Questions))}
			for i, q := range plan.Questions {
				out = append(out, fmt.Sprintf("  Q%d %s", i+1, q))
			}
			return out
		}
		return []string{"▮ Needs you - press ↵ enter to respond (see log below)."}
	case store.StatePlanReview:
		// The full plan lives in the dedicated full-screen viewer (press enter);
		// keep the detail pane to a short teaser so it never overflows the log.
		out := []string{"▮ Plan ready - press ↵ enter to read the full plan, then approve or give feedback"}
		if plan, err := agent.ReadPlan(paths.WorktreeFor(s.Repo, s.Ticket)); err == nil && plan != nil {
			if plan.Plan != "" {
				out = append(out, "  "+plan.Plan)
			}
			out = append(out, fmt.Sprintf("  %d step(s) · Confidence: %s · Type: %s", len(plan.Steps), plan.Confidence, plan.Type))
		}
		return out
	case "failed":
		return []string{
			"✗ Failed - " + failReason(s.Ticket),
			"  Open Android Studio (o) to fix, then press R to gate & open the PR.",
		}
	case "needs-you":
		return []string{
			"⚑ Needs you - the agent got stuck (see log below).",
			"  Open Android Studio (o) to fix, then press R to gate & open the PR.",
		}
	case store.StateReview:
		line := "◎ Under review - PR is open and waiting for your approval."
		if s.PRURL != "" {
			line = "◎ Under review - " + s.PRURL
		}
		actions := "  Open Android Studio (o) · open in Claude Code (c)"
		if !worktreeExists(s.Repo, s.Ticket) {
			actions = "  Worktree removed - press enter to run again fresh."
		}
		return []string{line, actions}
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

func (m monitorModel) renderRow(s store.Session, w int, selected bool) string {
	// ShortDesc (LLM-generated <10-word gist) → Summary → latest log line.
	desc := s.ShortDesc
	if desc == "" {
		desc = s.Summary
	}
	if desc == "" {
		desc = cleanActivity(tailLines(paths.LogFor(s.Ticket), 1), s.Ticket)
	}
	retries := ""
	if s.Retries > 0 {
		retries = fmt.Sprintf(" ×%d", s.Retries)
	}
	left := fmt.Sprintf("  %s %-9s %-11s", glyphFor(s), s.Ticket, stateLabel(s)+retries)
	right := fmt.Sprintf(" %4s", age(s.UpdatedAt))

	// The selected row carries an inline "↵ actions" affordance so it's obvious
	// pressing enter opens the actions menu.
	hint := ""
	if selected {
		hint = "  ↵ actions "
	}
	flex := w - lipgloss.Width(left) - lipgloss.Width(right) - lipgloss.Width(hint) - 1
	if flex < 6 {
		flex = 6
	}
	mid := fmt.Sprintf(" %-*s", flex, truncate(desc, flex))
	if !selected {
		return left + mid + right
	}
	// Compose the highlight across the whole line, accenting only the hint. All
	// three segments share selStyle's background so the highlight stays solid.
	return selStyle.Render(left+mid) + hintStyle.Render(hint) + selStyle.Render(right)
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

// wrapLine breaks s into visual segments each at most w columns wide. Used in
// log views so long lines wrap instead of being cut off with "…".
func wrapLine(s string, w int) []string {
	if w <= 0 {
		return nil
	}
	if lipgloss.Width(s) <= w {
		return []string{s}
	}
	var out []string
	r := []rune(s)
	for len(r) > 0 {
		take := w
		if take > len(r) {
			take = len(r)
		}
		for take > 0 && lipgloss.Width(string(r[:take])) > w {
			take--
		}
		if take == 0 {
			take = 1
		}
		out = append(out, string(r[:take]))
		r = r[take:]
	}
	return out
}
