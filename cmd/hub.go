package cmd

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/andresuarezz26/magneton/internal/config"
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

// paletteItems is rebuilt each open so the daemon entry reflects current state.
func (m monitorModel) paletteItems() []paletteItem {
	items := []paletteItem{
		{"run", "Run new ticket…", "launch a ticket key or .md file"},
		{"doctor", "Doctor", "connectivity + setup health check"},
		{"config", "Edit config", "edit ~/.agent/config.toml fields"},
		{"setup", "Setup wizard", "configure Jira, repo, and tokens"},
	}
	if _, ok := daemonAlive(); ok {
		items = append(items, paletteItem{"stop", "Stop daemon", "stop the background poller"})
	} else {
		items = append(items, paletteItem{"start", "Start daemon", "poll Jira and run the fleet in the background"})
	}
	return append(items, paletteItem{"quit", "Quit", "exit the hub"})
}

func (m monitorModel) updatePalette(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	items := m.paletteItems()
	switch msg.String() {
	case "esc", ":", "q":
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
			return m.runPaletteItem(items[m.paletteCursor].key)
		}
	}
	return m, nil
}

func (m monitorModel) runPaletteItem(key string) (tea.Model, tea.Cmd) {
	m.view = viewDashboard
	switch key {
	case "run":
		m.view = viewRunInput
		m.runText = ""
	case "doctor":
		m.notice = "running doctor…"
		return m, m.runDoctor()
	case "config":
		m.openConfigForm()
	case "setup":
		m.openSetupForm()
	case "start":
		m.notice = "starting daemon…"
		return m, m.startDaemon()
	case "stop":
		m.notice = "stopping daemon…"
		return m, m.stopDaemon()
	case "quit":
		return m, tea.Quit
	}
	return m, nil
}

func (m monitorModel) renderPalette(w int) string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("  Commands") + "\n\n")
	for i, it := range m.paletteItems() {
		line := fmt.Sprintf("  %-16s %s", it.label, dimStyle.Render(it.desc))
		if i == m.paletteCursor {
			line = selStyle.Render(fmt.Sprintf("  %-16s ", it.label)) + dimStyle.Render(it.desc)
		}
		b.WriteString(truncate(line, w) + "\n")
	}
	b.WriteString("\n  " + dimStyle.Render("↑↓ select · enter run · esc close"))
	return b.String()
}

// ---- run-new input ---------------------------------------------------------

func (m monitorModel) updateRunInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		text := strings.TrimSpace(m.runText)
		m.view = viewDashboard
		if text == "" {
			return m, nil
		}
		return m, m.launchRun(text)
	case tea.KeyEsc:
		m.view = viewDashboard
		m.runText = ""
	case tea.KeyBackspace:
		if r := []rune(m.runText); len(r) > 0 {
			m.runText = string(r[:len(r)-1])
		}
	case tea.KeySpace:
		m.runText += " "
	case tea.KeyRunes:
		m.runText += string(msg.Runes)
	default:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m monitorModel) renderRunInput(w int) string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("  Run new") + "\n")
	b.WriteString(dimStyle.Render("  ticket key(s) or .md path(s), space-separated") + "\n\n")
	b.WriteString("  › " + m.runText + "▌\n")
	b.WriteString("\n  " + dimStyle.Render("enter launch · esc cancel"))
	return b.String()
}

// launchRun spawns `agent run <args…>` detached; the new session appears in the
// dashboard and streams to its log (which the detail pane tails).
func (m monitorModel) launchRun(text string) tea.Cmd {
	self := m.selfPath
	return func() tea.Msg {
		args := append([]string{"run"}, strings.Fields(text)...)
		c := exec.Command(self, args...)
		c.Stdout, c.Stderr = nil, nil
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		err := c.Start()
		return runLaunchedMsg{text: text, err: err}
	}
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
func configFields(cfg *config.Config) []formField {
	repo := config.Repo{}
	if len(cfg.Repos) > 0 {
		repo = cfg.Repos[0]
	}
	return []formField{
		{label: "Jira base URL", value: cfg.JiraBaseURL},
		{label: "Jira email", value: cfg.JiraEmail},
		{label: "In-progress status", value: cfg.JiraInProgressStatus},
		{label: "Concurrency", value: strconv.Itoa(cfg.Concurrency)},
		{label: "Repo path", value: repo.Path},
		{label: "JQL", value: repo.JQL},
		{label: "Branch", value: repo.Branch},
		{label: "Compile", value: repo.Compile},
		{label: "Test", value: repo.Test},
	}
}

// applyConfigFields writes the (non-secret) form values back onto a config.
func applyConfigFields(cfg *config.Config, f []formField) {
	repo := config.Repo{MaxRetries: 3}
	if len(cfg.Repos) > 0 {
		repo = cfg.Repos[0]
	}
	cfg.JiraBaseURL = f[0].value
	cfg.JiraEmail = f[1].value
	cfg.JiraInProgressStatus = f[2].value
	cfg.Concurrency, _ = strconv.Atoi(f[3].value)
	repo.Path = f[4].value
	repo.JQL = f[5].value
	repo.Branch = f[6].value
	repo.Compile = f[7].value
	repo.Test = f[8].value
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
				JQL:     "labels = ai-agent AND status = 'To Do'",
				Compile: "./gradlew :app:compileDebug", Test: "./gradlew testDebugUnitTest",
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
			applyConfigFields(cfg, f[:9]) // first 9 are the config fields
			if err := config.Save(cfg); err != nil {
				return formDoneMsg{err: err}
			}
			if tok := strings.TrimSpace(f[9].value); tok != "" {
				_ = secrets.Set(secrets.Jira, tok)
			}
			if key := strings.TrimSpace(f[10].value); key != "" {
				_ = secrets.Set(secrets.Anthropic, key)
			}
			return formDoneMsg{notice: "setup saved — pick Doctor to verify"}
		},
	}
	m.view = viewForm
}
