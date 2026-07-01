package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/andresuarezz26/magneton/internal/agent"
	"github.com/andresuarezz26/magneton/internal/config"
	"github.com/andresuarezz26/magneton/internal/paths"
	"github.com/andresuarezz26/magneton/internal/secrets"
)

// hubView is the top-level view of the hub. The zero value is the dashboard.
type hubView int

const (
	viewDashboard hubView = iota
	viewPalette
	viewRunInput
	viewOutput
	viewForm
	viewConsent
)

// ---- messages --------------------------------------------------------------

type doctorDoneMsg struct{ out string }
type formDoneMsg struct {
	notice string
	err    error
}
type runLaunchedMsg struct {
	text string
	err  error
}

// pendingTicket is one queued ticket in the "Start new ticket(s)" input, shown
// as a chip. kind is content|jira|file.
type pendingTicket struct {
	id        string // resolved ticket id; also the typed buffer while prompting
	title     string // parsed title (or the key itself, for a jira chip)
	lines     int
	kind      string // "content" | "jira" | "file"
	body      string // raw pasted content (content kind)
	path      string // on-disk path (file kind)
	resolving bool   // content: async id lookup still in flight
}

// ticketIDResolvedMsg carries the result of an async paste-time id lookup for
// the chip at index idx (regex fast-path, then AI extraction).
type ticketIDResolvedMsg struct {
	idx   int
	id    string
	found bool
}
type daemonMsg struct {
	action string // "start" | "stop"
	err    error
}

// ---- command palette -------------------------------------------------------

type paletteItem struct {
	key   string
	label string
	desc  string
}

// paletteItems is the Enter menu: the selected agent's actions first, then the
// global commands. Rebuilt each open so it reflects the current selection/daemon.
func (m monitorModel) paletteItems() []paletteItem {
	var items []paletteItem
	if s := m.selected(); s != nil {
		items = append(items, agentActions(*s)...)
	}
	items = append(items,
		paletteItem{"run", "Start new ticket(s)", "launch a ticket key or .md file"},
		paletteItem{"doctor", "Doctor", "connectivity + setup health check"},
		paletteItem{"config", "Edit config", "edit ~/.agent/config.toml fields"},
		paletteItem{"setup", "Setup wizard", "configure Jira, repo, and tokens"},
	)
	if _, ok := daemonAlive(); ok {
		items = append(items, paletteItem{"daemon-stop", "Stop daemon", "stop the background poller"})
	} else {
		items = append(items, paletteItem{"daemon-start", "Start daemon", "poll Jira and run the fleet in the background"})
	}
	return append(items, paletteItem{"quit", "Quit", "exit the hub"})
}

func (m monitorModel) updatePalette(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	items := m.paletteItems()
	switch msg.String() {
	case "esc", "q":
		m.view = viewDashboard
	case "up", "k":
		if m.paletteCursor > 0 {
			m.paletteCursor--
		}
	case "down", "j":
		if m.paletteCursor < len(items)-1 {
			m.paletteCursor++
		}
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		if m.paletteCursor < len(items) {
			id := items[m.paletteCursor].key
			m.view = viewDashboard // close the menu; doAction may reopen another view
			return m.doAction(id)
		}
	}
	return m, nil
}

func (m monitorModel) renderPalette(w int) string {
	var b strings.Builder
	title := "Actions"
	if s := m.selected(); s != nil {
		title = "Actions — " + s.Ticket
	}
	b.WriteString(headerStyle.Render("  "+title) + "\n\n")
	for i, it := range m.paletteItems() {
		marker := "   "
		label := it.label
		if i == m.paletteCursor {
			marker = " ▸ "
			label = selStyle.Render(" " + it.label + " ")
		}
		b.WriteString(truncate(marker+label+"  "+dimStyle.Render(it.desc), w) + "\n")
	}
	b.WriteString("\n  " + dimStyle.Render("↑↓ select · enter: choose · esc: close"))
	return b.String()
}

// ---- run-new input ---------------------------------------------------------

func (m monitorModel) updateRunInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Sub-mode: typing an id for a chip whose id couldn't be detected.
	if m.runIDPrompt >= 0 {
		return m.updateRunIDPrompt(msg)
	}

	// Bracketed paste arrives as one KeyMsg with the whole blob (newlines
	// included), so it never trips Enter=launch. Each paste becomes one chip.
	if msg.Paste {
		return m.addPastedTicket(string(msg.Runes))
	}

	switch msg.Type {
	case tea.KeyEnter:
		m = m.commitBuffer() // fold any typed key/path into a chip first
		if len(m.runTickets) == 0 {
			m.view = viewDashboard
			return m, nil
		}
		if m.anyResolving() {
			m.notice = "still detecting a ticket id…"
			return m, nil
		}
		tickets := m.runTickets
		m.view = viewDashboard
		m.runTickets = nil
		return m, m.launchRun(tickets)
	case tea.KeyEsc:
		m.view = viewDashboard
		m.runText = ""
		m.runTickets = nil
		m.runIDPrompt = -1
	case tea.KeyBackspace:
		if r := []rune(m.runText); len(r) > 0 {
			m.runText = string(r[:len(r)-1])
		} else if n := len(m.runTickets); n > 0 {
			m.runTickets = m.runTickets[:n-1] // pop the last chip
		}
	case tea.KeySpace:
		if strings.TrimSpace(m.runText) != "" {
			m = m.commitBuffer() // space commits a typed key/path as a chip
		}
	case tea.KeyRunes:
		m.runText += string(msg.Runes)
	default:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}
	return m, nil
}

// addPastedTicket classifies one pasted blob and appends it as a chip. A bare
// key or a single-line file path resolve immediately; anything else is treated
// as ticket content and its id is resolved asynchronously.
func (m monitorModel) addPastedTicket(blob string) (tea.Model, tea.Cmd) {
	blob = normalizeNewlines(blob) // terminals paste newlines as \r
	trimmed := strings.TrimSpace(blob)
	if trimmed == "" {
		return m, nil
	}
	if isTicketKey(trimmed) {
		m.runTickets = append(m.runTickets, pendingTicket{
			id: normalizeTicket(trimmed), title: trimmed, kind: "jira",
		})
		return m, nil
	}
	if !strings.ContainsRune(trimmed, '\n') {
		if fi, err := os.Stat(trimmed); err == nil && !fi.IsDir() {
			m.runTickets = append(m.runTickets, newFileTicket(trimmed))
			return m, nil
		}
	}
	title, _, err := parseTicketContent(blob)
	if err != nil || title == "" {
		title = "(untitled)"
	}
	idx := len(m.runTickets)
	m.runTickets = append(m.runTickets, pendingTicket{
		title: title, lines: lineCount(blob), kind: "content", body: blob, resolving: true,
	})
	return m, m.resolveTicketID(idx, blob)
}

// commitBuffer folds a typed token (a Jira key or a .md path) into a chip.
func (m monitorModel) commitBuffer() monitorModel {
	tok := strings.TrimSpace(m.runText)
	m.runText = ""
	if tok == "" {
		return m
	}
	if fi, err := os.Stat(tok); err == nil && !fi.IsDir() {
		m.runTickets = append(m.runTickets, newFileTicket(tok))
		return m
	}
	m.runTickets = append(m.runTickets, pendingTicket{
		id: normalizeTicket(tok), title: tok, kind: "jira",
	})
	return m
}

// newFileTicket builds a chip for a .md/text ticket file on disk.
func newFileTicket(path string) pendingTicket {
	t := pendingTicket{kind: "file", path: path, title: filepath.Base(path)}
	if sp, err := loadLocalTicket(path); err == nil {
		t.id, t.title = sp.ticket, sp.summary
	}
	if raw, err := os.ReadFile(path); err == nil {
		t.lines = lineCount(string(raw))
	}
	return t
}

// resolveTicketID looks up a pasted ticket's id: the regex fast-path first, then
// a Claude extraction pass. The result routes back through Update as a
// ticketIDResolvedMsg (which either fills the chip or opens the id prompt).
func (m monitorModel) resolveTicketID(idx int, content string) tea.Cmd {
	model := m.implModel
	return func() tea.Msg {
		if id, ok := detectTicketID(content); ok {
			return ticketIDResolvedMsg{idx: idx, id: id, found: true}
		}
		if id, err := agent.ExtractTicketID(content, model, secrets.Get(secrets.Anthropic)); err == nil {
			if id != "" && id != "NONE" {
				return ticketIDResolvedMsg{idx: idx, id: id, found: true}
			}
		}
		return ticketIDResolvedMsg{idx: idx, found: false}
	}
}

// updateRunIDPrompt handles typing the id for a pasted ticket whose id could not
// be detected. The chip's id field doubles as the edit buffer here.
func (m monitorModel) updateRunIDPrompt(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	i := m.runIDPrompt
	if i < 0 || i >= len(m.runTickets) {
		m.runIDPrompt = -1
		return m, nil
	}
	switch msg.Type {
	case tea.KeyEnter:
		if id := normalizeTicket(m.runTickets[i].id); id != "" {
			m.runTickets[i].id = id
			m.runIDPrompt = -1
		}
	case tea.KeyEsc:
		m.runTickets = append(m.runTickets[:i], m.runTickets[i+1:]...) // drop this chip
		m.runIDPrompt = -1
	case tea.KeyBackspace:
		if r := []rune(m.runTickets[i].id); len(r) > 0 {
			m.runTickets[i].id = string(r[:len(r)-1])
		}
	case tea.KeyRunes:
		m.runTickets[i].id += string(msg.Runes)
	default:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m monitorModel) anyResolving() bool {
	for _, t := range m.runTickets {
		if t.resolving {
			return true
		}
	}
	return false
}

func (m monitorModel) renderRunInput(w int) string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("  Start new ticket(s)") + "\n")
	b.WriteString(dimStyle.Render("  paste a ticket (content, or a key/path); paste again to add more") + "\n\n")

	for i, t := range m.runTickets {
		var label string
		if t.kind == "jira" {
			label = fmt.Sprintf("[%s]", t.id)
		} else {
			id := t.id
			if t.resolving {
				id = "⋯"
			} else if id == "" {
				id = "?"
			}
			label = fmt.Sprintf("[%s · %s · %d line%s]", id, truncate(stripIDPrefix(t.title, t.id), 40), t.lines, plural(t.lines))
		}
		if i == m.runIDPrompt {
			label = selStyle.Render(" " + label + " ")
		}
		b.WriteString("  " + label + "\n")
	}
	if len(m.runTickets) > 0 {
		b.WriteString("\n")
	}

	if m.runIDPrompt >= 0 && m.runIDPrompt < len(m.runTickets) {
		b.WriteString(dimStyle.Render("  no ticket id found — type one for this ticket:") + "\n")
		b.WriteString("  › " + m.runTickets[m.runIDPrompt].id + "▌\n")
		b.WriteString("\n  " + dimStyle.Render("enter accept · esc drop this ticket"))
		return b.String()
	}

	b.WriteString("  › " + m.runText + "▌\n")
	b.WriteString("\n  " + dimStyle.Render("paste/type · space add · enter launch · esc cancel"))
	return b.String()
}

// launchRun spawns `agent run <args…>` detached; the new session appears in the
// dashboard and streams to its log (which the detail pane tails). Pasted content
// is written to a temp .md named by its id so it flows through the existing local
// pipeline; jira keys and file paths pass through as-is.
func (m monitorModel) launchRun(tickets []pendingTicket) tea.Cmd {
	self := m.selfPath
	return func() tea.Msg {
		args := []string{"run"}
		for _, t := range tickets {
			switch t.kind {
			case "content":
				path, err := writePastedTicket(t.id, t.body)
				if err != nil {
					return runLaunchedMsg{err: err}
				}
				args = append(args, path)
			case "file":
				args = append(args, t.path)
			default: // jira
				args = append(args, t.id)
			}
		}
		if len(args) == 1 {
			return runLaunchedMsg{err: fmt.Errorf("no tickets to launch")}
		}
		c := exec.Command(self, args...)
		c.Stdout, c.Stderr = nil, nil
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		err := c.Start()
		return runLaunchedMsg{text: fmt.Sprintf("%d ticket(s)", len(tickets)), err: err}
	}
}

// spawnRun launches `agent run <args…>` detached with pre-split args (no
// whitespace splitting, so paths with spaces survive). Used for resume/ship/rerun
// of an existing ticket, where args are a ticket key/path plus optional flags.
func (m monitorModel) spawnRun(args ...string) tea.Cmd {
	self := m.selfPath
	full := append([]string{"run"}, args...)
	return func() tea.Msg {
		c := exec.Command(self, full...)
		c.Stdout, c.Stderr = nil, nil
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		err := c.Start()
		return runLaunchedMsg{text: strings.Join(args, " "), err: err}
	}
}

// writePastedTicket saves a pasted ticket blob to ~/.agent/pasted/<id>.md so the
// run subprocess can read it as a local ticket (ticketIDFromPath recovers <id>).
func writePastedTicket(id, body string) (string, error) {
	if err := paths.EnsureDirs(); err != nil {
		return "", err
	}
	if id == "" {
		id = "PASTED"
	}
	p := filepath.Join(paths.Pasted(), id+".md")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		return "", err
	}
	return p, nil
}

// lineCount returns the number of lines in s, ignoring a single trailing newline.
func lineCount(s string) int {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// ---- doctor output ---------------------------------------------------------

func (m monitorModel) runDoctor() tea.Cmd {
	self := m.selfPath
	return func() tea.Msg {
		out, _ := exec.Command(self, "doctor").CombinedOutput()
		return doctorDoneMsg{out: string(out)}
	}
}

func (m monitorModel) updateOutput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "enter":
		m.view = viewDashboard
	case "ctrl+c":
		return m, tea.Quit
	}
	return m, nil
}

func (m monitorModel) renderOutput(w int) string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("  "+m.outputTitle) + "\n\n")
	for _, ln := range strings.Split(strings.TrimRight(m.outputText, "\n"), "\n") {
		b.WriteString(truncate(ln, w) + "\n")
	}
	b.WriteString("\n  " + dimStyle.Render("esc/enter close"))
	return b.String()
}

// ---- daemon control --------------------------------------------------------

// daemonAlive reports the daemon pid and whether it's running (reuses start.go).
func daemonAlive() (int, bool) {
	if pid, ok := readPid(); ok && processAlive(pid) {
		return pid, true
	}
	return 0, false
}

func (m monitorModel) startDaemon() tea.Cmd {
	self := m.selfPath
	return func() tea.Msg {
		c := exec.Command(self, "start")
		c.Stdout, c.Stderr = nil, nil
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		return daemonMsg{action: "start", err: c.Start()}
	}
}

func (m monitorModel) stopDaemon() tea.Cmd {
	return func() tea.Msg {
		pid, ok := daemonAlive()
		if !ok {
			return daemonMsg{action: "stop", err: fmt.Errorf("daemon not running")}
		}
		return daemonMsg{action: "stop", err: syscall.Kill(pid, syscall.SIGTERM)}
	}
}

// ---- config / setup forms --------------------------------------------------

func (m monitorModel) updateForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.form == nil {
		m.view = viewDashboard
		return m, nil
	}
	switch msg.Type {
	case tea.KeyEsc:
		m.view = viewDashboard
		m.form = nil
	case tea.KeyEnter:
		submit, fields := m.form.submit, m.form.fields
		m.view = viewDashboard
		m.form = nil
		if submit != nil {
			return m, func() tea.Msg { return submit(fields) }
		}
	case tea.KeyTab, tea.KeyDown:
		m.form.next()
	case tea.KeyShiftTab, tea.KeyUp:
		m.form.prev()
	case tea.KeyBackspace:
		m.form.backspace()
	case tea.KeySpace:
		m.form.typeRunes(" ")
	case tea.KeyRunes:
		m.form.typeRunes(string(msg.Runes))
	default:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}
	return m, nil
}

// configFields builds the editable (non-secret) field set from a config.
//
// Models are free-text on purpose: the set of usable model identifiers depends
// on the account (personal vs enterprise) and the org's policy, and Claude Code
// fetches that list at runtime — there's no offline list magneton could show
// without it going stale or offering models a company forbids. Leave a model
// blank to inherit Claude Code's configured default; or type the exact id your
// account allows (e.g. "sonnet", "claude-opus-4-8", or a Bedrock/Vertex id).
func configFields(cfg *config.Config) []formField {
	repo := config.Repo{}
	if len(cfg.Repos) > 0 {
		repo = cfg.Repos[0]
	}
	return []formField{
		{label: "Jira base URL", value: cfg.JiraBaseURL},
		{label: "Jira email", value: cfg.JiraEmail},
		{label: "Repo path", value: repo.Path},
		{label: "Branch", value: repo.Branch},
		{label: "Model · plan (blank = default)", value: cfg.ModelPlan},
		{label: "Model · implement (blank = default)", value: cfg.ModelImpl},
		{label: "Model · review (blank = default)", value: cfg.ModelReview},
	}
}

// applyConfigFields writes the (non-secret) form values back onto a config.
func applyConfigFields(cfg *config.Config, f []formField) {
	repo := config.Repo{}
	if len(cfg.Repos) > 0 {
		repo = cfg.Repos[0]
	}
	cfg.JiraBaseURL = f[0].value
	cfg.JiraEmail = f[1].value
	repo.Path = f[2].value
	repo.Branch = f[3].value
	cfg.ModelPlan = f[4].value
	cfg.ModelImpl = f[5].value
	cfg.ModelReview = f[6].value
	cfg.Repos = []config.Repo{repo}
}

func (m *monitorModel) openConfigForm() {
	cfg, err := config.Load()
	if err != nil {
		// No config yet → fall back to the setup wizard.
		m.openSetupForm()
		return
	}
	m.form = &formModel{
		title:  "Edit config",
		note:   "~/.agent/config.toml · first repo",
		fields: configFields(cfg),
		submit: func(f []formField) tea.Msg {
			cfg, err := config.Load()
			if err != nil {
				cfg = &config.Config{}
			}
			applyConfigFields(cfg, f)
			if err := config.Save(cfg); err != nil {
				return formDoneMsg{err: err}
			}
			return formDoneMsg{notice: "config saved"}
		},
	}
	m.view = viewForm
}

func (m *monitorModel) openSetupForm() {
	cfg, err := config.Load()
	if err != nil {
		cfg = &config.Config{
			JiraBaseURL: "https://your-org.atlassian.net",
			Concurrency: 3, PollInterval: 30, MaxBudgetUSD: 5,
			Repos: []config.Repo{{
				Path: "~/src/android-app", Branch: "ai/{ticket}-{slug}",
			}},
		}
	}
	fields := append(configFields(cfg),
		formField{label: "Jira API token", secret: true},
		formField{label: "Anthropic key (blank=skip)", secret: true},
	)
	m.form = &formModel{
		title:  "Setup wizard",
		note:   "writes ~/.agent/config.toml; tokens go to the OS keychain",
		fields: fields,
		submit: func(f []formField) tea.Msg {
			cfg, err := config.Load()
			if err != nil {
				cfg = &config.Config{PollInterval: 30, Concurrency: 3, MaxBudgetUSD: 5}
			}
			n := len(f) - 2 // last two fields are the secret tokens
			applyConfigFields(cfg, f[:n])
			if err := config.Save(cfg); err != nil {
				return formDoneMsg{err: err}
			}
			if tok := strings.TrimSpace(f[n].value); tok != "" {
				_ = secrets.Set(secrets.Jira, tok)
			}
			if key := strings.TrimSpace(f[n+1].value); key != "" {
				_ = secrets.Set(secrets.Anthropic, key)
			}
			return formDoneMsg{notice: "setup saved — pick Doctor to verify"}
		},
	}
	m.view = viewForm
}
